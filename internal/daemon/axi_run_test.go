package daemon

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

func TestAXIRunClaimIsSingleUseAndBoundToIntent(t *testing.T) {
	manager := &RunManager{axiRunClaims: make(map[string]axiRunClaim)}
	params := ipc.PrepareAXIRunParams{
		RepoID:  "repo-1",
		Branch:  "feature/x",
		HeadSHA: "abc123",
		Intent:  "PR destination: owner/repo",
	}
	token, err := manager.prepareAXIRun(params, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !manager.consumeAXIRun(token, params.RepoID, params.Branch, params.HeadSHA, params.Intent, 0) {
		t.Fatal("matching AXI run capability was refused")
	}
	if manager.consumeAXIRun(token, params.RepoID, params.Branch, params.HeadSHA, params.Intent, 0) {
		t.Fatal("AXI run capability was reusable")
	}

	token, err = manager.prepareAXIRun(params, 0)
	if err != nil {
		t.Fatal(err)
	}
	if manager.consumeAXIRun(token, params.RepoID, params.Branch, params.HeadSHA, "PR destination: other/repo", 0) {
		t.Fatal("AXI run capability authorized different intent")
	}
}
