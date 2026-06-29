package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestPRStep_TicketPrefixEndToEnd drives the real PRStep against a fake gh and
// records the actual `gh pr create` invocation so a reviewer can see the
// work-item id leading the PR title end to end. The same agent-generated
// conventional title is run twice: once on a WEB-12345 branch with
// ticket_prefix_pattern set, and once on a branch with no match to show the
// conventional-commit behavior is unchanged.
func TestPRStep_TicketPrefixEndToEnd(t *testing.T) {
	evidenceDir := os.Getenv("NM_EVIDENCE_DIR")

	cases := []struct {
		name           string
		branch         string
		pattern        string
		agentTitle     string
		wantTitleArg   string
		unwantTitleArg string
	}{
		{
			name:         "ticket branch prefixes title with work-item id",
			branch:       "refs/heads/WEB-12345-improve-pipeline-header",
			pattern:      `WEB-\d+`,
			agentTitle:   "fix: improve pipeline header UX",
			wantTitleArg: "--title WEB-12345: improve pipeline header UX --body",
		},
		{
			name:           "no match falls back to conventional commit title",
			branch:         "refs/heads/improve-pipeline-header",
			pattern:        `WEB-\d+`,
			agentTitle:     "fix: improve pipeline header UX",
			wantTitleArg:   "--title fix: improve pipeline header UX --body",
			unwantTitleArg: "WEB-",
		},
	}

	var transcript strings.Builder
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, baseSHA, headSHA := setupGitRepo(t)
			env, logFile := fakeGH(t, "")

			agentTitle := tc.agentTitle
			ag := &mockAgent{
				name: "test",
				runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
					payload := json.RawMessage(`{"title":"` + agentTitle + `","body":"## Summary\n\n- keep branch status readable"}`)
					return &agent.Result{Output: payload}, nil
				},
			}

			sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
			sctx.Env = env
			sctx.Run.Branch = tc.branch
			sctx.Config.TicketPrefixPattern = tc.pattern

			reviewStep, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepReview)
			if err != nil {
				t.Fatal(err)
			}
			if err := sctx.DB.UpdateStepStatus(reviewStep.ID, types.StepStatusCompleted); err != nil {
				t.Fatal(err)
			}
			if err := sctx.DB.SetStepFindings(reviewStep.ID, `{"findings":[],"summary":"clean","risk_level":"low","risk_rationale":""}`); err != nil {
				t.Fatal(err)
			}

			step := &PRStep{}
			if _, err := step.Execute(sctx); err != nil {
				t.Fatal(err)
			}

			logData, err := os.ReadFile(logFile)
			if err != nil {
				t.Fatal(err)
			}
			ghLog := string(logData)
			if !strings.Contains(ghLog, tc.wantTitleArg) {
				t.Fatalf("expected gh pr create title %q, got:\n%s", tc.wantTitleArg, ghLog)
			}
			if tc.unwantTitleArg != "" && strings.Contains(ghLog, tc.unwantTitleArg) {
				t.Fatalf("did not expect %q in gh call, got:\n%s", tc.unwantTitleArg, ghLog)
			}

			for _, line := range strings.Split(strings.TrimSpace(ghLog), "\n") {
				if strings.Contains(line, "pr create") {
					transcript.WriteString("branch: " + tc.branch + "\n")
					transcript.WriteString("ticket_prefix_pattern: " + tc.pattern + "\n")
					transcript.WriteString("agent title: " + tc.agentTitle + "\n")
					transcript.WriteString("$ gh " + line + "\n\n")
				}
			}
		})
	}

	if evidenceDir != "" {
		path := filepath.Join(evidenceDir, "pr-create-transcript.txt")
		if err := os.WriteFile(path, []byte(transcript.String()), 0o644); err != nil {
			t.Fatalf("write evidence: %v", err)
		}
		t.Logf("wrote PR-create transcript to %s", path)
	}
}

// TestTicketPrefix_AuthoredCommitSubjectsEndToEnd makes real git commits using
// the production commit-subject builders and captures `git log` so a reviewer
// can see the work-item id leading the commit subjects no-mistakes authors
// (step fix commits, push fix commits, CI fix commits), and that branches with
// no ticket match keep the conventional "no-mistakes(...)" subjects.
func TestTicketPrefix_AuthoredCommitSubjectsEndToEnd(t *testing.T) {
	evidenceDir := os.Getenv("NM_EVIDENCE_DIR")

	cases := []struct {
		name    string
		branch  string
		pattern string
	}{
		{"ticket branch", "refs/heads/WEB-12345-tidy-retry", `WEB-\d+`},
		{"no match", "refs/heads/tidy-retry", `WEB-\d+`},
	}

	var transcript strings.Builder
	ctx := context.Background()
	for _, tc := range cases {
		dir := t.TempDir()
		for _, args := range [][]string{
			{"init", "-b", "main"},
			{"config", "user.email", "evidence@test"},
			{"config", "user.name", "evidence"},
		} {
			if _, err := git.Run(ctx, dir, args...); err != nil {
				t.Fatalf("git %v: %v", args, err)
			}
		}

		sctx := stepContextForBranch(tc.branch, tc.pattern)
		subjects := []string{
			deterministicFixCommitMessage(sctx, types.StepReview, "tidy retry helper"),
			fixedFixCommitMessage(sctx, "apply agent fixes"),
			fixedFixCommitMessage(sctx, "apply CI fixes"),
		}

		transcript.WriteString("branch: " + tc.branch + "\n")
		transcript.WriteString("ticket_prefix_pattern: " + tc.pattern + "\n")
		for _, subject := range subjects {
			if _, err := git.Run(ctx, dir, "commit", "--allow-empty", "-m", subject); err != nil {
				t.Fatalf("git commit %q: %v", subject, err)
			}
		}
		out, err := git.Run(ctx, dir, "log", "--format=%s")
		if err != nil {
			t.Fatalf("git log: %v", err)
		}
		transcript.WriteString("$ git log --format=%s\n" + strings.TrimSpace(out) + "\n\n")
	}

	if evidenceDir != "" {
		path := filepath.Join(evidenceDir, "commit-subjects-git-log.txt")
		if err := os.WriteFile(path, []byte(transcript.String()), 0o644); err != nil {
			t.Fatalf("write evidence: %v", err)
		}
		t.Logf("wrote commit-subject git log to %s", path)
	}
}
