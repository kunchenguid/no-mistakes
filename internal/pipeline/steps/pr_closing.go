package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/closingissues"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/scm/github"
)

// The pipeline adds closing references to a PR body from exactly one place:
// explicit `axi run --closes` values, persisted on the run and claimed once by
// the PR step (sctx.ClosingIssueRefs). They render in one stable `## Issues`
// section. An author-preserving (pr.template) body keeps its author text
// verbatim, so only the references the author text does not already close
// are added to its generated appendix. Closure is never inferred from intent,
// commits, or branch names: pipeline-generated text is published with its
// closing references neutralized (neutralizeClosingReferences).
//
// An ordinary update replaces the whole body, so it carries the AUTHOR's own
// standalone closing lines over into the Issues section
// (sctx.CarriedClosingLines) instead of dropping them (issue #763). Which
// lines are the author's is never guessed from headings: the body records
// every closing line the pipeline itself published (the --closes lines and
// anything its own text left that the extractor reads as a closing line) in a
// hidden ledger comment (closingLedgerPrefix, sealClosingLedger), so every
// other closing line in a body carrying that ledger appeared after
// publication and is the author's. See authorClosingLines for bodies without
// a ledger.

const issuesSectionHeading = "## Issues"

const closingKeywordPattern = `(?:close|closes|closed|fix|fixes|fixed|resolve|resolves|resolved):?\s+(?:#[1-9][0-9]*|[A-Za-z0-9-]+/[A-Za-z0-9._-]+#[1-9][0-9]*)`

// closingKeywordLinePattern matches a line that consists only of GitHub
// closing keywords and their targets, optionally as a bullet or ordered list
// item and with trailing sentence punctuation. A reference inside prose
// ("this fixes #4 partly") is deliberately not counted: it is not a
// standalone closing declaration.
var closingKeywordLinePattern = regexp.MustCompile(`(?i)^(?:(?:[-*+]|[0-9]+[.)])\s+)?` + closingKeywordPattern + `(?:\s*,\s*` + closingKeywordPattern + `)*[.;!]?$`)

var closingReferencePattern = regexp.MustCompile(`(?i)(?:[A-Za-z0-9-]+/[A-Za-z0-9._-]+#[1-9][0-9]*|#[1-9][0-9]*)`)

// extractClosingKeywordLines returns the distinct standalone closing-keyword
// lines of body, outside fenced and indented code blocks.
func extractClosingKeywordLines(body string) []string {
	seen := map[string]struct{}{}
	var lines []string
	var fence markdownFence
	for _, raw := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		inFence := fence.marker != 0
		fence.consume(raw)
		if inFence || fence.marker != 0 || strings.HasPrefix(raw, "\t") || strings.HasPrefix(raw, "    ") {
			continue
		}
		line := strings.TrimSpace(raw)
		if !closingKeywordLinePattern.MatchString(line) {
			continue
		}
		key := strings.ToLower(line)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		lines = append(lines, line)
	}
	return lines
}

var closingReferenceInTextPattern = regexp.MustCompile(`(?i)\b((?:close|closes|closed|fix|fixes|fixed|resolve|resolves|resolved):?\s+)((?:[A-Za-z0-9-]+/[A-Za-z0-9._-]+)?#[1-9][0-9]*|https?://[A-Za-z0-9.-]+(?::[0-9]+)?/[A-Za-z0-9-]+/[A-Za-z0-9._-]+/(?:issues|pull)/[1-9][0-9]*)\b`)

// neutralizeClosingReferences puts every closing-keyword reference in
// pipeline-generated PR text, including an issue or pull request URL, in an
// inline code span ("Fixes `#12`"), outside fenced, indented, and inline code
// and HTML <code>/<pre> elements (a tested command renders as <code>).
// GitHub ignores a reference in code, so nothing the pipeline publishes can
// close an issue; only the Issues section carries live ones.
func neutralizeClosingReferences(s string) string {
	if !closingReferenceInTextPattern.MatchString(s) {
		return s
	}
	lines := strings.Split(s, "\n")
	var fence markdownFence
	var htmlCode string
	for i, raw := range lines {
		inFence := fence.marker != 0
		fence.consume(raw)
		if inFence || fence.marker != 0 || strings.HasPrefix(raw, "\t") || strings.HasPrefix(raw, "    ") {
			continue
		}
		lines[i] = outsideInlineCode(raw, func(text string) string {
			return outsideHTMLCode(text, &htmlCode, func(text string) string {
				return closingReferenceInTextPattern.ReplaceAllString(text, "${1}`${2}`")
			})
		})
	}
	return strings.Join(lines, "\n")
}

