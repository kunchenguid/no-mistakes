package steps

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// prStepRuns drives the PR step twice against one fake GitHub PR body: a
// create, an optional edit of the live body standing in for the author, and
// an ordinary update. It returns the body after each run.
type prStepRuns struct {
	t        *testing.T
	sctx     *pipeline.StepContext
	bodyFile string
}

func newPRStepRuns(t *testing.T, ag *mockAgent) *prStepRuns {
	t.Helper()
	dir, baseSHA, headSHA := setupGitRepo(t)
	if ag == nil {
		ag = &mockAgent{name: "test"}
	}
	env, _ := fakeGH(t, "")
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	return &prStepRuns{t: t, sctx: sctx, bodyFile: envEntry(env, "FAKE_CLI_PR_BODY_FILE")}
}

func (r *prStepRuns) create(refs ...string) string {
	r.t.Helper()
	r.setRefs(refs)
	if _, err := (&PRStep{}).Execute(r.sctx); err != nil {
		r.t.Fatalf("create: %v", err)
	}
	return readPRBodyFile(r.t, r.bodyFile)
}

func (r *prStepRuns) update(refs ...string) string {
	r.t.Helper()
	env, _ := fakeGH(r.t, "https://github.com/test/repo/pull/99")
	r.sctx.Env = append(env, "FAKE_CLI_PR_BODY_FILE="+r.bodyFile)
	// A later run is a new run: nothing it inherits from the PR step's own
	// in-memory state.
	r.sctx.ClosingIssueRefs = nil
	r.sctx.CarriedClosingLines = nil
	r.setRefs(refs)
	if _, err := (&PRStep{}).Execute(r.sctx); err != nil {
		r.t.Fatalf("update: %v", err)
	}
	return readPRBodyFile(r.t, r.bodyFile)
}

func (r *prStepRuns) setRefs(refs []string) {
	r.t.Helper()
	run, err := r.sctx.DB.InsertRun(r.sctx.Repo.ID, "feature", r.sctx.Run.HeadSHA, r.sctx.Run.BaseSHA)
	if err != nil {
		r.t.Fatal(err)
	}
	if len(refs) > 0 {
		if err := r.sctx.DB.UpdateRunClosingIssueRefs(run.ID, refs); err != nil {
			r.t.Fatal(err)
		}
	}
	prURL := r.sctx.Run.PRURL
	r.sctx.Run = run
	r.sctx.Run.PRURL = prURL
}

func (r *prStepRuns) authorEdits(body string) {
	r.t.Helper()
	if err := os.WriteFile(r.bodyFile, []byte(body), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func closingLinesOf(body string) string {
	return strings.Join(extractClosingKeywordLines(body), "|")
}

// Issue #763: an author's closing line added to a body the pipeline published
// survives the next ordinary update instead of being stripped, and keeps
// surviving later ones because carried lines are not recorded as the
// pipeline's own.
func TestPRStep_KeepsAuthorClosingLineAddedToPipelineBody(t *testing.T) {
	t.Parallel()
	runs := newPRStepRuns(t, nil)
	created := runs.create()
	if _, ok := parseClosingLedger(created); !ok {
		t.Fatalf("a published ordinary body must carry the closing ledger:\n%s", created)
	}
	runs.authorEdits(created + "\n\nCloses owner/repo#7\n")

	updated := runs.update()
	if got := closingLinesOf(updated); got != "Closes owner/repo#7" {
		t.Fatalf("closing lines = %q, want the author's line kept exactly once:\n%s", got, updated)
	}
	if !strings.Contains(updated, "## Issues\n\nCloses owner/repo#7") {
		t.Fatalf("the author's line must be carried into the Issues section:\n%s", updated)
	}

	again := runs.update()
	if got := closingLinesOf(again); got != "Closes owner/repo#7" {
		t.Fatalf("after a second update closing lines = %q:\n%s", got, again)
	}
}

// An author's closing line naming the issue by URL, in this repository or
// another, is carried verbatim; a commented-out one is not.
func TestPRStep_KeepsAuthorURLClosingLines(t *testing.T) {
	t.Parallel()
	runs := newPRStepRuns(t, nil)
	created := runs.create()
	runs.authorEdits(created + "\n\nCloses https://github.com/test/repo/issues/12\nFixes https://github.com/cli/cli/issues/5\n\n<!--\nCloses https://github.com/test/repo/issues/13\n-->\n")

	updated := runs.update()
	want := "Closes https://github.com/test/repo/issues/12|Fixes https://github.com/cli/cli/issues/5"
	if got := closingLinesOf(updated); got != want {
		t.Fatalf("closing lines = %q, want %q:\n%s", got, want, updated)
	}
	if again := runs.update(); closingLinesOf(again) != want {
		t.Fatalf("after a second update closing lines = %q:\n%s", closingLinesOf(again), again)
	}
}

// The ledger keeps the pipeline's own --closes lines apart from the author's:
// a later run without --closes drops them, as before, while the author's
// line stays.
func TestPRStep_DropsPipelineClosesLinesButKeepsAuthorLines(t *testing.T) {
	t.Parallel()
	runs := newPRStepRuns(t, nil)
	created := runs.create("95")
	if got := closingLinesOf(created); got != "Closes #95" {
		t.Fatalf("created closing lines = %q", got)
	}
	runs.authorEdits(created + "\n\nFixes #42\n")

	updated := runs.update()
	if got := closingLinesOf(updated); got != "Fixes #42" {
		t.Fatalf("closing lines = %q, want only the author's line after a run without --closes:\n%s", got, updated)
	}
}

// The re-read just before the write is authoritative both ways: a line the
// author removed while the update was drafting is not written back, and one
// they added is carried.
func TestPRStep_CarriedLinesFollowAuthorEditsDuringDrafting(t *testing.T) {
	t.Parallel()
	var runs *prStepRuns
	drafting := false
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		if drafting {
			live := readPRBodyFile(runs.t, runs.bodyFile)
			live = strings.Replace(live, "Fixes #42\n", "Closes #7\n", 1)
			if err := os.WriteFile(runs.bodyFile, []byte(live), 0o644); err != nil {
				return nil, err
			}
		}
		return &agent.Result{}, nil
	}}
	runs = newPRStepRuns(t, ag)
	created := runs.create()
	runs.authorEdits(created + "\n\nFixes #42\n")

	drafting = true
	updated := runs.update()
	if got := closingLinesOf(updated); got != "Closes #7" {
		t.Fatalf("closing lines = %q, want the removed line gone and the added one carried:\n%s", got, updated)
	}
}

