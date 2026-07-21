package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
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
	gitCmd(t, repoDir, "branch", "-M", "main")
	base = gitCmd(t, repoDir, "rev-parse", "HEAD")
	gitCmd(t, repoDir, "checkout", "-b", "feature")
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

func gitCmdAt(t *testing.T, dir string, when time.Time, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	date := when.UTC().Format(time.RFC3339)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test",
		"GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=test",
		"GIT_COMMITTER_EMAIL=test@test.com",
		"GIT_AUTHOR_DATE="+date,
		"GIT_COMMITTER_DATE="+date,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
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

func TestIntentStep_Integration_DeletedFilesDoNotDiluteIntentMatch(t *testing.T) {
	repoDir := t.TempDir()
	gitCmd(t, repoDir, "init")
	gitCmd(t, repoDir, "config", "user.email", "test@example.com")
	gitCmd(t, repoDir, "config", "user.name", "Tester")
	if err := os.WriteFile(filepath.Join(repoDir, "active.go"), []byte("package active\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40; i++ {
		name := filepath.Join(repoDir, fmt.Sprintf("obsolete_%02d.go", i))
		if err := os.WriteFile(name, []byte("package obsolete\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitCmd(t, repoDir, "add", ".")
	gitCmd(t, repoDir, "commit", "-m", "base")
	gitCmd(t, repoDir, "branch", "-M", "main")
	base := gitCmd(t, repoDir, "rev-parse", "HEAD")
	gitCmd(t, repoDir, "checkout", "-b", "feature")

	for i := 0; i < 40; i++ {
		if err := os.Remove(filepath.Join(repoDir, fmt.Sprintf("obsolete_%02d.go", i))); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(repoDir, "active.go"), []byte("package active\nfunc Run() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repoDir, "add", ".")
	gitCmd(t, repoDir, "commit", "-m", "replace obsolete code")
	head := gitCmd(t, repoDir, "rev-parse", "HEAD")

	fakeHome := t.TempDir()
	encoded := testClaudeProjectDirName(repoDir)
	claudeDir := filepath.Join(fakeHome, ".claude", "projects", encoded)
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := `{"type":"user","cwd":` + testJSONString(t, repoDir) + `,"timestamp":"2026-04-18T02:15:37.407Z","uuid":"u1","sessionId":"s1","message":{"role":"user","content":"please add Run() to active.go"}}
{"type":"assistant","cwd":` + testJSONString(t, repoDir) + `,"timestamp":"2026-04-18T02:15:38.000Z","uuid":"u2","sessionId":"s1","message":{"role":"assistant","content":[{"type":"tool_use","name":"Edit","input":{"file_path":` + testJSONString(t, filepath.Join(repoDir, "active.go")) + `}}]}}
`
	if err := os.WriteFile(filepath.Join(claudeDir, "session.jsonl"), []byte(transcript), 0o644); err != nil {
		t.Fatal(err)
	}
	withFakeHome(t, fakeHome)

	cfg := &config.Config{Intent: config.Intent{Enabled: true, Threshold: 0.2, SlackDays: 3}}
	sctx := newIntentIntegrationContext(t, repoDir, base, head, cfg)

	outcome, err := (&IntentStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if outcome == nil || outcome.Skipped {
		t.Fatalf("expected deleted files to be ignored for intent matching, got %+v", outcome)
	}

	got, _ := sctx.DB.GetRun(sctx.Run.ID)
	if got.Intent == nil {
		t.Fatal("intent not attached")
	}
	if got.IntentSource == nil || *got.IntentSource != "claude" {
		t.Fatalf("intent source = %v, want claude", got.IntentSource)
	}
	if got.IntentScore == nil || *got.IntentScore < 1 {
		t.Fatalf("intent score = %v, want full active.go match", got.IntentScore)
	}
	t.Logf("deleted 40 files and changed active.go; attached intent from claude session with score %.2f: %s", *got.IntentScore, *got.Intent)
}

func TestIntentStep_Integration_ZeroBaseSHA_NewBranchPush(t *testing.T) {
	repoDir, fakeHome, base, _ := initIntentRepo(t)
	withFakeHome(t, fakeHome)

	gitCmd(t, repoDir, "checkout", "-B", "feature", base)
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

func TestIntentStep_Integration_ZeroBaseUsesOldFeatureSessionNotRecentUnrelated(t *testing.T) {
	repoDir := t.TempDir()
	gitCmd(t, repoDir, "init")
	gitCmd(t, repoDir, "config", "user.email", "test@example.com")
	gitCmd(t, repoDir, "config", "user.name", "Tester")
	headTime := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	baseTime := headTime.Add(-60 * 24 * time.Hour)
	oldSessionTime := baseTime.Add(24 * time.Hour)
	recentSessionTime := headTime.Add(-30 * time.Minute)
	if err := os.WriteFile(filepath.Join(repoDir, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repoDir, "add", ".")
	gitCmdAt(t, repoDir, baseTime, "commit", "-m", "base")
	gitCmd(t, repoDir, "branch", "-M", "main")
	gitCmd(t, repoDir, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(repoDir, "feature.go"), []byte("package feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repoDir, "add", ".")
	gitCmdAt(t, repoDir, headTime, "commit", "-m", "feature")
	head := gitCmd(t, repoDir, "rev-parse", "HEAD")

	fakeHome := t.TempDir()
	claudeDir := filepath.Join(fakeHome, ".claude", "projects", testClaudeProjectDirName(repoDir))
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTranscript := func(name, request, file string, when time.Time) {
		t.Helper()
		transcript := `{"type":"user","cwd":` + testJSONString(t, repoDir) + `,"timestamp":` + testJSONString(t, when.Format(time.RFC3339Nano)) + `,"uuid":"u1","sessionId":` + testJSONString(t, name) + `,"message":{"role":"user","content":` + testJSONString(t, request) + `}}
{"type":"assistant","cwd":` + testJSONString(t, repoDir) + `,"timestamp":` + testJSONString(t, when.Add(time.Minute).Format(time.RFC3339Nano)) + `,"uuid":"u2","sessionId":` + testJSONString(t, name) + `,"message":{"role":"assistant","content":[{"type":"tool_use","name":"Edit","input":{"file_path":` + testJSONString(t, filepath.Join(repoDir, file)) + `}}]}}
`
		path := filepath.Join(claudeDir, name+".jsonl")
		if err := os.WriteFile(path, []byte(transcript), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatal(err)
		}
	}
	writeTranscript("old-feature", "add the feature package", "feature.go", oldSessionTime)
	writeTranscript("recent-unrelated", "change unrelated code", "unrelated.go", recentSessionTime)
	withFakeHome(t, fakeHome)

	const zeroSHA = "0000000000000000000000000000000000000000"
	disabledReaders := map[string]bool{"codex": true, "opencode": true, "rovodev": true, "pi": true, "copilot": true}
	cfg := &config.Config{Intent: config.Intent{Enabled: true, Threshold: 0.1, DisabledReaders: disabledReaders}}
	sctx := newIntentIntegrationContext(t, repoDir, zeroSHA, head, cfg)

	outcome, err := (&IntentStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if outcome == nil || outcome.Skipped {
		t.Fatalf("expected old feature session to match, got %+v", outcome)
	}
	got, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.IntentSessionID == nil || *got.IntentSessionID != "old-feature" {
		t.Fatalf("intent session = %v, want old-feature", got.IntentSessionID)
	}
}

func TestResolveIntentBaseSHAUsesPipelineBaseAfterSubsequentPush(t *testing.T) {
	repoDir := t.TempDir()
	gitCmd(t, repoDir, "init")
	gitCmd(t, repoDir, "config", "user.email", "test@example.com")
	gitCmd(t, repoDir, "config", "user.name", "Tester")
	if err := os.WriteFile(filepath.Join(repoDir, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repoDir, "add", ".")
	gitCmd(t, repoDir, "commit", "-m", "base")
	gitCmd(t, repoDir, "checkout", "-b", "staging")
	if err := os.WriteFile(filepath.Join(repoDir, "staging.txt"), []byte("staging\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repoDir, "add", ".")
	gitCmd(t, repoDir, "commit", "-m", "staging")
	pipelineBase := gitCmd(t, repoDir, "rev-parse", "HEAD")
	gitCmd(t, repoDir, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(repoDir, "first.txt"), []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repoDir, "add", ".")
	gitCmd(t, repoDir, "commit", "-m", "first push")
	previousFeatureTip := gitCmd(t, repoDir, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repoDir, "second.txt"), []byte("second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repoDir, "add", ".")
	gitCmd(t, repoDir, "commit", "-m", "second push")

	got := resolveIntentBaseSHA(context.Background(), repoDir, "staging")
	if got != pipelineBase {
		t.Fatalf("resolveIntentBaseSHA = %q, want pipeline base %q", got, pipelineBase)
	}
	if got == previousFeatureTip {
		t.Fatalf("resolveIntentBaseSHA reused previous feature tip %q", got)
	}
}

func TestIntentStep_Integration_UsesPipelineWorkDirForGitState(t *testing.T) {
	originRepo := t.TempDir()
	gitCmd(t, originRepo, "init", "-b", "main")
	gitCmd(t, originRepo, "config", "user.email", "test@example.com")
	gitCmd(t, originRepo, "config", "user.name", "Tester")
	if err := os.WriteFile(filepath.Join(originRepo, "internal_foo.go"), []byte("package foo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, originRepo, "add", ".")
	gitCmd(t, originRepo, "commit", "-m", "base")
	base := gitCmd(t, originRepo, "rev-parse", "HEAD")

	fakeHome := t.TempDir()
	encoded := testClaudeProjectDirName(originRepo)
	claudeDir := filepath.Join(fakeHome, ".claude", "projects", encoded)
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := `{"type":"user","cwd":` + testJSONString(t, originRepo) + `,"timestamp":"2026-04-18T02:15:37.407Z","uuid":"u1","sessionId":"s1","message":{"role":"user","content":"please add Bar() to internal_foo.go"}}
{"type":"assistant","cwd":` + testJSONString(t, originRepo) + `,"timestamp":"2026-04-18T02:15:38.000Z","uuid":"u2","sessionId":"s1","message":{"role":"assistant","content":[{"type":"tool_use","name":"Edit","input":{"file_path":` + testJSONString(t, filepath.Join(originRepo, "internal_foo.go")) + `}}]}}
`
	if err := os.WriteFile(filepath.Join(claudeDir, "session.jsonl"), []byte(transcript), 0o644); err != nil {
		t.Fatal(err)
	}
	withFakeHome(t, fakeHome)

	pipelineWorkDir := filepath.Join(t.TempDir(), "worktree")
	gitCmd(t, t.TempDir(), "clone", originRepo, pipelineWorkDir)
	gitCmd(t, pipelineWorkDir, "config", "user.email", "test@example.com")
	gitCmd(t, pipelineWorkDir, "config", "user.name", "Tester")
	gitCmd(t, pipelineWorkDir, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(pipelineWorkDir, "internal_foo.go"), []byte("package foo\nfunc Bar() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, pipelineWorkDir, "add", ".")
	gitCmd(t, pipelineWorkDir, "commit", "-m", "head only in pipeline workdir")
	head := gitCmd(t, pipelineWorkDir, "rev-parse", "HEAD")

	cfg := &config.Config{Intent: config.Intent{Enabled: true, Threshold: 0.1, SlackDays: 3}}
	sctx := newIntentIntegrationContext(t, originRepo, base, head, cfg)
	sctx.WorkDir = pipelineWorkDir

	outcome, err := (&IntentStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if outcome == nil || outcome.Skipped {
		t.Fatalf("expected non-skipped outcome when head exists in pipeline workdir, got %+v", outcome)
	}

	got, _ := sctx.DB.GetRun(sctx.Run.ID)
	if got.Intent == nil {
		t.Fatal("intent not attached")
	}
	if !strings.Contains(*got.Intent, "Bar()") {
		t.Errorf("Intent = %q", *got.Intent)
	}
}

// After a force push, Run.BaseSHA is the prior remote tip of the branch, which
// may be unreachable in the worktree (rewritten away or never fetched). The
// step must fall back to merge-base against the default branch instead of
// trusting the orphaned SHA, otherwise `git diff <orphaned>..<head>` fails
// with "Invalid revision range" and intent silently skips.
func TestIntentStep_Integration_ForcePushedOrphanedBaseSHA(t *testing.T) {
	repoDir, fakeHome, _, _ := initIntentRepo(t)
	withFakeHome(t, fakeHome)

	// Add another feature commit that touches internal_foo.go. Vary the
	// existing feature content to preserve a real diff from main.
	if err := os.WriteFile(filepath.Join(repoDir, "internal_foo.go"), []byte("package foo\nfunc Bar() { /* feature */ }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repoDir, "add", ".")
	gitCmd(t, repoDir, "commit", "-m", "feature head")
	branchHead := gitCmd(t, repoDir, "rev-parse", "HEAD")

	// Simulate a force-pushed branch: BaseSHA is a non-zero, non-existent
	// commit (the previous remote tip that got rewritten away).
	const orphanedBaseSHA = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	cfg := &config.Config{Intent: config.Intent{Enabled: true, Threshold: 0.1, SlackDays: 3}}
	sctx := newIntentIntegrationContext(t, repoDir, orphanedBaseSHA, branchHead, cfg)

	outcome, err := (&IntentStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if outcome == nil || outcome.Skipped {
		t.Fatalf("expected non-skipped outcome on force-pushed branch, got %+v", outcome)
	}

	got, _ := sctx.DB.GetRun(sctx.Run.ID)
	if got.Intent == nil {
		t.Fatal("force-pushed branch: intent not attached")
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
