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

// highScoreEverything answers every relevance score high.
func highScoreEverything(string, jev.Question) jev.Answer {
	return jev.Answer{Type: "score", Score: 2.6, Confidence: 0.9, Probabilities: map[string]float64{"2": 0.8, "3": 0.1}}
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
// ranked context reaches the review prompt as an advisory section, and the
// coverage obligations stay in the prompt alongside it.
func TestReviewStep_JevPrebriefAddsAdvisorySection(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupJevRepo(t)
	fake := &fakeJevClient{answer: highScoreEverything}
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
		"widget/user.go",        // the use site of the changed definition, ranked
		"widget/style.go",       // a same-directory sibling
		"Report reviewed_paths", // the coverage obligation survives
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	if strings.Index(prompt, "widget/user.go") > strings.Index(prompt, "widget/style.go") {
		t.Error("a sibling Jev scored the same is listed ahead of the use site")
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
	want := "jev review assist is enabled but " + jev.EnvKey + " is not set"
	if !strings.Contains(strings.Join(logs, "\n"), want) {
		t.Fatalf("logs do not report the missing key as %q:\n%s", want, strings.Join(logs, "\n"))
	}
	for _, line := range logs {
		if strings.Contains(line, "jev pre-brief") {
			t.Fatalf("pre-brief work ran without an API key: %q", line)
		}
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
	run("config", "user.name", "test")
	run("config", "user.email", "test@test.com")
	run("checkout", "-b", "main")
	write("widget/widget.go", "package widget\n\nfunc RenderWidget() string {\n\treturn \"base\"\n}\n")
	write("widget/user.go", "package widget\n\nfunc Show() string {\n\treturn RenderWidget()\n}\n")
	write("widget/widget_test.go", "package widget\n\nimport \"testing\"\n\nfunc TestRenderWidget(t *testing.T) {\n\tif RenderWidget() == \"\" {\n\t\tt.Fatal(\"empty\")\n\t}\n}\n")
	write("widget/style.go", "package widget\n")
	write("widget/widget_pb.go", "package widget\n")
	write("fixtures/payload.json", "{\"call\": \"RenderWidget\"}\n")
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
// candidate set: use sites of changed definitions and same-directory siblings
// are included, and the changed file itself is never a candidate.
func TestJevContextCandidates_FindsUseSitesAndSiblings(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupJevRepo(t)

	diff := gitCmd(t, dir, "diff", "--no-renames", baseSHA+".."+headSHA)
	changed := []string{"widget/widget.go"}
	candidates := jevContextCandidates(context.Background(), dir, diff, changed, changed, nil)

	byPath := map[string]jevCandidate{}
	for _, c := range candidates {
		byPath[c.Path] = c
		if c.Path == "widget/widget.go" {
			t.Fatal("the changed file must never be its own context candidate")
		}
	}
	if _, ok := byPath["widget/user.go"]; !ok {
		t.Fatalf("widget/user.go (use site) not among candidates: %v", byPath)
	}
	if _, ok := byPath["widget/widget_test.go"]; !ok {
		t.Fatal("test file referencing the changed definition not among candidates")
	}
	if _, ok := byPath["widget/style.go"]; !ok {
		t.Fatal("same-directory sibling not among candidates")
	}
}

// TestJevContextCandidates_RanksRareNamesAboveUbiquitousOnes reproduces a cap
// filled in path order: a changed local name that appears in more files than
// the cap must not crowd out the one file that uses the changed function.
func TestJevContextCandidates_RanksRareNamesAboveUbiquitousOnes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
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
	gitCmd(t, dir, "init")
	write("zz/widget.go", "package zz\n\nfunc RenderWidget() string {\n\treturn \"base\"\n}\n")
	write("zz/user.go", "package zz\n\nfunc Show() string {\n\treturn RenderWidget()\n}\n")
	for i := 0; i < jevMaxCandidates+5; i++ {
		write(fmt.Sprintf("aa/f%02d.go", i), fmt.Sprintf("package aa\n\nfunc f%02d() int {\n\tresult := %d\n\treturn result\n}\n", i, i))
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "base")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	write("zz/widget.go", "package zz\n\nfunc RenderWidget() string {\n\tvar result = \"changed\"\n\treturn result\n}\n")
	gitCmd(t, dir, "commit", "-am", "change")

	diff := gitCmd(t, dir, "diff", "--no-renames", baseSHA+"..HEAD")
	changed := []string{"zz/widget.go"}
	candidates := jevContextCandidates(context.Background(), dir, diff, changed, changed, nil)
	if len(candidates) != jevMaxCandidates {
		t.Fatalf("candidates = %d, want the cap %d", len(candidates), jevMaxCandidates)
	}
	if candidates[0].Path != "zz/user.go" {
		t.Fatalf("first candidate = %s, want zz/user.go (the only use site of RenderWidget)", candidates[0].Path)
	}
}

// TestJevContextCandidates_LaterFileNamesAreSearched reproduces a search
// budget spent on the first file in path order: names the first file
// introduces have no use site yet, so they must not keep a later file's
// changed function from being searched.
func TestJevContextCandidates_LaterFileNamesAreSearched(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
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
	gitCmd(t, dir, "init")
	write("aa/first.go", "package aa\n")
	write("zz/widget.go", "package zz\n\nfunc RenderWidget() string {\n\treturn \"base\"\n}\n")
	write("app/user.go", "package app\n\nfunc Show() string {\n\treturn zz.RenderWidget()\n}\n")
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "base")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	var fresh strings.Builder
	fresh.WriteString("package aa\n")
	for i := 0; i < jevMaxGreps; i++ {
		fmt.Fprintf(&fresh, "\nfunc FreshHelper%02d() {}\n", i)
	}
	write("aa/first.go", fresh.String())
	write("zz/widget.go", "package zz\n\nfunc RenderWidget() string {\n\treturn \"changed\"\n}\n")
	gitCmd(t, dir, "commit", "-am", "change")

	diff := gitCmd(t, dir, "diff", "--no-renames", baseSHA+"..HEAD")
	changed := []string{"aa/first.go", "zz/widget.go"}
	candidates := jevContextCandidates(context.Background(), dir, diff, changed, changed, nil)
	for _, c := range candidates {
		if c.Path == "app/user.go" {
			return
		}
	}
	t.Fatalf("app/user.go (the only use site of the changed RenderWidget) not among candidates: %v", candidates)
}

// TestJevIdentifiers pins which names become search terms and in what order:
// definitions in code, never prose (documentation files, comments, a keyword
// inside a longer word), never names too short to point at a use site, never
// an unchanged neighbour after a hunk's first change, and taken in turn from
// each changed file.
func TestJevIdentifiers(t *testing.T) {
	t.Parallel()
	diff := `diff --git a/docs/guide.md b/docs/guide.md
--- a/docs/guide.md
+++ b/docs/guide.md
@@ -1,2 +1,3 @@
+type ProseOnly struct{}
+let the reviewer replay it
diff --git a/widget/widget.go b/widget/widget.go
--- a/widget/widget.go
+++ b/widget/widget.go
@@ -1,5 +1,9 @@ func RenderWidget() string {
 type EnclosingKind int
+func RenderWidgetV2() string {
+	var b strings.Builder
+	// we let callers choose
+	return RenderWidget()
+}
+export type WidgetOption struct{}
+outlet Plug
 func NeighbourAfter() {}
diff --git a/gadget/gadget.go b/gadget/gadget.go
--- a/gadget/gadget.go
+++ b/gadget/gadget.go
@@ -1,2 +1,4 @@
+func GadgetOne() {}
+func GadgetTwo() {}
`
	got := jevIdentifiers(diff)
	want := []string{"RenderWidget", "GadgetOne", "EnclosingKind", "GadgetTwo", "RenderWidgetV2", "WidgetOption"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("identifiers = %v, want %v", got, want)
	}
}

func TestFormatJevPrebrief_Thresholds(t *testing.T) {
	t.Parallel()
	candidates := []jevCandidate{{Path: "a.go"}, {Path: "b.go"}, {Path: "c.go"}}
	resp := &jev.Response{Answers: map[string]jev.Answer{
		// Most mass at relevant or better, confident: listed.
		"ctx_0": {Type: "score", Score: 2.0, Confidence: 0.9, Probabilities: map[string]float64{"1": 0.2, "2": 0.7, "3": 0.1}},
		// Same mass but unconfident: not listed.
		"ctx_1": {Type: "score", Score: 2.0, Confidence: 0.1, Probabilities: map[string]float64{"1": 0.2, "2": 0.7, "3": 0.1}},
		// Most mass below relevant: not listed, however confident.
		"ctx_2": {Type: "score", Score: 1.4, Confidence: 0.95, Probabilities: map[string]float64{"1": 0.6, "2": 0.4}},
	}}
	section, listed := formatJevPrebrief(resp, candidates)
	if listed != 1 {
		t.Fatalf("listed=%d, want 1", listed)
	}
	if !strings.Contains(section, "a.go") || strings.Contains(section, "b.go") || strings.Contains(section, "c.go") {
		t.Errorf("section lists the wrong candidates:\n%s", section)
	}
}

// TestFormatJevPrebrief_BlendsUseSiteEvidenceIntoOrder pins the ordering
// contract: Jev's score decides what is listed, and the code's use-site
// evidence lifts a candidate by up to one rubric level in the order.
func TestFormatJevPrebrief_BlendsUseSiteEvidenceIntoOrder(t *testing.T) {
	t.Parallel()
	candidates := []jevCandidate{
		{Path: "sibling.go"},
		{Path: "strong_use.go", coupling: 0.5},
		{Path: "weak_use.go", coupling: 0.25},
		{Path: "unlisted_use.go", coupling: 0.5},
	}
	resp := &jev.Response{Answers: map[string]jev.Answer{
		"ctx_0": {Type: "score", Score: 2.9, Confidence: 0.9, Probabilities: map[string]float64{"2": 0.1, "3": 0.9}},
		"ctx_1": {Type: "score", Score: 2.2, Confidence: 0.9, Probabilities: map[string]float64{"2": 0.6, "3": 0.2}},
		"ctx_2": {Type: "score", Score: 2.0, Confidence: 0.9, Probabilities: map[string]float64{"1": 0.35, "2": 0.6, "3": 0.05}},
		"ctx_3": {Type: "score", Score: 1.5, Confidence: 0.9, Probabilities: map[string]float64{"1": 0.6, "2": 0.4}},
	}}
	section, listed := formatJevPrebrief(resp, candidates)
	if listed != 3 || strings.Contains(section, "unlisted_use.go") {
		t.Fatalf("listed=%d, want 3 without the below-threshold use site:\n%s", listed, section)
	}
	strong, sibling, weak := strings.Index(section, "strong_use.go"), strings.Index(section, "sibling.go"), strings.Index(section, "weak_use.go")
	if !(strong < sibling && sibling < weak) {
		t.Fatalf("order is not strong_use, sibling, weak_use:\n%s", section)
	}
}

// TestFormatJevPrebrief_CapKeepsJevsTopRatedFiles reproduces the use-site
// blend deciding which files survive the listing cap: with more qualifying
// candidates than jevMaxListed, the cap must keep the files Jev scored
// highest, and the blend may only reorder them.
func TestFormatJevPrebrief_CapKeepsJevsTopRatedFiles(t *testing.T) {
	t.Parallel()
	candidates := []jevCandidate{{Path: "sibling.go"}}
	answers := map[string]jev.Answer{"ctx_0": {Type: "score", Score: 2.9, Confidence: 0.9, Probabilities: map[string]float64{"2": 0.1, "3": 0.9}}}
	for i := 1; i <= jevMaxListed+1; i++ {
		candidates = append(candidates, jevCandidate{Path: fmt.Sprintf("use%02d.go", i), coupling: 0.1})
		answers[fmt.Sprintf("ctx_%d", i)] = jev.Answer{Type: "score", Score: 2.1, Confidence: 0.9, Probabilities: map[string]float64{"2": 0.7, "3": 0.05}}
	}
	section, listed := formatJevPrebrief(&jev.Response{Answers: answers}, candidates)
	if listed != jevMaxListed {
		t.Fatalf("listed=%d, want the cap %d", listed, jevMaxListed)
	}
	if !strings.Contains(section, "sibling.go") {
		t.Fatalf("the cap dropped the file Jev scored highest:\n%s", section)
	}
	if strings.Index(section, "use01.go") > strings.Index(section, "sibling.go") {
		t.Fatalf("the use-site blend did not reorder the kept files:\n%s", section)
	}
}

func TestFormatJevPrebrief_NothingToSurface(t *testing.T) {
	t.Parallel()
	resp := &jev.Response{Answers: map[string]jev.Answer{
		"ctx_0": {Type: "score", Score: 0.4, Confidence: 0.99, Probabilities: map[string]float64{"1": 0.8, "2": 0.15, "3": 0.05}},
	}}
	section, listed := formatJevPrebrief(resp, []jevCandidate{{Path: "a.go"}})
	if section != "" || listed != 0 {
		t.Fatalf("section=%q listed=%d, want empty", section, listed)
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

// TestBuildJevQuestions pins the question battery shape: one score per
// candidate, keyed stably.
func TestBuildJevQuestions(t *testing.T) {
	t.Parallel()
	candidates := []jevCandidate{{Path: "a.go"}, {Path: "b.go"}}
	questions := buildJevQuestions(candidates)
	if len(questions) != 2 {
		t.Fatalf("questions = %d, want 2", len(questions))
	}
	for i := range candidates {
		q, ok := questions[fmt.Sprintf("ctx_%d", i)]
		if !ok || q.Type != "score" {
			t.Errorf("candidate %d missing or not a score", i)
		}
	}
}

// TestReviewStep_JevPrebriefRereviewDigestsFixerWork pins that a rereview's
// Jev state digests the worktree against the base, so the fixer's changes are
// part of what the ranking judges, and that it carries no reviewer-written
// findings text.
func TestReviewStep_JevPrebriefRereviewDigestsFixerWork(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupJevRepo(t)
	fake := &fakeJevClient{answer: highScoreEverything}
	ag := &mockAgent{
		name: "test",
		runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if opts.Purpose == "review-fix" {
				if err := os.WriteFile(filepath.Join(dir, "widget", "widget.go"), []byte("package widget\n\nfunc RenderWidget() string {\n\treturn \"fixed\"\n}\n\nfunc RenderWidgetV2() string {\n\treturn RenderWidget()\n}\n"), 0o644); err != nil {
					t.Error(err)
				}
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
	encoded, err := json.Marshal(fake.state)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fake.state.Change.Diff, `"fixed"`) {
		t.Fatalf("rereview digest misses the fixer's change:\n%s", fake.state.Change.Diff)
	}
	if strings.Contains(string(encoded), "broke its callers") {
		t.Fatal("rereview state carries the outstanding findings text")
	}
}

// TestReviewStep_JevPrebriefDigestSkipsIgnoredPaths reproduces a state
// carrying ignored and unchanged content: files matching ignore_patterns stay
// out of the diff, the stat, and the candidates sent to Jev, and unchanged
// candidates are sent as paths without their content.
func TestReviewStep_JevPrebriefDigestSkipsIgnoredPaths(t *testing.T) {
	t.Parallel()
	dir, baseSHA, _ := setupJevRepo(t)
	if err := os.MkdirAll(filepath.Join(dir, "dist"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "dist", "bundle.js"), []byte("function Bundled() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "add bundle")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	fake := &fakeJevClient{answer: highScoreEverything}
	ag := &mockAgent{
		name: "test",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			return cleanReviewResult(t, dir, baseSHA), nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Jev.ReviewAssist = true
	sctx.Config.IgnorePatterns = []string{"dist/**", "fixtures/**", "*_pb.go"}

	if _, err := (&ReviewStep{jev: fake}).Execute(sctx); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if fake.state == nil {
		t.Fatal("jev was not consulted")
	}
	for name, text := range map[string]string{"diff": fake.state.Change.Diff, "stat": fake.state.Change.DiffStat} {
		if strings.Contains(text, "dist/bundle.js") {
			t.Errorf("digest %s carries the ignored file:\n%s", name, text)
		}
		if !strings.Contains(text, "widget/widget.go") {
			t.Errorf("digest %s misses the reviewable file:\n%s", name, text)
		}
	}
	paths := map[string]bool{}
	for _, c := range fake.state.Candidates {
		paths[c.Path] = true
	}
	if !paths["widget/user.go"] || !paths["widget/style.go"] {
		t.Errorf("candidates miss the reviewable use site or sibling: %v", paths)
	}
	for _, ignored := range []string{"fixtures/payload.json", "widget/widget_pb.go"} {
		if paths[ignored] {
			t.Errorf("candidates carry the ignored file %s", ignored)
		}
	}
	encoded, err := json.Marshal(fake.state)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "func Show()") {
		t.Errorf("state carries content of the unchanged candidate widget/user.go:\n%s", encoded)
	}
}
