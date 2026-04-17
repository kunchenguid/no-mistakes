package agent

import (
	"strings"
	"testing"
)

func TestOpencodeTokensToUsage(t *testing.T) {
	tokens := &opencodeTokens{
		Input:  100,
		Output: 50,
		Cache:  &opencodeCache{Read: 30, Write: 10},
	}
	u := opencodeTokensToUsage(tokens)
	if u.InputTokens != 100 {
		t.Errorf("expected input 100, got %d", u.InputTokens)
	}
	if u.OutputTokens != 50 {
		t.Errorf("expected output 50, got %d", u.OutputTokens)
	}
	if u.CacheReadTokens != 30 {
		t.Errorf("expected cache read 30, got %d", u.CacheReadTokens)
	}
	if u.CacheCreationTokens != 10 {
		t.Errorf("expected cache creation 10, got %d", u.CacheCreationTokens)
	}
}

func TestOpencodeTokensToUsage_NoCache(t *testing.T) {
	tokens := &opencodeTokens{Input: 100, Output: 50}
	u := opencodeTokensToUsage(tokens)
	if u.CacheReadTokens != 0 || u.CacheCreationTokens != 0 {
		t.Error("expected zero cache tokens when cache is nil")
	}
}

func TestAccumulateUsage(t *testing.T) {
	byMsg := map[string]TokenUsage{
		"msg1": {InputTokens: 50, OutputTokens: 20},
		"msg2": {InputTokens: 100, OutputTokens: 30, CacheReadTokens: 10},
	}
	total := accumulateUsage(byMsg)
	if total.InputTokens != 150 {
		t.Errorf("expected input 150, got %d", total.InputTokens)
	}
	if total.OutputTokens != 50 {
		t.Errorf("expected output 50, got %d", total.OutputTokens)
	}
	if total.CacheReadTokens != 10 {
		t.Errorf("expected cache read 10, got %d", total.CacheReadTokens)
	}
}

func TestParseOpencodeSSE_PartDelta(t *testing.T) {
	input := `data: {"payload":{"type":"message.part.delta","properties":{"sessionID":"s1","field":"text","partID":"p1","delta":"hello "}}}

data: {"payload":{"type":"message.part.delta","properties":{"sessionID":"s1","field":"text","partID":"p1","delta":"world"}}}

data: {"payload":{"type":"message.part.updated","properties":{"sessionID":"s1","part":{"id":"p1","messageID":"asst-msg","type":"text","text":"hello world"}}}}

data: {"payload":{"type":"message.updated","properties":{"sessionID":"s1","info":{"id":"asst-msg","role":"assistant"}}}}

data: {"payload":{"type":"session.idle"}}

`
	state := &opencodeStreamState{
		sessionID:  "s1",
		textParts:  make(map[string]*opencodeTextPart),
		usageByMsg: make(map[string]TokenUsage),
	}
	var chunks []string
	state.onChunk = func(text string) { chunks = append(chunks, text) }

	err := parseOpencodeSSE(strings.NewReader(input), state)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d: %v", len(chunks), chunks)
	}
	if chunks[0] != "hello world" {
		t.Errorf("expected chunk 'hello world', got %q", chunks[0])
	}
	if state.lastText != "hello world" {
		t.Errorf("expected lastText 'hello world', got %q", state.lastText)
	}
}

func TestParseOpencodeSSE_PartUpdated_TextStreamsChunk(t *testing.T) {
	input := `data: {"payload":{"type":"message.part.updated","properties":{"sessionID":"s1","part":{"id":"p1","messageID":"asst-msg","type":"text","text":"streamed text"}}}}

data: {"payload":{"type":"message.updated","properties":{"sessionID":"s1","info":{"id":"asst-msg","role":"assistant"}}}}

data: {"payload":{"type":"session.idle"}}

`
	state := &opencodeStreamState{
		sessionID:  "s1",
		textParts:  make(map[string]*opencodeTextPart),
		usageByMsg: make(map[string]TokenUsage),
	}
	var chunks []string
	state.onChunk = func(text string) { chunks = append(chunks, text) }

	err := parseOpencodeSSE(strings.NewReader(input), state)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d: %v", len(chunks), chunks)
	}
	if chunks[0] != "streamed text" {
		t.Errorf("expected streamed chunk 'streamed text', got %q", chunks[0])
	}
}

