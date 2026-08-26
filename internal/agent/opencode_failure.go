package agent

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
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
	terminal   bool

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
		terminal:     isTerminalRetryError(strings.ToLower(strings.Join(e.providerText(), "\n"))),
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
		if failure.retryable && !failure.terminal && !failure.toolActivity {
			return failure.label(), true
		}
		return "", false
	}
	// Everything else - a dropped SSE stream, a failed message request, an
	// unparseable turn, a thinking conflict - reaches the shared substring
	// classifier with no verdict from opencode, and that classifier reads
	// only text. The marker is the one place the tool evidence survives, so
	// it is checked before the fall-through rather than at each call site.
	if opencodeReplayUnsafe(err) {
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

// opencodeToolEvidence is the three-valued answer to "did the failed turn run
// a tool". Both the retry and the prompt-only fallback replay the whole
// prompt in a FRESH session, so only a PROOF that no tool ran can authorise
// one, and silence is not that proof: the stream is what carries the tool
// events, so a stream that dies mid-turn can leave a tool already executed,
// its event undelivered, and the message response that lists it still in
// flight. Reading that silence as "no tools ran" is the same replay the gate
// exists to prevent, reached through a timing window.
type opencodeToolEvidence int

const (
	// opencodeToolsUnknown is the fail-closed value, and the zero value on
	// purpose: nothing observed says a tool ran, and nothing observed says
	// none did.
	opencodeToolsUnknown opencodeToolEvidence = iota
	// opencodeToolsNone is a proof rather than an absence: a complete
	// record of the turn was read and holds no tool part.
	opencodeToolsNone
	// opencodeToolsRan is a tool part, from the stream or the response.
	opencodeToolsRan
)

// replaySafe reports whether the turn may be run again in a fresh session.
func (e opencodeToolEvidence) replaySafe() bool { return e == opencodeToolsNone }

func (e opencodeToolEvidence) String() string {
	switch e {
	case opencodeToolsRan:
		return "ran tools"
	case opencodeToolsNone:
		return "no tools"
	default:
		return "unknown"
	}
}

// reason names a withheld retry. The two cases stay distinguishable because
// the operator's next step differs: a turn known to have run tools is a
// decision about replaying side effects that exist, while an unverified turn
// is one to go and look at before deciding anything.
func (e opencodeToolEvidence) reason() string {
	if e == opencodeToolsRan {
		return "the failed turn already ran tools"
	}
	return "could not verify the failed turn ran no tools"
}

// marker is the sentinel the evidence travels as, so a caller several frames
// away can ask errors.Is instead of threading the value through.
func (e opencodeToolEvidence) marker() error {
	if e == opencodeToolsRan {
		return errOpencodeToolsAlreadyRan
	}
	return errOpencodeToolActivityUnknown
}

// opencodeReplayUnsafe reports whether err belongs to a turn that must not be
// replayed in a fresh session: it ran a tool, or nothing available proves it
// did not.
func opencodeReplayUnsafe(err error) bool {
	return errors.Is(err, errOpencodeToolsAlreadyRan) || errors.Is(err, errOpencodeToolActivityUnknown)
}

// resolveOpencodeToolEvidence answers the gate's question from what the
// client actually observed of the turn:
//
//   - a tool part, on the stream or in the message response, is the fact
//     itself;
//   - a message response lists the turn's parts, so one carrying no tool
//     part proves none ran;
//   - a stream that ran to session.idle is a complete record in its own
//     right, because every tool part of the session crossed it;
//   - a message request that never reached opencode proves the same thing
//     from the other end - opencode never held this attempt's prompt;
//   - anything else is UNKNOWN, which is precisely the window this gate had
//     left open: a stream that died mid-turn and no readable response is the
//     state in which a tool can have run unrecorded.
func resolveOpencodeToolEvidence(state *opencodeStreamState, mr opencodeMessageResult, streamComplete bool) opencodeToolEvidence {
	if state != nil && state.toolInvoked {
		return opencodeToolsRan
	}
	if mr.resp != nil {
		for _, part := range mr.resp.Parts {
			if isOpencodeToolPart(part.Type) {
				return opencodeToolsRan
			}
		}
		return opencodeToolsNone
	}
	if streamComplete {
		return opencodeToolsNone
	}
	if mr.settled && requestNeverReachedOpencode(mr.err) {
		return opencodeToolsNone
	}
	return opencodeToolsUnknown
}

// requestNeverReachedOpencode reports whether the message request failed
// before opencode could receive the prompt. A dial that never connected - the
// managed server died, or the port was never up - cannot have run a tool of
// this attempt, so that failure keeps the retry it has always had, which is
// also what restarts the server in recoverTransientRetry. A failure after the
// connection was established is a different claim: opencode holds the prompt
// from that point on, and holding the prompt is enough to have run a tool.
func requestNeverReachedOpencode(err error) bool {
	if err == nil {
		return false
	}
	var opErr *net.OpError
	return errors.As(err, &opErr) && opErr.Op == "dial"
}

// opencodeMessageResult is the outcome of the concurrent POST
// /session/{id}/message. settled separates a request that answered from one
// still in flight; nothing else distinguishes them, and they are not the same
// evidence.
type opencodeMessageResult struct {
	resp    *opencodeMessageResponse
	err     error
	settled bool
}

// opencodeEvidenceWait bounds how long a failed turn waits for its message
// response before the turn is declared unverifiable. Long enough for the
// abort to end the turn and for opencode to answer with the assistant
// message; bounded because an unreachable server never answers at all. It is
// a package var so tests can shorten it.
var opencodeEvidenceWait = 10 * time.Second

// pollOpencodeMessage takes the message result only if it has already
// arrived. On its own this is what left the gate blind: "nothing yet" and "no
// tool ran" are different answers.
func pollOpencodeMessage(ch <-chan opencodeMessageResult) opencodeMessageResult {
	select {
	case mr := <-ch:
		return mr
	default:
		return opencodeMessageResult{}
	}
}

// awaitOpencodeMessage waits up to d for the message result, and reports an
// unsettled result when it gives up rather than an empty one.
func awaitOpencodeMessage(ch <-chan opencodeMessageResult, d time.Duration) opencodeMessageResult {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case mr := <-ch:
		return mr
	case <-timer.C:
		return opencodeMessageResult{}
	}
}

