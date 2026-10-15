// Package testartifact owns the Test step's typed evidence contract: which
// artifact kinds exist, what each kind may publish, and the one conversion
// from an agent's artifact (or a stored record of one) to what a pull request
// may render.
//
// The contract has three postures and no others:
//
//   - log: a local file. Its bytes are never shown in the pull request; only a
//     machine-derived summary appears there (an inert label, line and byte
//     counts, a scan outcome, and counts of a fixed set of phases). Its path,
//     agent label, and inline content are also omitted from the pull request.
//   - text-quote: a short excerpt the agent chose to show inline. It is capped
//     and scanned for value-shaped text, and refused (never trimmed or
//     rewritten) when either check fails.
//   - image / video: media that declares what it shows (ui, chart, or
//     terminal). Only ui and chart media are shown in the pull request; a
//     terminal capture is a picture of a log and is withheld from the PR.
//
// Validate is the contract as the agent sees it: an unknown kind or a
// malformed posture is a structured rejection the analyzer correction turn
// can repair. Canonicalize is the one representation a validated or stored
// record takes. PublishAll is the renderer boundary: it accepts any record,
// including ones written before this contract existed, and returns only
// typed Publication values, so a field the contract does not define has no
// way into a pull request.
package testartifact

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Artifact kinds. These are the only kinds a Test agent may report.
const (
	KindLog       = "log"
	KindTextQuote = "text-quote"
	KindImage     = "image"
	KindVideo     = "video"
)

// What an image or video shows. Only ui and chart media are published.
const (
	ShowsUI       = "ui"
	ShowsChart    = "chart"
	ShowsTerminal = "terminal"
)

// Log summary scan outcomes.
const (
	// OutcomeComplete means the counts cover the whole file.
	OutcomeComplete = "complete"
	// OutcomeTruncated means the file is larger than MaxLogScanBytes and the
	// line and phase counts cover only its first MaxLogScanBytes.
	OutcomeTruncated = "truncated"
)

// The approved log phases, in the order a summary reports them.
const (
	PhaseReady    = "ready"
	PhaseError    = "error"
	PhaseWarning  = "warning"
	PhaseShutdown = "shutdown"
)

// Why an artifact is listed without its content.
const (
	WithheldTerminal   = "terminal capture"
	WithheldUndeclared = "content not declared"
	WithheldQuote      = "quote refused by the publication check"
)

const (
	// MaxQuoteBytes and MaxQuoteLines cap a text-quote. Longer output is a
	// log: keep the file and let the summary describe it.
	MaxQuoteBytes = 1024
	MaxQuoteLines = 20
	// MaxLabelRunes caps a published label.
	MaxLabelRunes = 120
	// MaxLogScanBytes bounds how much of a log file a summary reads.
	MaxLogScanBytes = 4 << 20
)

var (
	knownKinds = []string{KindLog, KindTextQuote, KindImage, KindVideo}
	knownShows = []string{ShowsUI, ShowsChart, ShowsTerminal}

	// legacyKinds are the kinds the pre-contract schema suggested. They map
	// onto the contract rather than being rejected, so an older agent's
	// spelling keeps working; "command-output" was text to inline and now
	// takes the log posture, which can only make it less public.
	legacyKinds = map[string]string{
		"screenshot":     KindImage,
		"gif":            KindImage,
		"recording":      KindVideo,
		"command-output": KindLog,
	}

	machineLabelNoun = map[string]string{
		KindLog:       "Log",
		KindTextQuote: "Quote",
		KindImage:     "Image",
		KindVideo:     "Video",
	}

	tokenShape = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)
)

// LogSummary is everything a log publishes.
type LogSummary struct {
	Lines   int64
	Bytes   int64
	Outcome string
	Phases  []PhaseCount
}

// PhaseCount is how many lines of a log matched one approved phase.
type PhaseCount struct {
	Phase string
	Lines int64
}

// Media is a publishable image or video: a local evidence file to upload or
// link, or an http(s) URL the agent reported.
type Media struct {
	Path  string
	URL   string
	Shows string
}

