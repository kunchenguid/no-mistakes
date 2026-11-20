package steps

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
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
		{name: "stops at a top-level title", body: "## Intent\n\nGoal.\n\n# Notes\n", want: "Goal.", ok: true},
		{name: "keeps subheadings and fenced headings", body: "## Intent\n\nGoal.\n\n### Detail\n\n```\n## inside\n```\n\n## Pipeline\n", want: "Goal.\n\n### Detail\n\n```\n## inside\n```", ok: true},
		{name: "last section", body: "## Intent\n\nGoal.\n", want: "Goal.", ok: true},
	} {
		got, ok := liveIntentSection(tc.body)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: liveIntentSection = %q, %v; want %q, %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}
