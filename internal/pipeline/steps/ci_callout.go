package steps

import (
	"errors"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// PublishGateDecision refreshes the PR's Not validated live callout after an
// operator skipped CI or approved past checks that were not green. The PR step
// writes the callout before CI runs, so without this the published body would
// miss a CI-time exception that axi's run view lists. Only the callout's own
// lines change; every other byte of the body, and an owned appendix's digest,
// are kept. A host that cannot read PR content is skipped with a log line,
// like the attestation restamp. Any other failure is returned so the run does
// not finish over a stale callout.
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
	refresh, err := validationCalloutRefresh(sctx, provider, scm.HostMaxPRBodyChars(host))
	if err != nil {
		return err
	}
	return rewritePRBody(sctx.Ctx, host, &scm.PR{Number: number, URL: prURL}, "validation callout refresh", "refreshed the Not validated live callout", sctx.Log, refresh)
}

// validationCalloutRefresh returns the body transform PublishGateDecision
// applies: the run's current callout and the Intent section it must follow,
// both in the form the PR step published them.
func validationCalloutRefresh(sctx *pipeline.StepContext, provider scm.Provider, bodyLimit int) (func(body string) (string, error), error) {
	callout, err := validationCallout(sctx, provider)
	if err != nil {
		return nil, fmt.Errorf("read validation state for PR callout: %w", err)
	}
	// The transforms an owned appendix gets before its digest is stamped; an
	// ordinary body gets the same redaction and control neutralization.
	published := func(text string) string {
		return redactPRContent(prContent{Body: escapePRAppendixMarkers(neutralizePRControls(text))}, sctx.WorkDir, nmWorktreesRoot()).Body
	}
	intent := publishedIntent{}
	switch {
	case publicPRIntentSource(sctx) == "":
	case needsPRIntentBrief(sctx):
		intent.brief = true
	default:
		intent.exact = published(strings.TrimSpace("## Intent\n\n" + neutralizeAttestationMarkers(provider, publicPRIntent(sctx))))
	}
	callout = published(callout)
	return func(body string) (string, error) {
		return refreshValidationCallout(body, callout, intent, bodyLimit)
	}, nil
}

// refreshValidationCallout splices callout into a published body. An owned
// body gets it inside its appendix, which is re-wrapped with a new digest; an
// appendix edited by someone else is refused, as the restamp refuses it. A
// refreshed body that no longer fits the provider limit is refused rather than
// truncated.
func refreshValidationCallout(body, callout string, intent publishedIntent, bodyLimit int) (string, error) {
	var updated string
	if !hasPRAppendixMarkers(body) {
		var err error
		if updated, err = spliceValidationCallout(body, callout, intent); err != nil {
			return "", err
		}
	} else {
		parts, err := parsePROwnedBody(body)
		if err != nil {
			return "", err
		}
		appendix, err := spliceValidationCallout(parts.appendix, callout, intent)
		if err != nil {
			return "", err
		}
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

// publishedIntent is the Intent section the PR step put at the top of the
// body (or of the owned appendix), which the callout follows. exact is its
// published text when the run's intent was published verbatim; brief marks a
// configured public brief, which is found by its fixed rendered shape because
// the brief itself is not kept after the PR step. Neither means no Intent
// section was published and the callout leads the text.
type publishedIntent struct {
	exact string
	brief bool
}

var errCalloutPlacement = errors.New("PR body does not have the lead the PR step published; refusing to place the Not validated live callout")

// end returns the offset just past the Intent section at the start of text,
// or 0 when none was published.
func (p publishedIntent) end(text string) (int, error) {
	end := 0
	switch {
	case p.exact != "":
		if !strings.HasPrefix(text, p.exact) {
			return 0, errCalloutPlacement
		}
		end = len(p.exact)
	case p.brief:
		// "## Intent", a blank line, the goal line, then any of the labeled
		// sections, each a "### " line followed by "- " bullet lines
		// (renderPRIntentBrief).
		const head = "## Intent\n\n"
		if !strings.HasPrefix(text, head) {
			return 0, errCalloutPlacement
		}
		// The sections come in configured order, so match any label.
		end = lineEnd(text, len(head))
		for matched := true; matched; {
			matched = false
			for _, label := range []string{"Why", "Scope", "Out of scope"} {
				if section := "\n\n### " + label + "\n- "; strings.HasPrefix(text[end:], section) {
					end = lineEnd(text, end+len(section))
					for strings.HasPrefix(text[end:], "\n- ") {
						end = lineEnd(text, end+1)
					}
					matched = true
				}
			}
		}
	default:
		return 0, nil
	}
	if end < len(text) && !strings.HasPrefix(text[end:], "\n\n") {
		return 0, errCalloutPlacement
	}
	return end, nil
}

// calloutEnd returns the offset just past a published callout that starts at
// start: its heading, a blank line, the summary line, a blank line, and its
// consecutive "- " item lines (renderValidationCallout; every item is one
// line). found is false when no callout starts there.
func calloutEnd(text string, start int) (end int, found bool, err error) {
	if !strings.HasPrefix(text[start:], notValidatedLiveHeading+"\n\n") {
		return 0, false, nil
	}
	summaryStart := start + len(notValidatedLiveHeading) + 2
	end = lineEnd(text, summaryStart)
	if end == summaryStart || strings.HasPrefix(text[summaryStart:], "- ") || !strings.HasPrefix(text[end:], "\n\n- ") {
		return 0, false, errors.New("published Not validated live callout is not in its rendered shape; refusing to rewrite it")
	}
	end = lineEnd(text, end+2)
	for strings.HasPrefix(text[end:], "\n- ") {
		end = lineEnd(text, end+1)
	}
	return end, true, nil
}

func lineEnd(text string, from int) int {
	if i := strings.IndexByte(text[from:], '\n'); i >= 0 {
		return from + i
	}
	return len(text)
}

// spliceValidationCallout replaces the published callout in text, or inserts
// one where the PR step puts it: directly after the leading Intent section,
// or at the top. Only the callout's own lines and one separating blank line
// change; an empty callout removes it.
func spliceValidationCallout(text, callout string, intent publishedIntent) (string, error) {
	lead, err := intent.end(text)
	if err != nil {
		return "", err
	}
	start := lead
	if lead > 0 && lead < len(text) {
		start += len("\n\n")
	}
	rest := text[start:]
	end, found, err := calloutEnd(text, start)
	if err != nil {
		return "", err
	}
	if found {
		rest = strings.TrimPrefix(text[end:], "\n\n")
	} else if callout == "" {
		return text, nil
	}
	var out strings.Builder
	for _, block := range []string{text[:lead], callout, rest} {
		if block == "" {
			continue
		}
		if out.Len() > 0 && strings.TrimSpace(block) != "" {
			out.WriteString("\n\n")
		}
		out.WriteString(block)
	}
	return out.String(), nil
}