func TestParseOpencodeSSE_PartUpdatedAfterDelta_StreamsOnlySuffix(t *testing.T) {
	input := `data: {"payload":{"type":"message.part.delta","properties":{"sessionID":"s1","field":"text","partID":"p1","delta":"hello"}}}

data: {"payload":{"type":"message.part.updated","properties":{"sessionID":"s1","part":{"id":"p1","messageID":"asst-msg","type":"text","text":"hello world"}}}}

data: {"payload":{"type":"message.updated","properties":{"sessionID":"s1","info":{"id":"asst-msg","role":"assistant"}}}}

data: {"payload":{"type":"session.idle"}}

`
	state := &opencodeStreamState{
		sessionID:  "s1",
		textParts:  make(map[string]*opencodeTextPart),
		usageByMsg: make(map[string]TokenUsage),
	}
	var chunks []string
	state.onChunk = func(text string) { chunks = append(chunks, text) }

	err := parseOpencodeSSE(strings.NewReader(input), state)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d: %v", len(chunks), chunks)
	}
	if chunks[0] != "hello world" {
		t.Errorf("expected chunk 'hello world', got %q", chunks[0])
	}
	if state.lastText != "hello world" {
		t.Errorf("expected lastText 'hello world', got %q", state.lastText)
	}
	if got := state.textParts["p1"]; got == nil || got.text != "hello world" {
		t.Fatalf("expected cached part text 'hello world', got %#v", got)
	}
}

func TestParseOpencodeSSE_DeltaAfterUpdated_AppendsFromLatestText(t *testing.T) {
	input := `data: {"payload":{"type":"message.part.updated","properties":{"sessionID":"s1","part":{"id":"p1","messageID":"asst-msg","type":"text","text":"hello"}}}}

data: {"payload":{"type":"message.part.updated","properties":{"sessionID":"s1","part":{"id":"p1","messageID":"asst-msg","type":"text","text":"hello world"}}}}

data: {"payload":{"type":"message.part.delta","properties":{"sessionID":"s1","field":"text","partID":"p1","delta":"!"}}}

data: {"payload":{"type":"message.updated","properties":{"sessionID":"s1","info":{"id":"asst-msg","role":"assistant"}}}}

data: {"payload":{"type":"session.idle"}}

`
	state := &opencodeStreamState{
		sessionID:  "s1",
		textParts:  make(map[string]*opencodeTextPart),
		usageByMsg: make(map[string]TokenUsage),
	}
	var chunks []string
	state.onChunk = func(text string) { chunks = append(chunks, text) }

	err := parseOpencodeSSE(strings.NewReader(input), state)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d: %v", len(chunks), chunks)
	}
	if chunks[0] != "hello world!" {
		t.Errorf("expected chunk 'hello world!', got %q", chunks[0])
	}
	if state.lastText != "hello world!" {
		t.Errorf("expected lastText 'hello world!', got %q", state.lastText)
	}
	if got := state.textParts["p1"]; got == nil || got.text != "hello world!" {
		t.Fatalf("expected cached part text 'hello world!', got %#v", got)
	}
}

func TestParseOpencodeSSE_PartUpdated_NonPrefixSnapshotStreamsCorrectedText(t *testing.T) {
	input := `data: {"payload":{"type":"message.part.delta","properties":{"sessionID":"s1","field":"text","partID":"p1","delta":"hello world"}}}

data: {"payload":{"type":"message.part.updated","properties":{"sessionID":"s1","part":{"id":"p1","messageID":"asst-msg","type":"text","text":"hello there"}}}}

data: {"payload":{"type":"message.updated","properties":{"sessionID":"s1","info":{"id":"asst-msg","role":"assistant"}}}}

data: {"payload":{"type":"session.idle"}}

`
	state := &opencodeStreamState{
		sessionID:  "s1",
		textParts:  make(map[string]*opencodeTextPart),
		usageByMsg: make(map[string]TokenUsage),
	}
	var chunks []string
	state.onChunk = func(text string) { chunks = append(chunks, text) }

	err := parseOpencodeSSE(strings.NewReader(input), state)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d: %v", len(chunks), chunks)
	}
	if chunks[0] != "hello there" {
		t.Errorf("expected corrected chunk 'hello there', got %q", chunks[0])
	}
	if state.lastText != "hello there" {
		t.Errorf("expected lastText 'hello there', got %q", state.lastText)
	}
	if got := state.textParts["p1"]; got == nil || got.text != "hello there" {
		t.Fatalf("expected cached part text 'hello there', got %#v", got)
	}
}

