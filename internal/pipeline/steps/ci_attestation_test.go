package steps

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const verifyPyRelPath = "../../../.github/actions/require-no-mistakes/verify.py"

func compliantPipelineBody(t *testing.T, headSHA string) string {
	t.Helper()
	stepResults := []*db.StepResult{
		{ID: "review", StepName: types.StepReview, Status: types.StepStatusCompleted},
		{ID: "test", StepName: types.StepTest, Status: types.StepStatusCompleted},
		{ID: "document", StepName: types.StepDocument, Status: types.StepStatusCompleted},
	}
	rounds := make(map[string][]*db.StepRound, len(stepResults))
	for _, sr := range stepResults {
		rounds[sr.ID] = []*db.StepRound{{Round: 1, Trigger: "initial", DurationMS: 1}}
	}
	md, _ := BuildPipelineSummary(stepResults, rounds, headSHA)
	if md == "" {
		t.Fatal("BuildPipelineSummary returned empty markdown")
	}
	return md
}

func pythonInterpreterForVerify(t *testing.T) string {
	t.Helper()
	for _, candidate := range []string{"python3", "python"} {
		if path, err := exec.LookPath(candidate); err == nil {
			return path
		}
	}
	t.Skip("no python interpreter available to execute verify.py")
	return ""
}

func runVerifyPy(t *testing.T, body, headSHA string) (conclusion, output string) {
	t.Helper()
	python := pythonInterpreterForVerify(t)
	outputFile := filepath.Join(t.TempDir(), "github_output")
	if err := os.WriteFile(outputFile, nil, 0o644); err != nil {
		t.Fatalf("seed GITHUB_OUTPUT: %v", err)
	}
	cmd := exec.Command(python, verifyPyRelPath)
	cmd.Env = append(os.Environ(),
		"PR_BODY="+body,
		"PR_HEAD_SHA="+headSHA,
		"PR_NUMBER=42",
		"GITHUB_OUTPUT="+outputFile,
	)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	switch {
	case err == nil:
		return "success", buf.String()
	default:
		if _, ok := err.(*exec.ExitError); !ok {
			t.Fatalf("execute verify.py: %v\n%s", err, buf.String())
		}
		return "failure", buf.String()
	}
}

func TestRebindPipelineAttestationHead_VerifyPyRoundTrip(t *testing.T) {
	t.Parallel()
	originalHead := testPipelineHeadSHA
	repairHead := strings.Repeat("ab", 20)
	original := compliantPipelineBody(t, originalHead)

	if got, out := runVerifyPy(t, original, originalHead); got != "success" {
		t.Fatalf("original body at original head: conclusion=%s\n%s", got, out)
	}
	if got, out := runVerifyPy(t, original, repairHead); got != "failure" || !strings.Contains(out, "does not match") {
		t.Fatalf("stale attestation at new head must fail the head bind, got %s\n%s", got, out)
	}

	rebound, ok := rebindPipelineAttestationHead(original, repairHead)
	if !ok {
		t.Fatal("expected a live attestation to rebind")
	}
	if parsePipelineAttestationForTest(t, rebound).HeadSHA != repairHead {
		t.Fatalf("rebound head = %q, want %q", parsePipelineAttestationForTest(t, rebound).HeadSHA, repairHead)
	}
	if got, out := runVerifyPy(t, rebound, repairHead); got != "success" {
		t.Fatalf("rebound attestation at the new head must pass, got %s\n%s", got, out)
	}

	foreign := "a regular pull request with no pipeline section"
	unchanged, ok := rebindPipelineAttestationHead(foreign, repairHead)
	if ok {
		t.Fatal("rebind must not mint an attestation for a PR that was not raised through no-mistakes")
	}
	if unchanged != foreign {
		t.Fatal("body without an attestation must be left untouched")
	}
	if got, out := runVerifyPy(t, unchanged, repairHead); got != "failure" || !strings.Contains(out, "not raised through no-mistakes") {
		t.Fatalf("a PR not raised through no-mistakes must still fail, got %s\n%s", got, out)
	}
}

type attestationTestHost struct {
	scm.Host
	title       string
	body        string
	updated     scm.PRContent
	updates     int
	failUpdates int
}

