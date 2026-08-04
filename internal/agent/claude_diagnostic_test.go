package agent

import (
	"strings"
	"testing"
)

func TestClaudeAPIErrorSniffer_ExtractsDiagnostic(t *testing.T) {
	for _, tc := range []struct {
		name   string
		writes []string
		want   string
	}{
		{
			name:   "no diagnostic",
			writes: []string{`{"type":"assistant","message":{"content":[{"type":"text","text":"all good"}]}}` + "\n"},
		},
		{
			name:   "stream-json assistant event stops at the closing quote",
			writes: []string{`{"type":"assistant","message":{"content":[{"type":"text","text":"API Error: Connection closed mid-response. The response above may be incomplete."}]}}` + "\n"},
			want:   "API Error: Connection closed mid-response. The response above may be incomplete.",
		},
		{
			name:   "plain text line",
			writes: []string{"API Error: Unable to connect to API (ENOTIMP)\n"},
			want:   "API Error: Unable to connect to API (ENOTIMP)",
		},
		{
			name:   "unterminated trailing line",
			writes: []string{"API Error: Unable to connect to API (ENOTIMP)"},
			want:   "API Error: Unable to connect to API (ENOTIMP)",
		},
		{
			name:   "split across writes",
			writes: []string{"API Er", "ror: Connection closed mid-r", "esponse.\n"},
			want:   "API Error: Connection closed mid-response.",
		},
		{
			name: "last diagnostic wins",
			writes: []string{
				"API Error: first\n",
				`{"type":"assistant","message":{"content":[{"type":"text","text":"thinking"}]}}` + "\n",
				"API Error: second\n",
			},
			want: "API Error: second",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var s claudeAPIErrorSniffer
			for _, w := range tc.writes {
				n, err := s.Write([]byte(w))
				if err != nil || n != len(w) {
					t.Fatalf("Write(%q) = %d, %v", w, n, err)
				}
			}
			if got := s.LastAPIError(); got != tc.want {
				t.Errorf("LastAPIError = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestClaudeAPIErrorSniffer_BoundsRetainedAndBufferedBytes(t *testing.T) {
	var s claudeAPIErrorSniffer
	line := "API Error: " + strings.Repeat("x", 4*claudeAPIErrorMaxLen)
	if _, err := s.Write([]byte(line + "\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := len(s.LastAPIError()); got != claudeAPIErrorMaxLen {
		t.Errorf("retained %d bytes, want the %d-byte bound", got, claudeAPIErrorMaxLen)
	}

	// A stream that never emits a newline must not grow the pending buffer
	// without bound, and must still surrender a diagnostic it already saw.
	var unterminated claudeAPIErrorSniffer
	if _, err := unterminated.Write([]byte("API Error: dropped stream ")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	for i := 0; i < 4; i++ {
		if _, err := unterminated.Write([]byte(strings.Repeat("y", claudeAPIErrorMaxPending))); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if got := len(unterminated.pending); got > claudeAPIErrorMaxPending {
			t.Fatalf("pending grew to %d bytes, want at most %d", got, claudeAPIErrorMaxPending)
		}
	}
	if !strings.HasPrefix(unterminated.LastAPIError(), "API Error: dropped stream") {
		t.Errorf("LastAPIError = %q, want the diagnostic seen before the flood", unterminated.LastAPIError())
	}
}
