package agent

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPiEncodeSessionDirName_MatchesPiConvention(t *testing.T) {
	t.Parallel()
	// Verified against pi's dist/core/session-manager.js
	// getDefaultSessionDirPath: strip one leading separator, then every
	// '/', '\', and ':' becomes '-', wrapped in double dashes.
	cases := []struct {
		in   string
		want string
	}{
		{"/Users/x/.no-mistakes/worktrees/abc/01RUN", "--Users-x-.no-mistakes-worktrees-abc-01RUN--"},
		{"/", "----"},
		{`C:\work\repo`, "--C--work-repo--"},
		{`D:/mixed\seps/here`, "--D--mixed-seps-here--"},
	}
	for _, tc := range cases {
		if got := piEncodeSessionDirName(tc.in); got != tc.want {
			t.Errorf("piEncodeSessionDirName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPiSessionFileID(t *testing.T) {
	t.Parallel()
	const id = "019ff2f3-5f31-744b-90b8-679074ff7686"
	if got := piSessionFileID("2026-08-25T16-00-06-143Z_" + id + ".jsonl"); got != id {
		t.Errorf("valid name parsed as %q, want %q", got, id)
	}
	for _, bad := range []string{
		"session.jsonl",
		"_" + id + ".jsonl",
		"2026-08-25T16-00-06-143Z_" + id + ".tmp",
		"2026-08-25T16-00-06-143Z_not-a-uuid.jsonl",
		id + ".jsonl",
		"",
	} {
		if got := piSessionFileID(bad); got != "" {
			t.Errorf("piSessionFileID(%q) = %q, want rejected", bad, got)
		}
	}
}

func TestPiAgentDirFromEnv(t *testing.T) {
	t.Parallel()
	if got := piAgentDirFromEnv([]string{"PI_CODING_AGENT_DIR=/custom/agent", "HOME=/home/x"}); got != "/custom/agent" {
		t.Errorf("PI_CODING_AGENT_DIR must win, got %q", got)
	}
	if got := piAgentDirFromEnv([]string{"HOME=/home/x"}); got != filepath.Join("/home/x", ".pi", "agent") {
		t.Errorf("home-derived agent dir = %q", got)
	}
	if got := piAgentDirFromEnv([]string{"HOME=/first", "HOME=/last"}); got != filepath.Join("/last", ".pi", "agent") {
		t.Errorf("last env entry must win, got %q", got)
	}
}

// piWatchFixture collects watcher callbacks for assertions.
type piWatchFixture struct {
	mu         sync.Mutex
	activities []ActivityKind
	notes      []string
	headerID   atomic.Value // stores string; late stdout header simulation
}

func (f *piWatchFixture) setHeaderID(id string) { f.headerID.Store(id) }

func (f *piWatchFixture) headerIDString() string {
	id, _ := f.headerID.Load().(string)
	return id
}

func (f *piWatchFixture) onActivity(kind ActivityKind) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.activities = append(f.activities, kind)
}

func (f *piWatchFixture) onNote(msg string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notes = append(f.notes, msg)
}

func (f *piWatchFixture) sessionActivityCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, k := range f.activities {
		if k == ActivitySession {
			n++
		}
	}
	return n
}

func (f *piWatchFixture) notesJoined() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.notes, "\n")
}

func shortPiWatchPoll(t *testing.T) {
	t.Helper()
	prev := piSessionWatchPollInterval
	piSessionWatchPollInterval = 10 * time.Millisecond
	t.Cleanup(func() { piSessionWatchPollInterval = prev })
}