func (h *attestationTestHost) GetPRContent(context.Context, *scm.PR) (scm.PRContent, error) {
	return scm.PRContent{Title: h.title, Body: h.body}, nil
}

func (h *attestationTestHost) UpdatePR(_ context.Context, pr *scm.PR, content scm.PRContent) (*scm.PR, error) {
	h.updates++
	if h.updates <= h.failUpdates {
		return nil, fmt.Errorf("temporary PR update failure")
	}
	h.updated = content
	h.body = content.Body
	if strings.TrimSpace(content.Title) != "" {
		h.title = content.Title
	}
	return pr, nil
}

func TestRestampPRAttestation_RebindsExistingAndSkipsMissing(t *testing.T) {
	t.Parallel()
	originalHead := testPipelineHeadSHA
	repairHead := strings.Repeat("cd", 20)
	pr := &scm.PR{Number: "42", URL: "https://github.com/test/repo/pull/42"}

	t.Run("existing_attestation_is_rebound", func(t *testing.T) {
		t.Parallel()
		host := &attestationTestHost{title: "fix: ci", body: compliantPipelineBody(t, originalHead)}
		if err := restampPRAttestation(context.Background(), host, pr, repairHead, nil); err != nil {
			t.Fatal(err)
		}
		if host.updates != 1 {
			t.Fatalf("UpdatePR calls = %d, want 1", host.updates)
		}
		if parsePipelineAttestationForTest(t, host.updated.Body).HeadSHA != repairHead {
			t.Fatalf("updated attestation head = %q, want %q", parsePipelineAttestationForTest(t, host.updated.Body).HeadSHA, repairHead)
		}
		if got, out := runVerifyPy(t, host.updated.Body, repairHead); got != "success" {
			t.Fatalf("restamped body must pass verify.py at the new head, got %s\n%s", got, out)
		}

		secondHead := strings.Repeat("ef", 20)
		if err := restampPRAttestation(context.Background(), host, pr, secondHead, nil); err != nil {
			t.Fatal(err)
		}
		if host.updates != 2 {
			t.Fatalf("UpdatePR calls after a second repair = %d, want 2", host.updates)
		}
		if parsePipelineAttestationForTest(t, host.updated.Body).HeadSHA != secondHead {
			t.Fatalf("second restamp head = %q, want %q", parsePipelineAttestationForTest(t, host.updated.Body).HeadSHA, secondHead)
		}
		if got, out := runVerifyPy(t, host.updated.Body, secondHead); got != "success" {
			t.Fatalf("attestation must stay valid across a further repair push, got %s\n%s", got, out)
		}
		if got, out := runVerifyPy(t, host.updated.Body, originalHead); got != "failure" {
			t.Fatalf("a restamped attestation must not still bind the original head, got %s\n%s", got, out)
		}
	})

	t.Run("missing_attestation_is_not_invented", func(t *testing.T) {
		t.Parallel()
		const foreign = "a regular pull request with no pipeline section"
		host := &attestationTestHost{title: "feat: hand rolled", body: foreign}
		if err := restampPRAttestation(context.Background(), host, pr, repairHead, nil); err != nil {
			t.Fatal(err)
		}
		if host.updates != 0 {
			t.Fatalf("UpdatePR calls = %d, want 0 (must not mint an attestation)", host.updates)
		}
		if host.body != foreign {
			t.Fatal("body without an attestation must be left untouched")
		}
		if got, out := runVerifyPy(t, host.body, repairHead); got != "failure" {
			t.Fatalf("a PR not raised through no-mistakes must still fail, got %s\n%s", got, out)
		}
	})
}