// opencodeToolActivityFailure wraps a failure the turn cannot be replayed
// past, for every path that carries no retryability verdict of its own: a
// dropped SSE stream ("opencode events:"), an HTTP failure on the message
// request, a response the turn left unparseable. Those reach the shared
// substring classifier, which reads an "unexpected EOF" or a 503 in the text
// and retries - and the retry is the same FRESH session
// classifyOpencodeTransient refuses on the message path, replaying every tool
// the failed turn already ran. Marking the error where the evidence is still
// in hand is what lets one classifier decision cover them all, instead of
// each path deciding for itself and the next one added forgetting to.
type opencodeToolActivityFailure struct {
	err      error
	evidence opencodeToolEvidence
}

// Unwrap reports the evidence marker alongside the cause, so errors.Is finds
// both the original failure and the reason it is not being replayed - the
// same markers the prompt-only structured-output fallback gates on.
func (e *opencodeToolActivityFailure) Unwrap() []error {
	return []error{e.err, e.evidence.marker()}
}

func (e *opencodeToolActivityFailure) Error() string {
	msg := e.err.Error()
	// Same convention as opencodeMessageFailure: name the withheld retry
	// only where there would have been one, so it never reads as an
	// explanation for a failure that was never going to be retried.
	// Classifying the cause here cannot recurse - the wrapper is not part of
	// it.
	if _, retryable := classifyTransient(e.err); retryable {
		msg += " (not retried: " + e.evidence.reason() + ")"
	}
	return msg
}

// opencodeTurnFailure marks err with the turn's tool evidence. Every
// non-message-failure error return in runOnceWithFormat from the point the
// prompt is sent onwards goes through it, because whether the shared
// classifier will find a transient needle in the text - a provider blip, a
// network drop, or a 503 quoted in an output snippet - is not knowable at the
// call site. Only proven-no-tools passes through unmarked.
func opencodeTurnFailure(evidence opencodeToolEvidence, err error) error {
	if err == nil || evidence.replaySafe() {
		return err
	}
	return &opencodeToolActivityFailure{err: err, evidence: evidence}
}
