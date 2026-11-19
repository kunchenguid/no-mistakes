package github

import (
	"fmt"
	"html"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// copilotSummary is what a submitted Copilot review's summary body says,
// reduced to evidence. The body is untrusted bot prose: it is inspected as
// data, never followed, rendered, or used to fetch anything, and anything this
// parser does not positively recognize as a clean inventory becomes an item an
// operator must look at rather than a pass.
type copilotSummary struct {
	// Items are the distinct issues and concerns the summary carries. An
	// item may reference inline discussions; the adapter resolves those
	// against the pull request's own review threads.
	Items []copilotSummaryItem
	// Incomplete is set when the summary reports that the review did not
	// cover the change (a service failure or partial review notice).
	Incomplete bool
}

type copilotSummaryItem struct {
	// Section names where the item came from, e.g. "previously missed
	// issue" or "review assessment".
	Section string
	Title   string
	Text    string
	// Discussions are the inline-comment references the item links to.
	Discussions []copilotDiscussionRef
}

// copilotDiscussionRef is a link to an inline review comment. A bare
// "#discussion_r<id>" fragment leaves Repo and Number empty, meaning this pull
// request.
type copilotDiscussionRef struct {
	Repo      string
	Number    int
	CommentID int64
}

// maxCopilotSummaryBodyBytes is the size at which a review body is treated as
// possibly truncated: GitHub caps review bodies at 65536 characters, so a body
// that reaches the cap may have lost its tail, which could hold an issue.
const maxCopilotSummaryBodyBytes = 65536

// maxCopilotSummaryItems bounds the evidence one summary can produce. The CI
// findings layer applies its own, smaller budget; this only keeps a hostile
// body from allocating without limit before that.
const maxCopilotSummaryItems = 200

var (
	copilotTagPattern       = regexp.MustCompile(`(?is)<details\b[^>]*>|</details\s*>|<summary\b[^>]*>(.*?)</summary\s*>`)
	copilotStrayTagPattern  = regexp.MustCompile(`(?is)</?(details|summary)\b`)
	copilotHTMLTagPattern   = regexp.MustCompile(`(?s)<[^>]*>`)
	copilotImgAltPattern    = regexp.MustCompile(`(?is)<img\b[^>]*\balt\s*=\s*"([^"]*)"[^>]*>`)
	copilotHeadingPattern   = regexp.MustCompile(`^(#{1,6})\s+(.*?)\s*#*\s*$`)
	copilotBoldLinePattern  = regexp.MustCompile(`^(?:\*\*|__)(.+?)(?:\*\*|__):?$`)
	copilotListItemPattern  = regexp.MustCompile(`^\s*(?:[-*+]|\d{1,3}[.)])\s+(.*)$`)
	copilotShortcodePattern = regexp.MustCompile(`:[a-z0-9_+-]+:`)
	copilotDiscussionLink   = regexp.MustCompile(`(?i)(?:https://github\.com/([a-z0-9_.-]+/[a-z0-9_.-]+)/pull/(\d+))?#discussion_r(\d+)`)
	copilotIssueLabel       = regexp.MustCompile(`^(\d+)\s+(?:previously\s+missed|open|new|unresolved|outstanding|remaining)\s+issues?\b`)
	copilotSuppressedLabel  = regexp.MustCompile(`^comments?\s+suppressed\s+due\s+to\s+low\s+confidence\s*\(?(\d+)\)?`)
	copilotFailurePatterns  = []*regexp.Regexp{
		regexp.MustCompile(`copilot\b[^.\n]{0,80}\b(?:was\s+unable|wasn't\s+able|was\s+not\s+able|is\s+unable|could\s*n[o']t|couldn't|failed|encountered\s+an\s+error)\b`),
		regexp.MustCompile(`\b(?:unable|not\s+able|failed)\s+to\s+(?:complete\s+(?:the|this|a)\s+)?review\b`),
		regexp.MustCompile(`\breview\s+(?:failed|could\s+not\s+be\s+completed|was\s+incomplete|is\s+incomplete)\b`),
	}
	// Lines of the known clean templates that carry no feedback. Matched
	// against a normalized, lowercase line.
	copilotBoilerplate = []*regexp.Regexp{
		regexp.MustCompile(`^copilot reviewed \d+ out of \d+ (?:changed )?files?\b.*$`),
		regexp.MustCompile(`^(?:no|0|zero) (?:open |new |remaining |unresolved |outstanding )?(?:issues?|comments?|concerns?|findings?)(?: (?:were )?found| to report| identified)?\.?$`),
		regexp.MustCompile(`custom instructions`),
		regexp.MustCompile(`^tip:`),
		regexp.MustCompile(`(?:share|give|leave) (?:your )?feedback`),
		regexp.MustCompile(`^learn (?:more|how)\b`),
		regexp.MustCompile(`^[-*_=]{3,}$`),
	}
)

// copilotSectionKind classifies a heading or a details label.
type copilotSectionKind int

const (
	copilotSectionSubstantive copilotSectionKind = iota
	copilotSectionInformational
	copilotSectionConcernVerdict
)

func copilotHeadingKind(label string) copilotSectionKind {
	switch strings.TrimSuffix(label, ":") {
	case "pull request overview", "overview", "key changes", "reviewed changes", "files reviewed",
		"changed files", "file summary", "summary per file", "show a summary per file",
		"looks good", "no issues found", "lgtm":
		return copilotSectionInformational
	case "needs a closer look", "changes recommended", "changes requested", "needs attention", "needs changes":
		return copilotSectionConcernVerdict
	}
	if strings.HasPrefix(label, "resolved") || strings.Contains(label, "resolved since") || strings.HasPrefix(label, "files not reviewed") {
		return copilotSectionInformational
	}
	return copilotSectionSubstantive
}

type copilotNode struct {
	label    string
	hasLabel bool
	parts    []copilotPart
}

type copilotPart struct {
	text  string
	child *copilotNode
}

// parseCopilotSummary reads a Copilot review summary body. Everything it
// cannot place - unknown prose, malformed nesting, counts that do not match
// their items, a body that may be truncated - becomes an item, so an unknown
// or changed template can never read as a clean review.
func parseCopilotSummary(body string) copilotSummary {
	var result copilotSummary
	body = strings.ReplaceAll(body, "\r\n", "\n")
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		result.Items = append(result.Items, copilotSummaryItem{Section: "review summary", Title: "Empty review summary", Text: "The submitted review has no summary, so it cannot show the change was reviewed."})
		return result
	}
	if len(body) >= maxCopilotSummaryBodyBytes {
		result.Items = append(result.Items, copilotSummaryItem{Section: "review summary", Title: "Review summary may be truncated", Text: "The submitted review summary reached GitHub's size limit, so part of it may be missing."})
	}
	normalizedBody := strings.ToLower(copilotPlainText(body))
	for _, pattern := range copilotFailurePatterns {
		if loc := pattern.FindStringIndex(normalizedBody); loc != nil {
			result.Incomplete = true
			result.Items = append(result.Items, copilotSummaryItem{Section: "review status", Title: "Review did not complete", Text: copilotSentenceAround(copilotPlainText(body), loc[0])})
			break
		}
	}
	root, ok := buildCopilotTree(body)
	if !ok {
		result.Items = append(result.Items, copilotSummaryItem{Section: "review summary", Title: "Review summary could not be read", Text: "The summary's collapsible sections are not well formed: " + copilotPlainText(body)})
		return result
	}
	walker := copilotWalker{}
	walker.walk(root, copilotSectionSubstantive, "")
	result.Items = append(result.Items, walker.items...)
	if walker.concernVerdict != "" && len(walker.items) == 0 && !result.Incomplete {
		result.Items = append(result.Items, copilotSummaryItem{
			Section: "review assessment", Title: walker.concernVerdict,
			Text: "The review's verdict asks for attention, but no individual issue could be read from its summary; inspect the review.",
		})
	}
	if len(result.Items) > maxCopilotSummaryItems {
		result.Items = append(result.Items[:maxCopilotSummaryItems-1], copilotSummaryItem{
			Section: "review summary", Title: "More summary items",
			Text: fmt.Sprintf("%d more summary items were not read; inspect the review.", len(result.Items)-maxCopilotSummaryItems+1),
		})
	}
	return result
}

