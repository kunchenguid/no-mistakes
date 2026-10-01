package citest

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps/internal/stepstest"
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
			sctx.Env = stepstest.FakeCIGHChecksError(t, "OPEN", "MERGEABLE", providerError)
			commandLog := filepath.Join(t.TempDir(), "provider-commands.log")
			sctx.Env = append(sctx.Env, "FAKE_CLI_LOG="+commandLog)
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
			checkReads := 0
			for _, line := range strings.Split(strings.TrimSpace(string(commands)), "\n") {
				args := strings.Fields(line)
				if len(args) >= 4 && args[0] == "api" && args[1] == "--hostname" {
					args = append([]string{"api"}, args[3:]...)
				}
				if len(args) >= 2 && args[0] == "api" && args[1] == "graphql" {
					checkReads++
				}
				allowed := len(args) >= 2 && ((args[0] == "auth" && args[1] == "status") || (args[0] == "api" && args[1] == "graphql"))
				allowed = allowed || (len(args) >= 3 && args[0] == "pr" && args[1] == "view" && args[2] == "42")
				if !allowed {
					t.Fatalf("diagnostic performed an unexpected operation (must not resolve or repair): %s", line)
				}
			}
			if checkReads != steps.ConsecutiveCheckErrorLimit() {
				t.Fatalf("provider made %d API reads, want only %d check reads and no SHA lookup", checkReads, steps.ConsecutiveCheckErrorLimit())
			}
			t.Logf("Synthetic provider failure; emitted end-user finding:\n%s", outcome.Findings)
		})
	}
}
