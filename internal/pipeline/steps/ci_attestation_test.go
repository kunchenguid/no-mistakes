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

func TestCIStep_PublishRepairSkipsRestampWithoutReader(t *testing.T) {
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
		t.Fatalf("repair = %+v, want a published head advance without restamp", repair)
	}
	if !strings.Contains(f.log(), "skipping attestation rebind") {
		t.Fatalf("log did not skip restamp on a host without a PR content reader:\n%s", f.log())
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
