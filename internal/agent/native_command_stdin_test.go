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
