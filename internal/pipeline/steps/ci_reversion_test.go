package steps

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// reviewedWorkflowPinRepo builds the repository shape of AGFloorPlanner PR 2891:
// a default branch that pins six reviewed workflows by fingerprint, a signature
// over that store, and a SKILL.md policy list that states the same count; then a
// branch commit carrying the captain's reviewed approval of a seventh workflow.
//
// The returned dir is a real git worktree whose HEAD is the branch commit.
func reviewedWorkflowPinRepo(t *testing.T) (dir, baseSHA, headSHA string) {
	t.Helper()
	dir = t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")

	mustMkdirAll(t, filepath.Join(dir, ".github", "workflows"))
	reviewed := []string{"build", "deploy", "lint", "release", "test", "wiki-sync"}
	for _, name := range reviewed {
		mustWrite(t, filepath.Join(dir, ".github", "workflows", name+".yml"), "name: "+name+"\n")
	}
	var store strings.Builder
	for _, name := range reviewed {
		store.WriteString("sha256:" + name + "-fingerprint " + name + ".yml\n")
	}
	mustWrite(t, filepath.Join(dir, ".github", "workflow-fingerprints"), store.String())
	// The signature is the captain's, over the six-line store. The incident left
	// it untouched, which is why the repair's six-line store still verified.
	mustWrite(t, filepath.Join(dir, ".github", "workflow-fingerprints.sig"), "captain-signature-over-six\n")
	mustWrite(t, filepath.Join(dir, "SKILL.md"), skillMD(reviewed))

	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "pin six reviewed workflows")
	baseSHA = gitCmd(t, dir, "rev-parse", "HEAD")

	gitCmd(t, dir, "checkout", "-b", "feature")
	approved := append(append([]string{}, reviewed...), "wiki-drift-monitor")
	mustWrite(t, filepath.Join(dir, ".github", "workflows", "wiki-drift-monitor.yml"), "name: wiki-drift-monitor\n")
	store.WriteString("sha256:wiki-drift-monitor-fingerprint wiki-drift-monitor.yml\n")
	mustWrite(t, filepath.Join(dir, ".github", "workflow-fingerprints"), store.String())
	mustWrite(t, filepath.Join(dir, "SKILL.md"), skillMD(approved))
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "review and pin wiki-drift-monitor")
	headSHA = gitCmd(t, dir, "rev-parse", "HEAD")
	return dir, baseSHA, headSHA
}

func skillMD(workflows []string) string {
	var b strings.Builder
	b.WriteString("# Workflow pin authority\n\n")
	b.WriteString("Reviewed workflow count: " + countWord(len(workflows)) + "\n\n")
	b.WriteString("## POLICY\n\n")
	for _, name := range workflows {
		b.WriteString("- " + name + ".yml: reviewed\n")
	}
	return b.String()
}

func countWord(n int) string {
	switch n {
	case 6:
		return "six"
	case 7:
		return "seven"
	default:
		return "unknown"
	}
}

func mustMkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// applyOccurrence3Repair reproduces commit 674f728c: the CI fix agent chases the
// red workflow-pin check by deleting the content the pre-existing signature was
// supposed to cover, until the old signature fits the store again. It never
// touches the .sig file and never removes the workflow itself.
func applyOccurrence3Repair(t *testing.T, dir string) {
	t.Helper()
	reviewed := []string{"build", "deploy", "lint", "release", "test", "wiki-sync"}
	var store strings.Builder
	for _, name := range reviewed {
		store.WriteString("sha256:" + name + "-fingerprint " + name + ".yml\n")
	}
	mustWrite(t, filepath.Join(dir, ".github", "workflow-fingerprints"), store.String())
	mustWrite(t, filepath.Join(dir, "SKILL.md"), skillMD(reviewed))
}

