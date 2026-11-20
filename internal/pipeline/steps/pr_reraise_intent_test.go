package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A re-raise of an open PR regenerates the sections the pipeline owns, but the
// Intent says what the PR was opened to do. These tests pin that a follow-up
// run's intent never replaces it.

const (
	reraiseTitle          = "feat: helper"
	reraiseOriginalIntent = "Ship the original goal."
	reraiseFollowUpIntent = "Follow-up task: tighten one check."
)

func reraiseAgent() *mockAgent {
	return &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(`{"title":"` + reraiseTitle + `","body":"## What Changed\n\n- A helper."}`)}, nil
	}}
}

func reraiseContext(t *testing.T, prURL string) (sctx *pipeline.StepContext, bodyFile, logFile string) {
	t.Helper()
	dir, base, head := setupGitRepo(t)
	env, logFile := fakeGH(t, prURL)
	sctx = newPRTestContext(t, reraiseAgent(), dir, base, head, config.Commands{})
	sctx.Env = append(env, "FAKE_CLI_PR_TITLE="+reraiseTitle)
	completeReraiseStep(t, sctx, types.StepReview)
	return sctx, envEntry(env, "FAKE_CLI_PR_BODY_FILE"), logFile
}

func completeReraiseStep(t *testing.T, sctx *pipeline.StepContext, name types.StepName) {
	t.Helper()
	sr, err := sctx.DB.InsertStepResult(sctx.Run.ID, name)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateStepStatus(sr.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
}

func reraiseAttestationStatus(t *testing.T, body string, step types.StepName) types.StepStatus {
	t.Helper()
	start := strings.Index(body, pipelineAttestationCommentPrefix)
	if start < 0 {
		t.Fatalf("body carries no attestation:\n%s", body)
	}
	payload, _, ok := strings.Cut(body[start+len(pipelineAttestationCommentPrefix):], pipelineAttestationCommentClosingToken)
	if !ok {
		t.Fatalf("attestation is not closed:\n%s", body)
	}
	var attestation pipelineAttestation
	if err := json.Unmarshal([]byte(payload), &attestation); err != nil {
		t.Fatalf("attestation payload must be JSON: %v", err)
	}
	for _, item := range attestation.Steps {
		if item.Step == step {
			return item.Status
		}
	}
	return ""
}

func patchCount(t *testing.T, logFile string) int {
	t.Helper()
	data, err := os.ReadFile(logFile)
	if os.IsNotExist(err) {
		return 0 // gh has not been called yet.
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), "--method PATCH")
}

func TestPRStep_InitialRaiseWritesIntentFromTheTask(t *testing.T) {
	t.Parallel()
	sctx, bodyFile, logFile := reraiseContext(t, "")
	sctx.UserIntent = reraiseOriginalIntent

	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mustRead(t, logFile)), "pr create") {
		t.Fatalf("expected a new PR to be created:\n%s", mustRead(t, logFile))
	}
	body := string(mustRead(t, bodyFile))
	if !strings.HasPrefix(body, "## Intent\n\n"+reraiseOriginalIntent+"\n\n## What Changed") {
		t.Fatalf("created body does not lead with the task's intent:\n%s", body)
	}
}

