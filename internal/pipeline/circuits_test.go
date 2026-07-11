package pipeline

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestProviderCircuitsFromAttemptsRestoresClassifiedFailures(t *testing.T) {
	openAI := types.FailureDomainOpenAI
	circuits := providerCircuitsFromAttempts([]*db.InvocationAttempt{
		{Terminal: &types.InvocationAttemptTerminal{Outcome: types.InvocationOutcomeFailed, FailureDomain: openAI}},
		{Terminal: &types.InvocationAttemptTerminal{Outcome: types.InvocationOutcomeFailed}},
		nil,
	})
	if !circuits.isOpen(types.FailureDomainOpenAI) {
		t.Fatal("OpenAI circuit was not restored")
	}
	if circuits.isOpen(types.FailureDomainAnthropic) {
		t.Fatal("Anthropic circuit opened without a classified failure")
	}
}
