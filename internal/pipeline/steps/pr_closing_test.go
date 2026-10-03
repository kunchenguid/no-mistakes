package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// Without --closes the body never gains a closing reference, even though
// the intent names an issue.
func TestPRStep_NeverInfersClosingReferenceFromIntent(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	env, _ := fakeGH(t, "")
	bodyFile := envEntry(env, "FAKE_CLI_PR_BODY_FILE")
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.UserIntent = "Implement issue #95"

	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	body := readPRBodyFile(t, bodyFile)
	if strings.Contains(body, "## Issues") || len(extractClosingKeywordLines(body)) != 0 {
		t.Fatalf("closing reference inferred without --closes:\n%s", body)
	}
}

// The pipeline publishes a closing reference in the intent neutralized, so
// without --closes the PR closes nothing.
func TestPRStep_NeutralizesClosingReferenceInPublishedIntent(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	env, _ := fakeGH(t, "")
	bodyFile := envEntry(env, "FAKE_CLI_PR_BODY_FILE")
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.UserIntent = "Refactor X\nFixes #12"

	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	body := readPRBodyFile(t, bodyFile)
	if !strings.Contains(body, "Fixes `#12`") || strings.Contains(body, "## Issues") || len(extractClosingKeywordLines(body)) != 0 {
		t.Fatalf("intent's Fixes #12 must be published neutralized:\n%s", body)
	}
}