// buildCopilotTree splits the body on details/summary tags. It fails when the
// nesting is not well formed, because a misread boundary could move an issue
// into an informational section.
func buildCopilotTree(body string) (*copilotNode, bool) {
	root := &copilotNode{}
	stack := []*copilotNode{root}
	last := 0
	for _, loc := range copilotTagPattern.FindAllStringSubmatchIndex(body, -1) {
		current := stack[len(stack)-1]
		if text := body[last:loc[0]]; text != "" {
			current.parts = append(current.parts, copilotPart{text: text})
		}
		last = loc[1]
		tag := strings.ToLower(body[loc[0]:loc[1]])
		switch {
		case strings.HasPrefix(tag, "<details"):
			child := &copilotNode{}
			current.parts = append(current.parts, copilotPart{child: child})
			stack = append(stack, child)
		case strings.HasPrefix(tag, "</details"):
			if len(stack) == 1 {
				return nil, false
			}
			stack = stack[:len(stack)-1]
		default: // <summary>...</summary>
			if len(stack) == 1 || current.hasLabel {
				return nil, false
			}
			for _, part := range current.parts {
				if part.child != nil || strings.TrimSpace(copilotPlainText(part.text)) != "" {
					return nil, false
				}
			}
			current.parts = nil
			current.label = body[loc[2]:loc[3]]
			current.hasLabel = true
		}
	}
	if len(stack) != 1 {
		return nil, false
	}
	if text := body[last:]; text != "" {
		root.parts = append(root.parts, copilotPart{text: text})
	}
	// An unmatched fragment such as "<summary>" without its closing tag
	// was not consumed above and would otherwise read as plain prose.
	var stray func(*copilotNode) bool
	stray = func(node *copilotNode) bool {
		for _, part := range node.parts {
			if part.child != nil && stray(part.child) || part.child == nil && copilotStrayTagPattern.MatchString(part.text) {
				return true
			}
		}
		return false
	}
	if stray(root) {
		return nil, false
	}
	return root, true
}

