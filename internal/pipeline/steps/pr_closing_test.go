package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
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
	if strings.Contains(body, "## Issues") || len(extractClosingKeywordLines(body, githubClosingGrammar)) != 0 {
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
	if !strings.Contains(body, "Fixes `#12`") || strings.Contains(body, "## Issues") || len(extractClosingKeywordLines(body, githubClosingGrammar)) != 0 {
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
	if !strings.Contains(body, "Closes `#7`") || strings.Contains(body, "## Issues") || len(extractClosingKeywordLines(body, githubClosingGrammar)) != 0 {
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
	if got := strings.Join(extractClosingKeywordLines(body, githubClosingGrammar), "|"); got != "Closes #95" || !strings.Contains(body, "## Issues\n\nCloses #95") {
		t.Fatalf("closing lines = %q, want only the Issues section's Closes #95:\n%s", got, body)
	}
}

func TestNeutralizeClosingReferences(t *testing.T) {
	t.Parallel()
	in := "Fixes #12 and resolved: owner/repo#3\n- closes #4.\nsee #5, fixes `#6`, `Fixes #7`\n```\nCloses #8\n```\n    Fixes #9"
	want := "Fixes `#12` and resolved: `owner/repo#3`\n- closes `#4`.\nsee #5, fixes `#6`, `Fixes #7`\n```\nCloses #8\n```\n    Fixes #9"
	got := neutralizeGitHubClosingReferences(in)
	if got != want {
		t.Fatalf("neutralizeClosingReferences = %q, want %q", got, want)
	}
	if again := neutralizeGitHubClosingReferences(got); again != got {
		t.Fatalf("neutralizeClosingReferences is not idempotent: %q", again)
	}
}

func TestNeutralizeClosingReferencesURLForm(t *testing.T) {
	t.Parallel()
	in := "Fixes https://github.com/o/r/issues/12\nresolves: http://ghe.example.com:8080/o/r/pull/3.\nsee https://github.com/o/r/issues/5, `Closes https://github.com/o/r/issues/6`"
	want := "Fixes `https://github.com/o/r/issues/12`\nresolves: `http://ghe.example.com:8080/o/r/pull/3`.\nsee https://github.com/o/r/issues/5, `Closes https://github.com/o/r/issues/6`"
	got := neutralizeGitHubClosingReferences(in)
	if got != want {
		t.Fatalf("neutralizeClosingReferences = %q, want %q", got, want)
	}
	if again := neutralizeGitHubClosingReferences(got); again != got {
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
	if got := neutralizeAttestationMarkers(scm.ProviderGitHub, in); got != want {
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
	t.Parallel()
	body := "Fixes #4.\n1. Closes #5\n2) Resolves owner/repo#6;\nThis fixes #7 partly.\n"
	got := strings.Join(extractClosingKeywordLines(body, githubClosingGrammar), "|")
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
	lines := extractClosingKeywordLines(readPRBodyFile(t, bodyFile), githubClosingGrammar)
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

// The GitLab half of the grammar: its default closing pattern adds the gerunds
// and "implements" to GitHub's words and lets a project path nest under
// subgroups. A line GitHub would not close on must not be counted as a closing
// declaration there either, or an author's ordinary sentence would silently
// cover a requested reference.
func TestClosingGrammar_GitLabWordsAndNestedReferences(t *testing.T) {
	t.Parallel()

	for _, line := range []string{
		"Implements #12",
		"Implement #12",
		"Fixing #12",
		"Resolving #12",
		"- Closes: #12.",
		"Closes group/subgroup/project#12",
		"Fixes #1, #2 and #3",
		"Closes issues #12",
	} {
		if got := extractClosingKeywordLines(line, gitlabClosingGrammar); len(got) != 1 {
			t.Fatalf("extractClosingKeywordLines(%q, gitlab) = %v, want the line recognised", line, got)
		}
	}
	// A URL closing reference is protected from generated prose (the neutralizer
	// covers it) but is not a comparable target, so it is deliberately not a
	// closing *line* here: nothing links its host and path back to a
	// canonicalizable reference.
	if got := extractClosingKeywordLines("Closes https://gitlab.example.com/group/subgroup/project/-/issues/12", gitlabClosingGrammar); len(got) != 0 {
		t.Fatalf("extractClosingKeywordLines(URL, gitlab) = %v, want none", got)
	}
	if got := neutralizeGitLabClosingReferences("Closes https://gitlab.example.com/group/subgroup/project/-/issues/12"); !strings.Contains(got, `Closes&#32;https://`) {
		t.Fatalf("neutralizeGitLabClosingReferences(URL) = %q, want the URL reference neutralized", got)
	}
	for _, line := range []string{"Implements #12", "Fixing #12"} {
		if got := extractClosingKeywordLines(line, githubClosingGrammar); len(got) != 0 {
			t.Fatalf("extractClosingKeywordLines(%q, github) = %v, want none - GitHub has no such keyword", line, got)
		}
	}
}

func TestNeutralizeGitLabClosingReferences(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"leading zero", "Fixes #012, #34", "Fixes&#32;#012, #34"},
		{"project leading zero", "Fixes project#012, #34", "Fixes&#32;project#012, #34"},
		{"bracket leading zero", "Fixes [issue:012], #34", "Fixes&#32;[issue:012], #34"},
		{"unknown reference", "Fixes future-reference, #34", "Fixes&#32;future-reference, #34"},
		{"colon and spaces", "Closes:   #12", "Closes:&#32;  #12"},
		{"implements", "Implements #12", "Implements&#32;#12"},
		{"gerund", "Fixing #12", "Fixing&#32;#12"},
		{"issues prefix", "Closes issues #12", "Closes&#32;issues #12"},
		{"nested project path", "Closes group/subgroup/project#12", "Closes&#32;group/subgroup/project#12"},
		{"issue url", "Closes https://gitlab.example.com/group/project/-/issues/12", "Closes&#32;https://gitlab.example.com/group/project/-/issues/12"},
		{"legacy issue url", "Closes https://gitlab.com/group/project/issues/12", "Closes&#32;https://gitlab.com/group/project/issues/12"},
		{"incident url", "Closes https://gitlab.com/group/project/-/issues/incident/12", "Closes&#32;https://gitlab.com/group/project/-/issues/incident/12"},
		{"legacy incident url", "Closes https://gitlab.com/group/project/issues/incident/12", "Closes&#32;https://gitlab.com/group/project/issues/incident/12"},
		{"jira key", "Fixes PROJECT-7", "Fixes&#32;PROJECT-7"},
		{"jira key with digits and underscore", "Resolves PROJ_2-7", "Resolves&#32;PROJ_2-7"},
		{"alternative issue prefix", "Closes GL-12", "Closes&#32;GL-12"},
		{"bracketed issue", "Closes [issue:12]", "Closes&#32;[issue:12]"},
		{"bracketed cross-project issue", "Closes [issue:group/subgroup/project/12]", "Closes&#32;[issue:group/subgroup/project/12]"},
		{"mixed targets", "Fixes PROJECT-7, #12 and GL-13", "Fixes&#32;PROJECT-7, #12 and GL-13"},
		{"generic url before issue", "Fixes https://example.com/docs, #12", "Fixes&#32;https://example.com/docs, #12"},
		{"http url before issue", "Fixes http://example.com/docs, #12", "Fixes&#32;http://example.com/docs, #12"},
		{"complete mixed statement", "Closes https://example.com/docs, #12 and group/subgroup/project#13, [issue:14], PROJECT-15", "Closes&#32;https://example.com/docs, #12 and group/subgroup/project#13, [issue:14], PROJECT-15"},
		{"url after native reference", "Fixes #12, https://example.com/docs and #13", "Fixes&#32;#12, https://example.com/docs and #13"},
		{"comma without spaces", "Fixes https://example.com/docs,#12", "Fixes&#32;https://example.com/docs,#12"},
		{"space separated references", "Fixes #12 #13", "Fixes&#32;#12 #13"},
		{"adjacent references", "Fixes #12#13", "Fixes&#32;#12#13"},
		{"repeated issues prefix", "Fixes issues #12, issues #13", "Fixes&#32;issues #12, issues #13"},
		{"statement stops at prose", "Fixes #12, related to #13", "Fixes&#32;#12, related to #13"},
		{"multiple statements", "Fixes #12, #13; Implements #14 and #15", "Fixes&#32;#12, #13; Implements&#32;#14 and #15"},
		{"project work item url", "Closes https://gitlab.example.com/group/project/-/work_items/12", "Closes&#32;https://gitlab.example.com/group/project/-/work_items/12"},
		{"group work item url", "Closes https://gitlab.example.com/groups/group/subgroup/-/work_items/12", "Closes&#32;https://gitlab.example.com/groups/group/subgroup/-/work_items/12"},
		{"fenced work item url", "```\nCloses https://gitlab.com/group/project/-/work_items/12\n```", "```\nCloses&#32;https://gitlab.com/group/project/-/work_items/12\n```"},
		{"inside a fenced block", "```text\nFixes #12\n```", "```text\nFixes&#32;#12\n```"},
		{"inside an indented block", "    Closes #12", "    Closes&#32;#12"},
		{"a reference without a keyword", "Related to #12", "Related to #12"},
		{"an already neutralized reference", "Fixes `#12`", "Fixes&#32;`#12`"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := neutralizeGitLabClosingReferences(tc.in)
			if html.UnescapeString(got) != tc.in {
				t.Fatalf("neutralization changed readable text: %q", got)
			}
			for _, wrap := range [][2]string{{"", ""}, {"```text\n", "\n```"}, {"    ", ""}, {"`", "`"}} {
				input, want := wrap[0]+tc.in+wrap[1], wrap[0]+tc.want+wrap[1]
				if published := neutralizeAttestationMarkers(scm.ProviderGitLab, input); published != want {
					t.Fatalf("published text = %q, want %q", published, want)
				}
			}
			if got != tc.want {
				t.Fatalf("neutralizeGitLabClosingReferences(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if again := neutralizeGitLabClosingReferences(got); again != got {
				t.Fatalf("neutralization is not idempotent: %q -> %q", got, again)
			}
		})
	}
}

// GitHub's neutralizer is untouched: the GitLab-only words stay ordinary text,
// and the code-block exemption it relies on stays exactly as it was.
func TestNeutralizeGitHubClosingReferencesLeavesGitLabOnlyFormsAlone(t *testing.T) {
	t.Parallel()

	in := "Implements #12\nFixing #13\n```\nFixes #14\n```\nFixes #15"
	want := "Implements #12\nFixing #13\n```\nFixes #14\n```\nFixes `#15`"
	if got := neutralizeGitHubClosingReferences(in); got != want {
		t.Fatalf("neutralizeGitHubClosingReferences() = %q, want %q", got, want)
	}
}

// gitlabClosingContext is a StepContext whose repository resolves to GitLab, so
// the closing grammar, the project path, and the rendered references all come
// from the GitLab side.
func gitlabClosingContext(refs ...string) *pipeline.StepContext {
	return &pipeline.StepContext{
		Ctx:              context.Background(),
		Repo:             &db.Repo{ID: "repo-1", UpstreamURL: "https://gitlab.com/group/subgroup/project.git"},
		Run:              &db.Run{ID: "run-1"},
		Config:           &config.Config{},
		ClosingIssueRefs: refs,
	}
}

// A requested reference renders in the Issues section and verifies against the
// published body on GitLab, including a cross-project reference to a project
// nested under subgroups.
func TestIssuesSectionOnGitLabRendersAndVerifiesEveryReferenceForm(t *testing.T) {
	t.Parallel()

	sctx := gitlabClosingContext("42", "77", "other/subgroup/project#5")
	section := issuesSection(sctx, "")
	want := "## Issues\n\nCloses #42\nCloses #77\nCloses other/subgroup/project#5"
	if section != want {
		t.Fatalf("issuesSection() =\n%s\nwant:\n%s", section, want)
	}
	if err := verifyClosingIssuesInBody("body\n\n"+section+"\n", sctx); err != nil {
		t.Fatalf("verifyClosingIssuesInBody() = %v", err)
	}
	if err := verifyClosingIssuesInBody("## Issues\n\nCloses #42\n", sctx); err == nil {
		t.Fatal("verifyClosingIssuesInBody() accepted a body missing two requested references")
	}
}

// The author's own closing line covers a requested reference on GitLab for the
// words GitLab closes on - including "Implements", which GitHub does not.
func TestIssuesSectionAuthorCoverageFollowsTheGitLabGrammar(t *testing.T) {
	t.Parallel()

	sctx := gitlabClosingContext("42")
	if got := issuesSection(sctx, "## Overview\n\nImplements #42\n"); got != "" {
		t.Fatalf("issuesSection() = %q, want the author's GitLab closing line to cover #42", got)
	}
	// The same author text on GitHub closes nothing, so the reference is
	// rendered rather than silently assumed closed.
	sctx.Repo.UpstreamURL = "https://github.com/owner/repo.git"
	if got := issuesSection(sctx, "## Overview\n\nImplements #42\n"); !strings.Contains(got, "Closes #42") {
		t.Fatalf("issuesSection() = %q, want the reference rendered on GitHub", got)
	}
}

// closesBoundaryHost is a Host that only declares the capabilities a case needs
// plus the body read every closing-reference publication verifies against.
type closesBoundaryHost struct {
	scm.Host
	caps scm.Capabilities
}

func (h closesBoundaryHost) Capabilities() scm.Capabilities { return h.caps }

func (h closesBoundaryHost) GetPRContent(context.Context, *scm.PR) (scm.PRContent, error) {
	return scm.PRContent{Title: "title", Body: "body"}, nil
}

// claimClosingIssueRefs is the one gate --closes passes through: it localizes
// the run's references, fails closed on a reference shape the hosting provider
// would misread, and fails closed when the provider does not close issues from
// the body at all.
func TestClaimClosingIssueRefsProviderBoundaries(t *testing.T) {
	t.Parallel()

	closingCaps := scm.Capabilities{ClosingReferences: true}
	for _, tc := range []struct {
		name     string
		upstream string
		provider scm.Provider
		caps     scm.Capabilities
		refs     []string
		wantRefs []string
		wantErr  string
	}{
		{
			name:     "gitlab accepts bare, own-project nested, and cross-project references",
			upstream: "https://gitlab.com/group/subgroup/project.git",
			provider: scm.ProviderGitLab,
			caps:     closingCaps,
			refs:     []string{"42", "GROUP/Subgroup/Project#42", "other/project#7"},
			wantRefs: []string{"42", "other/project#7"},
		},
		{
			name:     "github refuses a nested subgroup reference",
			upstream: "https://github.com/owner/repo.git",
			provider: scm.ProviderGitHub,
			caps:     closingCaps,
			refs:     []string{"group/subgroup/project#42"},
			wantErr:  "GitLab subgroup reference",
		},
		{
			name:     "a provider that cannot close issues from the body is refused",
			upstream: "https://bitbucket.org/test/repo.git",
			provider: scm.ProviderBitbucket,
			caps:     scm.Capabilities{},
			refs:     []string{"42"},
			wantErr:  "does not support closing references",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := setupGitRepo(t)
			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
			sctx.Repo.UpstreamURL = tc.upstream
			if err := sctx.DB.UpdateRunClosingIssueRefs(sctx.Run.ID, tc.refs); err != nil {
				t.Fatal(err)
			}

			host := closesBoundaryHost{caps: tc.caps}
			err := claimClosingIssueRefs(sctx, host, tc.provider)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("claimClosingIssueRefs() error = %v, want %q", err, tc.wantErr)
				}
				if len(sctx.ClosingIssueRefs) != 0 {
					t.Fatalf("refs = %v, want none claimed on a refusal", sctx.ClosingIssueRefs)
				}
				return
			}
			if err != nil {
				t.Fatalf("claimClosingIssueRefs() error = %v", err)
			}
			if got := strings.Join(sctx.ClosingIssueRefs, ","); got != strings.Join(tc.wantRefs, ",") {
				t.Fatalf("refs = %q, want %q", got, strings.Join(tc.wantRefs, ","))
			}
		})
	}
}

