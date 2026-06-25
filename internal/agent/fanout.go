package agent

import (
	"context"
	"sync"
)

// FanOutResult pairs a reviewer agent with the outcome of running it. For a
// completed agent exactly one of Result or Err is non-nil; Agent is always the
// input agent for that slot so callers can attribute the outcome.
type FanOutResult struct {
	Agent  Agent
	Result *Result
	Err    error
}

// FanOut runs the same RunOpts against every agent concurrently and returns one
// result slot per input agent, in input order. maxParallel bounds the number of
// agents running at once; maxParallel <= 0 means unbounded. A per-agent failure
// is captured in that agent's slot (Err) and never aborts the others - FanOut
// itself returns no top-level error. Each goroutine writes only its own
// preallocated slot, so the results slice needs no additional synchronization
// beyond the WaitGroup.
func FanOut(ctx context.Context, agents []Agent, opts RunOpts, maxParallel int) []FanOutResult {
	results := make([]FanOutResult, len(agents))
	if len(agents) == 0 {
		return results
	}

	// A buffered semaphore caps concurrency. nil means unbounded.
	var sem chan struct{}
	if maxParallel > 0 {
		sem = make(chan struct{}, maxParallel)
	}

	var wg sync.WaitGroup
	for i, ag := range agents {
		wg.Add(1)
		go func(i int, ag Agent) {
			defer wg.Done()
			results[i].Agent = ag
			// Honor cancellation while queued on the semaphore...
			if sem != nil {
				select {
				case sem <- struct{}{}:
					defer func() { <-sem }()
				case <-ctx.Done():
					results[i].Err = ctx.Err()
					return
				}
			}
			// ...and again before invoking Run, so an already-cancelled ctx
			// short-circuits instead of spawning the agent.
			if err := ctx.Err(); err != nil {
				results[i].Err = err
				return
			}
			res, err := ag.Run(ctx, opts)
			results[i].Result = res
			results[i].Err = err
		}(i, ag)
	}
	wg.Wait()
	return results
}