type copilotWalker struct {
	items          []copilotSummaryItem
	concernVerdict string
}

// walk classifies a node's content line by line. kind is the classification
// inherited from the enclosing section; heading names it. Every markdown
// heading starts a new section; an unknown heading is substantive even when
// it follows an overview, so new feedback under a new heading is never hidden.
func (w *copilotWalker) walk(node *copilotNode, kind copilotSectionKind, heading string) {
	var pending []string
	flush := func() {
		text := strings.TrimSpace(strings.Join(pending, "\n"))
		pending = nil
		if text == "" {
			return
		}
		title := heading
		if title == "" {
			title = "Review summary"
		}
		w.items = append(w.items, copilotSummaryItem{Section: "review assessment", Title: title, Text: text, Discussions: copilotDiscussions(text)})
	}
	for _, part := range node.parts {
		if part.child != nil {
			flush()
			w.walkDetails(part.child, kind, heading)
			continue
		}
		for _, raw := range strings.Split(part.text, "\n") {
			line := strings.TrimSpace(copilotPlainText(raw))
			if line == "" {
				continue
			}
			if m := copilotHeadingPattern.FindStringSubmatch(line); m != nil {
				flush()
				kind = copilotHeadingKind(copilotLabel(m[2]))
				heading = copilotCleanTitle(m[2])
				if kind == copilotSectionConcernVerdict && w.concernVerdict == "" {
					w.concernVerdict = heading
				}
				continue
			}
			if kind == copilotSectionInformational || copilotIsBoilerplate(copilotLabel(line)) || copilotIsFailureNotice(line) {
				continue
			}
			pending = append(pending, line)
		}
	}
	flush()
}

