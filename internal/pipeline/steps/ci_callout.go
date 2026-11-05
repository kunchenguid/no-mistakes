package steps

import (
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// PublishGateDecision refreshes the PR's Not validated live callout after an
// operator skipped CI or approved past checks that were not green. The PR step
// writes the callout before CI runs, so without this the published body would
// miss a CI-time exception that axi's run view lists. Only the callout section
// changes; the rest of the body, and an owned appendix's digest, are kept. A
// host that cannot read PR content is skipped with a log line, like the
// attestation restamp. Any other failure is returned so the run does not
// finish over a stale callout.
func (s *CIStep) PublishGateDecision(sctx *pipeline.StepContext) error {
	prURL := ""
	if sctx.Run.PRURL != nil {
		prURL = strings.TrimSpace(*sctx.Run.PRURL)
	}
	if prURL == "" {
		return nil
	}
	provider := resolvedProvider(sctx)
	host, reason := buildHost(sctx, provider)
	if host == nil {
		return fmt.Errorf("refresh Not validated live callout: %s", reason)
	}
	if err := host.Available(sctx.Ctx); err != nil {
		return fmt.Errorf("refresh Not validated live callout: %w", err)
	}
	number, err := scm.ExtractPRNumber(prURL)
	if err != nil {
		return fmt.Errorf("refresh Not validated live callout: %w", err)
	}
	callout, err := validationCallout(sctx, provider)
	if err != nil {
		return fmt.Errorf("read validation state for PR callout: %w", err)
	}
	// The same transforms an owned appendix gets before its digest is
	// stamped, so an unchanged callout splices back byte for byte.
	callout = redactPRContent(prContent{Body: escapePRAppendixMarkers(neutralizePRControls(callout))}, sctx.WorkDir, nmWorktreesRoot()).Body
	limit := scm.HostMaxPRBodyChars(host)
	return rewritePRBody(sctx.Ctx, host, &scm.PR{Number: number, URL: prURL}, "validation callout refresh", "refreshed the Not validated live callout", sctx.Log, func(body string) (string, error) {
		return refreshValidationCallout(body, callout, limit)
	})
}

// refreshValidationCallout splices callout into a published body. An owned
// body gets it inside its appendix, which is re-wrapped with a new digest; an
// appendix edited by someone else is refused, as the restamp refuses it. A
// refreshed body that no longer fits the provider limit is refused rather than
// truncated.
func refreshValidationCallout(body, callout string, bodyLimit int) (string, error) {
	var updated string
	if !hasPRAppendixMarkers(body) {
		updated = spliceValidationCallout(body, callout)
	} else {
		parts, err := parsePROwnedBody(body)
		if err != nil {
			return "", err
		}
		appendix := spliceValidationCallout(parts.appendix, callout)
		if appendix == parts.appendix {
			return body, nil
		}
		updated = parts.before + wrapPRAppendix(appendix) + parts.after
	}
	if updated != body {
		if err := validateOwnedPRBudget(updated, bodyLimit); err != nil {
			return "", err
		}
	}
	return updated, nil
}

// spliceValidationCallout replaces the Not validated live section of text, or
// inserts it where the PR step puts it: after a leading Intent section, or at
// the top. A section runs to the next "## " heading outside a code fence, the
// boundary stripGeneratedSections uses. An empty callout removes the section.
func spliceValidationCallout(text, callout string) string {
	type heading struct {
		offset int
		text   string
	}
	var headings []heading
	var fence markdownFence
	offset := 0
	for _, line := range strings.SplitAfter(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if fence.marker == 0 && strings.HasPrefix(trimmed, "## ") {
			headings = append(headings, heading{offset, trimmed})
		}
		fence.consume(strings.TrimSuffix(line, "\n"))
		offset += len(line)
	}
	sectionEnd := func(i int) int {
		if i+1 < len(headings) {
			return headings[i+1].offset
		}
		return len(text)
	}
	start, end := -1, -1
	for i, h := range headings {
		if h.text == notValidatedLiveHeading {
			start, end = h.offset, sectionEnd(i)
			break
		}
	}
	if start < 0 {
		if callout == "" {
			return text
		}
		start, end = 0, 0
		if len(headings) > 0 && headings[0].text == "## Intent" && strings.TrimSpace(text[:headings[0].offset]) == "" {
			start = sectionEnd(0)
			end = start
		}
	}
	// Blocks are separated by one blank line, as joinBlocks separates them,
	// but the text around the section is kept byte for byte so an unchanged
	// callout splices back to the same text.
	var out strings.Builder
	for _, block := range []string{strings.TrimRight(text[:start], "\n"), callout, strings.TrimLeft(text[end:], "\n")} {
		if block == "" {
			continue
		}
		if out.Len() > 0 {
			out.WriteString("\n\n")
		}
		out.WriteString(block)
	}
	return out.String()
}
