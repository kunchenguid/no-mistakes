package steps

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestCIFixTreatsForgejoLogsAsOptionalEvidence(t *testing.T) {
	tests := []struct {
		name       string
		logs       string
		fetchErr   error
		wantLogs   bool
		wantMarker string
	}{
		{name: "available", logs: "Forgejo Actions run 91, job test:\nassertion failed", wantLogs: true, wantMarker: "assertion failed"},
		{name: "unavailable", fetchErr: errors.New("job log expired"), wantLogs: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, baseSHA, headSHA := setupGitRepo(t)
			var prompt string
			ag := &mockAgent{
				name: "test",
				runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
					prompt = opts.Prompt
					return &agent.Result{}, nil
				},
			}
			sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
			host := &forgejoLogTestHost{logs: tt.logs, err: tt.fetchErr}

			pushed, err := (&CIStep{}).autoFixCI(sctx, host, &scm.PR{Number: "42", URL: "https://forge.example/octo/widgets/pulls/42"}, []string{"CI / test (pull_request)"}, false)
			if err != nil {
				t.Fatalf("autoFixCI() error = %v", err)
			}
			if pushed {
				t.Fatal("autoFixCI() pushed without agent changes")
			}
			if host.calls != 1 {
				t.Fatalf("FetchFailedCheckLogs calls = %d, want 1", host.calls)
			}
			if !strings.Contains(prompt, "failing checks: CI / test (pull_request)") {
				t.Fatalf("prompt omitted failing check after optional log result:\n%s", prompt)
			}
			hasLogs := strings.Contains(prompt, "CI logs:")
			if hasLogs != tt.wantLogs {
				t.Fatalf("prompt CI logs presence = %v, want %v:\n%s", hasLogs, tt.wantLogs, prompt)
			}
			if tt.wantMarker != "" && !strings.Contains(prompt, tt.wantMarker) {
				t.Fatalf("prompt omitted useful Forgejo log marker:\n%s", prompt)
			}
		})
	}
}

type forgejoLogTestHost struct {
	scm.Host
	logs  string
	err   error
	calls int
}

func (h *forgejoLogTestHost) Capabilities() scm.Capabilities {
	return scm.Capabilities{FailedCheckLogs: true}
}

func (h *forgejoLogTestHost) FetchFailedCheckLogs(context.Context, *scm.PR, string, string, []string) (string, error) {
	h.calls++
	return h.logs, h.err
}
