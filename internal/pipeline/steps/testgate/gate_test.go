// Package testgate holds the Test step's diff-class gate tests that drive a
// whole step execution.
//
// They live outside internal/pipeline/steps for the reason citest does: each
// case stands up a git repository and runs the step, and on Windows those
// subprocess spawns cost roughly ten times what they cost on Linux. The
// parent package measured 647s of a 900s budget on the windows-steps shard
// before this gate existed, so seventeen more step executions there put it
// over the cap with no test failing. A sibling package gets its own budget
// and runs concurrently with the parent.
//
// The gate's cheap white-box unit tests (the pattern matcher, the fail-open
// classification, the PR line renderer) stay in the parent package, where
// they cost nothing and can reach package internals.
//
// What these tests pin: the live-evidence agent - the most expensive turn in
// the pipeline - runs only when the run's diff touches a product file, or
// when no earlier go verdict on this branch already covers the current
// product state. The agent-invocation count is the assertion that matters; a
// gate that records the right verdict while still paying for the turn has not
// done its job.
package testgate

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps/internal/stepstest"
	"github.com/kunchenguid/no-mistakes/internal/reviewqa"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// passingScenarioFindingsJSON is a go verdict with one live passing scenario:
// the answer a real evidence turn gives when it ran and everything worked.
const passingScenarioFindingsJSON = `{
  "findings": [],
  "summary": "",
  "tested": ["npm run e2e -- checkout"],
  "testing_summary": "drove checkout end to end",
  "artifacts": [],
  "scenarios": [{"name":"user reaches the success screen","result":"pass","live":true,"evidence":"checkout.png","reason":""}],
  "verdict": "go"
}`

// gateAgent returns a mock agent that answers every evidence turn with a
// passing payload, so any invocation count above zero means the gate let the
// turn run.
func gateAgent() *stepstest.MockAgent {
	return &stepstest.MockAgent{
		AgentName: "test",
		RunFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(passingScenarioFindingsJSON)}, nil
		},
	}
}

// commitFiles writes files onto the current branch of dir and returns the new
// head SHA, so a case can shape the run's diff by path class.
func commitFiles(t *testing.T, dir, message string, files map[string]string) string {
	t.Helper()
	for name, body := range files {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	stepstest.GitCmd(t, dir, "add", "-A")
	stepstest.GitCmd(t, dir, "commit", "-m", message)
	return stepstest.GitCmd(t, dir, "rev-parse", "HEAD")
}

// gateContext builds a Test-step context with the gate OPTED IN and the
// shipped default classification, which is what a real run gets from
// config.Merge for a repository whose trusted .no-mistakes.yaml sets
// test.evidence_gate: diff-class.
//
// Every case below therefore describes the opt-in behavior.
// TestTestStep_GateOffRunsTheAgentAndRecordsNothing is the one that leaves
// the key unset, and it is the proof the default is unchanged.
func gateContext(t *testing.T, ag agent.Agent, dir, baseSHA, headSHA string) *pipeline.StepContext {
	t.Helper()
	sctx := gateOffContext(t, ag, dir, baseSHA, headSHA)
	sctx.Config.Test.EvidenceGate = config.TestEvidenceGateDiffClass
	return sctx
}

// gateOffContext is gateContext without the opt-in: test.evidence_gate unset,
// which config.Merge resolves to config.DefaultTestEvidenceGate.
func gateOffContext(t *testing.T, ag agent.Agent, dir, baseSHA, headSHA string) *pipeline.StepContext {
	t.Helper()
	sctx := stepstest.NewTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.HeadSHA = headSHA
	sctx.Config.Test.EvidenceGate = config.DefaultTestEvidenceGate
	sctx.Config.Test.NonProductPaths = append([]string(nil), config.DefaultNonProductPaths...)
	setRunIntent(t, sctx, sctx.Run.ID, gateTestIntent)
	return sctx
}

// gateTestIntent is the user intent every case shares by default. Reuse
// requires the prior run and this run to carry the SAME intent, so a case that
// means to exercise some OTHER reuse condition has to satisfy this one first -
// otherwise it would pass while the intent rule alone declined reuse and the
// condition it was written for was never reached.
const gateTestIntent = "ship the checkout success screen"

// setRunIntent records intent on a run both in the database, which is where
// reuse reads the PRIOR run's intent from, and on the in-memory run, which is
// where it reads THIS run's. An empty intent leaves the run without one.
func setRunIntent(t *testing.T, sctx *pipeline.StepContext, runID, intent string) {
	t.Helper()
	if intent == "" {
		return
	}
	if err := sctx.DB.UpdateRunIntent(runID, db.RunIntent{Summary: intent, Source: "user", SessionID: "test", Score: 1}); err != nil {
		t.Fatal(err)
	}
	if sctx.Run != nil && sctx.Run.ID == runID {
		sctx.Run.Intent = &intent
	}
}

func parseOutcomeFindings(t *testing.T, outcome *pipeline.StepOutcome) types.Findings {
	t.Helper()
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatalf("parse step findings: %v\n%s", err, outcome.Findings)
	}
	return findings
}

// shortSHA mirrors the steps package's own abbreviation, which the reuse
// reason is rendered with.
func shortSHA(sha string) string {
	if len(sha) <= 12 {
		return sha
	}
	return sha[:12]
}

// recordPriorGoVerdict inserts a completed Test step on ANOTHER run of the
// same branch that recorded a go verdict at headSHA, which is the evidence a
// later run may reuse.
func recordPriorGoVerdict(t *testing.T, sctx *pipeline.StepContext, headSHA string) string {
	t.Helper()
	return recordPriorVerdictWithIntent(t, sctx, headSHA, types.TestVerdictGo, gateTestIntent)
}

// recordPriorVerdict is recordPriorGoVerdict for any verdict, so a case can
// shape what this branch's evidence history actually says.
func recordPriorVerdict(t *testing.T, sctx *pipeline.StepContext, headSHA, verdict string) string {
	t.Helper()
	return recordPriorVerdictWithIntent(t, sctx, headSHA, verdict, gateTestIntent)
}

