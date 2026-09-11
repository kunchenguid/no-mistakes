package agent

import (
	"errors"
	"strings"
	"testing"
)

type failingWriteCloser struct {
	writeErr error
	closeErr error
}

func (w failingWriteCloser) Write([]byte) (int, error) { return 0, w.writeErr }
func (w failingWriteCloser) Close() error              { return w.closeErr }

func TestWriteNativeAgentStdinReportsWriteAndCloseFailures(t *testing.T) {
	writeErr := errors.New("write failed")
	closeErr := errors.New("close failed")
	err := <-writeNativeAgentStdin(failingWriteCloser{writeErr: writeErr, closeErr: closeErr}, "prompt")
	if !errors.Is(err, writeErr) || !errors.Is(err, closeErr) {
		t.Fatalf("error = %v, want joined write and close failures", err)
	}
	if got := err.Error(); !strings.Contains(got, "write failed") || !strings.Contains(got, "close failed") {
		t.Fatalf("error = %q, want both failure diagnostics", got)
	}
}

func TestCaptureNativeAgentStderrKeepsBoundedTail(t *testing.T) {
	prefix := strings.Repeat("a", nativeAgentStderrLimit)
	tail := "LAST-PI-DIAGNOSTIC"
	got := string(captureNativeAgentStderr(strings.NewReader(prefix + tail)))

	if !strings.Contains(got, "discarded") {
		t.Fatalf("captured stderr did not report truncation: %q", got[:min(len(got), 120)])
	}
	if !strings.HasSuffix(got, tail) {
		t.Fatalf("captured stderr lost diagnostic tail: suffix=%q", got[max(0, len(got)-64):])
	}
	if len(got) > nativeAgentStderrLimit+128 {
		t.Fatalf("captured stderr length = %d, want bounded near %d", len(got), nativeAgentStderrLimit)
	}
}
