package steps

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

const (
	prAppendixNamespace = "no-mistakes-pr-appendix"
	prAppendixStart     = "<!-- " + prAppendixNamespace + ":v1 sha256="
	prAppendixEnd       = "<!-- /" + prAppendixNamespace + ":v1 -->"
)

var prAppendixMarkerPattern = regexp.MustCompile(`(?i)<!--\s*/?\s*` + prAppendixNamespace)

func hasPRAppendixMarkers(body string) bool {
	return prAppendixMarkerPattern.MatchString(body)
}

type prOwnedBody struct {
	before, appendix, after string
	managed                 bool
}

// The digest is an accidental-edit/ownership guard, NOT authentication. PR
// authors can edit every byte, including the attestation. We never infer
// ownership from headings: only an intact, unchanged, explicitly delimited
// appendix is replaceable. Editing inside it fails instead of erasing notes.
func parsePROwnedBody(body string) (prOwnedBody, error) {
	if !hasPRAppendixMarkers(body) {
		if strings.Contains(body, pipelineAttestationCommentPrefix) {
			return prOwnedBody{}, fmt.Errorf("existing PR has an unowned legacy attestation; explicitly separate its generated evidence before enabling pr.template")
		}
		return prOwnedBody{before: body}, nil
	}
	start := strings.Index(body, prAppendixStart)
	end := strings.Index(body, prAppendixEnd)
	if len(prAppendixMarkerPattern.FindAllStringIndex(body, -1)) != 2 || start < 0 || end < start || (start > 0 && body[start-1] != '\n') || markdownFenceOpen(body[:max(start, 0)]) {
		return prOwnedBody{}, fmt.Errorf("ambiguous PR appendix ownership markers; refusing to discard author content")
	}
	digestStart := start + len(prAppendixStart)
	contentStart := digestStart + 64 + len(" -->\n")
	afterEnd := end + len(prAppendixEnd)
	if contentStart >= end || body[digestStart+64:contentStart] != " -->\n" || body[end-1] != '\n' || (afterEnd < len(body) && body[afterEnd] != '\n') {
		return prOwnedBody{}, fmt.Errorf("malformed PR appendix ownership markers")
	}
	parts := prOwnedBody{before: body[:start], appendix: body[contentStart : end-1], after: body[afterEnd:], managed: true}
	if markdownFenceOpen(parts.appendix) || fmt.Sprintf("%x", sha256.Sum256([]byte(parts.appendix))) != body[digestStart:digestStart+64] {
		return prOwnedBody{}, fmt.Errorf("PR appendix was edited or is malformed; refusing to overwrite possible author content")
	}
	if strings.Contains(parts.before+parts.after, pipelineAttestationCommentPrefix) || strings.Count(parts.appendix, pipelineAttestationCommentPrefix) != 1 {
		return prOwnedBody{}, fmt.Errorf("ambiguous PR attestation ownership")
	}
	marker := strings.SplitN(parts.appendix, pipelineAttestationCommentPrefix, 2)[1]
	payload, _, ok := strings.Cut(marker, pipelineAttestationCommentClosingToken)
	var attestation pipelineAttestation
	if !ok || json.Unmarshal([]byte(payload), &attestation) != nil || attestation.HeadSHA == "" {
		return prOwnedBody{}, fmt.Errorf("malformed attestation in PR appendix")
	}
	return parts, nil
}

// Only track fenced blocks to keep a quoted marker from acquiring ownership.
// This deliberately does not interpret headings or attempt general Markdown
// rewriting. Four-space-indented lines cannot open a CommonMark fence.
func markdownFenceOpen(text string) bool {
	var fence markdownFence
	for _, line := range strings.Split(text, "\n") {
		fence.consume(line)
	}
	return fence.marker != 0
}

type markdownFence struct {
	marker byte
	width  int
}

func (f *markdownFence) consume(raw string) {
	line := strings.TrimLeft(raw, " ")
	if len(raw)-len(line) > 3 || len(line) < 3 || (line[0] != '`' && line[0] != '~') {
		return
	}
	n := 0
	for n < len(line) && line[n] == line[0] {
		n++
	}
	if n < 3 {
		return
	}
	if f.marker == 0 {
		// CommonMark forbids backticks in a backtick fence's info string.
		if line[0] == '`' && strings.ContainsRune(line[n:], '`') {
			return
		}
		f.marker, f.width = line[0], n
	} else if line[0] == f.marker && n >= f.width && strings.TrimSpace(line[n:]) == "" {
		f.marker, f.width = 0, 0
	}
}

func wrapPRAppendix(appendix string) string {
	return fmt.Sprintf("%s%x -->\n%s\n%s", prAppendixStart, sha256.Sum256([]byte(appendix)), appendix, prAppendixEnd)
}

