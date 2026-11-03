package steps

import (
	"fmt"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
)

// With rerere.enabled and rerere.autoupdate in the operator's git config, a
// conflict git has seen before is resolved and staged from the recorded
// resolution, yet rebase and merge still exit 1 with no unmerged paths left.
// The step must not depend on that ambient setting: it still reports the
// conflict, or hands it to the agent when fixing, instead of failing with
// "exit status 1" or "no conflicts detected".
func TestRebaseStep_RerereAutoupdateStillReportsConflict(t *testing.T) {
	t.Parallel()
	for _, strategy := range []string{"", config.RebaseStrategyMerge} {
		for _, fixing := range []bool{false, true} {
			t.Run(fmt.Sprintf("strategy=%s/fixing=%t", strategy, fixing), func(t *testing.T) {
				t.Parallel()
				f := newMergeFixture(t, true)
				gitCmd(t, f.dir, "config", "rerere.enabled", "true")
				gitCmd(t, f.dir, "config", "rerere.autoupdate", "true")

				// Record a resolution for this conflict, then put the branch back.
				if out, err := runGitDirect(f.dir, "rebase", "origin/main"); err == nil {
					t.Fatalf("expected the recording rebase to conflict, got:\n%s", out)
				}
				writeFixtureFile(t, f.dir, "shared.txt", "main line\nfeature line\n")
				gitCmd(t, f.dir, "add", "shared.txt")
				gitCmd(t, f.dir, "-c", "core.editor=true", "rebase", "--continue")
				gitCmd(t, f.dir, "reset", "--hard", f.headSHA)

				ag := &mockAgent{name: "test"}
				sctx := f.context(t, ag, strategy)
				sctx.Fixing = fixing
				outcome, err := (&RebaseStep{}).Execute(sctx)

				if fixing {
					// The mock agent resolves nothing, so the step may still fail
					// afterwards; what matters is that the conflict reached it.
					if len(ag.calls) != 1 {
						t.Fatalf("expected the conflict to reach the agent once, got %d calls (err: %v)", len(ag.calls), err)
					}
					if !strings.Contains(ag.calls[0].Prompt, "shared.txt") {
						t.Fatalf("resolver prompt does not name shared.txt:\n%s", ag.calls[0].Prompt)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if !outcome.NeedsApproval {
					t.Fatal("expected NeedsApproval for the conflict")
				}
				if !strings.Contains(outcome.Findings, "shared.txt") {
					t.Fatalf("expected findings to name shared.txt, got: %s", outcome.Findings)
				}
			})
		}
	}
}