func TestRestampPRAttestation_PreservesContentEditedWhilePreparingRewrite(t *testing.T) {
	t.Parallel()
	originalHead := testPipelineHeadSHA
	repairHead := strings.Repeat("34", 20)
	body := compliantPipelineBody(t, originalHead) + "\n\nUser edit made during settlement."
	host := &attestationTestHost{
		title: "title edited by the user",
		body:  body,
	}

	if err := restampPRAttestation(context.Background(), host, &scm.PR{Number: "42"}, repairHead, nil); err != nil {
		t.Fatal(err)
	}
	if host.updated.Title != "" {
		t.Fatalf("restamp must not write a title, got %q", host.updated.Title)
	}
	if host.title != "title edited by the user" {
		t.Fatalf("stored title = %q, want the concurrent title left untouched", host.title)
	}
	if !strings.Contains(host.updated.Body, "User edit made during settlement.") {
		t.Fatalf("updated body lost concurrent user edit:\n%s", host.updated.Body)
	}
	if got := parsePipelineAttestationForTest(t, host.updated.Body).HeadSHA; got != repairHead {
		t.Fatalf("updated attestation head = %q, want %q", got, repairHead)
	}
}

func TestRestampPRAttestation_RetriesAndRequiresSettlement(t *testing.T) {
	t.Parallel()
	pr := &scm.PR{Number: "42", URL: "https://github.com/test/repo/pull/42"}
	repairHead := strings.Repeat("12", 20)

	t.Run("transient_failure_settles", func(t *testing.T) {
		host := &attestationTestHost{
			title:       "fix: ci",
			body:        compliantPipelineBody(t, testPipelineHeadSHA),
			failUpdates: 2,
		}
		if err := restampPRAttestation(context.Background(), host, pr, repairHead, nil); err != nil {
			t.Fatal(err)
		}
		if host.updates != 3 {
			t.Fatalf("UpdatePR calls = %d, want 3", host.updates)
		}
		if got, out := runVerifyPy(t, host.body, repairHead); got != "success" {
			t.Fatalf("settled body must pass verification, got %s\n%s", got, out)
		}
	})

	t.Run("persistent_failure_is_returned", func(t *testing.T) {
		host := &attestationTestHost{
			title:       "fix: ci",
			body:        compliantPipelineBody(t, testPipelineHeadSHA),
			failUpdates: 3,
		}
		err := restampPRAttestation(context.Background(), host, pr, repairHead, nil)
		if err == nil || !strings.Contains(err.Error(), "failed after 3 attempts") {
			t.Fatalf("restamp error = %v, want exhausted settlement error", err)
		}
		if host.updates != 3 {
			t.Fatalf("UpdatePR calls = %d, want 3", host.updates)
		}
		if got, _ := runVerifyPy(t, host.body, repairHead); got != "failure" {
			t.Fatal("unsettled body unexpectedly passed verification")
		}
	})
}

type readerlessHost struct{ scm.Host }

func TestRestampPRAttestation_MissingReaderIsSkipped(t *testing.T) {
	t.Parallel()
	pr := &scm.PR{Number: "42", URL: "https://bitbucket.org/test/repo/pull-requests/42"}
	var logs []string
	err := restampPRAttestation(context.Background(), &readerlessHost{}, pr, strings.Repeat("ab", 20), func(s string) {
		logs = append(logs, s)
	})
	if err != nil {
		t.Fatalf("missing reader must skip, not fail: %v", err)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "cannot read PR content") {
		t.Fatalf("skip warning = %q, want a missing-reader warning", logs)
	}
}