// composeOwnedPRContent uses the same publication redaction owner as ordinary
// drafting, BEFORE stamping the byte-integrity guard. No clamp or heading-based
// stripper may run here: if author text and all recorded evidence cannot fit,
// publication fails. That includes suffix text/closing lines added after the
// generated block. Model-authored copies of ownership markers in evidence are
// escaped before the actual delimiters are inserted, like foreign attestations.
func composeOwnedPRContent(parts prOwnedBody, title, appendix string, bodyLimit int) (prContent, error) {
	before := redactPRContent(prContent{Body: parts.before}).Body
	after := redactPRContent(prContent{Body: parts.after}).Body
	appendix = prAppendixMarkerPattern.ReplaceAllStringFunc(appendix, func(marker string) string {
		// Break the namespace without rewriting ordinary mentions of its name.
		i := strings.LastIndex(marker, "-")
		return marker[:i] + "\\" + marker[i:]
	})
	appendix = redactPRContent(prContent{Body: appendix}).Body
	if !parts.managed && before != "" {
		before += "\n\n"
	}
	content := prContent{Title: redactPRContent(prContent{Title: title}).Title, Body: before + wrapPRAppendix(appendix) + after}
	if err := validateOwnedPRBudget(content.Body, bodyLimit); err != nil {
		return prContent{}, err
	}
	if _, err := parsePROwnedBody(content.Body); err != nil {
		return prContent{}, err
	}
	return content, nil
}

func validateOwnedPRBudget(body string, bodyLimit int) error {
	if len(body) > maxPullRequestBodyBytes || (bodyLimit > 0 && scm.PRBodyLen(body) > bodyLimit) {
		return fmt.Errorf("PR body exceeds provider budget; refusing to drop author text, closing references or recorded evidence")
	}
	return nil
}

// updateOwnedPR re-reads before the full-body write and verifies afterwards.
// This is NOT compare-and-swap: providers expose full-body writes. Detected
// pre-write edits are merged from their latest version (bounded); a write error
// or post-write divergence fails without replaying a potentially applied write.
func updateOwnedPR(sctx *pipeline.StepContext, host scm.Host, pr *scm.PR, initial scm.PRContent, title, emptyNarrative, appendix string, bodyLimit int) error {
	reader, ok := host.(scm.PRContentReader)
	if !ok {
		return fmt.Errorf("provider cannot read PR content; author-safe template updates are unsupported")
	}
	current := initial
	for attempt := 0; attempt < 3; attempt++ {
		parts, err := parsePROwnedBody(current.Body)
		if err != nil {
			return err
		}
		if current.Body == "" && !parts.managed {
			parts.before = emptyNarrative
		}
		content, err := composeOwnedPRContent(parts, title, appendix, bodyLimit)
		if err != nil {
			return err
		}
		latest, err := reader.GetPRContent(sctx.Ctx, pr)
		if err != nil {
			return fmt.Errorf("re-read PR before template update: %w", err)
		}
		if latest.Body != current.Body {
			current = latest
			continue
		}
		if content.Body != current.Body || content.Title != "" {
			if _, err := host.UpdatePR(sctx.Ctx, pr, scm.PRContent(content)); err != nil {
				return fmt.Errorf("update templated PR: %w", err)
			}
		}
		verified, err := reader.GetPRContent(sctx.Ctx, pr)
		if err != nil {
			return fmt.Errorf("verify templated PR update: %w", err)
		}
		if verified.Body != content.Body {
			return fmt.Errorf("PR body changed or update did not settle; refusing to report successful publication")
		}
		return nil
	}
	return fmt.Errorf("PR body kept changing before template update; no write performed")
}

// Restamping is also an owner-authorized appendix edit. Recompute its integrity
// guard without touching author text, while refusing evidence edited by anyone
// else. Legacy unmarked bodies retain the existing restamp contract.
func rebindOwnedPRAttestation(body, head string, steps []*db.StepResult) (string, bool, error) {
	if !hasPRAppendixMarkers(body) {
		updated, rebound := rebindPipelineAttestationWithSteps(body, head, steps)
		return updated, rebound, nil
	}
	parts, err := parsePROwnedBody(body)
	if err != nil {
		return "", false, err
	}
	appendix, rebound := rebindPipelineAttestationWithSteps(parts.appendix, head, steps)
	if !rebound {
		return "", false, fmt.Errorf("cannot rebind the owned PR attestation")
	}
	updated := parts.before + wrapPRAppendix(appendix) + parts.after
	if len(updated) > maxPullRequestBodyBytes {
		return "", false, fmt.Errorf("restamped PR body exceeds the publication budget")
	}
	return updated, true, nil
}
