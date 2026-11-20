package github

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// A successful CLI may write diagnostics on stderr; only stdout is JSON.
func TestGetChecksSeparatesDiagnosticsFromJSON(t *testing.T) {
	for _, tc := range []struct {
		name          string
		commitWarning string
		runsWarning   string
		commitCode    int
		runsCode      int
		wantErr       string
	}{
		{name: "clean"},
		{name: "commit_warning", commitWarning: "mise WARN synthetic tool warning\n"},
		{name: "workflow_warning", runsWarning: "mise WARN synthetic tool warning\n"},
		{name: "commit_failure", commitWarning: "synthetic check request failed\n", commitCode: 1, wantErr: "synthetic check request failed"},
		{name: "workflow_failure", runsWarning: "synthetic workflow request failed\n", runsCode: 1, wantErr: "synthetic workflow request failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			responses := map[string]githubTestResponse{
				"gh pr view 123 --repo test/repo --json headRefOid --jq .headRefOid": {stdout: "deadbeef\n"},
				githubCommitChecksCommand("", "test/repo", "deadbeef"): {
					stdout: githubCommitChecksResponse(`[{"__typename":"CheckRun","name":"build","status":"COMPLETED","conclusion":"SUCCESS"}]`),
					stderr: tc.commitWarning,
					code:   tc.commitCode,
				},
				"gh api --method GET repos/test/repo/actions/runs -f head_sha=deadbeef -f per_page=100 --paginate --slurp": {
					stdout: `[{"total_count":0,"workflow_runs":[]}]`,
					stderr: tc.runsWarning,
					code:   tc.runsCode,
				},
			}
			factory := githubTestCmdFactory(responses)
			host := New(func(ctx context.Context, name string, args ...string) *exec.Cmd {
				cmd := factory(ctx, name, args...)
				var fixtureEnv []string
				for _, value := range cmd.Env {
					if strings.HasPrefix(value, "GITHUB_TEST_") {
						fixtureEnv = append(fixtureEnv, value)
					}
				}
				cmd.Env = fixtureEnv
				return cmd
			}, nil, "", "test/repo")
			checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "123", HeadSHA: "deadbeef"})
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want diagnostic %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("successful CLI with valid stdout JSON must be readable: %v", err)
			}
			if len(checks) != 1 || checks[0].Name != "build" || checks[0].Bucket != scm.CheckBucketPass {
				t.Fatalf("checks = %+v, want one build/pass check", checks)
			}
		})
	}
}