func TestParseOpencodeSSE_PartUpdated_Text(t *testing.T) {
	input := `data: {"payload":{"type":"message.part.updated","properties":{"sessionID":"s1","part":{"id":"p1","messageID":"asst-msg","type":"text","text":"final text","metadata":{"openai":{"phase":"final_answer"}}}}}}

data: {"payload":{"type":"message.updated","properties":{"sessionID":"s1","info":{"id":"asst-msg","role":"assistant"}}}}

data: {"payload":{"type":"session.idle"}}

`
	state := &opencodeStreamState{
		sessionID:  "s1",
		textParts:  make(map[string]*opencodeTextPart),
		usageByMsg: make(map[string]TokenUsage),
	}

	err := parseOpencodeSSE(strings.NewReader(input), state)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state.lastFinalText != "final text" {
		t.Errorf("expected lastFinalText 'final text', got %q", state.lastFinalText)
	}
	if state.lastText != "final text" {
		t.Errorf("expected lastText 'final text', got %q", state.lastText)
	}
}

func TestParseOpencodeSSE_StepFinish_Usage(t *testing.T) {
	input := `data: {"payload":{"type":"message.part.updated","properties":{"sessionID":"s1","part":{"id":"step1","messageID":"msg1","type":"step-finish","tokens":{"input":100,"output":50,"cache":{"read":20,"write":5}}}}}}

data: {"payload":{"type":"session.idle"}}

`
	state := &opencodeStreamState{
		sessionID:  "s1",
		textParts:  make(map[string]*opencodeTextPart),
		usageByMsg: make(map[string]TokenUsage),
	}

	err := parseOpencodeSSE(strings.NewReader(input), state)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state.usage.InputTokens != 100 {
		t.Errorf("expected input 100, got %d", state.usage.InputTokens)
	}
	if state.usage.OutputTokens != 50 {
		t.Errorf("expected output 50, got %d", state.usage.OutputTokens)
	}
	if state.usage.CacheReadTokens != 20 {
		t.Errorf("expected cache read 20, got %d", state.usage.CacheReadTokens)
	}
}

func TestParseOpencodeSSE_MessageUpdated_Usage(t *testing.T) {
	input := `data: {"payload":{"type":"message.updated","properties":{"sessionID":"s1","info":{"id":"msg1","role":"assistant","tokens":{"input":200,"output":80}}}}}

data: {"payload":{"type":"session.idle"}}

`
	state := &opencodeStreamState{
		sessionID:  "s1",
		textParts:  make(map[string]*opencodeTextPart),
		usageByMsg: make(map[string]TokenUsage),
	}

	err := parseOpencodeSSE(strings.NewReader(input), state)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state.usage.InputTokens != 200 {
		t.Errorf("expected input 200, got %d", state.usage.InputTokens)
	}
	if state.usage.OutputTokens != 80 {
		t.Errorf("expected output 80, got %d", state.usage.OutputTokens)
	}
}

func TestParseOpencodeSSE_FiltersOtherSessions(t *testing.T) {
	input := `data: {"payload":{"type":"message.part.delta","properties":{"sessionID":"other-session","field":"text","partID":"p1","delta":"should be ignored"}}}

data: {"payload":{"type":"message.part.updated","properties":{"sessionID":"s1","part":{"id":"p2","messageID":"asst-msg","type":"text","text":"included"}}}}

data: {"payload":{"type":"message.updated","properties":{"sessionID":"s1","info":{"id":"asst-msg","role":"assistant"}}}}

data: {"payload":{"type":"session.idle"}}

`
	state := &opencodeStreamState{
		sessionID:  "s1",
		textParts:  make(map[string]*opencodeTextPart),
		usageByMsg: make(map[string]TokenUsage),
	}
	var chunks []string
	state.onChunk = func(text string) { chunks = append(chunks, text) }

	err := parseOpencodeSSE(strings.NewReader(input), state)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 1 || chunks[0] != "included" {
		t.Errorf("expected 1 chunk 'included', got %v", chunks)
	}
}

