package agent

import (
	"context"
	"strings"
	"testing"
)

func TestTokenUsageNormalizesProviderInputBuckets(t *testing.T) {
	for _, tc := range []struct {
		name  string
		parse func(t *testing.T) TokenUsage
	}{
		{"claude", func(t *testing.T) TokenUsage {
			var usage TokenUsage
			events := `{"type":"assistant","message":{"usage":{"input_tokens":10000,"output_tokens":500,"cache_read_input_tokens":20000,"cache_creation_input_tokens":1000},"content":[]}}`
			if err := parseClaudeEvents(context.Background(), strings.NewReader(events), nil, &usage, nil); err != nil {
				t.Fatal(err)
			}
			return usage
		}},
		{"grok", func(t *testing.T) TokenUsage {
			events := `{"type":"result","subtype":"success","result":"done","usage":{"input_tokens":10000,"output_tokens":500,"cache_read_input_tokens":20000,"cache_creation_input_tokens":1000}}`
			result, err := parseGrokEvents(context.Background(), strings.NewReader(events), nil)
			if err != nil {
				t.Fatal(err)
			}
			return result.Usage
		}},
		{"pi", func(t *testing.T) TokenUsage {
			parser := &piParser{}
			events := `{"type":"message_end","message":{"role":"assistant","stopReason":"stop","content":[{"type":"text","text":"done"}],"usage":{"input":10000,"output":500,"cacheRead":20000,"cacheWrite":1000}}}`
			if err := parser.parse(context.Background(), strings.NewReader(events)); err != nil {
				t.Fatal(err)
			}
			return parser.usage
		}},
		{"opencode", func(t *testing.T) TokenUsage {
			state := &opencodeStreamState{sessionID: "s1", usageByMsg: make(map[string]TokenUsage)}
			events := "data: {\"payload\":{\"type\":\"message.updated\",\"properties\":{\"sessionID\":\"s1\",\"info\":{\"id\":\"m1\",\"role\":\"assistant\",\"tokens\":{\"input\":10000,\"output\":500,\"cache\":{\"read\":20000,\"write\":1000}}}}}}\n\ndata: {\"payload\":{\"type\":\"session.idle\"}}\n\n"
			if err := parseOpencodeSSE(strings.NewReader(events), state); err != nil {
				t.Fatal(err)
			}
			return state.usage
		}},
		{"rovodev", func(t *testing.T) TokenUsage {
			var usage TokenUsage
			var text string
			events := "event: request-usage\ndata: {\"input_tokens\":31000,\"output_tokens\":500,\"cache_read_tokens\":20000,\"cache_write_tokens\":1000}\n\n"
			if err := parseRovodevSSE(strings.NewReader(events), nil, &usage, &text); err != nil {
				t.Fatal(err)
			}
			return usage
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			usage := tc.parse(t)
			fresh := FreshInputTokens(usage.InputTokens, usage.CacheReadTokens+usage.CacheCreationTokens)
			if !usage.Reported || !usage.CacheCreationReported || usage.InputTokens != 31_000 ||
				usage.OutputTokens != 500 || usage.CacheReadTokens != 20_000 || usage.CacheCreationTokens != 1_000 ||
				usage.Total() != 31_500 || fresh != 10_000 {
				t.Fatalf("usage = %+v total %d fresh %d, want 31000 input, 31500 total, 10000 fresh", usage, usage.Total(), fresh)
			}
		})
	}
}
