package steps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/jev"
)

// fakeJevClient answers pre-brief evaluations from a script.
type fakeJevClient struct {
	answer  func(id string, q jev.Question) jev.Answer
	err     error
	calls   int
	lastReq map[string]jev.Question
	state   *jevChangeState
}

func (f *fakeJevClient) Evaluate(_ context.Context, state any, questions map[string]jev.Question) (*jev.Response, error) {
	f.calls++
	f.lastReq = questions
	if s, ok := state.(*jevChangeState); ok {
		f.state = s
	}
	if f.err != nil {
		return nil, f.err
	}
	answers := make(map[string]jev.Answer, len(questions))
	for id, q := range questions {
		answers[id] = f.answer(id, q)
	}
	return &jev.Response{Model: jev.Model, Answers: answers, Usage: jev.Usage{InputTokens: 12000}}, nil
}

// highScoreEverything answers every relevance score high and every domain
// noul low.
func highScoreEverything(id string, q jev.Question) jev.Answer {
	if strings.HasPrefix(id, "ctx_") {
		return jev.Answer{Type: "score", Score: 2.6, Confidence: 0.9}
	}
	return jev.Answer{Type: "noul", Noul: 0.05}
}

func reviewPromptOf(t *testing.T, ag *mockAgent) string {
	t.Helper()
	for _, call := range ag.calls {
		if call.Purpose == "review" {
			return call.Prompt
		}
	}
	t.Fatal("no review turn ran")
	return ""
}

func cleanReviewResult(t *testing.T, dir, baseSHA string) *agent.Result {
	t.Helper()
	findings := cleanReviewFindings()
	findings.ReviewedPaths = fullReviewCoverage(t, dir, baseSHA)
	encoded, err := json.Marshal(findings)
	if err != nil {
		t.Fatal(err)
	}
	return &agent.Result{Output: encoded}
}

