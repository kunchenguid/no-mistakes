package steps

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestConfiguredCommandFailureSummaryBoundsHeadTailAndUTF8(t *testing.T) {
	head := "HEAD_MARKER context before failure\n"
	tail := "\nTAIL_MARKER 最后的错误🙂\n"
	output := head + strings.Repeat("middle diagnostic line αβγ\n", 10000) + tail

	got := configuredCommandFailureSummary(output, types.StepTest)
	if len(got) > configuredCommandFailureSummaryMaxBytes {
		t.Fatalf("summary has %d bytes, cap is %d", len(got), configuredCommandFailureSummaryMaxBytes)
	}
	if !utf8.ValidString(got) {
		t.Fatal("summary is not valid UTF-8")
	}
	for _, want := range []string{
		"HEAD_MARKER context before failure",
		"TAIL_MARKER 最后的错误🙂",
		fmt.Sprintf("original byte count: %d", len(output)),
		"complete output: Test step log",
		"no-mistakes axi logs --step test --full",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary missing %q", want)
		}
	}

	omitted := parseOmittedByteCount(t, got)
	markerStart := strings.Index(got, "\n\n[configured Test output truncated:")
	markerEnd := strings.Index(got, "]\n\n")
	if markerStart < 0 || markerEnd < 0 {
		t.Fatalf("truncation marker missing from summary")
	}
	retainedBytes := markerStart + len(got[markerEnd+3:])
	if omitted != len(output)-retainedBytes {
		t.Fatalf("omitted byte count = %d, want exact %d", omitted, len(output)-retainedBytes)
	}
}

func TestConfiguredCommandFailureSummaryUsesLintLogAndLeavesSmallOutputIntact(t *testing.T) {
	small := "lint failed at file.go:10\n"
	if got := configuredCommandFailureSummary(small, types.StepLint); got != small {
		t.Fatalf("small output changed: %q", got)
	}

	large := "LINT_HEAD\n" + strings.Repeat("x\n", configuredCommandFailureSummaryMaxBytes) + "LINT_TAIL\n"
	got := configuredCommandFailureSummary(large, types.StepLint)
	for _, want := range []string{"LINT_HEAD", "LINT_TAIL", "complete output: Lint step log", "--step lint --full"} {
		if !strings.Contains(got, want) {
			t.Errorf("lint summary missing %q", want)
		}
	}
	if len(got) > configuredCommandFailureSummaryMaxBytes {
		t.Fatalf("lint summary has %d bytes, cap is %d", len(got), configuredCommandFailureSummaryMaxBytes)
	}
}

func parseOmittedByteCount(t *testing.T, summary string) int {
	t.Helper()
	const prefix = "omitted byte count: "
	start := strings.Index(summary, prefix)
	if start < 0 {
		t.Fatal("summary has no omitted byte count")
	}
	start += len(prefix)
	end := start
	for end < len(summary) && summary[end] >= '0' && summary[end] <= '9' {
		end++
	}
	value, err := strconv.Atoi(summary[start:end])
	if err != nil {
		t.Fatalf("parse omitted byte count: %v", err)
	}
	return value
}
