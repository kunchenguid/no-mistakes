package supervision

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStoreArmThenClaimStopHook(t *testing.T) {
	store := NewStore(t.TempDir())
	armed, err := store.Arm(Registration{RunID: "run-1", RepoID: "repo-1", CWD: "/work"})
	if err != nil {
		t.Fatalf("Arm() error = %v", err)
	}
	if armed.Phase != PhaseArmed {
		t.Fatalf("Arm() phase = %q, want %q", armed.Phase, PhaseArmed)
	}

	claimed, ok, err := store.Claim("/work", "session-1")
	if err != nil || !ok {
		t.Fatalf("Claim() = (%+v, %v, %v), want claimed registration", claimed, ok, err)
	}
	if claimed.SessionID != "session-1" || claimed.Phase != PhaseWatching {
		t.Fatalf("Claim() = %+v, want session and watching phase", claimed)
	}

	_, ok, err = store.Claim("/work", "session-2")
	if err != nil || ok {
		t.Fatalf("second Claim() = (_, %v, %v), want not claimed", ok, err)
	}
}

func TestStoreClaimRejectsDifferentWorkingDirectory(t *testing.T) {
	store := NewStore(t.TempDir())
	if _, err := store.Arm(Registration{RunID: "run-1", RepoID: "repo-1", CWD: "/work"}); err != nil {
		t.Fatalf("Arm() error = %v", err)
	}
	if _, ok, err := store.Claim("/other", "session-1"); err != nil || ok {
		t.Fatalf("Claim(other cwd) = (_, %v, %v), want no claim", ok, err)
	}
}

func TestStoreArmRejectsAnotherActiveRegistrationInSameWorkingDirectory(t *testing.T) {
	store := NewStore(t.TempDir())
	if _, err := store.Arm(Registration{RunID: "run-1", RepoID: "repo-1", CWD: "/work"}); err != nil {
		t.Fatalf("first Arm() error = %v", err)
	}
	if _, err := store.Arm(Registration{RunID: "run-2", RepoID: "repo-1", CWD: "/work"}); err == nil {
		t.Fatal("second Arm() error = nil, want active-registration rejection")
	}
}

func TestStoreFindByCWDKeepsAwaitingUserRegistration(t *testing.T) {
	store := NewStore(t.TempDir())
	reg, err := store.Arm(Registration{RunID: "run-1", RepoID: "repo-1", CWD: "/work"})
	if err != nil {
		t.Fatalf("Arm() error = %v", err)
	}
	reg.Phase = PhaseAwaitingUser
	if err := store.Save(reg); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got, found, err := store.FindByCWD("/work")
	if err != nil || !found || got.RunID != "run-1" {
		t.Fatalf("FindByCWD() = (%+v, %v, %v), want awaiting registration", got, found, err)
	}
}

func TestStoreUsesRunScopedFiles(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	if _, err := store.Arm(Registration{RunID: "run-1", RepoID: "repo-1", CWD: "/work"}); err != nil {
		t.Fatalf("Arm() error = %v", err)
	}
	if got, want := store.Path("run-1"), filepath.Join(dir, "run-1.json"); got != want {
		t.Fatalf("Path() = %q, want %q", got, want)
	}
}

func TestStoreRecoversFromStaleClaimLockFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".claim.lock"), []byte("stale"), 0o600); err != nil {
		t.Fatalf("write stale lock: %v", err)
	}
	store := NewStore(dir)
	if _, err := store.Arm(Registration{RunID: "run-1", RepoID: "repo-1", CWD: "/work"}); err != nil {
		t.Fatalf("Arm() with stale lock file error = %v", err)
	}
}

func TestStorePrepareHandoffDeduplicatesTurn(t *testing.T) {
	store := NewStore(t.TempDir())
	if _, err := store.Arm(Registration{RunID: "run-1", RepoID: "repo-1", CWD: "/work"}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.Claim("/work", "session-1"); err != nil || !ok {
		t.Fatalf("Claim() = (_, %v, %v)", ok, err)
	}
	first, emitted, err := store.PrepareHandoff("run-1", "session-1", "turn-1", "heartbeat-1", "progress-1", PhaseHandoffInProgress, 123, 1)
	if err != nil || !emitted {
		t.Fatalf("first PrepareHandoff() = (%+v, %v, %v)", first, emitted, err)
	}
	if first.LastHandoffTurnID != "turn-1" || first.LastHandoffFingerprint != "heartbeat-1" {
		t.Fatalf("first handoff fields = %+v", first)
	}
	_, emitted, err = store.PrepareHandoff("run-1", "session-1", "turn-1", "heartbeat-2", "progress-2", PhaseHandoffInProgress, 124, 2)
	if err != nil || emitted {
		t.Fatalf("duplicate PrepareHandoff() emitted=%v err=%v, want false nil", emitted, err)
	}
	_, emitted, err = store.PrepareHandoff("run-1", "session-1", "turn-2", "heartbeat-1", "progress-1", PhaseHandoffInProgress, 124, 2)
	if err != nil || !emitted {
		t.Fatalf("new-turn PrepareHandoff() emitted=%v err=%v, want true nil", emitted, err)
	}
}
