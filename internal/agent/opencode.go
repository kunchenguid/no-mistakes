package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
)

// errOpencodeToolsAlreadyRan annotates a failure whose turn had already
// invoked a tool. A retry re-runs the whole prompt in a fresh session, so it
// must not be taken past this marker.
var errOpencodeToolsAlreadyRan = errors.New("the failed turn already ran tools")

// errOpencodeToolActivityUnknown annotates a failure whose turn could not be
// read at all: no tool event observed, and no complete record of the turn to
// prove none ran. It withholds the same replay as errOpencodeToolsAlreadyRan
// - an unverified turn is not a turn that did nothing - and stays a separate
// sentinel so the surfaced error says which of the two it was.
var errOpencodeToolActivityUnknown = errors.New("could not verify the failed turn ran no tools")

// errOpencodeStreamEnded is the cause recorded when the event stream closed
// before the turn's agent loop reported an end.
var errOpencodeStreamEnded = errors.New("event stream ended before the turn finished")

// opencodeAgent drives a persistent `opencode serve` (opencode 2.x, the
// authenticated `/api` surface) over REST plus its SSE event stream. See
// opencode_types.go for the wire protocol and how it differs from v1.
type opencodeAgent struct {
	bin       string
	extraArgs []string
	// profile is the harness-neutral model/effort selection resolved by
	// internal/agentcfg. `opencode serve` rejects model and variant flags
	// outright, so unlike every other native adapter these two knobs cannot
	// ride argv: they belong to the session (see createSession).
	profile agentcfg.Profile
	subprocessContext
	mu     sync.Mutex
	server *managedServer
	// password is the basic-auth secret the running server was started with.
	// It lives and dies with server: recoverTransientRetry drops both and
	// ensureServer mints a new pair.
	password string
}

func (a *opencodeAgent) Name() string { return "opencode" }

func (a *opencodeAgent) ReportsAgentAttempts() bool { return true }

func (a *opencodeAgent) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	return runWithRetry(ctx, "opencode", opts, claudeMaxRetries, classifyOpencodeTransient, a.recoverTransientRetry, func() (*Result, error) {
		return a.runOnce(ctx, opts)
	})
}

func (a *opencodeAgent) recoverTransientRetry(label string) {
	if label != "connection refused" {
		return
	}
	a.mu.Lock()
	srv := a.server
	a.server = nil
	a.password = ""
	a.mu.Unlock()
	if srv != nil {
		srv.shutdown()
	}
}

