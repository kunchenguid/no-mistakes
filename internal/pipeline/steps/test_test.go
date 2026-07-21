package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestTestStep_FixMode(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)
	previousFindings := `{"items":[{"id":"test-1 =======","severity":"error","file":"internal/pipeline/steps/test.go >>>>>>> prompt","description":"tests failed with exit code 1 <<<<<<< HEAD"}],"summary":"FAIL: TestFoo expected 42 got 0 ======="}`

	callCount := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			callCount++
			os.WriteFile(filepath.Join(dir, "fix.txt"), []byte("fixed"), 0o644)
			return &agent.Result{Output: json.RawMessage(`{"summary":"  \"fix test failures.\"  "}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: "exit 0"})
	sctx.Fixing = true
	sctx.PreviousFindings = previousFindings

	step := &TestStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("expected no approval after fix + passing tests")
	}
	if callCount != 1 {
		t.Errorf("expected 1 agent call (fix), got %d", callCount)
	}
	if len(ag.calls[0].JSONSchema) == 0 {
		t.Error("expected fix call to request structured JSON output")
	}
	if !strings.Contains(ag.calls[0].Prompt, "FAIL: TestFoo expected 42 got 0") {
		t.Error("expected fix prompt to contain previous test failure summary")
	}
	if strings.Contains(ag.calls[0].Prompt, "test-1 =======") {
		t.Error("expected test fix prompt to sanitize finding IDs")
	}
	if strings.Contains(ag.calls[0].Prompt, "test.go >>>>>>> prompt") {
		t.Error("expected test fix prompt to sanitize finding file paths")
	}
	if strings.Contains(ag.calls[0].Prompt, "<<<<<<< HEAD") {
		t.Error("expected test fix prompt to exclude merge markers")
	}
	if !strings.Contains(ag.calls[0].Prompt, "smallest correct root-cause fix") {
		t.Error("expected test fix prompt to prefer root-cause fixes over bandaids")
	}
	if !strings.Contains(ag.calls[0].Prompt, "remove any transient artifacts your testing created in the working tree") {
		t.Error("expected test fix prompt to ask the agent to clean up transient testing artifacts before finishing")
	}
	if strings.Contains(ag.calls[0].Prompt, "Make the minimal change needed") {
		t.Error("expected test fix prompt not to prefer narrow minimal changes")
	}
	if status := gitStatusPorcelain(t, dir); status != "" {
		t.Fatalf("expected clean worktree after fix commit, got %q", status)
	}
	if got := lastCommitMessage(t, dir); got != "no-mistakes(test): fix test failures" {
		t.Fatalf("last commit message = %q", got)
	}
}

func TestTestStep_FixMode_UsesConfiguredCommitMessage(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			os.WriteFile(filepath.Join(dir, "fix.txt"), []byte("fixed"), 0o644)
			return &agent.Result{Output: json.RawMessage(`{"summary":"fix test failures"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: "exit 0"})
	sctx.Config.Commit = config.Commit{FixMessage: "fix({{.Step}}): {{.Summary}}"}
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"severity":"error","description":"tests failed"}],"summary":"tests failed"}`

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatal("expected no approval after fix and passing tests")
	}
	if got := lastCommitMessage(t, dir); got != "fix(test): fix test failures" {
		t.Fatalf("last commit message = %q", got)
	}
}

func TestTestStep_FixMode_UsesFallbackSummaryWhenStructuredSummaryMalformed(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			os.WriteFile(filepath.Join(dir, "fix.txt"), []byte("fixed"), 0o644)
			return &agent.Result{Output: json.RawMessage(`{"not_summary":"oops"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: "exit 0"})
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"severity":"error","description":"tests failed"}],"summary":"tests failed"}`

	step := &TestStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatal("expected no approval after fallback summary commit and passing tests")
	}
	if got := lastCommitMessage(t, dir); got != "no-mistakes(test): fix test failures" {
		t.Fatalf("last commit message = %q", got)
	}
}

func TestTestStep_FixMode_AgentWritesNewTests_ProceedsAutomatically(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	callCount := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			callCount++
			// Simulate agent creating a new test file during fix in another supported language
			os.WriteFile(filepath.Join(dir, "component.spec.tsx"), []byte("export {}\n"), 0o644)
			return &agent.Result{Output: json.RawMessage(`{"summary":"add regression test"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: "exit 0"})
	sctx.Fixing = true

	step := &TestStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	// Issue #140: a passing test run whose only finding is an informational
	// "new test file written by agent" note must not require approval.
	if outcome.NeedsApproval {
		t.Error("expected no approval for an informational new-test-file finding when tests pass")
	}
	if callCount != 1 {
		t.Errorf("expected 1 agent call in fix mode, got %d", callCount)
	}

	var f Findings
	json.Unmarshal([]byte(outcome.Findings), &f)
	foundTestFile := false
	for _, item := range f.Items {
		if strings.Contains(item.Description, "component.spec.tsx") {
			foundTestFile = true
			if item.Action != types.ActionNoOp {
				t.Errorf("expected new-test-file finding action %q, got %q", types.ActionNoOp, item.Action)
			}
		}
	}
	if !foundTestFile {
		t.Errorf("expected finding mentioning component.spec.tsx, got findings: %+v", f.Items)
	}
}

