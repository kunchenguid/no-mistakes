package testartifact

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Every value below is synthetic. The shapes are what matters: each one is a
// class of value the contract must keep out of anything publishable.
const (
	syntheticSecret   = "bootstrap password: Synth3tic-Not-A-Real-Secret-0000"
	syntheticHomePath = "/home/example-user/project/data/live.db"
	syntheticEndpoint = "Server ready at http://localhost:43210"
	syntheticPID      = "worker pid=48213 started"
	syntheticRunID    = "run 01ARZ3NDEKTSV4RRFFQ69G5FAV finished"
	syntheticUUID     = "session 123e4567-e89b-42d3-a456-426614174000 opened"
)

var syntheticLogLines = []string{
	"Migrating database",
	syntheticHomePath,
	syntheticEndpoint,
	syntheticSecret,
	syntheticPID,
	syntheticRunID,
	syntheticUUID,
	"WARN slow query",
	"ERROR request failed",
	"Shutdown complete",
}

func syntheticLogFile(t *testing.T) (string, string) {
	t.Helper()
	body := strings.Join(syntheticLogLines, "\n") + "\n"
	path := filepath.Join(t.TempDir(), "server.log")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path, body
}

func allowAll(p string) string { return p }

func TestValidate_KindsAndPostures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		artifact types.TestArtifact
		want     []string
	}{
		{name: "log with local path", artifact: types.TestArtifact{Kind: "log", Label: "Server log", Path: "/evidence/server.log"}},
		{name: "legacy log carrying content is coerced not rejected", artifact: types.TestArtifact{Kind: "log", Label: "Server log", Path: "/evidence/server.log", Content: syntheticSecret}},
		{name: "legacy command-output alias", artifact: types.TestArtifact{Kind: "command-output", Label: "CLI", Path: "/evidence/cli.txt"}},
		{name: "kind spelling is case-insensitive", artifact: types.TestArtifact{Kind: " Log ", Label: "Server log", Path: "/evidence/server.log"}},
		{name: "ui screenshot", artifact: types.TestArtifact{Kind: "image", Shows: "ui", Label: "Checkout", Path: "/evidence/checkout.png"}},
		{name: "legacy screenshot alias with chart", artifact: types.TestArtifact{Kind: "screenshot", Shows: "chart", Label: "Latency chart", Path: "/evidence/latency.png"}},
		{name: "terminal video is a valid declaration", artifact: types.TestArtifact{Kind: "video", Shows: "terminal", Label: "CLI session", Path: "/evidence/cli.mp4"}},
		{name: "short clean quote", artifact: types.TestArtifact{Kind: "text-quote", Label: "Rendered heading", Content: "## Testing\n\n- Evidence captured"}},
		{name: "missing kind", artifact: types.TestArtifact{Label: "Thing", Path: "/evidence/x"}, want: []string{"artifact 1: missing kind"}},
		{name: "unknown kind", artifact: types.TestArtifact{Kind: "other", Label: "Thing", Path: "/evidence/x"}, want: []string{`artifact 1: kind "other" is not one of log, text-quote, image, video`}},
		{name: "legacy log with only inline content is coerced not rejected", artifact: types.TestArtifact{Kind: "log", Label: "Server log", Content: syntheticSecret}},
		{name: "shows on a log", artifact: types.TestArtifact{Kind: "log", Shows: "ui", Label: "Server log", Path: "/evidence/server.log"}, want: []string{"artifact 1: shows applies only to image and video"}},
		{name: "media without shows", artifact: types.TestArtifact{Kind: "image", Label: "Shot", Path: "/evidence/shot.png"}, want: []string{"artifact 1: image missing shows"}},
		{name: "media with unknown shows", artifact: types.TestArtifact{Kind: "image", Shows: "logs", Label: "Shot", Path: "/evidence/shot.png"}, want: []string{`artifact 1: shows "logs" is not one of ui, chart, terminal`}},
		{name: "media without a file or url", artifact: types.TestArtifact{Kind: "video", Shows: "ui", Label: "Rec"}, want: []string{"artifact 1: video needs a path or url"}},
		{name: "media with inline content", artifact: types.TestArtifact{Kind: "image", Shows: "ui", Label: "Shot", Path: "/evidence/shot.png", Content: "caption"}, want: []string{"artifact 1: image carries no inline content"}},
		{name: "quote without content", artifact: types.TestArtifact{Kind: "text-quote", Label: "Quote"}, want: []string{"artifact 1: text-quote missing content"}},
		{name: "quote with a path", artifact: types.TestArtifact{Kind: "text-quote", Label: "Quote", Content: "ok", Path: "/evidence/x.txt"}, want: []string{"artifact 1: text-quote is published from content alone"}},
		{name: "quote over the cap", artifact: types.TestArtifact{Kind: "text-quote", Label: "Quote", Content: strings.Repeat("a", MaxQuoteBytes+1)}, want: []string{"artifact 1: text-quote content is longer than"}},
		{name: "quote over the line cap", artifact: types.TestArtifact{Kind: "text-quote", Label: "Quote", Content: strings.Repeat("a\n", MaxQuoteLines+1)}, want: []string{"artifact 1: text-quote content is longer than"}},
		{name: "quote with a JSON credential", artifact: types.TestArtifact{Kind: "text-quote", Label: "Quote", Content: `{"password":"short"}`}, want: []string{"credential"}},
		{name: "quote with a control character", artifact: types.TestArtifact{Kind: "text-quote", Label: "Quote", Content: "ok\x1b[31mred"}, want: []string{"artifact 1: text-quote content contains control characters"}},
		{name: "quote with a code fence", artifact: types.TestArtifact{Kind: "text-quote", Label: "Quote", Content: "```text\nok\n```"}, want: []string{"PR structure (markdown-fence)"}},
		{name: "quote with a fold heading", artifact: types.TestArtifact{Kind: "text-quote", Label: "Quote", Content: "ok\n### Pipeline"}, want: []string{"PR structure (fold-marker)"}},
		{name: "quote with a details fold", artifact: types.TestArtifact{Kind: "text-quote", Label: "Quote", Content: "<details>\nok"}, want: []string{"PR structure (fold-marker)"}},
		{name: "quote with an attestation", artifact: types.TestArtifact{Kind: "text-quote", Label: "Quote", Content: "<!-- no-mistakes-pipeline-attestation:v1 {} -->"}, want: []string{"PR structure (attestation-marker)"}},
		{name: "quote with an appendix marker", artifact: types.TestArtifact{Kind: "text-quote", Label: "Quote", Content: "<!-- /no-mistakes-pr-appendix:v1 -->"}, want: []string{"PR structure (appendix-marker)"}},
		{name: "quote with a closing reference", artifact: types.TestArtifact{Kind: "text-quote", Label: "Quote", Content: "Closes #42"}, want: []string{"PR structure (closing-reference)"}},
		{name: "quote with a tabbed closing reference", artifact: types.TestArtifact{Kind: "text-quote", Label: "Quote", Content: "Fixes\t#42"}, want: []string{"PR structure (closing-reference)"}},
		{name: "quote with a secret label", artifact: types.TestArtifact{Kind: "text-quote", Label: "Quote", Content: syntheticSecret}, want: []string{"artifact 1: text-quote content contains value-shaped text (", "credential"}},
		{name: "quote with a home path", artifact: types.TestArtifact{Kind: "text-quote", Label: "Quote", Content: syntheticHomePath}, want: []string{"absolute-path"}},
		{name: "quote with a custom rooted path", artifact: types.TestArtifact{Kind: "text-quote", Label: "Quote", Content: "Saved /custom/acme/private/report.txt"}, want: []string{"absolute-path"}},
		{name: "quote with a forward-slash Windows path", artifact: types.TestArtifact{Kind: "text-quote", Label: "Quote", Content: "Saved C:/Users/alice/private/report.txt"}, want: []string{"absolute-path"}},
		{name: "quote with a patient identifier", artifact: types.TestArtifact{Kind: "text-quote", Label: "Quote", Content: "patient_id=12345"}, want: []string{"private-identifier"}},
		{name: "quote with an endpoint", artifact: types.TestArtifact{Kind: "text-quote", Label: "Quote", Content: syntheticEndpoint}, want: []string{"endpoint"}},
		{name: "quote with a pid", artifact: types.TestArtifact{Kind: "text-quote", Label: "Quote", Content: syntheticPID}, want: []string{"process-id"}},
		{name: "quote with a run id", artifact: types.TestArtifact{Kind: "text-quote", Label: "Quote", Content: syntheticRunID}, want: []string{"run-id"}},
		{name: "quote with a uuid", artifact: types.TestArtifact{Kind: "text-quote", Label: "Quote", Content: syntheticUUID}, want: []string{"uuid"}},
		{name: "media label with an endpoint", artifact: types.TestArtifact{Kind: "image", Shows: "ui", Label: "Page at localhost:3000", Path: "/evidence/a.png"}, want: []string{"artifact 1: label contains value-shaped text (endpoint)"}},
		{name: "media label with a forward-slash Windows path", artifact: types.TestArtifact{Kind: "image", Shows: "ui", Label: "Saved C:/Users/alice/private/report.txt", Path: "/evidence/a.png"}, want: []string{"artifact 1: label contains value-shaped text (absolute-path)"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			issues := Validate([]types.TestArtifact{tt.artifact})
			if len(tt.want) == 0 {
				if len(issues) != 0 {
					t.Fatalf("Validate() = %q, want no issues", issues)
				}
				return
			}
			joined := strings.Join(issues, "\n")
			for _, want := range tt.want {
				if !strings.Contains(joined, want) {
					t.Fatalf("Validate() = %q, want an issue containing %q", issues, want)
				}
			}
		})
	}
}

