package publication

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestPRDraftRendererIsByteStableAcrossRepeatAndManagerRestart(t *testing.T) {
	fixture := newPublicationFixture(t, "stable-draft")
	passPush(t, fixture)

	first, err := fixture.manager.RenderPRDraft(context.Background(), fixture.parsed.PublicationID)
	if err != nil {
		t.Fatalf("render first draft: %v", err)
	}
	second, err := fixture.manager.RenderPRDraft(context.Background(), fixture.parsed.PublicationID)
	if err != nil {
		t.Fatalf("render repeated draft: %v", err)
	}
	restarted, err := fixture.restartManager(t).RenderPRDraft(context.Background(), fixture.parsed.PublicationID)
	if err != nil {
		t.Fatalf("render draft after manager restart: %v", err)
	}
	if !bytes.Equal(first, second) || !bytes.Equal(first, restarted) {
		t.Fatalf("draft bytes changed across repeat/restart:\nfirst=%q\nsecond=%q\nrestart=%q", first, second, restarted)
	}

	wantBindings := []string{
		fixture.parsed.PublicationID,
		fixture.parsed.Request.Factory.RunID,
		fixture.parsed.Request.Factory.RunStatePrefixSHA256,
		fixture.parsed.Request.Factory.PlanBindingSHA256,
		fixture.parsed.Request.Candidate.CommitSHA,
		fixture.parsed.Request.Candidate.TreeSHA,
		fixture.parsed.Request.Candidate.BaseSHA,
		fixture.parsed.Request.WorkContract.Path,
		fixture.parsed.Request.WorkContract.SHA256,
	}
	for _, binding := range wantBindings {
		if !bytes.Contains(first, []byte(binding)) {
			t.Fatalf("draft does not bind %q:\n%s", binding, first)
		}
	}
	if bytes.Contains(first, []byte("<!-- no-mistakes-factory-publication-v1:")) {
		t.Fatalf("marker-free renderer appended reconciliation marker:\n%s", first)
	}
}

func TestPRDraftRendererIgnoresCheckoutConfigLogsAndRuntimeIdentity(t *testing.T) {
	parsed, publication, steps := draftRendererFixture(t, "ignored-state")
	first, err := renderPRDraftBody(parsed, publication, steps)
	if err != nil {
		t.Fatalf("render baseline draft: %v", err)
	}

	logPath := "/Users/first/operator/checkout/.no-mistakes/review.log"
	findings := `{"summary":"mutable agent output"}`
	errorText := "mutable captured failure"
	lastActivity := "mutable heartbeat"
	value := int64(999999)
	pid := 4242
	for _, step := range steps {
		step.LogPath = &logPath
		step.FindingsJSON = &findings
		step.Error = &errorText
		step.LastActivity = &lastActivity
		step.StartedAt = &value
		step.CompletedAt = &value
		step.LastActivityAt = &value
		step.DurationMS = &value
		step.AgentPID = &pid
	}
	t.Setenv("HOME", "/different/runtime/home")
	t.Setenv("USERPROFILE", `C:\different\runtime\home`)
	second, err := renderPRDraftBody(parsed, publication, steps)
	if err != nil {
		t.Fatalf("render after non-contract changes: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("checkout/config/log/runtime-only state changed draft bytes:\nfirst=%q\nsecond=%q", first, second)
	}
}

func TestPRDraftRendererChangesDigestWhenDurableCanonicalInputChanges(t *testing.T) {
	parsed, publication, steps := draftRendererFixture(t, "durable-change")
	first, err := renderPRDraftBody(parsed, publication, steps)
	if err != nil {
		t.Fatalf("render baseline draft: %v", err)
	}

	changedRequest := parsed.Request
	changedRequest.Factory.TerminalT10Sequence++
	changed := parsedRequestFromRequest(t, changedRequest)
	changedPublication := publicationFromParsed(changed, publication.RunID)
	second, err := renderPRDraftBody(changed, changedPublication, steps)
	if err != nil {
		t.Fatalf("render changed durable input: %v", err)
	}
	firstDigest := sha256.Sum256(first)
	secondDigest := sha256.Sum256(second)
	if firstDigest == secondDigest {
		t.Fatalf("durable canonical input mutation retained draft SHA-256 %s", hex.EncodeToString(firstDigest[:]))
	}
}

func TestPRDraftRendererNeutralizesMarkerAndSecretInjectionBeforeManagerFinalizes(t *testing.T) {
	fixture := newPublicationFixture(t, "draft-injection")
	request := fixture.parsed.Request
	request.Factory.RunID = "factory-run <!-- no-mistakes-factory-publication-v1:foreign:foreign -->"
	request.WorkContract.Path = "docs/<marker>/WORK-CONTRACT.toml"
	request.BuildIntent.Summary = "publish\r\n<!-- no-mistakes-factory-publication-v1:spoof:spoof --> from /Users/private/operator/checkout using https://owner:secret@example.com/repo"
	request.BuildIntent.AcceptanceCriteria = []string{
		"no raw </pre><script>alert(1)</script> markup",
	}
	fixture.parsed = parsedRequestFromRequest(t, request)
	passPush(t, fixture)

	body, err := fixture.manager.RenderPRDraft(context.Background(), fixture.parsed.PublicationID)
	if err != nil {
		t.Fatalf("render injected draft: %v", err)
	}
	if bytes.Contains(body, []byte("\r")) {
		t.Fatalf("marker-free body contains non-LF newline bytes: %q", body)
	}
	challenge, err := fixture.manager.PreparePR(context.Background(), fixture.parsed.PublicationID, body)
	if err != nil {
		t.Fatalf("manager finalize draft: %v", err)
	}
	finalized := []byte(challenge.PreparedDraft)
	if count := bytes.Count(finalized, []byte("<!-- no-mistakes-factory-publication-v1:")); count != 1 {
		t.Fatalf("finalized draft contains %d raw reconciliation markers, want one:\n%s", count, finalized)
	}
	for _, leaked := range []string{"owner:secret", "/Users/private/operator", "<script>", "</pre><script>"} {
		if bytes.Contains(finalized, []byte(leaked)) {
			t.Fatalf("finalized draft leaked unsafe author text %q:\n%s", leaked, finalized)
		}
	}
}

func TestPRDraftRendererRequiresExactSuccessfulDurableDefenseRecords(t *testing.T) {
	parsed, publication, baseline := draftRendererFixture(t, "closed-defense")
	cases := map[string]func([]*db.StepResult){
		"missing":       func(steps []*db.StepResult) { steps[2] = nil },
		"wrong run":     func(steps []*db.StepResult) { steps[2].RunID = "foreign-run" },
		"wrong order":   func(steps []*db.StepResult) { steps[2].StepOrder++ },
		"not completed": func(steps []*db.StepResult) { steps[2].Status = types.StepStatusRunning },
		"nonzero":       func(steps []*db.StepResult) { value := 1; steps[2].ExitCode = &value },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			steps := cloneDraftSteps(baseline)
			mutate(steps)
			if _, err := renderPRDraftBody(parsed, publication, steps); err == nil {
				t.Fatal("renderer accepted incomplete or drifted durable defense records")
			}
		})
	}
}