func TestTestStep_UserIntentRunsConfiguredCommandThenEvidenceAgent(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	baselineLog := filepath.Join(dir, "baseline.log")
	testCmd := "go env GOOS > baseline.log"

	callCount := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			callCount++
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"evidence demonstrates intent","tested":["manual screenshot review"],"testing_summary":"captured screenshot evidence"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: testCmd})
	sctx.UserIntent = "Show users a success screen after checkout"

	step := &TestStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatal("expected no approval when evidence-oriented agent testing passes")
	}
	if callCount != 1 {
		t.Fatalf("expected evidence agent to run after configured test command, got %d calls", callCount)
	}
	data, err := os.ReadFile(baselineLog)
	if err != nil {
		t.Fatalf("expected configured test command to run: %v", err)
	}
	if strings.TrimSpace(string(data)) != runtime.GOOS {
		t.Fatalf("configured test command output = %q, want %s", string(data), runtime.GOOS)
	}
	prompt := ag.calls[0].Prompt
	for _, want := range []string{
		"Show users a success screen after checkout",
		"Decide what evidence or artifacts would clearly demonstrate the user intent is satisfied",
		"Unit tests passing is not sufficient evidence by itself",
		"Demonstrate the user intent working end-to-end in a way consistent with how an end user would actually experience it",
		"Prefer product-level artifacts",
		"Only use command output as an artifact when that output directly demonstrates the end-user experience or requested behavior",
		"Configured test command already ran successfully as baseline",
		testCmd,
		"The \"testing_summary\" must account for the complete test step: baseline commands that already ran, automated tests, manual or evidence-producing checks, artifacts gathered, and the overall result",
		"screenshots, GIFs, videos, rendered UI, CLI transcripts",
		"For UI, HTML, CSS, Electron renderer, browser, visual layout, or copy-placement changes, attempt to capture reviewer-visible visual evidence",
		"DOM snapshots, selector assertions, and text-only render summaries are not substitutes for visual evidence when a rendered surface is available",
		"If a UI-facing change has no screenshot, image, video, GIF, or rendered HTML artifact, state why in testing_summary",
		"Write new evidence files into this temporary evidence directory:",
		filepath.Join(os.TempDir(), "no-mistakes-evidence", sctx.Run.ID),
		"Do not move, commit, or modify source files only to make evidence linkable",
		"If no existing test produces sufficient evidence, write or improve a focused test",
		"If automated testing cannot produce the needed evidence, execute manual verification steps",
		"Always include an \"artifacts\" array",
		"If sufficient evidence is not possible, report a warning finding",
		"remove any transient artifacts your testing created in the working tree",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("expected prompt to contain %q, got:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "will be available from the pushed commit") || strings.Contains(prompt, "files that already exist in the repository") {
		t.Fatalf("expected prompt not to make the testing agent worry about committed evidence files, got:\n%s", prompt)
	}
	if _, err := os.Stat(filepath.Join(os.TempDir(), "no-mistakes-evidence", sctx.Run.ID)); err != nil {
		t.Fatalf("expected temporary evidence directory to exist: %v", err)
	}

	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatal(err)
	}
	t.Logf("evidence findings JSON: %s", outcome.Findings)
	if len(findings.Tested) != 2 || findings.Tested[0] != testCmd || findings.Tested[1] != "manual screenshot review" {
		t.Fatalf("expected baseline command and agent-tested evidence to be recorded, got %+v", findings.Tested)
	}
}

