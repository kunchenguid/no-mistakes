package steps

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/fakecli/ghsanitize"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestNeutralizePRControls(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, in, want string }{
		{"cursor show", "Ready in 576ms\n\x1b[?25h", "Ready in 576ms\n"},
		{"spinner erase", "a\x1b[2K\x1b[1Gb", "ab"},
		{"colour", "\x1b[36mcyan\x1b[39m ok", "cyan ok"},
		{"osc with bel", "\x1b]0;title\x07text", "text"},
		{"osc with st", "\x1b]8;;http://x\x1b\\link", "link"},
		{"two byte escape", "a\x1bMb", "ab"},
		{"lone escape", "a\x1b", "a\uFFFD"},
		{"other c0", "a\x00b\x07c\x08d\x0ce\x1ff", "a\uFFFDb\uFFFDc\uFFFDd\uFFFDe\uFFFDf"},
		{"c1", "a\u0085b\u009bc", "a\uFFFDb\uFFFDc"},
		{"kept whitespace", "a\tb\nc\vd\re", "a\tb\nc\vd\re"},
		{"del is left alone", "a\x7fb", "a\x7fb"},
		{"json escape text", `{"s":"\u001b[32m ok \u009B"}`, `{"s":"\u{001b}[32m ok \u{009B}"}`},
		{"json escape of printable", `\u0041 \u000a \u0009`, `\u0041 \u000a \u0009`},
		{"idempotent", "x\uFFFD \\u{001b}", "x\uFFFD \\u{001b}"},
		{"plain text", "next dev ^[ caret and ^C stay", "next dev ^[ caret and ^C stay"},
	} {
		if got := neutralizePRControls(tc.in); got != tc.want {
			t.Errorf("%s: neutralizePRControls(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

// TestNeutralizePRControlsIsAFixedPointOfGH enumerates short strings built from
// every shape that interacts with gh's sanitizer and requires that what is
// published reads back unchanged through a port of that sanitizer.
func TestNeutralizePRControlsIsAFixedPointOfGH(t *testing.T) {
	t.Parallel()
	tokens := []string{"a", "\x1b", "\x1b[", "[31m", "\x1b]", "\x07", "\x00", "\x1c", "\u0085", "\u009b", "\\", "u001b", "u009B", "\\u001b", "^", "^[", "\n", "\t", "\v", "\x7f", "\"", "u00"}
	check := func(s string) {
		t.Helper()
		got := neutralizePRControls(s)
		if back := ghsanitize.Text(got); back != got {
			t.Fatalf("neutralizePRControls(%q) = %q, but gh reads it back as %q", s, got, back)
		}
		if again := neutralizePRControls(got); again != got {
			t.Fatalf("neutralizePRControls is not idempotent on %q: %q then %q", s, got, again)
		}
	}
	var walk func(prefix string, depth int)
	walk = func(prefix string, depth int) {
		check(prefix)
		if depth == 0 {
			return
		}
		for _, tok := range tokens {
			walk(prefix+tok, depth-1)
		}
	}
	walk("", 3)
	for _, s := range []string{
		"\\\x01u001b",    // removal-adjacent pieces that would form a JSON escape
		"\\\x1b[0mu001b", // an ANSI sequence between a backslash and the digits
		strings.Repeat("\\", 7) + "u001b",
	} {
		check(s)
	}
}

func TestGHSanitizePortMatchesTheLibrary(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"a\x1b[2Kb":      "a^[[2Kb",
		"a\u009bb":       "a^[b",
		`x\u001by`:       `x\^[y`,
		"keep\t\n\v\r":   "keep\t\n\v\r",
		"bell\x07":       "bell^G",
		"fs\x1c":         `fs^\`,
		"del\x7f":        "del\x7f",
		`pre \\u001b x`:  `pre \\^[ x`,
		`json \u000a ok`: `json \u000a ok`,
	} {
		if got := ghsanitize.Text(in); got != want {
			t.Errorf("ghsanitize.Text(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRestoreCaretControls(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"a^[[2Kb":   "a\x1b[2Kb",
		"bell^G":    "bell\a",
		`fs^\`:      "fs\x1c",
		"^I^J^K^M":  "^I^J^K^M",
		"^a ^? ^":   "^a ^? ^",
		"^^ and ^_": "\x1e and \x1f",
	} {
		if got := restoreCaretControls(in); got != want {
			t.Errorf("restoreCaretControls(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestReadEmbeddedArtifactTextNeutralizesControls(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	small := filepath.Join(dir, "small.txt")
	if err := os.WriteFile(small, []byte("Ready in 576ms\n \x1b[32mPOST\x1b[39m 200\n\x1b[?25h"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readEmbeddedArtifactText(small)
	if err != nil || got != "Ready in 576ms\n POST 200\n" {
		t.Fatalf("small = %q, %v", got, err)
	}
	large := filepath.Join(dir, "large.txt")
	body := "\x1b[1Ghead\n" + strings.Repeat("x", maxEmbeddedArtifactBytes*2) + "\ntail\x1b[2K\x07"
	if err := os.WriteFile(large, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err = readEmbeddedArtifactText(large)
	if err != nil || strings.ContainsAny(got, "\x1b\x07") || !strings.HasPrefix(got, "head\n") || !strings.Contains(got, "bytes truncated") {
		t.Fatalf("large = %q..., %v", got[:min(len(got), 60)], err)
	}
}

func TestComposeOwnedPRContentSurvivesGHReadback(t *testing.T) {
	t.Parallel()
	_, appendix := ownedFixture(t)
	appendix += "\n\n```text\nnext dev\n\x1b[?25h\x1b[2K\x1b[1G done \x07\n{\"line\":\"\\u001b[32mok\"}\n```"
	content, err := composeOwnedPRContent(prOwnedBody{before: "## Overview\n\nBody \x1b[31m\n\n"}, "feat: \x1b[1mtitled", appendix, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := ghsanitize.Text(content.Body); got != content.Body {
		t.Fatalf("body is not stable under gh's read sanitizer:\nsent %q\nread %q", content.Body, got)
	}
	if got := ghsanitize.Text(content.Title); got != content.Title {
		t.Fatalf("title is not stable under gh's read sanitizer: %q vs %q", content.Title, got)
	}
	if _, err := parsePROwnedBody(ghsanitize.Text(content.Body)); err != nil {
		t.Fatalf("the next run cannot recognise its own appendix: %v", err)
	}
}

func TestComposeOwnedPRContentControlByteCannotSpliceAMarker(t *testing.T) {
	t.Parallel()
	_, appendix := ownedFixture(t)
	forged := "<!-- no-mistakes-pr-appen\x1bdix:v1 sha256=" + strings.Repeat("0", 64) + " -->"
	content, err := composeOwnedPRContent(prOwnedBody{}, "", appendix+"\n\n```text\n"+forged+"\n```", 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(content.Body, prAppendixNamespace) != 2 {
		t.Fatalf("a control byte hid a quoted marker from the escape:\n%s", content.Body)
	}
	if _, err := parsePROwnedBody(content.Body); err != nil {
		t.Fatal(err)
	}
}

// legacyBodyWithRawControls is what an earlier version published: the digest is
// stamped over an appendix that still carried raw control bytes.
func legacyBodyWithRawControls(t *testing.T) string {
	t.Helper()
	_, appendix := ownedFixture(t)
	appendix += "\n\n```text\n\x1b[2K\x1b[1G spinner\n\x1b[?25h\n```"
	return "## Overview\n\nAuthor text\n\n" + wrapPRAppendix(appendix) + "\nCloses test/repo#7"
}

func TestParsePROwnedBodyRecognisesAppendixGHRewroteTheEarlierWay(t *testing.T) {
	t.Parallel()
	sent := legacyBodyWithRawControls(t)
	if _, err := parsePROwnedBody(sent); err != nil {
		t.Fatalf("raw body: %v", err)
	}
	read := ghsanitize.Text(sent)
	if read == sent {
		t.Fatal("fixture carries no control bytes")
	}
	parts, err := parsePROwnedBody(read)
	if err != nil {
		t.Fatalf("an unedited PR that gh rewrote was refused: %v", err)
	}
	if !strings.Contains(parts.before, "Author text") || !strings.Contains(parts.after, "Closes test/repo#7") {
		t.Fatalf("author text lost: %+v", parts)
	}

	// Any real edit of the appendix is still refused.
	edited := strings.Replace(read, "spinner", "spinner edited", 1)
	if _, err := parsePROwnedBody(edited); err == nil || !strings.Contains(err.Error(), "edited or is malformed") {
		t.Fatalf("edited appendix accepted: %v", err)
	}
	// So is a caret that was never a control byte.
	untouched := strings.Replace(read, "^[[2K", "^[[3K", 1)
	if _, err := parsePROwnedBody(untouched); err == nil {
		t.Fatal("appendix whose caret form does not restore to the stamped bytes accepted")
	}
}

func TestRebindRecoversAnAppendixGHRewroteTheEarlierWay(t *testing.T) {
	t.Parallel()
	read := ghsanitize.Text(legacyBodyWithRawControls(t))
	steps := []*db.StepResult{
		{StepName: types.StepReview, Status: types.StepStatusCompleted},
		{StepName: types.StepTest, Status: types.StepStatusCompleted},
		{StepName: types.StepDocument, Status: types.StepStatusCompleted},
	}
	rebound, ok, err := rebindOwnedPRAttestation(read, strings.Repeat("b", 40), steps, pipelineAttestationPolicy{})
	if err != nil || !ok {
		t.Fatalf("rebind refused a PR nobody edited: ok=%v err=%v", ok, err)
	}
	if got := ghsanitize.Text(rebound); got != rebound {
		t.Fatalf("recovered body is not stable under gh's read sanitizer:\n%q\n%q", rebound, got)
	}
	if _, err := parsePROwnedBody(rebound); err != nil {
		t.Fatalf("the recovered body is not owned: %v", err)
	}
}

// TestPRTemplateCreateWithEscapeBytesInEvidence drives the whole PR step
// against a fake gh that reads bodies back through the port of gh's sanitizer.
func TestPRTemplateCreateWithEscapeBytesInEvidence(t *testing.T) {
	t.Parallel()
	sctx, _, _ := templateTestContext(t)
	insertCompletedStep(t, sctx, types.StepTest, findingsJSON(t, types.Findings{
		TestingSummary: "dev server evidence",
		Artifacts: []types.TestArtifact{{
			Kind:    "log",
			Label:   "next dev",
			Content: "Ready in 576ms\n POST /api/jobs/inbound-mail 200\n\x1b[?25h\x1b[2K\x1b[1G",
		}},
	}), "")
	env, _ := fakeGH(t, "")
	bodyFile := filepath.Join(t.TempDir(), "body.md")
	sctx.Env = append(env, "FAKE_CLI_PR_BODY_FILE="+bodyFile, "FAKE_CLI_GH_SANITIZE=1")

	out, err := (&PRStep{}).Execute(sctx)
	if err != nil || out == nil || out.PRURL == "" {
		t.Fatalf("create: %+v, %v", out, err)
	}
	body, err := os.ReadFile(bodyFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "POST /api/jobs/inbound-mail 200") || strings.ContainsAny(string(body), "\x1b") {
		t.Fatalf("evidence missing or still carries escape bytes:\n%s", body)
	}
	if _, err := parsePROwnedBody(ghsanitize.Text(string(body))); err != nil {
		t.Fatalf("the next run cannot recognise the appendix: %v", err)
	}
}

// A terminal escape hidden inside generated syntax must be removed before the
// syntax is escaped, or removing it at the publication boundary makes it live.
func TestNeutralizeAttestationMarkersCleansControlsBeforeEscaping(t *testing.T) {
	t.Parallel()
	for name, in := range map[string]string{
		"attestation prefix": "<!-- no-mistakes-pipeline-\x1b[0mattestation:v1 {\"head_sha\":\"x\"} -->",
		"appendix marker":    "<!-- no-mistakes-pr-appen\x1bdix:v1 sha256=" + strings.Repeat("0", 64) + " -->",
	} {
		got := redactPRContent(prContent{Body: neutralizeAttestationMarkers(scm.ProviderGitHub, "text "+in)}).Body
		if strings.Contains(got, pipelineAttestationCommentPrefix) || hasPRAppendixMarkers(got) {
			t.Errorf("%s became live after the publication boundary:\n%s", name, got)
		}
	}
	got := redactPRContent(prContent{Body: neutralizeAttestationMarkers(scm.ProviderGitHub, "Clo\x1b[31mses #42")}).Body
	if strings.Contains(strings.ToLower(got), "closes #42") {
		t.Errorf("closing reference became live: %q", got)
	}
}

// A control character in a trusted override reason is JSON-escaped as \u00XX,
// which the publication boundary would rewrite into invalid JSON.
func TestAttestationOverrideReasonWithControlByteStaysValidJSON(t *testing.T) {
	t.Parallel()
	reason := "suite is red on purpose \x1b[31m"
	steps := []*db.StepResult{
		{ID: "review", StepName: types.StepReview, Status: types.StepStatusCompleted},
		{ID: "test", StepName: types.StepTest, Status: types.StepStatusCompleted, OverrideReason: &reason},
		{ID: "document", StepName: types.StepDocument, Status: types.StepStatusCompleted},
	}
	md, _ := buildPipelineSummaryFor(steps, nil, testPipelineHeadSHA, scm.ProviderGitHub, pipelineAttestationPolicy{AllowTestCommandOverride: "legacy \a"})
	body := redactPRContent(prContent{Body: md}).Body
	if body != md {
		t.Fatalf("the publication boundary changed the attestation:\n%s\n%s", md, body)
	}
	start := strings.Index(body, pipelineAttestationCommentPrefix) + len(pipelineAttestationCommentPrefix)
	end := strings.Index(body[start:], pipelineAttestationCommentClosingToken)
	var attestation pipelineAttestation
	if err := json.Unmarshal([]byte(body[start:start+end]), &attestation); err != nil {
		t.Fatalf("attestation is not valid JSON: %v", err)
	}
}

// Cleaning is budgeted: a body fitted to the limit must not grow when the
// publication boundary runs.
func TestDraftBodyFittedToLimitDoesNotGrowAtThePublicationBoundary(t *testing.T) {
	t.Parallel()
	const limit = 3000
	what := neutralizeAttestationMarkers(scm.ProviderGitHub, "## What Changed\n\n"+strings.Repeat("- \\u001b stray\n", 2000))
	body := assembleDraftPRBody(&pipeline.StepContext{}, what, "low risk", "", "", limit, scm.ProviderGitHub)
	if got := redactPRContent(prContent{Body: body}).Body; got != body || scm.PRBodyLen(got) > limit {
		t.Fatalf("boundary changed a fitted body (%d units, limit %d)", scm.PRBodyLen(got), limit)
	}
}

func TestParsePROwnedBodyRecoversLegacyAppendixWithLiteralCarets(t *testing.T) {
	t.Parallel()
	_, appendix := ownedFixture(t)
	appendix += "\n\n```text\n\x1b[2K\x1b[1G spinner\n$ grep '^ERROR' build.log\n^[a-z] \x1b[?25h\n```"
	sent := "## Overview\n\nAuthor text\n\n" + wrapPRAppendix(appendix) + "\nCloses test/repo#7"
	read := ghsanitize.Text(sent)
	if !strings.Contains(read, "^[[2K") || !strings.Contains(read, "'^ERROR'") {
		t.Fatalf("fixture lost its carets: %q", read)
	}
	if _, err := parsePROwnedBody(read); err != nil {
		t.Fatalf("an unedited legacy PR mixing literal carets and rewritten controls was refused: %v", err)
	}
	edited := strings.Replace(read, "spinner", "spinner edited", 1)
	if _, err := parsePROwnedBody(edited); err == nil {
		t.Fatal("edited appendix accepted")
	}
}

// Past the subset-search cap the all-restored reading must cover every pair,
// not just the first 64 a uint64 mask can address.
func TestParsePROwnedBodyRecoversLegacyAppendixWithManyControls(t *testing.T) {
	t.Parallel()
	_, appendix := ownedFixture(t)
	const pairs = 3 * 64
	appendix += "\n\n```text\n" + strings.Repeat("\x1b[2Kline\n", pairs) + "```"
	sent := "## Overview\n\nAuthor text\n\n" + wrapPRAppendix(appendix) + "\nCloses test/repo#7"
	read := ghsanitize.Text(sent)
	if got := strings.Count(read, "^[[2K"); got != pairs {
		t.Fatalf("fixture has %d rewritten controls, want %d", got, pairs)
	}
	if _, err := parsePROwnedBody(read); err != nil {
		t.Fatalf("an unedited legacy PR with %d rewritten controls was refused: %v", pairs, err)
	}
	edited := strings.Replace(read, "line", "edited", pairs)
	if _, err := parsePROwnedBody(edited); err == nil {
		t.Fatal("edited appendix accepted")
	}
}