func TestCIStep_PublishRepairRebindsAttestationAcrossRepairPushes(t *testing.T) {
	f := newCIRepairFixture(t, false, writeCIFix)
	original := compliantPipelineBody(t, f.headSHA)
	bodyFile := filepath.Join(t.TempDir(), "pr-body.md")
	if err := os.WriteFile(bodyFile, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	logFile := filepath.Join(t.TempDir(), "gh.log")
	f.sctx.Repo.UpstreamURL = "https://github.com/test/repo.git"
	env := fakeCIGH(t, "OPEN", `[{"name":"test","state":"FAILURE","bucket":"fail"}]`)
	f.sctx.Env = append(env,
		"FAKE_CLI_PR_LIST_JSON=[{\"number\":42,\"url\":\"https://github.com/test/repo/pull/42\",\"baseRefName\":\"main\"}]",
		"FAKE_CLI_PR_BODY_FILE="+bodyFile,
		"FAKE_CLI_PR_TITLE=fix: ci",
		"FAKE_CLI_LOG="+logFile,
	)
	f.sctx.Ctx = context.Background()
	writeCIFix(f.dir)

	repair, err := (&CIStep{}).commitRepair(f.sctx, "repair the failing check")
	if err != nil {
		t.Fatalf("commitRepair: %v\nlog:\n%s", err, f.log())
	}
	if !repair.HeadAdvanced || repair.Revalidate {
		t.Fatalf("repair = %+v, want a published head advance", repair)
	}
	newHead := f.localHead(t)
	if newHead == f.headSHA {
		t.Fatal("expected a new repair commit")
	}

	updated := readFakeGHBodyArg(t, logFile)
	if parsePipelineAttestationForTest(t, updated).HeadSHA != newHead {
		t.Fatalf("published attestation head = %q, want the repair commit %q", parsePipelineAttestationForTest(t, updated).HeadSHA, newHead)
	}
	if got, out := runVerifyPy(t, updated, newHead); got != "success" {
		t.Fatalf("attestation after a repair push must stay valid, got %s\n%s", got, out)
	}
	if got, out := runVerifyPy(t, original, newHead); got != "failure" {
		t.Fatalf("the pre-repair attestation must fail at the new head, got %s\n%s", got, out)
	}
}

func TestCIStep_UnsettledRepairPushParksImmediately(t *testing.T) {
	f := newCIRepairFixture(t, false, writeCIFix)
	bodyFile := filepath.Join(t.TempDir(), "pr-body.md")
	if err := os.WriteFile(bodyFile, []byte(compliantPipelineBody(t, f.headSHA)), 0o644); err != nil {
		t.Fatal(err)
	}
	f.sctx.Repo.UpstreamURL = "https://github.com/test/repo.git"
	f.sctx.Env = append(f.sctx.Env,
		"FAKE_CLI_PR_BODY_FILE="+bodyFile,
		"FAKE_CLI_PR_TITLE=fix: ci",
		"FAKE_CLI_PR_EDIT_ERR=provider unavailable",
	)

	outcome, err := f.run(t)
	if err != nil {
		t.Fatal(err)
	}
	if outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("outcome = %#v, want unsettled push approval gate", outcome)
	}
	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(findings.Summary, "attestation is unsettled") {
		t.Fatalf("findings summary = %q, want settlement failure", findings.Summary)
	}
	if !strings.Contains(f.log(), "CI repair push is not settled") {
		t.Fatalf("log did not report unsettled push:\n%s", f.log())
	}
}

func TestCIStep_PublishRepairFailsWhenAttestationCannotSettle(t *testing.T) {
	f := newCIRepairFixture(t, false, writeCIFix)
	bodyFile := filepath.Join(t.TempDir(), "pr-body.md")
	if err := os.WriteFile(bodyFile, []byte(compliantPipelineBody(t, f.headSHA)), 0o644); err != nil {
		t.Fatal(err)
	}
	f.sctx.Repo.UpstreamURL = "https://github.com/test/repo.git"
	f.sctx.Env = append(fakeCIGH(t, "OPEN", `[{"name":"test","state":"FAILURE","bucket":"fail"}]`),
		"FAKE_CLI_PR_LIST_JSON=[{\"number\":42,\"url\":\"https://github.com/test/repo/pull/42\",\"baseRefName\":\"main\"}]",
		"FAKE_CLI_PR_BODY_FILE="+bodyFile,
		"FAKE_CLI_PR_TITLE=fix: ci",
		"FAKE_CLI_PR_EDIT_ERR=provider unavailable",
	)
	f.sctx.Ctx = context.Background()
	writeCIFix(f.dir)

	repair, err := (&CIStep{}).commitRepair(f.sctx, "repair the failing check")
	if err == nil || !strings.Contains(err.Error(), "failed after 3 attempts") {
		t.Fatalf("commitRepair error = %v, want unsettled attestation failure", err)
	}
	if repair.HeadAdvanced {
		t.Fatalf("repair = %+v, must not report a successfully settled repair", repair)
	}
	newHead := f.localHead(t)
	if newHead == f.headSHA {
		t.Fatal("expected the repair push to advance before settlement failed")
	}
	body, err := os.ReadFile(bodyFile)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := runVerifyPy(t, string(body), newHead); got != "failure" {
		t.Fatal("failed PR edit unexpectedly settled the attestation")
	}
}