func TestParseOpencodeSSE_FiltersUserMessageParts(t *testing.T) {
	input := strings.Join([]string{
		// User message comes first
		`data: {"payload":{"type":"message.updated","properties":{"sessionID":"s1","info":{"id":"user-msg","role":"user"}}}}`,
		``,
		`data: {"payload":{"type":"message.part.updated","properties":{"sessionID":"s1","part":{"id":"p-user","messageID":"user-msg","type":"text","text":"this is the prompt"}}}}`,
		``,
		// Then assistant response
		`data: {"payload":{"type":"message.part.updated","properties":{"sessionID":"s1","part":{"id":"p-asst","messageID":"asst-msg","type":"text","text":"response"}}}}`,
		``,
		`data: {"payload":{"type":"message.updated","properties":{"sessionID":"s1","info":{"id":"asst-msg","role":"assistant"}}}}`,
		``,
		`data: {"payload":{"type":"session.idle"}}`,
		``,
		``,
	}, "\n")

	state := &opencodeStreamState{
		sessionID:  "s1",
		textParts:  make(map[string]*opencodeTextPart),
		usageByMsg: make(map[string]TokenUsage),
	}
	var chunks []string
	state.onChunk = func(text string) { chunks = append(chunks, text) }

	err := parseOpencodeSSE(strings.NewReader(input), state)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should only get the assistant response, not the user prompt
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d: %v", len(chunks), chunks)
	}
	if chunks[0] != "response" {
		t.Errorf("expected 'response', got %q", chunks[0])
	}
}

func TestParseOpencodeSSE_DoesNotLeakUserDeltasBeforeRoleIsKnown(t *testing.T) {
	input := strings.Join([]string{
		`data: {"payload":{"type":"message.part.updated","properties":{"sessionID":"s1","part":{"id":"p-user","messageID":"user-msg","type":"text","text":"prompt"}}}}`,
		``,
		`data: {"payload":{"type":"message.part.delta","properties":{"sessionID":"s1","field":"text","partID":"p-user","delta":" details"}}}`,
		``,
		`data: {"payload":{"type":"message.updated","properties":{"sessionID":"s1","info":{"id":"user-msg","role":"user"}}}}`,
		``,
		`data: {"payload":{"type":"message.part.updated","properties":{"sessionID":"s1","part":{"id":"p-asst","messageID":"asst-msg","type":"text","text":"response"}}}}`,
		``,
		`data: {"payload":{"type":"message.updated","properties":{"sessionID":"s1","info":{"id":"asst-msg","role":"assistant"}}}}`,
		``,
		`data: {"payload":{"type":"session.idle"}}`,
		``,
		``,
	}, "\n")

	state := &opencodeStreamState{
		sessionID:  "s1",
		textParts:  make(map[string]*opencodeTextPart),
		usageByMsg: make(map[string]TokenUsage),
	}
	var chunks []string
	state.onChunk = func(text string) { chunks = append(chunks, text) }

	err := parseOpencodeSSE(strings.NewReader(input), state)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d: %v", len(chunks), chunks)
	}
	if chunks[0] != "response" {
		t.Fatalf("expected assistant response only, got %q", chunks[0])
	}
	if _, ok := state.textParts["p-user"]; ok {
		t.Fatalf("expected user part to be dropped, got %#v", state.textParts["p-user"])
	}
	if state.lastText != "response" {
		t.Fatalf("expected lastText to stay on assistant output, got %q", state.lastText)
	}
}

func TestParseOpencodeSSE_DoesNotLeakUnknownDeltaBeforeUserRoleIsKnown(t *testing.T) {
	input := strings.Join([]string{
		`data: {"payload":{"type":"message.part.delta","properties":{"sessionID":"s1","field":"text","partID":"p-user","delta":"prompt"}}}`,
		``,
		`data: {"payload":{"type":"message.part.updated","properties":{"sessionID":"s1","part":{"id":"p-user","messageID":"user-msg","type":"text","text":"prompt details"}}}}`,
		``,
		`data: {"payload":{"type":"message.updated","properties":{"sessionID":"s1","info":{"id":"user-msg","role":"user"}}}}`,
		``,
		`data: {"payload":{"type":"message.part.updated","properties":{"sessionID":"s1","part":{"id":"p-asst","messageID":"asst-msg","type":"text","text":"response"}}}}`,
		``,
		`data: {"payload":{"type":"message.updated","properties":{"sessionID":"s1","info":{"id":"asst-msg","role":"assistant"}}}}`,
		``,
		`data: {"payload":{"type":"session.idle"}}`,
		``,
		``,
	}, "\n")

	state := &opencodeStreamState{
		sessionID:  "s1",
		textParts:  make(map[string]*opencodeTextPart),
		usageByMsg: make(map[string]TokenUsage),
	}
	var chunks []string
	state.onChunk = func(text string) { chunks = append(chunks, text) }

	err := parseOpencodeSSE(strings.NewReader(input), state)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d: %v", len(chunks), chunks)
	}
	if chunks[0] != "response" {
		t.Fatalf("expected assistant response only, got %q", chunks[0])
	}
}