// fakeGitHubRenderer returns a step context and the GitHub host the PR step
// would build for it, rendering through the fake gh (internal/fakegfm).
func fakeGitHubRenderer(t *testing.T, env ...string) (*pipeline.StepContext, scm.Host) {
	t.Helper()
	sctx := newTestContext(t, &mockAgent{name: "test"}, t.TempDir(), "", "", config.Commands{})
	fake, _ := fakeGH(t, "")
	sctx.Env = append(fake, env...)
	host, _ := buildHost(sctx, scm.ProviderGitHub)
	return sctx, host
}

// Which live closing references are the author's is decided by GitHub's
// renderer on the live body, counted per target against the ledger.
func TestCarriedClosingLines(t *testing.T) {
	t.Parallel()
	sctx, host := fakeGitHubRenderer(t)
	ledger := func(targets ...string) string { return "\n\n" + renderClosingLedger(targets) }
	for name, tc := range map[string]struct {
		body string
		want string
	}{
		// Greptile: a commented-out copy does not use up the pipeline's
		// ledger entry, so a run without --closes drops #5.
		"commented-out copy of a pipeline line": {
			body: "<!--\nCloses #5\n-->\n\n## Issues\n\nCloses #5" + ledger("5"),
		},
		"author copy of a pipeline line": {
			body: "Closes #5\n\n## Issues\n\nCloses #5" + ledger("5"),
			want: "Closes #5",
		},
		"stray inline comment marker": {
			body: "Drops stray <!-- markers\n\nCloses #4" + ledger(),
			want: "Closes #4",
		},
		"comment, pre, and code author lines": {
			body: "<!--\nCloses #5\n-->\n\n<pre>\nFixes #9\n</pre>\n\n<code>Fixes #2</code>\n\n`Fixes #3`" + ledger(),
		},
		// A keyword and a reference in separate rendered blocks never pair.
		"keyword and reference in separate table cells": {
			body: "| kind | ref |\n| --- | --- |\n| fix | #5 |" + ledger(),
		},
		"keyword before a list of references": {
			body: "Fixes:\n- #5" + ledger(),
		},
		"closing line in its own paragraph": {
			body: "Summary.\n\nCloses #4" + ledger(),
			want: "Closes #4",
		},
		"URL closing lines in this and another repository": {
			body: "Closes https://github.com/test/repo/issues/12\nFixes https://github.com/cli/cli/pull/5\n\n## Issues\n\nCloses #7" + ledger("7"),
			want: "Closes https://github.com/test/repo/issues/12|Fixes https://github.com/cli/cli/pull/5",
		},
		"commented-out URL closing line": {
			body: "<!--\nCloses https://github.com/test/repo/issues/12\n-->" + ledger(),
		},
		"URL closing line on another host": {
			body: "Closes https://gitlab.com/test/repo/issues/12" + ledger(),
		},
		"reference in prose": {
			body: "This also fixes #8 for good." + ledger(),
			want: "Closes #8",
		},
		"hand-opened template comment": {
			body: "<!--\nFixes #123\n-->\n\nSummary.\n\nResolves test/repo#44\n",
			want: "Resolves test/repo#44",
		},
		"legacy pipeline body without a ledger": {
			body: "## Intent\n\nFixes #12\n\n## Pipeline\n\n" + noMistakesPRSignature + "\n",
		},
		"ambiguous ledger": {
			body: "Fixes #3" + ledger() + ledger("3"),
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := carriedClosingLines(context.Background(), sctx, host, tc.body)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(got, "|") != tc.want {
				t.Fatalf("carried = %q, want %q", got, tc.want)
			}
		})
	}
}

