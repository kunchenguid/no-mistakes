package citest

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps/internal/stepstest"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/scm/github"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The provider subprocess is fake: this verifies monitor integration, not live
// GitHub behavior. Assertions inspect the emitted gate and command protocol.
func TestCIStep_PersistentSelectorErrorDiagnostic(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		selector string
		wantSHA  bool
	}{
		{"SHA selector", "abc1234", true},
		{"numeric PR selector", "1000000", false},
		{"branch selector", "feature/foo", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)
			ag := &stepstest.MockAgent{AgentName: "test"}
			sctx := stepstest.NewTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
			providerError := "no pull requests found for branch '" + tc.selector + "'"
			binDir := stepstest.FakeCLIBinDir(t)
			stepstest.LinkFakeCLI(t, binDir, "gh")
			commandLog := filepath.Join(t.TempDir(), "provider-commands.log")
			sctx.Env = stepstest.FakeCLIEnv(binDir, map[string]string{
				"FAKE_CLI_MODE":        "ci-gh",
				"FAKE_CLI_STATE":       "OPEN",
				"FAKE_CLI_MERGEABLE":   "MERGEABLE",
				"FAKE_CLI_PR_HEAD_ERR": providerError,
				"FAKE_CLI_LOG":         commandLog,
			})
			// Exercise the actual selector at the provider's public interface.
			// CIStep below always selects a numeric PR from its recorded URL.
			ghName := "gh"
			if runtime.GOOS == "windows" {
				ghName += ".exe"
			}
			host := github.New(func(ctx context.Context, _ string, args ...string) *exec.Cmd {
				cmd := exec.CommandContext(ctx, filepath.Join(binDir, ghName), args...)
				cmd.Dir = dir
				cmd.Env = append(os.Environ(), sctx.Env...)
				return cmd
			}, nil, "github.com", "test/repo")
			if _, err := host.GetChecks(context.Background(), &scm.PR{Number: tc.selector, HeadSHA: headSHA}); err == nil || !strings.Contains(err.Error(), providerError) {
				t.Fatalf("PR-head lookup must surface selector stderr: %v", err)
			}
			prURL := "https://github.com/test/repo/pull/42"
			sctx.Run.PRURL = &prURL
			sctx.Config.CITimeout = time.Minute
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			sctx.Ctx = ctx
			readFailures := 0
			sctx.Log = func(message string) {
				if strings.HasPrefix(message, "warning: could not check CI:") {
					readFailures++
				}
			}
			waits := 0
			step := (&steps.CIStep{}).
				SetBaseBranchTip(func(context.Context) (string, bool) { return baseSHA, true }).
				SetWaitForNextPoll(func(context.Context, time.Duration) error {
					waits++
					return nil
				})
			outcome, err := step.Execute(sctx)
			if err != nil {
				t.Fatal(err)
			}
			if outcome == nil || !outcome.NeedsApproval || outcome.AutoFixable || outcome.Skipped {
				t.Fatalf("unexpected outcome: %+v", outcome)
			}
			if readFailures != steps.ConsecutiveCheckErrorLimit() || waits != steps.ConsecutiveCheckErrorLimit()-1 {
				t.Fatalf("read failures=%d waits=%d, want %d and %d", readFailures, waits, steps.ConsecutiveCheckErrorLimit(), steps.ConsecutiveCheckErrorLimit()-1)
			}
			var findings types.Findings
			if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
				t.Fatal(err)
			}
			if len(findings.Items) != 1 || findings.Items[0].Action != types.ActionAskUser {
				t.Fatalf("unexpected gate findings: %+v", findings)
			}
			desc := findings.Items[0].Description
			if !strings.Contains(desc, providerError) {
				t.Fatalf("provider stderr lost: %q", desc)
			}
			if got := strings.Contains(desc, "commit SHA was used as the PR selector"); got != tc.wantSHA {
				t.Fatalf("SHA hint=%t, want %t: %q", got, tc.wantSHA, desc)
			}
			if got := strings.Contains(desc, "gh >= 2.50"); got == tc.wantSHA {
				t.Fatalf("version hint=%t, want %t: %q", got, !tc.wantSHA, desc)
			}
			if len(ag.Calls) != 0 {
				t.Fatalf("diagnostic launched an agent: %+v", ag.Calls)
			}
			commands, err := os.ReadFile(commandLog)
			if err != nil {
				t.Fatal(err)
			}
			headReads, selectorReads := 0, 0
			for _, line := range strings.Split(strings.TrimSpace(string(commands)), "\n") {
				args := strings.Fields(line)
				allowed := len(args) >= 2 && args[0] == "auth" && args[1] == "status"
				if len(args) >= 3 && args[0] == "pr" && args[1] == "view" {
					isHeadRead := strings.Contains(line, "--json headRefOid")
					if args[2] == tc.selector && isHeadRead {
						selectorReads++
						allowed = true
					}
					if args[2] == "42" {
						allowed = true
						if isHeadRead {
							headReads++
						}
					}
				}
				if !allowed {
					t.Fatalf("diagnostic performed an unexpected operation (must not read checks, resolve or repair): %s", line)
				}
			}
			if selectorReads != 1 || headReads != steps.ConsecutiveCheckErrorLimit() {
				t.Fatalf("provider made %d selector lookups and %d monitor head reads, want 1 and %d", selectorReads, headReads, steps.ConsecutiveCheckErrorLimit())
			}
			t.Logf("Synthetic provider failure; emitted end-user finding:\n%s", outcome.Findings)
		})
	}
}