// runOnce runs one turn in a fresh session:
//
//  1. create the session (blanket permissions, cwd, model);
//  2. subscribe to the event stream BEFORE admitting the prompt, so no event
//     of the turn is missed;
//  3. admit the prompt; opencode answers at once and runs the turn;
//  4. follow the stream until session.execution.{succeeded,failed,interrupted};
//  5. read the message list back for the output text, the usage and the
//     tool record.
//
// Every failure from the moment the prompt may have been admitted carries
// the turn's tool evidence (opencodeTurnFailure), because a retry replays
// the prompt in a fresh session and only a proof that no tool ran can
// authorise one.
func (a *opencodeAgent) runOnce(ctx context.Context, opts RunOpts) (*Result, error) {
	baseURL, err := a.ensureServer(ctx, opts.CWD, opts.Env)
	if err != nil {
		return nil, err
	}

	sessionID, err := a.createSession(ctx, baseURL, opts.CWD)
	if err != nil {
		return nil, err
	}
	defer a.deleteSession(baseURL, sessionID)

	prompt := opts.Prompt
	if len(opts.JSONSchema) > 0 {
		prompt = buildOpencodePrompt(prompt, opts.JSONSchema)
	}

	streamCtx, streamCancel := context.WithCancel(ctx)
	defer streamCancel()
	eventBody, err := a.connectEventStream(streamCtx, baseURL)
	if err != nil {
		return nil, err
	}
	defer eventBody.Close()

	state := newOpencodeStreamState(sessionID, opts.OnChunk)

	if err := a.sendPrompt(ctx, baseURL, sessionID, prompt); err != nil {
		// A dial that never connected proves opencode never held the
		// prompt. Any other failure is resolved from the session itself: a
		// message list with no user message proves the same thing from the
		// server's side, and anything else stays unknown.
		evidence := opencodeToolsUnknown
		if requestNeverReachedOpencode(err) {
			evidence = opencodeToolsNone
		} else {
			evidence = resolveOpencodeToolEvidence(state, a.settleTurn(baseURL, sessionID), false)
		}
		return nil, opencodeTurnFailure(evidence, fmt.Errorf("opencode prompt: %w", err))
	}

	streamErr := parseOpencodeSSE(eventBody, state)
	streamCancel()

	if streamErr != nil || state.outcome == "" {
		// The stream carried the tool events, so with it gone the message
		// list is the remaining record of what the turn ran. Interrupting
		// first is the cleanup this branch owes anyway, and it is what lets
		// the session settle so the list can be complete; the wait is
		// bounded because a server that is gone never settles, and that
		// turn is simply unverifiable.
		a.interruptSession(baseURL, sessionID)
		record := a.settleTurn(baseURL, sessionID)
		evidence := resolveOpencodeToolEvidence(state, record, false)
		cause := streamErr
		if cause == nil {
			cause = errOpencodeStreamEnded
		}
		return resultFromUsage(turnUsage(state, record)), opencodeTurnFailure(evidence, fmt.Errorf("opencode events: %w", cause))
	}

	// The turn ended. Its message list is the record: the stream saw every
	// event, so the fetch failing does not make the turn unverifiable, and
	// the streamed text stands in for a list that cannot be read.
	record := a.fetchTurnRecord(ctx, baseURL, sessionID)
	evidence := resolveOpencodeToolEvidence(state, record, true)
	usage := turnUsage(state, record)

	switch state.outcome {
	case opencodeOutcomeInterrupted:
		reason := state.interruptReason
		if reason == "" {
			reason = "unknown reason"
		}
		return resultFromUsage(usage), opencodeTurnFailure(evidence, fmt.Errorf("opencode turn interrupted (%s)", reason))
	case opencodeOutcomeFailed:
		failure := state.failure
		if failure == nil {
			failure = record.assistantError()
		}
		if failure == nil {
			failure = &opencodeSessionError{Type: "unknown", Message: "opencode reported the turn failed without an error"}
		}
		return resultFromUsage(usage), newOpencodeMessageFailure(failure, evidence == opencodeToolsRan)
	}

	outputText := record.outputText()
	if !record.fetched {
		outputText = state.outputText()
	}
	// A turn that ended "succeeded" with nothing to show but an error on
	// its last assistant message is reported as that error rather than as
	// the undiagnosable "returned no text output".
	if outputText == "" {
		if failure := record.assistantError(); failure != nil {
			return resultFromUsage(usage), newOpencodeMessageFailure(failure, evidence == opencodeToolsRan)
		}
	}

	result, err := finalizeTextResult("opencode", outputText, opts.JSONSchema, usage)
	if err != nil {
		return result, opencodeTurnFailure(evidence, err)
	}
	return result, nil
}

// fetchTurnRecord reads the session's message list under the caller's ctx.
func (a *opencodeAgent) fetchTurnRecord(ctx context.Context, baseURL, sessionID string) opencodeTurnRecord {
	msgs, err := a.fetchMessages(ctx, baseURL, sessionID)
	if err != nil {
		return opencodeTurnRecord{err: err}
	}
	return opencodeTurnRecord{messages: msgs, fetched: true}
}

// settleTurn is fetchTurnRecord for a turn whose stream is gone: it first
// waits, bounded by opencodeEvidenceWait, for the session to go idle so the
// list it then reads is complete. The wait failing (the route is
// experimental, or the server is gone) is not itself an error - the list is
// read regardless, and an incomplete one resolves to unknown evidence.
func (a *opencodeAgent) settleTurn(baseURL, sessionID string) opencodeTurnRecord {
	ctx, cancel := context.WithTimeout(context.Background(), opencodeEvidenceWait)
	defer cancel()
	_ = a.waitForIdle(ctx, baseURL, sessionID)
	return a.fetchTurnRecord(ctx, baseURL, sessionID)
}

// turnUsage prefers the usage recorded on the turn's assistant messages and
// falls back to what the stream accumulated from session.step.ended events.
func turnUsage(state *opencodeStreamState, record opencodeTurnRecord) TokenUsage {
	if usage, ok := record.usage(); ok {
		return usage
	}
	if state != nil {
		return state.usage
	}
	return TokenUsage{}
}

// opencodeEvidenceWait bounds how long a failed turn waits for its session to
// settle before the turn is declared unverifiable. Long enough for the
// interrupt to end the turn and for opencode to write the idle marker;
// bounded because an unreachable server never answers at all. It is a
// package var so tests can shorten it.
var opencodeEvidenceWait = 10 * time.Second

func (a *opencodeAgent) Close() error {
	a.mu.Lock()
	srv := a.server
	a.server = nil
	a.password = ""
	a.mu.Unlock()
	if srv != nil {
		srv.shutdown()
	}
	return nil
}
