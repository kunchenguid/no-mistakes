package steps

import (
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// Closed by default: GitHub, GitLab, Gitea, Forgejo, and Azure render a
// details block shut until a reviewer opens it. The summary is the only
// visible label.
const (
	validationDetailsOpen  = "<details>\n<summary>Validation</summary>\n\n"
	validationDetailsClose = "\n</details>"
)

func appendixMode(sctx *pipeline.StepContext) string {
	if sctx == nil || sctx.Config == nil {
		return config.PRAppendixFull
	}
	return sctx.Config.PR.AppendixMode()
}

// appendixEvidence is the recorded Risk, Testing, and Pipeline tail.
// Intent is the caller's job: it stays outside a collapsed Validation block.
func appendixEvidence(mode string, flavor prBodyFlavor, risk, testing, pipeline string, provider scm.Provider) string {
	switch mode {
	case config.PRAppendixMinimal:
		return minimalTail(risk, pipeline, provider)
	case config.PRAppendixCollapsed:
		if flavor == prBodyHTML {
			return wrapValidation(joinAppendixSections(risk, testing, pipeline, provider))
		}
	}
	return joinAppendixSections(risk, testing, pipeline, provider)
}

func joinAppendixSections(risk, testing, pipeline string, provider scm.Provider) string {
	var parts []string
	if strings.TrimSpace(risk) != "" {
		parts = append(parts, "## Risk Assessment\n\n"+neutralizeAttestationMarkers(provider, risk))
	}
	if strings.TrimSpace(testing) != "" {
		parts = append(parts, neutralizeAttestationMarkers(provider, testing))
	}
	if pipeline != "" {
		parts = append(parts, pipeline)
	}
	return strings.Join(parts, "\n\n")
}

func wrapValidation(inner string) string {
	inner = strings.Trim(inner, "\n")
	if inner == "" {
		return ""
	}
	return validationDetailsOpen + inner + validationDetailsClose
}

// minimalTail is the one-line risk level plus the attestation the pipeline
// section already carries. Ordinary Bitbucket descriptions carry no
// attestation comment; this does not invent one. An owned Bitbucket body
// carries that comment inside a text fence, and the fence is kept so the
// marker stays visible there.
func minimalTail(risk, pipelineMD string, provider scm.Provider) string {
	return joinBlocks(oneLineRisk(neutralizeAttestationMarkers(provider, risk)), noMistakesPRSignature, carriedAttestation(pipelineMD))
}

func oneLineRisk(risk string) string {
	return strings.Join(strings.Fields(risk), " ")
}

func carriedAttestation(pipelineMD string) string {
	marker := extractPipelineAttestationMarker(pipelineMD)
	if marker == "" {
		return ""
	}
	fenced := "```text\n" + marker + "\n```"
	if strings.Contains(pipelineMD, fenced) {
		return fenced
	}
	return marker
}

func extractPipelineAttestationMarker(pipelineMD string) string {
	start := strings.Index(pipelineMD, pipelineAttestationCommentPrefix)
	if start < 0 {
		return ""
	}
	rest := pipelineMD[start:]
	end := strings.Index(rest, pipelineAttestationCommentClosingToken)
	if end < 0 {
		return ""
	}
	return rest[:end+len(pipelineAttestationCommentClosingToken)]
}

func joinBlocks(parts ...string) string {
	var out []string
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return strings.Join(out, "\n\n")
}

func narrativeWithIntent(sctx *pipeline.StepContext, whatChanged string) string {
	narrative := neutralizeAttestationMarkers(resolvedProviderForBody(sctx), stripGeneratedSections(whatChanged))
	return prependIntentSection(narrative, sctx)
}

func assemblePRBody(sctx *pipeline.StepContext, whatChanged, riskLine, testingMD, pipelineMD string, bodyLimit int, provider scm.Provider) string {
	switch appendixMode(sctx) {
	case config.PRAppendixMinimal:
		return assembleMinimalPRBody(sctx, whatChanged, riskLine, pipelineMD, bodyLimit, provider)
	case config.PRAppendixCollapsed:
		if prBodyFlavorFor(provider) == prBodyHTML {
			return assembleCollapsedPRBody(sctx, whatChanged, riskLine, testingMD, pipelineMD, bodyLimit, provider)
		}
	}
	return assemblePRBodyFull(sctx, whatChanged, riskLine, testingMD, pipelineMD, bodyLimit, provider)
}

func assembleMinimalPRBody(sctx *pipeline.StepContext, whatChanged, riskLine, pipelineMD string, bodyLimit int, provider scm.Provider) string {
	prefix := narrativeWithIntent(sctx, whatChanged)
	tail := minimalTail(riskLine, pipelineMD, provider)
	full := joinBlocks(prefix, tail)
	if bodyLimit <= 0 || scm.PRBodyLen(full) <= bodyLimit {
		return full
	}
	return shrinkMeasuredKeepingTail(full, tail, bodyLimit, scm.PRBodyLen, scm.ClampPRBody)
}

func assembleCollapsedPRBody(sctx *pipeline.StepContext, whatChanged, riskLine, testingMD, pipelineMD string, bodyLimit int, provider scm.Provider) string {
	prefix := narrativeWithIntent(sctx, whatChanged)
	if bodyLimit <= 0 {
		return joinBlocks(prefix, wrapValidation(joinAppendixSections(riskLine, testingMD, pipelineMD, provider)))
	}
	return fitCollapsed(prefix, riskLine, testingMD, pipelineMD, bodyLimit, scm.PRBodyLen, scm.ClampPRBody, provider)
}

func buildPRBody(body, riskLine, testingMD, pipelineMD string, sctx *pipeline.StepContext, provider scm.Provider) string {
	return buildPRBodyWithin(body, riskLine, testingMD, pipelineMD, sctx, provider, maxPullRequestBodyBytes)
}

// buildPRBodyWithin is buildPRBody under a byte budget below the default,
// which the Issues section uses to reserve its own room.
func buildPRBodyWithin(body, riskLine, testingMD, pipelineMD string, sctx *pipeline.StepContext, provider scm.Provider, maxBytes int) string {
	switch appendixMode(sctx) {
	case config.PRAppendixMinimal:
		return buildMinimalPRBody(body, riskLine, pipelineMD, sctx, maxBytes, provider)
	case config.PRAppendixCollapsed:
		if prBodyFlavorFor(provider) == prBodyHTML {
			return buildCollapsedPRBody(body, riskLine, testingMD, pipelineMD, sctx, maxBytes, provider)
		}
	}
	return buildPRBodyFull(body, riskLine, testingMD, pipelineMD, sctx, maxBytes, provider)
}

func buildMinimalPRBody(body, riskLine, pipelineMD string, sctx *pipeline.StepContext, maxBytes int, provider scm.Provider) string {
	prefix := narrativeWithIntent(sctx, body)
	tail := minimalTail(riskLine, pipelineMD, provider)
	full := joinBlocks(prefix, tail)
	if len(full) <= maxBytes {
		return full
	}
	return shrinkMeasuredKeepingTail(full, tail, maxBytes, func(s string) int { return len(s) }, clampPRBytes)
}

func buildCollapsedPRBody(body, riskLine, testingMD, pipelineMD string, sctx *pipeline.StepContext, maxBytes int, provider scm.Provider) string {
	prefix := narrativeWithIntent(sctx, body)
	return fitCollapsed(prefix, riskLine, testingMD, pipelineMD, maxBytes, func(s string) int { return len(s) }, clampPRBytes, provider)
}

func clampPRBytes(text string, max int) string {
	return truncateTextAtLineBoundary(text, max, essentialPRBodyTruncationMarker())
}

// fitCollapsed keeps the narrative outside one closed Validation block.
// Testing is dropped before pipeline prose, matching the ordinary budget
// order. The attestation stays inside the block because the inner truncator
// keeps the pipeline header.
func fitCollapsed(prefix, risk, testing, pipeline string, limit int, units func(string) int, clamp func(string, int) string, provider scm.Provider) string {
	untruncated := func(testingMD string) string {
		return joinBlocks(prefix, wrapValidation(joinAppendixSections(risk, testingMD, pipeline, provider)))
	}
	if full := untruncated(testing); units(full) <= limit {
		return full
	}
	// Testing is the section that inlines evidence logs. Drop it whole before
	// cutting pipeline history, the same order the ordinary body uses.
	if testing != "" {
		if dropped := untruncated(""); units(dropped) <= limit {
			return dropped
		}
	}
	folded := foldedWithin(risk, "", pipeline, limit, provider)
	if folded == "" {
		return shrinkMeasuredKeepingTail(joinBlocks(prefix, carriedAttestation(pipeline)), carriedAttestation(pipeline), limit, units, clamp)
	}
	sep := 0
	if strings.TrimSpace(prefix) != "" {
		sep = units("\n\n")
	}
	remain := limit - units(folded) - sep
	trimmed := prefix
	if remain <= 0 {
		trimmed = ""
	} else if units(prefix) > remain {
		trimmed = clamp(prefix, remain)
	}
	body := joinBlocks(trimmed, folded)
	if units(body) <= limit {
		return body
	}
	if units(folded) <= limit {
		return folded
	}
	return shrinkMeasuredKeepingTail(folded, carriedAttestation(pipeline), limit, units, clamp)
}

func foldedWithin(risk, testing, pipeline string, innerBudget int, provider scm.Provider) string {
	if innerBudget <= 0 {
		return ""
	}
	minimum := pipelineSectionHeader(pipeline)
	if minimum == "" {
		minimum = carriedAttestation(pipeline)
	}
	budget := innerBudget - len(validationDetailsOpen) - len(validationDetailsClose)
	if budget < len(minimum) {
		return ""
	}
	if risk != "" {
		riskBudget := budget - len(minimum) - len("## Risk Assessment\n\n") - len("\n\n") - len("\n\n")
		if riskBudget <= 0 {
			risk = ""
		} else if len(risk) > riskBudget {
			risk = truncateTextAtLineBoundary(risk, riskBudget, essentialPRBodyTruncationMarker())
		}
	}
	inner := strings.Trim(appendGeneratedSectionsToCleanBodyWithinLimit("", risk, testing, pipeline, budget, provider), "\n")
	if !strings.Contains(inner, carriedAttestation(pipeline)) {
		inner = minimum
	}
	return wrapValidation(inner)
}

func shrinkMeasuredKeepingTail(full, tail string, limit int, units func(string) int, clamp func(string, int) string) string {
	if limit <= 0 {
		return ""
	}
	if units(full) <= limit {
		return full
	}
	tail = strings.TrimSpace(tail)
	if tail == "" || !strings.HasSuffix(full, tail) {
		return clamp(full, limit)
	}
	if units(tail) > limit {
		marker := extractPipelineAttestationMarker(tail)
		if marker != "" && units(marker) <= limit {
			return marker
		}
		return clamp(tail, limit)
	}
	prefix := strings.TrimRight(strings.TrimSuffix(full, tail), "\n")
	sep := ""
	if prefix != "" {
		sep = "\n\n"
	}
	budget := limit - units(sep) - units(tail)
	if budget <= 0 {
		return tail
	}
	trimmed := prefix
	if units(prefix) > budget {
		trimmed = clamp(prefix, budget)
	}
	if strings.TrimSpace(trimmed) == "" {
		return tail
	}
	body := trimmed + sep + tail
	if units(body) <= limit {
		return body
	}
	return tail
}
