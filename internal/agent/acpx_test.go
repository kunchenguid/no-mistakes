package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestAcpxFirstPositive(t *testing.T) {
	cases := []struct {
		name   string
		values []int
		want   int
	}{
		{name: "empty", values: nil, want: 0},
		{name: "all zero", values: []int{0, 0, 0}, want: 0},
		{name: "all negative", values: []int{-1, -5, -100}, want: 0},
		{name: "single positive", values: []int{42}, want: 42},
		{name: "first positive wins even if later larger", values: []int{7, 999, 1}, want: 7},
		{name: "zeros then positive", values: []int{0, 0, 9, 0}, want: 9},
		{name: "negatives then positive", values: []int{-3, -2, 5, -1}, want: 5},
		{name: "leading zero does not count as positive", values: []int{0, 3}, want: 3},
		{name: "one is the smallest positive", values: []int{1}, want: 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := acpxFirstPositive(tc.values...); got != tc.want {
				t.Errorf("acpxFirstPositive(%v) = %d, want %d", tc.values, got, tc.want)
			}
		})
	}
}

func TestAcpxMaxUsage(t *testing.T) {
	cases := []struct {
		name string
		a    TokenUsage
		b    TokenUsage
		want TokenUsage
	}{
		{
			name: "both zero",
			a:    TokenUsage{},
			b:    TokenUsage{},
			want: TokenUsage{},
		},
		{
			name: "a zero b set",
			a:    TokenUsage{},
			b:    TokenUsage{InputTokens: 10, OutputTokens: 20, CacheReadTokens: 5, CacheCreationTokens: 2},
			want: TokenUsage{InputTokens: 10, OutputTokens: 20, CacheReadTokens: 5, CacheCreationTokens: 2},
		},
		{
			name: "a set b zero",
			a:    TokenUsage{InputTokens: 10, OutputTokens: 20, CacheReadTokens: 5, CacheCreationTokens: 2},
			b:    TokenUsage{},
			want: TokenUsage{InputTokens: 10, OutputTokens: 20, CacheReadTokens: 5, CacheCreationTokens: 2},
		},
		{
			name: "per-field max split across both",
			a:    TokenUsage{InputTokens: 100, OutputTokens: 1, CacheReadTokens: 50, CacheCreationTokens: 0},
			b:    TokenUsage{InputTokens: 1, OutputTokens: 200, CacheReadTokens: 0, CacheCreationTokens: 30},
			want: TokenUsage{InputTokens: 100, OutputTokens: 200, CacheReadTokens: 50, CacheCreationTokens: 30},
		},
		{
			name: "equal values",
			a:    TokenUsage{InputTokens: 7, OutputTokens: 7, CacheReadTokens: 7, CacheCreationTokens: 7},
			b:    TokenUsage{InputTokens: 7, OutputTokens: 7, CacheReadTokens: 7, CacheCreationTokens: 7},
			want: TokenUsage{InputTokens: 7, OutputTokens: 7, CacheReadTokens: 7, CacheCreationTokens: 7},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := acpxMaxUsage(tc.a, tc.b)
			if got != tc.want {
				t.Errorf("acpxMaxUsage(%+v, %+v) = %+v, want %+v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestAcpxUsageFieldsToTokenUsage(t *testing.T) {
	cases := []struct {
		name   string
		fields acpxUsageFields
		want   TokenUsage
	}{
		{
			name:   "all zero",
			fields: acpxUsageFields{},
			want:   TokenUsage{},
		},
		{
			name:   "snake_case input/output only",
			fields: acpxUsageFields{InputTokens: 10, OutputTokens: 20},
			want:   TokenUsage{InputTokens: 10, OutputTokens: 20},
		},
		{
			name:   "camelCase input/output only",
			fields: acpxUsageFields{InputTokensCamel: 11, OutputTokensCamel: 22},
			want:   TokenUsage{InputTokens: 11, OutputTokens: 22},
		},
		{
			name: "snake_case input beats camelCase when both set",
			fields: acpxUsageFields{
				InputTokens:      10,
				InputTokensCamel: 99,
			},
			want: TokenUsage{InputTokens: 10},
		},
		{
			name: "cache read first alias wins (cache_read_input_tokens)",
			fields: acpxUsageFields{
				CacheReadInputTokens:      30,
				CacheReadTokens:           31,
				CacheReadInputTokensCamel: 32,
			},
			want: TokenUsage{CacheReadTokens: 30},
		},
		{
			name: "cache creation first alias wins (cache_creation_input_tokens)",
			fields: acpxUsageFields{
				CacheCreationInputTokens: 40,
				CacheWriteInputTokens:    41,
				CacheWriteTokens:         42,
			},
			want: TokenUsage{CacheCreationTokens: 40},
		},
		{
			name: "cache read falls through to camel alias when snake zero",
			fields: acpxUsageFields{
				CachedReadTokensCamel: 77,
			},
			want: TokenUsage{CacheReadTokens: 77},
		},
		{
			name: "cache creation falls through to last alias",
			fields: acpxUsageFields{
				CachedWriteTokensCamel: 88,
			},
			want: TokenUsage{CacheCreationTokens: 88},
		},
		{
			name: "all fields populated picks first-positive per slot",
			fields: acpxUsageFields{
				InputTokens:              1,
				OutputTokens:             2,
				CacheReadInputTokens:     3,
				CacheCreationInputTokens: 4,
				InputTokensCamel:         100,
				OutputTokensCamel:        200,
				CacheReadTokensCamel:     300,
				CacheCreationTokensCamel: 400,
			},
			want: TokenUsage{
				InputTokens:         1,
				OutputTokens:        2,
				CacheReadTokens:     3,
				CacheCreationTokens: 4,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.want.Reported = tc.want != (TokenUsage{})
			tc.want.CacheCreationReported = tc.want.CacheCreationTokens > 0
			got := acpxUsageFieldsToTokenUsage(tc.fields)
			if got != tc.want {
				t.Errorf("acpxUsageFieldsToTokenUsage(%+v) = %+v, want %+v", tc.fields, got, tc.want)
			}
		})
	}
}

// TestAcpxUsageFields_JSONFieldAliases unmarshals JSON carrying a single
// alternative token-field name and verifies it lands in the correct
// TokenUsage slot. This catches silent regressions from JSON tag typos,
// which direct-struct tests (above) cannot detect.
func TestAcpxUsageFields_JSONFieldAliases(t *testing.T) {
	cases := []struct {
		name string
		json string
		want TokenUsage
	}{
		// Input aliases
		{name: "input_tokens", json: `{"input_tokens":7}`, want: TokenUsage{InputTokens: 7, Reported: true}},
		{name: "inputTokens", json: `{"inputTokens":7}`, want: TokenUsage{InputTokens: 7, Reported: true}},
		// Output aliases
		{name: "output_tokens", json: `{"output_tokens":9}`, want: TokenUsage{OutputTokens: 9, Reported: true}},
		{name: "outputTokens", json: `{"outputTokens":9}`, want: TokenUsage{OutputTokens: 9, Reported: true}},
		// Cache read aliases
		{name: "cache_read_input_tokens", json: `{"cache_read_input_tokens":11}`, want: TokenUsage{CacheReadTokens: 11, Reported: true}},
		{name: "cache_read_tokens", json: `{"cache_read_tokens":11}`, want: TokenUsage{CacheReadTokens: 11, Reported: true}},
		{name: "cached_input_tokens", json: `{"cached_input_tokens":11}`, want: TokenUsage{CacheReadTokens: 11, Reported: true}},
		{name: "cacheReadInputTokens", json: `{"cacheReadInputTokens":11}`, want: TokenUsage{CacheReadTokens: 11, Reported: true}},
		{name: "cachedInputTokens", json: `{"cachedInputTokens":11}`, want: TokenUsage{CacheReadTokens: 11, Reported: true}},
		{name: "cacheReadTokens", json: `{"cacheReadTokens":11}`, want: TokenUsage{CacheReadTokens: 11, Reported: true}},
		{name: "cachedReadTokens", json: `{"cachedReadTokens":11}`, want: TokenUsage{CacheReadTokens: 11, Reported: true}},
		// Cache creation aliases
		{name: "cache_creation_input_tokens", json: `{"cache_creation_input_tokens":13}`, want: TokenUsage{CacheCreationTokens: 13, Reported: true, CacheCreationReported: true}},
		{name: "cache_write_input_tokens", json: `{"cache_write_input_tokens":13}`, want: TokenUsage{CacheCreationTokens: 13, Reported: true, CacheCreationReported: true}},
		{name: "cache_write_tokens", json: `{"cache_write_tokens":13}`, want: TokenUsage{CacheCreationTokens: 13, Reported: true, CacheCreationReported: true}},
		{name: "cacheCreationInputTokens", json: `{"cacheCreationInputTokens":13}`, want: TokenUsage{CacheCreationTokens: 13, Reported: true, CacheCreationReported: true}},
		{name: "cacheCreationTokens", json: `{"cacheCreationTokens":13}`, want: TokenUsage{CacheCreationTokens: 13, Reported: true, CacheCreationReported: true}},
		{name: "cacheWriteTokens", json: `{"cacheWriteTokens":13}`, want: TokenUsage{CacheCreationTokens: 13, Reported: true, CacheCreationReported: true}},
		{name: "cachedWriteTokens", json: `{"cachedWriteTokens":13}`, want: TokenUsage{CacheCreationTokens: 13, Reported: true, CacheCreationReported: true}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var fields acpxUsageFields
			if err := json.Unmarshal([]byte(tc.json), &fields); err != nil {
				t.Fatalf("unmarshal %s: %v", tc.json, err)
			}
			markAcpxUsageFields(json.RawMessage(tc.json), &fields)
			got := acpxUsageFieldsToTokenUsage(fields)
			if got != tc.want {
				t.Errorf("json %s -> %+v, want %+v", tc.json, got, tc.want)
			}
		})
	}
}

func TestAcpxUpdateUsage(t *testing.T) {
	cases := []struct {
		name   string
		update acpxSessionUpdate
		want   TokenUsage
	}{
		{
			name:   "empty update",
			update: acpxSessionUpdate{},
			want:   TokenUsage{},
		},
		{
			name:   "only Used bumps InputTokens",
			update: acpxSessionUpdate{Used: 500},
			want:   TokenUsage{InputTokens: 500, Reported: true},
		},
		{
			name: "direct usage fields",
			update: acpxSessionUpdate{
				acpxUsageFields: acpxUsageFields{InputTokens: 10, OutputTokens: 20},
			},
			want: TokenUsage{InputTokens: 10, OutputTokens: 20, Reported: true},
		},
		{
			name: "_meta.usage contributes when direct fields zero",
			update: acpxSessionUpdate{
				Meta: struct {
					Usage acpxUsageFields `json:"usage"`
				}{Usage: acpxUsageFields{OutputTokens: 33, CacheReadInputTokens: 5}},
			},
			want: TokenUsage{OutputTokens: 33, CacheReadTokens: 5, Reported: true},
		},
		{
			name: "_meta.usage maxed against direct fields per-field",
			update: acpxSessionUpdate{
				acpxUsageFields: acpxUsageFields{InputTokens: 100, OutputTokens: 1},
				Meta: struct {
					Usage acpxUsageFields `json:"usage"`
				}{Usage: acpxUsageFields{InputTokens: 1, OutputTokens: 200}},
			},
			want: TokenUsage{InputTokens: 100, OutputTokens: 200, Reported: true},
		},
		{
			name: "Used overrides InputTokens when larger",
			update: acpxSessionUpdate{
				acpxUsageFields: acpxUsageFields{InputTokens: 50},
				Used:            100,
			},
			want: TokenUsage{InputTokens: 100, Reported: true},
		},
		{
			name: "Used does not override when not strictly greater",
			update: acpxSessionUpdate{
				acpxUsageFields: acpxUsageFields{InputTokens: 100},
				Used:            100,
			},
			want: TokenUsage{InputTokens: 100, Reported: true},
		},
		{
			name: "Used does not affect OutputTokens",
			update: acpxSessionUpdate{
				Used: 999,
			},
			want: TokenUsage{InputTokens: 999, Reported: true},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := acpxUpdateUsage(tc.update)
			if got != tc.want {
				t.Errorf("acpxUpdateUsage(%+v) = %+v, want %+v", tc.update, got, tc.want)
			}
		})
	}
}

func TestAcpxUpdateText(t *testing.T) {
	cases := []struct {
		name   string
		update acpxSessionUpdate
		want   string
	}{
		{
			name:   "empty",
			update: acpxSessionUpdate{},
			want:   "",
		},
		{
			name:   "Text field wins",
			update: acpxSessionUpdate{Text: "from text"},
			want:   "from text",
		},
		{
			name:   "Text takes precedence over Content",
			update: acpxSessionUpdate{Text: "from text", Content: json.RawMessage(`{"type":"text","text":"from content"}`)},
			want:   "from text",
		},
		{
			name:   "Content empty raw message",
			update: acpxSessionUpdate{Content: nil},
			want:   "",
		},
		{
			name:   "Content single object with text",
			update: acpxSessionUpdate{Content: json.RawMessage(`{"type":"text","text":"hello"}`)},
			want:   "hello",
		},
		{
			name:   "Content single object without text field",
			update: acpxSessionUpdate{Content: json.RawMessage(`{"type":"tool_use","text":""}`)},
			want:   "",
		},
		{
			name: "Content array concatenates text parts",
			update: acpxSessionUpdate{Content: json.RawMessage(`[
				{"type":"text","text":"part1"},
				{"type":"text","text":"part2"}
			]`)},
			want: "part1part2",
		},
		{
			name: "Content array skips empty text parts",
			update: acpxSessionUpdate{Content: json.RawMessage(`[
				{"type":"text","text":"a"},
				{"type":"text","text":""},
				{"type":"text","text":"b"}
			]`)},
			want: "ab",
		},
		{
			name:   "Content malformed JSON",
			update: acpxSessionUpdate{Content: json.RawMessage(`{not json`)},
			want:   "",
		},
		{
			name:   "Content array malformed JSON falls back to empty",
			update: acpxSessionUpdate{Content: json.RawMessage(`[not json]`)},
			want:   "",
		},
		{
			name:   "Content single object empty text",
			update: acpxSessionUpdate{Content: json.RawMessage(`{"type":"text","text":""}`)},
			want:   "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := acpxUpdateText(tc.update); got != tc.want {
				t.Errorf("acpxUpdateText() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEstimateAcpxTokens(t *testing.T) {
	cases := []struct {
		name      string
		charCount int
		want      int
	}{
		{name: "zero", charCount: 0, want: 0},
		{name: "negative one", charCount: -1, want: 0},
		{name: "large negative", charCount: -1000, want: 0},
		{name: "one char", charCount: 1, want: 1},
		{name: "three chars rounds up to one", charCount: 3, want: 1},
		{name: "four chars exactly one", charCount: 4, want: 1},
		{name: "five chars two tokens", charCount: 5, want: 2},
		{name: "eight chars two tokens", charCount: 8, want: 2},
		{name: "nine chars three tokens", charCount: 9, want: 3},
		{name: "one million chars", charCount: 1_000_000, want: 250_000},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := estimateAcpxTokens(tc.charCount); got != tc.want {
				t.Errorf("estimateAcpxTokens(%d) = %d, want %d", tc.charCount, got, tc.want)
			}
		})
	}
}

func TestBuildACPStructuredPrompt(t *testing.T) {
	t.Run("contains prompt, contract header, and schema in order", func(t *testing.T) {
		prompt := "review the code"
		schema := json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}}}`)
		got := buildACPStructuredPrompt(prompt, schema)

		promptIdx := strings.Index(got, prompt)
		headerIdx := strings.Index(got, "## no-mistakes final output contract")
		schemaIdx := strings.Index(got, string(schema))

		if promptIdx < 0 {
			t.Errorf("prompt not found in output: %s", got)
		}
		if headerIdx < 0 {
			t.Errorf("contract header not found in output: %s", got)
		}
		if schemaIdx < 0 {
			t.Errorf("schema not found in output: %s", got)
		}
		if !(promptIdx < headerIdx && headerIdx < schemaIdx) {
			t.Errorf("expected order prompt < header < schema; got prompt=%d header=%d schema=%d", promptIdx, headerIdx, schemaIdx)
		}
	})

	t.Run("exact output for known input", func(t *testing.T) {
		got := buildACPStructuredPrompt("P", json.RawMessage(`{}`))
		want := "P\n\n" +
			"## no-mistakes final output contract\n\n" +
			"When the task is complete, your final assistant message must be a single JSON object that matches this JSON Schema. " +
			"Return only the JSON object. Do not wrap it in Markdown fences. Do not include prose before or after the JSON.\n\n" +
			"{}"
		if got != want {
			t.Errorf("buildACPStructuredPrompt mismatch:\nwant %q\ngot  %q", want, got)
		}
	})

	t.Run("empty prompt still prepends header", func(t *testing.T) {
		got := buildACPStructuredPrompt("", json.RawMessage(`{}`))
		if !strings.HasPrefix(got, "\n\n## no-mistakes final output contract") {
			t.Errorf("empty prompt should still lead into contract header, got: %q", got)
		}
	})
}

func TestAcpxProcessErrorOutput(t *testing.T) {
	cases := []struct {
		name      string
		stderr    []byte
		stdoutErr string
		want      string
	}{
		{name: "both empty", stderr: nil, stdoutErr: "", want: ""},
		{name: "stderr only", stderr: []byte("boom"), stdoutErr: "", want: "boom"},
		{name: "stdoutErr only", stderr: nil, stdoutErr: "parse failed", want: "parse failed"},
		{name: "both joined with newline", stderr: []byte("boom"), stdoutErr: "parse failed", want: "boom\nparse failed"},
		{name: "stderr trimmed of surrounding whitespace", stderr: []byte("  boom  "), stdoutErr: "", want: "boom"},
		{name: "whitespace-only stderr treated as empty", stderr: []byte("   \n\t "), stdoutErr: "", want: ""},
		{name: "stderr internal newlines preserved", stderr: []byte("line1\nline2"), stdoutErr: "", want: "line1\nline2"},
		{name: "empty stderr bytes with stdoutErr", stderr: []byte{}, stdoutErr: "out", want: "out"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := acpxProcessErrorOutput(tc.stderr, tc.stdoutErr); got != tc.want {
				t.Errorf("acpxProcessErrorOutput(%q, %q) = %q, want %q", tc.stderr, tc.stdoutErr, got, tc.want)
			}
		})
	}
}

func TestParseAcpxJSONEvents_AgentMessageChunkText(t *testing.T) {
	events := `{"method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","text":"hello world"}}}
`
	var chunks []string
	var usage TokenUsage

	out, stdoutErr, err := parseAcpxJSONEvents(
		context.Background(),
		strings.NewReader(events),
		func(text string) { chunks = append(chunks, text) },
		&usage,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "hello world" {
		t.Errorf("output = %q, want %q", out, "hello world")
	}
	if stdoutErr != "" {
		t.Errorf("stdoutErr = %q, want empty", stdoutErr)
	}
	if len(chunks) != 1 || chunks[0] != "hello world" {
		t.Errorf("chunks = %v, want [hello world]", chunks)
	}
}

func TestParseAcpxJSONEvents_AgentMessageChunkContentArray(t *testing.T) {
	events := `{"method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","content":[{"type":"text","text":"part1"},{"type":"text","text":"part2"}]}}}
