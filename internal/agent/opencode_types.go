package agent

import (
	"encoding/json"
	"net/http"
)

// opencodeStreamEvent is the top-level JSON from an OpenCode SSE data field.
type opencodeStreamEvent struct {
	Directory string                      `json:"directory,omitempty"`
	Payload   *opencodeStreamEventPayload `json:"payload,omitempty"`
}

type opencodeStreamEventPayload struct {
	Type       string                         `json:"type"`
	Properties *opencodeStreamEventProperties `json:"properties,omitempty"`
}

type opencodeStreamEventProperties struct {
	SessionID string                `json:"sessionID,omitempty"`
	Field     string                `json:"field,omitempty"`
	Delta     string                `json:"delta,omitempty"`
	PartID    string                `json:"partID,omitempty"`
	Part      *opencodeEventPart    `json:"part,omitempty"`
	Info      *opencodeEventInfo    `json:"info,omitempty"`
	Error     *opencodeMessageError `json:"error,omitempty"`
}

type opencodeEventPart struct {
	ID        string            `json:"id,omitempty"`
	MessageID string            `json:"messageID,omitempty"`
	Type      string            `json:"type,omitempty"`
	Text      string            `json:"text,omitempty"`
	Tokens    *opencodeTokens   `json:"tokens,omitempty"`
	Metadata  *opencodeMetadata `json:"metadata,omitempty"`
}

type opencodeEventInfo struct {
	ID     string          `json:"id,omitempty"`
	Role   string          `json:"role,omitempty"`
	Tokens *opencodeTokens `json:"tokens,omitempty"`
}

// opencodeTokens is the token usage structure in OpenCode responses.
type opencodeTokens struct {
	Input  int            `json:"input"`
	Output int            `json:"output"`
	Cache  *opencodeCache `json:"cache,omitempty"`
}

type opencodeCache struct {
	Read  int `json:"read"`
	Write int `json:"write"`
}

type opencodeMetadata struct {
	OpenAI *opencodeOpenAI `json:"openai,omitempty"`
}

type opencodeOpenAI struct {
	Phase string `json:"phase,omitempty"`
}

// opencodeMessageResponse is the JSON body from POST /session/{id}/message.
type opencodeMessageResponse struct {
	Info  *opencodeMessageInfo  `json:"info,omitempty"`
	Parts []opencodeMessagePart `json:"parts,omitempty"`
}

type opencodeMessageInfo struct {
	ID         string                `json:"id,omitempty"`
	Role       string                `json:"role,omitempty"`
	Structured json.RawMessage       `json:"structured,omitempty"`
	Error      *opencodeMessageError `json:"error,omitempty"`
	Tokens     *opencodeTokens       `json:"tokens,omitempty"`
}

// opencodeMessageError mirrors the discriminated AssistantError union in
// opencode's session-v1 schema (StructuredOutputError, ProviderAuthError,
// APIError, MessageOutputLengthError, MessageAbortedError,
// ContentFilterError, ContextOverflowError, UnknownError).
//
// opencode serializes every NamedError as {"name": ..., "data": {...}}, so
// the payload fields live under Data; the flat Message/Retries copies are
// kept only as a fallback for older wire shapes. Data also preserves the
// provider details that identify the narrow thinking/tool-choice fallback.
type opencodeMessageError struct {
	Name string                    `json:"name"`
	Data *opencodeMessageErrorData `json:"data,omitempty"`

	// Legacy flat shape, superseded by Data.
	Message string `json:"message,omitempty"`
	Retries *int   `json:"retries,omitempty"`

	// rawData is the verbatim `data` payload. The typed Data above reads the
	// fields we act on; this keeps the provider-specific extras beside them,
	// because thinking/tool_choice detection scans the payload as text and a
	// decode into the typed struct would silently drop whatever it has no
	// field for.
	rawData json.RawMessage
}

func (e *opencodeMessageError) UnmarshalJSON(b []byte) error {
	var wire struct {
		Name    string          `json:"name"`
		Data    json.RawMessage `json:"data,omitempty"`
		Message string          `json:"message,omitempty"`
		Retries *int            `json:"retries,omitempty"`
	}
	if err := json.Unmarshal(b, &wire); err != nil {
		return err
	}
	e.Name = wire.Name
	e.Message = wire.Message
	e.Retries = wire.Retries
	e.rawData = wire.Data
	e.Data = nil
	if len(wire.Data) > 0 {
		var data opencodeMessageErrorData
		// A `data` that is not the expected object still reaches the text
		// scan through rawData; only the typed reads lose it.
		if err := json.Unmarshal(wire.Data, &data); err == nil {
			e.Data = &data
		}
	}
	return nil
}

