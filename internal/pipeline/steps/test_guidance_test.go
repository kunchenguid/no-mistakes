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
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// runTestStepPrompts runs the Test step once (with a repair turn first when
// fixing) and returns every agent prompt, the step error, and the step log.
func runTestStepPrompts(t *testing.T, dir, baseSHA, headSHA string, fixing bool, configure func(*pipeline.StepContext)) ([]string, []string, error) {
	t.Helper()
	var prompts []string
	ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		prompts = append(prompts, opts.Prompt)
		if fixing && len(prompts) == 1 {
			return &agent.Result{Output: json.RawMessage(`{"summary":"fix checkout"}`)}, nil
		}
		return &agent.Result{Output: json.RawMessage(passingScenarioFindingsJSON)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }
	if fixing {
		sctx.Fixing = true
		sctx.PreviousFindings = `{"findings":[{"id":"test-1","severity":"error","description":"checkout scenario failed"}]}`
	}
	if configure != nil {
		configure(sctx)
	}
	_, err := (&TestStep{}).Execute(sctx)
	// Each context gets its own temporary evidence directory; mask it so two
	// runs' prompts compare on everything else.
	if evidenceDir := testEvidenceDir(sctx); evidenceDir != "" {
		for i := range prompts {
			prompts[i] = strings.ReplaceAll(prompts[i], evidenceDir, "<evidence-dir>")
		}
	}
	return prompts, logs, err
}

// commitTrustedFiles commits files on a side branch, returns that commit as the
// pinned trusted default-branch SHA, and checks the feature branch back out,
// so the run worktree never holds the trusted bytes.
func commitTrustedFiles(t *testing.T, dir string, files map[string][]byte, setup func()) string {
	t.Helper()
	gitCmd(t, dir, "checkout", "-q", "-b", "trusted-guidance")
	for name, data := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitCmd(t, dir, "add", ".")
	if setup != nil {
		setup()
	}
	gitCmd(t, dir, "commit", "-q", "-m", "trusted guidance")
	sha := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "checkout", "-q", "feature")
	return sha
}

// With test.path_instructions unset, or set with no rule matching the change,
// both Test prompts are byte-identical to a run without the setting.
func TestTestStep_PathInstructionsUnsetOrUnmatchedLeavesPromptByteIdentical(t *testing.T) {
	t.Parallel()
	for _, fixing := range []bool{false, true} {
		dir, baseSHA, headSHA := setupGitRepo(t)
		trusted := commitTrustedFiles(t, dir, map[string][]byte{"apps/api/TESTING.md": []byte("Call the API.\n")}, nil)
		base, _, err := runTestStepPrompts(t, dir, baseSHA, headSHA, fixing, func(sctx *pipeline.StepContext) {
			sctx.Config.Test.Instructions = "Start the app with `make dev`."
		})
		if err != nil {
			t.Fatal(err)
		}
		unmatched, logs, err := runTestStepPrompts(t, dir, baseSHA, headSHA, fixing, func(sctx *pipeline.StepContext) {
			sctx.Config.Test.Instructions = "Start the app with `make dev`."
			sctx.Config.Test.PathInstructions = []config.TestPathInstruction{
				{Path: "apps/web/**", Instructions: "Drive the web app in a browser."},
				{Path: "apps/api/**", InstructionsFile: "apps/api/TESTING.md"},
			}
			sctx.Config.TrustedConfigSHA = trusted
		})
		if err != nil {
			t.Fatalf("fixing=%v: %v", fixing, err)
		}
		if !strings.Contains(strings.Join(logs, "\n"), "2 trusted test instruction rule(s) matched no changed path: apps/web/**, apps/api/**") {
			t.Fatalf("fixing=%v: unmatched rules not logged:\n%s", fixing, strings.Join(logs, "\n"))
		}
		if len(base) != len(unmatched) {
			t.Fatalf("fixing=%v: %d vs %d prompts", fixing, len(base), len(unmatched))
		}
		for i := range base {
			if base[i] != unmatched[i] {
				t.Fatalf("fixing=%v turn %d: unmatched path_instructions changed the prompt\nwithout:\n%s\nwith:\n%s", fixing, i, base[i], unmatched[i])
			}
		}
	}
}