var htmlCodeTagPattern = regexp.MustCompile(`(?i)<(/?)(code|pre)\b[^>]*>`)

// outsideHTMLCode applies rewrite to the parts of text outside HTML <code>
// and <pre> elements. open names the element still open from earlier text
// ("" when none), so an element may span lines.
func outsideHTMLCode(text string, open *string, rewrite func(string) string) string {
	var b strings.Builder
	last := 0
	for _, m := range htmlCodeTagPattern.FindAllStringSubmatchIndex(text, -1) {
		closing := m[3] > m[2]
		name := strings.ToLower(text[m[4]:m[5]])
		switch {
		case *open == "" && !closing:
			b.WriteString(rewrite(text[last:m[0]]))
			*open = name
		case *open == name && closing:
			b.WriteString(text[last:m[0]])
			*open = ""
		default:
			continue
		}
		b.WriteString(text[m[0]:m[1]])
		last = m[1]
	}
	if *open == "" {
		b.WriteString(rewrite(text[last:]))
	} else {
		b.WriteString(text[last:])
	}
	return b.String()
}

// outsideInlineCode applies rewrite to the parts of line outside inline code
// spans. A backtick run without a matching closing run is literal text.
func outsideInlineCode(line string, rewrite func(string) string) string {
	var b strings.Builder
	text := 0
	for i := 0; i < len(line); {
		if line[i] != '`' {
			i++
			continue
		}
		n := 1
		for i+n < len(line) && line[i+n] == '`' {
			n++
		}
		closing := -1
		for j := i + n; j < len(line); {
			if line[j] != '`' {
				j++
				continue
			}
			m := 1
			for j+m < len(line) && line[j+m] == '`' {
				m++
			}
			if m == n {
				closing = j
				break
			}
			j += m
		}
		if closing < 0 {
			i += n
			continue
		}
		b.WriteString(rewrite(line[text:i]))
		b.WriteString(line[i : closing+n])
		i = closing + n
		text = i
	}
	b.WriteString(rewrite(line[text:]))
	return b.String()
}

// closingTargets returns the canonical refs ("42", "owner/repo#42") closed
// by the given closing-keyword lines. A reference qualified with repo, the
// PR's own repository, is the bare number.
func closingTargets(lines []string, repo string) map[string]struct{} {
	targets := map[string]struct{}{}
	for _, line := range lines {
		for _, target := range closingReferencePattern.FindAllString(line, -1) {
			target = closingissues.Localize(strings.TrimPrefix(target, "#"), repo)
			targets[strings.ToLower(target)] = struct{}{}
		}
	}
	return targets
}

// prRepository returns the owner/repository the PR lives in, or "" when it
// is unknown.
func prRepository(sctx *pipeline.StepContext) string {
	if sctx == nil {
		return ""
	}
	if sctx.Repo != nil {
		if repo := github.RepoSlug(sctx.Repo.UpstreamURL); repo != "" {
			return repo
		}
	}
	if sctx.Run != nil && sctx.Run.PRURL != nil {
		return github.RepoSlug(*sctx.Run.PRURL)
	}
	return ""
}

func closingLine(ref string) string {
	return "Closes " + closingissues.Target(ref)
}

// requestedClosingLines returns one Closes line per requested ref that is not
// already closed by an existing line (live author text kept around the body,
// or carried author lines), so each reference appears exactly once.
func requestedClosingLines(sctx *pipeline.StepContext, existing []string) []string {
	if sctx == nil {
		return nil
	}
	var lines []string
	present := closingTargets(existing, prRepository(sctx))
	for _, ref := range sctx.ClosingIssueRefs {
		key := strings.ToLower(ref)
		if _, exists := present[key]; exists {
			continue
		}
		present[key] = struct{}{}
		lines = append(lines, closingLine(ref))
	}
	return lines
}