// TestCIRepair_RefusesToUndoAReviewedWorkflowApproval reproduces occurrence 3 of
// fm-nomistakes-fixer-reverses-decisions (AGFloorPlanner PR 2891, commit
// 674f728c) and pins the required behaviour: the CI repair must refuse instead
// of committing a change whose whole effect is to undo the branch's reviewed
// approval so a stale signature verifies again.
func TestCIRepair_RefusesToUndoAReviewedWorkflowApproval(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := reviewedWorkflowPinRepo(t)
	applyOccurrence3Repair(t, dir)

	// Fidelity of the reproduction: this is exactly the state the incident left
	// behind - the unreviewed workflow still on disk, its fingerprint gone from
	// the store, and the captain's six-line signature untouched, so the pin
	// reads GREEN over a workflow nobody reviewed.
	if _, err := os.Stat(filepath.Join(dir, ".github", "workflows", "wiki-drift-monitor.yml")); err != nil {
		t.Fatalf("reproduction should leave the unreviewed workflow on disk: %v", err)
	}
	storeNow := readFileString(t, filepath.Join(dir, ".github", "workflow-fingerprints"))
	if strings.Contains(storeNow, "wiki-drift-monitor") {
		t.Fatal("reproduction should have removed the workflow's fingerprint from the store")
	}
	if got := gitCmd(t, dir, "show", baseSHA+":.github/workflow-fingerprints"); strings.TrimSpace(got) != strings.TrimSpace(storeNow) {
		t.Fatal("reproduction should have restored the store to its pre-branch content")
	}

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"

	step := &CIStep{}
	changed, err := step.commitRepair(sctx, "restore workflow pin")
	if err == nil {
		t.Fatalf("CI repair committed a reversion of the branch's reviewed approval (changed=%v, head now %s): "+
			"the workflow-fingerprint store and SKILL.md are byte-identical to base %s while "+
			".github/workflows/wiki-drift-monitor.yml remains on disk, so the pin is green over an unreviewed workflow",
			changed, gitCmd(t, dir, "rev-parse", "HEAD"), baseSHA)
	}
	var reversion *decisionReversionError
	if !errors.As(err, &reversion) {
		t.Fatalf("commitRepair error = %v, want a decision-reversion refusal", err)
	}
	if changed {
		t.Error("a refused repair must not report a changed head")
	}
	if head := gitCmd(t, dir, "rev-parse", "HEAD"); head != headSHA {
		t.Fatalf("head = %s, want the branch head %s left untouched", head, headSHA)
	}
	report := reversion.Error()
	for _, want := range []string{".github/workflow-fingerprints", "SKILL.md"} {
		if !strings.Contains(report, want) {
			t.Errorf("refusal does not name %s: %s", want, report)
		}
	}
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// branchRepo builds a two-commit repository: a base commit carrying the files in
// base, and a branch commit carrying the files in branch. A nil value in branch
// deletes the file.
func branchRepo(t *testing.T, base, branch map[string]string) (dir, baseSHA, headSHA string) {
	t.Helper()
	dir = t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	for path, content := range base {
		mustMkdirAll(t, filepath.Dir(filepath.Join(dir, path)))
		mustWrite(t, filepath.Join(dir, path), content)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "base")
	baseSHA = gitCmd(t, dir, "rev-parse", "HEAD")

	gitCmd(t, dir, "checkout", "-b", "feature")
	for path, content := range branch {
		full := filepath.Join(dir, path)
		if content == "" {
			if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			continue
		}
		mustMkdirAll(t, filepath.Dir(full))
		mustWrite(t, full, content)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "branch work")
	headSHA = gitCmd(t, dir, "rev-parse", "HEAD")
	return dir, baseSHA, headSHA
}

func repairContext(t *testing.T, dir, baseSHA, headSHA string) *pipeline.StepContext {
	t.Helper()
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	return sctx
}

const needleClosure = `func expandTestReferenceNeedles(ref string) []string {
	// measured inert: every caller already routes through the map
	needles := []string{ref}
	for _, alias := range aliasesFor(ref) {
		needles = append(needles, alias)
	}
	return needles
}
`

// TestCIRepair_RefusesToReinstateContentADecisionRemoved reproduces occurrence 1
// (kunchenguid/firstmate PR 3098, commit faca4c93): a recorded ask-user decision
// removed a closure, and the CI fix round put it back. A reversion is invisible
// in the branch's own base..head diff, which is exactly why nothing noticed.
func TestCIRepair_RefusesToReinstateContentADecisionRemoved(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := branchRepo(t,
		map[string]string{"refs.go": "package refs\n\n" + needleClosure + "\nfunc route() {}\n"},
		map[string]string{"refs.go": "package refs\n\nfunc route() {}\n"},
	)
	// The fix agent reinstates the closure the decision removed, and adds a test
	// for it, so the file is NOT byte-identical to base.
	mustWrite(t, filepath.Join(dir, "refs.go"), "package refs\n\n"+needleClosure+"\nfunc route() {}\n\nfunc TestNeedles() {}\n")

	step := &CIStep{}
	changed, err := step.commitRepair(repairContext(t, dir, baseSHA, headSHA), "restore needle expansion")
	if err == nil {
		t.Fatalf("CI repair reinstated content a recorded decision removed (changed=%v)", changed)
	}
	var reversion *decisionReversionError
	if !errors.As(err, &reversion) {
		t.Fatalf("commitRepair error = %v, want a decision-reversion refusal", err)
	}
	if !strings.Contains(reversion.Error(), "refs.go") {
		t.Errorf("refusal does not name refs.go: %s", reversion.Error())
	}
}

// TestCIRepair_AllowsAnOrdinaryRepairOfTheBranchsOwnCode is the precision half:
// modifying a line the branch itself added is the ordinary CI repair and must
// not park. Nothing of the pre-branch content is restored by it.
func TestCIRepair_AllowsAnOrdinaryRepairOfTheBranchsOwnCode(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := branchRepo(t,
		map[string]string{"path_test.go": "package p\n\nfunc TestA(t *testing.T) {}\n"},
		map[string]string{"path_test.go": "package p\n\nfunc TestA(t *testing.T) {}\n\nfunc TestB(t *testing.T) {\n\twant := \"a/b\"\n}\n"},
	)
	// Windows CRLF repair of the branch's own new test line.
	mustWrite(t, filepath.Join(dir, "path_test.go"),
		"package p\n\nfunc TestA(t *testing.T) {}\n\nfunc TestB(t *testing.T) {\n\twant := filepath.Join(\"a\", \"b\")\n}\n")

	step := &CIStep{}
	changed, err := step.commitRepair(repairContext(t, dir, baseSHA, headSHA), "make the path test cross-platform")
	if err != nil {
		t.Fatalf("ordinary CI repair was refused: %v", err)
	}
	if !changed {
		t.Fatal("ordinary CI repair should commit")
	}
}

// TestCIRepair_AllowsRepairsThatOnlyAddNewContent covers the other common shape:
// the repair adds a file and appends to one the branch created.
func TestCIRepair_AllowsRepairsThatOnlyAddNewContent(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := branchRepo(t,
		map[string]string{"main.go": "package main\n\nfunc main() {}\n"},
		map[string]string{"main.go": "package main\n\nfunc main() { run() }\n", "run.go": "package main\n\nfunc run() {}\n"},
	)
	mustWrite(t, filepath.Join(dir, "run.go"), "package main\n\nimport \"os\"\n\nfunc run() { _ = os.Getenv(\"X\") }\n")
	mustWrite(t, filepath.Join(dir, "run_test.go"), "package main\n\nfunc TestRun() { run() }\n")

	step := &CIStep{}
	if _, err := step.commitRepair(repairContext(t, dir, baseSHA, headSHA), "add the missing import"); err != nil {
		t.Fatalf("additive CI repair was refused: %v", err)
	}
}

// TestCIRepair_RefusesToDeleteAFileTheBranchAdded covers total erasure by
// deletion: the file returns to its pre-branch state (absent).
func TestCIRepair_RefusesToDeleteAFileTheBranchAdded(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := branchRepo(t,
		map[string]string{"main.go": "package main\n\nfunc main() {}\n"},
		map[string]string{"feature.go": "package main\n\nfunc feature() string { return \"new\" }\n"},
	)
	if err := os.Remove(filepath.Join(dir, "feature.go")); err != nil {
		t.Fatal(err)
	}

	step := &CIStep{}
	_, err := step.commitRepair(repairContext(t, dir, baseSHA, headSHA), "drop the failing feature")
	var reversion *decisionReversionError
	if !errors.As(err, &reversion) {
		t.Fatalf("commitRepair error = %v, want a decision-reversion refusal", err)
	}
	if !strings.Contains(reversion.Error(), "feature.go") {
		t.Errorf("refusal does not name feature.go: %s", reversion.Error())
	}
}

// TestCIRepair_HonoursAnExplicitUserFixDecision proves the escape hatch: once a
// person has seen the evidence and answered the gate with a fix selection, the
// same repair is authorised rather than parked again.
func TestCIRepair_HonoursAnExplicitUserFixDecision(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := reviewedWorkflowPinRepo(t)
	applyOccurrence3Repair(t, dir)

	sctx := repairContext(t, dir, baseSHA, headSHA)
	sctx.Fixing = true

	step := &CIStep{}
	changed, err := step.commitRepair(sctx, "restore workflow pin")
	if err != nil {
		t.Fatalf("an explicitly requested fix round was still refused: %v", err)
	}
	if !changed {
		t.Fatal("an explicitly requested fix round should commit")
	}
}

// TestCIFixPrompt_CarriesRecordedDecisionsAndTheNonReversionRule pins the
// advisory half of the fix: occurrence 1's CI repair reversed an ask-user
// decision that had been recorded minutes earlier in the same run, and this was
// the one step prompt that never carried the round history.
func TestCIFixPrompt_CarriesRecordedDecisionsAndTheNonReversionRule(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := branchRepo(t,
		map[string]string{"main.go": "package main\n\nfunc main() {}\n"},
		map[string]string{"main.go": "package main\n\nfunc main() { run() }\n"},
	)

	var prompts []string
	ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		prompts = append(prompts, opts.Prompt)
		return &agent.Result{}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"

	step := &CIStep{}
	host := &recordingDecisionHost{}
	if _, err := step.autoFixCI(sctx, host, &scm.PR{Number: "1"}, []string{"build"}, false); err != nil {
		t.Fatalf("autoFixCI: %v", err)
	}
	if len(prompts) != 1 {
		t.Fatalf("agent invocations = %d, want 1", len(prompts))
	}
	if !strings.Contains(prompts[0], "Never repair a check by undoing work this branch deliberately did") {
		t.Errorf("CI fix prompt carries no non-reversion rule:\n%s", prompts[0])
	}
}

// recordingDecisionHost is the minimum scm.Host the fix round needs: it
// advertises no optional capability, so no provider call is made.
type recordingDecisionHost struct{}

func (h *recordingDecisionHost) Provider() scm.Provider          { return scm.ProviderGitHub }
func (h *recordingDecisionHost) Available(context.Context) error { return nil }
func (h *recordingDecisionHost) Capabilities() scm.Capabilities  { return scm.Capabilities{} }
func (h *recordingDecisionHost) FindPR(context.Context, string, string) (*scm.PR, error) {
	return nil, scm.ErrUnsupported
}
func (h *recordingDecisionHost) CreatePR(context.Context, string, string, scm.PRContent) (*scm.PR, error) {
	return nil, scm.ErrUnsupported
}
func (h *recordingDecisionHost) UpdatePR(context.Context, *scm.PR, scm.PRContent) (*scm.PR, error) {
	return nil, scm.ErrUnsupported
}
func (h *recordingDecisionHost) GetPRState(context.Context, *scm.PR) (scm.PRState, error) {
	return scm.PRStateOpen, nil
}
func (h *recordingDecisionHost) GetChecks(context.Context, *scm.PR) ([]scm.Check, error) {
	return nil, scm.ErrUnsupported
}
func (h *recordingDecisionHost) GetMergeableState(context.Context, *scm.PR) (scm.MergeableState, error) {
	return scm.MergeableUnknown, scm.ErrUnsupported
}
func (h *recordingDecisionHost) FetchFailedCheckLogs(context.Context, *scm.PR, string, string, []string) (string, error) {
	return "", scm.ErrUnsupported
}

// TestCIRepair_UnevaluableGuardFailsClosed pins the safety direction: silent
// success is the exact harm this guard exists to prevent, so a guard that cannot
// be evaluated parks rather than waving the repair through. A park costs one
// decision; a missed reversion costs the decision itself.
func TestCIRepair_UnevaluableGuardFailsClosed(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := branchRepo(t,
		map[string]string{"main.go": "package main\n\nfunc main() {}\n"},
		map[string]string{"main.go": "package main\n\nfunc main() { run() }\n"},
	)
	mustWrite(t, filepath.Join(dir, "main.go"), "package main\n\nfunc main() { run(); log() }\n")

	sctx := repairContext(t, dir, baseSHA, headSHA)
	// A run whose recorded base is not readable here: the guard has nothing to
	// compare the repair against.
	sctx.Run.BaseSHA = "0123456789012345678901234567890123456789"

	step := &CIStep{}
	_, err := step.commitRepair(sctx, "add logging")
	var reversion *decisionReversionError
	if !errors.As(err, &reversion) {
		t.Fatalf("commitRepair error = %v, want a fail-closed refusal", err)
	}
	if !strings.Contains(reversion.Error(), "could not be evaluated") {
		t.Fatalf("refusal should say the guard could not be evaluated: %s", reversion.Error())
	}
	if head := gitCmd(t, dir, "rev-parse", "HEAD"); head != headSHA {
		t.Fatal("a refused repair must leave the branch head untouched")
	}
}

// TestCIRepair_IgnoresFilesTheBranchNeverTouched keeps the guard's scope honest:
// only the branch's own contribution is protected, so a repair that happens to
// rewrite an untouched file back to something matching base is not a reversion
// of any decision this branch made.
func TestCIRepair_IgnoresFilesTheBranchNeverTouched(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := branchRepo(t,
		map[string]string{
			"main.go":      "package main\n\nfunc main() {}\n",
			"unrelated.go": "package main\n\nfunc helperThatIsUniqueInBase() {}\n",
		},
		map[string]string{"main.go": "package main\n\nfunc main() { run() }\n"},
	)
	// The repair edits a file the branch never changed, in a way that leaves
	// base content in place.
	mustWrite(t, filepath.Join(dir, "unrelated.go"),
		"package main\n\nfunc helperThatIsUniqueInBase() {}\n\nfunc added() {}\n")

	step := &CIStep{}
	if _, err := step.commitRepair(repairContext(t, dir, baseSHA, headSHA), "extend the helper"); err != nil {
		t.Fatalf("a repair to a file the branch never touched was refused: %v", err)
	}
}

// TestReinstatedBaseLines_IgnoresLinesWithNoIdentityOfTheirOwn is the precision
// rule behind the line signal: only content the pre-branch file carries exactly
// once can prove a specific decision was undone.
func TestReinstatedBaseLines_IgnoresLinesWithNoIdentityOfTheirOwn(t *testing.T) {
	t.Parallel()
	base := "func a() {\n\treturn nil\n}\n\nfunc b() {\n\treturn nil\n}\n\nfunc uniqueHelper() {}\n"
	pre := "func a() {\n}\n\nfunc b() {\n}\n"
	post := "func a() {\n\treturn nil\n}\n\nfunc b() {\n}\n"

	// `return nil` and `}` are not unique in base, so restoring one proves nothing.
	if got := reinstatedBaseLines(base, pre, post); len(got) != 0 {
		t.Fatalf("reinstatedBaseLines = %v, want none for repeated lines", got)
	}
	withUnique := post + "\nfunc uniqueHelper() {}\n"
	got := reinstatedBaseLines(base, pre, withUnique)
	if len(got) != 1 || got[0] != "func uniqueHelper() {}" {
		t.Fatalf("reinstatedBaseLines = %v, want the unique base line", got)
	}
}

// TestCIRepair_RefusesAReversionTheFixAgentCommittedItself closes the path a
// HEAD-anchored guard would miss entirely. A fix agent is free to commit its own
// work, and commitRepair accepts that head as the repair; measured against HEAD
// the reversion compares clean against its own commit, so the anchor has to be
// the head the branch had before the fix round started.
func TestCIRepair_RefusesAReversionTheFixAgentCommittedItself(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := reviewedWorkflowPinRepo(t)
	applyOccurrence3Repair(t, dir)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "agent: restore workflow pin")
	agentHead := gitCmd(t, dir, "rev-parse", "HEAD")

	sctx := repairContext(t, dir, baseSHA, headSHA)
	step := &CIStep{}
	changed, err := step.commitRepair(sctx, "")
	var reversion *decisionReversionError
	if !errors.As(err, &reversion) {
		t.Fatalf("commitRepair error = %v (changed=%v), want a decision-reversion refusal for the agent's own commit %s", err, changed, agentHead)
	}
	if sctx.Run.HeadSHA != headSHA {
		t.Fatalf("recorded head = %s, want the pre-repair head %s: a refused repair must not be adopted", sctx.Run.HeadSHA, headSHA)
	}
	if !strings.Contains(reversion.Error(), ".github/workflow-fingerprints") {
		t.Errorf("refusal does not name the reverted store: %s", reversion.Error())
	}
}
