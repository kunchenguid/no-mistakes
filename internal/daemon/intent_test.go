package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
)

// fakeIntentAgent always returns a canned summary - bypasses any real LLM.
type fakeIntentAgent struct{}

func (f *fakeIntentAgent) Name() string { return "fake" }
func (f *fakeIntentAgent) Run(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
	return &agent.Result{
		Output: []byte(`{"summary": "user wanted to add Bar() to internal/foo.go"}`),
		Text:   `{"summary": "user wanted to add Bar() to internal/foo.go"}`,
	}, nil
}
func (f *fakeIntentAgent) Close() error { return nil }

// initIntentRepo creates a real git repo with two commits and writes a
// matching Claude transcript fixture into a fake $HOME so extractIntent
// has something to discover.
func initIntentRepo(t *testing.T) (repoDir, fakeHome, base, head string) {
	t.Helper()
	repoDir = t.TempDir()
	gitCmd(t, repoDir, "init")
	gitCmd(t, repoDir, "config", "user.email", "test@example.com")
	gitCmd(t, repoDir, "config", "user.name", "Tester")
	if err := os.WriteFile(filepath.Join(repoDir, "internal_foo.go"), []byte("package foo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repoDir, "add", ".")
	gitCmd(t, repoDir, "commit", "-m", "base")
	base = gitOutput(t, repoDir, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repoDir, "internal_foo.go"), []byte("package foo\nfunc Bar() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repoDir, "add", ".")
	gitCmd(t, repoDir, "commit", "-m", "head")
	head = gitOutput(t, repoDir, "rev-parse", "HEAD")

	// Write a Claude fixture matching the repo's cwd.
	fakeHome = t.TempDir()
	encoded := testClaudeProjectDirName(repoDir)
	claudeDir := filepath.Join(fakeHome, ".claude", "projects", encoded)
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := `{"type":"user","cwd":` + testJSONString(t, repoDir) + `,"timestamp":"2026-04-18T02:15:37.407Z","uuid":"u1","sessionId":"s1","message":{"role":"user","content":"please add Bar() to internal_foo.go"}}
{"type":"assistant","cwd":` + testJSONString(t, repoDir) + `,"timestamp":"2026-04-18T02:15:38.000Z","uuid":"u2","sessionId":"s1","message":{"role":"assistant","content":[{"type":"tool_use","name":"Edit","input":{"file_path":` + testJSONString(t, filepath.Join(repoDir, "internal_foo.go")) + `}}]}}
`
	if err := os.WriteFile(filepath.Join(claudeDir, "session.jsonl"), []byte(transcript), 0o644); err != nil {
		t.Fatal(err)
	}
	return
}

func testClaudeProjectDirName(cwd string) string {
	replacer := strings.NewReplacer("/", "-", `\`, "-", ":", "-")
	return replacer.Replace(cwd)
}

func testJSONString(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// withFakeHome temporarily redirects HOME so the Claude reader picks up
// the fixture rather than the real machine's transcripts.
func withFakeHome(t *testing.T, fakeHome string) {
	t.Helper()
	t.Setenv("HOME", fakeHome)
}

func openIntentTestDB(t *testing.T) *db.DB {
	t.Helper()
	dir := t.TempDir()
	d, err := db.Open(filepath.Join(dir, "test.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestExtractIntent_AttachesSummaryToRun(t *testing.T) {
	repoDir, fakeHome, base, head := initIntentRepo(t)
	withFakeHome(t, fakeHome)

	d := openIntentTestDB(t)
	repo, err := d.InsertRepo(repoDir, "https://example.com/r.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", head, base)
	if err != nil {
		t.Fatal(err)
	}

	m := &RunManager{db: d}
	cfg := &config.Config{
		Intent: config.Intent{
			Enabled:   true,
			Threshold: 0.1,
			SlackDays: 3,
		},
	}

	m.extractIntent(context.Background(), cfg, &fakeIntentAgent{}, repo, run, repoDir, base, head)

	got, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Intent == nil || !strings.Contains(*got.Intent, "Bar()") {
		t.Errorf("Intent not attached: %+v", got.Intent)
	}
	if got.IntentSource == nil || *got.IntentSource != "claude" {
		t.Errorf("IntentSource = %v", got.IntentSource)
	}
	if got.IntentScore == nil || *got.IntentScore <= 0 {
		t.Errorf("IntentScore = %v", got.IntentScore)
	}
}

func TestExtractIntent_DisabledIsNoOp(t *testing.T) {
	repoDir, fakeHome, base, head := initIntentRepo(t)
	withFakeHome(t, fakeHome)

	d := openIntentTestDB(t)
	repo, _ := d.InsertRepo(repoDir, "https://example.com/r.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", head, base)

	m := &RunManager{db: d}
	cfg := &config.Config{Intent: config.Intent{Enabled: false}}

	m.extractIntent(context.Background(), cfg, &fakeIntentAgent{}, repo, run, repoDir, base, head)

	got, _ := d.GetRun(run.ID)
	if got.Intent != nil {
		t.Errorf("expected nil Intent when disabled, got %v", *got.Intent)
	}
}

func TestExtractIntent_NoTranscriptIsNoOp(t *testing.T) {
	repoDir, _, base, head := initIntentRepo(t)
	emptyHome := t.TempDir()
	withFakeHome(t, emptyHome)

	d := openIntentTestDB(t)
	repo, _ := d.InsertRepo(repoDir, "https://example.com/r.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", head, base)

	m := &RunManager{db: d}
	cfg := &config.Config{Intent: config.Intent{Enabled: true, Threshold: 0.5, SlackDays: 3}}

	// Should not panic, should not attach.
	m.extractIntent(context.Background(), cfg, &fakeIntentAgent{}, repo, run, repoDir, base, head)

	got, _ := d.GetRun(run.ID)
	if got.Intent != nil {
		t.Errorf("expected nil Intent with no matching transcript, got %v", *got.Intent)
	}
}

func TestExtractIntent_ZeroBaseSHA_NewBranchPush(t *testing.T) {
	repoDir, fakeHome, base, _ := initIntentRepo(t)
	withFakeHome(t, fakeHome)

	// Branch off from base so HEAD is ahead of "main" and merge-base
	// resolves to a strictly earlier commit. Without this, HEAD == main,
	// merge-base returns HEAD, and the diff is empty.
	gitCmd(t, repoDir, "checkout", "-b", "feature", base)
	if err := os.WriteFile(filepath.Join(repoDir, "internal_foo.go"), []byte("package foo\nfunc Bar() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repoDir, "add", ".")
	gitCmd(t, repoDir, "commit", "-m", "branch head")
	branchHead := gitOutput(t, repoDir, "rev-parse", "HEAD")

	d := openIntentTestDB(t)
	repo, _ := d.InsertRepo(repoDir, "https://example.com/r.git", "main")
	// Simulate a first-push to a new branch: base is the all-zeros SHA
	// that git's pre-receive hook reports for a freshly-created ref.
	const zeroSHA = "0000000000000000000000000000000000000000"
	run, _ := d.InsertRun(repo.ID, "feature", branchHead, zeroSHA)

	m := &RunManager{db: d}
	cfg := &config.Config{Intent: config.Intent{Enabled: true, Threshold: 0.1, SlackDays: 3}}

	m.extractIntent(context.Background(), cfg, &fakeIntentAgent{}, repo, run, repoDir, zeroSHA, branchHead)

	got, _ := d.GetRun(run.ID)
	if got.Intent == nil {
		t.Fatal("zero-base diff path failed; intent not attached")
	}
	if !strings.Contains(*got.Intent, "Bar()") {
		t.Errorf("Intent = %q", *got.Intent)
	}
}

// Ensure the function honors a tight timeout by not hanging beyond it.
func TestExtractIntent_RespectsTimeout(t *testing.T) {
	repoDir, fakeHome, base, head := initIntentRepo(t)
	withFakeHome(t, fakeHome)

	d := openIntentTestDB(t)
	repo, _ := d.InsertRepo(repoDir, "https://example.com/r.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", head, base)

	m := &RunManager{db: d}
	cfg := &config.Config{Intent: config.Intent{Enabled: true, Threshold: 0.1, SlackDays: 3}}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		m.extractIntent(ctx, cfg, &fakeIntentAgent{}, repo, run, repoDir, base, head)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(intentExtractTimeout + 5*time.Second):
		t.Fatal("extractIntent did not return within budget")
	}
}