// End to end on GitLab: a run carrying --closes publishes the reference in the
// merge request description and verifies it by reading the description back.
// The referenceless prose around it stays neutralized, including a drafted
// closing sentence - GitLab's closing pattern is a raw regex over the whole
// description, so that sentence in the body would otherwise close the issue.
func TestPRStep_GitLabClosesRendersAndVerifiesOnTheMR(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	env, _, stateFile := fakeGlabWithMRState(t)

	ag := &mockAgent{name: "test", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		payload := json.RawMessage(`{"title":"fix: implement the widget","body":"## What Changed\n\n- implement the widget\n\nImplements #95"}`)
		return &agent.Result{Output: payload}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Repo.UpstreamURL = "https://gitlab.com/test/repo.git"
	if err := sctx.DB.UpdateRunClosingIssueRefs(sctx.Run.ID, []string{"95", "other/subgroup/project#7"}); err != nil {
		t.Fatal(err)
	}

	step := &PRStep{}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}
	state, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatalf("the merge request was never published: %v", err)
	}
	var published struct {
		Description *string `json:"description"`
	}
	if err := json.Unmarshal(state, &published); err != nil {
		t.Fatal(err)
	}
	if published.Description == nil {
		t.Fatal("the created merge request carried no description")
	}
	body := *published.Description
	if !strings.Contains(body, "## Issues\n\nCloses #95\nCloses other/subgroup/project#7") {
		t.Fatalf("published body lacks the requested closing references:\n%s", body)
	}
	if err := verifyClosingIssuesInBody(body, sctx); err != nil {
		t.Fatalf("verifyClosingIssuesInBody() = %v", err)
	}
	lines := extractClosingKeywordLines(body, gitlabClosingGrammar)
	if got := strings.Join(lines, "|"); got != "Closes #95|Closes other/subgroup/project#7" {
		t.Fatalf("closing lines = %q, want exactly the two requested references:\n%s", got, body)
	}
	if !strings.Contains(body, `Implements&#32;#95`) {
		t.Fatalf("the drafted closing sentence was published live:\n%s", body)
	}
}