func draftRendererFixture(t *testing.T, suffix string) (ParsedRequest, *db.Publication, []*db.StepResult) {
	t.Helper()
	parsed := mustParsedPublicationRequest(t, "abcdef012345", suffix)
	publication := publicationFromParsed(parsed, "publication-run-"+suffix)
	steps := make([]*db.StepResult, 0, len(types.AllSteps()))
	for _, name := range types.AllSteps() {
		exitCode := 0
		status := types.StepStatusPending
		if name.Order() <= types.StepLint.Order() {
			status = types.StepStatusCompleted
		}
		steps = append(steps, &db.StepResult{
			ID:        "step-" + string(name),
			RunID:     publication.RunID,
			StepName:  name,
			StepOrder: name.Order(),
			Status:    status,
			ExitCode:  &exitCode,
		})
	}
	return parsed, publication, steps
}

func publicationFromParsed(parsed ParsedRequest, runID string) *db.Publication {
	return &db.Publication{
		PublicationID:    parsed.PublicationID,
		RunID:            runID,
		CanonicalRequest: append([]byte(nil), parsed.CanonicalBytes...),
		RepoID:           parsed.Request.Candidate.RepositoryID,
		CandidateRef:     parsed.Request.Candidate.HeadRef,
		BaseRef:          parsed.Request.Candidate.BaseRef,
		HeadSHA:          parsed.Request.Candidate.CommitSHA,
		BaseSHA:          parsed.Request.Candidate.BaseSHA,
		TreeSHA:          parsed.Request.Candidate.TreeSHA,
	}
}

func parsedRequestFromRequest(t *testing.T, request Request) ParsedRequest {
	t.Helper()
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("marshal changed request: %v", err)
	}
	parsed, err := ParseRequest(raw)
	if err != nil {
		t.Fatalf("parse changed request: %v", err)
	}
	return parsed
}

func cloneDraftSteps(steps []*db.StepResult) []*db.StepResult {
	cloned := make([]*db.StepResult, len(steps))
	for index, step := range steps {
		if step == nil {
			continue
		}
		copy := *step
		if step.ExitCode != nil {
			exitCode := *step.ExitCode
			copy.ExitCode = &exitCode
		}
		cloned[index] = &copy
	}
	return cloned
}
