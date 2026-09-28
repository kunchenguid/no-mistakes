package github

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestGetChecksForHeadRejectsMovementWithoutChangingOrdinaryDiscovery(t *testing.T) {
	for _, movement := range []string{"none", "before", "during", "unreadable"} {
		t.Run(movement, func(t *testing.T) {
			const view = "gh pr view 123 --repo test/repo --json headRefOid --jq .headRefOid"
			responses := map[string]githubTestResponse{
				view: {stdout: "head\n"},
				githubCommitChecksCommand("", "test/repo", "head"):                                                     {stdout: githubCommitChecksResponse(`[{"__typename":"CheckRun","name":"build","status":"COMPLETED","conclusion":"SUCCESS"}]`)},
				"gh api --method GET repos/test/repo/actions/runs -f head_sha=head -f per_page=100 --paginate --slurp": {stdout: `[{"total_count":0,"workflow_runs":[]}]`},
			}
			reads := 0
			factory := githubTestCmdFactory(responses)
			host := New(func(ctx context.Context, name string, args ...string) *exec.Cmd {
				if name+" "+strings.Join(args, " ") == view {
					reads++
					if reads == 2 && movement == "during" {
						responses[view] = githubTestResponse{stdout: "moved\n"}
					}
					if reads == 2 && movement == "unreadable" {
						responses[view] = githubTestResponse{code: 1}
					}
				}
				return factory(ctx, name, args...)
			}, nil, "", "test/repo")
			expected := "head"
			if movement == "before" {
				expected = "earlier-head"
			}
			pr := &scm.PR{Number: "123", HeadSHA: expected}
			checks, err := host.GetChecksForHead(context.Background(), pr, expected)
			if movement == "none" {
				if err != nil || len(checks) != 1 {
					t.Fatalf("checks=%+v err=%v", checks, err)
				}
			} else if err == nil {
				t.Fatalf("moved/unreadable head accepted: %+v", checks)
			}
			if pr.HeadSHA != expected {
				t.Fatal("recheck mutated caller's head binding")
			}
		})
	}
}