// The same run on GitHub publishes the GitHub spelling of protection: the
// drafted sentence's reference is code-spanned, and no backslash escape appears.
func TestPRStep_GitHubClosesKeepsGitHubNeutralization(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	env, _ := fakeGH(t, "")
	bodyFile := envEntry(env, "FAKE_CLI_PR_BODY_FILE")

	ag := &mockAgent{name: "test", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		payload := json.RawMessage(`{"title":"fix: implement the widget","body":"## What Changed\n\n- implement the widget\n\nImplements #95"}`)
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
	if !strings.Contains(body, "## Issues\n\nCloses #95") {
		t.Fatalf("published body lacks the requested closing reference:\n%s", body)
	}
	// "Implements" is not a GitHub closing keyword, so it stays verbatim.
	if !strings.Contains(body, "Implements #95") || strings.Contains(body, `Implements&#32;#95`) {
		t.Fatalf("GitHub body neutralization changed:\n%s", body)
	}
}

func TestGitLabClosingCoverageRequiresLiteralSpaces(t *testing.T) {
	for _, line := range []string{
		"Closes\t#42", "Closes:\t#42", "Closes issues\t#42",
		"Closes #1,\t#42", "Closes #1\tand #42", "Closes #1 and\t#42",
		"Closes #1\t,#42", "Closes #1, and\t#42",
	} {
		t.Run(line, func(t *testing.T) {
			sctx := gitlabClosingContext("42")
			if got := issuesSection(sctx, line); got != "## Issues\n\nCloses #42" {
				t.Fatalf("issuesSection(%q) = %q", line, got)
			}
			if err := verifyClosingIssuesInBody(line, sctx); err == nil {
				t.Fatalf("verification accepted %q", line)
			}
			valid := strings.ReplaceAll(line, "\t", " ")
			if got := issuesSection(sctx, valid); got != "" {
				t.Fatalf("valid line %q not credited: %q", valid, got)
			}
			if err := verifyClosingIssuesInBody(valid, sctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestGitLabClosingCoverageRejectsTabs(t *testing.T) {
	t.Parallel()
	for _, body := range []string{
		"Closes\t#42", "Closes: \t#42", "Closes issues\t#42",
		"Closes #1,\t#42", "Closes #1 and\t#42", "Closes #1\tand #42",
	} {
		t.Run(body, func(t *testing.T) {
			sctx := gitlabClosingContext("42")
			if got := issuesSection(sctx, body); got != "## Issues\n\nCloses #42" {
				t.Fatalf("author text suppressed requested closure: %q", got)
			}
			if err := verifyClosingIssuesInBody(body, sctx); err == nil {
				t.Fatal("tab-separated reference verified as a live closure")
			}
			if err := verifyClosingIssuesInBody(appendIssuesSection(body, issuesSection(sctx, body)), sctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}
