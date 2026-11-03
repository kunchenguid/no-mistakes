package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// runReviewFixRound drives one Review fix round whose fixer returns fixOutput,
// optionally writing a file first, and returns the fixer's RunOpts and the
// step log.
func runReviewFixRound(t *testing.T, fixOutput string, edit bool) (agent.RunOpts, []string) {
	t.Helper()
	if !edit {
		return runReviewFixRoundWith(t, fixOutput, nil)
	}
	return runReviewFixRoundWith(t, fixOutput, func(dir string) error {
		return os.WriteFile(filepath.Join(dir, "review-fix.txt"), []byte("fixed"), 0o644)
	})
}

// runReviewFixRoundWith is runReviewFixRound with the fixer's worktree action
// supplied by the caller, so a fixer that runs `git commit` itself can be
// driven too. A nil action makes no edits.
func runReviewFixRoundWith(t *testing.T, fixOutput string, fixerAction func(dir string) error) (agent.RunOpts, []string) {
	t.Helper()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	var fixOpts agent.RunOpts
	calls := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			calls++
			if calls == 1 {
				fixOpts = opts
				if fixerAction != nil {
					if err := fixerAction(dir); err != nil {
						return nil, err
					}
				}
				return &agent.Result{Output: json.RawMessage(fixOutput)}, nil
			}
			findings := Findings{Items: []Finding{}, Summary: "all clear", RiskLevel: "low", RiskRationale: "all clear", RiskScope: types.FindingsRiskScopeSourceOrExternal, ReviewedPaths: fullReviewCoverage(t, dir, baseSHA)}
			j, _ := json.Marshal(findings)
			return &agent.Result{Output: j}, nil
		},
	}

	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	var mu sync.Mutex
	var logs []string
	sctx.Log = func(line string) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, line)
	}
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"id":"review-1","severity":"warning","file":"main.go","description":"possible nil deref"}],"summary":"1 issue"}`

	if _, err := (&ReviewStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	return fixOpts, append([]string(nil), logs...)
}

func logsContain(logs []string, want string) bool {
	for _, line := range logs {
		if strings.Contains(line, want) {
			return true
		}
	}
	return false
}

// A missing verification tool must not read as permission to skip the edits:
// the Review fix prompt says so, and its result schema carries the field that
// reports the blocked verification.
func TestReviewFixPromptKeepsEditsWhenVerificationToolIsMissing(t *testing.T) {
	t.Parallel()
	opts, _ := runReviewFixRound(t, `{"summary":"address findings"}`, true)

	normalized := strings.Join(strings.Fields(opts.Prompt), " ")
	for _, want := range []string{
		"If the tool needed for that verification is missing, still apply every fix you intended and skip the unavailable verification.",
		"Never make fewer edits, or none, because verification is unavailable.",
		`Add a "verification_blocked" field`,
	} {
		if !strings.Contains(normalized, want) {
			t.Errorf("fix prompt missing %q:\n%s", want, opts.Prompt)
		}
	}
	if !strings.Contains(string(opts.JSONSchema), "verification_blocked") {
		t.Errorf("fix schema does not offer verification_blocked: %s", opts.JSONSchema)
	}
	var schema struct {
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(opts.JSONSchema, &schema); err != nil {
		t.Fatal(err)
	}
	if len(schema.Required) != 1 || schema.Required[0] != "summary" {
		t.Errorf("verification_blocked must stay optional, required = %v", schema.Required)
	}
}

func TestReviewFixLogsUnverifiedCommit(t *testing.T) {
	t.Parallel()
	_, logs := runReviewFixRound(t, `{"summary":"address findings","verification_blocked":"pytest not found: tests need a project virtualenv"}`, true)
	if !logsContain(logs, "fixes committed unverified, verification blocked: pytest not found: tests need a project virtualenv") {
		t.Errorf("committed round did not log blocked verification: %q", logs)
	}
	if logsContain(logs, "made no edits") {
		t.Errorf("committed round logged as a no-edit round: %q", logs)
	}
}

func TestReviewFixLogsNoEditRoundThatCitesMissingTool(t *testing.T) {
	t.Parallel()
	_, logs := runReviewFixRound(t, `{"summary":"no changes","verification_blocked":"pytest missing"}`, false)
	if !logsContain(logs, "agent made no edits and reported verification blocked: pytest missing; the findings were not fixed") {
		t.Errorf("no-edit round did not log the missing tool distinctly: %q", logs)
	}
	if !logsContain(logs, "no agent changes to commit") {
		t.Errorf("ordinary no-change line should still be logged: %q", logs)
	}
}

// commitInWorktree makes a fixer-authored commit, as a fixer that runs
// `git commit` itself does, leaving the worktree clean.
func commitInWorktree(t *testing.T) func(dir string) error {
	return func(dir string) error {
		if err := os.WriteFile(filepath.Join(dir, "review-fix.txt"), []byte("fixed"), 0o644); err != nil {
			return err
		}
		gitCmd(t, dir, "add", "review-fix.txt")
		gitCmd(t, dir, "commit", "-m", "fixer: address findings")
		return nil
	}
}

// A fixer that commits its own work leaves nothing for the pipeline to commit,
// but the round did produce a fix commit: the log must not claim that the agent
// made no edits.
func TestReviewFixLogsUnverifiedCommitWhenFixerCommittedItself(t *testing.T) {
	t.Parallel()
	_, logs := runReviewFixRoundWith(t, `{"summary":"address findings","verification_blocked":"pytest missing"}`, commitInWorktree(t))
	if !logsContain(logs, "no agent changes to commit") {
		t.Fatalf("test did not exercise the clean-worktree path: %q", logs)
	}
	if !logsContain(logs, "fixes committed unverified, verification blocked: pytest missing") {
		t.Errorf("fixer-committed round did not log blocked verification as unverified: %q", logs)
	}
	if logsContain(logs, "made no edits") || logsContain(logs, "the findings were not fixed") {
		t.Errorf("fixer-committed round logged as a no-edit round: %q", logs)
	}
}

func TestReviewFixLogsNothingExtraWhenFixerCommittedItselfAndVerificationRan(t *testing.T) {
	t.Parallel()
	_, logs := runReviewFixRoundWith(t, `{"summary":"address findings"}`, commitInWorktree(t))
	if logsContain(logs, "verification blocked") {
		t.Errorf("unexpected blocked-verification log: %q", logs)
	}
}

func TestReviewFixLogsNothingExtraWhenVerificationRan(t *testing.T) {
	t.Parallel()
	for name, edit := range map[string]bool{"committed": true, "no edits": false} {
		_, logs := runReviewFixRound(t, `{"summary":"address findings"}`, edit)
		if logsContain(logs, "verification blocked") {
			t.Errorf("%s: unexpected blocked-verification log: %q", name, logs)
		}
	}
}

func TestBlockedVerificationBoundsAndNormalizes(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("é", maxVerificationBlockedBytes)
	out, err := json.Marshal(map[string]string{"summary": "s", "verification_blocked": "  go\n  missing\t" + long})
	if err != nil {
		t.Fatal(err)
	}
	got := blockedVerification(&agent.Result{Output: out})
	if !strings.HasPrefix(got, "go missing ") {
		t.Errorf("whitespace not collapsed: %q", got)
	}
	if len(got) > maxVerificationBlockedBytes {
		t.Errorf("result %d bytes exceeds the %d byte bound", len(got), maxVerificationBlockedBytes)
	}
	if !strings.HasSuffix(got, "é") {
		t.Errorf("truncation split a rune: %q", got)
	}
	for _, result := range []*agent.Result{nil, {}, {Output: json.RawMessage(`not json`)}, {Output: json.RawMessage(`{"summary":"s"}`)}} {
		if got := blockedVerification(result); got != "" {
			t.Errorf("blockedVerification(%+v) = %q, want empty", result, got)
		}
	}
}