// A rejection names the hazard CLASS and never echoes the matched value: the
// issue text is logged and handed back to the agent, so echoing the value
// would copy it into two more places.
func TestValidate_IssuesNeverEchoTheValue(t *testing.T) {
	t.Parallel()
	issues := Validate([]types.TestArtifact{
		{Kind: "text-quote", Label: "Quote", Content: syntheticSecret + "\n" + syntheticHomePath + "\n" + syntheticEndpoint},
		{Kind: "image", Shows: "ui", Label: "Shot of 01ARZ3NDEKTSV4RRFFQ69G5FAV", Path: "/evidence/a.png"},
	})
	if len(issues) == 0 {
		t.Fatal("expected issues")
	}
	joined := strings.Join(issues, "\n")
	for _, value := range []string{"Synth3tic", "example-user", "43210", "01ARZ3NDEKTSV4RRFFQ69G5FAV"} {
		if strings.Contains(joined, value) {
			t.Fatalf("issue text echoed %q:\n%s", value, joined)
		}
	}
}

func TestCanonicalize_OneRepresentationAfterValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   types.TestArtifact
		want types.TestArtifact
	}{
		{
			name: "legacy log keeps only its local file",
			in:   types.TestArtifact{Kind: "LOG", Label: "Server log", Path: "/evidence/server.log", URL: "https://example.com/log.txt", Content: syntheticSecret, Shows: "ui"},
			want: types.TestArtifact{Kind: KindLog, Label: "Server log", Path: "/evidence/server.log"},
		},
		{
			name: "command-output becomes a log",
			in:   types.TestArtifact{Kind: "command-output", Label: "CLI", Content: "output"},
			want: types.TestArtifact{Kind: KindLog, Label: "CLI"},
		},
		{
			name: "unknown stored kind is held to the log posture",
			in:   types.TestArtifact{Kind: "other", Label: "Thing", Path: "/evidence/x.txt", Content: "inline"},
			want: types.TestArtifact{Kind: KindLog, Label: "Thing", Path: "/evidence/x.txt"},
		},
		{
			name: "missing stored kind is held to the log posture",
			in:   types.TestArtifact{Label: "Thing", Path: "/evidence/x.png"},
			want: types.TestArtifact{Kind: KindLog, Label: "Thing", Path: "/evidence/x.png"},
		},
		{
			name: "screenshot alias becomes image and keeps its declaration",
			in:   types.TestArtifact{Kind: "screenshot", Shows: " UI ", Label: "Shot", Path: "/evidence/a.png", Content: "caption"},
			want: types.TestArtifact{Kind: KindImage, Shows: ShowsUI, Label: "Shot", Path: "/evidence/a.png"},
		},
		{
			name: "recording alias becomes video",
			in:   types.TestArtifact{Kind: "recording", Shows: "chart", Label: "Rec", URL: "https://example.com/a.mp4"},
			want: types.TestArtifact{Kind: KindVideo, Shows: ShowsChart, Label: "Rec", URL: "https://example.com/a.mp4"},
		},
		{
			name: "quote keeps only its normalized content",
			in:   types.TestArtifact{Kind: "text-quote", Label: "Quote", Content: "\n ok \r\n\r\n", Path: "/evidence/x", URL: "https://example.com", Shows: "ui"},
			want: types.TestArtifact{Kind: KindTextQuote, Label: "Quote", Content: "\n ok \n\n"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := Canonicalize(tt.in); got != tt.want {
				t.Fatalf("Canonicalize() = %+v, want %+v", got, tt.want)
			}
			if again := Canonicalize(Canonicalize(tt.in)); again != tt.want {
				t.Fatalf("Canonicalize is not idempotent: %+v", again)
			}
		})
	}
}

