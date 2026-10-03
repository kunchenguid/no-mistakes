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
// survives the next ordinary update instead of being stripped.
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

	// It keeps surviving later updates too: carried lines are not recorded as
	// the pipeline's own.
	again := runs.update()
	if got := closingLinesOf(again); got != "Closes owner/repo#7" {
		t.Fatalf("after a second update closing lines = %q:\n%s", got, again)
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

// A body an older no-mistakes published (signature/attestation, no ledger)
// may carry live closing lines in its generated sections. They are not the
// author's, so nothing is carried, exactly as before.
func TestPRStep_LegacyPipelineBodyCarriesNothing(t *testing.T) {
	t.Parallel()
	runs := newPRStepRuns(t, nil)
	runs.create()
	// What an older no-mistakes published: intent verbatim, no ledger.
	legacy := "## Intent\n\nRefactor X\nFixes #12\n\n## What Changed\n\n- refactor\n\n## Pipeline\n\n" + noMistakesPRSignature + "\n"
	runs.authorEdits(legacy)

	updated := runs.update()
	if got := closingLinesOf(updated); got != "" {
		t.Fatalf("closing lines = %q, want none carried from a legacy pipeline body:\n%s", got, updated)
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

// Copies of the ledger in pipeline-generated text (a quoted PR body in Test
// evidence) are escaped, and an ambiguous ledger carries nothing.
func TestClosingLedgerIsOnlyTheOneThePipelineAppends(t *testing.T) {
	quoted := neutralizeAttestationMarkers("evidence:\n" + renderClosingLedger([]string{"Closes #1"}))
	if closingLedgerMarkerPattern.MatchString(quoted) {
		t.Fatalf("generated text must not carry a parseable ledger: %q", quoted)
	}
	body := "Fixes #3\n\n" + renderClosingLedger(nil) + "\n" + renderClosingLedger([]string{"Fixes #3"})
	if _, ok := parseClosingLedger(body); ok {
		t.Fatal("two ledgers must not parse")
	}
	if got := authorClosingLines(body); got != nil {
		t.Fatalf("ambiguous ledger carried %q", got)
	}
}

// Bitbucket Cloud escapes raw HTML, so it gets no ledger and carries nothing.
func TestOrdinaryIssuesBlockHasNoLedgerOnBitbucket(t *testing.T) {
	sctx := &pipeline.StepContext{ClosingIssueRefs: []string{"5"}}
	if got := ordinaryIssuesBlock(sctx, scm.ProviderBitbucket); got != "## Issues\n\nCloses #5" {
		t.Fatalf("bitbucket block = %q", got)
	}
	if carriesClosingLines(scm.ProviderBitbucket) {
		t.Fatal("bitbucket must not carry closing lines")
	}
}

// A closing line inside an HTML <pre> element is not live (GitHub closes
// nothing there, and neutralization leaves it alone), so it is never carried
// into the Issues section as the author's, while a real author line is.
func TestPRStep_ClosingLineInHTMLPreIsNotCarried(t *testing.T) {
	t.Parallel()
	runs := newPRStepRuns(t, nil)
	created := runs.create()
	runs.authorEdits(created + "\n\n<pre>\nFixes #12\n</pre>\n\nCloses #4\n")

	updated := runs.update()
	if got := closingLinesOf(updated); got != "Closes #4" {
		t.Fatalf("closing lines = %q, want only the author's live line carried:\n%s", got, updated)
	}
}

// A body the pipeline never published carries its live closing lines, but
// not ones inside a multi-line HTML comment or <code> element.
func TestAuthorClosingLinesSkipHTMLCommentsAndCode(t *testing.T) {
	body := "Summary\n\n<!--\nTemplate example:\nCloses #3\n-->\n\n<code>\nResolves #5\n</code>\n\nCloses #4\n"
	got := authorClosingLines(body)
	if strings.Join(got, "|") != "Closes #4" {
		t.Fatalf("author closing lines = %q, want only the live line", got)
	}
}

// Inline code never opens HTML element or comment state: prose naming
// `<pre>`, `<code>` and `<!--` leaves later author lines and the Issues
// section live, so the author's line is carried and --closes still verifies.
func TestPRStep_BacktickedHTMLDoesNotHideLaterClosingLines(t *testing.T) {
	t.Parallel()
	runs := newPRStepRuns(t, nil)
	created := runs.create()
	runs.authorEdits("Skips HTML `<pre>`/`<code>` elements and `<!--` comments.\n\nCloses #4\n\n" + created)

	updated := runs.update("9")
	if got := closingLinesOf(updated); got != "Closes #4|Closes #9" {
		t.Fatalf("closing lines = %q, want the author's line and the requested one:\n%s", got, updated)
	}
}

func TestClosingKeywordLinesAfterBacktickedHTMLStayLive(t *testing.T) {
	body := "Uses `<pre>`, `<code>` and `<!--` here.\n\n## Issues\n\nCloses #9\n"
	if got := closingLinesOf(body); got != "Closes #9" {
		t.Fatalf("closing lines = %q, want the Issues line live", got)
	}
	if got := neutralizeClosingReferences("Uses `<pre>` here.\nFixes #3"); got != "Uses `<pre>` here.\nFixes `#3`" {
		t.Fatalf("neutralized = %q", got)
	}
}