// TestCIStep_PublishRepairSkipsAttestationForNonGitHubProvider pins
// attestHeadBeforePush's earliest short-circuit: a non-GitHub provider
// returns nil before ever building a host or touching PR content, since only
// GitHub emits the HTML attestation comment and implements PRContentReader.
// A repair still publishes cleanly with no PR interaction at all - unlike the
// old post-push restamp, which needed a host-capability check (and a
// "cannot read PR content" log) to reach the same no-op.
func TestCIStep_PublishRepairSkipsAttestationForNonGitHubProvider(t *testing.T) {
	f := newCIRepairFixture(t, false, writeCIFix)
	gitlabPR := "https://gitlab.com/test/repo/-/merge_requests/42"
	f.sctx.Repo.UpstreamURL = "https://gitlab.com/test/repo.git"
	f.sctx.Run.PRURL = &gitlabPR
	writeCIFix(f.dir)

	repair, err := (&CIStep{}).commitRepair(f.sctx, "repair the failing check")
	if err != nil {
		t.Fatalf("commitRepair: %v\nlog:\n%s", err, f.log())
	}
	if !repair.HeadAdvanced || repair.Revalidate {
		t.Fatalf("repair = %+v, want a published head advance without attestation", repair)
	}
	if strings.Contains(f.log(), "pipeline attestation") || strings.Contains(f.log(), "attestation rebind") {
		t.Fatalf("expected no attestation interaction at all for a non-GitHub provider:\n%s", f.log())
	}
}

func TestCIStep_PublishRepairDoesNotMintAttestation(t *testing.T) {
	f := newCIRepairFixture(t, false, writeCIFix)
	const foreign = "a regular pull request with no pipeline section"
	bodyFile := filepath.Join(t.TempDir(), "pr-body.md")
	if err := os.WriteFile(bodyFile, []byte(foreign), 0o644); err != nil {
		t.Fatal(err)
	}
	logFile := filepath.Join(t.TempDir(), "gh.log")
	f.sctx.Repo.UpstreamURL = "https://github.com/test/repo.git"
	env := fakeCIGH(t, "OPEN", `[{"name":"test","state":"FAILURE","bucket":"fail"}]`)
	f.sctx.Env = append(env,
		"FAKE_CLI_PR_LIST_JSON=[{\"number\":42,\"url\":\"https://github.com/test/repo/pull/42\",\"baseRefName\":\"main\"}]",
		"FAKE_CLI_PR_BODY_FILE="+bodyFile,
		"FAKE_CLI_PR_TITLE=feat: hand rolled",
		"FAKE_CLI_LOG="+logFile,
	)
	f.sctx.Ctx = context.Background()
	writeCIFix(f.dir)

	repair, err := (&CIStep{}).commitRepair(f.sctx, "repair the failing check")
	if err != nil {
		t.Fatalf("commitRepair: %v\nlog:\n%s", err, f.log())
	}
	if !repair.HeadAdvanced {
		t.Fatal("expected the repair to publish")
	}
	if logData, err := os.ReadFile(logFile); err == nil && strings.Contains(string(logData), "stdin --body ") {
		t.Fatalf("must not write a PR body when no attestation was present:\n%s", logData)
	}
	newHead := f.localHead(t)
	if got, out := runVerifyPy(t, foreign, newHead); got != "failure" {
		t.Fatalf("a PR not raised through no-mistakes must still fail after a push, got %s\n%s", got, out)
	}
}

