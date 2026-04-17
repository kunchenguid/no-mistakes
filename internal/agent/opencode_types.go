package agent

import "encoding/json"

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
	SessionID string             `json:"sessionID,omitempty"`
	Field     string             `json:"field,omitempty"`
	Delta     string             `json:"delta,omitempty"`
	PartID    string             `json:"partID,omitempty"`
	Part      *opencodeEventPart `json:"part,omitempty"`
	Info      *opencodeEventInfo `json:"info,omitempty"`
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
	ID         string          `json:"id,omitempty"`
	Role       string          `json:"role,omitempty"`
	Structured json.RawMessage `json:"structured,omitempty"`
	Tokens     *opencodeTokens `json:"tokens,omitempty"`
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
	hadToolActivity bool
}
