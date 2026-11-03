//go:build e2e

package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestTestBaseAttributionJourney drives test.base_attribution through the
// real binary, daemon, gate push, and Test step: a failing configured
// commands.test is re-run on the run's base commit in a disposable checkout
// and the Test finding's summary separates introduced from pre-existing
// failures, while the finding itself and the park stay unchanged.
func TestTestBaseAttributionJourney(t *testing.T) {
	type journey struct {
		name string
		// trusted is appended to the default-branch .no-mistakes.yaml.
		trusted string
		// testScript and prepareScript are committed on main, so the base
		// and the head run the same script; their behavior keys on files
		// the pushed branch adds.
		testScript    string
		prepareScript string
		branchFiles   map[string]string
		// wantPark is false when the configured command passes on the head.
		wantPark    bool
		wantBaseRun bool
		want        []string
		wantAbsent  []string
		// overrides is the operator's machine-local repository override
		// block; localCheck is written as its additional test check.
		overrides  string
		localCheck string
		// wantAttributionAbsent must not appear in the attribution section
		// itself (the summary's text before the command output).
		wantAttributionAbsent []string
		// The daemon inherits suite niceness, so wantNice is an adjustment
		// rather than an absolute priority.
		wantNice string
	}
	goFailures := `echo '--- FAIL: TestOld (0.01s)'
echo '--- FAIL: TestParse (0.00s)'
printf 'FAIL\texample.com/pkg/a\t0.10s\n'
if [ -f regression ]; then
  echo '    --- FAIL: TestNew (0.02s)'
  echo '--- FAIL: TestParse (0.00s)'
  printf 'FAIL\texample.com/pkg/b\t0.20s\n'
fi
exit 1
`
	for _, tc := range []journey{
		{
			name:        "separates_introduced_from_preexisting_by_package",
			trusted:     "test:\n  base_attribution: true\n",
			testScript:  goFailures,
			branchFiles: map[string]string{"regression": "x\n"},
			wantPark:    true, wantBaseRun: true,
			want: []string{
				"Base attribution: commands.test also fails on base commit",
				"Introduced by this change (2):\n- example.com/pkg/b: --- FAIL: TestNew\n- example.com/pkg/b: --- FAIL: TestParse",
				"Pre-existing on the base commit (2):\n- example.com/pkg/a: --- FAIL: TestOld\n- example.com/pkg/a: --- FAIL: TestParse",
			},
			wantAbsent: []string{"Ambiguous"},
		},
		{
			name:    "unqualified_duplicate_name_is_ambiguous",
			trusted: "test:\n  base_attribution: true\n",
			testScript: `echo '  ✕ renders the header (12 ms)'
echo '  ✕ renders the header (3 ms)'
if [ -f regression ]; then echo '  ✕ brand new case (4 ms)'; fi
exit 1
`,
			branchFiles: map[string]string{"regression": "x\n"},
			wantPark:    true, wantBaseRun: true,
			want: []string{
				"Introduced by this change (1):\n- ✕ brand new case",
				"Ambiguous, could not attribute (1):\n- ✕ renders the header",
			},
			wantAbsent: []string{"Pre-existing on the base commit"},
		},
		{
			name:    "unrecognized_base_failure_cannot_be_separated",
			trusted: "test:\n  base_attribution: true\n",
			testScript: `if [ -f regression ]; then echo '--- FAIL: TestNew (0.01s)'; printf 'FAIL\texample.com/pkg/b\t0.1s\n'; exit 1; fi
printf 'FAIL\texample.com/pkg/a [build failed]\n'
exit 2
`,
			branchFiles: map[string]string{"regression": "x\n"},
			wantPark:    true, wantBaseRun: true,
			want: []string{
				"also fails on base commit",
				"(exit code 2)",
				"could not be separated",
			},
			wantAbsent: []string{"Introduced by this change", "Pre-existing on the base commit"},
		},
		{
			name:    "green_base_blames_the_change",
			trusted: "test:\n  base_attribution: true\n",
			testScript: `if [ -f regression ]; then echo 'not ok 3 - parses input # time=4ms'; exit 1; fi
echo 'ok 1 - parses input'
exit 0
`,
			branchFiles: map[string]string{"regression": "x\n"},
			wantPark:    true, wantBaseRun: true,
			want: []string{
				"commands.test passes on base commit",
				"so this change introduced the failure.\nIntroduced by this change (1):\n- not ok - parses input",
			},
		},
		{
			name:          "base_prepare_failure_degrades_to_unavailable",
			trusted:       "test:\n  base_attribution: true\n",
			testScript:    goFailures,
			prepareScript: "test -f prepare-ok || { echo 'base prepare refused'; exit 3; }\n",
			branchFiles:   map[string]string{"regression": "x\n", "prepare-ok": "x\n"},
			wantPark:      true, wantBaseRun: true,
			want: []string{
				"Base attribution unavailable (prepare command exited with code 3 on the base checkout); every failure is attributed to this change.",
			},
			wantAbsent: []string{"Introduced by this change", "Pre-existing"},
		},
		{
			name:    "package_without_test_line_is_unattributed",
			trusted: "test:\n  base_attribution: true\n",
			testScript: `echo '--- FAIL: TestFoo (0.01s)'
printf 'FAIL\texample.com/pkg/a\t0.10s\n'
if [ -f regression ]; then printf 'FAIL\texample.com/pkg/b [build failed]\n'; fi
exit 1
`,
			branchFiles: map[string]string{"regression": "x\n"},
			wantPark:    true, wantBaseRun: true,
			want: []string{
				"Base attribution: commands.test also fails on base commit",
				"Pre-existing on the base commit (1):\n- example.com/pkg/a: --- FAIL: TestFoo",
				"Failures without a per-test line (could not be attributed) (1):\n- FAIL example.com/pkg/b [build failed]",
			},
			wantAbsent: []string{"Introduced by this change"},
		},
		{
			name:          "local_check_output_is_not_attributed_and_base_honors_nice",
			trusted:       "test:\n  base_attribution: true\n",
			testScript:    goFailures,
			prepareScript: "exit 0\n",
			overrides:     "    commands:\n      test:\n        nice: 7\n        additional:\n          - LOCALCHECK\n      prepare:\n        nice: 7\n",
			localCheck: `echo '--- FAIL: TestLocalOnly (0.01s)'
printf 'FAIL\texample.com/pkg/local\t0.10s\n'
exit 1
`,
			branchFiles: map[string]string{"regression": "x\n"},
			wantPark:    true, wantBaseRun: true,
			want: []string{
				"Introduced by this change (2):\n- example.com/pkg/b: --- FAIL: TestNew\n- example.com/pkg/b: --- FAIL: TestParse",
				"Pre-existing on the base commit (2):\n- example.com/pkg/a: --- FAIL: TestOld\n- example.com/pkg/a: --- FAIL: TestParse",
			},
			wantAttributionAbsent: []string{"TestLocalOnly", "pkg/local"},
			wantNice:              "7",
		},
		{
			name:        "pushed_branch_cannot_opt_in",
			trusted:     "",
			testScript:  goFailures,
			branchFiles: map[string]string{"regression": "x\n", ".no-mistakes.yaml": "ignore_patterns:\n  - '*.generated.go'\n  - 'vendor/**'\nallow_repo_commands: true\ncommands:\n  test: sh scripts/test.sh\ntest:\n  base_attribution: true\n"},
			wantPark:    true, wantBaseRun: false,
			wantAbsent: []string{"Base attribution"},
		},
		{
			name:    "passing_command_never_runs_base",
			trusted: "test:\n  base_attribution: true\n",
			testScript: `echo 'ok 1 - all good'
exit 0
`,
			branchFiles: map[string]string{"feature.txt": "x\n"},
			wantPark:    false, wantBaseRun: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expectedNice := tc.wantNice
			if expectedNice != "" {
				priority, err := exec.Command("nice", "-n", expectedNice, "sh", "-c", "ps -o nice= -p $$").Output()
				if err != nil {
					t.Fatalf("measure adjusted niceness: %v", err)
				}
				expectedNice = strings.TrimSpace(string(priority))
			}
			h := NewHarness(t, SetupOpts{Agent: "claude"})
			cwdLog := filepath.Join(t.TempDir(), "cwd.log")
			record := "echo \"$0 $(pwd) nice=$(ps -o nice= -p $$ | tr -d ' ')\" >> '" + cwdLog + "'\n"
			config := "ignore_patterns:\n  - '*.generated.go'\n  - 'vendor/**'\nallow_repo_commands: true\ncommands:\n  test: sh scripts/test.sh\n"
			if tc.prepareScript != "" {
				config += "  prepare: sh scripts/prepare.sh\n"
				h.CommitChange("main", "scripts/prepare.sh", "#!/bin/sh\n"+record+tc.prepareScript, "add prepare script")
			}
			h.CommitChange("main", "scripts/test.sh", "#!/bin/sh\n"+record+tc.testScript, "add test script")
			h.CommitChange("main", ".no-mistakes.yaml", config+tc.trusted, "configure test command")
			if out, err := h.runGit(context.Background(), h.WorkDir, "push", "origin", "main"); err != nil {
				t.Fatalf("push trusted config: %v\n%s", err, out)
			}
			if tc.overrides != "" {
				localCheck := filepath.Join(t.TempDir(), "local-check.sh")
				writeMachineLocalScript(t, localCheck, tc.localCheck)
				setupMachineLocalCommands(t, h, strings.ReplaceAll(tc.overrides, "LOCALCHECK", "sh "+localCheck))
			} else if out, err := h.Run("init"); err != nil {
				t.Fatalf("init: %v\n%s", err, out)
			}
			branch := "attr-" + strings.ReplaceAll(tc.name, "_", "-")
			for path, content := range tc.branchFiles {
				h.CommitChange(branch, path, content, "change "+path)
			}
			h.PushToGate(branch)

			status := types.StepStatusCompleted
			if tc.wantPark {
				status = types.StepStatusAwaitingApproval
			}
			run := waitForStepStatus(t, h, branch, types.StepTest, status, 120*time.Second)
			step, _ := findStep(run.Steps, types.StepTest)
			logData := readStepLog(t, h, run.ID, string(types.StepTest))
			cwds, _ := os.ReadFile(cwdLog)

			var summary string
			if tc.wantPark {
				if step.FindingsJSON == nil {
					t.Fatal("parked Test step has no findings")
				}
				findings, err := types.ParseFindingsJSON(*step.FindingsJSON)
				if err != nil {
					t.Fatalf("parse findings: %v", err)
				}
				summary = findings.Summary
				var commandFinding *types.Finding
				for i := range findings.Items {
					if findings.Items[i].Category == types.FindingCategoryTestCommand && !strings.HasPrefix(findings.Items[i].Description, "machine-local ") {
						commandFinding = &findings.Items[i]
					}
				}
				if commandFinding == nil {
					t.Fatalf("no test-command finding in %s", *step.FindingsJSON)
				}
				if commandFinding.Severity != "error" || !strings.HasPrefix(commandFinding.Description, "configured test command failed with exit code ") || strings.Contains(commandFinding.Description, "attribution") {
					t.Fatalf("test-command finding changed: severity=%q description=%q", commandFinding.Severity, commandFinding.Description)
				}
			}
			for _, want := range tc.want {
				if !strings.Contains(summary, want) {
					t.Errorf("findings summary missing %q:\n%s", want, summary)
				}
			}
			for _, absent := range tc.wantAbsent {
				if strings.Contains(summary, absent) {
					t.Errorf("findings summary unexpectedly contains %q:\n%s", absent, summary)
				}
			}
			attribution, _, _ := strings.Cut(summary, "\n\n")
			for _, absent := range tc.wantAttributionAbsent {
				if strings.Contains(attribution, absent) {
					t.Errorf("attribution section classified machine-local output %q:\n%s", absent, attribution)
				}
			}
			if got := strings.Contains(logData, "re-running it on base commit"); got != (tc.wantBaseRun || tc.prepareScript != "") {
				t.Errorf("step log base re-run present = %v, want %v:\n%s", got, tc.wantBaseRun, logData)
			}
			if tc.want != nil && tc.wantPark && !anyPromptContains(h, tc.want[0]) {
				t.Errorf("no agent prompt carried the attribution %q", tc.want[0])
			}
			// Every base-side command ran in a disposable checkout outside
			// the run worktree, which is removed afterwards.
			for _, line := range strings.Split(strings.TrimSpace(string(cwds)), "\n") {
				fields := strings.Fields(line)
				if expectedNice != "" && len(fields) == 3 && fields[2] != "nice="+expectedNice {
					t.Errorf("command ran without the operator's niceness adjustment %s (expected %s): %s", tc.wantNice, expectedNice, line)
				}
				if !strings.Contains(line, "no-mistakes-test-base-") {
					continue
				}
				dir := fields[1]
				if strings.HasPrefix(dir, h.NMHome) || strings.HasPrefix(dir, h.WorkDir) {
					t.Errorf("base command ran inside the run's state: %s", dir)
				}
				if _, err := os.Stat(dir); !os.IsNotExist(err) {
					t.Errorf("base checkout %s was not removed (stat err=%v)", dir, err)
				}
			}
			if evidence := os.Getenv("NM_BASE_ATTRIBUTION_EVIDENCE"); evidence != "" {
				transcript := "# " + tc.name + "\n\n## Test step status: " + string(step.Status) + "\n\n## Commands ran in (script cwd log)\n" + string(cwds) +
					"\n## Findings summary\n" + summary + "\n\n## Test step log\n" + logData
				if step.FindingsJSON != nil {
					transcript += "\n## Findings JSON\n" + *step.FindingsJSON + "\n"
				}
				_ = os.WriteFile(filepath.Join(evidence, tc.name+".md"), []byte(transcript), 0o644)
			}
		})
	}
}