func renderIssuesSection(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return issuesSectionHeading + "\n\n" + strings.Join(lines, "\n")
}

// issuesSection renders the stable Issues section for an author-preserving
// (pr.template) appendix, or "" when there is nothing to render. Requested
// refs already closed by authorText (live author content kept verbatim
// around it) are not repeated.
func issuesSection(sctx *pipeline.StepContext, authorText string) string {
	return renderIssuesSection(requestedClosingLines(sctx, extractClosingKeywordLines(authorText)))
}

// closingLedgerPrefix opens the hidden record of every closing line an
// ordinary body was published with, except carried author lines. It is pipeline
// bookkeeping, not evidence of authorship: it only lets the next ordinary
// update tell those lines apart from closing lines the author added.
const (
	closingLedgerPrefix = "<!-- no-mistakes-closing-lines:v1 "
	closingLedgerSuffix = " -->"
)

var closingLedgerMarkerPattern = regexp.MustCompile(`(?i)<!--\s*no-mistakes-closing-lines`)

// escapeClosingLedgerMarkers breaks copies of the ledger marker in
// pipeline-generated text (for example a quoted PR body in Test evidence), so
// only the ledger the pipeline appends can be parsed back.
func escapeClosingLedgerMarkers(s string) string {
	return closingLedgerMarkerPattern.ReplaceAllStringFunc(s, func(marker string) string {
		i := strings.LastIndex(marker, "-")
		return marker[:i] + "\\" + marker[i:]
	})
}

func renderClosingLedger(lines []string) string {
	if lines == nil {
		lines = []string{}
	}
	data, _ := json.Marshal(lines)
	return closingLedgerPrefix + string(data) + closingLedgerSuffix
}

// parseClosingLedger returns the lines recorded by the body's ledger. ok is
// false unless exactly one well-formed ledger is present.
func parseClosingLedger(body string) (lines []string, ok bool) {
	if len(closingLedgerMarkerPattern.FindAllStringIndex(body, -1)) != 1 {
		return nil, false
	}
	start := strings.Index(body, closingLedgerPrefix)
	if start < 0 {
		return nil, false
	}
	rest := body[start+len(closingLedgerPrefix):]
	end := strings.Index(rest, closingLedgerSuffix)
	if end < 0 || json.Unmarshal([]byte(rest[:end]), &lines) != nil {
		return nil, false
	}
	return lines, true
}

// authorClosingLines returns the standalone closing lines of a live ordinary
// body that are the author's, for an update about to replace that body:
//   - a body with a ledger: every live closing line the ledger does not list;
//   - a body the pipeline never published (no ledger, attestation, or
//     signature): every live closing line;
//   - anything else (a body an older no-mistakes published before the ledger
//     existed, or one with an ambiguous ledger): none, as before, because its
//     generated text may carry live closing lines that are not the author's.
func authorClosingLines(body string) []string {
	live := extractClosingKeywordLines(body)
	if len(live) == 0 {
		return nil
	}
	if recorded, ok := parseClosingLedger(body); ok {
		pipelineOwned := make(map[string]struct{}, len(recorded))
		for _, line := range recorded {
			pipelineOwned[strings.ToLower(strings.TrimSpace(line))] = struct{}{}
		}
		var author []string
		for _, line := range live {
			if _, owned := pipelineOwned[strings.ToLower(line)]; !owned {
				author = append(author, line)
			}
		}
		return author
	}
	if closingLedgerMarkerPattern.MatchString(body) || strings.Contains(body, pipelineAttestationCommentPrefix) || strings.Contains(body, noMistakesPRSignature) {
		return nil
	}
	return live
}

// ordinaryIssuesBlock renders the Issues section of an ordinary body: carried
// author lines first, then requested refs they do not already close.
func ordinaryIssuesBlock(sctx *pipeline.StepContext) string {
	var carried []string
	if sctx != nil {
		carried = sctx.CarriedClosingLines
	}
	requested := requestedClosingLines(sctx, carried)
	return renderIssuesSection(append(append([]string(nil), carried...), requested...))
}

// carriesClosingLines reports whether an ordinary body on provider carries
// the ledger and so author closing lines over: forges that render HTML
// comments and have no body-size limit that could shed the ledger.
func carriesClosingLines(provider scm.Provider) bool {
	return prBodyFlavorFor(provider) == prBodyHTML && scm.MaxPRBodyChars(provider) == 0
}

