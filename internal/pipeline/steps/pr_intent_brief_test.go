package steps

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func hasPRIntentBriefSchema(t *testing.T, raw json.RawMessage) bool {
	t.Helper()
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("unparseable PR output schema: %v", err)
	}
	briefRaw, ok := schema.Properties["intent_brief"]
	if !ok {
		return false
	}
	var brief struct {
		Properties map[string]struct {
			Type      string `json:"type"`
			MaxLength int    `json:"maxLength"`
			MaxItems  int    `json:"maxItems"`
			Items     struct {
				Type      string `json:"type"`
				MaxLength int    `json:"maxLength"`
			} `json:"items"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(briefRaw, &brief); err != nil {
		t.Fatalf("unparseable intent_brief schema: %v", err)
	}
	goal, why, scope, out := brief.Properties["goal"], brief.Properties["why"], brief.Properties["scope"], brief.Properties["out_of_scope"]
	outerRequiresBrief := false
	for _, field := range schema.Required {
		outerRequiresBrief = outerRequiresBrief || field == "intent_brief"
	}
	innerRequires := map[string]bool{}
	for _, field := range brief.Required {
		innerRequires[field] = true
	}
	return outerRequiresBrief && goal.Type == "string" && goal.MaxLength == 200 &&
		why.Type == "array" && why.MaxItems == 3 && why.Items.Type == "string" && why.Items.MaxLength == 220 &&
		scope.Type == "array" && scope.MaxItems == 3 && scope.Items.Type == "string" && scope.Items.MaxLength == 220 &&
		out.Type == "array" && out.MaxItems == 3 && out.Items.Type == "string" && out.Items.MaxLength == 220 &&
		innerRequires["goal"] && innerRequires["why"] && innerRequires["scope"] && innerRequires["out_of_scope"]
}

func TestRenderPRIntentBriefUsesConfiguredSectionsAndOrder(t *testing.T) {
	got, err := renderPRIntentBrief(prIntentBrief{
		Goal:       "Give reviewers a clear migration path",
		Why:        []string{"Reduce upgrade mistakes"},
		Scope:      []string{"Add the validation command"},
		OutOfScope: []string{"Do not change existing defaults"},
	}, []string{"scope", "why"})
	if err != nil {
		t.Fatal(err)
	}
	want := "Give reviewers a clear migration path\n\n### Scope\n- Add the validation command\n\n### Why\n- Reduce upgrade mistakes"
	if got != want {
		t.Fatalf("rendered brief = %q, want %q", got, want)
	}
	if strings.Contains(got, "Out of scope") {
		t.Fatalf("unselected section was rendered: %s", got)
	}
}

func TestPRStep_BriefIsSeparateFromStoredIntent(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	const source = "Add the feature; internal apply run 123 and operator title are not reviewer context."
	ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if !strings.Contains(opts.Prompt, source) || !strings.Contains(opts.Prompt, "intent_brief") || !hasPRIntentBriefSchema(t, opts.JSONSchema) {
			t.Fatalf("PR draft prompt or schema lacks original review criteria or separate brief contract: %s", opts.Prompt)
		}
		return &agent.Result{Output: json.RawMessage(`{"title":"feat: add validation","body":"## What Changed\n\n- Add validation","intent_brief":{"goal":"Validate the submitted change","why":["Catch mistakes before execution"],"scope":["Check configured terms"],"out_of_scope":["Keep the default behavior unchanged"]}}`)}, nil
	}}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	repo, err := sctx.DB.InsertRepo(dir, "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	intentRecord := &db.RunIntent{Summary: source, Source: db.RunIntentSourceAgent, SessionID: "review-session", Score: 1}
	run, err := sctx.DB.InsertRunWithIntentAndLaunchNonce(repo.ID, "refs/heads/feature", headSHA, baseSHA, intentRecord, "launch-nonce", "validation-generation", "immutable-intent-digest", "", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	sctx.Repo, sctx.Run, sctx.UserIntent = repo, run, source
	sctx.Config.PR.Intent.Format = "brief"
	sctx.Config.PR.Intent.Sections = []string{"why", "scope", "out_of_scope"}
	insertCompletedStep(t, sctx, types.StepReview, "", "")

	content, err := (&PRStep{}).buildPRContent(sctx, "feature", "main", baseSHA, scm.ProviderGitHub, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"## Intent\n\nValidate the submitted change",
		"### Why\n- Catch mistakes before execution",
		"### Scope\n- Check configured terms",
		"### Out of scope\n- Keep the default behavior unchanged",
		"## What Changed",
	} {
		if !strings.Contains(content.Body, want) {
			t.Errorf("body missing %q:\n%s", want, content.Body)
		}
	}
	if strings.Contains(content.Body, source) || strings.Contains(content.Body, "internal apply run 123") {
		t.Fatalf("raw intent leaked into the public body:\n%s", content.Body)
	}
	if strings.Count(content.Body, "## Intent") != 1 || strings.Count(content.Body, pipelineAttestationCommentPrefix) != 1 {
		t.Fatalf("Intent/attestation count is wrong:\n%s", content.Body)
	}
	storedAfter, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sctx.UserIntent != source || storedAfter.Intent == nil || *storedAfter.Intent != source || storedAfter.IntentSource == nil || *storedAfter.IntentSource != db.RunIntentSourceAgent || storedAfter.IntentSessionID == nil || *storedAfter.IntentSessionID != "review-session" || storedAfter.IntentScore == nil || *storedAfter.IntentScore != 1 || storedAfter.LaunchNonce == nil || *storedAfter.LaunchNonce != "launch-nonce" || storedAfter.LaunchValidationGeneration == nil || *storedAfter.LaunchValidationGeneration != "validation-generation" || storedAfter.LaunchIntentDigest == nil || *storedAfter.LaunchIntentDigest != "immutable-intent-digest" {
		t.Fatalf("PR drafting changed stored intent, provenance, or launch digest: context=%q stored=%+v", sctx.UserIntent, storedAfter)
	}
}

func TestPRStep_TemplatePublishesTheSameStructuredBrief(t *testing.T) {
	t.Parallel()
	sctx, ag, _ := templateTestContext(t)
	const source = "The internal task ID task-735 is not reviewer context."
	sctx.UserIntent = source
	sctx.Config.PR.Intent.Format = "brief"
	sctx.Config.PR.Intent.Sections = []string{"scope", "why"}
	ag.runFn = func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if !strings.Contains(opts.Prompt, source) || !strings.Contains(opts.Prompt, "intent_brief") || !hasPRIntentBriefSchema(t, opts.JSONSchema) {
			t.Fatalf("template prompt or schema lacks original criteria or brief contract: %s", opts.Prompt)
		}
		payload, err := json.Marshal(prContent{
			Title: "feat: fill template",
			Body:  filledPRTemplate,
			IntentBrief: &prIntentBrief{
				Goal:  "Fill the contribution template",
				Why:   []string{"Make changes easier to review"},
				Scope: []string{"Preserve repository headings"},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return &agent.Result{Output: payload}, nil
	}

	content, err := (&PRStep{}).buildPRContent(sctx, "feature", "main", sctx.Run.BaseSHA, scm.ProviderGitHub, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content.Body, "## Intent\n\nFill the contribution template\n\n### Scope\n- Preserve repository headings\n\n### Why\n- Make changes easier to review") {
		t.Fatalf("template appendix did not use the structured brief:\n%s", content.Body)
	}
	if strings.Contains(content.Body, source) || strings.Count(content.Body, "## Intent") != 1 || strings.Count(content.Body, pipelineAttestationCommentPrefix) != 1 {
		t.Fatalf("template publication leaked source or duplicated managed sections:\n%s", content.Body)
	}
}

func TestPRStep_BriefPublicationPolicySkipsGeneration(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	const source = "source text remains drafting context under repository publication policy"
	ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if !strings.Contains(opts.Prompt, source) || strings.Contains(opts.Prompt, "intent_brief") {
			t.Fatalf("repository omission changed legacy prompt context or requested a brief: %s", opts.Prompt)
		}
		return &agent.Result{Output: json.RawMessage(`{"title":"feat: add validation","body":"## What Changed\n\n- Add validation"}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.UserIntent = source
	sctx.Config.PR.Intent.Format = "brief"
	publish := false
	sctx.Config.PR.PublishIntent = &publish

	content, err := (&PRStep{}).buildPRContent(sctx, "feature", "main", baseSHA, scm.ProviderGitHub, 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(content.Body, "## Intent") || strings.Contains(content.Body, source) {
		t.Fatalf("trusted publication policy leaked Intent into the body:\n%s", content.Body)
	}
}

func TestPRStep_OwnedAppendixDraftsBriefWithoutReplacingAuthorNarrative(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	const source = "Keep raw internal run run-411 out of the public brief."
	ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if !strings.Contains(opts.Prompt, source) || !strings.Contains(opts.Prompt, "intent_brief") {
			t.Fatalf("brief-only prompt lacks review criteria or output contract: %s", opts.Prompt)
		}
		return &agent.Result{Output: json.RawMessage(`{"intent_brief":{"goal":"Keep the release check reliable","why":["Catch invalid changes early"]}}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.UserIntent = source
	sctx.Config.PR.Intent.Format = "brief"
	sctx.Config.PR.Intent.Sections = []string{"why"}
	insertCompletedStep(t, sctx, types.StepReview, "", "")
	step := &PRStep{}
	if err := step.draftPRIntentBrief(sctx, "feature", "main", baseSHA); err != nil {
		t.Fatal(err)
	}
	appendix, err := step.buildPRAppendix(sctx, scm.ProviderGitHub)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(appendix, "## Intent\n\nKeep the release check reliable\n\n### Why\n- Catch invalid changes early") {
		t.Fatalf("owned appendix lacks brief:\n%s", appendix)
	}
	if strings.Contains(appendix, source) || strings.Count(appendix, "## Intent") != 1 || strings.Count(appendix, pipelineAttestationCommentPrefix) != 1 {
		t.Fatalf("owned appendix leaked source or duplicated managed sections:\n%s", appendix)
	}
}

func TestPRStep_ExistingPRUpdateAddsBriefWithoutReplacingNarrative(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	const (
		source    = "Keep internal follow-up token task-735 out of the public brief."
		narrative = "## What Changed\n\n- Preserve the author's existing explanation"
	)
	createEnv, _ := fakeGH(t, "")
	ag := &mockAgent{name: "test", runFn: func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(`{"title":"feat: preserve the existing narrative","body":"## What Changed\n\n- Preserve the author's existing explanation"}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = createEnv
	sctx.UserIntent = source
	sctx.Config.PR.UpdateText = config.PRUpdateTextKeep
	insertCompletedStep(t, sctx, types.StepReview, "", "")
	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	created := string(mustRead(t, envEntry(createEnv, "FAKE_CLI_PR_BODY_FILE")))
	if !strings.Contains(created, narrative) {
		t.Fatalf("created PR omitted narrative:\n%s", created)
	}

	sctx.Config.PR.Intent.Format = "brief"
	updateEnv, _ := fakeGH(t, "https://github.com/test/repo/pull/42")
	bodyFile := envEntry(updateEnv, "FAKE_CLI_PR_BODY_FILE")
	if err := os.WriteFile(bodyFile, []byte(created), 0o600); err != nil {
		t.Fatal(err)
	}
	ag.runFn = func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if strings.Contains(opts.Prompt, "Draft a pull request title and summary") {
			t.Fatalf("existing narrative update drafted replacement content: %s", opts.Prompt)
		}
		return &agent.Result{Output: json.RawMessage(`{"intent_brief":{"goal":"Keep the existing narrative stable"}}`)}, nil
	}
	sctx.Env = updateEnv
	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	updated := string(mustRead(t, bodyFile))
	if !strings.Contains(updated, narrative) || !strings.Contains(updated, "## Intent\n\nKeep the existing narrative stable") {
		t.Fatalf("existing PR update lost narrative or structured brief:\n%s", updated)
	}
	if strings.Contains(updated, source) || strings.Contains(updated, "task-735") || strings.Count(updated, "## Intent") != 1 {
		t.Fatalf("updated PR leaked raw intent or duplicated the brief:\n%s", updated)
	}
	if !strings.HasPrefix(updated, narrative) {
		t.Fatalf("updated PR changed the author's narrative prefix:\n%s", updated)
	}
}

func TestPRStep_ConfiguredTitleTurnAlsoReturnsBrief(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	const source = "Keep the full launch request as review criteria."
	ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if !strings.Contains(opts.Prompt, source) || !hasPRIntentBriefSchema(t, opts.JSONSchema) {
			t.Fatalf("configured title turn lacks source or brief contract: %s", opts.Prompt)
		}
		return &agent.Result{Output: json.RawMessage(`{"title":"improve reliability","intent_brief":{"goal":"Improve reliability","scope":["Use the existing validation path"]}}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.UserIntent = source
	sctx.Config.PR.Intent.Format = "brief"
	sctx.Config.PR.Intent.Sections = []string{"scope"}
	sctx.Config.PR.TitleFormat = "{{.Title}}"

	title, err := (&PRStep{}).draftConfiguredPRTitle(sctx, "feature", "main", baseSHA)
	if err != nil {
		t.Fatal(err)
	}
	if title != "improve reliability" || !strings.Contains(sctx.PublicIntentBrief, "### Scope\n- Use the existing validation path") || strings.Contains(sctx.PublicIntentBrief, source) {
		t.Fatalf("title/brief output = %q, %q", title, sctx.PublicIntentBrief)
	}
}

func TestPRStep_BriefOmissionSkipsGenerationAndPublication(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	const source = "Private source that must not be drafted or published"
	ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if strings.Contains(opts.Prompt, source) || strings.Contains(opts.Prompt, "intent_brief") {
			t.Fatalf("omitted intent reached PR drafting prompt: %s", opts.Prompt)
		}
		return &agent.Result{Output: json.RawMessage(`{"title":"feat: add validation","body":"## What Changed\n\n- Add validation"}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.UserIntent = source
	sctx.Run.OmitIntent = true
	sctx.Config.PR.Intent.Format = "brief"

	content, err := (&PRStep{}).buildPRContent(sctx, "feature", "main", baseSHA, scm.ProviderGitHub, 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(content.Body, "## Intent") || strings.Contains(content.Body, source) {
		t.Fatalf("omitted intent was published:\n%s", content.Body)
	}
}

func TestPRStep_BriefModeFailsClosedWithoutStructuredOutput(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	const source = "private intent that must not be used as fallback"
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(`{"title":"feat: add validation","body":"## What Changed\\n\\n- Add validation"}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.UserIntent = source
	sctx.Config.PR.Intent.Format = "brief"
	if _, err := (&PRStep{}).buildPRContent(sctx, "feature", "main", baseSHA, scm.ProviderGitHub, 0); err == nil || strings.Contains(err.Error(), source) {
		t.Fatalf("buildPRContent error = %v, want non-disclosing fail-closed error", err)
	}
}

func TestFallbackPRContentRefusesRawIntentInBriefMode(t *testing.T) {
	sctx := &pipeline.StepContext{Config: &config.Config{PR: config.PR{Intent: config.PRIntent{Format: "brief"}}}, UserIntent: "private raw intent"}
	if _, err := (&PRStep{}).fallbackPRContent(sctx, "", "", "", "", "", 0, scm.ProviderGitHub); err == nil || strings.Contains(err.Error(), "private raw intent") {
		t.Fatalf("fallback error = %v, want a non-disclosing fail-closed error", err)
	}
}
