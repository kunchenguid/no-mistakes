// Package publicprose removes known operator address from generated prose.
// It is not a repository-content filter or a detector of personal names.
package publicprose

import (
	"regexp"
	"strings"
)

// Only the reported vocative forms at a prose boundary are removed. A domain
// phrase such as "the captain, crew and ship" is not operator address.
var address = regexp.MustCompile(`(?im)(^[^\pL\pN\r\n]*|[.!?:][ \t]+|[ \t]+-[ \t]+|>[ \t]*)(captain[ \t]*[:,][ \t]*)`)

var quotations = regexp.MustCompile(`(?s)<!--.*?(?:-->|$)|(?i:<pre\b[^>]*>.*?(?:</pre\s*>|$)|<code\b[^>]*>.*?(?:</code\s*>|$))|<[^>\n]*>|"(?:\\.|[^"\\])*"|(?:^|[\s(])'(?:\\.|[^'\\])*'|“[^”]*”|‘[^’]*’|&#34;.*?&#34;|&#39;.*?&#39;|&quot;.*?&quot;`)

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
			fence = marker
			protect(offset, offset+len(line))
		case strings.HasPrefix(trimmed, ">"):
			blockquote = true
			protect(offset, offset+len(line))
		case trimmed == "":
			blockquote = false
		case blockquote, strings.HasPrefix(line, "    "), strings.HasPrefix(line, "\t"):
			protect(offset, offset+len(line))
		}
		offset += len(line)
	}

	// Backtick spans may use multiple ticks and span lines. Match the same
	// delimiter length, so an embedded single tick cannot end a double span.
	for i := 0; i < len(text); i++ {
		if text[i] != '`' || quoted[i] {
			continue
		}
		start := i
		for i < len(text) && text[i] == '`' {
			i++
		}
		marker := text[start:i]
		end := i
		for end < len(text) {
			next := strings.Index(text[end:], marker)
			if next < 0 {
				end = len(text)
				break
			}
			end += next + len(marker)
			if (end == len(text) || text[end] != '`') && text[end-len(marker)-1] != '`' {
				break
			}
		}
		protect(start, end)
		i = end - 1
	}
	for _, span := range quotations.FindAllStringIndex(text, -1) {
		protect(span[0], span[1])
	}

	var out strings.Builder
	start := 0
	for _, match := range matches {
		from, to := match[4], match[5] // address only; retain its prose prefix
		if quoted[from] {
			continue
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