// The ledger records exactly what renders live, minus the carried author
// lines, so a pipeline reference that escaped neutralization is dropped by
// the next run while the author's line is carried again.
func TestSealClosingLedgerRecordsWhatRendersLive(t *testing.T) {
	t.Parallel()
	sctx, host := fakeGitHubRenderer(t)
	sctx.CarriedClosingLines = []string{"Closes owner/repo#7"}
	body := "## Intent\n\n<pre>\nFixes #12\n</pre>\nFixes #3\n\n## Issues\n\nCloses owner/repo#7\nCloses #5"

	sealed, err := sealClosingLedger(context.Background(), sctx, host, scm.ProviderGitHub, body)
	if err != nil {
		t.Fatal(err)
	}
	if targets, ok := parseClosingLedger(sealed); !ok || strings.Join(targets, "|") != "3|5" {
		t.Fatalf("ledger = %q, %v; want the pipeline's live #3 and #5 only", targets, ok)
	}
	carried, err := carriedClosingLines(context.Background(), sctx, host, sealed)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(carried, "|") != "Closes owner/repo#7" {
		t.Fatalf("carried = %q, want only the author's line", carried)
	}
}

// The ledger pairs a keyword and a reference only within one rendered block.
func TestSealClosingLedgerIgnoresReferencesAcrossBlocks(t *testing.T) {
	t.Parallel()
	sctx, host := fakeGitHubRenderer(t)
	body := "| kind | ref |\n| --- | --- |\n| fix | #5 |\n\nFixes:\n- #6\n\nCloses #4"

	sealed, err := sealClosingLedger(context.Background(), sctx, host, scm.ProviderGitHub, body)
	if err != nil {
		t.Fatal(err)
	}
	if targets, ok := parseClosingLedger(sealed); !ok || strings.Join(targets, "|") != "4" {
		t.Fatalf("ledger = %q, %v; want only the paragraph's #4", targets, ok)
	}
}

// A failed render fails closed on both sides rather than guessing.
func TestClosingLinesFailClosedWhenRenderingFails(t *testing.T) {
	t.Parallel()
	sctx, host := fakeGitHubRenderer(t, "FAKE_CLI_MARKDOWN_ERR=HTTP 502")
	if _, err := carriedClosingLines(context.Background(), sctx, host, "Closes #4\n"); err == nil || !strings.Contains(err.Error(), "render the pull request body") {
		t.Fatalf("carry error = %v, want a render failure", err)
	}
	if _, err := sealClosingLedger(context.Background(), sctx, host, scm.ProviderGitHub, "## Issues\n\nCloses #4"); err == nil {
		t.Fatal("sealing without a render must fail")
	}
}

// Copies of the ledger in pipeline-generated text (a quoted PR body in Test
// evidence) are escaped, so only the ledger the pipeline appends parses.
func TestClosingLedgerIsOnlyTheOneThePipelineAppends(t *testing.T) {
	quoted := neutralizeAttestationMarkers("evidence:\n" + renderClosingLedger([]string{"1"}))
	if closingLedgerMarkerPattern.MatchString(quoted) {
		t.Fatalf("generated text must not carry a parseable ledger: %q", quoted)
	}
}

// Only GitHub's renderer decides what is live, so no other forge gets a
// ledger or carries anything.
func TestNoLedgerOffGitHub(t *testing.T) {
	sctx := &pipeline.StepContext{ClosingIssueRefs: []string{"5"}}
	body := "## What Changed\n\n- x\n\n" + ordinaryIssuesBlock(sctx)
	for _, provider := range []scm.Provider{scm.ProviderGitLab, scm.ProviderGitea, scm.ProviderForgejo, scm.ProviderBitbucket, scm.ProviderAzureDevOps} {
		if carriesClosingLines(provider) {
			t.Fatalf("%s must not carry closing lines", provider)
		}
		if got, err := sealClosingLedger(context.Background(), sctx, nil, provider, body); err != nil || got != body {
			t.Fatalf("%s sealed body = %q, %v; want it unchanged", provider, got, err)
		}
	}
}

// A body too large for the ledger fails closed instead of publishing without it.
func TestSealClosingLedgerFailsClosedOverTheSizeCap(t *testing.T) {
	sctx := &pipeline.StepContext{}
	if _, err := sealClosingLedger(context.Background(), sctx, nil, scm.ProviderGitHub, strings.Repeat("x", maxPullRequestBodyBytes)); err == nil {
		t.Fatal("a body without room for the ledger must fail")
	}
}