// Publication is the renderer boundary: the only artifact shape a pull
// request is built from. Exactly one of Log, Quote, Media, or Withheld is set.
type Publication struct {
	Kind string
	// Label is safe to publish: the agent's label for published media and
	// quotes when it passes the publication check, otherwise a machine label
	// such as "Log 1". A log is always machine-labelled.
	Label    string
	Log      *LogSummary
	Quote    string
	Media    *Media
	Withheld string
}

// NormalizeKind maps an agent's or a stored record's kind onto the contract.
// known is false for an empty or unrecognized kind.
func NormalizeKind(raw string) (kind string, known bool) {
	kind = strings.ToLower(strings.TrimSpace(raw))
	if mapped, ok := legacyKinds[kind]; ok {
		return mapped, true
	}
	return kind, slices.Contains(knownKinds, kind)
}

func normalizeShows(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

func isMedia(kind string) bool { return kind == KindImage || kind == KindVideo }

// Validate checks an agent's artifacts against the contract and returns one
// issue per violation, in artifact order. An issue names the problem and,
// for value-shaped text, the hazard class - never the matched text.
func Validate(artifacts []types.TestArtifact) []string {
	var issues []string
	for i, a := range artifacts {
		issues = append(issues, artifactIssues(i+1, a)...)
	}
	return issues
}

func artifactIssues(n int, a types.TestArtifact) []string {
	if strings.TrimSpace(a.Kind) == "" {
		return []string{fmt.Sprintf("artifact %d: missing kind - set kind to %s", n, kindList(", or "))}
	}
	kind, known := NormalizeKind(a.Kind)
	if !known {
		if tokenShape.MatchString(kind) {
			return []string{fmt.Sprintf("artifact %d: kind %q is not one of %s", n, kind, kindList(", "))}
		}
		return []string{fmt.Sprintf("artifact %d: kind is not one of %s", n, kindList(", "))}
	}
	shows := normalizeShows(a.Shows)
	var issues []string
	if !isMedia(kind) && shows != "" {
		issues = append(issues, fmt.Sprintf("artifact %d: shows applies only to image and video", n))
	}
	switch kind {
	case KindLog:
		// An older agent's log carries no posture beyond its kind, often an
		// inline copy of the output, and sometimes no file at all. None of that
		// is rejected: the log posture is summary-only whatever else the record
		// says, so the copy is dropped and a log with no readable local file
		// simply publishes nothing.
		return issues
	case KindTextQuote:
		if strings.TrimSpace(a.Content) == "" {
			issues = append(issues, fmt.Sprintf("artifact %d: text-quote missing content", n))
		}
		if strings.TrimSpace(a.Path) != "" || strings.TrimSpace(a.URL) != "" {
			issues = append(issues, fmt.Sprintf("artifact %d: text-quote is published from content alone - drop path and url, or report the file as a log", n))
		}
		quote := normalizeQuote(a.Content)
		if hasControlCharacters(quote) {
			issues = append(issues, fmt.Sprintf("artifact %d: text-quote content contains control characters - quote plain text only", n))
		}
		if quoteTooLong(quote) {
			issues = append(issues, fmt.Sprintf("artifact %d: text-quote content is longer than %d bytes or %d lines - quote less, or report the file as a log", n, MaxQuoteBytes, MaxQuoteLines))
		}
		if classes := Hazards(quote); len(classes) > 0 {
			issues = append(issues, fmt.Sprintf("artifact %d: text-quote content contains value-shaped text (%s) - quote something else, or report the file as a log", n, strings.Join(classes, ", ")))
		}
		if classes := quoteStructureHazards(quote); len(classes) > 0 {
			issues = append(issues, fmt.Sprintf("artifact %d: text-quote content contains PR structure (%s) - quote something else, or report the file as a log", n, strings.Join(classes, ", ")))
		}
	default:
		switch {
		case shows == "":
			issues = append(issues, fmt.Sprintf("artifact %d: %s missing shows - declare what it shows: %s (terminal captures are not shown in the pull request)", n, kind, strings.Join(knownShows, ", ")))
		case !slices.Contains(knownShows, shows):
			if tokenShape.MatchString(shows) {
				issues = append(issues, fmt.Sprintf("artifact %d: shows %q is not one of %s", n, shows, strings.Join(knownShows, ", ")))
			} else {
				issues = append(issues, fmt.Sprintf("artifact %d: shows is not one of %s", n, strings.Join(knownShows, ", ")))
			}
		}
		if strings.TrimSpace(a.Path) == "" && strings.TrimSpace(a.URL) == "" {
			issues = append(issues, fmt.Sprintf("artifact %d: %s needs a path or url", n, kind))
		}
		if strings.TrimSpace(a.Content) != "" {
			issues = append(issues, fmt.Sprintf("artifact %d: %s carries no inline content - put a short description in label", n, kind))
		}
	}
	if utf8.RuneCountInString(strings.TrimSpace(a.Label)) > MaxLabelRunes {
		issues = append(issues, fmt.Sprintf("artifact %d: label is longer than %d characters", n, MaxLabelRunes))
	}
	if classes := Hazards(a.Label); len(classes) > 0 {
		issues = append(issues, fmt.Sprintf("artifact %d: label contains value-shaped text (%s) - describe the evidence without paths, endpoints, or identifiers", n, strings.Join(classes, ", ")))
	}
	return issues
}

func kindList(lastSep string) string {
	return strings.Join(knownKinds[:len(knownKinds)-1], ", ") + lastSep + knownKinds[len(knownKinds)-1]
}

// Canonicalize returns the contract's one representation of a record: the
// kind mapped onto the contract (an unknown or missing kind takes the log
// posture, the least public one), shows normalized and kept only on media,
// and each field kept only where its posture uses it. It is idempotent.
func Canonicalize(a types.TestArtifact) types.TestArtifact {
	kind, known := NormalizeKind(a.Kind)
	if !known {
		kind = KindLog
	}
	out := types.TestArtifact{Kind: kind, Label: a.Label}
	switch kind {
	case KindLog:
		out.Path = a.Path
	case KindTextQuote:
		out.Content = normalizeQuote(a.Content)
	default:
		out.Path = a.Path
		out.URL = a.URL
		out.Shows = normalizeShows(a.Shows)
	}
	return out
}

// CanonicalizeAll applies Canonicalize to every artifact.
func CanonicalizeAll(artifacts []types.TestArtifact) []types.TestArtifact {
	if artifacts == nil {
		return nil
	}
	out := make([]types.TestArtifact, len(artifacts))
	for i, a := range artifacts {
		out[i] = Canonicalize(a)
	}
	return out
}

// PublishAll converts artifacts into what a pull request may render. resolve
// maps a log's recorded path to the local file the summary may read, or ""
// when that path is outside the roots the caller allows; a nil resolve reads
// nothing. A log that cannot be summarized publishes nothing at all.
func PublishAll(artifacts []types.TestArtifact, resolve func(string) string) []Publication {
	counts := map[string]int{}
	var out []Publication
	for _, raw := range artifacts {
		a := Canonicalize(raw)
		next := counts[a.Kind] + 1
		pub, ok := publish(a, next, resolve)
		if !ok {
			continue
		}
		counts[a.Kind] = next
		out = append(out, pub)
	}
	return out
}

func publish(a types.TestArtifact, n int, resolve func(string) string) (Publication, bool) {
	machine := fmt.Sprintf("%s %d", machineLabelNoun[a.Kind], n)
	switch a.Kind {
	case KindLog:
		if resolve == nil || strings.TrimSpace(a.Path) == "" {
			return Publication{}, false
		}
		summary, ok := SummarizeLog(resolve(a.Path))
		if !ok {
			return Publication{}, false
		}
		return Publication{Kind: KindLog, Label: machine, Log: &summary}, true
	case KindTextQuote:
		// The quote is checked and published exactly as recorded, apart from
		// line endings: rewriting it (collapsing whitespace, say) would show
		// different evidence and could shrink an over-cap quote under the cap.
		quote := normalizeQuote(a.Content)
		if quote == "" {
			return Publication{}, false
		}
		if quoteTooLong(quote) || hasControlCharacters(quote) || len(Hazards(quote)) > 0 || len(quoteStructureHazards(quote)) > 0 {
			return Publication{Kind: KindTextQuote, Label: machine, Withheld: WithheldQuote}, true
		}
		return Publication{Kind: KindTextQuote, Label: publishableLabel(a.Label, machine), Quote: quote}, true
	default:
		if strings.TrimSpace(a.Path) == "" && strings.TrimSpace(a.URL) == "" {
			return Publication{}, false
		}
		switch a.Shows {
		case ShowsUI, ShowsChart:
			return Publication{Kind: a.Kind, Label: publishableLabel(a.Label, machine), Media: &Media{Path: a.Path, URL: a.URL, Shows: a.Shows}}, true
		case ShowsTerminal:
			return Publication{Kind: a.Kind, Label: machine, Withheld: WithheldTerminal}, true
		default:
			return Publication{Kind: a.Kind, Label: machine, Withheld: WithheldUndeclared}, true
		}
	}
}

func publishableLabel(label, machine string) string {
	label = strings.TrimSpace(label)
	if label == "" || utf8.RuneCountInString(label) > MaxLabelRunes || len(Hazards(label)) > 0 {
		return machine
	}
	return label
}

// normalizeQuote is the one rewrite a quote gets: CRLF line endings become LF.
func normalizeQuote(content string) string {
	return strings.ReplaceAll(content, "\r\n", "\n")
}

// hasControlCharacters reports a control character other than newline and
// tab. A fence cannot neutralize one, and a lone carriage return or escape
// sequence would change how the published quote reads.
func hasControlCharacters(content string) bool {
	for _, r := range content {
		if r != '\n' && r != '\t' && unicode.IsControl(r) {
			return true
		}
	}
	return false
}

func quoteTooLong(content string) bool {
	content = normalizeQuote(content)
	return len(content) > MaxQuoteBytes || strings.Count(content, "\n")+1 > MaxQuoteLines
}

var phaseMatchers = []struct {
	phase string
	re    *regexp.Regexp
}{
	{PhaseReady, regexp.MustCompile(`(?i)\b(ready|listening|started)\b`)},
	{PhaseError, regexp.MustCompile(`(?i)\b(error|errors|fatal|panic|exception|failed|failure)\b`)},
	{PhaseWarning, regexp.MustCompile(`(?i)\b(warn|warning|warnings)\b`)},
	{PhaseShutdown, regexp.MustCompile(`(?i)\b(shutdown|shutting down|stopped|exited|exiting)\b`)},
}

// SummarizeLog reads at most MaxLogScanBytes of a local text file and counts
// its lines and approved phases. ok is false when there is nothing to
// summarize: no path, not a regular file, unreadable, or binary (a NUL byte).
func SummarizeLog(fsPath string) (summary LogSummary, ok bool) {
	if strings.TrimSpace(fsPath) == "" {
		return LogSummary{}, false
	}
	info, err := os.Stat(fsPath)
	if err != nil || !info.Mode().IsRegular() {
		return LogSummary{}, false
	}
	file, err := os.Open(fsPath)
	if err != nil {
		return LogSummary{}, false
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, MaxLogScanBytes))
	if err != nil && !errors.Is(err, io.EOF) {
		return LogSummary{}, false
	}
	if bytes.IndexByte(data, 0) != -1 {
		return LogSummary{}, false
	}
	summary = LogSummary{Bytes: info.Size(), Outcome: OutcomeComplete}
	truncated := info.Size() > int64(len(data))
	if truncated {
		summary.Outcome = OutcomeTruncated
		// The last line of a cut scan is partial; count only whole lines.
		if cut := bytes.LastIndexByte(data, '\n'); cut >= 0 {
			data = data[:cut+1]
		} else {
			data = nil
		}
	}
	counts := make([]int64, len(phaseMatchers))
	for len(data) > 0 {
		line := data
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			line, data = data[:i], data[i+1:]
		} else {
			data = nil
		}
		summary.Lines++
		for i, m := range phaseMatchers {
			if m.re.Match(line) {
				counts[i]++
			}
		}
	}
	summary.Phases = make([]PhaseCount, len(phaseMatchers))
	for i, m := range phaseMatchers {
		summary.Phases[i] = PhaseCount{Phase: m.phase, Lines: counts[i]}
	}
	return summary, true
}
