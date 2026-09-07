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

func TestCIFixSurfacesForgejoLogRetrievalFailures(t *testing.T) {
	tests := []struct {
		name       string
		logs       string
		fetchErr   error
		wantLogs   bool
		wantMarker string
	}{
		{name: "available", logs: "Forgejo Actions run 91, job test:\nassertion failed", wantLogs: true, wantMarker: "assertion failed"},
		{name: "unavailable", fetchErr: errors.New("job log expired"), wantLogs: true, wantMarker: "log retrieval incomplete: job log expired"},
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

			repair, err := (&CIStep{}).autoFixCI(sctx, host, &scm.PR{Number: "42", URL: "https://forge.example/octo/widgets/pulls/42"}, ciTargetsFor([]string{"CI / test (pull_request)"}, false))
			if err != nil {
				t.Fatalf("autoFixCI() error = %v", err)
			}
			if repair.HeadAdvanced {
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

func TestFetchCILogOutputBudgetsEachSelectedTargetAndSurfacesErrors(t *testing.T) {
	t.Parallel()

	host := &targetedLogTestHost{
		logs: map[string]string{
			"check-a": strings.Repeat("a", 32*1024) + " A_TAIL",
			"check-b": strings.Repeat("b", 32*1024) + " B_TAIL",
		},
		errs: map[string]error{"check-b": errors.New("job log expired")},
	}
	output := fetchCILogOutput(context.Background(), host, &scm.PR{Number: "42"}, "feature", "abc123", []scm.CheckTarget{
		{Name: "test", ProviderID: "check-a"},
		{Name: "test", ProviderID: "check-b"},
	}, 32*1024)
	if len(output) > 32*1024 {
		t.Fatalf("log output length = %d, want at most 32768", len(output))
	}
	for _, want := range []string{"Check \"test\" (check-a):", "Check \"test\" (check-b):", "A_TAIL", "B_TAIL", "[earlier log output omitted]", "[log retrieval incomplete: job log expired]"} {
		if !strings.Contains(output, want) {
			t.Fatalf("bounded targeted log output omitted %q:\n%s", want, output)
		}
	}
	if len(host.targets) != 2 || host.targets[0] != "check-a" || host.targets[1] != "check-b" {
		t.Fatalf("targeted fetches = %v, want one exact call per selected check", host.targets)
	}
}

type targetedLogTestHost struct {
	scm.Host
	logs    map[string]string
	errs    map[string]error
	targets []string
}

func (h *targetedLogTestHost) FetchFailedCheckTargetLogs(_ context.Context, _ *scm.PR, _, _ string, targets []scm.CheckTarget) (string, error) {
	id := targets[0].ProviderID
	h.targets = append(h.targets, id)
	return h.logs[id], h.errs[id]
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