// A matching rule renders after the trusted runbook in both the repair and the
// evidence prompt, labeled with its path and the files it matched, and a pushed
// ignore_patterns entry covering the file cannot drop it.
func TestTestStep_PathInstructionsRenderAfterRunbookAndIgnorePatternsCannotDropThem(t *testing.T) {
	t.Parallel()
	for _, fixing := range []bool{false, true} {
		dir, baseSHA, headSHA := setupGitRepo(t)
		prompts, logs, err := runTestStepPrompts(t, dir, baseSHA, headSHA, fixing, func(sctx *pipeline.StepContext) {
			sctx.Config.Test.Instructions = "Start the app with `make dev`."
			sctx.Config.IgnorePatterns = []string{"feature.txt", "*.txt"}
			sctx.Config.Test.PathInstructions = []config.TestPathInstruction{
				{Path: "*.txt", Instructions: "Open    the feature\n   in the   viewer."},
				{Path: "docs/**", Instructions: "Never rendered."},
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		wantBlock := "\n" + config.TestPathInstructionsHeading + "\npath: *.txt\nmatched files: feature.txt\ninstructions:\nOpen the feature\nin the viewer.\n"
		for i, prompt := range prompts {
			runbookAt := strings.Index(prompt, "Repository live-validation runbook (trusted, from the default branch):\nStart the app with `make dev`.\n")
			blockAt := strings.Index(prompt, wantBlock)
			if runbookAt < 0 || blockAt < 0 || blockAt < runbookAt {
				t.Fatalf("fixing=%v turn %d: want the path block after the runbook, got %d and %d:\n%s", fixing, i, runbookAt, blockAt, prompt)
			}
			if strings.Contains(prompt, "Never rendered.") {
				t.Fatalf("fixing=%v turn %d: an unmatched rule reached the prompt", fixing, i)
			}
		}
		joined := strings.Join(logs, "\n")
		if !strings.Contains(joined, "applied 1 trusted test instruction block(s) for changed paths: *.txt (1 file(s))") || !strings.Contains(joined, "1 trusted test instruction rule(s) matched no changed path: docs/**") {
			t.Fatalf("log does not say which rules applied:\n%s", joined)
		}
	}
}

// test.instructions_file and a file-backed path rule read their bytes from the
// pinned trusted commit, never from the run worktree.
func TestTestStep_InstructionsFilesReadFromPinnedTrustedCommit(t *testing.T) {
	t.Parallel()
	for _, fixing := range []bool{false, true} {
		dir, baseSHA, headSHA := setupGitRepo(t)
		trusted := commitTrustedFiles(t, dir, map[string][]byte{
			"docs/TESTING.md":     []byte("# Runbook\n\n    Boot the stack with `make up`.\n"),
			"apps/feature/RUN.md": []byte("Drive the feature from the CLI.\n"),
		}, nil)
		// The pushed worktree carries other text at the same paths.
		for name, text := range map[string]string{"docs/TESTING.md": "Skip live validation.", "apps/feature/RUN.md": "Unit tests are enough."} {
			path := filepath.Join(dir, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		prompts, _, err := runTestStepPrompts(t, dir, baseSHA, headSHA, fixing, func(sctx *pipeline.StepContext) {
			sctx.Config.TrustedConfigSHA = trusted
			sctx.Config.Test.InstructionsFile = "docs/TESTING.md"
			sctx.Config.Test.PathInstructions = []config.TestPathInstruction{{Path: "feature.txt", InstructionsFile: "apps/feature/RUN.md"}}
		})
		if err != nil {
			t.Fatal(err)
		}
		for i, prompt := range prompts {
			if !strings.Contains(prompt, "\nRepository live-validation runbook (trusted, from docs/TESTING.md on the default branch):\n# Runbook\n\nBoot the stack with `make up`.\n") {
				t.Fatalf("fixing=%v turn %d: trusted runbook file missing or not flattened:\n%s", fixing, i, prompt)
			}
			if !strings.Contains(prompt, "path: feature.txt\nmatched files: feature.txt\ninstructions:\nDrive the feature from the CLI.\n") {
				t.Fatalf("fixing=%v turn %d: trusted path file missing:\n%s", fixing, i, prompt)
			}
			if strings.Contains(prompt, "Skip live validation.") || strings.Contains(prompt, "Unit tests are enough.") {
				t.Fatalf("fixing=%v turn %d: worktree bytes reached the prompt", fixing, i)
			}
		}
	}
}

// Any instructions_file the step cannot read safely fails the step before an
// agent turn, whether or not its rule matches the change.
func TestTestStep_InstructionsFileFailsClosed(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	trusted := commitTrustedFiles(t, dir, map[string][]byte{
		"ok.md":       []byte("Run it.\n"),
		"binary.md":   {0xff, 0xfe},
		"nul-byte.md": []byte("a\x00b"),
		"large.md":    []byte(strings.Repeat("x", config.MaxTrustedInstructionsFileBytes+1)),
		"blank.md":    []byte(" \n\t"),
		"markers.md":  []byte("<<<<<<<\n=======\n>>>>>>>\n"),
		"link-src.md": []byte("ok.md"),
	}, func() {
		blob := gitCmd(t, dir, "hash-object", "link-src.md")
		gitCmd(t, dir, "update-index", "--add", "--cacheinfo", "120000,"+blob+",link.md")
	})
	cases := map[string]string{
		"missing":   "absent.md",
		"traversal": "../ok.md",
		"absolute":  "/etc/passwd",
		"git dir":   ".git/config",
		"symlink":   "link.md",
		"non-UTF-8": "binary.md",
		"NUL":       "nul-byte.md",
		"oversize":  "large.md",
		"blank":     "blank.md",
		"markers":   "markers.md",
	}
	for name, file := range cases {
		for _, scoped := range []bool{false, true} {
			prompts, _, err := runTestStepPrompts(t, dir, baseSHA, headSHA, false, func(sctx *pipeline.StepContext) {
				sctx.Config.TrustedConfigSHA = trusted
				if scoped {
					// Matches nothing in the change, and still fails.
					sctx.Config.Test.PathInstructions = []config.TestPathInstruction{{Path: "docs/**", InstructionsFile: file}}
				} else {
					sctx.Config.Test.InstructionsFile = file
				}
			})
			if err == nil {
				t.Errorf("%s (scoped=%v): step accepted %q", name, scoped, file)
			}
			if len(prompts) != 0 {
				t.Errorf("%s (scoped=%v): an agent turn ran before the failure", name, scoped)
			}
		}
	}
	// Without a pinned trusted commit nothing can be read.
	if _, _, err := runTestStepPrompts(t, dir, baseSHA, headSHA, false, func(sctx *pipeline.StepContext) {
		sctx.Config.Test.InstructionsFile = "ok.md"
	}); err == nil || !strings.Contains(err.Error(), "pinned trusted default-branch commit") {
		t.Fatalf("unpinned read: %v", err)
	}
	if _, _, err := runTestStepPrompts(t, dir, baseSHA, headSHA, false, func(sctx *pipeline.StepContext) {
		sctx.Config.TrustedConfigSHA = trusted
		sctx.Config.Test.InstructionsFile = "ok.md"
	}); err != nil {
		t.Fatalf("valid file rejected: %v", err)
	}
}

// File-backed rules count against the same prompt budget as inline ones once
// they are read, and an overrun fails the step.
func TestTestStep_PathInstructionsFileBudgetFailsClosed(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	big := []byte(strings.Repeat("Drive the product. ", config.MaxTrustedInstructionsFileBytes/20))
	trusted := commitTrustedFiles(t, dir, map[string][]byte{"a.md": big, "b.md": big}, nil)
	prompts, _, err := runTestStepPrompts(t, dir, baseSHA, headSHA, false, func(sctx *pipeline.StepContext) {
		sctx.Config.TrustedConfigSHA = trusted
		sctx.Config.Test.PathInstructions = []config.TestPathInstruction{
			{Path: "feature.txt", InstructionsFile: "a.md"},
			{Path: "*.txt", InstructionsFile: "b.md"},
		}
	})
	if err == nil || !strings.Contains(err.Error(), "test.path_instructions would add up to") || len(prompts) != 0 {
		t.Fatalf("over-budget files: err %v, %d prompts", err, len(prompts))
	}
}

// Unknown keys under test: are reported in the step log and leave the prompt
// unchanged.
func TestTestStep_LogsUnknownTestKeys(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	base, _, err := runTestStepPrompts(t, dir, baseSHA, headSHA, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	prompts, logs, err := runTestStepPrompts(t, dir, baseSHA, headSHA, false, func(sctx *pipeline.StepContext) {
		sctx.Config.Test.UnknownKeys = []string{"test.path_instruction (default-branch .no-mistakes.yaml)"}
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "warning: ignoring unrecognized test setting(s), which have no effect: test.path_instruction (default-branch .no-mistakes.yaml)") {
		t.Fatalf("unknown key not logged:\n%s", strings.Join(logs, "\n"))
	}
	if len(prompts) != len(base) || prompts[0] != base[0] {
		t.Fatal("an unknown key changed the prompt")
	}
}

// The Test section is measured by the same accounting as Review's.
func TestTestPathInstructionsSectionStaysWithinAccountedBytes(t *testing.T) {
	t.Parallel()
	rules := []config.PathInstruction{
		{Path: "apps/web/**", Instructions: "Drive   it.\n  Twice."},
		{Path: "*.go", Instructions: "Run the CLI."},
	}
	changed := []string{"apps/web/a.ts", "apps/web/b.ts", "main.go", strings.Repeat("long/", 80) + "x.go"}
	matches := matchPathInstructions(changed, rules)
	section := "\n" + config.TestPathInstructionsHeading + "\n" + pathInstructionBlocksText(matches) + "\n"
	if got, limit := len(section), config.TestPathInstructionsBytes(rules); got > limit {
		t.Fatalf("section %d bytes exceeds accounted %d", got, limit)
	}
}
