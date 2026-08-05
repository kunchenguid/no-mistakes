package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestInvocationLimiter_TwoActiveCallsQueueAndReleaseFIFO(t *testing.T) {
	limiter := NewInvocationLimiter(2)
	first, err := limiter.acquire(context.Background(), "first", nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := limiter.acquire(context.Background(), "second", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer second()

	admitted := make(chan string, 2)
	var releases sync.Map
	queue := func(name string) {
		go func() {
			release, err := limiter.acquire(context.Background(), name, nil)
			if err != nil {
				t.Errorf("acquire %s: %v", name, err)
				return
			}
			releases.Store(name, release)
			admitted <- name
		}()
	}
	queue("third")
	waitForLimiter(t, limiter, 2, 1)
	queue("fourth")
	waitForLimiter(t, limiter, 2, 2)
	select {
	case name := <-admitted:
		t.Fatalf("%s bypassed full limiter", name)
	default:
	}

	first()
	if got := <-admitted; got != "third" {
		t.Fatalf("first released waiter = %q, want third", got)
	}
	thirdRelease, ok := releases.Load("third")
	if !ok {
		t.Fatal("third admission had no release")
	}
	thirdRelease.(func())()
	if got := <-admitted; got != "fourth" {
		t.Fatalf("second released waiter = %q, want fourth", got)
	}
	fourthRelease, ok := releases.Load("fourth")
	if !ok {
		t.Fatal("fourth admission had no release")
	}
	fourthRelease.(func())()
	waitForLimiter(t, limiter, 1, 0) // second remains active until deferred cleanup
}

func TestInvocationLimiter_ReportsOnlyQueueWaitAndSlotCounts(t *testing.T) {
	limiter := NewInvocationLimiter(1)
	holder, err := limiter.acquire(context.Background(), "holder", nil)
	if err != nil {
		t.Fatal(err)
	}
	var events []LifecycleEvent
	var mu sync.Mutex
	done := make(chan error, 1)
	go func() {
		_, err := runWithRetry(context.Background(), "provider", RunOpts{
			invocationLimiter: limiter,
			OnLifecycle: func(event LifecycleEvent) {
				mu.Lock()
				events = append(events, event)
				mu.Unlock()
			},
		}, 0, classifyTransient, nil, func() (*Result, error) { return &Result{}, nil })
		done <- err
	}()
	waitForLimiter(t, limiter, 1, 1)
	holder()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 2 || events[0].Phase != LifecyclePhaseQueue || events[1].Phase != LifecyclePhaseAdmitted {
		t.Fatalf("lifecycle events = %+v, want queue then admitted", events)
	}
	for _, event := range events {
		if !containsAll(event.Message, "active_slots=", "provider") {
			t.Fatalf("event %q omitted privacy-safe slot observability", event.Message)
		}
	}
}

func TestInvocationLimiter_CancellationBeforeAndAfterAdmissionReleasesCapacity(t *testing.T) {
	limiter := NewInvocationLimiter(1)
	holder, err := limiter.acquire(context.Background(), "holder", nil)
	if err != nil {
		t.Fatal(err)
	}

	queuedCtx, cancelQueued := context.WithCancel(context.Background())
	queued := make(chan error, 1)
	queuedRan := make(chan struct{}, 1)
	go func() {
		_, err := runWithRetry(queuedCtx, "queued", RunOpts{invocationLimiter: limiter}, 0, classifyTransient, nil, func() (*Result, error) {
			queuedRan <- struct{}{}
			return nil, nil
		})
		queued <- err
	}()
	waitForLimiter(t, limiter, 1, 1)
	cancelQueued()
	if err := <-queued; !errors.Is(err, context.Canceled) {
		t.Fatalf("queued cancellation error = %v, want context.Canceled", err)
	}
	select {
	case <-queuedRan:
		t.Fatal("cancelled queued invocation ran")
	default:
	}
	holder()
	waitForLimiter(t, limiter, 0, 0)

	runningCtx, cancelRunning := context.WithCancel(context.Background())
	started := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		_, err := runWithRetry(runningCtx, "running", RunOpts{invocationLimiter: limiter}, 0, classifyTransient, nil, func() (*Result, error) {
			close(started)
			<-runningCtx.Done()
			return nil, runningCtx.Err()
		})
		finished <- err
	}()
	<-started
	waitForLimiter(t, limiter, 1, 0)
	cancelRunning()
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatalf("running cancellation error = %v, want context.Canceled", err)
	}
	waitForLimiter(t, limiter, 0, 0)
}

func TestInvocationLimiter_ReleasesAfterProviderErrorsPanicsAndFallback(t *testing.T) {
	limiter := NewInvocationLimiter(1)
	_, err := runWithRetry(context.Background(), "error", RunOpts{invocationLimiter: limiter}, 0, classifyTransient, nil, func() (*Result, error) {
		return nil, errors.New("provider error")
	})
	if err == nil {
		t.Fatal("provider error unexpectedly succeeded")
	}
	waitForLimiter(t, limiter, 0, 0)

	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("panic did not escape provider attempt")
			}
		}()
		_, _ = runWithRetry(context.Background(), "panic", RunOpts{invocationLimiter: limiter}, 0, classifyTransient, nil, func() (*Result, error) {
			panic("provider panic")
		})
	}()
	waitForLimiter(t, limiter, 0, 0)

	first := &limiterRetryAgent{name: "first", err: fmt.Errorf("first exited: unavailable")}
	second := &limiterRetryAgent{name: "second"}
	fallback := WithInvocationLimiter(NewFallback([]Agent{first, second}), limiter)
	if _, err := fallback.Run(context.Background(), RunOpts{}); err != nil {
		t.Fatalf("fallback run: %v", err)
	}
	if first.calls != 1 || second.calls != 1 {
		t.Fatalf("fallback calls = first:%d second:%d, want 1 each", first.calls, second.calls)
	}
	waitForLimiter(t, limiter, 0, 0)
}

func TestInvocationLimiter_NewDaemonRecoveryStartsWithNoLeasedCapacity(t *testing.T) {
	oldDaemon := NewInvocationLimiter(2)
	_, err := oldDaemon.acquire(context.Background(), "crashed-provider", nil)
	if err != nil {
		t.Fatal(err)
	}
	newDaemon := NewInvocationLimiter(2)
	waitForLimiter(t, newDaemon, 0, 0)
}

type limiterRetryAgent struct {
	name  string
	err   error
	calls int
}

func (a *limiterRetryAgent) Name() string { return a.name }
func (a *limiterRetryAgent) Close() error { return nil }
func (a *limiterRetryAgent) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	return runWithRetry(ctx, a.name, opts, 0, classifyTransient, nil, func() (*Result, error) {
		a.calls++
		return &Result{}, a.err
	})
}

func containsAll(text string, needles ...string) bool {
	for _, needle := range needles {
		if !strings.Contains(text, needle) {
			return false
		}
	}
	return true
}

func waitForLimiter(t *testing.T, limiter *InvocationLimiter, wantActive, wantWaiting int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		active, waiting, _ := limiter.snapshot()
		if active == wantActive && waiting == wantWaiting {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("limiter = active:%d waiting:%d, want active:%d waiting:%d", active, waiting, wantActive, wantWaiting)
		}
		time.Sleep(time.Millisecond)
	}
}
