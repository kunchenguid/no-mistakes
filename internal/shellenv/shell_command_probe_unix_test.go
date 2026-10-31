//go:build unix

package shellenv

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"
)

func TestShellCommandGroupActiveSlowPSStaysWithinWindow(t *testing.T) {
	for _, slowFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("slow_first=%t", slowFirst), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				// Use an existing group only for kill(0); ps is injected so no
				// real subprocess or wall-clock scheduling controls this test.
				pgid := syscall.Getpgrp()
				cmd := &exec.Cmd{Process: &os.Process{Pid: pgid}}
				const window = 2 * time.Second
				start := time.Now()
				calls := 0
				active, _, err := shellCommandGroupActive(cmd, window, func(ctx context.Context) ([]byte, error) {
					calls++
					if !slowFirst && calls == 1 {
						time.Sleep(1900 * time.Millisecond)
						return fmt.Appendf(nil, "%d S 123 worker\n", pgid), nil
					}
					<-ctx.Done()
					return nil, ctx.Err()
				})
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("active=%t err=%v; want unverified completion from deadline error", active, err)
				}
				if elapsed := time.Since(start); elapsed != window {
					t.Fatalf("slow ps returned at %v; want settle deadline %v", elapsed, window)
				}
				wantCalls := 1
				if !slowFirst {
					wantCalls = 2
				}
				if calls != wantCalls {
					t.Fatalf("ps calls=%d; want %d", calls, wantCalls)
				}
			})
		})
	}
}

func TestShellCommandGroupActiveInitialProbeBudget(t *testing.T) {
	for _, tc := range []struct {
		name   string
		window time.Duration
		budget time.Duration
	}{
		{"zero window", 0, 100 * time.Millisecond},
		{"expired window", -time.Second, 100 * time.Millisecond},
		{"small positive window", time.Millisecond, time.Millisecond},
		{"long window", 10 * time.Second, defaultWaitDelay},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cmd := &exec.Cmd{Process: &os.Process{Pid: syscall.Getpgrp()}}
				start := time.Now()
				_, _, err := shellCommandGroupActive(cmd, tc.window, func(ctx context.Context) ([]byte, error) {
					<-ctx.Done()
					return nil, ctx.Err()
				})
				if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != tc.budget {
					t.Fatalf("elapsed=%v err=%v; want %v and deadline error", time.Since(start), err, tc.budget)
				}
			})
		})
	}
}

func TestShellCommandGroupActiveExpiredPSCannotCertifyEmptyGroup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cmd := &exec.Cmd{Process: &os.Process{Pid: syscall.Getpgrp()}}
		_, _, err := shellCommandGroupActive(cmd, 2*time.Second, func(ctx context.Context) ([]byte, error) {
			<-ctx.Done()
			// Even apparently successful empty output cannot certify that
			// nothing is active once the probe's deadline has expired.
			return nil, nil
		})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err=%v; want unverified completion, not an empty-group verdict", err)
		}
	})
}

func TestShellCommandGroupActiveStopsAtDeadlineBetweenProbes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pgid := syscall.Getpgrp()
		cmd := &exec.Cmd{Process: &os.Process{Pid: pgid}}
		start := time.Now()
		calls := 0
		active, detail, err := shellCommandGroupActive(cmd, 2*time.Second, func(context.Context) ([]byte, error) {
			calls++
			time.Sleep(1990 * time.Millisecond)
			return fmt.Appendf(nil, "%d S 123 worker\n", pgid), nil
		})
		if !active || err != nil || !strings.Contains(detail, "worker") {
			t.Fatalf("active=%t detail=%q err=%v; want live worker", active, detail, err)
		}
		if calls != 1 || time.Since(start) != 2*time.Second {
			t.Fatalf("calls=%d elapsed=%v; want one probe and no cadence overrun", calls, time.Since(start))
		}
	})
}
