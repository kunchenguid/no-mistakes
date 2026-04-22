package main

import (
	"bytes"
	"context"
	"testing"
	"time"
)

func TestOpencodeSSECaptureWaitsForIdle(t *testing.T) {
	t.Helper()

	cap := newOpencodeSSECapture()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- cap.WaitForIdle(ctx)
	}()

	if _, err := cap.Write([]byte("event: message\ndata: {\"type\":\"session.idle\"}\n\n")); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := <-done; err != nil {
		t.Fatalf("wait for idle: %v", err)
	}
	if !bytes.Contains(cap.Bytes(), []byte("\"session.idle\"")) {
		t.Fatalf("captured bytes = %q, want session.idle", cap.Bytes())
	}
}
