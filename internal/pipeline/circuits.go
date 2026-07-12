package pipeline

import (
	"sync"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// providerCircuits is the run-wide provider-circuit state. A circuit opens when
// a Candidate's native adapter exhausts its retries with a classified
// operational failure; once open, every later Candidate in that provider domain
// is skipped for the rest of the gate. Each run starts with all circuits closed.
// The zero value is unusable; construct with newProviderCircuits.
type providerCircuits struct {
	mu   sync.Mutex
	open map[types.FailureDomain]bool
}

func newProviderCircuits() *providerCircuits {
	return &providerCircuits{open: make(map[types.FailureDomain]bool)}
}

// isOpen reports whether the provider domain's circuit is open for this run. A
// nil receiver or empty domain is always closed, so unrouted paths never skip.
func (c *providerCircuits) isOpen(domain types.FailureDomain) bool {
	if c == nil || domain == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.open[domain]
}

// markOpen opens a provider domain's circuit for the rest of the run. It is
// idempotent: the first classified operational failure opens the circuit and
// every later failure in the same domain leaves it open.
func (c *providerCircuits) markOpen(domain types.FailureDomain) {
	if c == nil || domain == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.open[domain] = true
}

func providerCircuitsFromAttempts(attempts []*db.InvocationAttempt) *providerCircuits {
	circuits := newProviderCircuits()
	for _, attempt := range attempts {
		if attempt == nil || attempt.Terminal == nil || attempt.Terminal.FailureDomain == "" {
			continue
		}
		if attempt.Terminal.Outcome != types.InvocationOutcomeFailed && attempt.Terminal.Outcome != types.InvocationOutcomeSkipped {
			continue
		}
		circuits.markOpen(attempt.Terminal.FailureDomain)
	}
	return circuits
}
