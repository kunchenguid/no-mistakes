package agent

import "strings"

// claudeAPIErrorMarker is the prefix Claude Code uses for its own API-error
// diagnostics, e.g. "API Error: Connection closed mid-response. The response
// above may be incomplete."
const claudeAPIErrorMarker = "API Error:"

// claudeAPIErrorMaxLen bounds how much of a diagnostic is retained so a
// pathological stdout line can never blow up the error the agent returns.
const claudeAPIErrorMaxLen = 512

// claudeAPIErrorMaxPending bounds the buffer holding an unterminated line.
const claudeAPIErrorMaxPending = 64 * 1024

// claudeAPIErrorSniffer watches a copy of claude's stdout for the CLI's own
// "API Error: ..." diagnostic.
//
// Claude Code reports API transport failures as assistant text on stdout and
// leaves stderr empty, so an exit error composed from stderr alone is handed to
// the retry classifier as a bare "claude exited: exit status 1: " with the only
// evidence of a transient failure discarded. The sniffer matches the raw line,
// which covers both the stream-json assistant event that normally carries the
// text and any plain text claude writes directly.
type claudeAPIErrorSniffer struct {
	pending string
	last    string
}

// Write implements io.Writer for use with io.TeeReader. It never fails: a line
// holding no diagnostic simply leaves the retained value unchanged.
func (s *claudeAPIErrorSniffer) Write(p []byte) (int, error) {
	s.pending += string(p)
	for {
		i := strings.IndexByte(s.pending, '\n')
		if i < 0 {
			break
		}
		s.consume(s.pending[:i])
		s.pending = s.pending[i+1:]
	}
	if len(s.pending) > claudeAPIErrorMaxPending {
		// An unterminated line this long is not a stream-json event; take any
		// diagnostic it already holds and drop it rather than growing.
		s.consume(s.pending)
		s.pending = ""
	}
	return len(p), nil
}

// LastAPIError returns the most recent diagnostic seen, including one on a
// trailing line claude never terminated with a newline. Call it only after the
// stdout copy is complete.
func (s *claudeAPIErrorSniffer) LastAPIError() string {
	s.consume(s.pending)
	s.pending = ""
	return s.last
}

func (s *claudeAPIErrorSniffer) consume(line string) {
	idx := strings.LastIndex(line, claudeAPIErrorMarker)
	if idx < 0 {
		return
	}
	diag := line[idx:]
	// A stream-json event carries the text inside a JSON string, so stop at the
	// closing quote instead of trailing the rest of the envelope into the error.
	if strings.HasPrefix(strings.TrimSpace(line), "{") {
		if end := strings.IndexByte(diag, '"'); end >= 0 {
			diag = diag[:end]
		}
	}
	if len(diag) > claudeAPIErrorMaxLen {
		diag = diag[:claudeAPIErrorMaxLen]
	}
	if diag = strings.TrimSpace(diag); diag != "" {
		s.last = diag
	}
}

// claudeExitDiagnostic joins the evidence available after a non-zero exit.
// stderr stays first because that is where claude reports its own process-level
// failures; the stdout API-error diagnostic is appended because API transport
// failures land there instead, with stderr empty.
func claudeExitDiagnostic(stderr []byte, apiErr string) string {
	parts := make([]string, 0, 2)
	if s := strings.TrimSpace(string(stderr)); s != "" {
		parts = append(parts, s)
	}
	if apiErr != "" && !strings.Contains(string(stderr), apiErr) {
		parts = append(parts, apiErr)
	}
	return strings.Join(parts, "; ")
}