// A closing reference in the agent-drafted narrative is published
// neutralized: closure comes only from --closes.
func TestPRStep_NeutralizesClosingReferenceInDraftedNarrative(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	env, _ := fakeGH(t, "")
	bodyFile := envEntry(env, "FAKE_CLI_PR_BODY_FILE")
	ag := &mockAgent{name: "test", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		payload := json.RawMessage(`{"title":"add widget","body":"## What Changed\n\n- add widget\n\nCloses #7"}`)
		return &agent.Result{Output: payload}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env

	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	body := readPRBodyFile(t, bodyFile)
	if !strings.Contains(body, "Closes `#7`") || strings.Contains(body, "## Issues") || len(extractClosingKeywordLines(body)) != 0 {
		t.Fatalf("drafted closing reference not neutralized:\n%s", body)
	}
}

// With --closes, only the Issues section closes: a drafted reference to the
// same issue is neutralized and the requested one renders and verifies once.
func TestPRStep_ClosesRendersOnlyInIssuesSection(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	env, _ := fakeGH(t, "")
	bodyFile := envEntry(env, "FAKE_CLI_PR_BODY_FILE")
	ag := &mockAgent{name: "test", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		payload := json.RawMessage(`{"title":"add widget","body":"## What Changed\n\n- add widget\n\nCloses #95"}`)
		return &agent.Result{Output: payload}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	if err := sctx.DB.UpdateRunClosingIssueRefs(sctx.Run.ID, []string{"95"}); err != nil {
		t.Fatal(err)
	}

	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	body := readPRBodyFile(t, bodyFile)
	if got := strings.Join(extractClosingKeywordLines(body), "|"); got != "Closes #95" || !strings.Contains(body, "## Issues\n\nCloses #95") {
		t.Fatalf("closing lines = %q, want only the Issues section's Closes #95:\n%s", got, body)
	}
}

func TestNeutralizeClosingReferences(t *testing.T) {
	t.Parallel()
	in := "Fixes #12 and resolved: owner/repo#3\n- closes #4.\nsee #5, fixes `#6`, `Fixes #7`\n```\nCloses #8\n```\n    Fixes #9"
	want := "Fixes `#12` and resolved: `owner/repo#3`\n- closes `#4`.\nsee #5, fixes `#6`, `Fixes #7`\n```\nCloses #8\n```\n    Fixes #9"
	got := neutralizeClosingReferences(in)
	if got != want {
		t.Fatalf("neutralizeClosingReferences = %q, want %q", got, want)
	}
	if again := neutralizeClosingReferences(got); again != got {
		t.Fatalf("neutralizeClosingReferences is not idempotent: %q", again)
	}
}

func TestNeutralizeClosingReferencesURLForm(t *testing.T) {
	t.Parallel()
	in := "Fixes https://github.com/o/r/issues/12\nresolves: http://ghe.example.com:8080/o/r/pull/3.\nsee https://github.com/o/r/issues/5, `Closes https://github.com/o/r/issues/6`"
	want := "Fixes `https://github.com/o/r/issues/12`\nresolves: `http://ghe.example.com:8080/o/r/pull/3`.\nsee https://github.com/o/r/issues/5, `Closes https://github.com/o/r/issues/6`"
	got := neutralizeClosingReferences(in)
	if got != want {
		t.Fatalf("neutralizeClosingReferences = %q, want %q", got, want)
	}
	if again := neutralizeClosingReferences(got); again != got {
		t.Fatalf("neutralizeClosingReferences is not idempotent: %q", again)
	}
}

// A tested command containing backticks renders as an HTML <code> element; it
// must be published exactly as recorded, while prose around it is neutralized.
func TestNeutralizeClosingReferencesLeavesHTMLCodeUnchanged(t *testing.T) {
	t.Parallel()
	rendered := renderTestedDetailFor("git commit -m \"`Fixes #12`\" && echo Fixes #12", prBodyHTML)
	if !strings.HasPrefix(rendered, "<code>") {
		t.Fatalf("tested command did not render as HTML code: %q", rendered)
	}
	in := "Fixes #3 via " + rendered + " then fixes #4\n<pre>\nCloses #5\n</pre>\nresolves #6"
	want := "Fixes `#3` via " + rendered + " then fixes `#4`\n<pre>\nCloses #5\n</pre>\nresolves `#6`"
	if got := neutralizeAttestationMarkers(in); got != want {
		t.Fatalf("neutralizeAttestationMarkers = %q, want %q", got, want)
	}
}

// pr.template bodies keep author text verbatim, so the requested reference
// lives in the regenerated appendix, and is not repeated once the author's
// own text closes the same issue.
func TestPRTemplate_ClosesRendersOnceAcrossCreateAndAuthorEditedUpdate(t *testing.T) {
	t.Parallel()
	sctx, _, _ := templateTestContext(t)
	if err := sctx.DB.UpdateRunClosingIssueRefs(sctx.Run.ID, []string{"42", "owner/repo#5"}); err != nil {
		t.Fatal(err)
	}
	env, _ := fakeGH(t, "")
	bodyFile := filepath.Join(t.TempDir(), "body.md")
	sctx.Env = append(env, "FAKE_CLI_PR_BODY_FILE="+bodyFile)

	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatalf("create: %v", err)
	}
	created := readPRBodyFile(t, bodyFile)
	parts, err := parsePROwnedBody(created)
	if err != nil {
		t.Fatalf("created body ownership: %v\n%s", err, created)
	}
	for _, line := range []string{"Closes #42", "Closes owner/repo#5"} {
		if strings.Count(created, line) != 1 || !strings.Contains(parts.appendix, line) {
			t.Fatalf("created body should carry %q once in the appendix:\n%s", line, created)
		}
	}

	// The author now closes #42 in their own text; the next refresh keeps it
	// verbatim and stops rendering a second reference to the same issue.
	edited := strings.Replace(created, "# Overview\n", "# Overview\n\nFixes #42\n", 1)
	if err := os.WriteFile(bodyFile, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	env, _ = fakeGH(t, "https://github.com/test/repo/pull/99")
	sctx.Env = append(env, "FAKE_CLI_PR_BODY_FILE="+bodyFile)
	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatalf("update: %v", err)
	}
	updated := readPRBodyFile(t, bodyFile)
	if strings.Count(updated, "Fixes #42") != 1 || strings.Contains(updated, "Closes #42") {
		t.Fatalf("#42 should be closed once, by the author's line:\n%s", updated)
	}
	if strings.Count(updated, "Closes owner/repo#5") != 1 {
		t.Fatalf("requested reference dropped on update:\n%s", updated)
	}
}

// The Issues section is derived from the author text actually being written:
// an author removing their own closing line between reads gets the requested
// reference back in the appendix instead of a body that closes nothing.
func TestPROwnershipUpdateRecomputesIssuesFromLatestAuthorText(t *testing.T) {
	t.Parallel()
	_, appendix := ownedFixture(t)
	content, err := composeOwnedPRContent(prOwnedBody{before: "## Summary\n\nFixes #95"}, "", appendix, 0)
	if err != nil {
		t.Fatal(err)
	}
	host := &ownershipRaceHost{body: content.Body, read: func(h *ownershipRaceHost) error {
		if h.reads == 1 {
			h.body = strings.Replace(h.body, "Fixes #95", "No longer closing here.", 1)
		}
		return nil
	}}
	sctx := &pipeline.StepContext{Ctx: context.Background(), ClosingIssueRefs: []string{"95"}}
	if err := updateOwnedPR(sctx, host, &scm.PR{Number: "42"}, scm.PRContent(content), "", "", appendix, 0); err != nil {
		t.Fatal(err)
	}
	if host.writes != 1 || strings.Contains(host.body, "Fixes #95") || strings.Count(host.body, "Closes #95") != 1 {
		t.Fatalf("written body must close #95 once after the author's edit: writes=%d\n%s", host.writes, host.body)
	}
}

func readPRBodyFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// A run carrying --closes must not skip publication because the PR host is
// unavailable: nothing would close the requested issues. Without --closes the
// step still skips.
func TestPRStep_ClosesFailsInsteadOfSkippingWhenHostUnavailable(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		setup func(*testing.T, *pipeline.StepContext)
	}{
		{"unauthenticated", func(t *testing.T, sctx *pipeline.StepContext) {
			sctx.Env = append(fakeCIGH(t, "OPEN", `[]`), "FAKE_CLI_AUTH_ERR=not logged in")
		}},
		{"no host", func(_ *testing.T, sctx *pipeline.StepContext) {
			sctx.Repo.UpstreamURL = "https://bitbucket.org/test/repo.git"
			sctx.Env = []string{"PATH="}
		}},
		{"base branch", func(_ *testing.T, sctx *pipeline.StepContext) {
			sctx.Run.Branch = effectivePRBaseBranch(sctx)
		}},
	} {
		for _, refs := range [][]string{nil, {"42"}} {
			t.Run(fmt.Sprintf("%s/closes=%v", tc.name, refs), func(t *testing.T) {
				t.Parallel()
				dir, baseSHA, headSHA := setupGitRepo(t)
				sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
				tc.setup(t, sctx)
				if refs != nil {
					if err := sctx.DB.UpdateRunClosingIssueRefs(sctx.Run.ID, refs); err != nil {
						t.Fatal(err)
					}
				}
				outcome, err := (&PRStep{}).Execute(sctx)
				if refs == nil {
					if err != nil || outcome == nil || !outcome.Skipped {
						t.Fatalf("outcome = %+v, err = %v; want skipped", outcome, err)
					}
					return
				}
				if err == nil || !strings.Contains(err.Error(), "--closes requires publishing a pull request") {
					t.Fatalf("outcome = %+v, err = %v; want failure", outcome, err)
				}
			})
		}
	}
}

