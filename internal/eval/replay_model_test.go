package eval

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Pi reports model and provider as separate fields, so a candidate spelled
// xai/grok-4.6 is served as model "grok-4.6" plus provider "xai".
const piServedGrok46Reply = `{"type":"message_end","message":{"role":"assistant","provider":"xai","model":"grok-4.6","content":[{"type":"text","text":"{\"findings\":[],\"risk_level\":\"low\",\"risk_rationale\":\"clean\",\"risk_scope\":\"source-or-external\"}"}]}}
{"type":"agent_end","messages":[]}
`

const piServedMuseReply = `{"type":"message_end","message":{"role":"assistant","provider":"different-sidecar","model":"meta/muse-spark-1.3-contributor","content":[{"type":"text","text":"{\"findings\":[],\"risk_level\":\"low\",\"risk_rationale\":\"clean\",\"risk_scope\":\"source-or-external\"}"}]}}
{"type":"agent_end","messages":[]}
`

func TestReplayPiModelIdentityComparison(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, _ := setupCapturedRun(t, ctx)
	defer sourceDB.Close()

	fakeDir := t.TempDir()
	installFakePiJSONL(t, fakeDir, piServedGrok46Reply)
	t.Setenv("PATH", fakeDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := Capture(ctx, store, p, sourceDB, run.ID); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name          string
		model         string
		wantCompleted bool
	}{
		{name: "bare request", model: "grok-4.6", wantCompleted: true},
		{name: "qualified request", model: "xai/grok-4.6", wantCompleted: true},
		{name: "true mismatch", model: "openai/gpt-5", wantCompleted: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, evaluations, err := Replay(ctx, store, ReplayOptions{
				Set:       "all",
				Candidate: Candidate{Agent: types.AgentPi, Model: tt.model},
				Repeats:   1,
			})
			if len(evaluations) != 1 {
				t.Fatalf("evaluations = %#v", evaluations)
			}
			got := evaluations[0]
			if tt.wantCompleted {
				if err != nil {
					t.Fatalf("Replay: %v (evaluation=%#v)", err, got)
				}
				if got.Status != "completed" {
					t.Fatalf("status = %q error = %q, want completed", got.Status, got.Error)
				}
				return
			}
			if err == nil {
				t.Fatal("Replay succeeded, want a model-identity failure")
			}
			if got.Status == "completed" || !strings.Contains(got.Error, `candidate served model "grok-4.6", requested "openai/gpt-5"`) {
				t.Fatalf("evaluation = %#v, want the served/requested mismatch", got)
			}
		})
	}
}

func TestReplayPiAcceptsRequestedModelWithDifferentProviderSidecar(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, _ := setupCapturedRun(t, ctx)
	defer sourceDB.Close()

	fakeDir := t.TempDir()
	installFakePiJSONL(t, fakeDir, piServedMuseReply)
	t.Setenv("PATH", fakeDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := Capture(ctx, store, p, sourceDB, run.ID); err != nil {
		t.Fatal(err)
	}

	const requested = "meta/muse-spark-1.3-contributor"
	_, evaluations, err := Replay(ctx, store, ReplayOptions{
		Set:       "all",
		Candidate: Candidate{Agent: types.AgentPi, Model: requested},
		Repeats:   1,
	})
	if err != nil {
		t.Fatalf("Replay: %v (evaluations=%#v)", err, evaluations)
	}
	if len(evaluations) != 1 {
		t.Fatalf("evaluations = %#v, want one persisted replay result", evaluations)
	}
	got := evaluations[0]
	if got.Status != "completed" || got.Model != "meta/muse-spark-1.3-contributor" {
		t.Fatalf("evaluation = %#v, want completed replay under the served model", got)
	}
	t.Logf("replay accepted requested model %q as served model %q with Pi provider sidecar %q; persisted status=%s", requested, got.Model, "different-sidecar", got.Status)
}

func installFakePiJSONL(t *testing.T, fakeDir, reply string) {
	t.Helper()
	path := filepath.Join(fakeDir, "pi")
	var script string
	if runtime.GOOS == "windows" {
		path += ".cmd"
		script = "@echo off\r\nmore >nul\r\necho " + strings.ReplaceAll(strings.TrimSpace(reply), "\n", "\r\necho ") + "\r\n"
	} else {
		script = "#!/bin/sh\ncat >/dev/null\ncat <<'EOF'\n" + reply + "EOF\n"
	}
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}
