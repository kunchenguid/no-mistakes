package agent

import (
	"encoding/json"
	"io"
	"strconv"
	"strings"
)

func opencodeTokensToUsage(t *opencodeTokens) TokenUsage {
	u := TokenUsage{
		InputTokens:  t.Input,
		OutputTokens: t.Output,
		Reported:     true,
	}
	if t.Cache != nil {
		u.CacheReadTokens = t.Cache.Read
		u.CacheCreationTokens = t.Cache.Write
		u.CacheCreationReported = true
	}
	if t.Reasoning != nil {
		u.ReasoningTokens = *t.Reasoning
		u.ReasoningReported = true
	}
	return u
}

func accumulateUsage(byMsg map[string]TokenUsage) TokenUsage {
	var total TokenUsage
	for _, u := range byMsg {
		total.Add(u)
	}
	return total
}

// parseOpencodeSSE processes the SSE stream from opencode's GET /api/event
// endpoint for one session until the turn's agent loop ends. It returns nil
// both when a session.execution.* event closed the turn (state.outcome is
// then set) and when the stream simply ended (state.outcome stays empty); the
// caller tells the two apart, because a stream that died mid-turn is not a
// turn that finished.
//
// The stream is global to the server, so every event is filtered by
// sessionID first. Events of other kinds (project.*, model.*, ...) and
// session events the adapter has no use for are skipped.
func parseOpencodeSSE(r io.Reader, state *opencodeStreamState) error {
	return parseSSE(r, func(ev sseEvent) bool {
		if ev.Data == "" {
			return true
		}

		var event opencodeEvent
		if err := json.Unmarshal([]byte(ev.Data), &event); err != nil {
			return true // skip malformed events
		}
		if !strings.HasPrefix(event.Type, "session.") {
			return true
		}
		var data opencodeEventData
		if len(event.Data) > 0 {
			if err := json.Unmarshal(event.Data, &data); err != nil {
				return true
			}
		}
		// Every session.* event names its session; one that does not, or
		// names another session, is not this turn's.
		if data.SessionID == "" || data.SessionID != state.sessionID {
			return true
		}

		switch event.Type {
		case "session.text.delta":
			if data.Delta == "" {
				break
			}
			part := state.part(data.AssistantMessageID, data.Ordinal)
			part.text += data.Delta
			state.emitTextPart(part)

		case "session.text.ended":
			part := state.part(data.AssistantMessageID, data.Ordinal)
			part.text = data.Text
			state.emitTextPart(part)

		case "session.step.ended", "session.step.failed":
			state.pendingStepSeparator = true
			if data.AssistantMessageID != "" && data.Tokens != nil {
				state.usageByMsg[data.AssistantMessageID] = opencodeTokensToUsage(data.Tokens)
				state.usage = accumulateUsage(state.usageByMsg)
			}
			if event.Type == "session.step.failed" && data.Error != nil {
				state.failure = data.Error
			}

		case "session.tool.called", "session.tool.success", "session.tool.failed":
			state.toolInvoked = true

		case "session.execution.succeeded":
			state.outcome = opencodeOutcomeSucceeded
			return false

		case "session.execution.failed":
			state.outcome = opencodeOutcomeFailed
			if data.Error != nil {
				state.failure = data.Error
			}
			return false

		case "session.execution.interrupted":
			state.outcome = opencodeOutcomeInterrupted
			state.interruptReason = data.Reason
			return false
		}

		return true
	})
}

// part returns the text block for one assistant message ordinal, creating
// and ordering it on first sight.
func (s *opencodeStreamState) part(messageID string, ordinal int) *opencodeTextPart {
	key := messageID + "/" + strconv.Itoa(ordinal)
	part := s.textParts[key]
	if part == nil {
		part = &opencodeTextPart{}
		s.textParts[key] = part
		s.textPartOrder = append(s.textPartOrder, key)
	}
	return part
}

func (s *opencodeStreamState) emitSeparatorIfNeeded() {
	if !s.pendingStepSeparator || s.onChunk == nil {
		return
	}
	if s.hasEmittedText {
		s.onChunk("\n\n")
	}
	s.pendingStepSeparator = false
}

// emitTextPart streams whatever of the part's text has not been shown yet.
// A delta appends, so the new suffix is emitted; a session.text.ended
// snapshot normally equals what the deltas built, and emits nothing, but a
// snapshot that is not a prefix extension is a correction and is emitted
// whole.
func (s *opencodeStreamState) emitTextPart(part *opencodeTextPart) {
	if part == nil {
		return
	}
	chunk := ""
	if strings.HasPrefix(part.text, part.emittedText) {
		chunk = part.text[len(part.emittedText):]
	} else if part.text != "" {
		chunk = part.text
	}
	if s.onChunk != nil && chunk != "" {
		s.emitSeparatorIfNeeded()
		s.onChunk(chunk)
		s.hasEmittedText = true
	}
	part.emittedText = part.text
}

// outputText joins the streamed text blocks in order, one blank line between
// them. It is the fallback for the message list when that cannot be read
// after a turn the stream saw finish.
func (s *opencodeStreamState) outputText() string {
	var parts []string
	for _, key := range s.textPartOrder {
		if part := s.textParts[key]; part != nil && strings.TrimSpace(part.text) != "" {
			parts = append(parts, part.text)
		}
	}
	return strings.Join(parts, "\n\n")
}