func TestSummarizeLog_CountsAreDeterministic(t *testing.T) {
	t.Parallel()
	path, body := syntheticLogFile(t)
	first, ok := SummarizeLog(path)
	if !ok {
		t.Fatal("SummarizeLog() refused a readable text log")
	}
	second, _ := SummarizeLog(path)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("summaries differ across runs: %+v vs %+v", first, second)
	}
	want := LogSummary{
		Lines:   int64(len(syntheticLogLines)),
		Bytes:   int64(len(body)),
		Outcome: OutcomeComplete,
		Phases: []PhaseCount{
			{Phase: PhaseReady, Lines: 2},
			{Phase: PhaseError, Lines: 1},
			{Phase: PhaseWarning, Lines: 1},
			{Phase: PhaseShutdown, Lines: 1},
		},
	}
	if !reflect.DeepEqual(first, want) {
		t.Fatalf("SummarizeLog() = %+v, want %+v", first, want)
	}
}

func TestSummarizeLog_IsBounded(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "big.log")
	line := "ERROR synthetic line\n"
	repeats := MaxLogScanBytes/len(line) + 100
	if err := os.WriteFile(path, []byte(strings.Repeat(line, repeats)), 0o644); err != nil {
		t.Fatal(err)
	}
	got, ok := SummarizeLog(path)
	if !ok {
		t.Fatal("SummarizeLog() refused a large text log")
	}
	if got.Outcome != OutcomeTruncated {
		t.Fatalf("Outcome = %q, want %q", got.Outcome, OutcomeTruncated)
	}
	if got.Bytes != int64(repeats*len(line)) {
		t.Fatalf("Bytes = %d, want the whole file size %d", got.Bytes, repeats*len(line))
	}
	if got.Lines > int64(MaxLogScanBytes/len(line))+1 {
		t.Fatalf("Lines = %d counted past the scan bound", got.Lines)
	}
}