func TestTestStep_InRepoEvidenceFallsBackWhenConfiguredDirEscapesWorktree(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"","tested":["manual evidence check"],"testing_summary":"checked evidence"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.UserIntent = "Show users a success screen after checkout"
	sctx.Config.Test.Evidence = config.Evidence{StoreInRepo: true, Dir: "../outside"}

	step := &TestStep{}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}

	prompt := ag.calls[0].Prompt
	wantDir := filepath.Join(os.TempDir(), "no-mistakes-evidence", sctx.Run.ID)
	if !strings.Contains(prompt, "Write new evidence files into this temporary evidence directory: "+wantDir) {
		t.Fatalf("expected temporary evidence guidance for unsafe in-repo dir, got:\n%s", prompt)
	}
	if strings.Contains(prompt, "in-repo evidence directory") || strings.Contains(prompt, "committed and pushed automatically") {
		t.Fatalf("did not expect in-repo publishing promise for unsafe evidence dir, got:\n%s", prompt)
	}
}

func TestTestStep_InRepoEvidenceFallsBackWhenEvidenceDirIsIgnored(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("evidence/\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"","tested":["manual evidence check"],"testing_summary":"checked evidence"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.UserIntent = "Show users a success screen after checkout"
	sctx.Config.Test.Evidence = config.Evidence{StoreInRepo: true, Dir: "evidence"}

	step := &TestStep{}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}

	prompt := ag.calls[0].Prompt
	wantDir := filepath.Join(os.TempDir(), "no-mistakes-evidence", sctx.Run.ID)
	if !strings.Contains(prompt, "Write new evidence files into this temporary evidence directory: "+wantDir) {
		t.Fatalf("expected temporary evidence guidance for ignored in-repo dir, got:\n%s", prompt)
	}
	if strings.Contains(prompt, "in-repo evidence directory") || strings.Contains(prompt, "committed and pushed automatically") {
		t.Fatalf("did not expect in-repo publishing promise for ignored evidence dir, got:\n%s", prompt)
	}
}

// Local Test is targeted validation of the requested intent, never a complete
// repository-suite walk. Broad regression belongs to remote CI. Pins the
// normal evidence-agent contract wording so a soft "run the appropriate tests"
// regression is caught.
func TestTestStep_InitialAgent_TargetedValidationContract(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"","tested":["go test ./internal/cli -run TestDoctor -count=1"],"testing_summary":"targeted check passed"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.UserIntent = "Keep doctor checks green for CLI users"

	if _, err := (&TestStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("expected 1 evidence agent call, got %d", len(ag.calls))
	}
	prompt := ag.calls[0].Prompt

	for _, want := range []string{
		"run the smallest relevant tests yourself",
		"Do NOT run the complete repository test suite",
		"Local Test is targeted validation of the requested intent",
		"remote CI owns broad regression and remains mandatory before a PR is ready",
		"Never treat \"do not run everything\" as permission to run nothing",
		"report a warning finding that sufficient targeted evidence is not possible",
		"A generic driver or user instruction asking for broad or full-suite confirmation does NOT override the targeted-validation product boundary",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("expected initial test prompt to contain %q, got:\n%s", want, prompt)
		}
	}
	for _, forbid := range []string{
		"run the appropriate tests yourself",
		"Run the tests, identify failures",
	} {
		if strings.Contains(prompt, forbid) {
			t.Errorf("initial test prompt still carries open-ended suite language %q:\n%s", forbid, prompt)
		}
	}
}