// Trailing punctuation and ordered-list items are ordinary ways to write a
// standalone closing line; verification must see them.
func TestClosingKeywordLinesAcceptPunctuationAndOrderedLists(t *testing.T) {
	body := "Fixes #4.\n1. Closes #5\n2) Resolves owner/repo#6;\nThis fixes #7 partly.\n"
	got := strings.Join(extractClosingKeywordLines(body), "|")
	if got != "Fixes #4.|1. Closes #5|2) Resolves owner/repo#6;" {
		t.Fatalf("extractClosingKeywordLines() = %q", got)
	}
	sctx := &pipeline.StepContext{ClosingIssueRefs: []string{"4", "5", "owner/repo#6"}}
	if err := verifyClosingIssuesInBody(body, sctx); err != nil {
		t.Fatalf("verifyClosingIssuesInBody() = %v", err)
	}
	if err := verifyClosingIssuesInBody("Fixes #5.\n", sctx); err == nil {
		t.Fatal("verifyClosingIssuesInBody() accepted a body missing requested references")
	}
}

// `95` and `<own repo>#95` name the same issue, so it renders exactly once,
// and an author's qualified line for the PR's own repository covers `95`.
func TestPRStep_OwnRepositoryQualifiedReferenceRendersOnce(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	env, _ := fakeGH(t, "")
	bodyFile := envEntry(env, "FAKE_CLI_PR_BODY_FILE")
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	if err := sctx.DB.UpdateRunClosingIssueRefs(sctx.Run.ID, []string{"95", "Test/Repo#95", "other/repo#95"}); err != nil {
		t.Fatal(err)
	}

	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	lines := extractClosingKeywordLines(readPRBodyFile(t, bodyFile))
	if got := strings.Join(lines, "|"); got != "Closes #95|Closes other/repo#95" {
		t.Fatalf("closing lines = %q, want #95 and other/repo#95 exactly once each", got)
	}

	authored := &pipeline.StepContext{ClosingIssueRefs: []string{"95"}, Repo: sctx.Repo}
	if got := issuesSection(authored, "Closes TEST/repo#95"); got != "" {
		t.Fatalf("issuesSection() = %q, want the author line to cover #95", got)
	}
	if err := verifyClosingIssuesInBody("Closes test/repo#95\n", authored); err != nil {
		t.Fatalf("verifyClosingIssuesInBody() = %v", err)
	}
}
