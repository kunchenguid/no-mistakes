package agent

import (
	"encoding/json"
	"io"
	"strings"
)

func opencodeTokensToUsage(t *opencodeTokens) TokenUsage {
	u := TokenUsage{
		InputTokens:  t.Input,
		OutputTokens: t.Output,
	}
	if t.Cache != nil {
		u.CacheReadTokens = t.Cache.Read
		u.CacheCreationTokens = t.Cache.Write
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

// parseOpencodeSSE processes the SSE stream from OpenCode's /global/event endpoint.
func parseOpencodeSSE(r io.Reader, state *opencodeStreamState) error {
	var sawIdle bool
	err := parseSSE(r, func(ev sseEvent) bool {
		if ev.Data == "" {
			return true
		}

		var event opencodeStreamEvent
		if err := json.Unmarshal([]byte(ev.Data), &event); err != nil {
			return true // skip malformed events
		}

		payload := event.Payload
		if payload == nil {
			return true
		}
		props := payload.Properties

		// Filter by session ID
		if props != nil && props.SessionID != "" && props.SessionID != state.sessionID {
			return true
		}

		switch payload.Type {
		case "message.part.delta":
			if props != nil && props.Field == "text" && props.PartID != "" && props.Delta != "" {
				if state.filteredPartIDs[props.PartID] {
					break
				}
				part := state.textParts[props.PartID]
				if part == nil {
					part = &opencodeTextPart{}
					state.textParts[props.PartID] = part
					state.trackTextPart(props.PartID)
				}
				part.text += props.Delta
				state.emitTextPartChunk(part, props.PartID)
			}

		case "message.part.updated":
			if props != nil && props.Part != nil {
				p := props.Part
				if p.Type == "text" && p.ID != "" {
					phase := ""
					if p.Metadata != nil && p.Metadata.OpenAI != nil {
						phase = p.Metadata.OpenAI.Phase
					}
					part := state.textParts[p.ID]
					if part == nil {
						part = &opencodeTextPart{}
						state.textParts[p.ID] = part
						state.trackTextPart(p.ID)
					}
					part.text = p.Text
					part.phase = phase
					if p.MessageID != "" {
						part.messageID = p.MessageID
					}
					if part.messageID != "" && state.userMsgIDs[part.messageID] {
						state.markPartFiltered(p.ID)
						delete(state.textParts, p.ID)
						break
					}
					state.emitTextPartChunk(part, p.ID)
				}
				if p.Type == "step-finish" {
					state.hadToolActivity = true
					if p.MessageID != "" && p.Tokens != nil {
						state.usageByMsg[p.MessageID] = opencodeTokensToUsage(p.Tokens)
						state.usage = accumulateUsage(state.usageByMsg)
					}
				}
			}

		case "message.updated":
			if props != nil && props.Info != nil {
				if props.Info.Role == "user" {
					if state.userMsgIDs == nil {
						state.userMsgIDs = make(map[string]bool)
					}
					state.userMsgIDs[props.Info.ID] = true
					state.dropMessageParts(props.Info.ID)
				}
				if props.Info.Role == "assistant" {
					if state.assistantMsgIDs == nil {
						state.assistantMsgIDs = make(map[string]bool)
					}
					state.assistantMsgIDs[props.Info.ID] = true
					state.emitBufferedMessageParts(props.Info.ID)
				}
				if props.Info.Role == "assistant" && props.Info.Tokens != nil {
					state.usageByMsg[props.Info.ID] = opencodeTokensToUsage(props.Info.Tokens)
					state.usage = accumulateUsage(state.usageByMsg)
				}
			}

		case "session.idle":
			sawIdle = true
			return false
		}

		return true
	})

	if err != nil {
		return err
	}
	if !sawIdle {
		// Stream ended without session.idle — not an error if message response
		// will provide the final result
	}
	return nil
}

func (s *opencodeStreamState) emitSeparatorIfNeeded() {
	if !s.hadToolActivity || s.onChunk == nil {
		return
	}
	if s.hasEmittedText {
		s.onChunk("\n\n")
	}
	s.hadToolActivity = false
}

func (s *opencodeStreamState) emitTextPartChunk(part *opencodeTextPart, partID string) {
	if part == nil || !s.shouldEmitTextPart(part) {
		return
	}
	chunk := ""
	if strings.HasPrefix(part.text, part.emittedText) {
		chunk = part.text[len(part.emittedText):]
	} else if part.text != "" {
		chunk = part.text
	}
	s.updateText(part.text, part.phase)
	if s.onChunk != nil && chunk != "" {
		s.emitSeparatorIfNeeded()
		s.onChunk(chunk)
		s.hasEmittedText = true
	}
	part.emittedText = part.text
	s.textParts[partID] = part
}

func (s *opencodeStreamState) shouldEmitTextPart(part *opencodeTextPart) bool {
	if part == nil {
		return false
	}
	if part.messageID == "" {
		return false
	}
	if s.userMsgIDs[part.messageID] {
		return false
	}
	return s.assistantMsgIDs[part.messageID]
}

func (s *opencodeStreamState) dropMessageParts(messageID string) {
	for partID, part := range s.textParts {
		if part != nil && part.messageID == messageID {
			s.markPartFiltered(partID)
			delete(s.textParts, partID)
		}
	}
}

func (s *opencodeStreamState) emitBufferedMessageParts(messageID string) {
	for _, partID := range s.textPartOrder {
		part := s.textParts[partID]
		if part != nil && part.messageID == messageID {
			s.emitTextPartChunk(part, partID)
		}
	}
}

func (s *opencodeStreamState) trackTextPart(partID string) {
	for _, existingPartID := range s.textPartOrder {
		if existingPartID == partID {
			return
		}
	}
	s.textPartOrder = append(s.textPartOrder, partID)
}

func (s *opencodeStreamState) markPartFiltered(partID string) {
	if s.filteredPartIDs == nil {
		s.filteredPartIDs = make(map[string]bool)
	}
	s.filteredPartIDs[partID] = true
}

func (s *opencodeStreamState) updateText(text, phase string) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return
	}
	s.lastText = text
	if phase == "final_answer" {
		s.lastFinalText = text
	}
}
