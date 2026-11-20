package steps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
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
	if !strings.HasPrefix(body, "## Intent\n\n"+reraiseOriginalIntent+"\n\n"+prIntentEndMarker+"\n\n## What Changed") {
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
			intent, err := liveIntentSection(string(mustRead(t, bodyFile)))
			if err != nil || intent != reraiseOriginalIntent {
				t.Fatalf("initial raise intent = %q, %v", intent, err)
			}
			return intent
		}},
		{name: "hand-written intent with nested markup", seed: func(t *testing.T, _ *pipeline.StepContext, bodyFile string) string {
			body := intentSection(handWritten) + "\n\n## What Changed\n\n- An older summary.\n\n## Pipeline\n\nOld pipeline section.\n"
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
			intent, err := liveIntentSection(after)
			if err != nil || intent != original {
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
	seed := intentSection(handWritten) + "\n\n## What Changed\n\n- An older summary.\n"
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
	if !strings.HasPrefix(after, "## Intent\n\n"+handWritten+"\n\n"+prIntentEndMarker+"\n\n## What Changed") {
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
	seed := intentSection(handWritten) + "\n"
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
	end := "\n\n" + prIntentEndMarker + "\n"
	for _, tc := range []struct {
		name, body, want string
		err              error
	}{
		{name: "no intent", body: "## What Changed\n\n- x\n"},
		{name: "empty intent", body: "## Intent" + end + "\n## What Changed\n\n- x\n"},
		{name: "intent only inside a fence", body: "```md\n## Intent\n\nquoted\n```\n\n## What Changed\n"},
		{name: "ends at the marker", body: "## Intent\n\nGoal." + end + "\n## What Changed\n\n- x\n", want: "Goal."},
		{name: "keeps generator-spelled author headings", body: "## Intent\n\nGoal.\n\n## Testing\n\n- run make e2e\n\n## What Changed\n\n- old\n\n# Notes" + end + "\n## What Changed\n\n- new\n\n## Pipeline\n", want: "Goal.\n\n## Testing\n\n- run make e2e\n\n## What Changed\n\n- old\n\n# Notes"},
		{name: "never takes in what follows the marker", body: "## Intent\n\nGoal." + end + "\n- A summary bullet.\n\n" + validationDetailsOpen + "## Pipeline\n", want: "Goal."},
		{name: "fenced marker is content", body: "## Intent\n\n```md\n" + prIntentEndMarker + "\n```\n\nMore." + end, want: "```md\n" + prIntentEndMarker + "\n```\n\nMore."},
		{name: "indented code is content", body: "## Intent\n\nGoal:\n\n    ## Example\n    " + prIntentEndMarker + "\n\nAfter." + end, want: "Goal:\n\n    ## Example\n    " + prIntentEndMarker + "\n\nAfter."},
		{name: "exact heading after a near miss wins", body: "Notes:\n\n    ## Intent\n    quoted\n\n## Intent\n\nReal goal." + end, want: "Real goal."},
		{name: "markerless intent is unmarked", body: "## Intent\n\nGoal.\n\n## What Changed\n\n- x\n\n## Pipeline\n", err: errUnmarkedPRIntent},
		{name: "marker only inside a fence is unmarked", body: "## Intent\n\n```\n" + prIntentEndMarker + "\n```\n", err: errUnmarkedPRIntent},
		{name: "re-cased heading is a near miss", body: "## intent\n\nGoal." + end, err: errNearMissPRIntent},
		{name: "re-indented heading is a near miss", body: "   ## Intent\n\nGoal." + end, err: errNearMissPRIntent},
		{name: "renamed heading is a near miss", body: "## Intent:\n\nGoal." + end, err: errNearMissPRIntent},
		{name: "closing hash sequence is a near miss", body: "## Intent ##\n\nGoal." + end, err: errNearMissPRIntent},
		{name: "other level is a near miss", body: "### Intent\n\nGoal." + end, err: errNearMissPRIntent},
	} {
		got, err := liveIntentSection(tc.body)
		if got != tc.want || !errors.Is(err, tc.err) || (tc.err == nil && err != nil) {
			t.Errorf("%s: liveIntentSection = %q, %v; want %q, %v", tc.name, got, err, tc.want, tc.err)
		}
	}
}

// Without the exact heading the generator writes, the step cannot tell a
// hand-edited section from author prose, so it states what it did not find:
// replaced when this run has an intent, removed when it has none.
func TestPRStep_ReRaiseStatesWhyItCannotKeepTheIntent(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, seed, intent, want string
	}{
		{name: "near-miss heading", seed: "## intent:\n\n" + reraiseOriginalIntent + "\n\n" + prIntentEndMarker + "\n\n## What Changed\n\n- An older summary.\n", intent: reraiseFollowUpIntent, want: "intent replaced: no exact ## Intent heading in the published body"},
		{name: "markerless without a run intent", seed: "## Intent\n\n" + reraiseOriginalIntent + "\n\n## What Changed\n\n- An older summary.\n", want: "intent removed: no intent marker in the published body; republish once to opt in"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sctx, bodyFile, _ := reraiseContext(t, "https://github.com/test/repo/pull/42")
			if err := os.WriteFile(bodyFile, []byte(tc.seed), 0o644); err != nil {
				t.Fatal(err)
			}
			var logs []string
			sctx.Log = func(line string) { logs = append(logs, line) }
			sctx.UserIntent = tc.intent
			if _, err := (&PRStep{}).Execute(sctx); err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(logs, tc.want) {
				t.Fatalf("step log does not state %q: %q", tc.want, logs)
			}
			after := string(mustRead(t, bodyFile))
			if strings.Contains(after, reraiseOriginalIntent) {
				t.Fatalf("the unkept Intent was republished:\n%s", after)
			}
			if tc.intent != "" {
				if got, err := liveIntentSection(after); err != nil || got != tc.intent {
					t.Fatalf("Intent = %q, %v; want this run's", got, err)
				}
			}
		})
	}
}

// A clamp that must shorten what follows the Intent finds the Intent's end the
// way a re-raise does: a marker line inside a fenced example is content, so
// the section ends at the real marker and nothing after it opens a fence.
func TestAssembleDraftPRBody_ClampEndsTheIntentAtTheRealMarker(t *testing.T) {
	t.Parallel()
	provider := scm.ProviderAzureDevOps
	limit := scm.MaxPRBodyChars(provider)
	intent := "Goal:\n\n```\nexample\n\n" + prIntentEndMarker + "\n```\n\nAfter."
	whatChanged := "## What Changed\n\n" + strings.Repeat("- a long generated bullet\n", 2*limit/len("- a long generated bullet\n"))
	pipelineMD := "## Pipeline\n\n" + noMistakesPRSignature + "\n\n" + pipelineAttestationCommentPrefix + `{"head_sha":"abc"}` + pipelineAttestationCommentClosingToken
	body, err := assembleDraftPRBody(&pipeline.StepContext{Config: &config.Config{}, UserIntent: intent}, whatChanged, "✅ Low: small", "", pipelineMD, "", limit, provider)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := liveIntentSection(body); err != nil || got != intent {
		t.Fatalf("published Intent = %q, %v; want the whole fenced example\nbody:\n%s", got, err, body)
	}
	if attestation := strings.Index(body, pipelineAttestationCommentPrefix); attestation < 0 || markdownFenceOpen(body[:attestation]) || scm.PRBodyLen(body) > limit {
		t.Fatalf("the generated sections render inside a fence or the limit is broken:\n%s", body)
	}
}

// A body published without the end marker cannot say where the author's text
// ends, so the re-raise replaces its Intent with this run's and says so.
func TestPRStep_ReRaiseReplacesAMarkerlessIntentAndSaysWhy(t *testing.T) {
	t.Parallel()
	sctx, bodyFile, _ := reraiseContext(t, "https://github.com/test/repo/pull/42")
	seed := "## Intent\n\n" + reraiseOriginalIntent + "\n\n## What Changed\n\n- An older summary.\n"
	if err := os.WriteFile(bodyFile, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	var logs []string
	sctx.Log = func(line string) { logs = append(logs, line) }
	sctx.UserIntent = reraiseFollowUpIntent
	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	after := string(mustRead(t, bodyFile))
	if intent, err := liveIntentSection(after); err != nil || intent != reraiseFollowUpIntent {
		t.Fatalf("re-raise Intent = %q, %v; want this run's intent, now marked\nbody:\n%s", intent, err, after)
	}
	if !slices.Contains(logs, "intent replaced: no intent marker in the published body; republish once to opt in") {
		t.Fatalf("replacement was not stated in the step log: %q", logs)
	}
}

// A provider body limit that forces the first publish to shorten its Intent
// cuts only the Intent text, so the published Intent keeps its end marker and
// a later re-raise republishes it within the same limit.
func TestAssembleDraftPRBody_BodyLimitKeepsTheIntentMarker(t *testing.T) {
	t.Parallel()
	pipelineMD := "## Pipeline\n\n" + noMistakesPRSignature + "\n\n" + pipelineAttestationCommentPrefix + `{"head_sha":"abc"}` + pipelineAttestationCommentClosingToken
	for _, provider := range []scm.Provider{scm.ProviderAzureDevOps, scm.ProviderGitHub} {
		limit := scm.MaxPRBodyChars(provider)
		size := 2 * max(limit, maxPullRequestBodyBytes)
		plain := strings.TrimSuffix(strings.Repeat("A long original goal line.\n", size/len("A long original goal line.\n")), "\n")
		// The fenced example crosses wherever the cut lands.
		fenced := "Goal:\n\n```go\n" + strings.Repeat("x := 1\n", size/len("x := 1\n")) + "```\n\nAfter."
		for _, intent := range []struct{ name, text string }{{"plain", plain}, {"fence crossing the cut", fenced}} {
			for _, mode := range []string{config.PRAppendixFull, config.PRAppendixCollapsed, config.PRAppendixMinimal} {
				t.Run(string(provider)+"/"+intent.name+"/"+mode, func(t *testing.T) {
					t.Parallel()
					cfg := &config.Config{}
					cfg.PR.Appendix = mode
					first, err := assembleDraftPRBody(&pipeline.StepContext{Config: cfg, UserIntent: intent.text}, "## What Changed\n\n- A helper.", "✅ Low: small", "", pipelineMD, "", limit, provider)
					if err != nil {
						t.Fatal(err)
					}
					kept, err := liveIntentSection(first)
					if err != nil || kept == "" || len(kept) >= len(intent.text) || len(first) > maxPullRequestBodyBytes || (limit > 0 && scm.PRBodyLen(first) > limit) {
						t.Fatalf("first publish Intent = %d chars, %v; want a shortened, marked Intent within the limit\nbody tail:\n%s", len(kept), err, first[max(0, len(first)-600):])
					}

					again, err := assembleDraftPRBody(&pipeline.StepContext{Config: cfg, UserIntent: reraiseFollowUpIntent, PublishedPRIntent: kept}, "## What Changed\n\n- A helper.", "✅ Low: small", "", pipelineMD, "", limit, provider)
					if err != nil {
						t.Fatalf("re-raise of the first publish failed: %v", err)
					}
					if got, err := liveIntentSection(again); err != nil || got != kept {
						t.Fatalf("re-raise Intent differs from the first publish (%v)", err)
					}
				})
			}
		}
	}
}

// A bare end-marker line or an unclosed fence in the Intent text cannot hide
// or fake the marker: the text after either survives a re-raise, and
// republishing what was read back changes nothing.
func TestIntentSection_MarkerStaysTheBoundary(t *testing.T) {
	t.Parallel()
	for _, text := range []string{
		"Goal.\n\n" + prIntentEndMarker + "\n\nAfter the marker line.",
		"Goal:\n\n```sh\nmake e2e\nAfter the open fence.",
	} {
		body := intentSection(text) + "\n\n## What Changed\n\n- A helper.\n"
		got, err := liveIntentSection(body)
		if err != nil || !strings.HasPrefix(got, "Goal") || !strings.Contains(got, "After the") || strings.Contains(got, "A helper.") {
			t.Fatalf("Intent read back = %q, %v\nbody:\n%s", got, err, body)
		}
		if again := intentSection(got) + "\n\n## What Changed\n\n- A helper.\n"; again != body {
			t.Fatalf("republishing the kept Intent changed it:\nfirst:\n%s\nagain:\n%s", body, again)
		}
	}
}

// A host that cannot read the published body has no Intent to keep, so the
// re-raise publishes this run's and says so.
func TestPRStep_ReRaiseOnAHostThatCannotReadSaysWhy(t *testing.T) {
	t.Parallel()
	sctx, bodyFile, _ := reraiseContext(t, "https://github.com/test/repo/pull/42")
	if err := os.WriteFile(bodyFile, []byte(intentSection(reraiseOriginalIntent)+"\n\n## What Changed\n\n- An older summary.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	host, reason := buildHost(sctx, resolvedProvider(sctx))
	if host == nil {
		t.Fatal(reason)
	}
	if _, ok := any(struct{ scm.Host }{host}).(scm.PRContentReader); ok {
		t.Fatal("wrapped host still reads PR content")
	}
	var logs []string
	sctx.Log = func(line string) { logs = append(logs, line) }
	sctx.UserIntent = reraiseFollowUpIntent
	if _, err := (&PRStep{host: struct{ scm.Host }{host}}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(logs, "intent replaced: this forge cannot read the published body") {
		t.Fatalf("replacement was not stated in the step log: %q", logs)
	}
	if intent, err := liveIntentSection(string(mustRead(t, bodyFile))); err != nil || intent != reraiseFollowUpIntent {
		t.Fatalf("Intent = %q, %v; want this run's intent", intent, err)
	}
}

// The Intent the PR was opened with may carry its own "## " sections, even ones
// spelled exactly like the generator's; only the generator's end marker ends it.
const reraiseSectionedIntent = "Ship the original goal.\n\n## Constraints\n\n- must not grow the API\n\n## Testing\n\n- run make e2e\n\n## What Changed\n\n- the author's own plan\n\n## Out of scope\n\nRefs the first report."

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
			if intent, err := publishedPRIntent(first); err != nil || intent != reraiseSectionedIntent {
				t.Fatalf("first raise Intent = %q, %v; want the task's intent\nbody:\n%s", intent, err, first)
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
			if intent, err := publishedPRIntent(after); err != nil || intent != reraiseSectionedIntent {
				t.Fatalf("re-raise Intent = %q, %v; want the original byte for byte\nbody:\n%s", intent, err, after)
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

// A drafted body without its "## What Changed" heading follows the Intent's end
// marker, and the marker alone keeps a later re-raise from taking the stale
// summary in as part of the Intent.
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
		if got, err := liveIntentSection(body); err != nil || got != reraiseOriginalIntent {
			t.Fatalf("Intent = %q, %v; want %q\nbody:\n%s", got, err, reraiseOriginalIntent, body)
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
	seed := intentSection(huge) + "\n\n## What Changed\n\n- An older summary.\n"
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

// Under pr.intent.format: brief, a re-raise that republishes the live Intent
// needs no new brief: no brief turn runs, and a draft without one is accepted.
func TestPRStep_ReRaiseKeepingTheIntentDraftsNoBrief(t *testing.T) {
	t.Parallel()
	const prURL = "https://github.com/test/repo/pull/42"
	for _, tc := range []struct {
		name string
		keep bool
	}{{name: "regenerate"}, {name: "update_text keep", keep: true}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sctx, bodyFile, _ := reraiseContext(t, prURL)
			if tc.keep {
				sctx.Config.PR.UpdateText = config.PRUpdateTextKeep
				if err := os.WriteFile(bodyFile, []byte(keptAuthorBody), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			sctx.UserIntent = reraiseOriginalIntent
			if _, err := (&PRStep{}).Execute(sctx); err != nil {
				t.Fatal(err)
			}
			if intent, err := publishedPRIntent(string(mustRead(t, bodyFile))); err != nil || intent != reraiseOriginalIntent {
				t.Fatalf("first raise Intent = %q, %v", intent, err)
			}

			sctx.Config.PR.Intent.Format = "brief"
			sctx.UserIntent = reraiseFollowUpIntent
			ag := sctx.Agent.(*mockAgent)
			ag.runFn = func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
				if hasPRIntentBriefSchema(t, opts.JSONSchema) {
					t.Fatalf("re-raise keeping the live Intent asked for a brief: %s", opts.Prompt)
				}
				return &agent.Result{Output: json.RawMessage(`{"title":"` + reraiseTitle + `","body":"## What Changed\n\n- A helper."}`)}, nil
			}
			if _, err := (&PRStep{}).Execute(sctx); err != nil {
				t.Fatal(err)
			}
			if intent, err := publishedPRIntent(string(mustRead(t, bodyFile))); err != nil || intent != reraiseOriginalIntent {
				t.Fatalf("re-raise Intent = %q, %v; want the original", intent, err)
			}
		})
	}
}
