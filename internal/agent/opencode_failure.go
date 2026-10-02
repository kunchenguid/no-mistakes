package agent

import (
	"errors"
	"fmt"
	"net"
	"strings"
)

// opencodeMessageFailure is a turn that opencode ended with an error - a
// session.execution.failed event, or an error recorded on the turn's last
// assistant message. It carries opencode's own classification of the cause
// so the retry loop can repeat a provider blip without ever repeating a
// request the provider rejected as invalid.
type opencodeMessageFailure struct {
	errType   string
	message   string
	status    int
	retryable bool
	terminal  bool

	// toolActivity records that the failed turn already invoked at least one
	// tool, which withdraws the retry however retryable opencode called the
	// failure. See classifyOpencodeTransient.
	toolActivity bool
}

func newOpencodeMessageFailure(e *opencodeSessionError, toolActivity bool) error {
	if e == nil {
		return nil
	}
	return &opencodeMessageFailure{
		errType:      e.Type,
		message:      e.message(),
		status:       e.Status,
		retryable:    e.retryable(),
		terminal:     isTerminalRetryError(strings.ToLower(e.Type + "\n" + e.message())),
		toolActivity: toolActivity,
	}
}

func (e *opencodeMessageFailure) Error() string {
	name := e.errType
	if name == "" {
		name = "error"
	}
	msg := fmt.Sprintf("opencode %s: %s", name, e.detail())
	if e.status != 0 {
		msg = fmt.Sprintf("opencode %s (status %d): %s", name, e.status, e.detail())
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
	name := e.errType
	if name == "" {
		name = "turn error"
	}
	if e.status != 0 {
		return fmt.Sprintf("opencode %s %d", name, e.status)
	}
	return "opencode " + name
}

// classifyOpencodeTransient extends the shared transient classifier with
// opencode's structured turn errors. When opencode reported the failure its
// typed classification is the authority on whether a retry is worthwhile, so
// a non-retryable one stops here rather than falling through to the
// substring matching - a 400 body quoting a provider's own rate-limit prose
// must not look transient.
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
		// Resuming the failed session instead is not a fix: another prompt
		// in the same session only appends a user message, so whether the
		// model re-runs the tool stays its judgement rather than a guarantee.
		if failure.retryable && !failure.terminal && !failure.toolActivity {
			return failure.label(), true
		}
		return "", false
	}
	// Everything else - a dropped SSE stream, a failed prompt request, an
	// unparseable turn - reaches the shared substring classifier with no
	// verdict from opencode, and that classifier reads only text. The marker
	// is the one place the tool evidence survives, so it is checked before
	// the fall-through rather than at each call site.
	if opencodeReplayUnsafe(err) {
		return "", false
	}
	// The event stream closing before the turn reported an end is the
	// server's side of a connection blip (a restart, or opencode failing a
	// slow subscriber's stream by contract). The shared classifier knows
	// only the client-side wording of such drops, so this one is named
	// here; the evidence marker above already refused it for any turn that
	// may have run a tool.
	if errors.Is(err, errOpencodeStreamEnded) {
		return "opencode stream ended", true
	}
	return classifyTransient(err)
}

// isOpencodeToolPart reports whether an assistant content type is a tool
// invocation. opencode names it "tool"; the prefix match is deliberate slack
// for wire drift, because a type this misses is a side effect silently
// replayed rather than a spurious refusal.
func isOpencodeToolPart(partType string) bool {
	return partType == "tool" || strings.HasPrefix(partType, "tool-")
}

// opencodeToolEvidence is the three-valued answer to "did the failed turn run
// a tool". A retry replays the whole prompt in a FRESH session, so only a
// PROOF that no tool ran can authorise one, and silence is not that proof:
// the stream is what carries the tool events, so a stream that dies mid-turn
// can leave a tool already executed, its event undelivered, and the message
// list that would show it not yet settled. Reading that silence as "no tools
// ran" is the same replay the gate exists to prevent, reached through a
// timing window.
type opencodeToolEvidence int