func TestParseOpencodeSSE_DoesNotClaimOrphanBeforeUserRoleArrives(t *testing.T) {
	input := strings.Join([]string{
		`data: {"payload":{"type":"message.part.delta","properties":{"sessionID":"s1","field":"text","partID":"p-user","delta":"prompt"}}}`,
		``,
		`data: {"payload":{"type":"message.updated","properties":{"sessionID":"s1","info":{"id":"asst-msg","role":"assistant"}}}}`,
		``,
		`data: {"payload":{"type":"message.part.updated","properties":{"sessionID":"s1","part":{"id":"p-user","messageID":"user-msg","type":"text","text":"prompt"}}}}`,
		``,
		`data: {"payload":{"type":"message.updated","properties":{"sessionID":"s1","info":{"id":"user-msg","role":"user"}}}}`,
		``,
		`data: {"payload":{"type":"message.part.updated","properties":{"sessionID":"s1","part":{"id":"p-asst","messageID":"asst-msg","type":"text","text":"response"}}}}`,
		``,
		`data: {"payload":{"type":"session.idle"}}`,
		``,
		``,
	}, "\n")

	state := &opencodeStreamState{
		sessionID:  "s1",
		textParts:  make(map[string]*opencodeTextPart),
		usageByMsg: make(map[string]TokenUsage),
	}
	var chunks []string
	state.onChunk = func(text string) { chunks = append(chunks, text) }

	err := parseOpencodeSSE(strings.NewReader(input), state)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d: %v", len(chunks), chunks)
	}
	if chunks[0] != "response" {
		t.Fatalf("expected assistant response only, got %q", chunks[0])
	}
	if _, ok := state.textParts["p-user"]; ok {
		t.Fatalf("expected user part to be dropped, got %#v", state.textParts["p-user"])
	}
	if state.lastText != "response" {
		t.Fatalf("expected lastText to stay on assistant output, got %q", state.lastText)
	}
}

func TestParseOpencodeSSE_DoesNotEmitDeltaOnlyAssistantWithoutOwnedPart(t *testing.T) {
	input := strings.Join([]string{
		`data: {"payload":{"type":"message.part.delta","properties":{"sessionID":"s1","field":"text","partID":"p1","delta":"hello "}}}`,
		``,
		`data: {"payload":{"type":"message.part.delta","properties":{"sessionID":"s1","field":"text","partID":"p1","delta":"world"}}}`,
		``,
		`data: {"payload":{"type":"message.updated","properties":{"sessionID":"s1","info":{"id":"asst-msg","role":"assistant"}}}}`,
		``,
		`data: {"payload":{"type":"session.idle"}}`,
		``,
		``,
	}, "\n")

	state := &opencodeStreamState{
		sessionID:       "s1",
		textParts:       make(map[string]*opencodeTextPart),
		usageByMsg:      make(map[string]TokenUsage),
		assistantMsgIDs: map[string]bool{"asst-msg": true},
	}
	var chunks []string
	state.onChunk = func(text string) { chunks = append(chunks, text) }

	err := parseOpencodeSSE(strings.NewReader(input), state)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 0 {
		t.Fatalf("expected no chunks, got %v", chunks)
	}
	if state.lastText != "" {
		t.Fatalf("expected lastText to remain empty, got %q", state.lastText)
	}
	if got := state.textParts["p1"]; got == nil || got.text != "hello world" || got.messageID != "" {
		t.Fatalf("expected cached part text 'hello world', got %#v", got)
	}
}

