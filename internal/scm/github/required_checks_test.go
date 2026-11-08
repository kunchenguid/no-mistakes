package github

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

const (
	requiredChecksBranchCommand = "gh api --hostname github.com --method GET repos/test/repo/branches/release/v1"
	requiredChecksRulesCommand  = "gh api --hostname github.com --method GET repos/test/repo/rules/branches/release/v1 --paginate --slurp"
	unprotectedBranchResponse   = `{"name":"release/v1","protected":false,"protection":{"enabled":false,"required_status_checks":{"enforcement_level":"off","contexts":[],"checks":[]}}}`
)

func TestRequiredChecks(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		branch  githubTestResponse
		rules   githubTestResponse
		want    []string
		wantErr string
	}{
		{
			name:   "nothing required",
			branch: githubTestResponse{stdout: unprotectedBranchResponse},
			rules:  githubTestResponse{stdout: `[[]]`},
			want:   []string{},
		},
		{
			name:   "private repository plan without rulesets",
			branch: githubTestResponse{stdout: unprotectedBranchResponse},
			rules:  githubTestResponse{stderr: "gh: Upgrade to GitHub Pro or make this repository public to enable this feature. (HTTP 403)", code: 1},
			want:   []string{},
		},
		{
			name:   "classic protection contexts and checks",
			branch: githubTestResponse{stdout: `{"name":"release/v1","protection":{"enabled":true,"required_status_checks":{"enforcement_level":"everyone","contexts":["ci/circleci"],"checks":[{"context":"build","app_id":15368}]}}}`},
			rules:  githubTestResponse{stdout: `[[]]`},
			want:   []string{"build", "ci/circleci"},
		},
		{
			name:   "protection with enforcement off requires nothing",
			branch: githubTestResponse{stdout: `{"name":"release/v1","protection":{"enabled":true,"required_status_checks":{"enforcement_level":"off","contexts":["stale"],"checks":[]}}}`},
			rules:  githubTestResponse{stdout: `[[]]`},
			want:   []string{},
		},
		{
			name:   "ruleset checks, required workflows, and code scanning across pages",
			branch: githubTestResponse{stdout: unprotectedBranchResponse},
			rules:  githubTestResponse{stdout: `[[{"type":"deletion"},{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"lint"}]}}],[{"type":"workflows","parameters":{}},{"type":"code_scanning","parameters":{}}]]`},
			want:   []string{"lint", "ruleset rule code_scanning", "ruleset rule workflows"},
		},
		{
			name:    "unreadable branch fails closed",
			branch:  githubTestResponse{stderr: "gh: Branch not found (HTTP 404)", code: 1},
			rules:   githubTestResponse{stdout: `[[]]`},
			wantErr: "read branch protection",
		},
		{
			name:    "branch response without a branch fails closed",
			branch:  githubTestResponse{stdout: `{}`},
			rules:   githubTestResponse{stdout: `[[]]`},
			wantErr: "names no branch",
		},
		{
			name:    "any other rules refusal fails closed",
			branch:  githubTestResponse{stdout: unprotectedBranchResponse},
			rules:   githubTestResponse{stderr: "gh: API rate limit exceeded (HTTP 403)", code: 1},
			wantErr: "read rulesets",
		},
		{
			name:    "malformed rules fail closed",
			branch:  githubTestResponse{stdout: unprotectedBranchResponse},
			rules:   githubTestResponse{stdout: `{"message":"oops"}`},
			wantErr: "parse rulesets",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			host := New(githubTestCmdFactory(map[string]githubTestResponse{
				requiredChecksBranchCommand: tc.branch,
				requiredChecksRulesCommand:  tc.rules,
			}), nil, "github.com", "test/repo")
			got, err := host.RequiredChecks(context.Background(), "release/v1")
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("RequiredChecks error = %v, want it to mention %q (got %v)", err, tc.wantErr, got)
				}
				if got != nil {
					t.Fatalf("a failed read must not return a requirement list, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("RequiredChecks: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("RequiredChecks = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestRequiredChecksRefusesAnUnknownRepository(t *testing.T) {
	t.Parallel()
	host := New(githubTestCmdFactory(nil), nil, "github.com", "")
	if _, err := host.RequiredChecks(context.Background(), "main"); err == nil {
		t.Fatal("expected an error without a repository slug, so gh never resolves a cwd-inferred repository")
	}
}