func TestSummarizeLog_RefusesWhatItCannotSummarize(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	binary := filepath.Join(dir, "capture.bin")
	if err := os.WriteFile(binary, []byte{0x89, 'P', 'N', 'G', 0x00, 0x01}, 0o644); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"binary":    binary,
		"missing":   filepath.Join(dir, "gone.log"),
		"directory": dir,
		"empty":     "",
	} {
		if got, ok := SummarizeLog(path); ok {
			t.Fatalf("%s: SummarizeLog() = %+v, want refusal", name, got)
		}
	}
}

func TestPublish_LogExposesOnlyTheMachineSummary(t *testing.T) {
	t.Parallel()
	path, body := syntheticLogFile(t)
	artifacts := []types.TestArtifact{{
		Kind:    "log",
		Label:   "Patient Jane Example visit log " + syntheticEndpoint,
		Path:    path,
		URL:     "https://example.com/" + syntheticPID,
		Content: syntheticSecret,
	}}
	pubs := PublishAll(artifacts, allowAll)
	if len(pubs) != 1 {
		t.Fatalf("PublishAll() = %d publications, want 1", len(pubs))
	}
	pub := pubs[0]
	if pub.Kind != KindLog || pub.Label != "Log 1" || pub.Log == nil || pub.Media != nil || pub.Quote != "" || pub.Withheld != "" {
		t.Fatalf("log publication = %+v, want only a machine-labelled summary", pub)
	}
	encoded, err := json.Marshal(pub)
	if err != nil {
		t.Fatal(err)
	}
	assertNoSyntheticValue(t, string(encoded))
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		if strings.Contains(string(encoded), line) {
			t.Fatalf("publication carries a raw log line %q: %s", line, encoded)
		}
	}
	if strings.Contains(string(encoded), "Jane") || strings.Contains(string(encoded), path) {
		t.Fatalf("publication carries agent label text or the local path: %s", encoded)
	}
}