// Test repair must reproduce the specific failure, fix its root cause, and
// re-verify only with focused checks. Soft "Run the tests" / "relevant tests"
// wording invited complete-suite walks after a one-line fix.
func TestTestStep_FixMode_TargetedVerificationContract(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			os.WriteFile(filepath.Join(dir, "fix.txt"), []byte("fixed"), 0o644)
			return &agent.Result{Output: json.RawMessage(`{"summary":"fix targeted failure"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: "exit 0"})
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"id":"test-1","severity":"error","description":"tests failed with exit code 1","action":"auto-fix"}],"summary":"FAIL: TestFoo"}`

	if _, err := (&TestStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) == 0 {
		t.Fatal("expected the test fixer to be invoked")
	}
	fixPrompt := ag.calls[0].Prompt

	for _, want := range []string{
		"Reproduce the specific failure",
		"Reproduce the specific failing case first",
		"re-run only that focused verification after the fix",
		"Do NOT run the complete repository test suite",
		"Local Test is targeted validation of the failure and the requested intent",
		"remote CI owns broad regression and remains mandatory before a PR is ready",
		"A generic driver or user instruction asking for broad or full-suite confirmation does NOT override this product boundary",
		"Never treat \"do not run everything\" as permission to run nothing",
	} {
		if !strings.Contains(fixPrompt, want) {
			t.Errorf("expected test fixer prompt to contain %q, got:\n%s", want, fixPrompt)
		}
	}
	for _, forbid := range []string{
		"Run the tests, identify failures, and fix either the tests or the code to make them pass",
		"Re-run the relevant tests before finishing",
	} {
		if strings.Contains(fixPrompt, forbid) {
			t.Errorf("test fixer prompt still carries open-ended suite language %q:\n%s", forbid, fixPrompt)
		}
	}
}

// A driver/user instruction that asks for full-suite confirmation must still
// be accompanied by the hard product boundary so the repair agent does not
// treat that instruction as license to expand scope.
func TestTestStep_FixMode_DriverFullSuiteInstructionDoesNotOverrideContract(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			os.WriteFile(filepath.Join(dir, "fix.txt"), []byte("fixed"), 0o644)
			return &agent.Result{Output: json.RawMessage(`{"summary":"fix focused failure"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: "exit 0"})
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"id":"test-1","severity":"error","description":"tests failed with exit code 1","action":"auto-fix","user_instructions":"confirm the full suite path for this failure is green"}],"summary":"FAIL: TestFoo"}`

	if _, err := (&TestStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	fixPrompt := ag.calls[0].Prompt
	if !strings.Contains(fixPrompt, "confirm the full suite path for this failure is green") {
		t.Fatalf("expected previous findings to still carry the driver instruction, got:\n%s", fixPrompt)
	}
	if !strings.Contains(fixPrompt, "A generic driver or user instruction asking for broad or full-suite confirmation does NOT override this product boundary") {
		t.Fatalf("expected product boundary to outrank the driver full-suite instruction, got:\n%s", fixPrompt)
	}
	if !strings.Contains(fixPrompt, "Do NOT run the complete repository test suite") {
		t.Fatalf("expected explicit no-full-suite rule in repair prompt, got:\n%s", fixPrompt)
	}
}

// Honest failure reporting when no targeted check can establish intent must
// remain mandatory; the no-full-suite rule must not collapse into "skip tests".
func TestTestStep_InitialAgent_NoTargetedEvidenceRequiresHonestFinding(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"findings":[{"severity":"warning","description":"no targeted test can prove the intent","action":"ask-user"}],"summary":"missing evidence","tested":["manual review of changed packages"],"testing_summary":"could not produce targeted evidence"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.UserIntent = "Prove the checkout success screen works end-to-end"

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected missing targeted evidence to require approval")
	}
	prompt := ag.calls[0].Prompt
	for _, want := range []string{
		"Never treat \"do not run everything\" as permission to run nothing",
		"write or improve a focused test",
		"perform manual verification with evidence",
		"report a warning finding that sufficient targeted evidence is not possible",
		"If sufficient evidence is not possible, report a warning finding",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("expected no-targeted-evidence guidance %q in prompt:\n%s", want, prompt)
		}
	}
}

func TestTestStep_PublishesOnlyImagesAndPreservesTemporaryTextEvidence(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	var textPath string
	var imagePath string
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			evidenceDir := filepath.Join(testEvidenceRoot(), "run-1")
			textPath = filepath.Join(evidenceDir, "transcript.txt")
			imagePath = filepath.Join(evidenceDir, "screen.png")
			if err := os.WriteFile(textPath, []byte("checkout completed"), 0o644); err != nil {
				return nil, err
			}
			if err := os.WriteFile(imagePath, testPNGBytes(), 0o644); err != nil {
				return nil, err
			}
			output := fmt.Sprintf(`{"findings":[],"summary":"","tested":["manual checkout"],"artifacts":[{"kind":"log","label":"Transcript","path":%q},{"kind":"screenshot","label":"Screen","path":%q}]}`, textPath, imagePath)
			return &agent.Result{Output: json.RawMessage(output)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.ID = "run-1"
	sctx.Run.Branch = "feature"
	sctx.UserIntent = "Show checkout success"
	sctx.Config.Test.Evidence = config.Evidence{StoreInRepo: true, Dir: "evidence"}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Join(testEvidenceRoot(), sctx.Run.ID)) })

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}

	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatal(err)
	}
	if findings.Artifacts[0].Path != textPath {
		t.Fatalf("text evidence path changed: %#v", findings.Artifacts[0])
	}
	if filepath.IsAbs(findings.Artifacts[1].Path) || !strings.HasPrefix(findings.Artifacts[1].Path, "evidence/"+generatedEvidenceDir+"/feature/") {
		t.Fatalf("image evidence was not published to a repository path: %#v", findings.Artifacts[1])
	}
	if _, err := os.Stat(textPath); err != nil {
		t.Fatalf("temporary text evidence was not preserved: %v", err)
	}
	if _, err := os.Stat(imagePath); err != nil {
		t.Fatalf("source image evidence was not preserved: %v", err)
	}
}
