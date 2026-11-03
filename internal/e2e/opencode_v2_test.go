//go:build e2e

package e2e

import "testing"

func TestOpenCodeV2PipelineJourney(t *testing.T) {
	t.Setenv("FAKEAGENT_OPENCODE_V2", "1")
	runHappyPath(t, "opencode")
}
