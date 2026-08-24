package agent

import (
	"errors"
	"fmt"
	"strings"
)

// opencodeMessageFailure is a turn that opencode completed with an error on
// the assistant message. It carries opencode's own retryability verdict so
// the retry loop can repeat a provider blip without ever repeating a request
// the provider rejected as invalid.
type opencodeMessageFailure struct {
	name       string
	message    string
	statusCode int
	retries    int
	retryable  bool
	structured bool

	// toolActivity records that the failed turn already invoked at least one
	// tool, which withdraws the retry however retryable opencode called the
	// failure. See classifyOpencodeTransient.
	toolActivity bool
}

func newOpencodeMessageFailure(e *opencodeMessageError, toolActivity bool) error {
	if e == nil {
		return nil
	}
	return &opencodeMessageFailure{
		name:         e.Name,
		message:      e.message(),
		statusCode:   e.statusCode(),
		retries:      e.retries(),
		retryable:    e.retryable(),
		structured:   e.IsStructuredOutput(),
		toolActivity: toolActivity,
	}
}

func (e *opencodeMessageFailure) Error() string {
	// StructuredOutputError keeps its own wording: the actionable fact is
	// that opencode already spent its internal retries trying to make the
	// model call the StructuredOutput tool.
	if e.structured {
		return fmt.Sprintf("opencode structured output failed after %d internal retries: %s",
			e.retries, e.detail())
	}
	name := e.name
	if name == "" {
		name = "error"
	}
	msg := fmt.Sprintf("opencode %s: %s", name, e.detail())
	if e.statusCode != 0 {
		msg = fmt.Sprintf("opencode %s (status %d): %s", name, e.statusCode, e.detail())
	}
	// Without this clause a withheld retry is indistinguishable from a
	// failure opencode called non-retryable, so an operator reading a 503
	// would expect the attempts the run did not spend.
	if e.retryable && e.toolActivity {
		msg += " (not retried: the failed turn already ran tools)"
	}
	return msg
}

func (e *opencodeMessageFailure) detail() string {
	if msg := strings.TrimSpace(e.message); msg != "" {
		return msg
	}
	return "no detail reported"
}

// label is the short telemetry tag for a retried failure.
func (e *opencodeMessageFailure) label() string {
	name := e.name
	if name == "" {
		name = "message error"
	}
	if e.statusCode != 0 {
		return fmt.Sprintf("opencode %s %d", name, e.statusCode)
	}
	return "opencode " + name
}

// classifyOpencodeTransient extends the shared transient classifier with
// opencode's assistant-message errors. When opencode reported the failure it
// is the authority on whether a retry is worthwhile, so a non-retryable one
// stops here rather than falling through to the substring matching - a 400
// body quoting a provider's own rate-limit prose must not look transient.
func classifyOpencodeTransient(err error) (string, bool) {
	var failure *opencodeMessageFailure
	if errors.As(err, &failure) {
		// A retry starts a FRESH opencode session (runOnce always calls
		// createSession), so it replays the whole prompt with no memory of
		// the tools the failed attempt already executed - a second commit, a
		// second file write, a second posted comment. Nothing in the wire
		// protocol says which of those were idempotent, so a turn that got
		// as far as running a tool fails closed and the operator decides.
		// The failure this retry exists for - a provider blip that kills the
		// turn before the model acts - is untouched by the gate.
		if failure.retryable && !failure.toolActivity {
			return failure.label(), true
		}
		return "", false
	}
	return classifyTransient(err)
}

// isOpencodeToolPart reports whether a message-part type is a tool
// invocation. opencode names the part "tool"; the prefix match is deliberate
// slack for wire drift, because a part type this misses is a side effect
// silently replayed rather than a spurious refusal.
func isOpencodeToolPart(partType string) bool {
	return partType == "tool" || strings.HasPrefix(partType, "tool-")
}

// opencodeTurnRanTools reports whether the turn invoked any tool, reading
// both places a tool part can appear: the SSE stream, and the message
// response body for a stream that was cut short before the part arrived.
func opencodeTurnRanTools(state *opencodeStreamState, resp *opencodeMessageResponse) bool {
	if state != nil && state.toolInvoked {
		return true
	}
	if resp == nil {
		return false
	}
	for _, part := range resp.Parts {
		if isOpencodeToolPart(part.Type) {
			return true
		}
	}
	return false
}
