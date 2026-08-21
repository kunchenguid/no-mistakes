package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
)

// writeFakePiExecutable materializes a fake `pi` binary for the pipeline
// package's adapter-integration test. It mirrors the POSIX/Windows dual
// fixture shape used by internal/agent's Pi tests.
func writeFakePiExecutable(t *testing.T, dir, posixScript, windowsScript string) string {
	t.Helper()

	name := "pi"
	script := posixScript
	if runtime.GOOS == "windows" {
		name = "pi.cmd"
		script = windowsScript
	}
	bin := filepath.Join(dir, name)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake pi: %v", err)
	}
	return bin
}

// TestRunSessions_PiAdapterPersistsAndFallsBack drives the real piAgent
// (backed by a fake pi executable) through RunSessions to prove the
// adapter-specific contract the generic fake-based tests cannot: Pi's minted
// UUID is persisted as run_agent_sessions metadata, a dead resumed session
// triggers a marked fresh-session fallback that re-runs the same turn, and the
// replacement identity replaces the stored row.
func TestRunSessions_PiAdapterPersistsAndFallsBack(t *testing.T) {
	const (
		id1 = "019ff2f3-5f31-744b-90b8-679074ff7681"
		id2 = "019ff2f3-5f31-744b-90b8-679074ff7682"
		id3 = "019ff2f3-5f31-744b-90b8-679074ff7683"
	)
	dir := t.TempDir()
	bin := writeFakePiExecutable(t, dir, `#!/bin/sh
set -eu
cat > /dev/null
req=""
prev=""
for a in "$@"; do
	if [ "$prev" = "--session" ]; then
		req="$a"
	fi
	prev="$a"
done
if [ -f pi-expire ] && [ -n "$req" ]; then
	echo "session file missing" >&2
	exit 20
fi
if [ -n "$req" ]; then
	id="$req"
else
	n=$(cat pi-counter 2>/dev/null || echo 0)
	n=$((n+1))
	printf '%s\n' "$n" > pi-counter
	case "$n" in
		1) id=`+id1+` ;;
		2) id=`+id2+` ;;
		*) id=`+id3+` ;;
	esac
fi
printf '%s\n' "{\"type\":\"session\",\"id\":\"$id\"}"
printf '%s\n' '{"type":"agent_end","messages":[{"role":"assistant","content":[{"type":"text","text":"ok"}]}]}'
`, strings.Join([]string{
		"@echo off",
		"setlocal EnableDelayedExpansion",
		"more > nul",
		"set req=",
		"set prev=",
		"for %%a in (%*) do (",
		"  if \"!prev!\"==\"--session\" set req=%%a",
		"  set prev=%%a",
		")",
		"if exist pi-expire if not \"!req!\"==\"\" (",
		"  echo session file missing 1>&2",
		"  exit /b 20",
		")",
		"if not \"!req!\"==\"\" (",
		"  set id=!req!",
		"  goto emit",
		")",
		"rem Every %n% below sits on its own sequential line, so cmd.exe expands it",
		"rem at that line's parse time - after set /a n=n+1 has run. Never move them",
		"rem into one parenthesized block: parse-time expansion there would freeze",
		"rem %n% at its pre-increment value and emit a stale session identity.",
		"if exist pi-counter (set /p n=<pi-counter) else (set n=0)",
		"set /a n=n+1",
		"echo !n!>pi-counter",
		"set id=" + id1,
		"if \"%n%\"==\"2\" set id=" + id2,
		"if \"%n%\"==\"3\" set id=" + id3,
		":emit",
		"echo {\"type\":\"session\",\"id\":\"!id!\"}",
		"echo {\"type\":\"agent_end\",\"messages\":[{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"ok\"}]}]}",
	}, "\r\n"))

	d, run := sessionTestDB(t)
	pa, err := agent.New("pi", bin, nil)
	if err != nil {
		t.Fatalf("new pi agent: %v", err)
	}
	rs := NewRunSessions(d, run.ID, pa, true)

	var fallbackFlags []bool
	opts := agent.RunOpts{
		Prompt: "fix",
		CWD:    dir,
		OnAttempt: func(a agent.Attempt) {
			fallbackFlags = append(fallbackFlags, a.SessionFallback)
		},
	}

	storedSession := func() string {
		t.Helper()
		sessions, err := d.GetRunAgentSessions(run.ID)
		if err != nil {
			t.Fatalf("get run sessions: %v", err)
		}
		if len(sessions) != 1 {
			t.Fatalf("stored %d session rows, want 1: %+v", len(sessions), sessions)
		}
		s := sessions[0]
		if s.Role != string(SessionRoleFixer) || s.Agent != "pi" {
			t.Fatalf("stored session row = %+v, want role=%s agent=pi", s, SessionRoleFixer)
		}
		return s.SessionID
	}

	first, err := rs.Run(context.Background(), pa, SessionRoleFixer, opts, nil)
	if err != nil {
		t.Fatalf("first fixer turn: %v", err)
	}
	if first.SessionID != id1 || first.Resumed {
		t.Fatalf("first result = %+v, want started session %s", first, id1)
	}
	if got := storedSession(); got != id1 {
		t.Fatalf("persisted session = %q, want %q", got, id1)
	}

	// The next resume hits a dead session: while the expire marker exists the
	// fake pi exits nonzero for any --session invocation. RunSessions must drop
	// the identity, re-run the same turn in a fresh session marked as fallback,
	// and persist the replacement.
	expire := filepath.Join(dir, "pi-expire")
	if err := os.WriteFile(expire, []byte("x"), 0o644); err != nil {
		t.Fatalf("write expire marker: %v", err)
	}
	second, err := rs.Run(context.Background(), pa, SessionRoleFixer, opts, nil)
	if err != nil {
		t.Fatalf("fixer turn after dead resume must fall back, got error: %v", err)
	}
	if second.SessionID != id2 || second.Resumed {
		t.Fatalf("fallback result = %+v, want fresh session %s", second, id2)
	}
	if got := storedSession(); got != id2 {
		t.Fatalf("persisted session after fallback = %q, want %q", got, id2)
	}

	// Attempt instrumentation: turn one emitted one non-fallback attempt; turn
	// two emitted the failed resume attempt plus the marked fresh-session retry.
	wantFlags := []bool{false, false, true}
	if len(fallbackFlags) != len(wantFlags) {
		t.Fatalf("attempt fallback flags = %v, want %v", fallbackFlags, wantFlags)
	}
	for i, want := range wantFlags {
		if fallbackFlags[i] != want {
			t.Fatalf("attempt[%d] fallback = %v, want %v (all: %v)", i, fallbackFlags[i], want, fallbackFlags)
		}
	}

	// With the marker gone the replacement session is resumed normally.
	if err := os.Remove(expire); err != nil {
		t.Fatalf("remove expire marker: %v", err)
	}
	third, err := rs.Run(context.Background(), pa, SessionRoleFixer, opts, nil)
	if err != nil {
		t.Fatalf("third fixer turn: %v", err)
	}
	if third.SessionID != id2 || !third.Resumed {
		t.Fatalf("third result = %+v, want resumed session %s", third, id2)
	}
}
