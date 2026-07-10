//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// writeScenario writes a fake-agent scenario YAML to a temp file and returns
// its path, for a journey that needs behavior beyond the built-in clean default.
func writeScenario(t *testing.T, yaml string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "scenario.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write scenario: %v", err)
	}
	return path
}

// initGate runs `no-mistakes init` in the work dir, creating the gate remote
// and starting the daemon so a journey can push a branch to the pipeline.
func initGate(t *testing.T, h *Harness) {
	t.Helper()
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("no-mistakes init: %v\n%s", err, out)
	}
}

// attemptsForPurpose returns the attempts whose routed Candidate targeted the
// given Purpose, in start order (launched and skipped alike).
func attemptsForPurpose(attempts []*db.InvocationAttempt, purpose types.Purpose) []*db.InvocationAttempt {
	var out []*db.InvocationAttempt
	for _, a := range attempts {
		if a.Start.Purpose == purpose {
			out = append(out, a)
		}
	}
	return out
}

// succeededAttemptsFor returns launched attempts for a purpose whose terminal
// outcome succeeded, in start order.
func succeededAttemptsFor(attempts []*db.InvocationAttempt, purpose types.Purpose) []*db.InvocationAttempt {
	var out []*db.InvocationAttempt
	for _, a := range attemptsForPurpose(attempts, purpose) {
		if a.Terminal != nil && a.Terminal.Outcome == types.InvocationOutcomeSucceeded {
			out = append(out, a)
		}
	}
	return out
}

// circuitSkips returns attempts a run-wide open circuit skipped for a failure
// domain, recording the decision without launching a runner.
func circuitSkips(attempts []*db.InvocationAttempt, domain types.FailureDomain) []*db.InvocationAttempt {
	var out []*db.InvocationAttempt
	for _, a := range attempts {
		if a.Terminal != nil && a.Terminal.Outcome == types.InvocationOutcomeSkipped && a.Terminal.FailureDomain == domain {
			out = append(out, a)
		}
	}
	return out
}

// candidateModels returns each attempt's routed model in order.
func candidateModels(attempts []*db.InvocationAttempt) []string {
	models := make([]string, len(attempts))
	for i, a := range attempts {
		models[i] = a.Start.Candidate.Model
	}
	return models
}

// assertCandidate checks an attempt launched the expected routed Candidate
// (profile, escalation tier, model substring, and effort).
func assertCandidate(t *testing.T, a *db.InvocationAttempt, profile string, tier int, modelSubstr string, effort types.Effort) {
	t.Helper()
	c := a.Start.Candidate
	if c.Profile != profile || c.Tier != tier || !strings.Contains(c.Model, modelSubstr) || c.Effort != effort {
		t.Fatalf("candidate = {profile:%q tier:%d model:%q effort:%q}, want {profile:%q tier:%d model~%q effort:%q}",
			c.Profile, c.Tier, c.Model, c.Effort, profile, tier, modelSubstr, effort)
	}
}