func writePiSessionFile(t *testing.T, dir, name string, lines ...string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func appendToFile(t *testing.T, path, data string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(data); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

const piWatchTestSessionID = "019ff2f3-5f31-744b-90b8-679074ff7686"

func piWatchEnv(home string) []string { return []string{"HOME=" + home} }

func waitForCondition(t *testing.T, timeout time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestPiSessionWatcher_ResumeBindsAndCreditsAdvancement(t *testing.T) {
	shortPiWatchPoll(t)
	home := t.TempDir()
	cwd := t.TempDir()
	dir := piSessionDirForCWD(piAgentDirFromEnv(piWatchEnv(home)), cwd)
	path := writePiSessionFile(t, dir, "2026-08-25T16-00-06-143Z_"+piWatchTestSessionID+".jsonl", `{"type":"session","id":"`+piWatchTestSessionID+`"}`)

	fx := &piWatchFixture{}
	w := startPiSessionWatcher(context.Background(), nil, piWatchEnv(home), cwd,
		&SessionRef{ID: piWatchTestSessionID}, func() string { return piWatchTestSessionID }, fx.onActivity, fx.onNote)
	if w == nil {
		t.Fatal("resume with an exact session file must bind a watcher")
	}
	defer w.shutdown()

	waitForCondition(t, 2*time.Second, "resume binding note", func() bool {
		return strings.Contains(fx.notesJoined(), "watching session "+piWatchTestSessionID)
	})
	if got := fx.sessionActivityCount(); got != 0 {
		t.Fatalf("pre-existing session content credited %d activities; only new advancement counts", got)
	}

	appendToFile(t, path, `{"type":"message","message":{"role":"assistant","content":"working"}}`)
	waitForCondition(t, 2*time.Second, "session advancement activity", func() bool {
		return fx.sessionActivityCount() > 0
	})
}

func TestPiSessionWatcher_FreshSessionBindsToTheOneNewFile(t *testing.T) {
	shortPiWatchPoll(t)
	home := t.TempDir()
	cwd := t.TempDir()
	dir := piSessionDirForCWD(piAgentDirFromEnv(piWatchEnv(home)), cwd)
	// A pre-existing unrelated session in the same dir must not count as new.
	writePiSessionFile(t, dir, "2026-08-20T10-00-00-000Z_019fe058-c457-72a1-a555-fcfaa1a9f7bd.jsonl", `{"type":"session"}`)

	fx := &piWatchFixture{}
	w := startPiSessionWatcher(context.Background(), nil, piWatchEnv(home), cwd,
		&SessionRef{}, fx.headerIDString, fx.onActivity, fx.onNote)
	if w == nil {
		t.Fatal("durable session must start a watcher")
	}
	defer w.shutdown()

	path := writePiSessionFile(t, dir, "2026-08-25T16-00-06-143Z_"+piWatchTestSessionID+".jsonl", `{"type":"session","id":"`+piWatchTestSessionID+`"}`)
	// The buffered stdout header arrives later and confirms the same id.
	fx.setHeaderID(piWatchTestSessionID)
	waitForCondition(t, 2*time.Second, "fresh binding note", func() bool {
		return strings.Contains(fx.notesJoined(), "watching session "+piWatchTestSessionID)
	})

	appendToFile(t, path, `{"type":"message"}`)
	waitForCondition(t, 2*time.Second, "fresh session advancement", func() bool {
		return fx.sessionActivityCount() > 0
	})
}

func TestPiSessionWatcher_TwoNewFilesAreAmbiguousAndCreditNothing(t *testing.T) {
	shortPiWatchPoll(t)
	home := t.TempDir()
	cwd := t.TempDir()
	dir := piSessionDirForCWD(piAgentDirFromEnv(piWatchEnv(home)), cwd)

	fx := &piWatchFixture{}
	w := startPiSessionWatcher(context.Background(), nil, piWatchEnv(home), cwd,
		&SessionRef{}, func() string { return "" }, fx.onActivity, fx.onNote)
	if w == nil {
		t.Fatal("watcher must start so it can detect ambiguity")
	}
	defer w.shutdown()

	writePiSessionFile(t, dir, "2026-08-25T16-00-06-143Z_"+piWatchTestSessionID+".jsonl", `{"type":"session"}`)
	writePiSessionFile(t, dir, "2026-08-25T16-00-07-000Z_019fe058-c457-72a1-a555-fcfaa1a9f7bd.jsonl", `{"type":"session"}`)
	waitForCondition(t, 2*time.Second, "ambiguity note", func() bool {
		return strings.Contains(fx.notesJoined(), "ambiguous")
	})
	appendToFile(t, filepath.Join(dir, "2026-08-25T16-00-06-143Z_"+piWatchTestSessionID+".jsonl"), `{"type":"message"}`)
	// Give the watcher several ticks to prove it never credits ambiguous files.
	time.Sleep(100 * time.Millisecond)
	if got := fx.sessionActivityCount(); got != 0 {
		t.Fatalf("ambiguous binding credited %d session activities; want conservative stdout-only behavior", got)
	}
}

func TestPiSessionWatcher_HeaderMismatchUnbinds(t *testing.T) {
	shortPiWatchPoll(t)
	home := t.TempDir()
	cwd := t.TempDir()
	dir := piSessionDirForCWD(piAgentDirFromEnv(piWatchEnv(home)), cwd)

	fx := &piWatchFixture{}
	w := startPiSessionWatcher(context.Background(), nil, piWatchEnv(home), cwd,
		&SessionRef{}, fx.headerIDString, fx.onActivity, fx.onNote)
	if w == nil {
		t.Fatal("watcher must start")
	}
	defer w.shutdown()

	path := writePiSessionFile(t, dir, "2026-08-25T16-00-06-143Z_"+piWatchTestSessionID+".jsonl", `{"type":"session"}`)
	waitForCondition(t, 2*time.Second, "binding note", func() bool {
		return strings.Contains(fx.notesJoined(), "watching session")
	})

	// The late stdout header names a DIFFERENT session: the bound file is not
	// this invocation's, so the watcher must fail closed and stop crediting.
	fx.setHeaderID("019fe058-c457-72a1-a555-fcfaa1a9f7bd")
	waitForCondition(t, 2*time.Second, "mismatch unbind note", func() bool {
		return strings.Contains(fx.notesJoined(), "does not match")
	})
	creditedAtUnbind := fx.sessionActivityCount()
	appendToFile(t, path, `{"type":"message"}`)
	time.Sleep(100 * time.Millisecond)
	if got := fx.sessionActivityCount(); got != creditedAtUnbind {
		t.Fatalf("unbound watcher credited %d further session activities", got-creditedAtUnbind)
	}
}

func TestPiSessionWatcher_SessionFreeInvocationHasNoWatcher(t *testing.T) {
	fx := &piWatchFixture{}
	if w := startPiSessionWatcher(context.Background(), nil, piWatchEnv(t.TempDir()), t.TempDir(),
		nil, func() string { return "" }, fx.onActivity, fx.onNote); w != nil {
		t.Fatal("--no-session invocations persist nothing and must not be watched")
	}
}

func TestPiSessionWatcher_RelocatedSessionStorageDisablesBinding(t *testing.T) {
	fx := &piWatchFixture{}
	if w := startPiSessionWatcher(context.Background(), nil, []string{"HOME=" + t.TempDir(), "PI_CODING_AGENT_SESSION_DIR=/flat/sessions"}, t.TempDir(),
		&SessionRef{}, func() string { return "" }, fx.onActivity, fx.onNote); w != nil {
		t.Fatal("PI_CODING_AGENT_SESSION_DIR relocation must disable binding")
	}
	if !strings.Contains(fx.notesJoined(), "stdout-only liveness") {
		t.Fatalf("relocation note = %q, want stdout-only explanation", fx.notesJoined())
	}

	fx2 := &piWatchFixture{}
	if w := startPiSessionWatcher(context.Background(), []string{"--session-dir", "/flat/sessions"}, piWatchEnv(t.TempDir()), t.TempDir(),
		&SessionRef{}, func() string { return "" }, fx2.onActivity, fx2.onNote); w != nil {
		t.Fatal("--session-dir override must disable binding")
	}
}

func TestPiSessionWatcher_StopsWithContext(t *testing.T) {
	shortPiWatchPoll(t)
	home := t.TempDir()
	cwd := t.TempDir()
	dir := piSessionDirForCWD(piAgentDirFromEnv(piWatchEnv(home)), cwd)
	path := writePiSessionFile(t, dir, "2026-08-25T16-00-06-143Z_"+piWatchTestSessionID+".jsonl", `{"type":"session"}`)

	ctx, cancel := context.WithCancel(context.Background())
	fx := &piWatchFixture{}
	w := startPiSessionWatcher(ctx, nil, piWatchEnv(home), cwd,
		&SessionRef{ID: piWatchTestSessionID}, func() string { return piWatchTestSessionID }, fx.onActivity, fx.onNote)
	if w == nil {
		t.Fatal("resume must bind")
	}
	waitForCondition(t, 2*time.Second, "binding note", func() bool {
		return strings.Contains(fx.notesJoined(), "watching session")
	})
	cancel()
	w.shutdown() // must return promptly: the goroutine is owned by ctx+shutdown
	appendToFile(t, path, `{"type":"message"}`)
	time.Sleep(50 * time.Millisecond)
	if got := fx.sessionActivityCount(); got != 0 {
		t.Fatalf("stopped watcher credited %d activities", got)
	}
}

func TestStartNativeAgentCommand_ReportsLifecycleAndStdoutActivity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixture")
	}
	var mu sync.Mutex
	var kinds []ActivityKind
	record := func(kind ActivityKind) {
		mu.Lock()
		defer mu.Unlock()
		kinds = append(kinds, kind)
	}
	cmd := exec.Command("sh", "-c", "printf hello; sleep 0.05")
	started, err := startNativeAgentCommand(cmd, record)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		_, _ = io.ReadAll(started.stderr)
	}()
	buf := make([]byte, 16)
	var out strings.Builder
	for {
		n, err := started.stdout.Read(buf)
		if n > 0 {
			out.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}
	<-stderrDone
	_ = started.wait()
	started.closePipes()
	if out.String() != "hello" {
		t.Fatalf("stdout = %q, want hello", out.String())
	}
	mu.Lock()
	defer mu.Unlock()
	var sawLifecycle, sawStdout bool
	for _, k := range kinds {
		sawLifecycle = sawLifecycle || k == ActivityLifecycle
		sawStdout = sawStdout || k == ActivityStdout
	}
	if !sawLifecycle {
		t.Fatal("process start did not report lifecycle activity")
	}
	if !sawStdout {
		t.Fatal("stdout bytes did not report stdout activity")
	}
}
