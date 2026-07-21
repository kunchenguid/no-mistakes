package steps

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// configuredCommandFailureSummaryMaxBytes is the fixed upper bound for the
// Test/Lint command-output projection that may enter findings, persisted round
// data, IPC, and repair prompts. 64 KiB leaves ample room below IPC's 1 MiB
// message ceiling for findings metadata and the rest of an AXI response. It is
// deliberately independent of host argv limits. The complete output remains in
// the authoritative step log.
const configuredCommandFailureSummaryMaxBytes = 64 * 1024

const configuredCommandFailureMarkerReserve = 512
const configuredCommandFailureLineSearchBytes = 4 * 1024

func configuredCommandFailureSummary(output string, step types.StepName) string {
	if len(output) <= configuredCommandFailureSummaryMaxBytes {
		return strings.ToValidUTF8(output, "?")
	}

	contentBudget := configuredCommandFailureSummaryMaxBytes - configuredCommandFailureMarkerReserve
	headEnd := utf8SafePrefixEnd(output, contentBudget/2)
	tailStart := utf8SafeTailStart(output, len(output)-(contentBudget-contentBudget/2))
	if tailStart < headEnd {
		tailStart = headEnd
	}

	head := strings.ToValidUTF8(output[:headEnd], "?")
	tail := strings.ToValidUTF8(output[tailStart:], "?")
	omitted := tailStart - headEnd
	stepLabel := configuredCommandStepLabel(step)
	marker := fmt.Sprintf(
		"\n\n[configured %s output truncated: original byte count: %d; omitted byte count: %d; complete output: %s step log (`no-mistakes axi logs --step %s --full`)]\n\n",
		stepLabel,
		len(output),
		omitted,
		stepLabel,
		step,
	)
	return head + marker + tail
}

func configuredCommandStepLabel(step types.StepName) string {
	switch step {
	case types.StepTest:
		return "Test"
	case types.StepLint:
		return "Lint"
	default:
		return string(step)
	}
}

func utf8SafePrefixEnd(output string, target int) int {
	if target >= len(output) {
		return len(output)
	}
	end := target
	for end > 0 && !utf8.RuneStart(output[end]) {
		end--
	}
	searchStart := end - configuredCommandFailureLineSearchBytes
	if searchStart < 0 {
		searchStart = 0
	}
	if newline := strings.LastIndexByte(output[searchStart:end], '\n'); newline >= 0 {
		return searchStart + newline + 1
	}
	return end
}

func utf8SafeTailStart(output string, target int) int {
	if target <= 0 {
		return 0
	}
	start := target
	for start < len(output) && !utf8.RuneStart(output[start]) {
		start++
	}
	searchEnd := start + configuredCommandFailureLineSearchBytes
	if searchEnd > len(output) {
		searchEnd = len(output)
	}
	if newline := strings.IndexByte(output[start:searchEnd], '\n'); newline >= 0 {
		return start + newline + 1
	}
	return start
}
