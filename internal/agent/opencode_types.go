package agent

import (
	"encoding/json"
	"sort"
	"strings"
)

// The wire shapes below are the parts of opencode's v2 `/api` surface the
// adapter reads. opencode 2.x replaced the flat v1 REST API (POST /session,
// POST /session/{id}/message, GET /global/event) with an authenticated
// `/api/*` tree, and the shape of every exchange changed with it:
//
//   - every JSON reply is wrapped in a `data` envelope;
//   - a prompt is ADMITTED, not answered: POST /api/session/{id}/prompt
//     returns the queued inbox entry at once and the turn runs
//     asynchronously, reporting through GET /api/event until a
//     session.execution.{succeeded,failed,interrupted} event;
//   - each model step is its own assistant message, so a turn that ran a
//     tool has several, and the output is read back from the session's
//     message list (GET /api/session/{id}/message);
//   - there is no native json_schema output mode any more, so structured
//     output always rides the prompt and is validated locally.
//
// Field sets are the subset the adapter acts on; unknown fields are ignored
// on purpose so additive upstream changes do not break decoding.

// opencodeEvent is one frame of GET /api/event. v2 frames are flat
// ({"id","type","data",...}); the v1 {"payload":{"type","properties"}}
// nesting is gone.
type opencodeEvent struct {
	ID   string          `json:"id"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// opencodeEventData is the union of the `data` fields carried by the
// session.* events the adapter reads. The events use disjoint field names, so
// one struct decodes all of them and a field an event does not carry decodes
// to its zero value.
type opencodeEventData struct {
	SessionID          string                `json:"sessionID"`
	AssistantMessageID string                `json:"assistantMessageID"`
	Ordinal            int                   `json:"ordinal"`
	Delta              string                `json:"delta"`
	Text               string                `json:"text"`
	Tokens             *opencodeTokens       `json:"tokens"`
	Error              *opencodeSessionError `json:"error"`
	Reason             string                `json:"reason"`
}

// opencodeTokens is opencode's TokenUsage.Info. Reasoning is a pointer so an
// older or partial shape that omits it is reported as not-reported rather
// than as a fabricated zero.
type opencodeTokens struct {
	Input     int            `json:"input"`
	Output    int            `json:"output"`
	Reasoning *int           `json:"reasoning,omitempty"`
	Cache     *opencodeCache `json:"cache,omitempty"`
}

type opencodeCache struct {
	Read  int `json:"read"`
	Write int `json:"write"`
}

// opencodeSessionError is opencode's Session.StructuredError: a flat
// {type, message, status?} record. `type` is opencode's own classification of
// the cause (provider.rate-limit, provider.auth, provider.transport, ...,
// aborted, permission.rejected, tool.execution, unknown) and `status` is the
// provider's HTTP status when there was one. v1's {"name","data":{...}}
// NamedError shape and its isRetryable verdict do not exist in v2.
type opencodeSessionError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Status  int    `json:"status,omitempty"`
}

// opencodeErrorTypeRetryable is opencode's structured classification of a
// failed turn mapped to whether repeating it could plausibly succeed. This is
// the v2 stand-in for v1's isRetryable flag: it reads the typed field, never
// the free-text message, so a 400 whose message quotes a provider's own
// rate-limit prose stays non-retryable. A type not listed here falls back to
// the status class.
var opencodeErrorTypeRetryable = map[string]bool{
	"provider.rate-limit": true,
	"provider.transport":  true,
	"provider.internal":   true,
	"provider.timeout":    true,

	"provider.auth":                  false,
	"provider.quota":                 false,
	"provider.content-filter":        false,
	"provider.invalid-output":        false,
	"provider.invalid-request":       false,
	"provider.unsupported-operation": false,
	"provider.no-route":              false,
	"provider.unknown":               false,
	"permission.rejected":            false,
	"tool.execution":                 false,
	"aborted":                        false,
}

func (e *opencodeSessionError) message() string {
	if e == nil {
		return ""
	}
	return strings.TrimSpace(e.Message)
}

// retryable reports whether repeating the turn could plausibly succeed.
// opencode's own error type wins when it is one we know; otherwise the status
// class decides, and an error carrying neither a known type nor a status is
// not retried.
func (e *opencodeSessionError) retryable() bool {
	if e == nil {
		return false
	}
	if verdict, known := opencodeErrorTypeRetryable[e.Type]; known {
		return verdict
	}
	switch code := e.Status; {
	case code == 429, code == 408:
		return true
	case code >= 500:
		return true
	default:
		return false
	}
}

// opencodeMessage is one item of GET /api/session/{id}/message. The list is
// a tagged union on `type`; the adapter reads user (Text), assistant
// (Content, Tokens, Error, Finish) and idle (Outcome) messages and ignores
// the rest.
type opencodeMessage struct {
	ID      string                `json:"id"`
	Type    string                `json:"type"`
	Text    string                `json:"text,omitempty"`
	Content []opencodeContent     `json:"content,omitempty"`
	Tokens  *opencodeTokens       `json:"tokens,omitempty"`
	Error   *opencodeSessionError `json:"error,omitempty"`
	Finish  string                `json:"finish,omitempty"`
	Outcome string                `json:"outcome,omitempty"`
	Time    struct {
		Created int64 `json:"created"`
	} `json:"time"`
}

// opencodeContent is one entry of an assistant message's content: text,
// reasoning, or tool.
type opencodeContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	Name string `json:"name,omitempty"`
}

type opencodeMessageList struct {
	Data []opencodeMessage `json:"data"`
}

type opencodeSessionCreated struct {
	Data struct {
		ID string `json:"id"`
	} `json:"data"`
}

// opencodeTurnRecord is the session's message list as read back after the
// turn, or the reason it could not be read. It is the durable record of what
// the turn did, which is what the tool-evidence gate and the output text are
// resolved from.
type opencodeTurnRecord struct {
	messages []opencodeMessage
	fetched  bool
	err      error
}

// sortOpencodeMessages orders messages oldest first. The endpoint returns
// them newest first by default and the adapter reasons about "after the
// prompt", so the order is normalised once here.
func sortOpencodeMessages(msgs []opencodeMessage) {
	sort.SliceStable(msgs, func(i, j int) bool {
		return msgs[i].Time.Created < msgs[j].Time.Created
	})
}

// turnStart is the index of the message after the last user message: the
// turn the adapter sent is the last prompt admitted to this fresh session,
// so everything after it is that turn's record. It is len(messages) when no
// user message exists, in which case nothing was admitted.
func (r opencodeTurnRecord) turnStart() int {
	for i := len(r.messages) - 1; i >= 0; i-- {
		if r.messages[i].Type == "user" {
			return i + 1
		}
	}
	return len(r.messages)
}

// admitted reports whether the session holds a user message at all. A fetch
// that shows none proves the prompt never entered the session.
func (r opencodeTurnRecord) admitted() bool {
	if !r.fetched {
		return false
	}
	for _, m := range r.messages {
		if m.Type == "user" {
			return true
		}
	}
	return false
}

// complete reports whether the turn reached its idle marker: opencode writes
// an `idle` message when the agent loop settles, whatever the outcome, so a
// record carrying one after the prompt lists every step the turn ran.
func (r opencodeTurnRecord) complete() bool {
	if !r.fetched {
		return false
	}
	for _, m := range r.messages[r.turnStart():] {
		if m.Type == "idle" {
			return true
		}
	}
	return false
}

// toolRan reports whether any assistant message of the turn holds a tool
// content entry.
func (r opencodeTurnRecord) toolRan() bool {
	if !r.fetched {
		return false
	}
	for _, m := range r.messages[r.turnStart():] {
		if m.Type != "assistant" {
			continue
		}
		for _, c := range m.Content {
			if isOpencodeToolPart(c.Type) {
				return true
			}
		}
	}
	return false
}

// assistantError is the error recorded on the turn's last failed assistant
// message, if any.
func (r opencodeTurnRecord) assistantError() *opencodeSessionError {
	if !r.fetched {
		return nil
	}
	var last *opencodeSessionError
	for i := r.turnStart(); i < len(r.messages); i++ {
		if r.messages[i].Type == "assistant" && r.messages[i].Error != nil {
			last = r.messages[i].Error
		}
	}
	return last
}

// outputText joins the text content of the turn's assistant messages in
// order, one blank line between steps. A tool-using turn spans several
// assistant messages in v2 (one per model step), and this is the same text
// the stream showed the operator; the structured-output parser extracts the
// JSON from it.
func (r opencodeTurnRecord) outputText() string {
	if !r.fetched {
		return ""
	}
	var parts []string
	for _, m := range r.messages[r.turnStart():] {
		if m.Type != "assistant" {
			continue
		}
		for _, c := range m.Content {
			if c.Type == "text" && strings.TrimSpace(c.Text) != "" {
				parts = append(parts, c.Text)
			}
		}
	}
	return strings.Join(parts, "\n\n")
}

// usage sums the token usage of the turn's assistant messages. ok is false
// when the record was not fetched or none of them reported usage, so the
// caller can fall back to what the stream accumulated.
func (r opencodeTurnRecord) usage() (TokenUsage, bool) {
	if !r.fetched {
		return TokenUsage{}, false
	}
	var total TokenUsage
	reported := false
	for _, m := range r.messages[r.turnStart():] {
		if m.Type == "assistant" && m.Tokens != nil {
			total.Add(opencodeTokensToUsage(m.Tokens))
			reported = true
		}
	}
	return total, reported
}

// opencodeTextPart tracks the accumulated text of one assistant text block
// (assistant message + ordinal) during streaming.
type opencodeTextPart struct {
	text        string
	emittedText string
}

// opencodeOutcome is how the turn's agent loop ended, from the
// session.execution.* event that closed it.
type opencodeOutcome string

const (
	opencodeOutcomeSucceeded   opencodeOutcome = "succeeded"
	opencodeOutcomeFailed      opencodeOutcome = "failed"
	opencodeOutcomeInterrupted opencodeOutcome = "interrupted"
)

// opencodeStreamState holds mutable state during SSE event processing.
type opencodeStreamState struct {
	sessionID string
	onChunk   func(string)

	textParts     map[string]*opencodeTextPart
	textPartOrder []string
	usageByMsg    map[string]TokenUsage
	usage         TokenUsage

	hasEmittedText bool

	// pendingStepSeparator marks a completed model step so the next emitted
	// text is separated from what came before it. It is cosmetic and is
	// consumed on every emit.
	pendingStepSeparator bool

	// toolInvoked records that the turn invoked at least one tool. Unlike
	// pendingStepSeparator it is never cleared: it is the durable evidence
	// that a failed turn may have left side effects behind, which is what
	// decides whether the turn can be retried in a fresh session.
	toolInvoked bool

	// outcome is set by the session.execution.* event that ended the turn;
	// empty means the stream ended before the turn did.
	outcome         opencodeOutcome
	failure         *opencodeSessionError
	interruptReason string
}

func newOpencodeStreamState(sessionID string, onChunk func(string)) *opencodeStreamState {
	return &opencodeStreamState{
		sessionID:  sessionID,
		onChunk:    onChunk,
		textParts:  make(map[string]*opencodeTextPart),
		usageByMsg: make(map[string]TokenUsage),
	}
}