// TestReviewStep_JevPrebriefDisabledByDefault pins the opt-in contract: with
// jev.review_assist unset, no pre-brief client is consulted and the review
// prompt carries no pre-brief section.
func TestReviewStep_JevPrebriefDisabledByDefault(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	fake := &fakeJevClient{answer: highScoreEverything}
	ag := &mockAgent{
		name: "test",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			return cleanReviewResult(t, dir, baseSHA), nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	outcome, err := (&ReviewStep{jev: fake}).Execute(sctx)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if outcome == nil {
		t.Fatal("no outcome")
	}
	if fake.calls != 0 {
		t.Fatalf("jev client called %d times with the assist disabled", fake.calls)
	}
	if strings.Contains(reviewPromptOf(t, ag), "Pre-brief") {
		t.Fatal("prompt carries a pre-brief with the assist disabled")
	}
}

// TestReviewStep_JevPrebriefAddsAdvisorySection pins the enabled path: the
// ranked context and domain flags reach the review prompt as an advisory
// section, and the coverage obligations stay in the prompt alongside it.
func TestReviewStep_JevPrebriefAddsAdvisorySection(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupJevRepo(t)
	fake := &fakeJevClient{answer: func(id string, q jev.Question) jev.Answer {
		if strings.HasPrefix(id, "ctx_") {
			return jev.Answer{Type: "score", Score: 2.6, Confidence: 0.9}
		}
		if id == "domain_concurrency" {
			return jev.Answer{Type: "noul", Noul: 0.9}
		}
		return jev.Answer{Type: "noul", Noul: 0.05}
	}}
	ag := &mockAgent{
		name: "test",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			return cleanReviewResult(t, dir, baseSHA), nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Jev.ReviewAssist = true

	if _, err := (&ReviewStep{jev: fake}).Execute(sctx); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	prompt := reviewPromptOf(t, ag)
	for _, want := range []string{
		"Pre-brief (advisory",
		"claims, not evidence",
		"widget/user.go", // the use site of the changed definition, ranked
		"Domain flag: the change appears to involve concurrency",
		"Report reviewed_paths", // the coverage obligation survives
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	if fake.calls != 1 {
		t.Fatalf("jev calls = %d, want 1 batched evaluation", fake.calls)
	}
	if fake.state == nil || !strings.Contains(fake.state.Change.Diff, "RenderWidget") {
		t.Fatal("jev state does not carry the change diff")
	}
}

// TestReviewStep_JevPrebriefFailureFallsBack pins fail-closed: a Jev error
// leaves the review prompt byte-identical to the assist being off, and the
// review still runs.
func TestReviewStep_JevPrebriefFailureFallsBack(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	fake := &fakeJevClient{err: errors.New("jev: 529 overloaded")}
	var logs []string
	ag := &mockAgent{
		name: "test",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			return cleanReviewResult(t, dir, baseSHA), nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Jev.ReviewAssist = true
	sctx.Log = func(s string) { logs = append(logs, s) }

	if _, err := (&ReviewStep{jev: fake}).Execute(sctx); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(reviewPromptOf(t, ag), "Pre-brief") {
		t.Fatal("failed pre-brief leaked into the prompt")
	}
	found := false
	for _, line := range logs {
		if strings.Contains(line, "jev pre-brief unavailable") {
			found = true
		}
	}
	if !found {
		t.Fatal("no log line records the pre-brief fallback")
	}
}

// TestReviewStep_JevPrebriefMissingKeyFallsBack covers the enabled-but-no-key
// configuration: one log line, no pre-brief, review unchanged.
func TestReviewStep_JevPrebriefMissingKeyFallsBack(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	t.Setenv(jev.EnvKey, "")
	var logs []string
	ag := &mockAgent{
		name: "test",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			return cleanReviewResult(t, dir, baseSHA), nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Jev.ReviewAssist = true
	sctx.Log = func(s string) { logs = append(logs, s) }

	if _, err := (&ReviewStep{}).Execute(sctx); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(reviewPromptOf(t, ag), "Pre-brief") {
		t.Fatal("pre-brief present without an API key")
	}
	found := false
	for _, line := range logs {
		if strings.Contains(line, jev.EnvKey) {
			found = true
		}
	}
	if !found {
		t.Fatal("no log line names the missing key")
	}
}

// setupJevRepo builds a repo whose feature change introduces a definition
// that an unchanged file references, so the candidate builder has a use site
// to find.
func setupJevRepo(t *testing.T) (string, string, string) {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) string {
		return gitCmd(t, dir, args...)
	}
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	run("init")
	run("checkout", "-b", "main")
	write("widget/widget.go", "package widget\n\nfunc RenderWidget() string {\n\treturn \"base\"\n}\n")
	write("widget/user.go", "package widget\n\nfunc Show() string {\n\treturn RenderWidget()\n}\n")
	write("widget/widget_test.go", "package widget\n\nimport \"testing\"\n\nfunc TestRenderWidget(t *testing.T) {\n\tif RenderWidget() == \"\" {\n\t\tt.Fatal(\"empty\")\n\t}\n}\n")
	run("add", "-A")
	run("commit", "-m", "base commit")
	baseSHA := run("rev-parse", "HEAD")

	run("checkout", "-b", "feature")
	write("widget/widget.go", "package widget\n\nfunc RenderWidget() string {\n\treturn \"changed\"\n}\n\nfunc RenderWidgetV2() string {\n\treturn RenderWidget()\n}\n")
	run("add", "-A")
	run("commit", "-m", "change widget")
	headSHA := run("rev-parse", "HEAD")
	return dir, baseSHA, headSHA
}

// TestJevContextCandidates_FindsUseSitesAndSiblings pins the code-built
// candidate set: use sites of changed definitions carry their matched lines,
// siblings and test counterparts are included, and the changed file itself is
// never a candidate.
func TestJevContextCandidates_FindsUseSitesAndSiblings(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupJevRepo(t)
	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	diff := gitCmd(t, dir, "diff", "--no-renames", baseSHA+".."+headSHA)
	candidates := jevContextCandidates(sctx.Ctx, sctx, diff, []string{"widget/widget.go"})

	byPath := map[string]jevCandidate{}
	for _, c := range candidates {
		byPath[c.Path] = c
		if c.Path == "widget/widget.go" {
			t.Fatal("the changed file must never be its own context candidate")
		}
	}
	user, ok := byPath["widget/user.go"]
	if !ok {
		t.Fatalf("widget/user.go (use site) not among candidates: %v", byPath)
	}
	joined := strings.Join(user.Matches, "\n")
	if !strings.Contains(joined, "RenderWidget") {
		t.Fatalf("use-site candidate carries no coupling evidence: %v", user.Matches)
	}
	if _, ok := byPath["widget/widget_test.go"]; !ok {
		t.Fatal("test counterpart not among candidates")
	}
}

func TestJevIdentifiers(t *testing.T) {
	t.Parallel()
	diff := `+++ b/widget/widget.go
@@ -1,5 +1,9 @@ func RenderWidget() string {
+func RenderWidgetV2() string {
+	return RenderWidget()
+}
+type WidgetOption struct{}
`
	ids := jevIdentifiers(diff)
	joined := strings.Join(ids, ",")
	for _, want := range []string{"RenderWidgetV2", "WidgetOption", "RenderWidget"} {
		if !strings.Contains(joined, want) {
			t.Errorf("identifiers %v missing %q", ids, want)
		}
	}
	for _, unwanted := range []string{"return"} {
		for _, id := range ids {
			if id == unwanted {
				t.Errorf("identifier %q should not be extracted", unwanted)
			}
		}
	}
}

func TestFormatJevPrebrief_Thresholds(t *testing.T) {
	t.Parallel()
	candidates := []jevCandidate{{Path: "a.go"}, {Path: "b.go"}, {Path: "c.go"}}
	resp := &jev.Response{Answers: map[string]jev.Answer{
		// High score, confident: listed.
		"ctx_0": {Type: "score", Score: 2.5, Confidence: 0.9},
		// High score but unconfident: not listed.
		"ctx_1": {Type: "score", Score: 2.5, Confidence: 0.1},
		// Below the relevance bar: not listed.
		"ctx_2": {Type: "score", Score: 1.4, Confidence: 0.95},
		// Domain below threshold: no flag.
		"domain_auth": {Type: "noul", Noul: 0.4},
		// Domain at threshold: flagged.
		"domain_errors": {Type: "noul", Noul: 0.65},
	}}
	section, listed, flags := formatJevPrebrief(resp, candidates)
	if listed != 1 || flags != 1 {
		t.Fatalf("listed=%d flags=%d, want 1 and 1", listed, flags)
	}
	if !strings.Contains(section, "a.go") || strings.Contains(section, "b.go") || strings.Contains(section, "c.go") {
		t.Errorf("section lists the wrong candidates:\n%s", section)
	}
	if strings.Contains(section, "protected-resource") {
		t.Error("auth domain below threshold must not flag")
	}
	if !strings.Contains(section, "error handling or rollback") {
		t.Error("errors domain at threshold must flag")
	}
}

func TestFormatJevPrebrief_NothingToSurface(t *testing.T) {
	t.Parallel()
	resp := &jev.Response{Answers: map[string]jev.Answer{
		"ctx_0":       {Type: "score", Score: 0.4, Confidence: 0.99},
		"domain_auth": {Type: "noul", Noul: 0.1},
	}}
	section, listed, flags := formatJevPrebrief(resp, []jevCandidate{{Path: "a.go"}})
	if section != "" || listed != 0 || flags != 0 {
		t.Fatalf("section=%q listed=%d flags=%d, want empty", section, listed, flags)
	}
}

func TestClipMiddle(t *testing.T) {
	t.Parallel()
	short := "short text"
	if got := clipMiddle(short, 100); got != short {
		t.Errorf("clipMiddle changed a short text")
	}
	long := strings.Repeat("a", 1000)
	got := clipMiddle(long, 100)
	if len(got) >= 1000 {
		t.Errorf("clipMiddle did not clip: %d bytes", len(got))
	}
	if !strings.Contains(got, "bytes omitted") {
		t.Errorf("clipMiddle lost its omission marker")
	}
	if !strings.HasPrefix(got, "aaa") || !strings.HasSuffix(got, "aaa") {
		t.Errorf("clipMiddle lost head or tail: %q...%q", got[:10], got[len(got)-10:])
	}
}

func TestTestCounterpart(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"pkg/foo.go":      "pkg/foo_test.go",
		"pkg/foo_test.go": "pkg/foo.go",
		"pkg/mod.py":      "pkg/test_mod.py",
		"pkg/test_mod.py": "pkg/mod.py",
		"web/app.ts":      "web/app.test.ts",
		"web/app.test.ts": "web/app.ts",
		"README.md":       "",
	}
	for in, want := range cases {
		if got := testCounterpart(in); got != want {
			t.Errorf("testCounterpart(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestBuildJevQuestions pins the question battery shape: one noul per domain
// plus one score per candidate, keyed stably.
func TestBuildJevQuestions(t *testing.T) {
	t.Parallel()
	candidates := []jevCandidate{{Path: "a.go"}, {Path: "b.go"}}
	questions := buildJevQuestions(candidates)
	if len(questions) != len(jevDomains)+2 {
		t.Fatalf("questions = %d, want %d", len(questions), len(jevDomains)+2)
	}
	for _, d := range jevDomains {
		q, ok := questions[d.id]
		if !ok || q.Type != "noul" {
			t.Errorf("domain %s missing or not a noul", d.id)
		}
	}
	for i := range candidates {
		q, ok := questions[fmt.Sprintf("ctx_%d", i)]
		if !ok || q.Type != "score" {
			t.Errorf("candidate %d missing or not a score", i)
		}
	}
}

// TestReviewStep_JevPrebriefStateCarriesRereviewFindings pins that a
// rereview's Jev state includes the sanitized outstanding findings, so domain
// triggers and ranking judge the whole round, not the bare diff.
func TestReviewStep_JevPrebriefStateCarriesRereviewFindings(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupJevRepo(t)
	fake := &fakeJevClient{answer: highScoreEverything}
	ag := &mockAgent{
		name: "test",
		runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if opts.Purpose == "review-fix" {
				return &agent.Result{Text: `{"summary":"fixed it"}`, Output: json.RawMessage(`{"summary":"fixed it"}`)}, nil
			}
			return cleanReviewResult(t, dir, baseSHA), nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Jev.ReviewAssist = true
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"severity":"error","file":"widget/widget.go","line":3,"description":"RenderWidget broke its callers","action":"auto-fix"}],"risk_level":"medium","risk_rationale":"x","risk_scope":"source-or-external"}`

	if _, err := (&ReviewStep{jev: fake}).Execute(sctx); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if fake.calls != 1 {
		t.Fatalf("jev calls = %d, want 1", fake.calls)
	}
	if fake.state == nil || !strings.Contains(fake.state.Change.OutstandingFindings, "RenderWidget broke its callers") {
		t.Fatalf("rereview state missing outstanding findings: %+v", fake.state)
	}
	// The rereview digests the worktree diff (base..worktree), so the fixer's
	// uncommitted work would be included; with no fixer edits here the diff
	// still covers the feature change.
	if !strings.Contains(fake.state.Change.Diff, "RenderWidgetV2") {
		t.Fatal("rereview diff digest missing the change")
	}
}