`
	var chunks []string
	var usage TokenUsage

	out, _, err := parseAcpxJSONEvents(
		context.Background(),
		strings.NewReader(events),
		func(text string) { chunks = append(chunks, text) },
		&usage,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "part1part2" {
		t.Errorf("output = %q, want %q", out, "part1part2")
	}
	// A single chunk carrying the assembled text.
	if len(chunks) != 1 || chunks[0] != "part1part2" {
		t.Errorf("chunks = %v, want [part1part2]", chunks)
	}
}

func TestParseAcpxJSONEvents_UsageUpdate(t *testing.T) {
	events := `{"method":"session/update","params":{"update":{"sessionUpdate":"usage_update","input_tokens":50,"output_tokens":25,"cache_read_input_tokens":10,"_meta":{"usage":{"cache_creation_input_tokens":5}},"used":100}}}
`
	var usage TokenUsage

	_, _, err := parseAcpxJSONEvents(context.Background(), strings.NewReader(events), nil, &usage)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// used=100 overrides input_tokens=50 (100 > 50); output from direct fields;
	// cache read from direct; cache creation from _meta.usage.
	want := TokenUsage{InputTokens: 100, OutputTokens: 25, CacheReadTokens: 10, CacheCreationTokens: 5, Reported: true, CacheCreationReported: true}
	if usage != want {
		t.Errorf("usage = %+v, want %+v", usage, want)
	}
}

func TestParseAcpxJSONEvents_ResultUsageNormalized(t *testing.T) {
	// A non-session/update message still contributes result.usage, and the
	// ~17 alternative JSON field names must normalize into TokenUsage.
	events := `{"method":"session/initialized","result":{"usage":{"inputTokens":7,"outputTokens":9,"cacheReadInputTokens":11,"cacheCreationInputTokens":13}}}
