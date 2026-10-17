package agent

import (
	"testing"
	"time"
)

const ClaudeEarlyPartialSubmission = claudeEarlyPartialSubmission

// NewClaudeLateSubmitHelperAgent returns a Claude agent that replays the
// submit-then-idle session with its submission made after delay.
func NewClaudeLateSubmitHelperAgent(t *testing.T, delay time.Duration) Agent {
	t.Helper()
	t.Setenv("NM_CLAUDE_STDIN_HELPER", "late-submit-then-idle")
	t.Setenv("NM_CLAUDE_SUBMIT_DELAY", delay.String())
	return newClaudeStdinHelperAgent(t)
}

// WithClaudeQuietStop sets the idle-end margin and derives the idle grace
// from quietStop as production derives it from the pipeline's quiet stop.
func WithClaudeQuietStop(t *testing.T, quietStop, margin time.Duration) {
	t.Helper()
	withClaudeIdleEndBeforeDeadline(t, margin)
	withClaudeSubmittedIdleGrace(t, claudeSubmittedIdleGraceUnder(quietStop))
}
