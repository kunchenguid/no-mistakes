package main

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

const (
	targetEnd   = "data: {\"id\":\"e\",\"type\":\"session.execution.succeeded\",\"data\":{\"sessionID\":\"target-session\"}}\n\n"
	otherEnd    = "data: {\"id\":\"e\",\"type\":\"session.execution.succeeded\",\"data\":{\"sessionID\":\"other-session\"}}\n\n"
	targetText  = "data: {\"id\":\"e\",\"type\":\"session.text.ended\",\"data\":{\"sessionID\":\"target-session\",\"assistantMessageID\":\"m\",\"ordinal\":0,\"text\":\"hi\"}}\n\n"
	otherText   = "data: {\"id\":\"e\",\"type\":\"session.text.ended\",\"data\":{\"sessionID\":\"other-session\",\"assistantMessageID\":\"m\",\"ordinal\":0,\"text\":\"no\"}}\n\n"
	unattribute = "data: {\"id\":\"e\",\"type\":\"server.connected\",\"data\":{}}\n\n"
)

func TestOpencodeSSECaptureWaitsForExecutionEnd(t *testing.T) {
	cap := newOpencodeSSECapture("target-session")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- cap.WaitForEnd(ctx)
	}()

	if _, err := cap.Write([]byte(targetEnd)); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := <-done; err != nil {
		t.Fatalf("wait for end: %v", err)
	}
	if !bytes.Contains(cap.Bytes(), []byte("session.execution.succeeded")) {
		t.Fatalf("captured bytes = %q, want the execution end", cap.Bytes())
	}
}

func TestOpencodeSSECaptureEveryExecutionEndCounts(t *testing.T) {
	for _, outcome := range []string{"succeeded", "failed", "interrupted"} {
		cap := newOpencodeSSECapture("target-session")
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		frame := "data: {\"id\":\"e\",\"type\":\"session.execution." + outcome + "\",\"data\":{\"sessionID\":\"target-session\"}}\n\n"
		if _, err := cap.Write([]byte(frame)); err != nil {
			t.Fatalf("write %s: %v", outcome, err)
		}
		if err := cap.WaitForEnd(ctx); err != nil {
			t.Fatalf("%s should end the capture: %v", outcome, err)
		}
		cancel()
	}
}

func TestOpencodeSSECaptureIgnoresExecutionEndSubstringInsideText(t *testing.T) {
	cap := newOpencodeSSECapture("target-session")
	endCtx, endCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer endCancel()

	frame := "data: {\"id\":\"e\",\"type\":\"session.text.ended\",\"data\":{\"sessionID\":\"target-session\",\"text\":\"mentions session.execution.succeeded in prose\"}}\n\n"
	if _, err := cap.Write([]byte(frame)); err != nil {
		t.Fatalf("write text event: %v", err)
	}
	if err := cap.WaitForEnd(endCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait for end error = %v, want deadline exceeded", err)
	}

	realCtx, realCancel := context.WithTimeout(context.Background(), time.Second)
	defer realCancel()
	if _, err := cap.Write([]byte(targetEnd)); err != nil {
		t.Fatalf("write end event: %v", err)
	}
	if err := cap.WaitForEnd(realCtx); err != nil {
		t.Fatalf("wait for real end: %v", err)
	}
}

func TestOpencodeSSECaptureIgnoresExecutionEndForOtherSession(t *testing.T) {
	cap := newOpencodeSSECapture("target-session")
	endCtx, endCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer endCancel()

	if _, err := cap.Write([]byte(otherEnd)); err != nil {
		t.Fatalf("write other-session end: %v", err)
	}
	if err := cap.WaitForEnd(endCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait for end error = %v, want deadline exceeded", err)
	}

	realCtx, realCancel := context.WithTimeout(context.Background(), time.Second)
	defer realCancel()
	if _, err := cap.Write([]byte(targetEnd)); err != nil {
		t.Fatalf("write target end: %v", err)
	}
	if err := cap.WaitForEnd(realCtx); err != nil {
		t.Fatalf("wait for target end: %v", err)
	}
}

func TestOpencodeSSECaptureExcludesEventsFromOtherSessionsButKeepsUnattributedOnes(t *testing.T) {
	cap := newOpencodeSSECapture("target-session")

	for _, frame := range []string{unattribute, otherText, targetText, targetEnd} {
		if _, err := cap.Write([]byte(frame)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	got := string(cap.Bytes())
	if bytes.Contains([]byte(got), []byte("other-session")) {
		t.Fatalf("captured bytes = %q, want other-session events excluded", got)
	}
	if !bytes.Contains([]byte(got), []byte("target-session")) {
		t.Fatalf("captured bytes = %q, want target-session events", got)
	}
	if !bytes.Contains([]byte(got), []byte("server.connected")) {
		t.Fatalf("captured bytes = %q, want unattributed events kept as the real stream carries them", got)
	}
}

func TestOpencodeSSECaptureStopsRecordingAfterTheEnd(t *testing.T) {
	cap := newOpencodeSSECapture("target-session")
	for _, frame := range []string{targetEnd, targetText} {
		if _, err := cap.Write([]byte(frame)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if bytes.Contains(cap.Bytes(), []byte("session.text.ended")) {
		t.Fatalf("captured bytes = %q, want nothing after the execution end", cap.Bytes())
	}
}

// Frames split across writes must still be reassembled on the "\n\n" boundary.
func TestOpencodeSSECaptureReassemblesSplitFrames(t *testing.T) {
	cap := newOpencodeSSECapture("target-session")
	half := len(targetEnd) / 2
	if _, err := cap.Write([]byte(targetEnd[:half])); err != nil {
		t.Fatalf("write first half: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	if err := cap.WaitForEnd(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("half a frame ended the capture: %v", err)
	}
	cancel()
	if _, err := cap.Write([]byte(targetEnd[half:])); err != nil {
		t.Fatalf("write second half: %v", err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := cap.WaitForEnd(ctx); err != nil {
		t.Fatalf("wait for end: %v", err)
	}
}