func TestPRStep_ReRaisePreservesTheOriginalIntent(t *testing.T) {
	t.Parallel()
	// The original Intent carries a subheading, a fenced "## " line, and
	// trailing spaces, so a lossy section reader cannot pass.
	handWritten := "Ship the original goal.\n\n### Constraints\n\n- keep it small  \n\n```text\n## not a heading\n```\n\nRefs the first report."
	for _, tc := range []struct {
		name string
		seed func(t *testing.T, sctx *pipeline.StepContext, bodyFile string) string
	}{
		{name: "intent written by the initial raise", seed: func(t *testing.T, sctx *pipeline.StepContext, bodyFile string) string {
			sctx.UserIntent = reraiseOriginalIntent
			if _, err := (&PRStep{}).Execute(sctx); err != nil {
				t.Fatal(err)
			}
			intent, ok := liveIntentSection(string(mustRead(t, bodyFile)))
			if !ok || intent != reraiseOriginalIntent {
				t.Fatalf("initial raise intent = %q, %v", intent, ok)
			}
			return intent
		}},
		{name: "hand-written intent with nested markup", seed: func(t *testing.T, _ *pipeline.StepContext, bodyFile string) string {
			body := "## Intent\n\n" + handWritten + "\n\n## What Changed\n\n- An older summary.\n\n## Pipeline\n\nOld pipeline section.\n"
			if err := os.WriteFile(bodyFile, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			return handWritten
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sctx, bodyFile, logFile := reraiseContext(t, "https://github.com/test/repo/pull/42")
			original := tc.seed(t, sctx, bodyFile)
			before := string(mustRead(t, bodyFile))
			if strings.Contains(before, pipelineAttestationCommentPrefix) && reraiseAttestationStatus(t, before, types.StepTest) == types.StepStatusCompleted {
				t.Fatal("seed body already reports Test completed; the update would prove nothing")
			}
			patches := patchCount(t, logFile)

			// The follow-up run: new intent, and a newly completed step the
			// pipeline-owned sections must now report.
			sctx.UserIntent = reraiseFollowUpIntent
			completeReraiseStep(t, sctx, types.StepTest)
			if _, err := (&PRStep{}).Execute(sctx); err != nil {
				t.Fatal(err)
			}
			if got := patchCount(t, logFile); got != patches+1 {
				t.Fatalf("re-raise PATCH count = %d, want %d", got, patches+1)
			}
			after := string(mustRead(t, bodyFile))
			intent, ok := liveIntentSection(after)
			if !ok || intent != original {
				t.Fatalf("re-raise Intent = %q, want the original byte for byte %q\nbody:\n%s", intent, original, after)
			}
			if !strings.HasPrefix(after, "## Intent\n\n"+original+"\n\n") {
				t.Fatalf("re-raise moved the Intent section:\n%s", after)
			}
			if strings.Contains(after, reraiseFollowUpIntent) {
				t.Fatalf("follow-up intent leaked into the re-raised body:\n%s", after)
			}
			if got := reraiseAttestationStatus(t, after, types.StepTest); got != types.StepStatusCompleted {
				t.Fatalf("re-raised attestation reports Test as %q, want completed", got)
			}
			if publishedAttestationHead(t, after) != sctx.Run.HeadSHA {
				t.Fatalf("re-raised attestation head = %s, want %s", publishedAttestationHead(t, after), sctx.Run.HeadSHA)
			}
			if strings.Contains(after, "An older summary.") || !strings.Contains(after, "- A helper.") {
				t.Fatalf("pipeline-owned narrative was not regenerated:\n%s", after)
			}
		})
	}
}

func TestPRStep_ReRaiseWithNothingToUpdateLeavesTheBodyAlone(t *testing.T) {
	t.Parallel()
	sctx, bodyFile, logFile := reraiseContext(t, "https://github.com/test/repo/pull/42")
	sctx.UserIntent = reraiseOriginalIntent
	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if patchCount(t, logFile) != 1 {
		t.Fatalf("the first raise must write the body:\n%s", mustRead(t, logFile))
	}
	before := string(mustRead(t, bodyFile))

	sctx.UserIntent = reraiseFollowUpIntent
	outcome, err := (&PRStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := patchCount(t, logFile); got != 1 {
		t.Fatalf("re-raise with nothing to update wrote the PR (%d PATCH calls):\n%s", got, mustRead(t, logFile))
	}
	if after := string(mustRead(t, bodyFile)); after != before {
		t.Fatalf("body changed without a write:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if outcome.PRURL != "https://github.com/test/repo/pull/42" {
		t.Fatalf("PR URL = %q, want the existing PR", outcome.PRURL)
	}
}

// The tighten-only publication controls still win over a preserved Intent.
func TestPRStep_ReRaiseOmitIntentStillRemovesIt(t *testing.T) {
	t.Parallel()
	sctx, bodyFile, _ := reraiseContext(t, "https://github.com/test/repo/pull/42")
	sctx.UserIntent = reraiseOriginalIntent
	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	sctx.Run.OmitIntent = true
	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if body := string(mustRead(t, bodyFile)); strings.Contains(body, "## Intent") || strings.Contains(body, reraiseOriginalIntent) {
		t.Fatalf("omitted intent was republished:\n%s", body)
	}
}

func mustAssembleDraftPRBody(t *testing.T, sctx *pipeline.StepContext, whatChanged, riskLine, testingMD, pipelineMD, evidenceMD string, bodyLimit int, provider scm.Provider) string {
	t.Helper()
	body, err := assembleDraftPRBody(sctx, whatChanged, riskLine, testingMD, pipelineMD, evidenceMD, bodyLimit, provider)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// The kept Intent is author text: a hand-written closing line must still close
// its issue after a re-raise, so it is not neutralized like generated prose.
func TestPRStep_ReRaiseKeepsAHandWrittenClosingLine(t *testing.T) {
	t.Parallel()
	sctx, bodyFile, logFile := reraiseContext(t, "https://github.com/test/repo/pull/42")
	handWritten := "Fix the crash on start.\n\nCloses #12"
	seed := "## Intent\n\n" + handWritten + "\n\n## What Changed\n\n- An older summary.\n"
	if err := os.WriteFile(bodyFile, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	sctx.UserIntent = reraiseFollowUpIntent
	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if patchCount(t, logFile) != 1 {
		t.Fatalf("re-raise must write the regenerated body:\n%s", mustRead(t, logFile))
	}
	after := string(mustRead(t, bodyFile))
	if !strings.HasPrefix(after, "## Intent\n\n"+handWritten+"\n\n## What Changed") {
		t.Fatalf("hand-written Intent was not kept byte for byte:\n%s", after)
	}
	grammar := closingGrammarFor(scm.ProviderGitHub)
	if targets := closingTargets(extractClosingKeywordLines(after, grammar), "test/repo", grammar); len(targets) != 1 {
		t.Fatalf("closing reference no longer live after the re-raise (targets %v):\n%s", targets, after)
	}
}

// An Intent too long to leave room for the generated sections refuses the
// update; it is never shortened to make room.
func TestPRStep_ReRaiseRefusesAnIntentThatCannotFit(t *testing.T) {
	t.Parallel()
	sctx, bodyFile, logFile := reraiseContext(t, "https://github.com/test/repo/pull/42")
	handWritten := strings.Repeat("Original goal line.\n", maxPullRequestBodyBytes/len("Original goal line.\n"))
	handWritten = strings.TrimSuffix(handWritten, "\n")
	seed := "## Intent\n\n" + handWritten + "\n"
	if err := os.WriteFile(bodyFile, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	sctx.UserIntent = reraiseFollowUpIntent
	_, err := (&PRStep{}).Execute(sctx)
	if err == nil || !strings.Contains(err.Error(), "refusing to shorten author text") {
		t.Fatalf("Execute error = %v, want a refusal to shorten the kept Intent", err)
	}
	if got := patchCount(t, logFile); got != 0 {
		t.Fatalf("refused re-raise still wrote the PR (%d PATCH calls)", got)
	}
	if after := string(mustRead(t, bodyFile)); after != seed {
		t.Fatal("refused re-raise changed the live body")
	}
}

func TestAssembleDraftPRBody_KeptIntentIsNeverShortened(t *testing.T) {
	t.Parallel()
	kept := "Goal, as the author wrote it.\n\nCloses #12"
	pipelineMD := "## Pipeline\n\n" + pipelineAttestationCommentPrefix + `{"head":"abc"}` + pipelineAttestationCommentClosingToken
	sctx := &pipeline.StepContext{PublishedPRIntent: kept}
	whatChanged := "## What Changed\n\n" + strings.Repeat("- a long generated bullet\n", 40)
	block := "## Intent\n\n" + kept + "\n\n"

	limit := scm.PRBodyLen(block) + scm.PRBodyLen(pipelineMD) + 200
	got, err := assembleDraftPRBody(sctx, whatChanged, "low risk", "", pipelineMD, "", limit, scm.ProviderAzureDevOps)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, block) || scm.PRBodyLen(got) > limit || !strings.Contains(got, pipelineAttestationCommentPrefix) {
		t.Fatalf("generated text, not the kept Intent, must give way under the limit (%d):\n%s", limit, got)
	}

	for _, limit := range []int{scm.PRBodyLen(block) - 1, scm.PRBodyLen(block) + 10} {
		if _, err := assembleDraftPRBody(sctx, whatChanged, "low risk", "", pipelineMD, "", limit, scm.ProviderAzureDevOps); err == nil || !strings.Contains(err.Error(), "refusing to shorten author text") {
			t.Fatalf("limit %d: error = %v, want a refusal naming the limit", limit, err)
		} else if !strings.Contains(err.Error(), fmt.Sprintf("%d-character", limit)) {
			t.Fatalf("limit %d: refusal does not name the limit: %v", limit, err)
		}
	}
}

func TestLiveIntentSection(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, body, want string
		ok               bool
	}{
		{name: "no intent", body: "## What Changed\n\n- x\n"},
		{name: "empty intent", body: "## Intent\n\n## What Changed\n\n- x\n"},
		{name: "intent only inside a fence", body: "```md\n## Intent\n\nquoted\n```\n\n## What Changed\n"},
		{name: "stops at the next section", body: "## Intent\n\nGoal.\n\n## What Changed\n\n- x\n", want: "Goal.", ok: true},
		{name: "keeps a top-level title", body: "## Intent\n\nGoal.\n\n# Notes\n\n## Pipeline\n", want: "Goal.\n\n# Notes", ok: true},
		{name: "keeps its own sections", body: "## Intent\n\nGoal.\n\n## Constraints\n\n- must not Y\n\n## What Changed\n\n- x\n", want: "Goal.\n\n## Constraints\n\n- must not Y", ok: true},
		{name: "stops at the collapsed validation block", body: "## Intent\n\nGoal.\n\n" + validationDetailsOpen + "## Pipeline\n", want: "Goal.", ok: true},
		{name: "stops at the pipeline introduction", body: "## Intent\n\nGoal.\n\n" + noMistakesPRSignature + "\n", want: "Goal.", ok: true},
		{name: "stops at the minimal risk line", body: "## Intent\n\nGoal.\n\n⚠️ Medium: touches publication\n\n" + noMistakesPRSignature + "\n", want: "Goal.", ok: true},
		{name: "risk-like line without the introduction is content", body: "## Intent\n\n✅ Low effort.\n\n## Pipeline\n", want: "✅ Low effort.", ok: true},
		{name: "keeps subheadings and fenced headings", body: "## Intent\n\nGoal.\n\n### Detail\n\n```\n## inside\n```\n\n## Pipeline\n", want: "Goal.\n\n### Detail\n\n```\n## inside\n```", ok: true},
		{name: "last section", body: "## Intent\n\nGoal.\n", want: "Goal.", ok: true},
		{name: "indented code is content", body: "## Intent\n\nGoal:\n\n    ## Example\n    more\n\nAfter.\n\n## Pipeline\n", want: "Goal:\n\n    ## Example\n    more\n\nAfter.", ok: true},
		{name: "tab-indented code is content", body: "## Intent\n\nGoal.\n\n\t## Example\n\n## Pipeline\n", want: "Goal.\n\n\t## Example", ok: true},
		{name: "fence nested in a list item", body: "## Intent\n\n- step:\n\n    ```md\n    ## Example\n    ```\n\n## Pipeline\n", want: "- step:\n\n    ```md\n    ## Example\n    ```", ok: true},
		{name: "indented intent is not the heading", body: "Notes:\n\n    ## Intent\n    quoted\n\n## Intent\n\nReal goal.\n\n## What Changed\n", want: "Real goal.", ok: true},
		{name: "indented intent alone", body: "    ## Intent\n\n    quoted\n"},
		{name: "up to three spaces is a heading", body: "   ## Intent\n\nGoal.\n\n   ## What Changed\n", want: "Goal.", ok: true},
		{name: "closing hash sequence", body: "## Intent ##\n\nGoal.\n\n## Pipeline\n", want: "Goal.", ok: true},
		{name: "hash without a space is text", body: "## Intent\n\nGoal.\n\n##not-a-heading\n\n## Pipeline\n", want: "Goal.\n\n##not-a-heading", ok: true},
	} {
		got, ok := liveIntentSection(tc.body)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: liveIntentSection = %q, %v; want %q, %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

// The Intent the PR was opened with may carry its own "## " sections; only the
// sections the pipeline generates end it.
const reraiseSectionedIntent = "Ship the original goal.\n\n## Constraints\n\n- must not grow the API\n\n## Out of scope\n\nRefs the first report."

// Every update route refreshes the evidence the pipeline owns but republishes
// the PR's original Intent byte for byte: the ordinary rewrite, the owned
// appendix of pr.template and pr.update_text: keep, an appendix those left
// behind after the setting was removed, and a freshly supplied description.
func TestPRStep_EveryReRaiseRoutePreservesTheOriginalIntent(t *testing.T) {
	t.Parallel()
	const prURL = "https://github.com/test/repo/pull/42"
	keepContext := func(t *testing.T) (*pipeline.StepContext, string, string) {
		sctx, bodyFile, logFile := reraiseContext(t, prURL)
		sctx.Config.PR.UpdateText = config.PRUpdateTextKeep
		if err := os.WriteFile(bodyFile, []byte(keptAuthorBody), 0o644); err != nil {
			t.Fatal(err)
		}
		return sctx, bodyFile, logFile
	}
	templateContext := func(t *testing.T) (*pipeline.StepContext, string, string) {
		sctx, _, _ := templateTestContext(t)
		env, logFile := fakeGH(t, prURL)
		sctx.Env = append(env, "FAKE_CLI_PR_TITLE="+reraiseTitle)
		bodyFile := envEntry(env, "FAKE_CLI_PR_BODY_FILE")
		if err := os.WriteFile(bodyFile, []byte(filledPRTemplate), 0o644); err != nil {
			t.Fatal(err)
		}
		return sctx, bodyFile, logFile
	}
	for _, tc := range []struct {
		name     string
		setup    func(t *testing.T) (*pipeline.StepContext, string, string)
		followUp func(sctx *pipeline.StepContext)
	}{
		{name: "regenerate", setup: func(t *testing.T) (*pipeline.StepContext, string, string) { return reraiseContext(t, prURL) }},
		{name: "update_text keep", setup: keepContext},
		{name: "keep appendix after the setting is removed", setup: keepContext, followUp: func(sctx *pipeline.StepContext) {
			sctx.Config.PR.UpdateText = ""
		}},
		{name: "template", setup: templateContext},
		{name: "template appendix after the template is removed", setup: templateContext, followUp: func(sctx *pipeline.StepContext) {
			sctx.Config.PR.Template = ""
		}},
		{name: "fresh supplied description", setup: func(t *testing.T) (*pipeline.StepContext, string, string) { return reraiseContext(t, prURL) }, followUp: func(sctx *pipeline.StepContext) {
			body := suppliedPRBodyText
			sctx.Run.PRBody = &body
			sctx.Run.PRBodyFresh = true
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sctx, bodyFile, _ := tc.setup(t)
			sctx.UserIntent = reraiseSectionedIntent
			if _, err := (&PRStep{}).Execute(sctx); err != nil {
				t.Fatal(err)
			}
			first := string(mustRead(t, bodyFile))
			if intent, ok := publishedPRIntent(first); !ok || intent != reraiseSectionedIntent {
				t.Fatalf("first raise Intent = %q, %v; want the task's intent\nbody:\n%s", intent, ok, first)
			}
			if reraiseAttestationStatus(t, first, types.StepTest) == types.StepStatusCompleted {
				t.Fatal("first raise already reports Test completed; the update would prove nothing")
			}

			sctx.UserIntent = reraiseFollowUpIntent
			completeReraiseStep(t, sctx, types.StepTest)
			if tc.followUp != nil {
				tc.followUp(sctx)
			}
			if _, err := (&PRStep{}).Execute(sctx); err != nil {
				t.Fatal(err)
			}
			after := string(mustRead(t, bodyFile))
			if intent, ok := publishedPRIntent(after); !ok || intent != reraiseSectionedIntent {
				t.Fatalf("re-raise Intent = %q, %v; want the original byte for byte\nbody:\n%s", intent, ok, after)
			}
			if strings.Count(after, "## Intent") != 1 || strings.Contains(after, reraiseFollowUpIntent) {
				t.Fatalf("follow-up intent was published or the Intent repeated:\n%s", after)
			}
			if got := reraiseAttestationStatus(t, after, types.StepTest); got != types.StepStatusCompleted {
				t.Fatalf("re-raised attestation reports Test as %q, want completed", got)
			}
		})
	}
}

// A drafted body without its "## What Changed" heading is given one, so a
// later re-raise does not take the stale summary in as part of the Intent.
func TestPRStep_ReRaiseDoesNotAbsorbAHeadinglessSummary(t *testing.T) {
	t.Parallel()
	dir, base, head := setupGitRepo(t)
	env, _ := fakeGH(t, "https://github.com/test/repo/pull/42")
	bodyFile := envEntry(env, "FAKE_CLI_PR_BODY_FILE")
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(`{"title":"` + reraiseTitle + `","body":"- A helper."}`)}, nil
	}}
	sctx := newPRTestContext(t, ag, dir, base, head, config.Commands{})
	sctx.Env = append(env, "FAKE_CLI_PR_TITLE="+reraiseTitle)
	completeReraiseStep(t, sctx, types.StepReview)
	for _, intent := range []string{reraiseOriginalIntent, reraiseFollowUpIntent} {
		sctx.UserIntent = intent
		if _, err := (&PRStep{}).Execute(sctx); err != nil {
			t.Fatal(err)
		}
		body := string(mustRead(t, bodyFile))
		if got, ok := liveIntentSection(body); !ok || got != reraiseOriginalIntent {
			t.Fatalf("Intent = %q, %v; want %q\nbody:\n%s", got, ok, reraiseOriginalIntent, body)
		}
		if strings.Count(body, "- A helper.") != 1 {
			t.Fatalf("summary repeated or lost:\n%s", body)
		}
	}
}

// An owned update whose kept Intent cannot fit the body budget is refused,
// never shortened.
func TestPRStep_OwnedReRaiseRefusesAnIntentThatCannotFit(t *testing.T) {
	t.Parallel()
	sctx, bodyFile, logFile := reraiseContext(t, "https://github.com/test/repo/pull/42")
	huge := strings.TrimSuffix(strings.Repeat("Original goal line.\n", maxPullRequestBodyBytes/len("Original goal line.\n")), "\n")
	seed := "## Intent\n\n" + huge + "\n\n## What Changed\n\n- An older summary.\n"
	if err := os.WriteFile(bodyFile, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	body := suppliedPRBodyText
	sctx.Run.PRBody = &body
	sctx.Run.PRBodyFresh = true
	sctx.UserIntent = reraiseFollowUpIntent
	_, err := (&PRStep{}).Execute(sctx)
	if err == nil || !strings.Contains(err.Error(), "refusing to drop author text") {
		t.Fatalf("Execute error = %v, want a refusal to drop the kept Intent", err)
	}
	if got := patchCount(t, logFile); got != 0 {
		t.Fatalf("refused re-raise still wrote the PR (%d PATCH calls)", got)
	}
	if after := string(mustRead(t, bodyFile)); after != seed {
		t.Fatal("refused re-raise changed the live body")
	}
}
