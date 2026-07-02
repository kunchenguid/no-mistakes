package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// fakeAgent is a minimal in-test Agent. Run returns a result whose Text is the
// agent name, or runErr if set.
type fakeAgent struct {
	name   string
	runErr error
}

func (f *fakeAgent) Name() string { return f.name }

func (f *fakeAgent) Run(_ context.Context, _ RunOpts) (*Result, error) {
	if f.runErr != nil {
		return nil, f.runErr
	}
	return &Result{Text: f.name}, nil
}

func (f *fakeAgent) Close() error { return nil }

// blockingAgent signals each Run start on started and then blocks until release
// is closed. It lets a test observe how many agents are running concurrently.
type blockingAgent struct {
	name    string
	started chan struct{}
	release chan struct{}
}

func (b *blockingAgent) Name() string { return b.name }

func (b *blockingAgent) Run(_ context.Context, _ RunOpts) (*Result, error) {
	b.started <- struct{}{}
	<-b.release
	return &Result{Text: b.name}, nil
}

func (b *blockingAgent) Close() error { return nil }

func TestFanOut_ResultsInInputOrder(t *testing.T) {
	agents := []Agent{
		&fakeAgent{name: "codex"},
		&fakeAgent{name: "claude"},
		&fakeAgent{name: "pi"},
	}

	results := FanOut(context.Background(), agents, RunOpts{}, 0)

	if len(results) != len(agents) {
		t.Fatalf("got %d results, want %d", len(results), len(agents))
	}
	for i, want := range []string{"codex", "claude", "pi"} {
		if results[i].Err != nil {
			t.Errorf("results[%d].Err = %v, want nil", i, results[i].Err)
		}
		if results[i].Agent == nil || results[i].Agent.Name() != want {
			t.Errorf("results[%d].Agent = %v, want %q", i, results[i].Agent, want)
		}
		if results[i].Result == nil || results[i].Result.Text != want {
			t.Errorf("results[%d].Result = %v, want Text %q", i, results[i].Result, want)
		}
	}
}

func TestFanOut_Empty(t *testing.T) {
	results := FanOut(context.Background(), nil, RunOpts{}, 2)
	if len(results) != 0 {
		t.Fatalf("got %d results, want 0", len(results))
	}
}

func TestFanOut_OneAgentErrorIsolated(t *testing.T) {
	wantErr := errors.New("boom")
	agents := []Agent{
		&fakeAgent{name: "codex"},
		&fakeAgent{name: "claude", runErr: wantErr},
		&fakeAgent{name: "pi"},
	}

	results := FanOut(context.Background(), agents, RunOpts{}, 0)

	// The erroring agent's slot carries the error and a nil result.
	if !errors.Is(results[1].Err, wantErr) {
		t.Errorf("results[1].Err = %v, want %v", results[1].Err, wantErr)
	}
	if results[1].Result != nil {
		t.Errorf("results[1].Result = %v, want nil for erroring agent", results[1].Result)
	}
	// The other agents are unaffected: results present, no error.
	for _, i := range []int{0, 2} {
		if results[i].Err != nil {
			t.Errorf("results[%d].Err = %v, want nil", i, results[i].Err)
		}
		if results[i].Result == nil {
			t.Errorf("results[%d].Result = nil, want a result", i)
		}
	}
}

// countingAgent records how many times Run was invoked.
type countingAgent struct {
	name string
	ran  *atomic.Int32
}

func (c *countingAgent) Name() string { return c.name }

func (c *countingAgent) Run(_ context.Context, _ RunOpts) (*Result, error) {
	c.ran.Add(1)
	return &Result{Text: c.name}, nil
}

func (c *countingAgent) Close() error { return nil }

func TestFanOut_CancelledContextSkipsRun(t *testing.T) {
	var ran atomic.Int32
	agents := []Agent{
		&countingAgent{name: "codex", ran: &ran},
		&countingAgent{name: "claude", ran: &ran},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before FanOut runs

	results := FanOut(ctx, agents, RunOpts{}, 0)

	if got := ran.Load(); got != 0 {
		t.Errorf("Run invoked %d times, want 0 for a cancelled ctx", got)
	}
	for i := range results {
		if !errors.Is(results[i].Err, context.Canceled) {
			t.Errorf("results[%d].Err = %v, want context.Canceled", i, results[i].Err)
		}
		if results[i].Result != nil {
			t.Errorf("results[%d].Result = %v, want nil", i, results[i].Result)
		}
		if results[i].Agent == nil {
			t.Errorf("results[%d].Agent = nil, want the input agent attributed", i)
		}
	}
}

func TestFanOut_BoundedParallelism(t *testing.T) {
	const n = 4
	const maxParallel = 2

	// started is buffered so the first maxParallel goroutines can record their
	// start without blocking; goroutines beyond the limit are held at the
	// semaphore and never reach Run, so they never signal started.
	started := make(chan struct{}, n)
	release := make(chan struct{})
	agents := make([]Agent, n)
	for i := range agents {
		agents[i] = &blockingAgent{name: "r", started: started, release: release}
	}

	done := make(chan []FanOutResult, 1)
	go func() {
		done <- FanOut(context.Background(), agents, RunOpts{}, maxParallel)
	}()

	// Exactly maxParallel agents should start; the rest wait on the semaphore.
	for i := 0; i < maxParallel; i++ {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d agents started, want %d", i, maxParallel)
		}
	}
	select {
	case <-started:
		t.Fatalf("a %drd agent started; concurrency exceeded maxParallel=%d", maxParallel+1, maxParallel)
	case <-time.After(100 * time.Millisecond):
		// Expected: no further starts while the first batch is blocked.
	}

	// Release the panel and confirm every agent completes successfully.
	close(release)
	select {
	case results := <-done:
		if len(results) != n {
			t.Fatalf("got %d results, want %d", len(results), n)
		}
		for i, r := range results {
			if r.Err != nil {
				t.Errorf("results[%d].Err = %v, want nil", i, r.Err)
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("FanOut did not return after release")
	}
}
