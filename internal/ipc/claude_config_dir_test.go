package ipc_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

func TestRequireClaudeConfigDirSupport(t *testing.T) {
	profile := filepath.VolumeName(t.TempDir()) + "/caller/.claude1"
	for _, tc := range []struct {
		name       string
		health     *ipc.HealthResult
		profile    string
		wantErr    string
		wantHealth int32
	}{
		{name: "older daemon, no profile", health: &ipc.HealthResult{Status: "ok"}},
		{name: "older daemon, profile", health: &ipc.HealthResult{Status: "ok"}, profile: profile, wantErr: "no-mistakes daemon restart", wantHealth: 1},
		{name: "current daemon, profile", health: &ipc.HealthResult{Status: "ok", ClaudeConfigDir: true}, profile: profile, wantHealth: 1},
		{name: "health unavailable", profile: profile, wantErr: "check daemon support for CLAUDE_CONFIG_DIR", wantHealth: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sock := socketPath(t)
			srv := startServer(t, sock)
			var healthCalls atomic.Int32
			srv.Handle(ipc.MethodHealth, func(context.Context, json.RawMessage) (interface{}, error) {
				healthCalls.Add(1)
				if tc.health == nil {
					return nil, context.DeadlineExceeded
				}
				return tc.health, nil
			})
			client, err := ipc.Dial(sock)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()

			err = client.RequireClaudeConfigDirSupport(tc.profile)
			if tc.wantErr == "" && err != nil {
				t.Fatalf("unexpected refusal: %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
			}
			if got := healthCalls.Load(); got != tc.wantHealth {
				t.Fatalf("health calls = %d, want %d", got, tc.wantHealth)
			}
		})
	}
}