func TestPublish_LegacyLogRecordsSummarize(t *testing.T) {
	t.Parallel()
	path, _ := syntheticLogFile(t)
	// The shape older agents returned: a log with no posture beyond its kind,
	// often with an inline copy of the output as well.
	raw := `{"findings":[],"summary":"","artifacts":[` +
		`{"kind":"log","label":"Server log","path":` + jsonString(path) + `,"content":` + jsonString(syntheticSecret) + `},` +
		`{"kind":"log","label":"Inline only","content":` + jsonString(syntheticSecret) + `},` +
		`{"kind":"other","label":"Mystery","path":` + jsonString(path) + `}]}`
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	pubs := PublishAll(findings.Artifacts, allowAll)
	if len(pubs) != 2 {
		t.Fatalf("PublishAll() = %+v, want the two file-backed records summarized and the inline-only one dropped", pubs)
	}
	for i, pub := range pubs {
		if pub.Kind != KindLog || pub.Log == nil {
			t.Fatalf("publication %d = %+v, want a log summary", i, pub)
		}
	}
	if pubs[0].Label != "Log 1" || pubs[1].Label != "Log 2" {
		t.Fatalf("labels = %q, %q, want machine numbering", pubs[0].Label, pubs[1].Label)
	}
	encoded, _ := json.Marshal(pubs)
	assertNoSyntheticValue(t, string(encoded))
}

func TestPublish_LogWithoutAReadableLocalFilePublishesNothing(t *testing.T) {
	t.Parallel()
	path, _ := syntheticLogFile(t)
	refuse := func(string) string { return "" }
	if pubs := PublishAll([]types.TestArtifact{{Kind: "log", Label: "Server log", Path: path}}, refuse); len(pubs) != 0 {
		t.Fatalf("PublishAll() = %+v, want nothing when the path is outside the allowed roots", pubs)
	}
	if pubs := PublishAll([]types.TestArtifact{{Kind: "log", Label: "Server log", Path: path}}, nil); len(pubs) != 0 {
		t.Fatalf("PublishAll() = %+v, want nothing without a resolver", pubs)
	}
}

