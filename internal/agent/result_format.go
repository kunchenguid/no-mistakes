package agent

import (
	"context"
	"errors"
	"fmt"
)

// isResultFormatError reports a completed turn whose structured result was
// rejected. Cancellation and deadlines remain invocation failures instead.
func isResultFormatError(err error) bool {
	return err != nil &&
		!errors.Is(err, context.Canceled) &&
		!errors.Is(err, context.DeadlineExceeded) &&
		IsStructuredOutputRejected(err)
}

// keptFormatResult marks a completed format failure as ineligible for session
// resume. A nil flag is treated as retryable once a resumed session id is
// present, which would run the same fixer turn again in a fresh session.
func keptFormatResult(result *Result) *Result {
	var kept Result
	if result != nil {
		kept = *result
	}
	retryable := false
	kept.ResumeRetryable = &retryable
	return &kept
}

func formatKept(cause error) error {
	return fmt.Errorf("result format failed and completed work was kept: %w", cause)
}
