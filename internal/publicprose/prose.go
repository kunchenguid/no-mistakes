// Package publicprose removes known operator address from generated prose.
// It is not a repository-content filter or a detector of personal names.
package publicprose

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Only the reported vocative forms at a prose boundary are removed. A domain
// phrase such as "the captain, crew and ship" is not operator address.
var address = regexp.MustCompile(`(?im)(^[^\pL\pN\r\n]*?|[.!?:][ \t]+|[ \t]+-[ \t]+|>[ \t]*)([*_]*)(captain[ \t]*)([*_]*)([:,])([*_]*)([ \t]*)`)

var codeTokens = regexp.MustCompile(`(?s)<!--.*?(?:-->|$)|(?i:<pre\b[^>]*>.*?(?:</pre\s*>|$)|<code\b[^>]*>.*?(?:</code\s*>|$))|<[^>\n]*>|` + "`+")
var listMarker = regexp.MustCompile(`^(?:[-+*]|[0-9]+[.)])[ \t]+`)

// Headings and nonempty list items interrupt a quoted paragraph's lazy
// continuation. Ordered lists can interrupt only when they start at one.
var blockquoteBreak = regexp.MustCompile(`^ {0,3}(?:#{1,6}(?:[ \t]|$)|(?:[-+*]|1[.)])[ \t]+\S)`)
var quoteTokens = regexp.MustCompile(`(?s)\\.|&#34;|&#39;|&quot;|["'“”‘’]`)

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
	blockquote := false
	offset := 0
	for line := range strings.SplitAfterSeq(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if fence == "" {
			trimmed = listMarker.ReplaceAllString(trimmed, "")
		}
		marker := ""
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			marker = trimmed[:len(trimmed)-len(strings.TrimLeft(trimmed, trimmed[:1]))]
		}
		switch {
		case fence != "":
			protect(offset, offset+len(line))
			if strings.HasPrefix(marker, fence) && strings.TrimSpace(strings.TrimPrefix(trimmed, marker)) == "" {
				fence = ""
			}
		case marker != "":
			blockquote = false
			fence = marker
			protect(offset, offset+len(line))
		case strings.HasPrefix(trimmed, ">"):
			blockquote = true
			protect(offset, offset+len(line))
		case trimmed == "" || blockquoteBreak.MatchString(line):
			blockquote = false
		case blockquote, strings.HasPrefix(line, "    "), strings.HasPrefix(line, "\t"):
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
			for next := to; next < end; {
				tick := strings.IndexByte(text[next:end], '`')
				if tick < 0 {
					break
				}
				closeStart := next + tick
				next = closeStart
				for next < end && text[next] == '`' {
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
		from, to := match[6], match[11]
		if quoted[from] {
			continue
		}
		opening := text[match[4]:match[5]]
		before := text[match[8]:match[9]]
		after := text[match[12]:match[13]]
		if before != "" {
			if before != opening {
				continue
			}
			from = match[4]
		}
		if after != "" && after == opening && before == "" {
			from, to = match[4], match[15]
		} else if after == "" {
			to = match[15]
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
