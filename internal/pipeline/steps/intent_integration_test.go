package steps

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
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
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
// matching Claude transcript fixture into a fake $HOME so the default
// intent extractor has something to discover.
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
	base = gitCmd(t, repoDir, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repoDir, "internal_foo.go"), []byte("package foo\nfunc Bar() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repoDir, "add", ".")
	gitCmd(t, repoDir, "commit", "-m", "head")
	head = gitCmd(t, repoDir, "rev-parse", "HEAD")

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

func withFakeHome(t *testing.T, fakeHome string) {
	t.Helper()
	t.Setenv("HOME", fakeHome)
	t.Setenv("USERPROFILE", fakeHome)
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

func newIntentIntegrationContext(t *testing.T, repoDir, base, head string, cfg *config.Config) *pipeline.StepContext {
	t.Helper()
	d := openIntentTestDB(t)
	repo, err := d.InsertRepo(repoDir, "https://example.com/r.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", head, base)
	if err != nil {
		t.Fatal(err)
	}
	return &pipeline.StepContext{
		Ctx:      context.Background(),
		Run:      run,
		Repo:     repo,
		WorkDir:  repoDir,
		Agent:    &fakeIntentAgent{},
		Config:   cfg,
		DB:       d,
		Log:      func(string) {},
		LogChunk: func(string) {},
		LogFile:  func(string) {},
	}
}

func TestIntentStep_Integration_AttachesSummaryToRun(t *testing.T) {
	repoDir, fakeHome, base, head := initIntentRepo(t)
	withFakeHome(t, fakeHome)

	cfg := &config.Config{
		Intent: config.Intent{Enabled: true, Threshold: 0.1, SlackDays: 3},
	}
	sctx := newIntentIntegrationContext(t, repoDir, base, head, cfg)

	outcome, err := (&IntentStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if outcome == nil || outcome.Skipped {
		t.Fatalf("expected non-skipped outcome, got %+v", outcome)
	}

	got, err := sctx.DB.GetRun(sctx.Run.ID)
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

func TestIntentStep_Integration_DisabledIsNoOp(t *testing.T) {
	repoDir, fakeHome, base, head := initIntentRepo(t)
	withFakeHome(t, fakeHome)

	cfg := &config.Config{Intent: config.Intent{Enabled: false}}
	sctx := newIntentIntegrationContext(t, repoDir, base, head, cfg)

	outcome, err := (&IntentStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if outcome == nil || !outcome.Skipped {
		t.Errorf("expected Skipped when disabled, got %+v", outcome)
	}
	got, _ := sctx.DB.GetRun(sctx.Run.ID)
	if got.Intent != nil {
		t.Errorf("expected nil Intent when disabled, got %v", *got.Intent)
	}
}

func TestIntentStep_Integration_NoTranscriptIsNoOp(t *testing.T) {
	repoDir, _, base, head := initIntentRepo(t)
	emptyHome := t.TempDir()
	withFakeHome(t, emptyHome)

	cfg := &config.Config{Intent: config.Intent{Enabled: true, Threshold: 0.5, SlackDays: 3}}
	sctx := newIntentIntegrationContext(t, repoDir, base, head, cfg)

	outcome, err := (&IntentStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if outcome == nil || !outcome.Skipped {
		t.Errorf("expected Skipped with no matching transcript, got %+v", outcome)
	}
	got, _ := sctx.DB.GetRun(sctx.Run.ID)
	if got.Intent != nil {
		t.Errorf("expected nil Intent, got %v", *got.Intent)
	}
}

func TestIntentStep_Integration_ZeroBaseSHA_NewBranchPush(t *testing.T) {
	repoDir, fakeHome, base, _ := initIntentRepo(t)
	withFakeHome(t, fakeHome)

	gitCmd(t, repoDir, "checkout", "-b", "feature", base)
	if err := os.WriteFile(filepath.Join(repoDir, "internal_foo.go"), []byte("package foo\nfunc Bar() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repoDir, "add", ".")
	gitCmd(t, repoDir, "commit", "-m", "branch head")
	branchHead := gitCmd(t, repoDir, "rev-parse", "HEAD")

	const zeroSHA = "0000000000000000000000000000000000000000"
	cfg := &config.Config{Intent: config.Intent{Enabled: true, Threshold: 0.1, SlackDays: 3}}
	sctx := newIntentIntegrationContext(t, repoDir, zeroSHA, branchHead, cfg)

	outcome, err := (&IntentStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if outcome == nil || outcome.Skipped {
		t.Fatalf("expected non-skipped outcome on zero-base path, got %+v", outcome)
	}

	got, _ := sctx.DB.GetRun(sctx.Run.ID)
	if got.Intent == nil {
		t.Fatal("zero-base diff path failed; intent not attached")
	}
	if !strings.Contains(*got.Intent, "Bar()") {
		t.Errorf("Intent = %q", *got.Intent)
	}
}

// Ensure the step honors its internal timeout by not hanging beyond it.
func TestIntentStep_Integration_RespectsTimeout(t *testing.T) {
	repoDir, fakeHome, base, head := initIntentRepo(t)
	withFakeHome(t, fakeHome)

	cfg := &config.Config{Intent: config.Intent{Enabled: true, Threshold: 0.1, SlackDays: 3}}
	sctx := newIntentIntegrationContext(t, repoDir, base, head, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sctx.Ctx = ctx

	done := make(chan struct{})
	go func() {
		(&IntentStep{}).Execute(sctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(intentExtractTimeout + 5*time.Second):
		t.Fatal("IntentStep.Execute did not return within budget")
	}
}