const (
	// opencodeToolsUnknown is the fail-closed value, and the zero value on
	// purpose: nothing observed says a tool ran, and nothing observed says
	// none did.
	opencodeToolsUnknown opencodeToolEvidence = iota
	// opencodeToolsNone is a proof rather than an absence: a complete
	// record of the turn was read and holds no tool part.
	opencodeToolsNone
	// opencodeToolsRan is a tool event on the stream or a tool content
	// entry in the message list.
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
//   - a tool event on the stream, or a tool content entry in the message
//     list, is the fact itself;
//   - a message list that reached the turn's idle marker lists every step
//     the turn ran, so one holding no tool entry proves none ran;
//   - a message list holding no user message proves the prompt was never
//     admitted, so nothing ran at all;
//   - a stream that ran to the turn's execution end is a complete record in
//     its own right, because every tool event of the session crossed it;
//   - anything else is UNKNOWN, which is precisely the window this gate had
//     left open: a stream that died mid-turn and no settled list is the
//     state in which a tool can have run unrecorded.
func resolveOpencodeToolEvidence(state *opencodeStreamState, record opencodeTurnRecord, streamComplete bool) opencodeToolEvidence {
	if state != nil && state.toolInvoked {
		return opencodeToolsRan
	}
	if record.toolRan() {
		return opencodeToolsRan
	}
	if record.fetched && (record.complete() || !record.admitted()) {
		return opencodeToolsNone
	}
	if streamComplete {
		return opencodeToolsNone
	}
	return opencodeToolsUnknown
}

// requestNeverReachedOpencode reports whether the prompt request failed
// before opencode could receive it. A dial that never connected - the
// managed server died, or the port was never up - cannot have run a tool of
// this attempt, so that failure keeps the retry it has always had, which is
// also what restarts the server in recoverTransientRetry. A failure after the
// connection was established is a different claim: opencode may hold the
// prompt from that point on, and holding the prompt is enough to have run a
// tool.
func requestNeverReachedOpencode(err error) bool {
	if err == nil {
		return false
	}
	var opErr *net.OpError
	return errors.As(err, &opErr) && opErr.Op == "dial"
}

// opencodeToolActivityFailure wraps a failure the turn cannot be replayed
// past, for every path that carries no retryability verdict of its own: a
// dropped SSE stream ("opencode events:"), an HTTP failure on the prompt
// request, a turn the parser rejected. Those reach the shared substring
// classifier, which reads an "unexpected EOF" or a 503 in the text and
// retries - and the retry is the same FRESH session classifyOpencodeTransient
// refuses on the typed path, replaying every tool the failed turn already
// ran. Marking the error where the evidence is still in hand is what lets one
// classifier decision cover them all, instead of each path deciding for
// itself and the next one added forgetting to.
type opencodeToolActivityFailure struct {
	err      error
	evidence opencodeToolEvidence
}

// Unwrap reports the evidence marker alongside the cause, so errors.Is finds
// both the original failure and the reason it is not being replayed.
func (e *opencodeToolActivityFailure) Unwrap() []error {
	return []error{e.err, e.evidence.marker()}
}

func (e *opencodeToolActivityFailure) Error() string {
	msg := e.err.Error()
	// Same convention as opencodeMessageFailure: name the withheld retry
	// only where there would have been one, so it never reads as an
	// explanation for a failure that was never going to be retried.
	// Classifying the cause here cannot recurse - the wrapper is not part of
	// it - and it is the opencode-aware classification, so a stream that
	// ended early names its withheld retry like a dropped one does.
	if _, retryable := classifyOpencodeTransient(e.err); retryable {
		msg += " (not retried: " + e.evidence.reason() + ")"
	}
	return msg
}

// opencodeTurnFailure marks err with the turn's tool evidence. Every
// non-typed error return in runOnce from the point the prompt may have been
// admitted onwards goes through it, because whether the shared classifier
// will find a transient needle in the text - a provider blip, a network
// drop, or a 503 quoted in an output snippet - is not knowable at the call
// site. Only proven-no-tools passes through unmarked.
func opencodeTurnFailure(evidence opencodeToolEvidence, err error) error {
	if err == nil || evidence.replaySafe() {
		return err
	}
	return &opencodeToolActivityFailure{err: err, evidence: evidence}
}