func TestPublish_MediaPostures(t *testing.T) {
	t.Parallel()
	artifacts := []types.TestArtifact{
		{Kind: "image", Shows: "ui", Label: "Checkout page", Path: "/evidence/checkout.png", Content: syntheticSecret},
		{Kind: "video", Shows: "chart", Label: "Latency chart", URL: "https://example.com/latency.mp4"},
		{Kind: "image", Shows: "terminal", Label: "Boot log screenshot " + syntheticEndpoint, Path: "/evidence/boot.png"},
		{Kind: "screenshot", Label: "Undeclared legacy shot", Path: "/evidence/legacy.png"},
		{Kind: "image", Shows: "ui", Label: "Shot of " + syntheticRunID, Path: "/evidence/ok.png"},
		{Kind: "image", Shows: "ui", Label: "Saved C:/Users/alice/private/report.txt", Path: "/evidence/path.png"},
	}
	pubs := PublishAll(artifacts, allowAll)
	if len(pubs) != len(artifacts) {
		t.Fatalf("PublishAll() = %+v", pubs)
	}
	if pubs[0].Media == nil || pubs[0].Media.Path != "/evidence/checkout.png" || pubs[0].Media.Shows != ShowsUI || pubs[0].Label != "Checkout page" || pubs[0].Quote != "" {
		t.Fatalf("ui image = %+v, want published media without its stray content", pubs[0])
	}
	if pubs[1].Media == nil || pubs[1].Media.URL != "https://example.com/latency.mp4" || pubs[1].Kind != KindVideo {
		t.Fatalf("chart video = %+v, want published media", pubs[1])
	}
	if pubs[2].Media != nil || pubs[2].Withheld != WithheldTerminal || pubs[2].Label != "Image 2" {
		t.Fatalf("terminal image = %+v, want withheld under a machine label", pubs[2])
	}
	if pubs[3].Media != nil || pubs[3].Withheld != WithheldUndeclared || pubs[3].Label != "Image 3" {
		t.Fatalf("undeclared legacy image = %+v, want withheld", pubs[3])
	}
	if pubs[4].Media == nil || pubs[4].Label != "Image 4" {
		t.Fatalf("ui image with a value-shaped label = %+v, want media under a machine label", pubs[4])
	}
	if pubs[5].Media == nil || pubs[5].Label != "Image 5" {
		t.Fatalf("ui image with a forward-slash Windows path label = %+v, want media under a machine label", pubs[5])
	}
	encoded, _ := json.Marshal(pubs)
	assertNoSyntheticValue(t, string(encoded))
}

func TestPublish_QuotesAreCappedAndScannedAtTheBoundary(t *testing.T) {
	t.Parallel()
	artifacts := []types.TestArtifact{
		{Kind: "text-quote", Label: "Rendered heading", Content: "## Testing\n\n- Evidence captured"},
		{Kind: "text-quote", Label: "Stored before the scan", Content: "ok\n" + syntheticSecret},
		{Kind: "text-quote", Label: "Too long", Content: strings.Repeat("x", MaxQuoteBytes+1)},
	}
	artifacts = append(artifacts,
		types.TestArtifact{Kind: "text-quote", Label: "Spacing", Content: "{\"message\":\"Hello   world\"}\r\n\tindented"},
		types.TestArtifact{Kind: "text-quote", Label: "Control", Content: "ok\x1b[31mred"},
		types.TestArtifact{Kind: "text-quote", Label: "Indented", Content: "\n  \n    indented first line\n  second  \n\n"},
	)
	pubs := PublishAll(artifacts, allowAll)
	if len(pubs) != 6 {
		t.Fatalf("PublishAll() = %+v", pubs)
	}
	if pubs[5].Quote != "\n  \n    indented first line\n  second  \n\n" {
		t.Fatalf("indented quote = %q, want every byte kept apart from line endings", pubs[5].Quote)
	}
	if pubs[3].Quote != "{\"message\":\"Hello   world\"}\n\tindented" {
		t.Fatalf("quote was rewritten: %q, want it verbatim apart from line endings", pubs[3].Quote)
	}
	if pubs[4].Withheld != WithheldQuote {
		t.Fatalf("quote with a control character = %+v, want withheld", pubs[4])
	}
	pubs = pubs[:3]
	if pubs[0].Quote != "## Testing\n\n- Evidence captured" || pubs[0].Label != "Rendered heading" {
		t.Fatalf("clean quote = %+v", pubs[0])
	}
	for _, pub := range pubs[1:] {
		if pub.Quote != "" || pub.Withheld != WithheldQuote {
			t.Fatalf("refused quote = %+v, want withheld", pub)
		}
	}
	if pubs[1].Label != "Quote 2" || pubs[2].Label != "Quote 3" {
		t.Fatalf("refused quote labels = %q, %q, want machine labels", pubs[1].Label, pubs[2].Label)
	}
	encoded, _ := json.Marshal(pubs)
	assertNoSyntheticValue(t, string(encoded))
}