// walkDetails handles one collapsible block: an issue list (read even inside
// an overview, where the classic template keeps its low-confidence comments),
// an informational inventory, or an unknown block whose content is read like
// the surrounding section.
func (w *copilotWalker) walkDetails(node *copilotNode, kind copilotSectionKind, heading string) {
	label := copilotLabel(copilotPlainText(node.label))
	title := copilotCleanTitle(copilotPlainText(node.label))
	if m := copilotIssueLabel.FindStringSubmatch(label); m != nil {
		w.collectIssues(node, m[1], title)
		return
	}
	if m := copilotSuppressedLabel.FindStringSubmatch(label); m != nil {
		w.collectIssues(node, m[1], title)
		return
	}
	switch copilotHeadingKind(label) {
	case copilotSectionInformational:
		// An inventory carries no feedback, but an issue block nested in it
		// still does.
		w.walk(node, copilotSectionInformational, title)
		return
	case copilotSectionConcernVerdict:
		if w.concernVerdict == "" {
			w.concernVerdict = title
		}
		w.walk(node, copilotSectionConcernVerdict, title)
		return
	}
	if title == "" {
		title = heading
	}
	if kind == copilotSectionInformational {
		w.walk(node, copilotSectionInformational, title)
		return
	}
	w.walk(node, copilotSectionSubstantive, title)
}

// collectIssues emits one item per issue in a counted issue block: each
// nested details block, or each top-level list entry. A count that does not
// match the items found means part of the block was misread.
func (w *copilotWalker) collectIssues(node *copilotNode, rawCount, section string) {
	want, err := strconv.Atoi(rawCount)
	if err != nil {
		want = -1
	}
	var found []copilotSummaryItem
	var prefix []string
	var current *copilotSummaryItem
	finish := func() {
		if current != nil {
			current.Text = strings.TrimSpace(current.Text)
			found = append(found, *current)
			current = nil
		}
	}
	for _, part := range node.parts {
		if part.child != nil {
			finish()
			title := copilotCleanTitle(copilotPlainText(part.child.label))
			text := strings.TrimSpace(copilotFlatten(part.child))
			if len(prefix) > 0 {
				text = strings.Join(prefix, "\n") + "\n" + text
				prefix = nil
			}
			found = append(found, copilotSummaryItem{Section: section, Title: title, Text: text})
			continue
		}
		for _, raw := range strings.Split(part.text, "\n") {
			line := strings.TrimSpace(copilotPlainText(raw))
			if line == "" {
				continue
			}
			indent := len(raw) - len(strings.TrimLeft(raw, " \t"))
			if m := copilotListItemPattern.FindStringSubmatch(raw); m != nil && indent < 2 {
				finish()
				text := strings.TrimSpace(copilotPlainText(m[1]))
				if len(prefix) > 0 {
					text = strings.Join(prefix, " ") + ": " + text
					prefix = nil
				}
				current = &copilotSummaryItem{Section: section, Title: copilotClip(text, 200), Text: text}
				continue
			}
			if copilotBoldLinePattern.MatchString(line) || current == nil {
				finish()
				prefix = append(prefix, line)
				continue
			}
			current.Text += "\n" + line
		}
	}
	finish()
	for i := range found {
		found[i].Discussions = copilotDiscussions(found[i].Title + "\n" + found[i].Text)
		if found[i].Title == "" {
			found[i].Title = fmt.Sprintf("%s %d", section, i+1)
		}
	}
	if len(prefix) > 0 {
		// Text after the last entry that belongs to no entry is still feedback.
		found = append(found, copilotSummaryItem{Section: section, Title: section, Text: strings.Join(prefix, "\n"), Discussions: copilotDiscussions(strings.Join(prefix, "\n"))})
		if want >= 0 {
			want = -1
		}
	}
	w.items = append(w.items, found...)
	if want != len(found) {
		w.items = append(w.items, copilotSummaryItem{
			Section: section, Title: "Issue count does not match",
			Text: fmt.Sprintf("The summary block %q has %d readable issues, which does not match its label; inspect the review for any that were not read.", section, len(found)),
		})
	}
}

