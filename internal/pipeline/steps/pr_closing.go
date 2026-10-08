package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"regexp"
	"slices"
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
// closing references over into the Issues section (sctx.CarriedClosingLines)
// instead of dropping them (issue #763). What is live is never guessed from
// Markdown or HTML structure, and whose it is never from headings: the
// published body and the live one are both rendered by GitHub's own Markdown
// renderer (renderedClosingTargets), and the published body records every
// live closing reference the pipeline itself published in a hidden ledger
// comment (closingLedgerPrefix, sealClosingLedger). A live reference in a
// later body beyond those it records is the author's (carriedClosingLines).
// Carry-over is therefore GitHub-only.

const issuesSectionHeading = "## Issues"

// closingURLPattern matches an issue or pull request URL, which GitHub
// honors as a closing target on its own host (closingTargetKey).
const closingURLPattern = `https?://[A-Za-z0-9.-]+(?::[0-9]+)?/[A-Za-z0-9-]+/[A-Za-z0-9._-]+/(?:issues|pull)/[1-9][0-9]*`

const closingKeywordPattern = `(?:close|closes|closed|fix|fixes|fixed|resolve|resolves|resolved):?\s+(?:#[1-9][0-9]*|[A-Za-z0-9-]+/[A-Za-z0-9._-]+#[1-9][0-9]*|` + closingURLPattern + `)`

// closingKeywordLinePattern matches a line that consists only of GitHub
// closing keywords and their targets, optionally as a bullet or ordered list
// item and with trailing sentence punctuation. A reference inside prose
// ("this fixes #4 partly") is deliberately not counted: it is not a
// standalone closing declaration.
var closingKeywordLinePattern = regexp.MustCompile(`(?i)^(?:(?:[-*+]|[0-9]+[.)])\s+)?` + closingKeywordPattern + `(?:\s*,\s*` + closingKeywordPattern + `)*[.;!]?$`)

var closingReferencePattern = regexp.MustCompile(`(?i)(?:` + closingURLPattern + `|[A-Za-z0-9-]+/[A-Za-z0-9._-]+#[1-9][0-9]*|#[1-9][0-9]*)`)

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
// by the given closing-keyword lines (closingTargetKey).
func closingTargets(lines []string, sctx *pipeline.StepContext) map[string]struct{} {
	targets := map[string]struct{}{}
	for _, line := range lines {
		for _, target := range closingReferencePattern.FindAllString(line, -1) {
			if key := closingTargetKey(target, sctx); key != "" {
				targets[key] = struct{}{}
			}
		}
	}
	return targets
}

var closingURLPartsPattern = regexp.MustCompile(`(?i)^https?://([A-Za-z0-9.-]+)(?::[0-9]+)?/([A-Za-z0-9-]+/[A-Za-z0-9._-]+)/(?:issues|pull)/([1-9][0-9]*)$`)

// closingTargetKey is the canonical, case-folded form of a reference target
// ("#42" -> "42", "owner/repo#42"), with the PR's own repository written as
// the bare number. An issue or pull request URL is "owner/repo#42" when it
// is on github.com or the PR's own host, and "" (no target) otherwise.
func closingTargetKey(target string, sctx *pipeline.StepContext) string {
	if m := closingURLPartsPattern.FindStringSubmatch(target); m != nil {
		if host := strings.ToLower(m[1]); host != "github.com" && host != prHost(sctx) {
			return ""
		}
		target = m[2] + "#" + m[3]
	}
	return strings.ToLower(closingissues.Localize(strings.TrimPrefix(target, "#"), prRepository(sctx)))
}

