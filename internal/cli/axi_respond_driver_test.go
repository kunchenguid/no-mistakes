package cli

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

// axi respond is how an agent drives a run, so its --instructions reach the
// daemon marked as the driver's guidance, not the user's own words.
func TestAxiRespond_InstructionsAreMarkedAsTheDriversGuidance(t *testing.T) {
	var gotParams ipc.RespondParams
	var responded atomic.Bool
	fx := newAxiTimeoutFixture(t, axiTimeoutOpts{
		respond: func(_ context.Context, raw json.RawMessage) (interface{}, error) {
			if err := json.Unmarshal(raw, &gotParams); err != nil {
				return nil, err
			}
			responded.Store(true)
			return &ipc.RespondResult{OK: true, Fixed: []string{"R1"}, Ignored: []string{"R2"}}, nil
		},
	})
	fx.setGetRun(func(context.Context, int) (*ipc.RunInfo, error) {
		if responded.Load() {
			return fx.completed(), nil
		}
		return fx.awaiting(), nil
	})
	chdir(t, t.TempDir())

	out, err := executeCmd("axi", "respond", "--run", "run-timeout", "--action", "fix", "--findings", "R1", "--ignore", "R2", "--instructions", "only touch a.go", "--wait", "3s")
	if err != nil {
		t.Fatalf("respond: %v\n%s", err, out)
	}
	if gotParams.Instructions["R1"] != "only touch a.go" {
		t.Fatalf("forwarded instructions = %v", gotParams.Instructions)
	}
	if !gotParams.InstructionsFromDriver {
		t.Fatal("axi --instructions reached the daemon as the user's own instructions")
	}
}
