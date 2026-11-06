package ipc_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

// The capability contract requires an affirmative, decodable reply. A health
// response or successful RPC with absent/false OK cannot prove account isolation.
func TestRequireTranscriptHomes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		probe func(context.Context, json.RawMessage) (interface{}, error)
		ok    bool
	}{
		{name: "older daemon"},
		{name: "declined", probe: func(context.Context, json.RawMessage) (interface{}, error) {
			return &ipc.ProbeTranscriptHomesResult{OK: false}, nil
		}},
		{name: "missing acknowledgement", probe: func(context.Context, json.RawMessage) (interface{}, error) {
			return json.RawMessage(`{}`), nil
		}},
		{name: "null acknowledgement", probe: func(context.Context, json.RawMessage) (interface{}, error) {
			return json.RawMessage(`null`), nil
		}},
		{name: "malformed acknowledgement", probe: func(context.Context, json.RawMessage) (interface{}, error) {
			return json.RawMessage(`{"ok":"yes"}`), nil
		}},
		{name: "probe error", probe: func(context.Context, json.RawMessage) (interface{}, error) {
			return nil, fmt.Errorf("synthetic probe failure")
		}},
		{name: "supported", ok: true, probe: func(context.Context, json.RawMessage) (interface{}, error) {
			return &ipc.ProbeTranscriptHomesResult{OK: true}, nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sock := socketPath(t)
			srv := startServer(t, sock)
			if tc.probe != nil {
				srv.Handle(ipc.MethodProbeTranscriptHomes, tc.probe)
			}
			client, err := ipc.Dial(sock)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			err = client.RequireTranscriptHomes()
			if tc.ok {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "transcript-home selection") || !strings.Contains(err.Error(), "no-mistakes daemon restart") {
				t.Fatalf("unconfirmed support must refuse with restart guidance: %v", err)
			}
		})
	}
}
