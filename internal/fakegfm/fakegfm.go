// Package fakegfm is the test fakes' stand-in for GitHub's Markdown renderer
// (POST /markdown, mode gfm), used by the fake gh CLIs
// (internal/pipeline/fakecli and cmd/fakeagent). It renders only the subset
// the PR step's closing-reference handling relies on, each case matching the
// real renderer:
//   - an HTML comment is removed: one opened at the start of a line (after up
//     to three spaces) runs through the line holding `-->`, across lines, or
//     to the end of the document, and an inline one closed on its own line is
//     removed too;
//   - a stray inline `<!--` with no closing `-->` is literal text and hides
//     nothing after it;
//   - fenced code renders inside <pre><code>, an inline code span inside
//     <code>, and raw <pre>/<code> elements stay as written;
//   - a table row renders each cell in its own <td> on its own line (the
//     delimiter row renders nothing), and a `-`, `*`, or `+` list item
//     renders inside <ul><li>, so text in separate blocks is never on one
//     line;
//   - a github.com issue or pull request URL outside code renders as a link
//     whose text is `#N` in the context repository and `owner/repo#N` in
//     another.
//
// Everything else passes through unchanged.
package fakegfm

import (
	"html"
	"regexp"
	"strings"
)

var (
	codeSpanPattern       = regexp.MustCompile("`([^`]+)`")
	inlineCommentPattern  = regexp.MustCompile(`<!--.*?-->`)
	tableDelimiterPattern = regexp.MustCompile(`^\|[\s|:-]*$`)
	listItemPattern       = regexp.MustCompile(`^[-*+][ \t]+`)
	issueURLPattern       = regexp.MustCompile(`https?://github\.com/([A-Za-z0-9-]+/[A-Za-z0-9._-]+)/(?:issues|pull)/([1-9][0-9]*)\b`)
)

// Render returns text rendered to HTML as GitHub would for the subset above,
// in the context of repo ("owner/name").
func Render(text, repo string) string {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	var out []string
	fence := ""
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimLeft(line, " ")
		blockStart := len(line)-len(trimmed) <= 3
		switch {
		case fence != "":
			if blockStart && strings.HasPrefix(trimmed, fence) {
				out = append(out, "</code></pre>")
				fence = ""
			} else {
				out = append(out, html.EscapeString(line))
			}
		case blockStart && (strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~")):
			fence = trimmed[:3]
			out = append(out, "<pre><code>")
		case blockStart && strings.HasPrefix(trimmed, "<!--"):
			for i < len(lines) && !strings.Contains(lines[i], "-->") {
				i++
			}
		case blockStart && strings.HasPrefix(trimmed, "|"):
			if tableDelimiterPattern.MatchString(trimmed) {
				continue
			}
			row := []string{"<tr>"}
			for _, cell := range strings.Split(strings.Trim(strings.TrimSpace(trimmed), "|"), "|") {
				row = append(row, "<td>"+renderInline(repo, strings.TrimSpace(cell))+"</td>")
			}
			out = append(out, append(row, "</tr>")...)
		case blockStart && listItemPattern.MatchString(trimmed):
			out = append(out, "<ul>", "<li>"+renderInline(repo, listItemPattern.ReplaceAllString(trimmed, ""))+"</li>", "</ul>")
		default:
			out = append(out, renderInline(repo, line))
		}
	}
	if fence != "" {
		out = append(out, "</code></pre>")
	}
	return strings.Join(out, "\n")
}

func renderInline(repo, line string) string {
	var out strings.Builder
	last := 0
	for _, span := range codeSpanPattern.FindAllStringIndex(line, -1) {
		out.WriteString(linkIssueURLs(repo, line[last:span[0]]))
		out.WriteString("<code>" + html.EscapeString(line[span[0]+1:span[1]-1]) + "</code>")
		last = span[1]
	}
	out.WriteString(linkIssueURLs(repo, line[last:]))
	line = out.String()
	line = inlineCommentPattern.ReplaceAllString(line, "")
	return strings.ReplaceAll(line, "<!--", "&lt;!--")
}

func linkIssueURLs(repo, text string) string {
	return issueURLPattern.ReplaceAllStringFunc(text, func(url string) string {
		m := issueURLPattern.FindStringSubmatch(url)
		label := m[1] + "#" + m[2]
		if strings.EqualFold(m[1], repo) {
			label = "#" + m[2]
		}
		return `<a href="` + url + `">` + label + "</a>"
	})
}
