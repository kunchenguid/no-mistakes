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
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// These tests run the public Test step against committed Git trees. Mock agent
// evidence supplies the semantic assessment; the gate verifies Git and syntax.
func TestTestStep_DocumentationOnlyLiveValidation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, path, before, after string
		extraPath, extraContent   string
		eligible                  bool
		semanticExample           bool
	}{
		{name: "readme prose", path: "README.md", before: "# Guide\n\nA clear explanation.\n", after: "# Guide\n\nA clearer explanation.\n", eligible: true},
		{name: "new prose guide", path: "docs/guide.md", after: "# Guide\n\nThe [overview](overview.md) provides context.\n", eligible: true},
		{name: "deleted prose guide", path: "documentation/old.txt", before: "An obsolete explanation.\n", eligible: true},
		{name: "nested prose", path: "documentation/usage/guide.md", before: "The old prose.\n", after: "The new prose.\n", eligible: true},
		{name: "ordinary imperative prose", path: "docs/guide.md", after: "Read the guide.\n", eligible: true},
		{name: "mixed code", path: "README.md", after: "New prose.\n", extraPath: "main.go", extraContent: "package main\n"},
		{name: "mixed configuration", path: "README.md", after: "New prose.\n", extraPath: ".no-mistakes.yaml", extraContent: "test:\n  prepare: true\n"},
		{name: "workflow", path: ".github/workflows/check.yml", after: "name: check\n"},
		{name: "schema", path: "docs/schema.json", after: "{}\n"},
		{name: "api schema", path: "documentation/openapi.yaml", after: "openapi: 3.1.0\n"},
		{name: "frontend", path: "docs/page.tsx", after: "export default () => null\n"},
		{name: "script", path: "docs/check.sh", after: "#!/bin/sh\necho check\n"},
		{name: "fenced runnable example", path: "docs/guide.md", after: "# Guide\n\n```sh\nnpm test\n```\n"},
		{name: "existing runnable example", path: "README.md", before: "Old prose.\n\n```sh\nnpm test\n```\n", after: "New prose.\n\n```sh\nnpm test\n```\n"},
		{name: "removed runnable example", path: "README.md", before: "Old prose.\n\n```sh\nnpm test\n```\n", after: "New prose.\n"},
		{name: "tilde fence", path: "docs/guide.md", after: "~~~sh\nnpm test\n~~~\n"},
		{name: "inline runnable example", path: "docs/guide.md", after: "Run `npm test` now.\n"},
		{name: "prose runnable example", path: "docs/guide.md", after: "Run npm test.\n", semanticExample: true},
		{name: "unchanged prose runnable example", path: "docs/guide.md", before: "Old prose.\n\nRun npm test.\n", after: "New prose.\n\nRun npm test.\n", semanticExample: true},
		{name: "removed prose runnable example", path: "docs/guide.md", before: "Old prose.\n\nRun npm test.\n", after: "New prose.\n", semanticExample: true},
		{name: "added unlisted runnable example", path: "docs/guide.md", after: "Run cargo test.\n", semanticExample: true},
		{name: "unchanged unlisted runnable example", path: "docs/guide.md", before: "Old prose.\n\nRun cargo test.\n", after: "New prose.\n\nRun cargo test.\n", semanticExample: true},
		{name: "removed unlisted runnable example", path: "docs/guide.md", before: "Old prose.\n\nRun cargo test.\n", after: "New prose.\n", semanticExample: true},
		{name: "added other runnable example", path: "docs/guide.md", after: "Run pwd.\n", semanticExample: true},
		{name: "declarative runnable example", path: "docs/guide.md", after: "The command is cargo test.\n", semanticExample: true},
		{name: "added imperative example", path: "docs/guide.md", after: "Execute touch marker.\n", semanticExample: true},
		{name: "list imperative example", path: "docs/guide.md", after: "- Run cargo test.\n"},
		{name: "heading imperative example", path: "docs/guide.md", after: "# Run cargo test\n", semanticExample: true},
		{name: "bare carriage return before command", path: "docs/guide.md", after: "Read the guide.\rRun npm test.\n"},
		{name: "carriage return line ending", path: "docs/guide.md", after: "The guide explains.\r\n", eligible: true},
		{name: "indented runnable example", path: "docs/guide.md", after: "Example:\n\n    npm test\n"},
		{name: "tab example", path: "docs/guide.md", after: "Example:\n\n\tnpm test\n"},
		{name: "html", path: "docs/guide.md", after: "<script>alert('x')</script>\n"},
		{name: "mdx", path: "docs/guide.mdx", after: "# Welcome\n"},
		{name: "frontmatter", path: "docs/guide.md", after: "---\nredirect: /new\n---\nProse.\n"},
		{name: "template", path: "docs/guide.md", after: "Welcome {{ user.name }}.\n"},
		{name: "active link", path: "docs/guide.md", after: "[click](javascript:alert(1))\n"},
		{name: "agent policy", path: "AGENTS.md", after: "Use a different test command.\n"},
		{name: "nested agent policy", path: "docs/AGENTS.md", after: "Use a different test command.\n"},
		{name: "skill policy", path: ".agents/skills/check/SKILL.md", after: "Change validation policy.\n"},
		{name: "unknown markdown purpose", path: "prompts/task.md", after: "Change the product response.\n"},
		{name: "unfenced command", path: "docs/guide.md", after: "npm test\n", semanticExample: true},
		{name: "unfenced configuration", path: "docs/guide.md", after: "enabled: true\n"},
		{name: "reference directive", path: "docs/guide.md", after: ".. include:: /etc/passwd\n"},
		{name: "added plus prose list", path: "docs/guide.md", before: "", after: "# Overview\n\n+ Item\n"},
		{name: "unchanged plus prose list", path: "docs/guide.md", before: "Old prose.\n\n+ Item\n", after: "New prose.\n\n+ Item\n"},
		{name: "removed plus prose list", path: "docs/guide.md", before: "Old prose.\n\n+ Item\n", after: "New prose.\n"},
		{name: "added ordered prose list", path: "docs/guide.md", before: "", after: "# Overview\n\n1. Item\n"},
		{name: "unchanged ordered prose list", path: "docs/guide.md", before: "Old prose.\n\n1. Item\n", after: "New prose.\n\n1. Item\n"},
		{name: "removed ordered prose list", path: "docs/guide.md", before: "Old prose.\n\n1. Item\n", after: "New prose.\n"},
		{name: "added parenthesized ordered prose list", path: "docs/guide.md", before: "", after: "# Overview\n\n1) Item\n"},
		{name: "unchanged parenthesized ordered prose list", path: "docs/guide.md", before: "Old prose.\n\n1) Item\n", after: "New prose.\n\n1) Item\n"},
		{name: "removed parenthesized ordered prose list", path: "docs/guide.md", before: "Old prose.\n\n1) Item\n", after: "New prose.\n"},
		{name: "prose list", path: "docs/guide.md", after: "# Overview\n\n- The overview provides context.\n- The next section explains the design.\n"},
		{name: "active heading link", path: "docs/guide.md", after: "# [Click](javascript:alert(1))\n"},
		{name: "image has a visual surface", path: "docs/guide.md", after: "![Screen](screen.png)\n"},
		{name: "shell example in txt", path: "docs/guide.txt", after: "#!/bin/sh\necho example\n"},
		{name: "example path even with prose", path: "docs/examples/guide.md", after: "A changed example.\n"},
		{name: "unknown binary purpose", path: "assets/readme.md", after: "A changed description.\n"},
		{name: "binary markdown", path: "docs/guide.md", after: "text\x00data\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, base, _ := setupGitRepo(t)
			gitCmd(t, dir, "reset", "--hard", base)
			write := func(path, content string) {
				if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, path)), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, path), []byte(content), 0644); err != nil {
					t.Fatal(err)
				}
			}
			if tc.before != "" {
				write(tc.path, tc.before)
			}
			gitCmd(t, dir, "add", "-A")
			gitCmd(t, dir, "commit", "--allow-empty", "-m", "documentation baseline")
			base = gitCmd(t, dir, "rev-parse", "HEAD")
			gitCmd(t, dir, "branch", "-f", "main", base)
			if tc.after != "" {
				write(tc.path, tc.after)
			} else if err := os.Remove(filepath.Join(dir, tc.path)); err != nil {
				t.Fatal(err)
			}
			if tc.extraPath != "" {
				write(tc.extraPath, tc.extraContent)
			}
			gitCmd(t, dir, "add", "-A")
			gitCmd(t, dir, "commit", "-m", "change under validation")
			head := gitCmd(t, dir, "rev-parse", "HEAD")
			var payload map[string]any
			if err := json.Unmarshal([]byte(docsNoSurfaceEvidence), &payload); err != nil {
				t.Fatal(err)
			}
			paths := []string{tc.path}
			if tc.extraPath != "" {
				paths = append(paths, tc.extraPath)
			}
			payload["documentation_applicability"] = map[string]any{"docs_only": true, "no_executable_examples": !tc.semanticExample, "no_live_surface": true, "reason": "Inspected both versions and found no live behavior", "base_sha": base, "head_sha": head, "reviewed_paths": paths}
			output, _ := json.Marshal(payload)
			ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				return &agent.Result{Output: output}, nil
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
			sctx.Config.IgnorePatterns = []string{"*.go", ".no-mistakes.yaml"}
			outcome, err := (&TestStep{}).Execute(sctx)
			if err != nil {
				t.Fatal(err)
			}
			if outcome.NeedsApproval == tc.eligible {
				t.Fatalf("NeedsApproval=%v, eligible=%v: %s", outcome.NeedsApproval, tc.eligible, outcome.Findings)
			}
			t.Logf("TestStep result: path=%s base=%s head=%s approval=%t", tc.path, base, head, outcome.NeedsApproval)
			findings, err := types.ParseFindingsJSON(outcome.Findings)
			if err != nil {
				t.Fatal(err)
			}
			if findings.Verdict != types.TestVerdictNoSurface || !types.NoLiveExercisableScenarios(findings.Scenarios) {
				t.Fatalf("lost truthful no-surface evidence: %+v", findings)
			}
			if tc.eligible {
				if !strings.Contains(findings.TestingSummary, "not applicable") || !strings.Contains(findings.TestingSummary, head) || !strings.Contains(findings.TestingSummary, base) {
					t.Fatalf("missing diff-bound N/A evidence: %+v", findings)
				}
				steps, rounds := testStepWithFindings(t, outcome.Findings)
				if rendered := BuildTestingSummary(steps, rounds); !strings.Contains(rendered, "not applicable") {
					t.Fatalf("PR omitted N/A evidence: %s", rendered)
				} else if tc.name == "readme prose" {
					t.Logf("PR testing summary:\n%s", rendered)
				}
			}
			if len(ag.calls) != 1 {
				t.Fatalf("evidence turn was skipped or replayed: %d", len(ag.calls))
			}
		})
	}
}

