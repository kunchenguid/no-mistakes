package daemon

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/gatecontext"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

const axiRunClaimTTL = 5 * time.Minute

type axiRunClaim struct {
	repoID     string
	branch     string
	headSHA    string
	intentHash [sha256.Size]byte
	issuerPID  int
	expiresAt  time.Time
}

func (m *RunManager) prepareAXIRun(params ipc.PrepareAXIRunParams, issuerPID int) (string, error) {
	if strings.TrimSpace(params.RepoID) == "" || strings.TrimSpace(params.Branch) == "" || strings.TrimSpace(params.HeadSHA) == "" || strings.TrimSpace(params.Intent) == "" {
		return "", fmt.Errorf("repo, branch, head, and intent are required")
	}
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("create AXI run capability: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(random)
	now := time.Now()
	m.axiRunClaimsMu.Lock()
	defer m.axiRunClaimsMu.Unlock()
	if m.axiRunClaims == nil {
		m.axiRunClaims = make(map[string]axiRunClaim)
	}
	for existingToken, claim := range m.axiRunClaims {
		if !claim.expiresAt.After(now) {
			delete(m.axiRunClaims, existingToken)
		}
	}
	m.axiRunClaims[token] = axiRunClaim{
		repoID:     params.RepoID,
		branch:     params.Branch,
		headSHA:    params.HeadSHA,
		intentHash: sha256.Sum256([]byte(params.Intent)),
		issuerPID:  issuerPID,
		expiresAt:  now.Add(axiRunClaimTTL),
	}
	return token, nil
}

func (m *RunManager) consumeAXIRun(token, repoID, branch, headSHA, intent string, peerPID int) bool {
	if token == "" {
		return false
	}
	m.axiRunClaimsMu.Lock()
	claim, ok := m.axiRunClaims[token]
	delete(m.axiRunClaims, token)
	m.axiRunClaimsMu.Unlock()
	if !ok || !claim.expiresAt.After(time.Now()) || claim.repoID != repoID || claim.branch != branch || claim.headSHA != headSHA || claim.intentHash != sha256.Sum256([]byte(intent)) {
		return false
	}
	if claim.issuerPID <= 0 || peerPID <= 0 {
		return true
	}
	matched, err := gatecontext.ProcessDescendsFrom(peerPID, claim.issuerPID)
	return err == nil && matched
}
