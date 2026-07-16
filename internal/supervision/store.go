// Package supervision persists the smallest possible amount of local state
// needed to reconnect an explicitly armed No-Mistakes run to a Codex Stop hook.
package supervision

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type Phase string

const (
	PhaseArmed             Phase = "armed"
	PhaseWatching          Phase = "watching"
	PhaseHandoffInProgress Phase = "handoff_in_progress"
	PhaseAwaitingUser      Phase = "awaiting_user"
	PhaseAwaitingMerge     Phase = "awaiting_merge_result"
	PhasePaused            Phase = "paused"
	PhaseCompleted         Phase = "completed"
)

type Registration struct {
	RunID                  string `json:"run_id"`
	RepoID                 string `json:"repo_id"`
	CWD                    string `json:"cwd"`
	Branch                 string `json:"branch"`
	SessionID              string `json:"session_id,omitempty"`
	Phase                  Phase  `json:"phase"`
	Fingerprint            string `json:"fingerprint,omitempty"`
	LastHandoffTurnID      string `json:"last_handoff_turn_id,omitempty"`
	LastHandoffFingerprint string `json:"last_handoff_fingerprint,omitempty"`
	NextHeartbeatAt        int64  `json:"next_heartbeat_at,omitempty"`
	StaleHeartbeats        int    `json:"stale_heartbeats,omitempty"`
	Error                  string `json:"error,omitempty"`
	UpdatedAt              int64  `json:"updated_at"`
}

type Store struct{ dir string }

func NewStore(dir string) *Store { return &Store{dir: dir} }

func (s *Store) Path(runID string) string { return filepath.Join(s.dir, runID+".json") }

func (s *Store) Arm(reg Registration) (Registration, error) {
	if strings.TrimSpace(reg.RunID) == "" || strings.TrimSpace(reg.RepoID) == "" || strings.TrimSpace(reg.CWD) == "" {
		return Registration{}, fmt.Errorf("run id, repo id, and cwd are required")
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return Registration{}, fmt.Errorf("create supervision directory: %w", err)
	}
	lock, err := acquireStoreLockWait(filepath.Join(s.dir, ".claim.lock"))
	if err != nil {
		return Registration{}, err
	}
	defer lock.Release()
	regs, err := s.all()
	if err != nil {
		return Registration{}, err
	}
	for _, existing := range regs {
		if existing.CWD != reg.CWD || existing.Phase == PhaseCompleted || existing.Phase == PhasePaused || existing.Phase == PhaseAwaitingMerge {
			continue
		}
		return Registration{}, fmt.Errorf("supervision is already active for this working directory")
	}
	reg.Phase = PhaseArmed
	reg.SessionID = ""
	reg.Fingerprint = ""
	reg.LastHandoffTurnID = ""
	reg.LastHandoffFingerprint = ""
	reg.NextHeartbeatAt = 0
	reg.StaleHeartbeats = 0
	reg.Error = ""
	reg.UpdatedAt = time.Now().UTC().Unix()
	if err := s.write(reg); err != nil {
		return Registration{}, err
	}
	return reg, nil
}

// Claim binds one armed registration for cwd to a session id. Returning false
// is an ordinary no-op: hooks run for every Codex turn, not just supervision.
func (s *Store) Claim(cwd, sessionID string) (Registration, bool, error) {
	if strings.TrimSpace(cwd) == "" || strings.TrimSpace(sessionID) == "" {
		return Registration{}, false, nil
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return Registration{}, false, fmt.Errorf("create supervision directory: %w", err)
	}
	unlock, acquired, err := s.acquireClaimLock()
	if err != nil {
		return Registration{}, false, err
	}
	if !acquired {
		return Registration{}, false, nil
	}
	defer unlock()
	regs, err := s.all()
	if err != nil {
		return Registration{}, false, err
	}
	for _, reg := range regs {
		if reg.CWD != cwd || reg.Phase != PhaseArmed {
			continue
		}
		reg.SessionID = sessionID
		reg.Phase = PhaseWatching
		reg.UpdatedAt = time.Now().UTC().Unix()
		if err := s.write(reg); err != nil {
			return Registration{}, false, err
		}
		return reg, true, nil
	}
	return Registration{}, false, nil
}

func (s *Store) Get(runID string) (Registration, bool, error) {
	data, err := os.ReadFile(s.Path(runID))
	if os.IsNotExist(err) {
		return Registration{}, false, nil
	}
	if err != nil {
		return Registration{}, false, fmt.Errorf("read registration: %w", err)
	}
	var reg Registration
	if err := json.Unmarshal(data, &reg); err != nil {
		return Registration{}, false, fmt.Errorf("decode registration: %w", err)
	}
	return reg, true, nil
}

// FindByCWD returns the one registration owned by a Codex session in cwd.
// Armed registrations are intentionally included: a Stop hook is what binds an
// otherwise session-less arm request to the session that actually ended.
func (s *Store) FindByCWD(cwd string) (Registration, bool, error) {
	regs, err := s.all()
	if err != nil {
		return Registration{}, false, err
	}
	for _, reg := range regs {
		if reg.CWD == cwd && reg.Phase != PhaseCompleted && reg.Phase != PhasePaused && reg.Phase != PhaseAwaitingMerge {
			return reg, true, nil
		}
	}
	return Registration{}, false, nil
}

