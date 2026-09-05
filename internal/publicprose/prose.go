// Package publicprose removes known operator address from generated prose.
// It is not a repository-content filter or a detector of personal names.
package publicprose

import (
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Only the reported vocative forms at a prose boundary are removed. A domain
// phrase such as "the captain, crew and ship" is not operator address.
var address = regexp.MustCompile(`(?m)(^[^\pL\pN\r\n]*?|^[ \t]*[0-9]+[.)][ \t]+|[.!?:][*_]*[ \t]+|[ \t]+-[ \t]+|>[ \t]*)([*_]*)(Captain[ \t]*)([:,])([*_]*)([ \t]*)`)

var codeTokens = regexp.MustCompile(`(?s)<!--.*?(?:-->|$)|(?i:<pre\b[^>]*>.*?(?:</pre\s*>|$)|<code\b[^>]*>.*?(?:</code\s*>|$))|<[^>\n]*>|` + "`+")
var listMarker = regexp.MustCompile(`^(?:[-+*]|[0-9]+[.)])[ \t]+`)

// Headings and nonempty list items interrupt a quoted paragraph's lazy
// continuation. Ordered lists can interrupt only when they start at one.
var blockquoteBreak = regexp.MustCompile(`^ {0,3}(?:#{1,6}(?:[ \t]|$)|(?:[-+*]|1[.)])[ \t]+\S)`)

// Thematic breaks also end lazy continuation, but quoted/fenced breaks stay literal.
var thematicBreak = regexp.MustCompile(`(?m)^ {0,3}(?:(?:-[ \t]*){3,}|(?:\*[ \t]*){3,}|(?:_[ \t]*){3,})\r?$`)
var quoteTokens = regexp.MustCompile(`(?s)\\.|&#34;|&#39;|&quot;|["'“”‘’]`)

// Quotation pairing restarts at every block start, so an unbalanced quote
// never protects a later block. A heading is a single-line block, so the
// line after it starts a block too. HTML block starts follow CommonMark 4.6.
var atxHeading = regexp.MustCompile(`^ {0,3}#{1,6}(?:[ \t\r\n]|$)`)
var blockStart = regexp.MustCompile(`^(?: {0,3}(?:(?:[-+*]|[0-9]+[.)])[ \t]|>)|    |\t)`)

// The first capture marks paragraph-interrupting HTML. Standalone tags matched
// only by the last alternative reset quotations, but must not end a blockquote's
// lazy continuation or split inline code.
var htmlBlockStart = regexp.MustCompile(`(?i)^ {0,3}<(?:(!--|!\[CDATA\[|![a-z]|\?|(?:pre|script|style|textarea)(?:[ \t\r\n>]|$)|/?(?:address|article|aside|base|basefont|blockquote|body|caption|center|col|colgroup|dd|details|dialog|dir|div|dl|dt|fieldset|figcaption|figure|footer|form|frame|frameset|h[1-6]|head|header|hr|html|iframe|legend|li|link|main|menu|menuitem|nav|noframes|ol|optgroup|option|p|param|search|section|summary|table|tbody|td|tfoot|th|thead|title|tr|track|ul)(?:[ \t\r\n>]|/>|$))|/?[a-z][a-z0-9-]*(?:[ \t][^<>]*)?/?>[ \t\r]*(?:\n|$))`)

// Text strips "Captain," / "Captain:" from generated prose while retaining
// quoted text byte-for-byte. Callers must keep repository text and captured
// output in quotes/code, and must not pass raw files through this function.
// The result never grows, so publication byte limits remain valid.
func Text(text string) string {
	matches := address.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return text
	}
	quoted := make([]bool, len(text))
	protect := func(start, end int) {
		for i := start; i < end; i++ {
			quoted[i] = true
		}
	}

	// Preserve fenced and indented code, including unfinished fences, and
	// blockquotes with lazy paragraph continuation lines.
	var fence string
	// Quotation resets use all block starts; inline spans stop only at paragraph
	// interruptions, including new items in an existing list.
	var blockStarts, paragraphBreaks []int
	blockquote := false
	paragraphOpen := false
	listContents := []int{0}
	offset := 0
	for line := range strings.SplitAfterSeq(text, "\n") {
		trimmed := strings.TrimSpace(line)
		leading := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
		indent := len(leading) + 3*strings.Count(leading, "\t")
		rule := thematicBreak.MatchString(line)
		htmlBlock := htmlBlockStart.FindStringSubmatchIndex(line)
		htmlInterrupt := htmlBlock != nil && htmlBlock[2] >= 0
		paragraphInterrupt := blockquoteBreak.MatchString(line)
		listItem := false
		if fence == "" {
			item := listMarker.FindString(line[len(leading):])
			// Only sibling items bypass paragraph-interruption rules; a new
			// nested ordered list must still start at one.
			siblingItem := len(listContents) > 1 && indent < listContents[len(listContents)-1]
			switch {
			case rule:
				listContents = listContents[:1]
			case trimmed != "":
				if !paragraphOpen || item != "" || paragraphInterrupt || htmlInterrupt {
					for len(listContents) > 1 && indent < listContents[len(listContents)-1] {
						listContents = listContents[:len(listContents)-1]
					}
				}
				listItem = item != "" && indent < listContents[len(listContents)-1]+4 &&
					(!paragraphOpen || siblingItem || blockquoteBreak.MatchString(line[len(leading):]))
				if listItem {
					listContents = append(listContents, indent+len(item))
					trimmed = listMarker.ReplaceAllString(trimmed, "")
				}
			}
		}
		listContent := listContents[len(listContents)-1]
		marker := ""
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			marker = trimmed[:len(trimmed)-len(strings.TrimLeft(trimmed, trimmed[:1]))]
		}
		if fence == "" {
			heading := indent < listContent+4 && atxHeading.MatchString(trimmed)
			if heading || rule || trimmed == "" || marker != "" || blockStart.MatchString(line) || htmlBlock != nil {
				blockStarts = append(blockStarts, offset)
			}
			if heading || rule || trimmed == "" || marker != "" || paragraphInterrupt || htmlInterrupt || listItem {
				paragraphBreaks = append(paragraphBreaks, offset)
			}
			if heading {
				blockStarts = append(blockStarts, offset+len(line))
				paragraphBreaks = append(paragraphBreaks, offset+len(line))
			}
			paragraphOpen = trimmed != "" && !heading && !rule && marker == "" && !htmlInterrupt
		}
		switch {
		case fence != "":
			protect(offset, offset+len(line))
			if strings.HasPrefix(marker, fence) && strings.TrimSpace(strings.TrimPrefix(trimmed, marker)) == "" {
				fence = ""
				blockStarts = append(blockStarts, offset+len(line))
				paragraphBreaks = append(paragraphBreaks, offset+len(line))
			}
		case marker != "":
			blockquote = false
			fence = marker
			protect(offset, offset+len(line))
		case strings.HasPrefix(trimmed, ">"):
			blockquote = true
			protect(offset, offset+len(line))
		case rule, trimmed == "", paragraphInterrupt, htmlInterrupt:
			blockquote = false
		case blockquote, indent >= listContent+4:
			protect(offset, offset+len(line))
		}
		offset += len(line)
	}

	// Scan markup and inline code together, outside block evidence. The first
	// opener owns its contents: a tag inside backticks (or vice versa) is literal
	// and cannot protect unrelated prose after that span.
	for i := 0; i < len(text); {
		if quoted[i] {
			i++
			continue
		}
		end := i
		for end < len(text) && !quoted[end] {
			end++
		}
		for i < end {
			token := codeTokens.FindStringIndex(text[i:end])
			if token == nil {
				break
			}
			from, to := i+token[0], i+token[1]
			i = to
			if text[from] != '`' {
				protect(from, to)
				continue
			}
			// Inline spans require a matching run of ticks. An unmatched tick
			// is ordinary text; only unfinished block fences extend to EOF.
			spanEnd := end
			if nextBreak := sort.SearchInts(paragraphBreaks, to); nextBreak < len(paragraphBreaks) {
				spanEnd = min(spanEnd, paragraphBreaks[nextBreak])
			}
			for next := to; next < spanEnd; {
				tick := strings.IndexByte(text[next:spanEnd], '`')
				if tick < 0 {
					break
				}
				closeStart := next + tick
				next = closeStart
				for next < spanEnd && text[next] == '`' {
					next++
				}
				if next-closeStart == to-from {
					protect(from, next)
					i = next
					break
				}
			}
		}
		i = end
	}
	closing, quoteStart := "", 0
	for _, token := range quoteTokens.FindAllStringIndex(text, -1) {
		from, to := token[0], token[1]
		for len(blockStarts) > 0 && blockStarts[0] <= from {
			blockStarts, closing = blockStarts[1:], ""
		}
		if quoted[from] || text[from] == '\\' {
			continue
		}
		delimiter := text[from:to]
		if delimiter == "'" || delimiter == "&#39;" || delimiter == "’" {
			prev, _ := utf8.DecodeLastRuneInString(text[:from])
			next, _ := utf8.DecodeRuneInString(text[to:])
			if (unicode.IsLetter(prev) || unicode.IsNumber(prev)) &&
				(closing == "" || unicode.IsLetter(next) || unicode.IsNumber(next)) {
				continue
			}
		}
		if closing == "" {
			switch delimiter {
			case "”", "’":
				continue
			case "“":
				closing = "”"
			case "‘":
				closing = "’"
			default:
				closing = delimiter
			}
			quoteStart = from
		} else if delimiter == closing {
			protect(quoteStart, to)
			closing = ""
		}
	}

	var out strings.Builder
	start := 0
	for _, match := range matches {
		from, to := match[6], match[9]
		if quoted[from] {
			continue
		}
		opening := text[match[4]:match[5]]
		after := text[match[10]:match[11]]
		if after != "" && after == opening {
			from, to = match[4], match[13]
		} else if after == "" {
			to = match[13]
		}
		out.WriteString(text[start:from])
		start = to
	}
	if start == 0 {
		return text
	}
	out.WriteString(text[start:])
	return out.String()
}
