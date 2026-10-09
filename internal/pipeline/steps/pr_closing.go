package steps

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/closingissues"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/scm/github"
	"github.com/kunchenguid/no-mistakes/internal/scm/gitlab"
)

// The pipeline adds closing references to a PR body from exactly one place:
// explicit `axi run --closes` values, persisted on the run and claimed once by
// the PR step (sctx.ClosingIssueRefs). They render in one stable `## Issues`
// section. An author-preserving (pr.template) body keeps its author text
// verbatim, so only the references the author text does not already close
// are added to its generated appendix. Closure is never inferred from intent,
// commits, or branch names: pipeline-generated text is published with its
// closing references neutralized by the provider's closingGrammar.

const issuesSectionHeading = "## Issues"

// closingGrammar is the issue-closing language of the forge a PR body is
// published to.
//
// One shared pattern cannot cover both dialects, because they differ in more
// than vocabulary:
//
//   - GitHub's words are close/fix/resolve - no gerunds, no "implement" - and
//     GitHub ignores a reference inside code, which is why its neutralizer may
//     wrap a reference in an inline code span.
//   - GitLab's default pattern (the documented server regex, replaceable by a
//     self-managed administrator) adds the gerunds and "implements", and
//     Gitlab::ClosingIssueExtractor applies it as a plain regexp to the raw
//     merge request description and commit messages: code blocks and inline
//     code are NOT exempt.
type closingGrammar struct {
	// keywordLine matches a line that is nothing but closing keywords and their
	// targets, optionally as a bullet or ordered list item and with trailing
	// sentence punctuation. A reference inside prose ("this fixes #4 partly")
	// is deliberately not counted: it is not a standalone closing declaration.
	keywordLine *regexp.Regexp
	// reference extracts the targets of a closing-keyword line.
	reference *regexp.Regexp
	// neutralize breaks closing references in generated text under the
	// supported grammar, which on GitLab is the default server pattern.
	neutralize func(string) string
	// codeAware reports whether the forge ignores a reference inside code. It
	// decides whether the closing-line scan may skip fenced and indented blocks
	// (a reference there is a closing reference only on a forge that does not).
	codeAware bool
}

const githubClosingKeywordPattern = `(?:close|closes|closed|fix|fixes|fixed|resolve|resolves|resolved):?\s+(?:#[1-9][0-9]*|[A-Za-z0-9-]+/[A-Za-z0-9._-]+#[1-9][0-9]*)`

var closingKeywordLinePattern = regexp.MustCompile(`(?i)^(?:(?:[-*+]|[0-9]+[.)])\s+)?` + githubClosingKeywordPattern + `(?:\s*,\s*` + githubClosingKeywordPattern + `)*[.;!]?$`)

var closingReferencePattern = regexp.MustCompile(`(?i)(?:[A-Za-z0-9-]+/[A-Za-z0-9._-]+#[1-9][0-9]*|#[1-9][0-9]*)`)

var closingReferenceInTextPattern = regexp.MustCompile(`(?i)\b((?:close|closes|closed|fix|fixes|fixed|resolve|resolves|resolved):?\s+)((?:[A-Za-z0-9-]+/[A-Za-z0-9._-]+)?#[1-9][0-9]*|https?://[A-Za-z0-9.-]+(?::[0-9]+)?/[A-Za-z0-9-]+/[A-Za-z0-9._-]+/(?:issues|pull)/[1-9][0-9]*)\b`)

// GitLab's default closing pattern, from
// https://docs.gitlab.com/user/project/issues/managing_issues/#default-closing-pattern.
// Its words are the [Cc]los/[Ff]ix/[Rr]esolv families plus the gerunds and
// [Ii]mplement, and its references nest: "group/subgroup/project#12" and the
// full "<host>/<path>/-/issues/12" URL are both closing references.
const gitlabClosingKeywordPattern = `(?:[Cc]los(?:e[sd]?|ing)|[Ff]ix(?:e[sd]|ing)?|[Rr]esolv(?:e[sd]?|ing)|[Ii]mplement(?:s|ed|ing)?)`

// gitlabClosingTargetPattern is the reference half of a closing-keyword line
// without the URL form: a target is compared against the run's requested refs,
// which are project paths or bare numbers.
const gitlabClosingTargetPattern = `(?:[A-Za-z0-9_.-]+(?:/[A-Za-z0-9_.-]+)*#[1-9][0-9]*|#[1-9][0-9]*)`