// copilotFlatten returns a node's text, nested blocks included, as plain
// lines.
func copilotFlatten(node *copilotNode) string {
	var lines []string
	for _, part := range node.parts {
		if part.child != nil {
			if label := strings.TrimSpace(copilotPlainText(part.child.label)); label != "" {
				lines = append(lines, label)
			}
			lines = append(lines, copilotFlatten(part.child))
			continue
		}
		for _, raw := range strings.Split(part.text, "\n") {
			if line := strings.TrimSpace(copilotPlainText(raw)); line != "" {
				lines = append(lines, line)
			}
		}
	}
	return strings.Join(lines, "\n")
}

// copilotPlainText turns review markup into inert text: an image keeps only
// its alt text, every other tag is dropped, and entities are decoded. Nothing
// is ever rendered or fetched.
func copilotPlainText(s string) string {
	s = copilotImgAltPattern.ReplaceAllString(s, " $1 ")
	s = copilotHTMLTagPattern.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	return strings.Join(strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == '\t' || r == ' ' }), " ")
}

// copilotLabel normalizes a line for template matching: lowercase, with
// emoji, markdown emphasis, and surrounding punctuation removed.
func copilotLabel(s string) string {
	s = strings.ToLower(copilotShortcodePattern.ReplaceAllString(s, " "))
	s = strings.Map(dropEmoji, s)
	s = strings.NewReplacer("**", "", "__", "", "`", "", "*", "").Replace(s)
	s = strings.TrimLeft(s, "#>-+ ")
	return strings.TrimSpace(strings.Join(strings.Fields(s), " "))
}

// copilotCleanTitle is a display title: emoji and emphasis removed, case kept.
func copilotCleanTitle(s string) string {
	s = copilotShortcodePattern.ReplaceAllString(s, " ")
	s = strings.Map(dropEmoji, s)
	s = strings.NewReplacer("**", "", "__", "", "`", "").Replace(s)
	return copilotClip(strings.TrimSpace(strings.Join(strings.Fields(strings.TrimLeft(s, "#>-+* ")), " ")), 200)
}

func dropEmoji(r rune) rune {
	if unicode.Is(unicode.So, r) || unicode.Is(unicode.Sk, r) || r == '\ufe0f' || r == '\u200d' {
		return -1
	}
	return r
}

// copilotIsFailureNotice reports a line parseCopilotSummary already reported
// as the review's incomplete status.
func copilotIsFailureNotice(line string) bool {
	line = strings.ToLower(line)
	for _, pattern := range copilotFailurePatterns {
		if pattern.MatchString(line) {
			return true
		}
	}
	return false
}

func copilotIsBoilerplate(label string) bool {
	for _, pattern := range copilotBoilerplate {
		if pattern.MatchString(label) {
			return true
		}
	}
	return false
}

func copilotDiscussions(text string) []copilotDiscussionRef {
	var refs []copilotDiscussionRef
	seen := map[string]bool{}
	for _, m := range copilotDiscussionLink.FindAllStringSubmatch(text, -1) {
		if seen[m[0]] {
			continue
		}
		seen[m[0]] = true
		id, err := strconv.ParseInt(m[3], 10, 64)
		if err != nil || id <= 0 {
			// An unreadable reference still references something: keep it
			// so it cannot be located and the item stays visible.
			refs = append(refs, copilotDiscussionRef{Repo: "unreadable", CommentID: -1})
			continue
		}
		ref := copilotDiscussionRef{CommentID: id}
		if m[1] != "" {
			ref.Repo = m[1]
			ref.Number, _ = strconv.Atoi(m[2])
		}
		refs = append(refs, ref)
	}
	return refs
}

// copilotSentenceAround returns a bounded excerpt of text around offset.
func copilotSentenceAround(text string, offset int) string {
	if offset > len(text) {
		offset = len(text)
	}
	start := strings.LastIndexAny(text[:offset], ".\n") + 1
	end := strings.IndexAny(text[offset:], ".\n")
	if end < 0 {
		end = len(text)
	} else {
		end += offset + 1
	}
	return copilotClip(strings.TrimSpace(text[start:end]), 400)
}

// copilotClip shortens s to at most limit bytes on a UTF-8 boundary.
func copilotClip(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}