func TestPublish_StoredQuotesWithPRStructureAreWithheld(t *testing.T) {
	t.Parallel()
	artifacts := []types.TestArtifact{
		{Kind: KindTextQuote, Content: "```text\nok\n```"},
		{Kind: KindTextQuote, Content: "ok\n### Pipeline"},
		{Kind: KindTextQuote, Content: "<details>\nok"},
		{Kind: KindTextQuote, Content: "<!-- no-mistakes-pipeline-attestation:v1 {} -->"},
		{Kind: KindTextQuote, Content: "<!-- no-mistakes-pr-appendix:v1 sha256=x -->"},
		{Kind: KindTextQuote, Content: "Closes #42"},
		{Kind: KindTextQuote, Content: "Fixes\t#42"},
		{Kind: KindTextQuote, Content: "Saved /custom/acme/private/report.txt"},
		{Kind: KindTextQuote, Content: "Saved C:/Users/alice/private/report.txt"},
		{Kind: KindTextQuote, Content: "patient_id=12345"},
	}
	pubs := PublishAll(artifacts, allowAll)
	if len(pubs) != len(artifacts) {
		t.Fatalf("PublishAll() returned %d publications, want %d", len(pubs), len(artifacts))
	}
	for _, pub := range pubs {
		if pub.Quote != "" || pub.Withheld != WithheldQuote {
			t.Fatalf("structural quote = %+v, want withheld", pub)
		}
	}
}

// Fields the contract does not define never reach the publication, whatever
// the agent names them: decoding into the typed record drops them, and the
// publication is built only from typed fields.
func TestPublish_AgentCannotSmuggleExtraFields(t *testing.T) {
	t.Parallel()
	path, _ := syntheticLogFile(t)
	raw := `{"findings":[],"summary":"","artifacts":[{"kind":"log","label":"Server log","path":` + jsonString(path) +
		`,"summary":` + jsonString(syntheticSecret) +
		`,"excerpt":` + jsonString(syntheticEndpoint) +
		`,"lines":` + jsonString(syntheticHomePath) +
		`,"phases":{"ready":` + jsonString(syntheticPID) + `}` +
		`,"publish":"inline","raw":` + jsonString(syntheticUUID) + `}]}`
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(PublishAll(findings.Artifacts, allowAll))
	assertNoSyntheticValue(t, string(encoded))
}

