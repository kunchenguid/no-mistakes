package scm

import "strings"

// prBodyTruncationMarker is appended by ClampPRBody when it has to cut a body,
// so a shortened description is visibly marked rather than ending mid-text.
const prBodyTruncationMarker = "\n\n…(description truncated)"

// PRBodyLen reports the length of s the way Azure DevOps (a .NET service)
// measures a PR description: in UTF-16 code units, so a non-BMP rune (an emoji,
// some CJK) counts as two. It is the strictest common denominator across
// providers, so a body that "fits" by this measure is genuinely safe to send.
func PRBodyLen(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// MaxPRBodyChars returns the hard limit a provider enforces on a PR description,
// measured in PRBodyLen units, or 0 when the provider imposes no practical
// limit. Azure DevOps rejects `az repos pr create`/`update` with "Invalid
// argument value. ... A description for a pull request must not be longer than
// 4000 characters."; GitHub and GitLab allow far larger bodies than this tool
// ever produces, so they report 0 (unlimited).
func MaxPRBodyChars(p Provider) int {
	switch p {
	case ProviderAzureDevOps:
		return 4000
	default:
		return 0
	}
}

// ClampPRBody truncates body to at most max PRBodyLen units, cutting on a rune
// boundary and appending a truncation marker (kept inside the budget) when it
// cuts. max <= 0 means unlimited and returns body unchanged. This is the
// last-resort backstop: callers that can shed whole sections to fit a budget
// should do so before relying on a blind clamp.
func ClampPRBody(body string, max int) string {
	if max <= 0 || PRBodyLen(body) <= max {
		return body
	}
	budget := max - PRBodyLen(prBodyTruncationMarker)
	if budget < 0 {
		budget = 0
	}
	var b strings.Builder
	used := 0
	for _, r := range body {
		w := 1
		if r > 0xFFFF {
			w = 2
		}
		if used+w > budget {
			break
		}
		b.WriteRune(r)
		used += w
	}
	b.WriteString(prBodyTruncationMarker)
	return b.String()
}
