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
	// Everything else - a dropped SSE stream, a failed message request, an
	// unparseable turn, a thinking conflict - reaches the shared substring
	// classifier with no verdict from opencode, and that classifier reads
	// only text. The marker is the one place the tool activity survives, so
	// it is checked before the fall-through rather than at each call site.
	if errors.Is(err, errOpencodeToolsAlreadyRan) {
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

// opencodeToolActivityFailure wraps a failure from a turn that had already
// invoked a tool, for every path that carries no retryability verdict of its
// own: a dropped SSE stream ("opencode events:"), an HTTP failure on the
// message request, a response the turn left unparseable. Those reach the
// shared substring classifier, which reads an "unexpected EOF" or a 503 in
// the text and retries - and the retry is the same FRESH session
// classifyOpencodeTransient refuses on the message path, replaying every
// tool the failed turn already ran. Marking the error where the tool
// activity is still in hand is what lets one classifier decision cover them
// all, instead of each path deciding for itself and the next one added
// forgetting to.
type opencodeToolActivityFailure struct{ err error }

// Unwrap reports the marker alongside the cause, so errors.Is finds both the
// original failure and errOpencodeToolsAlreadyRan - the same marker the
// prompt-only structured-output fallback already gates on.
func (e *opencodeToolActivityFailure) Unwrap() []error {
	return []error{e.err, errOpencodeToolsAlreadyRan}
}

func (e *opencodeToolActivityFailure) Error() string {
	msg := e.err.Error()
	// Same convention as opencodeMessageFailure: name the withheld retry
	// only where there would have been one, so it never reads as an
	// explanation for a failure that was never going to be retried.
	// Classifying the cause here cannot recurse - the wrapper is not part of
	// it.
	if _, retryable := classifyTransient(e.err); retryable {
		msg += " (not retried: the failed turn already ran tools)"
	}
	return msg
}

// opencodeTurnFailure marks err as belonging to a turn that already ran a
// tool. Every non-message-failure error return in runOnceWithFormat from the
// point the prompt is sent onwards goes through it, because whether the
// shared classifier will find a transient needle in the text - a provider
// blip, a network drop, or a 503 quoted in an output snippet - is not
// knowable at the call site.
func opencodeTurnFailure(state *opencodeStreamState, resp *opencodeMessageResponse, err error) error {
	if err == nil || !opencodeTurnRanTools(state, resp) {
		return err
	}
	return &opencodeToolActivityFailure{err: err}
}