`
	var usage TokenUsage

	_, _, err := parseAcpxJSONEvents(context.Background(), strings.NewReader(events), nil, &usage)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := TokenUsage{InputTokens: 7, OutputTokens: 9, CacheReadTokens: 11, CacheCreationTokens: 13, Reported: true, CacheCreationReported: true}
	if usage != want {
		t.Errorf("usage = %+v, want %+v", usage, want)
	}
}

func TestParseAcpxJSONEvents_PreservesUsagePresence(t *testing.T) {
	events := strings.Join([]string{
		`{"method":"session/update","params":{"update":{"sessionUpdate":"usage_update","used":42}}}`,
		`{"method":"session/update","params":{"update":{"sessionUpdate":"usage_update","cache_write_tokens":0}}}`,
		"",
	}, "\n")
	var usage TokenUsage
	if _, _, err := parseAcpxJSONEvents(context.Background(), strings.NewReader(events), nil, &usage); err != nil {
		t.Fatal(err)
	}
	if !usage.Reported || usage.InputTokens != 42 {
		t.Fatalf("used-only usage = %+v", usage)
	}
	if !usage.CacheCreationReported || usage.CacheCreationTokens != 0 {
		t.Fatalf("zero cache creation = %+v", usage)
	}
}

func TestParseAcpxJSONEvents_UsageTracksMaxNotSum(t *testing.T) {
	// Two result events report different input token counts. The parser
	// tracks the max across events (not the sum), matching acpx semantics.
	events := `{"method":"session/initialized","result":{"usage":{"input_tokens":50,"output_tokens":10}}}
{"method":"session/initialized","result":{"usage":{"input_tokens":100,"output_tokens":5}}}
`
	var usage TokenUsage

	_, _, err := parseAcpxJSONEvents(context.Background(), strings.NewReader(events), nil, &usage)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if usage.InputTokens != 100 {
		t.Errorf("input tokens: want max 100, got %d", usage.InputTokens)
	}
	if usage.OutputTokens != 10 {
		t.Errorf("output tokens: want max 10, got %d", usage.OutputTokens)
	}
}

func TestParseAcpxJSONEvents_MultipleChunksAccumulate(t *testing.T) {
	events := strings.Join([]string{
		`{"method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","text":"hello "}}}`,
		`{"method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","text":"world"}}}`,
		"",
	}, "\n")

	var chunks []string
	var usage TokenUsage

	out, _, err := parseAcpxJSONEvents(
		context.Background(),
		strings.NewReader(events),
		func(text string) { chunks = append(chunks, text) },
		&usage,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "hello world" {
		t.Errorf("output = %q, want %q", out, "hello world")
	}
	wantChunks := []string{"hello ", "world"}
	if len(chunks) != len(wantChunks) {
		t.Fatalf("chunks = %v, want %v", chunks, wantChunks)
	}
	for i, want := range wantChunks {
		if chunks[i] != want {
			t.Errorf("chunk[%d] = %q, want %q", i, chunks[i], want)
		}
	}
}

func TestParseAcpxJSONEvents_CapturesFirstError(t *testing.T) {
	// Only the first non-empty error.message is surfaced as stdoutErr.
	events := strings.Join([]string{
		`{"method":"session/update","error":{"message":"first failure"}}`,
		`{"method":"session/update","error":{"message":"second failure"}}`,
		"",
	}, "\n")

	var usage TokenUsage
	out, stdoutErr, err := parseAcpxJSONEvents(context.Background(), strings.NewReader(events), nil, &usage)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stdoutErr != "first failure" {
		t.Errorf("stdoutErr = %q, want %q", stdoutErr, "first failure")
	}
	if out != "" {
		t.Errorf("output = %q, want empty", out)
	}
}

func TestParseAcpxJSONEvents_SkipsMalformedAndEmptyLines(t *testing.T) {
	events := strings.Join([]string{
		"not json at all",
		"",           // empty line
		`{"broken":`, // partial json
		`{"method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","text":"ok"}}}`,
		"",
	}, "\n")

	var chunks []string
	var usage TokenUsage

	out, _, err := parseAcpxJSONEvents(
		context.Background(),
		strings.NewReader(events),
		func(text string) { chunks = append(chunks, text) },
		&usage,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "ok" {
		t.Errorf("output = %q, want %q", out, "ok")
	}
	if len(chunks) != 1 || chunks[0] != "ok" {
		t.Errorf("chunks = %v, want [ok]", chunks)
	}
}

func TestParseAcpxJSONEvents_EmptyStream(t *testing.T) {
	var usage TokenUsage
	out, stdoutErr, err := parseAcpxJSONEvents(context.Background(), strings.NewReader(""), nil, &usage)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "" {
		t.Errorf("output = %q, want empty", out)
	}
	if stdoutErr != "" {
		t.Errorf("stdoutErr = %q, want empty", stdoutErr)
	}
	if usage != (TokenUsage{}) {
		t.Errorf("usage = %+v, want zero", usage)
	}
}

func TestParseAcpxJSONEvents_NilOnChunkSafe(t *testing.T) {
	// A nil onChunk callback must not panic when text is emitted.
	events := `{"method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","text":"safe"}}}
`
	var usage TokenUsage
	out, _, err := parseAcpxJSONEvents(context.Background(), strings.NewReader(events), nil, &usage)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "safe" {
		t.Errorf("output = %q, want %q", out, "safe")
	}
}

func TestParseAcpxJSONEvents_EmptyChunkTextSkipped(t *testing.T) {
	// An agent_message_chunk whose text resolves to "" is skipped entirely
	// (no empty onChunk invocation, nothing appended to output).
	events := `{"method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","text":""}}}
`
	var chunks []string
	var usage TokenUsage
	out, _, err := parseAcpxJSONEvents(
		context.Background(),
		strings.NewReader(events),
		func(text string) { chunks = append(chunks, text) },
		&usage,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "" {
		t.Errorf("output = %q, want empty", out)
	}
	if len(chunks) != 0 {
		t.Errorf("chunks = %v, want none", chunks)
	}
}

func TestParseAcpxJSONEvents_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before reading

	events := `{"method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","text":"never"}}}
`
	var usage TokenUsage
	_, _, err := parseAcpxJSONEvents(ctx, strings.NewReader(events), nil, &usage)
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
}