// The publication's shape is the renderer boundary. A new string field here is
// a new channel to the pull request, so it must be added deliberately.
func TestPublication_ShapeIsPinned(t *testing.T) {
	t.Parallel()
	pinned := map[reflect.Type][]string{
		reflect.TypeOf(Publication{}): {"Kind", "Label", "Log", "Quote", "Media", "Withheld"},
		reflect.TypeOf(LogSummary{}):  {"Lines", "Bytes", "Outcome", "Phases"},
		reflect.TypeOf(PhaseCount{}):  {"Phase", "Lines"},
		reflect.TypeOf(Media{}):       {"Path", "URL", "Shows"},
	}
	for typ, want := range pinned {
		var got []string
		for i := 0; i < typ.NumField(); i++ {
			got = append(got, typ.Field(i).Name)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s fields = %v, want %v", typ.Name(), got, want)
		}
	}
}

func assertNoSyntheticValue(t *testing.T, published string) {
	t.Helper()
	for _, value := range []string{
		"Synth3tic", "bootstrap password", "example-user", "/home/", "localhost", "43210",
		"48213", "01ARZ3NDEKTSV4RRFFQ69G5FAV", "123e4567-e89b-42d3-a456-426614174000",
	} {
		if strings.Contains(published, value) {
			t.Fatalf("publication carries %q:\n%s", value, published)
		}
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestHazards_ClassesArePinned(t *testing.T) {
	t.Parallel()
	tests := []struct {
		text string
		want []string
	}{
		{"POST /checkout 200", nil},
		{"## Testing\n\n- Evidence captured", nil},
		{"handler.go:42 returned early", nil},
		{"3 passed, 0 failed in 12:30", nil},
		{`{"button":{"top":169,"left":248}}`, nil},
		{"token refresh succeeded", nil},
		{"TestPRStep_ForeignAttestationsInEveryComponentDoNotShadowTheRealOne", nil},
		{"api_key=synthetic", []string{HazardCredential}},
		{`{"password":"short"}`, []string{HazardCredential}},
		{`{'api_key': 'x'}`, []string{HazardCredential}},
		{`{"token" : "x"}`, []string{HazardCredential}},
		{`{"tokens_used": 3}`, nil},
		{"DB_PASSWORD=short-secret", []string{HazardCredential}},
		{"MY_API_KEY: x", []string{HazardCredential}},
		{"database:/srv/private/report.txt", []string{HazardAbsolutePath}},
		{"Authorization: Bearer synthetic", []string{HazardCredential}},
		{"-----BEGIN OPENSSH PRIVATE KEY-----", []string{HazardCredential}},
		{"opened /home/example-user/notes.txt", []string{HazardAbsolutePath}},
		{"saved /custom/acme/private/report.txt", []string{HazardAbsolutePath}},
		{"opened /Users/example/notes.txt", []string{HazardAbsolutePath}},
		{`opened C:\Users\example\notes.txt`, []string{HazardAbsolutePath}},
		{"opened C:/Users/example/notes.txt", []string{HazardAbsolutePath}},
		{"opened ~/notes.txt", []string{HazardAbsolutePath}},
		{"listening on 127.0.0.1", []string{HazardEndpoint}},
		{"listening on [::1]", []string{HazardEndpoint}},
		{"see https://example.com/report", []string{HazardEndpoint}},
		{"dial db.internal:5432", []string{HazardEndpoint}},
		{"PID 4242 exited", []string{HazardProcessID}},
		{"run 01ARZ3NDEKTSV4RRFFQ69G5FAV", []string{HazardRunID, HazardToken}},
		{"0123456789abcdef0123456789abcdef", []string{HazardHexToken, HazardToken}},
		{"cookie value c2VjcmV0LXN5bnRoZXRpYy12YWx1ZTEyMzQ", []string{HazardToken}},
		{"mail example.person@example.com", []string{HazardPrivateIdentifier}},
		{"PatNum 12 DOB unknown", []string{HazardPrivateIdentifier}},
		{"patient_id=12345", []string{HazardPrivateIdentifier}},
		{`{"user-id":"abc-123"}`, []string{HazardPrivateIdentifier}},
	}
	for _, tt := range tests {
		if got := Hazards(tt.text); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("Hazards(%q) = %v, want %v", tt.text, got, tt.want)
		}
	}
}
