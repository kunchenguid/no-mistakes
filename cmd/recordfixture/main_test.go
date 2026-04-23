package main

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestSplitBinArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		def     string
		wantBin string
		want    []string
	}{
		{
			name:    "default bin keeps forwarded args",
			args:    []string{"--model", "sonnet", "--profile", "ci"},
			def:     "claude",
			wantBin: "claude",
			want:    []string{"--model", "sonnet", "--profile", "ci"},
		},
		{
			name:    "custom bin removed from forwarded args",
			args:    []string{"--model", "sonnet", "--bin", "/tmp/agent", "--profile", "ci"},
			def:     "claude",
			wantBin: "/tmp/agent",
			want:    []string{"--model", "sonnet", "--profile", "ci"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotBin, got := splitBinArgs(tt.args, tt.def)
			if gotBin != tt.wantBin {
				t.Fatalf("bin = %q, want %q", gotBin, tt.wantBin)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("forwarded args = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestTerminateWithFallbackKillsWhenTerminateFails(t *testing.T) {
	done := make(chan error, 1)
	done <- nil

	terminated := false
	killed := false
	errUnsupported := errors.New("unsupported")

	err := terminateWithFallback(func() error {
		terminated = true
		return errUnsupported
	}, func() error {
		killed = true
		return nil
	}, done, time.Millisecond, func(err error) bool {
		return errors.Is(err, errUnsupported)
	})
	if err != nil {
		t.Fatalf("terminateWithFallback: %v", err)
	}
	if !terminated {
		t.Fatal("expected terminate attempt")
	}
	if !killed {
		t.Fatal("expected kill fallback after terminate failure")
	}
}

func TestTerminateWithFallbackReturnsUnexpectedTerminateError(t *testing.T) {
	done := make(chan error, 1)
	errBoom := errors.New("boom")

	err := terminateWithFallback(func() error {
		return errBoom
	}, func() error {
		t.Fatal("kill should not run for unexpected terminate errors")
		return nil
	}, done, time.Millisecond, func(err error) bool {
		return false
	})
	if !errors.Is(err, errBoom) {
		t.Fatalf("error = %v, want %v", err, errBoom)
	}
	select {
	case <-done:
		t.Fatal("done channel should not be consumed on early return")
	default:
	}
}

func TestTerminateWithFallbackReturnsKillError(t *testing.T) {
	done := make(chan error, 1)
	errKill := errors.New("kill failed")

	err := terminateWithFallback(func() error {
		return errors.New("unsupported")
	}, func() error {
		return errKill
	}, done, 0, func(err error) bool {
		return err.Error() == "unsupported"
	})
	if !errors.Is(err, errKill) {
		t.Fatalf("error = %v, want %v", err, errKill)
	}
}
