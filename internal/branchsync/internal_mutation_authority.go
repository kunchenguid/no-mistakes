package branchsync

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
)

type InternalRefMutationAuthorization struct {
	Capability string `json:"capability"`
	Phase      string `json:"phase"`
	GatePath   string `json:"gate_path"`
	Branch     string `json:"branch"`
	Ref        string `json:"ref"`
	OldSHA     string `json:"old_sha"`
	NewSHA     string `json:"new_sha"`
	Operation  string `json:"operation"`
	Scope      string `json:"scope"`
}

type internalMutationAuthority struct {
	listener   net.Listener
	endpoint   string
	database   *db.DB
	owner      *BranchOwnershipLock
	generation string
	done       chan struct{}
	wg         sync.WaitGroup
	mu         sync.Mutex
	closed     bool
	connMu     sync.Mutex
	conns      map[net.Conn]struct{}
}

func newInternalMutationAuthority(database *db.DB, endpoint string, owner *BranchOwnershipLock, generation string) (*internalMutationAuthority, error) {
	listener, endpoint, err := listenInternalMutationAuthority(endpoint)
	if err != nil {
		return nil, err
	}
	authority := &internalMutationAuthority{listener: listener, endpoint: endpoint, database: database, owner: owner, generation: generation, done: make(chan struct{}), conns: make(map[net.Conn]struct{})}
	go authority.serve()
	return authority, nil
}

func (a *internalMutationAuthority) serve() {
	defer close(a.done)
	for {
		conn, err := a.listener.Accept()
		if err != nil {
			return
		}
		a.mu.Lock()
		if a.closed {
			a.mu.Unlock()
			_ = conn.Close()
			continue
		}
		a.connMu.Lock()
		a.conns[conn] = struct{}{}
		a.connMu.Unlock()
		a.wg.Add(1)
		a.mu.Unlock()
		go func() {
			defer a.wg.Done()
			a.handle(conn)
		}()
	}
}

func (a *internalMutationAuthority) handle(conn net.Conn) {
	defer func() {
		a.connMu.Lock()
		delete(a.conns, conn)
		a.connMu.Unlock()
		_ = conn.Close()
	}()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	var request InternalRefMutationAuthorization
	response := struct {
		Error string `json:"error,omitempty"`
	}{}
	if err := json.NewDecoder(conn).Decode(&request); err != nil {
		response.Error = err.Error()
	} else if err := a.authorize(request); err != nil {
		response.Error = err.Error()
	}
	_ = json.NewEncoder(conn).Encode(response)
}

func (a *internalMutationAuthority) authorize(request InternalRefMutationAuthorization) error {
	if a.owner == nil {
		return db.ErrInternalRefMutation
	}
	a.owner.authorityMu.Lock()
	defer a.owner.authorityMu.Unlock()
	a.mu.Lock()
	closed := a.closed || a.owner.authority != a || a.owner.authorityGeneration != a.generation
	a.mu.Unlock()
	if closed || a.owner.file == nil {
		return db.ErrInternalRefMutation
	}
	if _, err := a.owner.file.Stat(); err != nil {
		return db.ErrInternalRefMutation
	}
	return a.database.AdvanceInternalRefMutation(a.endpoint, request.Phase, request.GatePath, request.Branch, request.Ref, request.OldSHA, request.NewSHA, request.Operation, request.Scope, request.Capability)
}

func (a *internalMutationAuthority) close() {
	if a == nil {
		return
	}
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return
	}
	a.closed = true
	_ = a.listener.Close()
	a.mu.Unlock()
	a.connMu.Lock()
	connections := make([]net.Conn, 0, len(a.conns))
	for conn := range a.conns {
		connections = append(connections, conn)
	}
	a.connMu.Unlock()
	for _, conn := range connections {
		_ = conn.Close()
	}
	<-a.done
	a.wg.Wait()
	_ = a.database.InvalidateInternalRefMutations(a.endpoint)
	closeInternalMutationAuthority(a.endpoint)
}

func (l *BranchOwnershipLock) ensureInternalMutationAuthority(database *db.DB) (string, error) {
	if l == nil || database == nil {
		return "", fmt.Errorf("issue internal ref mutation: active branch lock is required")
	}
	l.authorityMu.Lock()
	defer l.authorityMu.Unlock()
	if l.file == nil {
		return "", fmt.Errorf("issue internal ref mutation: active branch lock is required")
	}
	if _, err := l.file.Stat(); err != nil {
		return "", fmt.Errorf("issue internal ref mutation: active branch lock is required")
	}
	if l.authority != nil {
		return l.authority.endpoint, nil
	}
	if err := database.InvalidateInternalRefMutationsPrefix(l.authorityPrefix); err != nil {
		return "", fmt.Errorf("issue internal ref mutation: retire stale authority capabilities: %w", err)
	}
	endpoint, err := newInternalMutationAuthorityEndpoint(l.authorityPrefix)
	if err != nil {
		return "", fmt.Errorf("issue internal ref mutation: generate authority generation: %w", err)
	}
	generation, err := newGateRefLockGeneration()
	if err != nil {
		return "", fmt.Errorf("issue internal ref mutation: generate lock generation: %w", err)
	}
	authority, err := newInternalMutationAuthority(database, endpoint, l, generation)
	if err != nil {
		return "", fmt.Errorf("issue internal ref mutation: open live authority: %w", err)
	}
	if err := database.InvalidateInternalRefMutations(authority.endpoint); err != nil {
		authority.close()
		return "", fmt.Errorf("issue internal ref mutation: retire stale authority capabilities: %w", err)
	}
	l.authority = authority
	l.authorityGeneration = generation
	return authority.endpoint, nil
}

func newInternalMutationAuthorityEndpoint(prefix string) (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return prefix + "-" + hex.EncodeToString(value), nil
}

func (l *BranchOwnershipLock) closeInternalMutationAuthority() {
	if l == nil {
		return
	}
	l.authorityMu.Lock()
	authority := l.authority
	l.authority = nil
	l.authorityMu.Unlock()
	if authority != nil {
		authority.close()
	}
}

func AuthorizeInternalRefMutation(endpoint string, request InternalRefMutationAuthorization) error {
	conn, err := dialInternalMutationAuthority(endpoint)
	if err != nil {
		return fmt.Errorf("authorize internal ref mutation: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		return fmt.Errorf("authorize internal ref mutation: send request: %w", err)
	}
	var response struct {
		Error string `json:"error,omitempty"`
	}
	if err := json.NewDecoder(conn).Decode(&response); err != nil {
		return fmt.Errorf("authorize internal ref mutation: read response: %w", err)
	}
	if response.Error != "" {
		return errors.New(response.Error)
	}
	return nil
}