// closingLedgerReserveBytes is room kept free when an ordinary body is fitted
// to the size cap for ledger entries beyond the requested lines.
const closingLedgerReserveBytes = 1024

// sealClosingLedger appends the ledger to a final ordinary body about to be
// published: every closing line the extractor sees in it except the carried
// author lines. Whatever construct a pipeline-published line sat in, the next
// update reads it with the same extractor and finds it listed, so only lines
// that appeared after publication are carried as the author's.
func sealClosingLedger(sctx *pipeline.StepContext, provider scm.Provider, body string) (string, error) {
	if !carriesClosingLines(provider) {
		return body, nil
	}
	carried := make(map[string]struct{}, len(sctx.CarriedClosingLines))
	for _, line := range sctx.CarriedClosingLines {
		carried[strings.ToLower(line)] = struct{}{}
	}
	published := []string{}
	for _, line := range extractClosingKeywordLines(body) {
		if _, ok := carried[strings.ToLower(line)]; !ok {
			published = append(published, line)
		}
	}
	body = appendIssuesSection(body, renderClosingLedger(published))
	if len(body) > maxPullRequestBodyBytes {
		return "", fmt.Errorf("render closing issues: PR body exceeds the size limit after recording its closing lines")
	}
	return body, nil
}

// refreshCarriedClosingLines re-derives the author's closing lines from a
// live body re-read just before the write and, when they changed while the
// update was drafting, replaces body's trailing Issues block. The re-read is
// authoritative both ways: lines added since the first read are carried, and
// lines the author removed are not written back.
func refreshCarriedClosingLines(sctx *pipeline.StepContext, body, latestBody string, bodyLimit int) (string, error) {
	latest := authorClosingLines(latestBody)
	if sameClosingLines(sctx.CarriedClosingLines, latest) {
		return body, nil
	}
	previous := ordinaryIssuesBlock(sctx)
	if previous != "" {
		if !strings.HasSuffix(body, previous) {
			return "", fmt.Errorf("verify closing issues: cannot locate the Issues section to reconcile closing lines changed while the update was drafting")
		}
		body = strings.TrimRight(strings.TrimSuffix(body, previous), "\n")
	}
	sctx.CarriedClosingLines = latest
	body = appendIssuesSection(body, ordinaryIssuesBlock(sctx))
	if len(body) > maxPullRequestBodyBytes || (bodyLimit > 0 && scm.PRBodyLen(body) > bodyLimit) {
		return "", fmt.Errorf("verify closing issues: PR body exceeds provider budget after carrying over the author's closing lines")
	}
	return body, nil
}

func sameClosingLines(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !strings.EqualFold(a[i], b[i]) {
			return false
		}
	}
	return true
}

// appendIssuesSection appends section to body as its final block.
func appendIssuesSection(body, section string) string {
	if section == "" {
		return body
	}
	if strings.TrimSpace(body) == "" {
		return section
	}
	return body + "\n\n" + section
}

// claimClosingIssueRefs samples the run's --closes references for this PR
// body. The claim is what makes a late reattach (`axi run --closes` against a
// run already composing its PR) fail closed instead of reporting a reference
// that never reaches the body.
func claimClosingIssueRefs(sctx *pipeline.StepContext, host scm.Host, provider scm.Provider) error {
	if sctx.DB == nil {
		return nil
	}
	refs, err := sctx.DB.ClaimClosingIssueRefsForPRBody(sctx.Run.ID)
	if err != nil {
		return fmt.Errorf("resolve closing issue references: %w", err)
	}
	// The PR's own repository names an issue one way, so `95` and
	// `owner/repo#95` render once.
	repo := prRepository(sctx)
	for i, ref := range refs {
		refs[i] = closingissues.Localize(ref, repo)
	}
	refs, err = closingissues.Normalize(refs)
	if err != nil {
		return fmt.Errorf("resolve closing issue references: %w", err)
	}
	sctx.ClosingIssueRefs = refs
	if len(refs) == 0 {
		return nil
	}
	if provider != scm.ProviderGitHub {
		return fmt.Errorf("render closing issues: --closes currently supports GitHub repositories only")
	}
	if _, ok := host.(scm.PRContentReader); !ok {
		return fmt.Errorf("verify closing issues: provider cannot read the current pull request body")
	}
	return nil
}