// TestPushStep_AttestsHeadBeforePush pins design C, the simplest sufficient
// fix for the push-then-attest race: write the attestation for the head
// about to be pushed BEFORE pushing it, so every consumer of the shared
// require-no-mistakes action - including one pinned to an immutable
// revision whose verifier reads the PR body from the frozen `synchronize`
// event payload - already sees the correct attestation the moment the
// pushed head becomes current. A post-push write, however fast, cannot beat
// that: the payload is captured at push time, before any write could land.
func TestPushStep_AttestsHeadBeforePush(t *testing.T) {
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir, baseSHA, priorHead := setupGitRepo(t)
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "push", "origin", "feature")

	priorAttestedBody := compliantPipelineBody(t, priorHead)

	if err := os.WriteFile(filepath.Join(dir, "new-work.txt"), []byte("new work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "new work")
	newHead := gitCmd(t, dir, "rev-parse", "HEAD")

	prURL := "https://github.com/test/repo/pull/42"
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, newHead, config.Commands{})
	sctx.Repo.UpstreamURL = "https://github.com/test/repo"
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Run.PRURL = &prURL
	setupGateMirror(t, sctx)
	recordReviewApproval(t, sctx, newHead)

	for _, name := range []types.StepName{types.StepReview, types.StepTest, types.StepDocument} {
		sr, err := sctx.DB.InsertStepResult(sctx.Run.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		if err := sctx.DB.UpdateStepStatus(sr.ID, types.StepStatusCompleted); err != nil {
			t.Fatal(err)
		}
	}

	bodyFile := filepath.Join(t.TempDir(), "pr-body.md")
	if err := os.WriteFile(bodyFile, []byte(priorAttestedBody), 0o644); err != nil {
		t.Fatal(err)
	}
	logFile := filepath.Join(t.TempDir(), "gh.log")
	env := fakeCIGH(t, "OPEN", `[]`)
	sctx.Env = append(env,
		"FAKE_CLI_PR_LIST_JSON=[{\"number\":42,\"url\":\"https://github.com/test/repo/pull/42\",\"baseRefName\":\"main\"}]",
		"FAKE_CLI_PR_BODY_FILE="+bodyFile,
		"FAKE_CLI_PR_TITLE=fix: existing pr",
		"FAKE_CLI_LOG="+logFile,
	)

	if _, err := (&PushStep{}).Execute(sctx); err != nil {
		t.Fatalf("push step failed: %v", err)
	}

	remoteHead := gitCmd(t, upstream, "rev-parse", "refs/heads/feature")
	if remoteHead != newHead {
		t.Fatalf("remote head = %s, want %s", remoteHead, newHead)
	}

	updated := readFakeGHBodyArg(t, logFile)
	attestation := parsePipelineAttestationForTest(t, updated)
	if attestation.HeadSHA != newHead {
		t.Fatalf("attested head = %q, want the pushed head %q", attestation.HeadSHA, newHead)
	}
	if got, out := runVerifyPy(t, updated, newHead); got != "success" {
		t.Fatalf("attestation written before push must pass verify.py at the new head, got %s\n%s", got, out)
	}
	if got, out := runVerifyPy(t, priorAttestedBody, newHead); got != "failure" {
		t.Fatalf("the original stale attestation unexpectedly passed at the new head, got %s\n%s", got, out)
	}
	if got, out := runVerifyPy(t, updated, priorHead); got != "failure" {
		t.Fatalf("the new attestation unexpectedly passed at the OLD head - a check reading the body before the push lands must still see a mismatch, got %s\n%s", got, out)
	}
}

// TestPushStep_AttestationWriteFailureAbortsBeforePush is the ordering proof
// from the write side: when the attestation write itself cannot settle,
// Push fails before ever touching the remote. Silently continuing here would
// let code ship with no matching attestation at all - exactly the race this
// design exists to close.
func TestPushStep_AttestationWriteFailureAbortsBeforePush(t *testing.T) {
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir, baseSHA, priorHead := setupGitRepo(t)
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "push", "origin", "feature")

	priorAttestedBody := compliantPipelineBody(t, priorHead)

	if err := os.WriteFile(filepath.Join(dir, "new-work.txt"), []byte("new work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "new work")
	newHead := gitCmd(t, dir, "rev-parse", "HEAD")

	prURL := "https://github.com/test/repo/pull/42"
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, newHead, config.Commands{})
	sctx.Repo.UpstreamURL = "https://github.com/test/repo"
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Run.PRURL = &prURL
	setupGateMirror(t, sctx)
	recordReviewApproval(t, sctx, newHead)

	bodyFile := filepath.Join(t.TempDir(), "pr-body.md")
	if err := os.WriteFile(bodyFile, []byte(priorAttestedBody), 0o644); err != nil {
		t.Fatal(err)
	}
	env := fakeCIGH(t, "OPEN", `[]`)
	sctx.Env = append(env,
		"FAKE_CLI_PR_LIST_JSON=[{\"number\":42,\"url\":\"https://github.com/test/repo/pull/42\",\"baseRefName\":\"main\"}]",
		"FAKE_CLI_PR_BODY_FILE="+bodyFile,
		"FAKE_CLI_PR_TITLE=fix: existing pr",
		"FAKE_CLI_PR_EDIT_ERR=provider unavailable",
	)

	_, err := (&PushStep{}).Execute(sctx)
	if err == nil || !strings.Contains(err.Error(), "pipeline attestation write failed") {
		t.Fatalf("Execute() error = %v, want an attestation write failure", err)
	}

	remoteHead := gitCmd(t, upstream, "rev-parse", "refs/heads/feature")
	if remoteHead != priorHead {
		t.Fatalf("remote head = %s, want the push to never have happened (still %s)", remoteHead, priorHead)
	}
}

// TestPushStep_PushFailureAfterAttestationLeavesBodyAhead is the ordering
// proof from the other side, and the documented accepted failure mode: the
// attestation write succeeds, then the actual push is rejected by a genuine
// concurrent remote advance (a real git force-with-lease rejection, not a
// simulated error). The PR body is left attesting a head that never actually
// shipped until the run retries and republishes it - the mirror image of the
// old post-push design's failure (code shipped, attestation stuck).
func TestPushStep_PushFailureAfterAttestationLeavesBodyAhead(t *testing.T) {
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir, baseSHA, priorHead := setupGitRepo(t)
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "push", "origin", "feature")

	priorAttestedBody := compliantPipelineBody(t, priorHead)

	// A second clone prepares (but does not yet push) an interloper commit
	// that will land on the remote between the Push step's force-push
	// decision and its actual push attempt.
	other := t.TempDir()
	gitCmd(t, other, "clone", upstream, ".")
	gitCmd(t, other, "config", "user.name", "other")
	gitCmd(t, other, "config", "user.email", "other@test.com")
	gitCmd(t, other, "checkout", "feature")
	if err := os.WriteFile(filepath.Join(other, "intervening.txt"), []byte("intervening\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, other, "add", "-A")
	gitCmd(t, other, "commit", "-m", "intervening commit")

	if err := os.WriteFile(filepath.Join(dir, "new-work.txt"), []byte("new work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "new work")
	newHead := gitCmd(t, dir, "rev-parse", "HEAD")

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "git")
	linkTestBinary(t, binDir, "gh")

	prURL := "https://github.com/test/repo/pull/42"
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, newHead, config.Commands{})
	sctx.Repo.UpstreamURL = "https://github.com/test/repo"
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Run.PRURL = &prURL
	setupGateMirror(t, sctx)
	recordReviewApproval(t, sctx, newHead)

	bodyFile := filepath.Join(t.TempDir(), "pr-body.md")
	if err := os.WriteFile(bodyFile, []byte(priorAttestedBody), 0o644); err != nil {
		t.Fatal(err)
	}

	sctx.Env = []string{
		"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"FAKE_CLI_MODE=ci-gh-with-intervening-push",
		"FAKE_CLI_STATE=OPEN",
		"FAKE_CLI_CHECKS=[]",
		"FAKE_CLI_PR_HEAD_SHA=deadbeef",
		"FAKE_CLI_PR_LIST_JSON=[{\"number\":42,\"url\":\"https://github.com/test/repo/pull/42\",\"baseRefName\":\"main\"}]",
		"FAKE_CLI_PR_BODY_FILE=" + bodyFile,
		"FAKE_CLI_PR_TITLE=fix: existing pr",
		"FAKE_CLI_REAL_GIT=" + realGit,
		"FAKE_CLI_INTERLOPER_DIR=" + other,
		"FAKE_CLI_INTERLOPER_REMOTE=" + upstream,
		"FAKE_CLI_INTERLOPER_REF=feature",
	}

	_, err = (&PushStep{}).Execute(sctx)
	if err == nil {
		t.Fatal("expected the push to fail against the intervening remote advance")
	}
	if !strings.Contains(err.Error(), "stale info") && !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("Execute() error = %v, want a genuine force-with-lease rejection", err)
	}

	body, err := os.ReadFile(bodyFile)
	if err != nil {
		t.Fatal(err)
	}
	attestation := parsePipelineAttestationForTest(t, string(body))
	if attestation.HeadSHA != newHead {
		t.Fatalf("attested head = %q, want the head that failed to push (%q) - documents the accepted body-ahead failure mode", attestation.HeadSHA, newHead)
	}
	remoteHead := gitCmd(t, upstream, "rev-parse", "refs/heads/feature")
	if remoteHead == newHead {
		t.Fatal("expected the push to have actually failed - remote must not carry newHead")
	}
}

// TestPushStep_DoesNotMintAttestation confirms attestHeadBeforePush is
// strictly a rebind of an EXISTING attestation and never mints one for a PR
// that was not raised through no-mistakes.
func TestPushStep_DoesNotMintAttestation(t *testing.T) {
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir, baseSHA, _ := setupGitRepo(t)
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "push", "origin", "feature")

	if err := os.WriteFile(filepath.Join(dir, "new-work.txt"), []byte("new work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "new work")
	newHead := gitCmd(t, dir, "rev-parse", "HEAD")

	const foreign = "a regular pull request with no pipeline section"
	prURL := "https://github.com/test/repo/pull/42"
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, newHead, config.Commands{})
	sctx.Repo.UpstreamURL = "https://github.com/test/repo"
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Run.PRURL = &prURL
	setupGateMirror(t, sctx)
	recordReviewApproval(t, sctx, newHead)

	bodyFile := filepath.Join(t.TempDir(), "pr-body.md")
	if err := os.WriteFile(bodyFile, []byte(foreign), 0o644); err != nil {
		t.Fatal(err)
	}
	logFile := filepath.Join(t.TempDir(), "gh.log")
	env := fakeCIGH(t, "OPEN", `[]`)
	sctx.Env = append(env,
		"FAKE_CLI_PR_LIST_JSON=[{\"number\":42,\"url\":\"https://github.com/test/repo/pull/42\",\"baseRefName\":\"main\"}]",
		"FAKE_CLI_PR_BODY_FILE="+bodyFile,
		"FAKE_CLI_PR_TITLE=feat: hand rolled",
		"FAKE_CLI_LOG="+logFile,
	)

	if _, err := (&PushStep{}).Execute(sctx); err != nil {
		t.Fatalf("push step failed: %v", err)
	}

	if logData, err := os.ReadFile(logFile); err == nil && strings.Contains(string(logData), "pr edit") {
		t.Fatalf("must not write a PR body when no attestation was present:\n%s", logData)
	}
	body, err := os.ReadFile(bodyFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != foreign {
		t.Fatal("body without an attestation must be left untouched")
	}
	if got, out := runVerifyPy(t, foreign, newHead); got != "failure" {
		t.Fatalf("a PR not raised through no-mistakes must still fail, got %s\n%s", got, out)
	}
}

// TestPushStep_SkipsGhOnBaseBranch confirms attestHeadBeforePush skips a
// direct push to the configured PR base branch entirely, without building an
// SCM host or making any gh call at all - the PR step never manages a PR
// there either (see effectivePRBaseBranch), so a base-branch push must not
// gain a new dependency on an authenticated gh CLI.
func TestPushStep_SkipsGhOnBaseBranch(t *testing.T) {
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	if err := os.WriteFile(filepath.Join(dir, "direct.txt"), []byte("direct change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "direct change")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = "https://github.com/test/repo"
	sctx.Run.Branch = "refs/heads/main"
	setupGateMirror(t, sctx)
	recordReviewApproval(t, sctx, headSHA)

	logFile := filepath.Join(t.TempDir(), "gh.log")
	sctx.Env = append(fakeCIGH(t, "OPEN", `[]`), "FAKE_CLI_LOG="+logFile)

	if _, err := (&PushStep{}).Execute(sctx); err != nil {
		t.Fatalf("push step failed: %v", err)
	}

	if logData, err := os.ReadFile(logFile); err == nil && len(logData) > 0 {
		t.Fatalf("expected no gh invocation at all for a direct push to the base branch:\n%s", logData)
	}
}