// gitlabClosingReferenceList accepts the separators GitLab's pattern allows
// between several references ("Fixes #1, #2 and #3").
const gitlabClosingReferenceList = gitlabClosingTargetPattern + `(?:(?: *, *(?:and +)?| +and +)` + gitlabClosingTargetPattern + `)*`

// gitlabClosingSeparator is the keyword-to-reference separator: an optional
// colon, at least one space (GitLab's pattern is a literal ` +`, so a newline
// does not join a keyword to a reference), and GitLab's optional literal
// "issues" - "Closes issues #12" closes #12 too.
const gitlabClosingSeparator = `(?::? +(?:issues? +)?)`

var gitlabClosingKeywordLinePattern = regexp.MustCompile(`^(?:(?:[-*+]|[0-9]+[.)])[ \t]+)?` + gitlabClosingKeywordPattern + gitlabClosingSeparator + gitlabClosingReferenceList + `[.;!]?$`)

var gitlabClosingReferencePatternCompiled = regexp.MustCompile(gitlabClosingTargetPattern)

var gitlabClosingKeywordInTextPattern = regexp.MustCompile(`\b` + gitlabClosingKeywordPattern + `:? +`)

var (
	githubClosingGrammar = closingGrammar{
		keywordLine: closingKeywordLinePattern,
		reference:   closingReferencePattern,
		neutralize:  neutralizeGitHubClosingReferences,
		codeAware:   true,
	}
	gitlabClosingGrammar = closingGrammar{
		keywordLine: gitlabClosingKeywordLinePattern,
		reference:   gitlabClosingReferencePatternCompiled,
		neutralize:  neutralizeGitLabClosingReferences,
		codeAware:   false,
	}
)

// closingGrammarFor returns the closing grammar of the provider a PR body is
// published to. Every provider other than GitLab keeps GitHub's grammar, which
// is the behavior every existing body was rendered under.
func closingGrammarFor(provider scm.Provider) closingGrammar {
	if provider == scm.ProviderGitLab {
		return gitlabClosingGrammar
	}
	return githubClosingGrammar
}