func TestParseOpencodeSSE_SkipsUserDeltasAfterRoleIsKnown(t *testing.T) {
	input := strings.Join([]string{
		`data: {"payload":{"type":"message.updated","properties":{"sessionID":"s1","info":{"id":"user-msg","role":"user"}}}}`,
		``,
		`data: {"payload":{"type":"message.part.updated","properties":{"sessionID":"s1","part":{"id":"p-user","messageID":"user-msg","type":"text","text":"prompt"}}}}`,
		``,
		`data: {"payload":{"type":"message.part.delta","properties":{"sessionID":"s1","field":"text","partID":"p-user","delta":" more"}}}`,
		``,
		`data: {"payload":{"type":"message.part.updated","properties":{"sessionID":"s1","part":{"id":"p-asst","messageID":"asst-msg","type":"text","text":"response"}}}}`,
		``,
		`data: {"payload":{"type":"message.updated","properties":{"sessionID":"s1","info":{"id":"asst-msg","role":"assistant"}}}}`,
		``,
		`data: {"payload":{"type":"session.idle"}}`,
		``,
		``,
	}, "\n")

	state := &opencodeStreamState{
		sessionID:  "s1",
		textParts:  make(map[string]*opencodeTextPart),
		usageByMsg: make(map[string]TokenUsage),
	}
	var chunks []string
	state.onChunk = func(text string) { chunks = append(chunks, text) }

	err := parseOpencodeSSE(strings.NewReader(input), state)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 1 || chunks[0] != "response" {
		t.Fatalf("expected assistant response only, got %v", chunks)
	}
}

func TestParseOpencodeSSE_PreservesBufferedPartOrder(t *testing.T) {
	input := strings.Join([]string{
		`data: {"payload":{"type":"message.part.updated","properties":{"sessionID":"s1","part":{"id":"p1","messageID":"asst-msg","type":"text","text":"hello "}}}}`,
		``,
		`data: {"payload":{"type":"message.part.updated","properties":{"sessionID":"s1","part":{"id":"p2","messageID":"asst-msg","type":"text","text":"world"}}}}`,
		``,
		`data: {"payload":{"type":"message.updated","properties":{"sessionID":"s1","info":{"id":"asst-msg","role":"assistant"}}}}`,
		``,
		`data: {"payload":{"type":"session.idle"}}`,
		``,
		``,
	}, "\n")

	for i := 0; i < 200; i++ {
		state := &opencodeStreamState{
			sessionID:  "s1",
			textParts:  make(map[string]*opencodeTextPart),
			usageByMsg: make(map[string]TokenUsage),
		}
		var chunks []string
		state.onChunk = func(text string) { chunks = append(chunks, text) }

		err := parseOpencodeSSE(strings.NewReader(input), state)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(chunks) != 2 {
			t.Fatalf("expected 2 chunks, got %d: %v", len(chunks), chunks)
		}
		if chunks[0] != "hello " || chunks[1] != "world" {
			t.Fatalf("expected buffered chunks in order, got %v on iteration %d", chunks, i)
		}
		if state.lastText != "world" {
			t.Fatalf("expected lastText 'world', got %q on iteration %d", state.lastText, i)
		}
	}
}

func TestParseOpencodeSSE_SeparatesAfterToolStep(t *testing.T) {
	input := strings.Join([]string{
		// First assistant text
		`data: {"payload":{"type":"message.part.updated","properties":{"sessionID":"s1","part":{"id":"p1","messageID":"msg1","type":"text","text":"first"}}}}`,
		``,
		`data: {"payload":{"type":"message.updated","properties":{"sessionID":"s1","info":{"id":"msg1","role":"assistant"}}}}`,
		``,
		// Tool step completes
		`data: {"payload":{"type":"message.part.updated","properties":{"sessionID":"s1","part":{"id":"step1","messageID":"msg1","type":"step-finish","tokens":{"input":10,"output":5}}}}}`,
		``,
		// Second assistant text
		`data: {"payload":{"type":"message.part.updated","properties":{"sessionID":"s1","part":{"id":"p2","messageID":"msg1","type":"text","text":"second"}}}}`,
		``,
		`data: {"payload":{"type":"session.idle"}}`,
		``,
		``,
	}, "\n")

	state := &opencodeStreamState{
		sessionID:  "s1",
		textParts:  make(map[string]*opencodeTextPart),
		usageByMsg: make(map[string]TokenUsage),
	}
	var chunks []string
	state.onChunk = func(text string) { chunks = append(chunks, text) }

	err := parseOpencodeSSE(strings.NewReader(input), state)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 3 {
		t.Fatalf("expected 3 chunks (text, separator, text), got %d: %v", len(chunks), chunks)
	}
	if chunks[0] != "first" {
		t.Errorf("expected 'first', got %q", chunks[0])
	}
	if chunks[1] != "\n\n" {
		t.Errorf("expected separator '\\n\\n', got %q", chunks[1])
	}
	if chunks[2] != "second" {
		t.Errorf("expected 'second', got %q", chunks[2])
	}
}