// recordParkedPriorVerdict records a verdict on another run of this branch
// whose Test step then PARKED rather than completing, which is what every
// blocking verdict does and what a daemon crash, an abort, or a skip leaves
// behind. The payload survives all of those, so the verdict is still this
// branch's latest evidence and must still be the one reuse consults.
func recordParkedPriorVerdict(t *testing.T, sctx *pipeline.StepContext, headSHA, verdict string) string {
	t.Helper()
	return recordPriorVerdictSettled(t, sctx, headSHA, verdict, gateTestIntent, func(runID, stepID, payload string) {
		if err := sctx.DB.ParkStepForApproval(runID, stepID, types.StepStatusAwaitingApproval, 0, 1, &payload); err != nil {
			t.Fatal(err)
		}
	})
}

// recordPriorVerdictWithIntent additionally chooses the intent that prior run
// was validated under; an empty one leaves it with none.
func recordPriorVerdictWithIntent(t *testing.T, sctx *pipeline.StepContext, headSHA, verdict, intent string) string {
	t.Helper()
	return recordPriorVerdictSettled(t, sctx, headSHA, verdict, intent, func(_, stepID, payload string) {
		if err := sctx.DB.SetStepFindings(stepID, payload); err != nil {
			t.Fatal(err)
		}
		if err := sctx.DB.CompleteStep(stepID, 0, 1, ""); err != nil {
			t.Fatal(err)
		}
	})
}

// recordPriorVerdictSettled builds the prior run and its recorded payload, and
// leaves how that step ended to the caller.
func recordPriorVerdictSettled(t *testing.T, sctx *pipeline.StepContext, headSHA, verdict, intent string, settle func(runID, stepID, payload string)) string {
	t.Helper()
	run, err := sctx.DB.InsertRun(sctx.Run.RepoID, sctx.Run.Branch, headSHA, sctx.Run.BaseSHA)
	if err != nil {
		t.Fatal(err)
	}
	setRunIntent(t, sctx, run.ID, intent)
	step, err := sctx.DB.InsertStepResult(run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	findings := types.Findings{
		Tested:         []string{"npm run e2e -- checkout"},
		TestingSummary: "drove checkout end to end",
		Scenarios: []types.TestScenario{{
			Name:     "user reaches the success screen",
			Result:   types.ScenarioResultPass,
			Live:     true,
			Evidence: "checkout.png",
		}},
		Artifacts:      []types.TestArtifact{{Label: "checkout", Path: filepath.Join(filepath.Dir(sctx.EvidenceDir), run.ID, "checkout.png")}},
		Verdict:        verdict,
		TestedHeadSHA:  headSHA,
		EvidenceSource: types.TestEvidenceSourceAgent,
	}
	// A run that drove the agent left its artifacts on disk, in its own
	// evidence directory. The reuse path copies them into the reusing run's
	// directory, so a fixture without them would exercise the fallback rather
	// than the carry.
	writeRunEvidence(t, sctx, run.ID, "checkout.png")
	raw, err := json.Marshal(findings)
	if err != nil {
		t.Fatal(err)
	}
	settle(run.ID, step.ID, string(raw))
	return run.ID
}

// TestTestStep_ParkedNoGoBlocksReuseOfAnOlderGo: the newest RECORDED verdict
// is what reuse consults, and a blocking verdict's own step never completes -
// it parks for a human, and a daemon restart, an abort, or a skip all leave it
// non-completed with its payload intact. Consulting only completed steps would
// therefore hide exactly the verdict that must block, and republish an older
// go over a live turn that said no-go at the same product state.
func TestTestStep_ParkedNoGoBlocksReuseOfAnOlderGo(t *testing.T) {
	t.Parallel()
	dir, baseSHA, _ := stepstest.SetupGitRepo(t)
	validated := commitFiles(t, dir, "product change", map[string]string{
		"internal/checkout/checkout.go": "package checkout\n",
	})
	head := commitFiles(t, dir, "docs follow-up", map[string]string{"docs/guide.md": "# guide\n"})
	ag := gateAgent()
	sctx := gateContext(t, ag, dir, baseSHA, head)

	// Both name the head the go verdict validated, so no product file moved
	// since it was earned and only the verdict ordering can decline reuse.
	recordPriorGoVerdict(t, sctx, validated)
	recordParkedPriorVerdict(t, sctx, validated, types.TestVerdictNoGo)

	outcome, err := (&steps.TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ag.Calls) != 1 {
		t.Fatalf("evidence agent invocations = %d, want 1 (a parked no-go is still this branch's newest verdict)", len(ag.Calls))
	}
	if got := parseOutcomeFindings(t, outcome).EvidenceSource; got != types.TestEvidenceSourceAgent {
		t.Fatalf("evidence source = %q, want %q", got, types.TestEvidenceSourceAgent)
	}
}

// TestTestStep_AgentCannotSeedTheEvidenceOriginRunID: evidence_origin_run_id
// is joined into a filesystem path by carryOriginEvidence, and an agent turn's
// raw output is decoded by the same types.ParseFindingsJSON that reads a
// stored payload back, so a turn that emits the key must not have it persist.
// Were it to persist, a later reuse on the branch would copy the named
// directory's contents into this run's evidence directory, which publication
// then pushes to the evidence branch or uploads to the PR.
func TestTestStep_AgentCannotSeedTheEvidenceOriginRunID(t *testing.T) {
	t.Parallel()
	dir, baseSHA, _ := stepstest.SetupGitRepo(t)
	head := commitFiles(t, dir, "product change", map[string]string{
		"internal/checkout/checkout.go": "package checkout\n",
	})
	hostile := `{
  "findings": [],
  "summary": "",
  "tested": ["npm run e2e -- checkout"],
  "testing_summary": "drove checkout end to end",
  "artifacts": [],
  "scenarios": [{"name":"user reaches the success screen","result":"pass","live":true,"evidence":"checkout.png","reason":""}],
  "verdict": "go",
  "evidence_origin_run_id": "../../../../etc"
}`
	ag := &stepstest.MockAgent{
		AgentName: "test",
		RunFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(hostile)}, nil
		},
	}
	sctx := gateContext(t, ag, dir, baseSHA, head)

	outcome, err := (&steps.TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := parseOutcomeFindings(t, outcome).EvidenceOriginRunID; got != "" {
		t.Fatalf("evidence origin run = %q, want it cleared: the agent must not name a directory the pipeline will copy", got)
	}
}