// prHost returns the host the PR lives on, or "" when it is unknown.
func prHost(sctx *pipeline.StepContext) string {
	if sctx == nil {
		return ""
	}
	if sctx.Repo != nil && sctx.Repo.UpstreamURL != "" {
		return scm.ExtractHost(sctx.Repo.UpstreamURL)
	}
	if sctx.Run != nil && sctx.Run.PRURL != nil {
		return scm.ExtractHost(*sctx.Run.PRURL)
	}
	return ""
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
	present := closingTargets(existing, sctx)
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

// closingLedgerPrefix opens the hidden record of the live closing references
// an ordinary body was published with, except those of carried author lines:
// one closingTargetKey per occurrence. It is pipeline bookkeeping, not
// evidence of authorship: it only lets the next ordinary update tell those
// references apart from ones the author added.
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

func renderClosingLedger(targets []string) string {
	if targets == nil {
		targets = []string{}
	}
	data, _ := json.Marshal(targets)
	return closingLedgerPrefix + string(data) + closingLedgerSuffix
}

// parseClosingLedger returns the targets recorded by the body's ledger. ok is
// false unless exactly one well-formed ledger is present.
func parseClosingLedger(body string) (targets []string, ok bool) {
	if len(closingLedgerMarkerPattern.FindAllStringIndex(body, -1)) != 1 {
		return nil, false
	}
	start := strings.Index(body, closingLedgerPrefix)
	if start < 0 {
		return nil, false
	}
	rest := body[start+len(closingLedgerPrefix):]
	end := strings.Index(rest, closingLedgerSuffix)
	if end < 0 || json.Unmarshal([]byte(rest[:end]), &targets) != nil {
		return nil, false
	}
	return targets, true
}

var (
	// liveClosingPattern matches one closing keyword and its reference in
	// rendered text, wherever it sits: GitHub honors one inside prose too.
	// The two are paired only across horizontal whitespace, never a line
	// break, so text in separate rendered blocks is never joined.
	liveClosingPattern = regexp.MustCompile(`(?i)\b(?:close|closes|closed|fix|fixes|fixed|resolve|resolves|resolved):?[ \t]+(` + closingURLPattern + `|(?:[A-Za-z0-9-]+/[A-Za-z0-9._-]+)?#[1-9][0-9]*)\b`)
	// renderedCodePattern matches a rendered code element, whose text GitHub
	// never reads as a closing reference.
	renderedCodePattern = regexp.MustCompile(`(?is)<pre\b[^>]*>.*?</pre>|<code\b[^>]*>.*?</code>`)
	renderedTagPattern  = regexp.MustCompile(`<[^>]*>`)
	// renderedBlockTagPattern matches a rendered block-level tag, which
	// strips to a line break so separate blocks stay apart.
	renderedBlockTagPattern = regexp.MustCompile(`(?i)</?(?:p|li|ul|ol|td|th|tr|table|thead|tbody|tfoot|div|h[1-6]|blockquote|br|hr)\b[^>]*>`)
	// mayReferenceIssuePattern matches whatever a rendered issue reference
	// could come from: `#` before a digit (escaped or as a numeric entity
	// too), the `&num;` entity, or an issue or pull request URL.
	mayReferenceIssuePattern = regexp.MustCompile(`(?i)#[0-9x]|&num;|/(?:issues|pull)/[0-9]`)
)

// closingTargetOccurrences returns the closingTargetKey of every closing
// reference in text, one per occurrence, in order.
func closingTargetOccurrences(text string, sctx *pipeline.StepContext) []string {
	var targets []string
	for _, m := range liveClosingPattern.FindAllStringSubmatch(text, -1) {
		if key := closingTargetKey(m[1], sctx); key != "" {
			targets = append(targets, key)
		}
	}
	return targets
}

// renderedClosingTargets returns the live closing references of body, one
// closingTargetKey per occurrence, as GitHub's own Markdown renderer shows
// them: code elements dropped, block tags turned into line breaks, other
// tags stripped (link text kept), and
// entities unescaped. A body with nothing that could render as an issue
// reference is not sent to the renderer. A failed render fails closed.
func renderedClosingTargets(ctx context.Context, sctx *pipeline.StepContext, host scm.Host, body string) ([]string, error) {
	if !mayReferenceIssuePattern.MatchString(body) {
		return nil, nil
	}
	renderer, ok := host.(scm.MarkdownRenderer)
	if !ok {
		return nil, fmt.Errorf("verify closing issues: provider cannot render the pull request body")
	}
	rendered, err := renderer.RenderMarkdown(ctx, body)
	if err != nil {
		return nil, fmt.Errorf("verify closing issues: render the pull request body: %w", err)
	}
	text := renderedCodePattern.ReplaceAllString(rendered, " ")
	text = renderedBlockTagPattern.ReplaceAllString(text, "\n")
	text = html.UnescapeString(renderedTagPattern.ReplaceAllString(text, ""))
	return closingTargetOccurrences(text, sctx), nil
}

// carriedClosingLines returns the author's closing lines of a live ordinary
// body, for an update about to replace it. The author's references are the
// live ones (renderedClosingTargets) beyond those the body's ledger records,
// counted per target, so an author's own copy of a pipeline-published
// reference is still the author's. A body the pipeline never published (no
// ledger, attestation, or signature) records nothing. A body an older
// no-mistakes published before the ledger existed, or one with an ambiguous
// ledger, carries nothing, as before, because its generated text may hold
// live references that are not the author's.
//
// Each author target is carried once: as the first standalone closing line of
// the body whose targets are all the author's, else (a reference inside
// prose, say) as a canonical Closes line.
func carriedClosingLines(ctx context.Context, sctx *pipeline.StepContext, host scm.Host, body string) ([]string, error) {
	recorded, ok := parseClosingLedger(body)
	if !ok && (closingLedgerMarkerPattern.MatchString(body) || strings.Contains(body, pipelineAttestationCommentPrefix) || strings.Contains(body, noMistakesPRSignature)) {
		return nil, nil
	}
	live, err := renderedClosingTargets(ctx, sctx, host, body)
	if err != nil {
		return nil, err
	}
	unclaimed := make(map[string]int, len(recorded))
	for _, target := range recorded {
		unclaimed[strings.ToLower(target)]++
	}
	author := map[string]bool{}
	var order []string
	for _, target := range live {
		if unclaimed[target] > 0 {
			unclaimed[target]--
			continue
		}
		if _, seen := author[target]; !seen {
			author[target] = true
			order = append(order, target)
		}
	}
	var lines []string
	for _, line := range extractClosingKeywordLines(body) {
		targets := closingTargetOccurrences(line, sctx)
		if len(targets) == 0 || slices.ContainsFunc(targets, func(target string) bool { return !author[target] }) {
			continue
		}
		for _, target := range targets {
			author[target] = false
		}
		lines = append(lines, line)
	}
	for _, target := range order {
		if author[target] {
			lines = append(lines, closingLine(target))
		}
	}
	return lines, nil
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
// the ledger and so author closing lines over: GitHub only, the one forge
// whose Markdown renderer decides what is live (renderedClosingTargets).
func carriesClosingLines(provider scm.Provider) bool {
	return provider == scm.ProviderGitHub
}

// closingLedgerReserveBytes is room kept free when an ordinary body is fitted
// to the size cap for ledger entries beyond the requested references.
const closingLedgerReserveBytes = 1024

// sealClosingLedger appends the ledger to a final ordinary body about to be
// published: every live closing reference GitHub renders in it, one per
// occurrence, except those of the carried author lines. Whatever construct a
// pipeline-published reference sat in, the next update renders it the same
// way and finds it recorded, so only references that appeared after
// publication are carried as the author's.
func sealClosingLedger(ctx context.Context, sctx *pipeline.StepContext, host scm.Host, provider scm.Provider, body string) (string, error) {
	if !carriesClosingLines(provider) {
		return body, nil
	}
	live, err := renderedClosingTargets(ctx, sctx, host, body)
	if err != nil {
		return "", err
	}
	carried := map[string]int{}
	for _, line := range sctx.CarriedClosingLines {
		for _, target := range closingTargetOccurrences(line, sctx) {
			carried[target]++
		}
	}
	published := []string{}
	for _, target := range live {
		if carried[target] > 0 {
			carried[target]--
			continue
		}
		published = append(published, target)
	}
	body = appendIssuesSection(body, renderClosingLedger(published))
	if len(body) > maxPullRequestBodyBytes {
		return "", fmt.Errorf("render closing issues: PR body exceeds the size limit after recording its closing references")
	}
	return body, nil
}

// refreshCarriedClosingLines takes the author's closing lines carried from a
// live body re-read just before the write and, when they changed while the
// update was drafting, replaces body's trailing Issues block. The re-read is
// authoritative both ways: lines added since the first read are carried, and
// lines the author removed are not written back.
func refreshCarriedClosingLines(sctx *pipeline.StepContext, body string, latest []string, bodyLimit int) (string, error) {
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
	targets := closingTargets(present, sctx)
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