const docsNoSurfaceEvidence = `{"findings":[],"summary":"Documentation inspected","tested":["reviewed the documentation diff"],"testing_summary":"Documentation checks completed; no running product surface","artifacts":[],"scenarios":[{"name":"Live documentation behavior","result":"untested","live":false,"evidence":"","reason":"Prose-only documentation has no runtime product surface"}],"verdict":"no-surface"}`

func TestTestStep_DocumentationExceptionPreservesOtherValidation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, command, verdict, evidenceMode       string
		finding, dirty, executable, symlink, moved bool
		wantExit                                   int
	}{
		{name: "configured check passes", command: "git config docs-only.baseline ran", verdict: "no-surface"},
		{name: "configured check fails", command: "git definitely-not-a-command", verdict: "no-surface", wantExit: 1},
		{name: "unrelated ask-user", verdict: "no-surface", finding: true},
		{name: "inconclusive still parks", verdict: "inconclusive"},
		{name: "no-go still parks", verdict: "no-go"},
		{name: "uncommitted agent code", verdict: "no-surface", dirty: true},
		{name: "executable markdown", verdict: "no-surface", executable: true},
		{name: "symbolic link markdown", verdict: "no-surface", symlink: true},
		{name: "agent moved head", verdict: "no-surface", moved: true},
		{name: "missing applicability", verdict: "no-surface", evidenceMode: "missing"},
		{name: "uncertain assessment", verdict: "no-surface", evidenceMode: "uncertain"},
		{name: "empty reason", verdict: "no-surface", evidenceMode: "empty-reason"},
		{name: "stale base", verdict: "no-surface", evidenceMode: "stale-base"},
		{name: "stale head", verdict: "no-surface", evidenceMode: "stale-head"},
		{name: "missing coverage", verdict: "no-surface", evidenceMode: "missing-coverage"},
		{name: "duplicate coverage", verdict: "no-surface", evidenceMode: "duplicate-coverage"},
		{name: "foreign coverage", verdict: "no-surface", evidenceMode: "foreign-coverage"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, base, _ := setupGitRepo(t)
			gitCmd(t, dir, "reset", "--hard", base)
			if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("A clearer explanation.\n"), 0644); err != nil {
				t.Fatal(err)
			}
			if tc.symlink {
				if err := os.Remove(filepath.Join(dir, "README.md")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("base.txt", filepath.Join(dir, "README.md")); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			}
			gitCmd(t, dir, "add", "README.md")
			if tc.executable {
				gitCmd(t, dir, "update-index", "--chmod=+x", "README.md")
			}
			gitCmd(t, dir, "commit", "-m", "update prose")
			head := gitCmd(t, dir, "rev-parse", "HEAD")
			var payload map[string]any
			if err := json.Unmarshal([]byte(docsNoSurfaceEvidence), &payload); err != nil {
				t.Fatal(err)
			}
			payload["verdict"] = tc.verdict
			applicability := map[string]any{"docs_only": true, "no_executable_examples": true, "no_live_surface": true, "reason": "Inspected both versions", "base_sha": base, "head_sha": head, "reviewed_paths": []string{"README.md"}}
			switch tc.evidenceMode {
			case "uncertain":
				applicability["no_executable_examples"] = false
			case "empty-reason":
				applicability["reason"] = " "
			case "stale-base":
				applicability["base_sha"] = strings.Repeat("0", 40)
			case "stale-head":
				applicability["head_sha"] = strings.Repeat("0", 40)
			case "missing-coverage":
				applicability["reviewed_paths"] = []string{}
			case "duplicate-coverage":
				applicability["reviewed_paths"] = []string{"README.md", "README.md"}
			case "foreign-coverage":
				applicability["reviewed_paths"] = []string{"other.md"}
			}
			if tc.evidenceMode != "missing" {
				payload["documentation_applicability"] = applicability
			}
			if tc.finding {
				payload["findings"] = []any{map[string]string{"severity": "warning", "action": "ask-user", "description": "Documentation link target cannot be verified"}}
			}
			output, _ := json.Marshal(payload)
			ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				if tc.dirty || tc.moved {
					if err := os.WriteFile(filepath.Join(dir, "new.go"), []byte("package changed\n"), 0644); err != nil {
						t.Fatal(err)
					}
				}
				if tc.moved {
					gitCmd(t, dir, "add", "new.go")
					gitCmd(t, dir, "commit", "-m", "agent changed head")
				}
				return &agent.Result{Output: output}, nil
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{Test: tc.command})
			outcome, err := (&TestStep{}).Execute(sctx)
			if err != nil {
				t.Fatal(err)
			}
			wantApproval := tc.wantExit != 0 || tc.finding || tc.verdict != "no-surface" || tc.dirty || tc.executable || tc.symlink || tc.moved || tc.evidenceMode != ""
			if outcome.NeedsApproval != wantApproval {
				t.Fatalf("NeedsApproval=%v, want %v: %s", outcome.NeedsApproval, wantApproval, outcome.Findings)
			}
			t.Logf("TestStep result: approval=%t exit=%d verdict=%s", outcome.NeedsApproval, outcome.ExitCode, tc.verdict)
			if (outcome.ExitCode != 0) != (tc.wantExit != 0) {
				t.Fatalf("ExitCode=%d", outcome.ExitCode)
			}
			findings, err := types.ParseFindingsJSON(outcome.Findings)
			if err != nil {
				t.Fatal(err)
			}
			if tc.command != "" && (len(findings.Tested) == 0 || findings.Tested[0] != tc.command) {
				t.Fatalf("configured command lost: %+v", findings)
			}
			if tc.name == "configured check passes" && gitCmd(t, dir, "config", "--get", "docs-only.baseline") != "ran" {
				t.Fatal("configured test command did not execute")
			}
			if tc.finding && !strings.Contains(outcome.Findings, "Documentation link target cannot be verified") {
				t.Fatal("unrelated finding lost")
			}
		})
	}
}