// UpdateForSession atomically persists a small supervisor state transition for
// the registered session. It deliberately does not create a new claim: hooks
// for unrelated Codex sessions must remain harmless no-ops.
func (s *Store) UpdateForSession(runID, sessionID string, update func(*Registration)) (Registration, bool, error) {
	if strings.TrimSpace(runID) == "" || strings.TrimSpace(sessionID) == "" {
		return Registration{}, false, nil
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return Registration{}, false, fmt.Errorf("create supervision directory: %w", err)
	}
	unlock, acquired, err := s.acquireClaimLock()
	if err != nil {
		return Registration{}, false, err
	}
	if !acquired {
		return Registration{}, false, nil
	}
	defer unlock()
	reg, found, err := s.Get(runID)
	if err != nil || !found || reg.SessionID != sessionID {
		return Registration{}, false, err
	}
	update(&reg)
	reg.UpdatedAt = time.Now().UTC().Unix()
	if err := s.write(reg); err != nil {
		return Registration{}, false, err
	}
	return reg, true, nil
}

// ResumeAfterUser re-arms the one registration that deliberately yielded a
// visible ask-user gate. It is idempotent so a repeated AXI response cannot
// revive a paused, completed, or merge-handoff registration.
func (s *Store) ResumeAfterUser(runID string) (Registration, bool, error) {
	if strings.TrimSpace(runID) == "" {
		return Registration{}, false, nil
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return Registration{}, false, fmt.Errorf("create supervision directory: %w", err)
	}
	unlock, acquired, err := s.acquireClaimLock()
	if err != nil || !acquired {
		return Registration{}, false, err
	}
	defer unlock()
	reg, found, err := s.Get(runID)
	if err != nil || !found || reg.Phase != PhaseAwaitingUser {
		return reg, false, err
	}
	reg.Phase = PhaseWatching
	reg.Fingerprint = ""
	reg.LastHandoffTurnID = ""
	reg.LastHandoffFingerprint = ""
	reg.NextHeartbeatAt = 0
	reg.StaleHeartbeats = 0
	reg.Error = ""
	reg.UpdatedAt = time.Now().UTC().Unix()
	if err := s.write(reg); err != nil {
		return Registration{}, false, err
	}
	return reg, true, nil
}

// PrepareHandoff records the exact Stop-turn/event pair before the hook emits
// its JSON continuation. This is the idempotency boundary for repeated Stop
// deliveries; stop_hook_active itself is intentionally not used as a veto.
func (s *Store) PrepareHandoff(runID, sessionID, turnID, eventFingerprint, progressFingerprint string, phase Phase, nextHeartbeatAt int64, staleHeartbeats int) (Registration, bool, error) {
	if strings.TrimSpace(turnID) == "" || strings.TrimSpace(eventFingerprint) == "" {
		return Registration{}, false, nil
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return Registration{}, false, fmt.Errorf("create supervision directory: %w", err)
	}
	unlock, acquired, err := s.acquireClaimLock()
	if err != nil {
		return Registration{}, false, err
	}
	if !acquired {
		return Registration{}, false, nil
	}
	defer unlock()
	reg, found, err := s.Get(runID)
	if err != nil || !found || reg.SessionID != sessionID {
		return Registration{}, false, err
	}
	if reg.LastHandoffTurnID == turnID {
		return reg, false, nil
	}
	reg.Phase = phase
	reg.LastHandoffTurnID = turnID
	reg.LastHandoffFingerprint = eventFingerprint
	reg.Fingerprint = progressFingerprint
	reg.NextHeartbeatAt = nextHeartbeatAt
	reg.StaleHeartbeats = staleHeartbeats
	reg.UpdatedAt = time.Now().UTC().Unix()
	if err := s.write(reg); err != nil {
		return Registration{}, false, err
	}
	return reg, true, nil
}

func (s *Store) acquireClaimLock() (func(), bool, error) {
	lock, acquired, err := acquireStoreLock(filepath.Join(s.dir, ".claim.lock"))
	if err != nil || !acquired {
		return nil, acquired, err
	}
	return lock.Release, true, nil
}

func (s *Store) Save(reg Registration) error {
	if strings.TrimSpace(reg.RunID) == "" {
		return fmt.Errorf("run id is required")
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("create supervision directory: %w", err)
	}
	lock, err := acquireStoreLockWait(filepath.Join(s.dir, ".claim.lock"))
	if err != nil {
		return err
	}
	defer lock.Release()
	reg.UpdatedAt = time.Now().UTC().Unix()
	return s.write(reg)
}

func (s *Store) all() ([]Registration, error) {
	entries, err := os.ReadDir(s.dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list registrations: %w", err)
	}
	regs := make([]Registration, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		reg, ok, err := s.Get(strings.TrimSuffix(entry.Name(), ".json"))
		if err != nil {
			return nil, err
		}
		if ok {
			regs = append(regs, reg)
		}
	}
	sort.Slice(regs, func(i, j int) bool { return regs[i].RunID < regs[j].RunID })
	return regs, nil
}

func (s *Store) write(reg Registration) error {
	data, err := json.Marshal(reg)
	if err != nil {
		return fmt.Errorf("encode registration: %w", err)
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("create supervision directory: %w", err)
	}
	tmp, err := os.CreateTemp(s.dir, ".registration-*")
	if err != nil {
		return fmt.Errorf("create registration temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write registration: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("protect registration: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close registration: %w", err)
	}
	if err := os.Rename(tmpName, s.Path(reg.RunID)); err != nil {
		return fmt.Errorf("replace registration: %w", err)
	}
	return nil
}
