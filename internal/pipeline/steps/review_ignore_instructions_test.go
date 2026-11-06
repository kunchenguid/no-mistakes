package steps

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
)

func TestReviewStep_IgnorePatternsCannotWaiveTrustedInstructions(t *testing.T) {
	t.Parallel()
	const rule = "Inspect each changed fixture."
	const trustedRule = "review:\n  path_instructions:\n    - path: '*.txt'\n      instructions: '" + rule + "'\n"
	for _, tc := range []struct {
		name         string
		pushed       string
		trusted      string
		wantPaths    []string
		wantRule     string
		refuse       bool
		empty        bool
		omitCoverage bool
	}{
		{
			name:   "pushed_wildcard",
			pushed: "ignore_patterns: ['*']\n", trusted: trustedRule,
			wantPaths: []string{"feature.txt"}, wantRule: rule,
		},
		{
			name:   "pushed_wildcard_refusing_reviewer",
			pushed: "ignore_patterns: ['*']\n", trusted: trustedRule,
			wantPaths: []string{"feature.txt"}, wantRule: rule, refuse: true,
		},
		{
			name:   "required_path_needs_coverage",
			pushed: "ignore_patterns: ['*']\n", trusted: trustedRule,
			wantPaths: []string{"feature.txt"}, wantRule: rule, omitCoverage: true,
		},
		{
			name:   "partially_ignored",
			pushed: "ignore_patterns: ['*.txt']\n", trusted: trustedRule,
			wantPaths: []string{".no-mistakes.yaml", "feature.txt"}, wantRule: rule,
		},
		{
			name:   "no_ignore_control",
			pushed: "ignore_patterns: []\n", trusted: trustedRule,
			wantPaths: []string{".no-mistakes.yaml", "feature.txt"}, wantRule: rule,
		},
		{
			name:      "configuration_itself_is_claimed",
			pushed:    "ignore_patterns: ['*']\n",
			trusted:   "review:\n  path_instructions:\n    - path: '.no-mistakes.yaml'\n      instructions: 'Inspect configuration changes.'\n",
			wantPaths: []string{".no-mistakes.yaml"}, wantRule: "Inspect configuration changes.",
		},
		{
			name:   "trusted_ignore_still_skips",
			pushed: "ignore_patterns: ['*']\n", trusted: "ignore_patterns: ['*.txt']\n" + trustedRule,
		},
		{
			name:   "unrelated_trusted_ignore_cannot_waive_rule",
			pushed: "ignore_patterns: ['*']\n", trusted: "ignore_patterns: ['vendor/**']\n" + trustedRule,
			wantPaths: []string{"feature.txt"}, wantRule: rule,
		},
		{
			name:    "unmatched_rule_still_skips",
			pushed:  "ignore_patterns: ['*']\n",
			trusted: "review:\n  path_instructions:\n    - path: '*.go'\n      instructions: 'Inspect source changes.'\n",
		},
		{
			name:   "no_rules_still_skips",
			pushed: "ignore_patterns: ['*']\n",
		},
		{
			name:    "truly_empty_change",
			trusted: trustedRule, empty: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := setupGitRepo(t)
			if tc.empty {
				gitCmd(t, dir, "reset", "--hard", baseSHA)
				headSHA = baseSHA
			} else {
				if err := os.WriteFile(filepath.Join(dir, ".no-mistakes.yaml"), []byte(tc.pushed), 0o600); err != nil {
					t.Fatal(err)
				}
				gitCmd(t, dir, "add", "-A")
				gitCmd(t, dir, "commit", "-m", "add submitted configuration")
				headSHA = gitCmd(t, dir, "rev-parse", "HEAD")
			}
			pushed, err := config.LoadRepoFromBytes([]byte(tc.pushed))
			if err != nil {
				t.Fatal(err)
			}
			trusted, err := config.LoadRepoFromBytes([]byte(tc.trusted))
			if err != nil {
				t.Fatal(err)
			}
			reviewerErr := errors.New("synthetic reviewer refusal")
			ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
				if !strings.Contains(opts.Prompt, tc.wantRule) {
					t.Errorf("review prompt omitted trusted instruction %q", tc.wantRule)
				}
				if tc.refuse {
					return nil, reviewerErr
				}
				findings := cleanReviewFindings()
				findings.ReviewedPaths = tc.wantPaths
				if tc.omitCoverage {
					findings.ReviewedPaths = []string{}
				}
				output, err := json.Marshal(findings)
				return &agent.Result{Output: output}, err
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
			sctx.Config = config.Merge(config.DefaultGlobalConfig(), config.EffectiveRepoConfig(pushed, trusted, false))
			outcome, err := (&ReviewStep{}).Execute(sctx)
			wantCalls := 0
			if len(tc.wantPaths) > 0 {
				wantCalls = 1
			}
			if tc.omitCoverage {
				wantCalls = 2 // Initial review and bounded coverage completion.
			}
			if len(ag.calls) != wantCalls {
				t.Fatalf("reviewer_calls=%d, want %d; outcome=%+v err=%v", len(ag.calls), wantCalls, outcome, err)
			}
			if tc.refuse {
				if !errors.Is(err, reviewerErr) {
					t.Fatalf("error=%v, want reviewer refusal", err)
				}
				if outcome != nil && outcome.ReviewApprovedHeadSHA != "" {
					t.Fatalf("refusing reviewer approved head: %+v", outcome)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(outcome.ReviewablePaths, tc.wantPaths) {
				t.Errorf("reviewable paths=%v, want %v", outcome.ReviewablePaths, tc.wantPaths)
			}
			if outcome.NeedsApproval != tc.omitCoverage {
				t.Errorf("NeedsApproval=%v, want %v", outcome.NeedsApproval, tc.omitCoverage)
			}
			if !tc.omitCoverage && outcome.ReviewApprovedHeadSHA != headSHA {
				t.Errorf("clean review or authorized skip did not approve current head: %+v", outcome)
			}
		})
	}
}