// writeRunEvidence places an evidence file in a run's own evidence directory,
// which is its run ID under the same root the step context's directory sits in
// (see Executor.runEvidenceDir).
func writeRunEvidence(t *testing.T, sctx *pipeline.StepContext, runID, name string) {
	t.Helper()
	dir := filepath.Join(filepath.Dir(sctx.EvidenceDir), runID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte("\x89PNG evidence"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestTestStep_NewerNonGoVerdictBlocksReuseOfAnOlderGo: reuse consults this
// branch's NEWEST verdict only. A no-go recorded after a go is the branch's
// own latest evidence contradicting it, so the older go must not be published
// again even though no product file moved since it was earned.
func TestTestStep_NewerNonGoVerdictBlocksReuseOfAnOlderGo(t *testing.T) {
	t.Parallel()
	dir, baseSHA, _ := stepstest.SetupGitRepo(t)
	validated := commitFiles(t, dir, "product change", map[string]string{
		"internal/checkout/checkout.go": "package checkout\n",
	})
	head := commitFiles(t, dir, "docs follow-up", map[string]string{"docs/guide.md": "# guide\n"})
	ag := gateAgent()
	sctx := gateContext(t, ag, dir, baseSHA, head)

	// Both are recorded at the head the go verdict validated, so the product
	// diff to this head is empty and only the verdict ordering can block reuse.
	recordPriorGoVerdict(t, sctx, validated)
	recordPriorVerdict(t, sctx, validated, types.TestVerdictNoGo)

	outcome, err := (&steps.TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ag.Calls) != 1 {
		t.Fatalf("evidence agent invocations = %d, want 1 (a newer no-go supersedes the older go)", len(ag.Calls))
	}
	if got := parseOutcomeFindings(t, outcome).EvidenceSource; got != types.TestEvidenceSourceAgent {
		t.Fatalf("evidence source = %q, want %q", got, types.TestEvidenceSourceAgent)
	}
}

// TestTestStep_ProductChangeRunsTheEvidenceAgent is the control: a change that
// touches a product file still pays for the live-evidence turn.
func TestTestStep_ProductChangeRunsTheEvidenceAgent(t *testing.T) {
	t.Parallel()
	dir, baseSHA, _ := stepstest.SetupGitRepo(t)
	head := commitFiles(t, dir, "add a product file", map[string]string{
		"internal/checkout/checkout.go": "package checkout\n",
		"docs/checkout.md":              "# checkout\n",
	})
	ag := gateAgent()
	sctx := gateContext(t, ag, dir, baseSHA, head)

	outcome, err := (&steps.TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ag.Calls) != 1 {
		t.Fatalf("evidence agent invocations = %d, want 1", len(ag.Calls))
	}
	findings := parseOutcomeFindings(t, outcome)
	if findings.EvidenceSource != types.TestEvidenceSourceAgent {
		t.Fatalf("evidence source = %q, want %q", findings.EvidenceSource, types.TestEvidenceSourceAgent)
	}
}

// TestTestStep_LockfileOnlyChangeRunsTheEvidenceAgent: a lockfile change swaps
// the dependency versions the product ships and runs, so it is a runtime
// change even though no first-party source moved - and a lockfile-only diff is
// the ordinary shape of a dependabot or renovate bump and of any transitive
// update. Classifying it non-product would give exactly those runs an
// automatic no-surface, which never parks, so a dependency upgrade would ship
// with neither live validation nor a human decision.
func TestTestStep_LockfileOnlyChangeRunsTheEvidenceAgent(t *testing.T) {
	t.Parallel()
	for _, lockfile := range []string{"go.sum", "package-lock.json", "yarn.lock", "Cargo.lock", "poetry.lock", "Gemfile.lock"} {
		t.Run(lockfile, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, _ := stepstest.SetupGitRepo(t)
			if err := os.Remove(filepath.Join(dir, "feature.txt")); err != nil {
				t.Fatal(err)
			}
			head := commitFiles(t, dir, "bump a dependency", map[string]string{lockfile: "pinned\n"})
			ag := gateAgent()
			sctx := gateContext(t, ag, dir, baseSHA, head)

			outcome, err := (&steps.TestStep{}).Execute(sctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(ag.Calls) != 1 {
				t.Fatalf("evidence agent invocations = %d, want 1: a dependency bump changes what runs", len(ag.Calls))
			}
			if got := parseOutcomeFindings(t, outcome).EvidenceSource; got != types.TestEvidenceSourceAgent {
				t.Fatalf("evidence source = %q, want %q", got, types.TestEvidenceSourceAgent)
			}
		})
	}
}

// TestTestStep_NonProductOnlyChangeSkipsTheAgentWithoutParking is the gate's
// whole point. A docs-only run has no live-drivable surface by construction,
// so it records an AUTOMATIC no-surface and proceeds; the agent's own
// no-surface still parks, which TestTestStep_VerdictPolicy pins.
func TestTestStep_NonProductOnlyChangeSkipsTheAgentWithoutParking(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		files map[string]string
	}{
		{name: "docs only", files: map[string]string{"docs/guide.md": "# guide\n", "README.md": "hi\n"}},
		{name: "tests and fixtures only", files: map[string]string{
			"internal/checkout/checkout_test.go":    "package checkout\n",
			"internal/checkout/testdata/golden.txt": "golden\n",
		}},
		{name: "ci workflow only", files: map[string]string{".github/workflows/ci.yml": "name: ci\n"}},
		{name: "scripts only", files: map[string]string{"scripts/release.sh": "#!/bin/sh\n"}},
		{name: "pipeline config only", files: map[string]string{".no-mistakes.yaml": "commands:\n  test: go test ./...\n"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, _ := stepstest.SetupGitRepo(t)
			// feature.txt from the template repo is a product file, so remove
			// it to leave a diff of only this case's path class.
			if err := os.Remove(filepath.Join(dir, "feature.txt")); err != nil {
				t.Fatal(err)
			}
			head := commitFiles(t, dir, "non-product change", tc.files)
			ag := gateAgent()
			sctx := gateContext(t, ag, dir, baseSHA, head)

			outcome, err := (&steps.TestStep{}).Execute(sctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(ag.Calls) != 0 {
				t.Fatalf("evidence agent invocations = %d, want 0 (prompt: %q)", len(ag.Calls), ag.Calls[0].Prompt)
			}
			if outcome.NeedsApproval {
				t.Fatal("an automatic no-surface must not park the step")
			}
			findings := parseOutcomeFindings(t, outcome)
			if findings.Verdict != types.TestVerdictNoSurface {
				t.Fatalf("verdict = %q, want %q", findings.Verdict, types.TestVerdictNoSurface)
			}
			if findings.EvidenceSource != types.TestEvidenceSourceNoProductChange {
				t.Fatalf("evidence source = %q, want %q", findings.EvidenceSource, types.TestEvidenceSourceNoProductChange)
			}
			if findings.EvidenceReason != "no product file in diff" {
				t.Fatalf("evidence reason = %q, want %q", findings.EvidenceReason, "no product file in diff")
			}
			if len(findings.Items) != 0 {
				t.Fatalf("findings = %+v, want none", findings.Items)
			}
		})
	}
}

// TestTestStep_ConfiguredTestCommandStillRunsWhenTheAgentIsSkipped pins the
// half of the ruling the gate must never touch: commands.test runs on every
// run, and its failure still parks the step even though the evidence agent
// was skipped.
func TestTestStep_ConfiguredTestCommandStillRunsWhenTheAgentIsSkipped(t *testing.T) {
	t.Parallel()
	dir, baseSHA, _ := stepstest.SetupGitRepo(t)
	if err := os.Remove(filepath.Join(dir, "feature.txt")); err != nil {
		t.Fatal(err)
	}
	head := commitFiles(t, dir, "docs only", map[string]string{"docs/guide.md": "# guide\n"})
	ag := gateAgent()
	sctx := gateContext(t, ag, dir, baseSHA, head)
	sctx.Config.Commands.Test = "exit 3"

	outcome, err := (&steps.TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ag.Calls) != 0 {
		t.Fatalf("evidence agent invocations = %d, want 0", len(ag.Calls))
	}
	if outcome.ExitCode != 3 {
		t.Fatalf("exit code = %d, want 3 from the configured test command", outcome.ExitCode)
	}
	if !outcome.NeedsApproval {
		t.Fatal("a failing configured test command must still park the step")
	}
	findings := parseOutcomeFindings(t, outcome)
	if len(findings.Items) != 1 || findings.Items[0].Category != types.FindingCategoryTestCommand {
		t.Fatalf("findings = %+v, want one test-command finding", findings.Items)
	}
	if len(findings.Tested) != 1 || findings.Tested[0] != "exit 3" {
		t.Fatalf("tested = %v, want the configured command", findings.Tested)
	}
}

// TestTestStep_ReusesAGoVerdictWhenProductFilesAreUnchanged covers the
// decision-only re-run: the median task needs four runs, and re-buying
// identical evidence on each is what the gate removes.
func TestTestStep_ReusesAGoVerdictWhenProductFilesAreUnchanged(t *testing.T) {
	t.Parallel()
	dir, baseSHA, _ := stepstest.SetupGitRepo(t)
	validated := commitFiles(t, dir, "product change", map[string]string{
		"internal/checkout/checkout.go": "package checkout\n",
	})
	head := commitFiles(t, dir, "docs follow-up", map[string]string{"docs/guide.md": "# guide\n"})
	ag := gateAgent()
	sctx := gateContext(t, ag, dir, baseSHA, head)
	priorRunID := recordPriorGoVerdict(t, sctx, validated)

	outcome, err := (&steps.TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ag.Calls) != 0 {
		t.Fatalf("evidence agent invocations = %d, want 0", len(ag.Calls))
	}
	if outcome.NeedsApproval {
		t.Fatal("a reused go verdict must not park the step")
	}
	findings := parseOutcomeFindings(t, outcome)
	if findings.Verdict != types.TestVerdictGo {
		t.Fatalf("verdict = %q, want %q", findings.Verdict, types.TestVerdictGo)
	}
	if findings.EvidenceSource != types.TestEvidenceSourceReused {
		t.Fatalf("evidence source = %q, want %q", findings.EvidenceSource, types.TestEvidenceSourceReused)
	}
	for _, want := range []string{
		"product files unchanged since " + shortSHA(validated),
		"reused from run " + priorRunID,
	} {
		if !strings.Contains(findings.EvidenceReason, want) {
			t.Fatalf("evidence reason %q omits %q", findings.EvidenceReason, want)
		}
	}
	// The reason is published in the PR body, and the publication redactor
	// only removes home directories - test.evidence.local_root is documented
	// as any absolute path - so the reason must carry no host path at all.
	if dir := filepath.Dir(sctx.EvidenceDir); strings.Contains(findings.EvidenceReason, dir) {
		t.Fatalf("evidence reason %q publishes the host evidence path %q", findings.EvidenceReason, dir)
	}
	if len(findings.Scenarios) != 1 || findings.Scenarios[0].Name != "user reaches the success screen" {
		t.Fatalf("scenarios = %+v, want the reused run's scenario list", findings.Scenarios)
	}
	// The verdict is recorded against the head its scenarios were actually
	// driven at, never restamped onto this one. That is what makes
	// attestedLiveValidation omit live_validation for this head instead of
	// publishing a live claim for a run that drove nothing.
	if findings.TestedHeadSHA != validated {
		t.Fatalf("tested head = %q, want the head the scenarios were driven at %q", findings.TestedHeadSHA, validated)
	}
	if findings.TestedHeadSHA == head {
		t.Fatal("a reused verdict must not be restamped onto this run's head")
	}
	// The verdict is published with the evidence behind it: the originating
	// run's artifacts are carried, and its evidence file physically reaches
	// this run's directory, which is the only one publication reads.
	// The path must be rebased onto THIS run's directory: the renderer accepts
	// an absolute artifact path only under the repository root or this run's
	// evidence directory, so a carried path left naming the originating run is
	// dropped at render time and the carry silently does nothing.
	wantPath := filepath.Join(sctx.EvidenceDir, "checkout.png")
	if len(findings.Artifacts) != 1 || findings.Artifacts[0].Path != wantPath {
		t.Fatalf("artifacts = %+v, want one rebased onto %q", findings.Artifacts, wantPath)
	}
	if findings.EvidenceOriginRunID != priorRunID {
		t.Fatalf("origin run = %q, want %q", findings.EvidenceOriginRunID, priorRunID)
	}
	if _, err := os.Stat(filepath.Join(sctx.EvidenceDir, "checkout.png")); err != nil {
		t.Fatalf("the originating run's evidence file did not reach this run's directory: %v", err)
	}
}

// TestTestStep_ReusesAGoVerdictRecordedAtThisRunsOwnHead covers the reuse with
// no new commits at all - a re-run of the branch at the same commit, which is
// the decision-only re-run the gate exists for and the shape production hits
// most often. It has no short-circuit of its own: the general path diffs the
// prior verdict's head against this one, git reports a commit against itself
// as empty, and the reuse is granted through the same acceptance point every
// other case uses.
func TestTestStep_ReusesAGoVerdictRecordedAtThisRunsOwnHead(t *testing.T) {
	t.Parallel()
	dir, baseSHA, _ := stepstest.SetupGitRepo(t)
	head := commitFiles(t, dir, "product change", map[string]string{
		"internal/checkout/checkout.go": "package checkout\n",
	})
	ag := gateAgent()
	sctx := gateContext(t, ag, dir, baseSHA, head)
	priorRunID := recordPriorGoVerdict(t, sctx, head)

	outcome, err := (&steps.TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ag.Calls) != 0 {
		t.Fatalf("evidence agent invocations = %d, want 0 (nothing moved since the verdict was earned)", len(ag.Calls))
	}
	if outcome.NeedsApproval {
		t.Fatal("a reused go verdict must not park the step")
	}
	findings := parseOutcomeFindings(t, outcome)
	if findings.Verdict != types.TestVerdictGo {
		t.Fatalf("verdict = %q, want %q", findings.Verdict, types.TestVerdictGo)
	}
	if findings.EvidenceSource != types.TestEvidenceSourceReused {
		t.Fatalf("evidence source = %q, want %q", findings.EvidenceSource, types.TestEvidenceSourceReused)
	}
	if !strings.Contains(findings.EvidenceReason, "reused from run "+priorRunID) {
		t.Fatalf("evidence reason %q does not name the run that drove the scenarios", findings.EvidenceReason)
	}
	if findings.TestedHeadSHA != head {
		t.Fatalf("tested head = %q, want the head the scenarios were driven at %q", findings.TestedHeadSHA, head)
	}
}

// TestTestStep_ReuseLeavesThisRunsReviewConversationAlone: the run's evidence
// directory is its evidence ROOT, not an artifact bucket, and the review
// conversation lives inside it at reviewqa.Dir. Carrying the originating run's
// directory wholesale replaced THIS run's questions and answers with an earlier
// run's, which the PR body then published as this review's conversation - and
// with an earlier run's question reading as open at a gate this run's operator
// never saw. Only the files the carried artifacts name are copied.
func TestTestStep_ReuseLeavesThisRunsReviewConversationAlone(t *testing.T) {
	t.Parallel()
	dir, baseSHA, _ := stepstest.SetupGitRepo(t)
	validated := commitFiles(t, dir, "product change", map[string]string{
		"internal/checkout/checkout.go": "package checkout\n",
	})
	head := commitFiles(t, dir, "docs follow-up", map[string]string{"docs/guide.md": "# guide\n"})
	ag := gateAgent()
	sctx := gateContext(t, ag, dir, baseSHA, head)
	priorRunID := recordPriorGoVerdict(t, sctx, validated)

	priorDir := filepath.Join(filepath.Dir(sctx.EvidenceDir), priorRunID)
	writeConversation(t, priorDir, "q1", "the originating run's question")
	writeConversation(t, sctx.EvidenceDir, "q2", "this run's own question")

	outcome, err := (&steps.TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ag.Calls) != 0 {
		t.Fatalf("evidence agent invocations = %d, want 0", len(ag.Calls))
	}
	// The real evidence still arrives, so this is not passing by not carrying.
	findings := parseOutcomeFindings(t, outcome)
	wantPath := filepath.Join(sctx.EvidenceDir, "checkout.png")
	if len(findings.Artifacts) != 1 || findings.Artifacts[0].Path != wantPath {
		t.Fatalf("artifacts = %+v, want one rebased onto %q", findings.Artifacts, wantPath)
	}
	if _, err := os.Stat(wantPath); err != nil {
		t.Fatalf("the originating run's evidence file did not reach this run's directory: %v", err)
	}
	for name, want := range conversationContents("q2", "this run's own question") {
		got, err := os.ReadFile(filepath.Join(reviewqa.Dir(sctx.EvidenceDir), name))
		if err != nil {
			t.Fatalf("reading this run's %s: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("this run's %s = %q, want its own %q", name, got, want)
		}
	}
}

// writeConversation seeds a run's review conversation the way the reviewer and
// the daemon's answer handler do, under the run's own evidence directory.
func writeConversation(t *testing.T, evidenceDir, questionID, question string) {
	t.Helper()
	dir := reviewqa.Dir(evidenceDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range conversationContents(questionID, question) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func conversationContents(questionID, question string) map[string]string {
	return map[string]string{
		"questions.ndjson": `{"id":"` + questionID + `","question":"` + question + `"}` + "\n",
		"answers.ndjson":   `{"id":"` + questionID + `","answer":"answered"}` + "\n",
	}
}

// TestTestStep_IntentChangeDefeatsReuse: the evidence turn derives its
// scenarios from the run's user intent, and an --intent supplied one is
// authoritative acceptance criteria. A refined intent at a byte-identical
// product state therefore has criteria no earlier scenario ever exercised, so
// republishing the earlier go would publish a verdict for them. Absence on
// either side is a difference too - "no recorded intent" is an unknown, and
// two unknowns are not evidence of sameness - so every case here must still
// pay for the turn.
func TestTestStep_IntentChangeDefeatsReuse(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		priorIntent string
		thisIntent  string
	}{
		{name: "refined intent", priorIntent: gateTestIntent, thisIntent: gateTestIntent + "; must reject an expired token with 401"},
		{name: "absent on this run", priorIntent: gateTestIntent, thisIntent: ""},
		{name: "absent on the prior run", priorIntent: "", thisIntent: gateTestIntent},
		{name: "absent on both", priorIntent: "", thisIntent: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, _ := stepstest.SetupGitRepo(t)
			validated := commitFiles(t, dir, "product change", map[string]string{
				"internal/checkout/checkout.go": "package checkout\n",
			})
			head := commitFiles(t, dir, "docs follow-up", map[string]string{"docs/guide.md": "# guide\n"})
			ag := gateAgent()
			sctx := gateContext(t, ag, dir, baseSHA, head)
			sctx.Run.Intent = nil
			if tc.thisIntent != "" {
				mine := tc.thisIntent
				sctx.Run.Intent = &mine
			}
			recordPriorVerdictWithIntent(t, sctx, validated, types.TestVerdictGo, tc.priorIntent)

			outcome, err := (&steps.TestStep{}).Execute(sctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(ag.Calls) != 1 {
				t.Fatalf("evidence agent invocations = %d, want 1 (the verdict was earned under other criteria)", len(ag.Calls))
			}
			if got := parseOutcomeFindings(t, outcome).EvidenceSource; got != types.TestEvidenceSourceAgent {
				t.Fatalf("evidence source = %q, want %q", got, types.TestEvidenceSourceAgent)
			}
		})
	}
}

// TestTestStep_ChainedReuseKeepsPointingAtTheRunThatHoldsTheEvidence drives
// the flagship case - agent, then two decision-only re-runs - and pins the
// provenance. A gated run's own evidence directory is created before the gate
// is consulted and then left empty, so naming the immediate predecessor from
// the third run onward would send a reviewer to an empty directory as the
// basis for a go verdict.
func TestTestStep_ChainedReuseKeepsPointingAtTheRunThatHoldsTheEvidence(t *testing.T) {
	t.Parallel()
	dir, baseSHA, _ := stepstest.SetupGitRepo(t)
	validated := commitFiles(t, dir, "product change", map[string]string{
		"internal/checkout/checkout.go": "package checkout\n",
	})

	// R1 drives the agent and is the only run that holds artifacts.
	r1Agent := gateAgent()
	r1 := gateContext(t, r1Agent, dir, baseSHA, validated)
	r1Outcome, err := (&steps.TestStep{}).Execute(r1)
	if err != nil {
		t.Fatal(err)
	}
	if len(r1Agent.Calls) != 1 {
		t.Fatalf("R1 evidence agent invocations = %d, want 1", len(r1Agent.Calls))
	}
	persistTestOutcome(t, r1, r1.Run.ID, r1Outcome)

	// R2 and R3 advance the branch with docs only and reuse in turn, sharing
	// R1's database so each lookup is the real query over the previous run's
	// recorded findings rather than a hand-built fixture.
	r2Head := commitFiles(t, dir, "docs follow-up", map[string]string{"docs/guide.md": "# guide\n"})
	r2Agent := gateAgent()
	r2 := reRunOnSameBranch(t, r1, r2Agent, r2Head)
	r2Findings := executeReusingRun(t, r2, r2Agent, "R2")

	r3Head := commitFiles(t, dir, "more docs", map[string]string{"docs/more.md": "# more\n"})
	r3Agent := gateAgent()
	r3 := reRunOnSameBranch(t, r1, r3Agent, r3Head)
	r3Findings := executeReusingRun(t, r3, r3Agent, "R3")

	r2EvidenceDir := filepath.Join(filepath.Dir(r1.EvidenceDir), r2.Run.ID)
	if strings.Contains(r3Findings.EvidenceReason, r2EvidenceDir) {
		t.Fatalf("R3 reason %q points at R2's empty evidence directory", r3Findings.EvidenceReason)
	}
	if !strings.Contains(r3Findings.EvidenceReason, r1.Run.ID) {
		t.Fatalf("R3 reason %q no longer names run %s, which is the only run holding artifacts", r3Findings.EvidenceReason, r1.Run.ID)
	}
	for _, f := range []types.Findings{r2Findings, r3Findings} {
		if len(f.Scenarios) != 1 || f.Scenarios[0].Name != "user reaches the success screen" {
			t.Fatalf("scenarios = %+v, want the originating run's scenario list to survive the chain", f.Scenarios)
		}
	}
}

// reRunOnSameBranch builds the next run of the same branch against the SAME
// database, repository and evidence root as the first, which is what makes the
// reuse lookup in these cases real.
func reRunOnSameBranch(t *testing.T, first *pipeline.StepContext, ag *stepstest.MockAgent, headSHA string) *pipeline.StepContext {
	t.Helper()
	next := gateContext(t, ag, first.WorkDir, first.Run.BaseSHA, headSHA)
	next.DB = first.DB
	next.Repo = first.Repo
	run, err := first.DB.InsertRun(first.Run.RepoID, first.Run.Branch, headSHA, first.Run.BaseSHA)
	if err != nil {
		t.Fatal(err)
	}
	next.Run = run
	next.Run.HeadSHA = headSHA
	next.EvidenceDir = filepath.Join(filepath.Dir(first.EvidenceDir), run.ID)
	setRunIntent(t, next, run.ID, gateTestIntent)
	return next
}

// executeReusingRun runs the step, asserts it reused rather than paying for
// the turn, and records the result so the NEXT run of the branch reads it the
// way the executor would. The step itself does not persist step_results.
func executeReusingRun(t *testing.T, sctx *pipeline.StepContext, ag *stepstest.MockAgent, label string) types.Findings {
	t.Helper()
	outcome, err := (&steps.TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ag.Calls) != 0 {
		t.Fatalf("%s evidence agent invocations = %d, want 0", label, len(ag.Calls))
	}
	findings := parseOutcomeFindings(t, outcome)
	if findings.EvidenceSource != types.TestEvidenceSourceReused {
		t.Fatalf("%s evidence source = %q, want %q", label, findings.EvidenceSource, types.TestEvidenceSourceReused)
	}
	persistTestOutcome(t, sctx, sctx.Run.ID, outcome)
	return findings
}

// persistTestOutcome completes a Test step row carrying the outcome's findings,
// which is the executor's job in a real run and what the branch-evidence query
// reads.
func persistTestOutcome(t *testing.T, sctx *pipeline.StepContext, runID string, outcome *pipeline.StepOutcome) {
	t.Helper()
	step, err := sctx.DB.InsertStepResult(runID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.SetStepFindings(step.ID, outcome.Findings); err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.CompleteStep(step.ID, 0, 1, ""); err != nil {
		t.Fatal(err)
	}
}

// TestTestStep_ProductChangeSinceAGoVerdictRerunsTheAgent is the other half of
// reuse: the moment a product file moves, the prior verdict stops describing
// this head and the agent runs.
func TestTestStep_ProductChangeSinceAGoVerdictRerunsTheAgent(t *testing.T) {
	t.Parallel()
	dir, baseSHA, _ := stepstest.SetupGitRepo(t)
	validated := commitFiles(t, dir, "product change", map[string]string{
		"internal/checkout/checkout.go": "package checkout\n",
	})
	head := commitFiles(t, dir, "another product change", map[string]string{
		"internal/checkout/total.go": "package checkout\n",
	})
	ag := gateAgent()
	sctx := gateContext(t, ag, dir, baseSHA, head)
	recordPriorGoVerdict(t, sctx, validated)

	outcome, err := (&steps.TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ag.Calls) != 1 {
		t.Fatalf("evidence agent invocations = %d, want 1", len(ag.Calls))
	}
	if got := parseOutcomeFindings(t, outcome).EvidenceSource; got != types.TestEvidenceSourceAgent {
		t.Fatalf("evidence source = %q, want %q", got, types.TestEvidenceSourceAgent)
	}
}

// TestTestStep_NonGoVerdictIsNeverReused keeps the gate from restating a
// conclusion a human has not resolved.
func TestTestStep_NonGoVerdictIsNeverReused(t *testing.T) {
	t.Parallel()
	dir, baseSHA, _ := stepstest.SetupGitRepo(t)
	validated := commitFiles(t, dir, "product change", map[string]string{
		"internal/checkout/checkout.go": "package checkout\n",
	})
	head := commitFiles(t, dir, "docs follow-up", map[string]string{"docs/guide.md": "# guide\n"})
	ag := gateAgent()
	sctx := gateContext(t, ag, dir, baseSHA, head)

	recordPriorVerdict(t, sctx, validated, types.TestVerdictInconclusive)

	if _, err := (&steps.TestStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(ag.Calls) != 1 {
		t.Fatalf("evidence agent invocations = %d, want 1 (an inconclusive verdict is not reusable)", len(ag.Calls))
	}
}

// TestTestStep_AnotherBranchesGoVerdictIsNeverReused keeps evidence scoped to
// the branch that earned it.
func TestTestStep_AnotherBranchesGoVerdictIsNeverReused(t *testing.T) {
	t.Parallel()
	dir, baseSHA, _ := stepstest.SetupGitRepo(t)
	validated := commitFiles(t, dir, "product change", map[string]string{
		"internal/checkout/checkout.go": "package checkout\n",
	})
	head := commitFiles(t, dir, "docs follow-up", map[string]string{"docs/guide.md": "# guide\n"})
	ag := gateAgent()
	sctx := gateContext(t, ag, dir, baseSHA, head)

	branch := sctx.Run.Branch
	sctx.Run.Branch = "refs/heads/other"
	recordPriorGoVerdict(t, sctx, validated)
	sctx.Run.Branch = branch

	if _, err := (&steps.TestStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(ag.Calls) != 1 {
		t.Fatalf("evidence agent invocations = %d, want 1 (another branch's verdict is not reusable)", len(ag.Calls))
	}
}

// TestTestStep_FailingBaselineDefeatsReuse: restating an earlier go while this
// run's configured command is red would contradict the run's own evidence.
func TestTestStep_FailingBaselineDefeatsReuse(t *testing.T) {
	t.Parallel()
	dir, baseSHA, _ := stepstest.SetupGitRepo(t)
	validated := commitFiles(t, dir, "product change", map[string]string{
		"internal/checkout/checkout.go": "package checkout\n",
	})
	head := commitFiles(t, dir, "docs follow-up", map[string]string{"docs/guide.md": "# guide\n"})
	ag := gateAgent()
	sctx := gateContext(t, ag, dir, baseSHA, head)
	sctx.Config.Commands.Test = "exit 1"
	recordPriorGoVerdict(t, sctx, validated)

	if _, err := (&steps.TestStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(ag.Calls) != 1 {
		t.Fatalf("evidence agent invocations = %d, want 1 (a red baseline defeats reuse)", len(ag.Calls))
	}
}

// TestTestStep_FixRoundNeverReusesAnEarlierVerdict: a Test fix round exists
// because THIS run's own evidence reported a problem, and
// GetBranchTestEvidence excludes the current run, so every verdict reachable
// from a fix round predates the finding being repaired. Republishing one would
// publish a go over evidence this run has already contradicted.
//
// Every other reuse condition is satisfied here - same branch, same intent,
// newest verdict, green baseline, and only a test file changed since the go
// was earned - so the fix round is the only thing that can decline it.
func TestTestStep_FixRoundNeverReusesAnEarlierVerdict(t *testing.T) {
	t.Parallel()
	dir, baseSHA, _ := stepstest.SetupGitRepo(t)
	validated := commitFiles(t, dir, "product change", map[string]string{
		"internal/checkout/checkout.go": "package checkout\n",
	})
	head := commitFiles(t, dir, "repair the failing test", map[string]string{
		"internal/checkout/checkout_test.go": "package checkout\n",
	})
	ag := gateAgent()
	sctx := gateContext(t, ag, dir, baseSHA, head)
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"id":"test-1","severity":"error","action":"auto-fix","description":"scenario user reaches the success screen failed"}]}`
	recordPriorGoVerdict(t, sctx, validated)

	outcome, err := (&steps.TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := evidenceTurns(ag); got != 1 {
		t.Fatalf("evidence agent invocations = %d, want 1 (a fix round must re-earn its verdict)", got)
	}
	findings := parseOutcomeFindings(t, outcome)
	if findings.EvidenceSource != types.TestEvidenceSourceAgent {
		t.Fatalf("evidence source = %q, want %q", findings.EvidenceSource, types.TestEvidenceSourceAgent)
	}
	if findings.TestedHeadSHA != head {
		t.Fatalf("tested head = %q, want this run's head %q", findings.TestedHeadSHA, head)
	}
}

// evidenceTurns counts the live-evidence invocations only. A fix round asks
// the agent to repair the failure first, so the raw call count would report
// that turn as though the gate had let the evidence turn run.
func evidenceTurns(ag *stepstest.MockAgent) int {
	turns := 0
	for _, call := range ag.Calls {
		if strings.Contains(call.Prompt, "driving the product itself") {
			turns++
		}
	}
	return turns
}

// TestTestStep_RepoConfigOverridesTheNonProductClassification proves the
// classification is the repository's to set: a repo that declares its own
// generated directory non-product skips the turn for a change confined to it,
// and a repo that declares an empty list opts every path back into product.
func TestTestStep_RepoConfigOverridesTheNonProductClassification(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		nonProduct  []string
		wantCalls   int
		wantVerdict string
	}{
		{
			name:        "repository declares its generated tree non-product",
			nonProduct:  []string{"gen/**"},
			wantCalls:   0,
			wantVerdict: types.TestVerdictNoSurface,
		},
		{
			name:        "repository opts every path back into product",
			nonProduct:  []string{},
			wantCalls:   1,
			wantVerdict: types.TestVerdictGo,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, _ := stepstest.SetupGitRepo(t)
			if err := os.Remove(filepath.Join(dir, "feature.txt")); err != nil {
				t.Fatal(err)
			}
			head := commitFiles(t, dir, "generated change", map[string]string{"gen/api.pb.go": "package gen\n"})
			ag := gateAgent()
			sctx := gateContext(t, ag, dir, baseSHA, head)
			sctx.Config.Test.NonProductPaths = tc.nonProduct

			outcome, err := (&steps.TestStep{}).Execute(sctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(ag.Calls) != tc.wantCalls {
				t.Fatalf("evidence agent invocations = %d, want %d", len(ag.Calls), tc.wantCalls)
			}
			if got := parseOutcomeFindings(t, outcome).Verdict; got != tc.wantVerdict {
				t.Fatalf("verdict = %q, want %q", got, tc.wantVerdict)
			}
		})
	}
}

// TestTestStep_GateOffRunsTheAgentAndRecordsNothing is the opt-in's proof.
//
// It is the exact inverse of
// TestTestStep_NonProductOnlyChangeSkipsTheAgentWithoutParking: the same
// diffs, each one the gate would skip, but with test.evidence_gate left at its
// default. Every one must still pay for the evidence turn, and the payload
// must carry neither evidence field - a recorded source would change the
// findings JSON, the PR body's live-validation line, and axi's live_evidence
// field for a repository that never opted in.
func TestTestStep_GateOffRunsTheAgentAndRecordsNothing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		files map[string]string
	}{
		{name: "docs only", files: map[string]string{"docs/guide.md": "# guide\n", "README.md": "hi\n"}},
		{name: "tests and fixtures only", files: map[string]string{
			"internal/checkout/checkout_test.go":    "package checkout\n",
			"internal/checkout/testdata/golden.txt": "golden\n",
		}},
		{name: "ci workflow only", files: map[string]string{".github/workflows/ci.yml": "name: ci\n"}},
		{name: "scripts only", files: map[string]string{"scripts/release.sh": "#!/bin/sh\n"}},
		{name: "pipeline config only", files: map[string]string{".no-mistakes.yaml": "commands:\n  test: go test ./...\n"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, _ := stepstest.SetupGitRepo(t)
			if err := os.Remove(filepath.Join(dir, "feature.txt")); err != nil {
				t.Fatal(err)
			}
			head := commitFiles(t, dir, "non-product change", tc.files)
			ag := gateAgent()
			sctx := gateOffContext(t, ag, dir, baseSHA, head)

			outcome, err := (&steps.TestStep{}).Execute(sctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(ag.Calls) != 1 {
				t.Fatalf("evidence agent invocations = %d, want 1 with the gate off", len(ag.Calls))
			}
			findings := parseOutcomeFindings(t, outcome)
			if findings.EvidenceSource != "" || findings.EvidenceReason != "" {
				t.Fatalf("evidence source/reason = %q/%q, want both empty with the gate off", findings.EvidenceSource, findings.EvidenceReason)
			}
			if findings.Verdict != types.TestVerdictGo {
				t.Fatalf("verdict = %q, want the agent's own %q", findings.Verdict, types.TestVerdictGo)
			}
		})
	}
}

// TestTestStep_GateOffNeverReusesAnEarlierVerdict is the second half of the
// same proof: reuse is the gate's other skipping path, so an unchanged product
// state must still re-derive the verdict when the key is unset.
func TestTestStep_GateOffNeverReusesAnEarlierVerdict(t *testing.T) {
	t.Parallel()
	dir, baseSHA, _ := stepstest.SetupGitRepo(t)
	head := commitFiles(t, dir, "add a product file", map[string]string{"internal/checkout/checkout.go": "package checkout\n"})
	ag := gateAgent()
	sctx := gateOffContext(t, ag, dir, baseSHA, head)
	recordPriorGoVerdict(t, sctx, head)

	outcome, err := (&steps.TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ag.Calls) != 1 {
		t.Fatalf("evidence agent invocations = %d, want 1 with the gate off", len(ag.Calls))
	}
	findings := parseOutcomeFindings(t, outcome)
	if findings.EvidenceSource != "" {
		t.Fatalf("evidence source = %q, want empty with the gate off", findings.EvidenceSource)
	}
}
