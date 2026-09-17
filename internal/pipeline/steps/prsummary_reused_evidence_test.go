package steps

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A reused verdict is the one shape where the Test step records a go for a
// head no live-evidence turn ever touched. Its scenarios really were driven
// live - in an earlier run, at another head - so the danger is not the
// scenario records themselves but the two places they turn into a claim about
// the commit the PR is showing: the attestation's live_validation object and
// the PR body's live-validation line.
//
// These cases pin both, and they pin the discrimination rather than mere
// absence: the same fixture with the head the agent actually drove must still
// publish its claim in full, or a "fix" that simply stopped emitting
// live_validation would pass.

const reusedDrivenHeadSHA = "fedcba9876543210fedcba9876543210fedcba98"

func reusedEvidenceFindingsJSON(t *testing.T, source, testedHead string) string {
	t.Helper()
	raw, err := json.Marshal(types.Findings{
		Tested:         []string{"`npm run e2e -- checkout`"},
		TestingSummary: "product files unchanged since fedcba9; reused from run run-1",
		Scenarios: []types.TestScenario{
			{Name: "user reaches the success screen", Result: types.ScenarioResultPass, Live: true, Evidence: "checkout.png"},
			{Name: "expired token is refused", Result: types.ScenarioResultPass, Live: true, Evidence: "token.png"},
		},
		Verdict:        types.TestVerdictGo,
		TestedHeadSHA:  testedHead,
		EvidenceSource: source,
		EvidenceReason: "product files unchanged since fedcba9; reused from run run-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func attestedLiveValidationObject(t *testing.T, findingsJSON, headSHA string) map[string]any {
	t.Helper()
	steps, rounds := testStepWithFindings(t, findingsJSON)
	raw := buildPipelineAttestation(steps, rounds, headSHA)
	payload := strings.TrimSuffix(strings.TrimPrefix(raw, pipelineAttestationCommentPrefix), pipelineAttestationCommentClosingToken)
	var decoded map[string]any
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatalf("attestation payload did not parse: %v (%s)", err, payload)
	}
	if live, present := decoded["live_validation"]; present {
		obj, ok := live.(map[string]any)
		if !ok {
			t.Fatalf("live_validation is not an object: %v", live)
		}
		return obj
	}
	return nil
}

// TestAttestationMakesNoLiveClaimForAHeadNoTurnDrove guards the MECHANISM the
// reuse fix leans on, not the fix itself: that a recorded head which is not
// the published head yields no live_validation object at all. The step-driving
// half - that a reused verdict is never restamped onto this run's head - is
// pinned by TestTestStep_ReusesAGoVerdictWhenProductFilesAreUnchanged in the
// testgate package. Both have to hold; either one alone leaves a run that
// drove nothing able to publish a live count.
func TestAttestationMakesNoLiveClaimForAHeadNoTurnDrove(t *testing.T) {
	reused := reusedEvidenceFindingsJSON(t, types.TestEvidenceSourceReused, reusedDrivenHeadSHA)
	if got := attestedLiveValidationObject(t, reused, testPipelineHeadSHA); got != nil {
		t.Fatalf("a reused verdict published live_validation for a head it never drove: %v", got)
	}

	// The same scenarios, published against the head they were actually
	// driven at, still carry the full claim including the producer.
	driven := attestedLiveValidationObject(t, reused, reusedDrivenHeadSHA)
	if driven == nil {
		t.Fatal("the head the scenarios were driven at must still carry live_validation")
	}
	if driven["live"] != float64(2) || driven["total"] != float64(2) {
		t.Errorf("live_validation coverage = %v of %v, want 2 of 2", driven["live"], driven["total"])
	}
	if driven["source"] != types.TestEvidenceSourceReused {
		t.Errorf("live_validation.source = %v, want %q", driven["source"], types.TestEvidenceSourceReused)
	}

	// An agent turn is the unchanged case: it drove this head, so it claims it.
	agent := attestedLiveValidationObject(t, reusedEvidenceFindingsJSON(t, types.TestEvidenceSourceAgent, testPipelineHeadSHA), testPipelineHeadSHA)
	if agent == nil {
		t.Fatal("an agent-driven verdict must still carry live_validation for its own head")
	}
	if agent["live"] != float64(2) {
		t.Errorf("live count = %v, want 2", agent["live"])
	}
}

// TestLiveValidationLineSaysWhichRunDroveTheScenarios: the PR body's prose has
// to agree with the attestation. "2 of 2 scenarios driven live against the
// product" under a reused verdict reads as this run's work, so the sentence
// itself has to place the driving in the earlier run.
func TestLiveValidationLineSaysWhichRunDroveTheScenarios(t *testing.T) {
	scenarios := []types.TestScenario{
		{Name: "user reaches the success screen", Result: types.ScenarioResultPass, Live: true, Evidence: "checkout.png"},
	}
	reason := "product files unchanged since fedcba9; reused from run run-1"

	reused := renderLiveValidationLine(scenarios, types.TestVerdictGo, reason, types.TestEvidenceSourceReused)
	if !strings.Contains(reused, "not in this run") {
		t.Fatalf("reused line does not place the live driving in the earlier run: %q", reused)
	}
	if !strings.Contains(reused, "reused from run run-1") {
		t.Fatalf("reused line drops the provenance: %q", reused)
	}

	// Every other source keeps today's wording exactly, including a run
	// recorded with the gate off, which carries no source at all.
	for _, source := range []string{types.TestEvidenceSourceAgent, types.TestEvidenceSourceNoProductChange, ""} {
		line := renderLiveValidationLine(scenarios, types.TestVerdictGo, "", source)
		if !strings.Contains(line, "1 of 1 scenarios driven live against the product") {
			t.Fatalf("source %q changed the unqualified wording: %q", source, line)
		}
		if strings.Contains(line, "not in this run") {
			t.Fatalf("source %q wrongly qualified the count: %q", source, line)
		}
	}
}

// The reuse reason reaches a public PR body, and the publication redactor
// (safepath.RedactText, called from redactPRContent) only removes home
// directories while test.evidence.local_root is documented as any absolute
// path. So the reason must name the prior RUN, never a directory on the host.
func TestReuseReasonNamesTheRunNotAHostPath(t *testing.T) {
	decision := reuseDecision("run-1", Findings{
		TestedHeadSHA: reusedDrivenHeadSHA,
		Scenarios:     []types.TestScenario{{Name: "s", Result: types.ScenarioResultPass, Live: true}},
	})
	if !strings.Contains(decision.Reason, "reused from run run-1") {
		t.Fatalf("reason %q does not name the run holding the evidence", decision.Reason)
	}
	// No path separator at all is the assertion that survives a refactor: any
	// reintroduced directory, under any evidence root, contains one.
	if strings.ContainsAny(decision.Reason, `/\`) {
		t.Fatalf("reason %q publishes a host filesystem path", decision.Reason)
	}
	if decision.Reused.TestedHeadSHA != reusedDrivenHeadSHA {
		t.Fatalf("reused head = %q, want the head the scenarios were driven at", decision.Reused.TestedHeadSHA)
	}
}

// The reuse path exists to avoid re-deriving evidence, not to publish a
// verdict with nothing behind it. So a reused verdict carries the originating
// run's artifacts, and gatedTestOutcome copies that run's evidence directory
// into this run's - the only directory publication reads.
func TestReuseCarriesTheOriginatingRunsArtifacts(t *testing.T) {
	prior := Findings{
		TestedHeadSHA: reusedDrivenHeadSHA,
		Scenarios:     []types.TestScenario{{Name: "s", Result: types.ScenarioResultPass, Live: true, Evidence: "checkout.png"}},
		Artifacts:     []types.TestArtifact{{Label: "checkout", Path: "/evidence/run-1/checkout.png"}},
	}
	decision := reuseDecision("run-1", prior)
	if len(decision.Reused.Artifacts) != 1 {
		t.Fatalf("artifacts = %+v, want the originating run's list carried forward", decision.Reused.Artifacts)
	}
	if decision.Reused.EvidenceOriginRunID != "run-1" {
		t.Fatalf("origin run = %q, want run-1", decision.Reused.EvidenceOriginRunID)
	}

	// A reuse OF a reuse keeps naming the run that actually drove the agent,
	// because that is the only run whose directory holds anything.
	chained := reuseDecision("run-2", Findings{
		TestedHeadSHA:       reusedDrivenHeadSHA,
		EvidenceSource:      types.TestEvidenceSourceReused,
		EvidenceOriginRunID: "run-1",
		Artifacts:           prior.Artifacts,
	})
	if chained.Reused.EvidenceOriginRunID != "run-1" {
		t.Fatalf("chained origin = %q, want the originating run run-1", chained.Reused.EvidenceOriginRunID)
	}
	if !strings.Contains(chained.Reason, "reused from run run-1") {
		t.Fatalf("chained reason %q does not name the originating run", chained.Reason)
	}
}

// A carried artifact's recorded path decides whether it survives rendering at
// all: sanitizeAbsoluteArtifactPath accepts an absolute path only under the
// repository root or THIS run's evidence directory, and the evidence prompt
// has the agent record paths exactly where it wrote them. So the rebasing is
// the part that has to be pinned - and it cannot assume WHICH run directory a
// path names, because from the third run of a branch onward the predecessor
// was itself a reuse whose paths already name its own directory.
func TestCarriedArtifactPathsPointAtThisRunsCopy(t *testing.T) {
	root := t.TempDir()
	origin := filepath.Join(root, "run-1")
	predecessor := filepath.Join(root, "run-2")
	dest := filepath.Join(root, "run-3")
	for _, dir := range []string{origin, predecessor, dest} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"checkout.png", "token.png"} {
		if err := os.WriteFile(filepath.Join(origin, name), []byte("png"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sctx := &pipeline.StepContext{EvidenceDir: dest}

	carried, err := carryOriginEvidence(sctx, "run-1", []types.TestArtifact{
		{Label: "absolute under the origin", Path: filepath.Join(origin, "checkout.png")},
		{Label: "absolute under a predecessor that already rebased", Path: filepath.Join(predecessor, "token.png")},
		{Label: "relative to the evidence dir", Path: "checkout.png"},
		{Label: "an externally visible artifact", URL: "https://ci.example/run/1"},
		{Label: "inline output", Content: "all 4 scenarios passed"},
		{Label: "absolute outside the evidence root", Path: filepath.Join(t.TempDir(), "secret.png")},
		{Label: "a file nothing copied", Path: "missing.png"},
	})
	if err != nil {
		t.Fatalf("carrying evidence forward: %v", err)
	}

	byLabel := map[string]types.TestArtifact{}
	for _, a := range carried {
		byLabel[a.Label] = a
	}
	// The predecessor case is f1: rebasing relative to the ORIGIN would yield
	// ../run-2/token.png and drop it, collapsing the whole chain at run three.
	for label, wantPath := range map[string]string{
		"absolute under the origin":                         filepath.Join(dest, "checkout.png"),
		"absolute under a predecessor that already rebased": filepath.Join(dest, "token.png"),
		"relative to the evidence dir":                      "checkout.png",
	} {
		got, ok := byLabel[label]
		if !ok {
			t.Errorf("%q was dropped; want it carried at %q", label, wantPath)
			continue
		}
		if got.Path != wantPath {
			t.Errorf("%q path = %q, want %q", label, got.Path, wantPath)
		}
	}
	// url and content artifacts need no file and are carried untouched.
	if got, ok := byLabel["an externally visible artifact"]; !ok || got.URL != "https://ci.example/run/1" {
		t.Errorf("a url artifact must survive untouched, got %+v", got)
	}
	if got, ok := byLabel["inline output"]; !ok || got.Content != "all 4 scenarios passed" {
		t.Errorf("a content artifact must survive untouched, got %+v", got)
	}
	// Nothing copied these, so citing them would name a file the PR lacks.
	for _, label := range []string{"absolute outside the evidence root", "a file nothing copied"} {
		if _, ok := byLabel[label]; ok {
			t.Errorf("%q must be dropped: no file backs it here", label)
		}
	}
}

// A verdict evidenced entirely by url or content artifacts has no file to
// copy, so it must not be made to depend on the originating directory still
// existing - that would throw its scenarios away for nothing.
func TestCarryOriginEvidenceNeedsNoFilesForURLOnlyArtifacts(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "run-2")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	sctx := &pipeline.StepContext{EvidenceDir: dest}

	artifacts := []types.TestArtifact{
		{Label: "ci run", URL: "https://ci.example/run/1"},
		{Label: "output", Content: "all scenarios passed"},
	}
	// run-1's directory does not exist at all here, which is the point.
	carried, err := carryOriginEvidence(sctx, "run-1", artifacts)
	if err != nil {
		t.Fatalf("a url/content-only verdict must not need the originating directory: %v", err)
	}
	if len(carried) != 2 {
		t.Fatalf("carried = %+v, want both artifacts untouched", carried)
	}
}

// When files WERE expected and none arrived, the carry has failed and the
// caller drops the scenarios with them.
func TestCarryOriginEvidenceFailsWhenNoExpectedFileArrives(t *testing.T) {
	root := t.TempDir()
	origin := filepath.Join(root, "run-1")
	dest := filepath.Join(root, "run-2")
	for _, dir := range []string{origin, dest} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The directory is populated, so the copy runs, but nothing it holds
	// matches the recorded artifact.
	if err := os.WriteFile(filepath.Join(origin, "unrelated.log"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	sctx := &pipeline.StepContext{EvidenceDir: dest}

	if _, err := carryOriginEvidence(sctx, "run-1", []types.TestArtifact{
		{Label: "checkout", Path: filepath.Join(origin, "checkout.png")},
	}); err == nil {
		t.Fatal("a file artifact that never arrived must fail the carry, not be published")
	}
}

// carryOriginEvidence joins the recorded run ID into a filesystem path, and a
// findings payload is parsed by the same function whether it came from the
// database or from an agent turn, so a traversal there would have any
// directory on the host copied into a published evidence directory. The value
// is validated as one path segment, and test.go clears it on the agent path.
//
// The layout matters for this to test anything: run directories are siblings
// under one evidence root, so an escaping ID reaches a sibling OF THAT ROOT.
// A decoy planted inside the root is never reached, and the case then passes
// on a failed ReadDir whether the validation exists or not.
func TestCarryOriginEvidenceRefusesATraversingRunID(t *testing.T) {
	host := t.TempDir()
	evidenceRoot := filepath.Join(host, "evidence")
	dest := filepath.Join(evidenceRoot, "run-2")
	secrets := filepath.Join(host, "secrets")
	for _, dir := range []string{dest, secrets} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(secrets, "id_rsa"), []byte("PRIVATE KEY"), 0o600); err != nil {
		t.Fatal(err)
	}
	sctx := &pipeline.StepContext{EvidenceDir: dest}

	// Proof the decoy is actually reachable by the traversal this refuses:
	// without the guard, this is the directory the copy would read.
	if _, err := os.Stat(filepath.Join(filepath.Dir(dest), "../secrets", "id_rsa")); err != nil {
		t.Fatalf("the traversal target must be reachable or this case proves nothing: %v", err)
	}

	for _, id := range []string{"../secrets", "..", ".", "sub/dir", "  "} {
		if _, err := carryOriginEvidence(sctx, id, []types.TestArtifact{{Path: "x.png"}}); err == nil {
			t.Errorf("originating run %q was accepted as a path segment", id)
		}
	}
	if _, err := os.Stat(filepath.Join(dest, "id_rsa")); err == nil {
		t.Fatal("a traversing run ID copied a file from outside the evidence root")
	}
}

// The ordinary failure modes are not exceptional - retention ages evidence
// directories out - so each has to be a clean error the caller turns into
// "publish the verdict without its scenario table", never a silent success.
func TestCarryOriginEvidenceFailureModes(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "run-2")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	sctx := &pipeline.StepContext{EvidenceDir: dest}
	artifacts := []types.TestArtifact{{Path: "x.png"}}

	if _, err := carryOriginEvidence(sctx, "", artifacts); err == nil {
		t.Error("an unrecorded originating run must be an error")
	}
	if _, err := carryOriginEvidence(sctx, "run-1", artifacts); err == nil {
		t.Error("a missing originating directory must be an error, not a silent success")
	}
	src := filepath.Join(root, "run-1")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := carryOriginEvidence(sctx, "run-1", artifacts); err == nil {
		t.Error("an empty originating directory must be an error")
	}
	if _, err := carryOriginEvidence(&pipeline.StepContext{}, "run-1", artifacts); err == nil {
		t.Error("a run with no evidence directory must be an error")
	}
}

// Evidence is written by the live-evidence agent, so a link in an evidence
// directory is agent-influenced input. copyDirContents faithfully recreates a
// symlink, which would land it in a directory the pipeline publishes - and the
// media upload reads artifact files by path, so a link to any readable host
// file would put that file's contents in a public pull request. Nothing
// irregular is carried, and nothing irregular is cited.
func TestCarryOriginEvidenceDoesNotCarrySymlinks(t *testing.T) {
	host := t.TempDir()
	evidenceRoot := filepath.Join(host, "evidence")
	origin := filepath.Join(evidenceRoot, "run-1")
	dest := filepath.Join(evidenceRoot, "run-2")
	for _, dir := range []string{origin, dest} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	secret := filepath.Join(host, "id_rsa")
	if err := os.WriteFile(secret, []byte("PRIVATE KEY"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A real screenshot beside a link masquerading as one.
	if err := os.WriteFile(filepath.Join(origin, "checkout.png"), []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(origin, "leak.png")); err != nil {
		t.Skipf("symlinks unavailable on this platform: %v", err)
	}
	sctx := &pipeline.StepContext{EvidenceDir: dest}

	carried, err := carryOriginEvidence(sctx, "run-1", []types.TestArtifact{
		{Label: "real screenshot", Path: filepath.Join(origin, "checkout.png")},
		{Label: "a link pretending to be evidence", Path: filepath.Join(origin, "leak.png")},
	})
	if err != nil {
		t.Fatalf("a legitimate artifact beside a link must still carry: %v", err)
	}

	// The link is not reproduced in the published directory at all.
	if _, err := os.Lstat(filepath.Join(dest, "leak.png")); err == nil {
		t.Error("the symlink was recreated inside this run's evidence directory")
	}
	if _, err := os.Stat(filepath.Join(dest, "id_rsa")); err == nil {
		t.Error("the link's target was copied into this run's evidence directory")
	}
	// And it is never cited, while the real screenshot is.
	for _, a := range carried {
		if strings.Contains(a.Path, "leak.png") {
			t.Errorf("a link was published as an artifact: %+v", a)
		}
	}
	if len(carried) != 1 || filepath.Base(carried[0].Path) != "checkout.png" {
		t.Fatalf("carried = %+v, want only the real screenshot", carried)
	}
}

// regularFileAt is the second gate: os.Stat follows a link and would report on
// its target, so an artifact pointing through one would read as backed.
func TestRegularFileAtRefusesEverythingButARealFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "real.png")
	if err := os.WriteFile(file, []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.png")
	if err := os.Symlink(file, link); err != nil {
		t.Skipf("symlinks unavailable on this platform: %v", err)
	}

	if !regularFileAt(file) {
		t.Error("a real file must be accepted")
	}
	if regularFileAt(link) {
		t.Error("a symlink must be refused even though its target exists")
	}
	if regularFileAt(dir) {
		t.Error("a directory must be refused")
	}
	if regularFileAt(filepath.Join(dir, "absent.png")) {
		t.Error("a missing path must be refused")
	}
}