// refuseSkipWithClosingIssues fails a PR step that would be skipped while the
// run carries --closes references: skipping publishes no body that closes
// them. The claim makes a concurrent reattach fail closed too.
func refuseSkipWithClosingIssues(sctx *pipeline.StepContext, reason string) error {
	if sctx.DB == nil {
		return nil
	}
	refs, err := sctx.DB.ClaimClosingIssueRefsForPRBody(sctx.Run.ID)
	if err != nil {
		return fmt.Errorf("resolve closing issue references: %w", err)
	}
	if len(refs) == 0 {
		return nil
	}
	return fmt.Errorf("render closing issues: --closes requires publishing a pull request, but PR creation is unavailable: %s", reason)
}

// verifyClosingIssues re-reads the live PR body and fails unless it still
// carries every requested reference and every carried author closing line. A
// missing reference is invisible at review time (the issue simply stays open
// after merge), so the step must not report success without this check.
func verifyClosingIssues(ctx context.Context, host scm.Host, pr *scm.PR, sctx *pipeline.StepContext) error {
	if sctx == nil || len(sctx.ClosingIssueRefs) == 0 && len(sctx.CarriedClosingLines) == 0 {
		return nil
	}
	reader, ok := host.(scm.PRContentReader)
	if !ok {
		return fmt.Errorf("verify closing issues: provider cannot read the current pull request body")
	}
	if pr == nil {
		return fmt.Errorf("verify closing issues: pull request identity is unavailable")
	}
	content, err := reader.GetPRContent(ctx, pr)
	if err != nil {
		return fmt.Errorf("verify closing issues: %w", err)
	}
	return verifyClosingIssuesInBody(content.Body, sctx)
}

func verifyClosingIssuesInBody(body string, sctx *pipeline.StepContext) error {
	if sctx == nil {
		return nil
	}
	present := extractClosingKeywordLines(body)
	presentLines := make(map[string]struct{}, len(present))
	for _, line := range present {
		presentLines[strings.ToLower(line)] = struct{}{}
	}
	for _, line := range sctx.CarriedClosingLines {
		if _, ok := presentLines[strings.ToLower(line)]; !ok {
			return fmt.Errorf("verify closing issues: pull request body dropped the author's closing line %q", line)
		}
	}
	targets := closingTargets(present, prRepository(sctx))
	for _, ref := range sctx.ClosingIssueRefs {
		if _, ok := targets[strings.ToLower(ref)]; !ok {
			return fmt.Errorf("verify closing issues: pull request body is missing %s", closingLine(ref))
		}
	}
	return nil
}

// assembleDraftPRBody assembles an ordinary drafted body and appends the
// Issues section last, reserving its room up front so body-limit truncation
// sheds generated evidence rather than a closing reference.
func assembleDraftPRBody(sctx *pipeline.StepContext, whatChanged, riskLine, testingMD, pipelineMD string, bodyLimit int, provider scm.Provider) string {
	section := ordinaryIssuesBlock(sctx)
	reserve := 0
	if section != "" {
		reserve = len("\n\n" + section)
	}
	if carriesClosingLines(provider) {
		reserve += len("\n\n"+renderClosingLedger(requestedClosingLines(sctx, nil))) + closingLedgerReserveBytes
	}
	if bodyLimit > 0 {
		if section != "" {
			reserve = scm.PRBodyLen("\n\n" + section)
		}
		if bodyLimit-reserve <= 0 {
			// The section alone does not fit. Omit it rather than pass a
			// non-positive (unlimited) budget; the pre-publication check then
			// refuses a body that would drop a closing reference.
			return assemblePRBody(sctx, whatChanged, riskLine, testingMD, pipelineMD, bodyLimit, provider)
		}
		return appendIssuesSection(assemblePRBody(sctx, whatChanged, riskLine, testingMD, pipelineMD, bodyLimit-reserve, provider), section)
	}
	return appendIssuesSection(buildPRBodyWithin(whatChanged, riskLine, testingMD, pipelineMD, sctx, provider, maxPullRequestBodyBytes-reserve), section)
}
