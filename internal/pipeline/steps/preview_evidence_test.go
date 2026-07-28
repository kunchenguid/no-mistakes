package steps

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestCIStep_DoesNotMarkReadyUntilDeferredPreviewEvidenceIsPublished(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	ag := &mockAgent{
		name: "test",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{
				"findings": [],
				"summary": "preview evidence captured",
				"tested": ["opened the deployed preview"],
				"testing_summary": "Confirmed the rendered change.",
				"artifacts": [{
					"kind": "screenshot",
					"label": "rendered preview",
					"url": "https://cdn.example.test/preview.png"
				}]
			}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	prURL := "https://github.com/test/repo/pull/42"
	sctx.Run.PRURL = &prURL
	sctx.Env = fakeCIGH(t, "OPEN", `[{
		"name":"Vercel – web",
		"state":"SUCCESS",
		"bucket":"pass",
		"link":"https://vercel.com/acme/web/deploy-123"
	}]`)
	sctx.Config.CITimeout = time.Hour

	testStep, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(Findings{
		Items:   []Finding{},
		Summary: "visual evidence deferred",
		DeferredEvidence: []types.DeferredEvidence{{
			Kind:         "screenshot",
			Label:        "rendered preview",
			Instructions: "Capture the deployed preview.",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.SetStepFindings(testStep.ID, string(raw)); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sctx.Ctx = ctx
	refreshes := 0
	step := &CIStep{
		pollIntervalOverride: time.Millisecond,
		waitForNextPoll: func(context.Context, time.Duration) error {
			cancel()
			return context.Canceled
		},
		refreshPREvidence: func(*pipeline.StepContext) error {
			run, err := sctx.DB.GetRun(sctx.Run.ID)
			if err != nil {
				return err
			}
			if run.CIReadyAt != nil {
				t.Fatal("CI readiness was persisted before preview evidence refreshed the PR")
			}
			refreshes++
			return nil
		},
	}

	_, err = step.Execute(sctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Execute() error = %v, want context cancellation after first ready poll", err)
	}
	if refreshes != 1 {
		t.Fatalf("PR refreshes = %d, want 1", refreshes)
	}
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.CIReadyAt == nil {
		t.Fatal("CI should become ready after preview evidence is published")
	}
}

func TestCIStep_NoChecksStillRequiresDeferredPreviewEvidence(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	ag := &mockAgent{
		name: "test",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{
				"findings": [],
				"summary": "preview evidence captured",
				"tested": ["opened the deployed preview"],
				"testing_summary": "Confirmed the rendered change.",
				"artifacts": [{
					"kind": "screenshot",
					"label": "rendered preview",
					"url": "https://cdn.example.test/preview.png"
				}]
			}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	prURL := "https://github.com/test/repo/pull/42"
	sctx.Run.PRURL = &prURL
	sctx.Env = fakeCIGHSequence(t, "OPEN", []string{"[]", "[]"})
	sctx.Config.CITimeout = time.Hour

	testStep, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(Findings{
		Items:   []Finding{},
		Summary: "visual evidence deferred",
		DeferredEvidence: []types.DeferredEvidence{{
			Kind:         "screenshot",
			Label:        "rendered preview",
			Instructions: "Capture the deployed preview.",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.SetStepFindings(testStep.ID, string(raw)); err != nil {
		t.Fatal(err)
	}

	started := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	current := started
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sctx.Ctx = ctx
	refreshes := 0
	step := &CIStep{
		now:                  func() time.Time { return current },
		pollIntervalOverride: time.Millisecond,
		waitForNextPoll: func(context.Context, time.Duration) error {
			if current.Equal(started) {
				current = started.Add(defaultChecksGracePeriod)
				return nil
			}
			cancel()
			return context.Canceled
		},
		refreshPREvidence: func(*pipeline.StepContext) error {
			refreshes++
			return nil
		},
	}

	_, err = step.Execute(sctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Execute() error = %v, want context cancellation after preview capture", err)
	}
	if len(ag.calls) != 1 || refreshes != 1 {
		t.Fatalf("preview evidence calls=%d refreshes=%d, want one each", len(ag.calls), refreshes)
	}
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.CIReadyAt == nil {
		t.Fatal("CI should become ready only after deferred preview evidence is published")
	}
}

func TestCIStep_CapturesDeferredEvidenceFromDeployedPreview(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{
				"findings": [{
					"severity": "info",
					"description": "Preview rendered with the expected responsive layout.",
					"action": "no-op"
				}],
				"summary": "preview evidence captured",
				"tested": ["opened the deployed preview review queue"],
				"testing_summary": "Confirmed the new labels in the deployed PR preview.",
				"artifacts": [{
					"kind": "screenshot",
					"label": "review queue labels",
					"url": "https://cdn.example.test/review-queue.png"
				}]
			}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	testStep, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	original := Findings{
		Items:          []Finding{},
		Summary:        "source checks passed",
		Tested:         []string{"focused component test"},
		TestingSummary: "Source-level validation passed; visual evidence is deferred.",
		DeferredEvidence: []types.DeferredEvidence{{
			Kind:         "screenshot",
			Label:        "review queue labels",
			Instructions: "Capture the rendered admin review queue from the deployed PR preview.",
		}},
	}
	raw, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.SetStepFindings(testStep.ID, string(raw)); err != nil {
		t.Fatal(err)
	}

	refreshed := 0
	step := &CIStep{
		refreshPREvidence: func(*pipeline.StepContext) error {
			refreshed++
			return nil
		},
	}
	prURL := "https://github.com/test/repo/pull/42"
	outcome, err := step.captureDeferredPreviewEvidence(sctx, prURL, []scm.Check{{
		Name:       "Vercel – web",
		Bucket:     scm.CheckBucketPass,
		DetailsURL: "https://vercel.com/acme/web/deploy-123",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if outcome != nil {
		t.Fatalf("captureDeferredPreviewEvidence outcome = %+v, want nil", outcome)
	}
	if refreshed != 1 {
		t.Fatalf("PR refresh calls = %d, want 1", refreshed)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("agent calls = %d, want 1", len(ag.calls))
	}
	for _, want := range []string{
		"Use the deployed PR preview, not a local development server",
		prURL,
		"Vercel – web: https://vercel.com/acme/web/deploy-123",
		"Capture the rendered admin review queue",
		"registered credential context: " + sctx.Repo.WorkingPath,
		"read only the named values required from env files beneath the registered credential context",
		"Never use a local APP_BASE_URL as the rendered target",
	} {
		if !strings.Contains(ag.calls[0].Prompt, want) {
			t.Fatalf("preview prompt missing %q:\n%s", want, ag.calls[0].Prompt)
		}
	}

	steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var updated Findings
	for _, result := range steps {
		if result.StepName != types.StepTest || result.FindingsJSON == nil {
			continue
		}
		if err := json.Unmarshal([]byte(*result.FindingsJSON), &updated); err != nil {
			t.Fatal(err)
		}
	}
	if len(updated.DeferredEvidence) != 0 {
		t.Fatalf("DeferredEvidence = %+v, want consumed", updated.DeferredEvidence)
	}
	if len(updated.Artifacts) != 1 || updated.Artifacts[0].URL != "https://cdn.example.test/review-queue.png" {
		t.Fatalf("Artifacts = %+v", updated.Artifacts)
	}
	if len(updated.Tested) != 2 {
		t.Fatalf("Tested = %+v, want source and preview checks", updated.Tested)
	}
	if len(updated.Items) != 1 || updated.Items[0].Action != types.ActionNoOp {
		t.Fatalf("Items = %+v, want informational preview finding", updated.Items)
	}

	outcome, err = step.captureDeferredPreviewEvidence(sctx, prURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != nil {
		t.Fatalf("second capture outcome = %+v, want nil", outcome)
	}
	if len(ag.calls) != 1 || refreshed != 1 {
		t.Fatalf("deferred evidence repeated: calls=%d refreshes=%d", len(ag.calls), refreshed)
	}
}

func TestCIStep_RestoresDeferredEvidenceWhenPRRefreshFails(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	ag := &mockAgent{
		name: "test",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{
				"findings": [],
				"summary": "preview evidence captured",
				"tested": ["opened the deployed preview"],
				"testing_summary": "Confirmed the rendered change.",
				"artifacts": [{
					"kind": "screenshot",
					"label": "rendered preview",
					"url": "https://cdn.example.test/preview.png"
				}]
			}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	testStep, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(Findings{
		Items:   []Finding{},
		Summary: "visual evidence deferred",
		DeferredEvidence: []types.DeferredEvidence{{
			Kind:         "screenshot",
			Label:        "rendered preview",
			Instructions: "Capture the deployed preview.",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.SetStepFindings(testStep.ID, string(raw)); err != nil {
		t.Fatal(err)
	}

	refreshErr := errors.New("update pull request")
	step := &CIStep{
		refreshPREvidence: func(*pipeline.StepContext) error {
			return refreshErr
		},
	}
	_, err = step.captureDeferredPreviewEvidence(
		sctx,
		"https://github.com/test/repo/pull/42",
		[]scm.Check{{Name: "deploy", Bucket: scm.CheckBucketPass}},
	)
	if !errors.Is(err, refreshErr) {
		t.Fatalf("captureDeferredPreviewEvidence() error = %v, want %v", err, refreshErr)
	}

	steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range steps {
		if result.ID != testStep.ID || result.FindingsJSON == nil {
			continue
		}
		if *result.FindingsJSON != string(raw) {
			t.Fatalf("findings after failed refresh = %s, want original %s", *result.FindingsJSON, raw)
		}
		return
	}
	t.Fatal("test step not found")
}

func TestCIStep_DeferredVisualEvidenceRequiresPublicArtifact(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	ag := &mockAgent{
		name: "test",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{
				"findings": [],
				"summary": "captured locally",
				"tested": ["opened preview"],
				"testing_summary": "Captured a screenshot.",
				"artifacts": [{
					"kind": "screenshot",
					"label": "preview",
					"path": "/tmp/preview.png"
				}]
			}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	testStep, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(Findings{
		Items:   []Finding{},
		Summary: "visual evidence deferred",
		DeferredEvidence: []types.DeferredEvidence{{
			Kind:         "screenshot",
			Label:        "preview",
			Instructions: "Capture the deployed preview.",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.SetStepFindings(testStep.ID, string(raw)); err != nil {
		t.Fatal(err)
	}

	outcome, err := (&CIStep{}).captureDeferredPreviewEvidence(
		sctx,
		"https://github.com/test/repo/pull/42",
		[]scm.Check{{Name: "deploy", Bucket: scm.CheckBucketPass}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("outcome = %+v, want approval gate", outcome)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings.Items) != 1 || findings.Items[0].Action != types.ActionAskUser {
		t.Fatalf("findings = %+v, want one ask-user finding", findings.Items)
	}
	if !strings.Contains(findings.Items[0].Description, "publicly accessible") {
		t.Fatalf("finding description = %q", findings.Items[0].Description)
	}
}
