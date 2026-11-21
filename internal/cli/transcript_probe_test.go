package cli

import (
	"context"
	"encoding/json"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

// serveTranscriptHomesProbe registers the capability probes a launch fixture
// expects from a current daemon.
func serveTranscriptHomesProbe(srv *ipc.Server) {
	srv.Handle(ipc.MethodProbeCommitIdentity, func(context.Context, json.RawMessage) (interface{}, error) {
		return map[string]bool{"ok": true}, nil
	})
	srv.Handle(ipc.MethodProbeTranscriptHomes, func(context.Context, json.RawMessage) (interface{}, error) {
		return &ipc.ProbeTranscriptHomesResult{OK: true}, nil
	})
}