func TestParseOpencodeSSE_DoesNotSeparateWhenToolStepPrecedesFirstText(t *testing.T) {
	input := strings.Join([]string{
		`data: {"payload":{"type":"message.part.updated","properties":{"sessionID":"s1","part":{"id":"step1","messageID":"msg1","type":"step-finish","tokens":{"input":10,"output":5}}}}}`,
		``,
		`data: {"payload":{"type":"message.part.updated","properties":{"sessionID":"s1","part":{"id":"p1","messageID":"msg1","type":"text","text":"hello"}}}}`,
		``,
		`data: {"payload":{"type":"message.updated","properties":{"sessionID":"s1","info":{"id":"msg1","role":"assistant"}}}}`,
		``,
		`data: {"payload":{"type":"message.part.updated","properties":{"sessionID":"s1","part":{"id":"p1","messageID":"msg1","type":"text","text":"hello world"}}}}`,
		``,
		`data: {"payload":{"type":"session.idle"}}`,
		``,
		``,
	}, "\n")

	state := &opencodeStreamState{
		sessionID:  "s1",
		textParts:  make(map[string]*opencodeTextPart),
		usageByMsg: make(map[string]TokenUsage),
	}
	var chunks []string
	state.onChunk = func(text string) { chunks = append(chunks, text) }

	err := parseOpencodeSSE(strings.NewReader(input), state)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks without separator, got %d: %v", len(chunks), chunks)
	}
	if chunks[0] != "hello" {
		t.Errorf("expected first chunk 'hello', got %q", chunks[0])
	}
	if chunks[1] != " world" {
		t.Errorf("expected suffix chunk ' world', got %q", chunks[1])
	}
}

func TestParseOpencodeSSE_MalformedEvents(t *testing.T) {
	input := `data: not json at all

data: {"payload":{"type":"message.part.updated","properties":{"sessionID":"s1","part":{"id":"p1","messageID":"asst-msg","type":"text","text":"ok"}}}}

data: {"payload":{"type":"message.updated","properties":{"sessionID":"s1","info":{"id":"asst-msg","role":"assistant"}}}}

data: {"payload":{"type":"session.idle"}}

`
	state := &opencodeStreamState{
		sessionID:  "s1",
		textParts:  make(map[string]*opencodeTextPart),
		usageByMsg: make(map[string]TokenUsage),
	}

	err := parseOpencodeSSE(strings.NewReader(input), state)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state.lastText != "ok" {
		t.Errorf("expected lastText 'ok', got %q", state.lastText)
	}
}

func TestParseOpencodeSSE_EmptyData(t *testing.T) {
	input := "data: \n\ndata: {\"payload\":{\"type\":\"session.idle\"}}\n\n"
	state := &opencodeStreamState{
		sessionID:  "s1",
		textParts:  make(map[string]*opencodeTextPart),
		usageByMsg: make(map[string]TokenUsage),
	}

	err := parseOpencodeSSE(strings.NewReader(input), state)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state.lastText != "" {
		t.Errorf("expected empty lastText, got %q", state.lastText)
	}
}

func TestParseOpencodeSSE_StreamEndWithoutIdle(t *testing.T) {
	// Stream closes before session.idle - should not error
	input := `data: {"payload":{"type":"message.part.updated","properties":{"sessionID":"s1","part":{"id":"p1","messageID":"asst-msg","type":"text","text":"partial"}}}}

data: {"payload":{"type":"message.updated","properties":{"sessionID":"s1","info":{"id":"asst-msg","role":"assistant"}}}}

`
	state := &opencodeStreamState{
		sessionID:  "s1",
		textParts:  make(map[string]*opencodeTextPart),
		usageByMsg: make(map[string]TokenUsage),
	}

	err := parseOpencodeSSE(strings.NewReader(input), state)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state.lastText != "partial" {
		t.Errorf("expected lastText 'partial', got %q", state.lastText)
	}
}
