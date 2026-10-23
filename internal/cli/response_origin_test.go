package cli

import (
	"context"
	"encoding/json"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestGateResponseCallerOrigin(t *testing.T) {
	for _, tc := range []struct {
		automatic bool
		name      string
		want      string
	}{
		{false, "manual", "interactive"},
		{true, "auto_yes", "auto_yes"},
	} {
		automatic, name, want := tc.automatic, tc.name, tc.want
		t.Run(name, func(t *testing.T) {
			for _, findings := range []string{"", `{"findings":[{"id":"R1","severity":"warning","description":"repair","action":"auto-fix"}]}`} {
				t.Run(findings, func(t *testing.T) {
					received := make(chan ipc.RespondParams, 1)
					completed := make(chan struct{})
					var responded atomic.Bool
					fx := newAxiTimeoutFixture(t, axiTimeoutOpts{subscribe: func(ctx context.Context, _ json.RawMessage) (ipc.StreamFunc, error) {
						return func(send func(interface{}) error) error {
							select {
							case <-completed:
								status := string(types.RunCompleted)
								return send(ipc.Event{Type: ipc.EventRunCompleted, RunID: "run-timeout", Status: &status})
							case <-ctx.Done():
								return nil
							}
						}, nil
					}, respond: func(_ context.Context, raw json.RawMessage) (interface{}, error) {
						var params ipc.RespondParams
						if err := json.Unmarshal(raw, &params); err != nil {
							return nil, err
						}
						received <- params
						responded.Store(true)
						close(completed)
						return &ipc.RespondResult{OK: true}, nil
					}})
					fx.setGetRun(func(context.Context, int) (*ipc.RunInfo, error) {
						if responded.Load() {
							return fx.completed(), nil
						}
						run := fx.awaiting()
						run.Steps[0].FindingsJSON = &findings
						return run, nil
					})
					if automatic {
						p, err := paths.New()
						if err != nil {
							t.Fatal(err)
						}
						client := dialReady(t, p.Socket())
						defer client.Close()
						ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
						defer cancel()
						if _, _, err := driveRun(ctx, io.Discard, client, p.Socket(), "run-timeout", true); err != nil {
							t.Fatal(err)
						}
					} else {
						args := []string{"axi", "respond", "--action", "approve", "--wait", "3s"}
						if findings != "" {
							args = []string{"axi", "respond", "--action", "fix", "--findings", "R1", "--wait", "3s"}
						}
						if out, err := executeCmd(args...); err != nil {
							t.Fatalf("respond: %v\n%s", err, out)
						}
					}
					select {
					case params := <-received:
						if string(params.Origin) != want {
							t.Fatalf("origin = %q, want %q", params.Origin, want)
						}
						action := types.ActionApprove
						if findings != "" {
							action = types.ActionFix
						}
						if params.Action != action {
							t.Fatalf("gate behavior changed: action = %s, want %s", params.Action, action)
						}
					default:
						t.Fatal("no response reached IPC server")
					}
				})
			}
		})
	}
}