// providerText is every place a provider's own wording can land, as text.
func (e *opencodeMessageError) providerText() []string {
	if e == nil {
		return nil
	}
	text := []string{e.Name, e.message()}
	if len(e.rawData) > 0 {
		text = append(text, string(e.rawData))
	}
	return text
}

// opencodeMessageErrorData is the payload opencode nests under the error
// name. StatusCode and IsRetryable are only present on APIError, and
// IsRetryable is opencode's own verdict on whether the provider call is
// worth repeating - it is the authority for our retry decision.
type opencodeMessageErrorData struct {
	Message     string `json:"message,omitempty"`
	Retries     *int   `json:"retries,omitempty"`
	StatusCode  int    `json:"statusCode,omitempty"`
	IsRetryable *bool  `json:"isRetryable,omitempty"`
}

// IsStructuredOutput reports whether the error is opencode's
// StructuredOutputError, set when a `format: {type: "json_schema"}` prompt
// fails to elicit a StructuredOutput tool call after the configured retries.
func (e *opencodeMessageError) IsStructuredOutput() bool {
	return e != nil && e.Name == "StructuredOutputError"
}

func (e *opencodeMessageError) message() string {
	if e == nil {
		return ""
	}
	if e.Data != nil && e.Data.Message != "" {
		return e.Data.Message
	}
	return e.Message
}

func (e *opencodeMessageError) retries() int {
	if e == nil {
		return 0
	}
	if e.Data != nil && e.Data.Retries != nil {
		return *e.Data.Retries
	}
	if e.Retries != nil {
		return *e.Retries
	}
	return 0
}

func (e *opencodeMessageError) statusCode() int {
	if e == nil || e.Data == nil {
		return 0
	}
	return e.Data.StatusCode
}

// retryable reports whether repeating the turn could plausibly succeed.
// opencode's own isRetryable flag wins when present - a provider that
// rejected the request as invalid (a model refusing the forced tool_choice
// that json_schema output requires, for example) never becomes valid on a
// retry, and burning the step's remaining attempts on it only delays an
// actionable error. Without the flag we fall back to the status class, and
// an error carrying no status at all (StructuredOutputError, which has
// already exhausted opencode's internal retries) is not retried.
func (e *opencodeMessageError) retryable() bool {
	if e == nil {
		return false
	}
	if e.Data != nil && e.Data.IsRetryable != nil {
		return *e.Data.IsRetryable
	}
	switch code := e.statusCode(); {
	case code == http.StatusTooManyRequests, code == http.StatusRequestTimeout:
		return true
	case code >= 500:
		return true
	default:
		return false
	}
}

type opencodeMessagePart struct {
	Type     string            `json:"type,omitempty"`
	Text     string            `json:"text,omitempty"`
	Metadata *opencodeMetadata `json:"metadata,omitempty"`
}

// opencodeTextPart tracks accumulated text for a part ID during streaming.
type opencodeTextPart struct {
	text        string
	phase       string
	messageID   string
	emittedText string
}

// opencodeStreamState holds mutable state during SSE event processing.
type opencodeStreamState struct {
	sessionID       string
	onChunk         func(string)
	textParts       map[string]*opencodeTextPart
	textPartOrder   []string
	usageByMsg      map[string]TokenUsage
	usage           TokenUsage
	lastText        string
	lastFinalText   string
	userMsgIDs      map[string]bool
	assistantMsgIDs map[string]bool
	filteredPartIDs map[string]bool
	hasEmittedText  bool

	// pendingStepSeparator marks a completed model step so the next emitted
	// text is separated from what came before it. It is cosmetic and is
	// consumed on every emit.
	pendingStepSeparator bool

	// toolInvoked records that the turn invoked at least one tool. Unlike
	// pendingStepSeparator it is never cleared: it is the durable evidence
	// that a failed turn may have left side effects behind, which is what
	// decides whether the turn can be retried in a fresh session.
	toolInvoked bool
}
