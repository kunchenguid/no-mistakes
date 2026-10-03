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

// Bitbucket Cloud escapes raw HTML and Azure DevOps caps the body size, so
// neither gets a ledger and neither carries anything.
func TestNoLedgerOnBitbucketOrAzureDevOps(t *testing.T) {
	sctx := &pipeline.StepContext{ClosingIssueRefs: []string{"5"}}
	body := "## What Changed\n\n- x\n\n" + ordinaryIssuesBlock(sctx)
	for _, provider := range []scm.Provider{scm.ProviderBitbucket, scm.ProviderAzureDevOps} {
		if carriesClosingLines(provider) {
			t.Fatalf("%s must not carry closing lines", provider)
		}
		if got, err := sealClosingLedger(sctx, provider, body); err != nil || got != body {
			t.Fatalf("%s sealed body = %q, %v; want it unchanged", provider, got, err)
		}
	}
}

// A body too large for the ledger fails closed instead of publishing without it.
func TestSealClosingLedgerFailsClosedOverTheSizeCap(t *testing.T) {
	sctx := &pipeline.StepContext{}
	if _, err := sealClosingLedger(sctx, scm.ProviderGitHub, strings.Repeat("x", maxPullRequestBodyBytes)); err == nil {
		t.Fatal("a body without room for the ledger must fail")
	}
}

// Pipeline text the extractor reads as a closing line, whatever construct it
// sits in, is recorded in the ledger when published, so a later update never
// carries it as the author's.
func TestPRStep_PipelineClosingLinesAreLedgeredNotCarried(t *testing.T) {
	for name, intent := range map[string]string{
		"pre element":                 "Refactor X\n<pre>\nFixes #12\n</pre>",
		"unterminated inline comment": "Drops stray <!-- markers\nFixes #12",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runs := newPRStepRuns(t, nil)
			runs.sctx.UserIntent = intent
			created := runs.create()
			if lines, ok := parseClosingLedger(created); !ok || len(lines) != len(extractClosingKeywordLines(created)) {
				t.Fatalf("ledger = %q, %v; want every published closing line:\n%s", lines, ok, created)
			}

			updated := runs.update()
			if strings.Contains(updated, "## Issues") {
				t.Fatalf("an update must carry nothing from pipeline text:\n%s", updated)
			}
			if got := authorClosingLines(updated); got != nil {
				t.Fatalf("author closing lines = %q, want none", got)
			}
		})
	}
}

// Prose naming HTML tags in inline code does not hide a later author line:
// it is carried, and a requested reference still verifies.
func TestPRStep_BacktickedHTMLDoesNotHideAuthorClosingLines(t *testing.T) {
	t.Parallel()
	runs := newPRStepRuns(t, nil)
	created := runs.create()
	runs.authorEdits("Skips HTML `<pre>`/`<code>` elements and `<!--` comments.\n\nCloses #4\n\n" + created)

	updated := runs.update("9")
	if got := strings.Join(authorClosingLines(updated), "|"); got != "Closes #4" {
		t.Fatalf("author closing lines = %q, want the carried line:\n%s", got, updated)
	}
	if !strings.Contains(updated, "## Issues\n\nCloses #4\nCloses #9") {
		t.Fatalf("Issues section must carry the author's line and add the requested one:\n%s", updated)
	}
}

// GitHub closes nothing from a line inside an HTML comment or a <pre>/<code>
// element, so the carry never republishes one as a live line.
func TestAuthorClosingLinesSkipCommentsAndHTMLCode(t *testing.T) {
	for name, tc := range map[string]struct{ body, want string }{
		"PR template comment": {"<!--\nFixes #123\n-->\n\nSummary of the change.", ""},
		"pre element":         {"Summary\n\n<pre>\nFixes #9\n</pre>\n\nCloses #4", "Closes #4"},
		"code element":        {"<code>\nResolves #8\n</code>\nCloses #4", "Closes #4"},
		"backticked tags":     {"Skips `<pre>` and `<!--` markers.\n\nCloses #4", "Closes #4"},
	} {
		if got := strings.Join(authorClosingLines(tc.body), "|"); got != tc.want {
			t.Errorf("%s: author closing lines = %q, want %q", name, got, tc.want)
		}
	}
}

// The carry's reader only ever drops lines from the extractor's result.
func TestCarryableClosingLinesAreASubsetOfExtracted(t *testing.T) {
	for _, body := range []string{
		"Closes #1\n<!--\nFixes #2\n-->\nFixes #3",
		"<pre>\nCloses #4\n</pre>\n`<pre>`\nCloses #5",
		"Drops stray <!-- markers\nFixes #6",
		"```\nFixes #7\n```\n    Fixes #8\n- Resolves owner/repo#9",
		"<code>Fixes #10</code>\nCloses #11.",
	} {
		extracted := map[string]bool{}
		for _, line := range extractClosingKeywordLines(body) {
			extracted[line] = true
		}
		for _, line := range carryableClosingLines(body) {
			if !extracted[line] {
				t.Errorf("carryableClosingLines(%q) promoted %q", body, line)
			}
		}
	}
}

// An author's commented-out closing line in a ledgered body is not carried;
// a live one is.
func TestPRStep_CommentedOutAuthorClosingLineIsNotCarried(t *testing.T) {
	t.Parallel()
	runs := newPRStepRuns(t, nil)
	created := runs.create()
	runs.authorEdits(created + "\n\n<!--\nCloses #5\n-->\n\nCloses #4\n")

	updated := runs.update()
	if got := closingLinesOf(updated); got != "Closes #4" {
		t.Fatalf("closing lines = %q, want only the live author line carried:\n%s", got, updated)
	}
}