// extractClosingKeywordLines returns the distinct standalone closing-keyword
// lines of body. Code is skipped only for a provider that ignores references
// inside code: on GitLab, where the closing pattern is applied to the raw text,
// a fenced "Closes #95" is a live closing reference and is counted.
func extractClosingKeywordLines(body string, grammar closingGrammar) []string {
	seen := map[string]struct{}{}
	var lines []string
	var fence markdownFence
	for _, raw := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		if grammar.codeAware {
			inFence := fence.marker != 0
			fence.consume(raw)
			if inFence || fence.marker != 0 || strings.HasPrefix(raw, "\t") || strings.HasPrefix(raw, "    ") {
				continue
			}
		}
		line := strings.TrimSpace(raw)
		if !grammar.keywordLine.MatchString(line) {
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

// neutralizeGitHubClosingReferences puts every closing-keyword reference in
// pipeline-generated PR text, including an issue or pull request URL, in an
// inline code span ("Fixes `#12`"), outside fenced, indented, and inline code
// and HTML <code>/<pre> elements (a tested command renders as <code>).
// GitHub ignores a reference in code, so nothing the pipeline publishes can
// close an issue; only the Issues section carries live ones.
func neutralizeGitHubClosingReferences(s string) string {
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

func neutralizeGitLabClosingReferences(s string) string {
	return gitlabClosingKeywordInTextPattern.ReplaceAllStringFunc(s, func(match string) string {
		return strings.Replace(match, " ", "&#32;", 1)
	})
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

// closingTargets returns the canonical refs ("42", "owner/repo#42",
// "group/subgroup/project#42") closed by the given closing-keyword lines. A
// reference qualified with repo, the PR's own repository, is the bare number.
func closingTargets(lines []string, repo string, grammar closingGrammar) map[string]struct{} {
	targets := map[string]struct{}{}
	for _, line := range lines {
		for _, target := range grammar.reference.FindAllString(line, -1) {
			target = closingissues.Localize(strings.TrimPrefix(target, "#"), repo)
			targets[strings.ToLower(target)] = struct{}{}
		}
	}
	return targets
}

// prRepository returns the project path the PR lives in, or "" when it is
// unknown. GitHub needs owner/repository; GitLab needs the full project path,
// which may nest under subgroups and is what a cross-project closing reference
// is qualified with.
func prRepository(sctx *pipeline.StepContext, provider scm.Provider) string {
	if sctx == nil {
		return ""
	}
	if sctx.Repo != nil {
		if repo := projectPathForProvider(sctx.Repo.UpstreamURL, provider); repo != "" {
			return repo
		}
	}
	if sctx.Run != nil && sctx.Run.PRURL != nil {
		return projectPathForProvider(*sctx.Run.PRURL, provider)
	}
	return ""
}

func projectPathForProvider(raw string, provider scm.Provider) string {
	if provider == scm.ProviderGitLab {
		// A merge request URL needs its own accessor: ProjectPath reads a
		// repository remote and would leave the /-/merge_requests/<n> tail on.
		if project := gitlab.ProjectPathFromMRURL(raw); project != "" {
			return project
		}
		return gitlab.ProjectPath(raw)
	}
	return github.RepoSlug(raw)
}

func closingLine(ref string) string {
	return "Closes " + closingissues.Target(ref)
}

// issuesSection renders the stable Issues section, or "" when there is
// nothing to render. Requested refs already closed by authorText (live author
// content kept verbatim around it) are not repeated, so each reference
// appears exactly once.
func issuesSection(sctx *pipeline.StepContext, authorText string) string {
	if sctx == nil {
		return ""
	}
	provider := resolvedProviderForBody(sctx)
	grammar := closingGrammarFor(provider)
	var lines []string
	present := closingTargets(extractClosingKeywordLines(authorText, grammar), prRepository(sctx, provider), grammar)
	for _, ref := range sctx.ClosingIssueRefs {
		key := strings.ToLower(ref)
		if _, exists := present[key]; exists {
			continue
		}
		present[key] = struct{}{}
		lines = append(lines, closingLine(ref))
	}
	if len(lines) == 0 {
		return ""
	}
	return issuesSectionHeading + "\n\n" + strings.Join(lines, "\n")
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
	repo := prRepository(sctx, provider)
	for i, ref := range refs {
		refs[i] = closingissues.Localize(ref, repo)
	}
	refs, err = closingissues.Normalize(refs)
	if err != nil {
		return fmt.Errorf("resolve closing issue references: %w", err)
	}
	if len(refs) == 0 {
		sctx.ClosingIssueRefs = nil
		return nil
	}
	// A nested project path is a GitLab subgroup reference. Publishing one for
	// another forge would hand it text it reads as a *different* repository's
	// owner/repo reference, so it fails closed instead of being silently
	// mistranslated.
	if provider != scm.ProviderGitLab {
		for _, ref := range refs {
			if strings.Count(ref, "/") > 1 {
				return fmt.Errorf("render closing issues: %s is a GitLab subgroup reference, and this repository's provider (%s) names issues as owner/repository#number", ref, provider)
			}
		}
	}
	// A provider that does not close issues from the pull request body would
	// publish a reference that silently closes nothing, leaving the requested
	// issue open after merge.
	if !host.Capabilities().ClosingReferences {
		return fmt.Errorf("render closing issues: provider %s does not support closing references from the pull request body", provider)
	}
	if _, ok := host.(scm.PRContentReader); !ok {
		return fmt.Errorf("verify closing issues: provider cannot read the current pull request body")
	}
	// Assigned only once every gate has passed, so a refusal never leaves the
	// step holding references it will not publish.
	sctx.ClosingIssueRefs = refs
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
// carries every requested reference. A missing reference is invisible at
// review time (the issue simply stays open after merge), so the step must not
// report success without this check.
func verifyClosingIssues(ctx context.Context, host scm.Host, pr *scm.PR, sctx *pipeline.StepContext) error {
	if sctx == nil || len(sctx.ClosingIssueRefs) == 0 {
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
	provider := resolvedProviderForBody(sctx)
	grammar := closingGrammarFor(provider)
	targets := closingTargets(extractClosingKeywordLines(body, grammar), prRepository(sctx, provider), grammar)
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
	section := issuesSection(sctx, "")
	reserve := 0
	if section != "" {
		reserve = len("\n\n" + section)
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
