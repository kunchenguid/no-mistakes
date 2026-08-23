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
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Every fixture in this file uses a synthetic home. A test fixture is published
// source, exactly like the PR bodies this guard exists to keep clean, so no
// real account may appear here.
const (
	fixtureHome         = "/home/testuser"
	fixtureEvidenceDir  = fixtureHome + "/.no-mistakes/evidence/run-1"
	fixtureWorktreePath = fixtureHome + "/.no-mistakes/worktrees/ab12cd/1/svc"
)

// homePathLeakNeedles are checked against every assembled PR body regardless of
// which shape a case exercised. A shape-specific assertion would only catch the
// shape someone already thought of.
var homePathLeakNeedles = []string{
	fixtureHome,
	"/Users/testuser",
	`C:\Users\testuser`,
	"C:/Users/testuser",
}

type homePathLeakCase struct {
	name string
	// evidenceDir overrides the run's evidence directory. Absolute artifact
	// paths are only rendered when they resolve under the worktree or the
	// evidence directory, so a case exercising an artifact path has to put the
	// evidence directory under the synthetic home the same way production puts
	// it under the operator's real one.
	evidenceDir string
	// evidenceFiles are written under the resolved evidence directory before
	// assembly, for the case where captured output is embedded from disk.
	evidenceFiles  map[string]string
	reviewFindings string
	testFindings   string
	testStepError  string
	fixSummary     string
	userIntent     string
	agentTitle     string
	agentBody      string
	// wantVisible are strings that must survive redaction, so a "fix" that
	// simply drops the leaking section cannot pass.
	wantVisible []string
}

// TestPRStep_BuildPRContentRedactsAbsoluteHomePaths feeds realistic captured
// output, artifact records, and agent-authored prose through the PR-body
// assembly and asserts that no absolute home path survives into the content
// handed to the provider.
//
// Every subtest fails against the pre-fix assembly, which had no path-aware
// processing on this path at all.
func TestPRStep_BuildPRContentRedactsAbsoluteHomePaths(t *testing.T) {
	t.Parallel()

	cases := []homePathLeakCase{
		{
			name:        "artifact path rendered as a local file reference",
			evidenceDir: fixtureEvidenceDir,
			testFindings: findingsJSON(t, types.Findings{
				TestingSummary: "Captured the failing request/response pair.",
				Artifacts: []types.TestArtifact{{
					Kind:  "log",
					Label: "pytest output",
					Path:  fixtureEvidenceDir + "/pytest.log",
				}},
			}),
			wantVisible: []string{"pytest output", "~/.no-mistakes/evidence/run-1/pytest.log"},
		},
		{
			name:        "pytest rootdir header in captured output",
			evidenceDir: fixtureEvidenceDir,
			testFindings: findingsJSON(t, types.Findings{
				TestingSummary: "Ran the targeted suite.",
				Artifacts: []types.TestArtifact{{
					Kind:    "command-output",
					Label:   "pytest",
					Content: "platform linux -- Python 3.12.3, pytest-8.2.0\nrootdir: " + fixtureWorktreePath + "\nconfigfile: pyproject.toml\n2 passed in 0.31s",
				}},
			}),
			wantVisible: []string{"rootdir: ~/.no-mistakes/worktrees/ab12cd/1/svc", "2 passed in 0.31s"},
		},
		{
			name:        "worktree path assignment inside captured output",
			evidenceDir: fixtureEvidenceDir,
			testFindings: findingsJSON(t, types.Findings{
				TestingSummary: "Replayed the generator with the recorded settings.",
				Artifacts: []types.TestArtifact{{
					Kind:    "command-output",
					Label:   "settings dump",
					Content: `WORKTREE = "` + fixtureWorktreePath + `"` + "\nDEBUG = False",
				}},
			}),
			wantVisible: []string{`WORKTREE = "~/.no-mistakes/worktrees/ab12cd/1/svc"`, "DEBUG = False"},
		},
		{
			name:        "the same path repeated many times",
			evidenceDir: fixtureEvidenceDir,
			testFindings: findingsJSON(t, types.Findings{
				TestingSummary: "Collected the full session header.",
				Artifacts: []types.TestArtifact{{
					Kind:  "command-output",
					Label: "session header",
					Content: "rootdir: " + fixtureWorktreePath + "\n" +
						"cachedir: " + fixtureWorktreePath + "/.pytest_cache\n" +
						"configfile: " + fixtureWorktreePath + "/pyproject.toml\n" +
						"inifile: " + fixtureWorktreePath + "/pytest.ini\n" +
						"basetemp: " + fixtureHome + "/tmp/pytest-of-testuser",
				}},
			}),
			wantVisible: []string{"cachedir: ~/.no-mistakes/worktrees/ab12cd/1/svc/.pytest_cache"},
		},
		{
			name: "captured output embedded from an evidence file on disk",
			evidenceFiles: map[string]string{
				"pytest.log": "============ test session starts ============\n" +
					"rootdir: " + fixtureWorktreePath + "\n" +
					"plugins: anyio-4.4.0\n" +
					"1 passed in 0.10s\n",
			},
			testFindings: findingsJSON(t, types.Findings{
				TestingSummary: "Targeted suite passes.",
				Artifacts: []types.TestArtifact{{
					Kind:  "log",
					Label: "pytest log",
					Path:  "%EVIDENCE%/pytest.log",
				}},
			}),
			wantVisible: []string{"rootdir: ~/.no-mistakes/worktrees/ab12cd/1/svc", "1 passed in 0.10s"},
		},
		{
			name:        "macOS home root",
			evidenceDir: "/Users/testuser/.no-mistakes/evidence/run-1",
			testFindings: findingsJSON(t, types.Findings{
				TestingSummary: "Captured the run header.",
				Artifacts: []types.TestArtifact{{
					Kind:    "command-output",
					Label:   "pytest",
					Content: "rootdir: /Users/testuser/.no-mistakes/worktrees/ab12cd/1/svc",
				}},
			}),
			wantVisible: []string{"rootdir: ~/.no-mistakes/worktrees/ab12cd/1/svc"},
		},
		{
			name:        "windows home root",
			evidenceDir: fixtureEvidenceDir,
			testFindings: findingsJSON(t, types.Findings{
				TestingSummary: "Captured the run header.",
				Artifacts: []types.TestArtifact{{
					Kind:    "command-output",
					Label:   "pytest",
					Content: `rootdir: C:\Users\testuser\.no-mistakes\worktrees\ab12cd\1\svc`,
				}},
			}),
			wantVisible: []string{`rootdir: ~\.no-mistakes\worktrees\ab12cd\1\svc`},
		},
		{
			name:        "testing summary prose",
			evidenceDir: fixtureEvidenceDir,
			testFindings: findingsJSON(t, types.Findings{
				TestingSummary: "Ran the suite from " + fixtureWorktreePath + " and captured the output.",
			}),
			wantVisible: []string{"Ran the suite from ~/.no-mistakes/worktrees/ab12cd/1/svc"},
		},
		{
			name:        "tested command detail",
			evidenceDir: fixtureEvidenceDir,
			testFindings: findingsJSON(t, types.Findings{
				Items:  []types.Finding{{Severity: types.FindingSeverityError, Description: "one assertion failed"}},
				Tested: []string{"python -m pytest " + fixtureWorktreePath + "/tests/test_api.py"},
			}),
			wantVisible: []string{"python -m pytest ~/.no-mistakes/worktrees/ab12cd/1/svc/tests/test_api.py"},
		},
		{
			name:        "review finding file and description",
			evidenceDir: fixtureEvidenceDir,
			reviewFindings: findingsJSON(t, types.Findings{
				Items: []types.Finding{{
					Severity:    types.FindingSeverityWarning,
					File:        fixtureWorktreePath + "/api/handler.py",
					Line:        42,
					Description: "temporary fixture written to " + fixtureHome + "/tmp/fixture.json",
				}},
				RiskLevel: "low",
			}),
			wantVisible: []string{"~/.no-mistakes/worktrees/ab12cd/1/svc/api/handler.py", "~/tmp/fixture.json"},
		},
		{
			name:        "review risk rationale",
			evidenceDir: fixtureEvidenceDir,
			reviewFindings: findingsJSON(t, types.Findings{
				RiskLevel:     "medium",
				RiskRationale: "the generated config still points at " + fixtureHome + "/.config/svc.toml",
			}),
			wantVisible: []string{"the generated config still points at ~/.config/svc.toml"},
		},
		{
			name:        "auto-fix round summary",
			evidenceDir: fixtureEvidenceDir,
			reviewFindings: findingsJSON(t, types.Findings{
				Items: []types.Finding{{Severity: types.FindingSeverityWarning, Description: "hard-coded path"}},
			}),
			fixSummary:  "replaced the hard-coded " + fixtureHome + "/data path with a config key",
			wantVisible: []string{"replaced the hard-coded ~/data path with a config key"},
		},
		{
			name:          "failed step error text",
			evidenceDir:   fixtureEvidenceDir,
			testStepError: "open " + fixtureEvidenceDir + "/pytest.log: no such file or directory",
			wantVisible:   []string{"open ~/.no-mistakes/evidence/run-1/pytest.log: no such file or directory"},
		},
		{
			name:        "agent-authored title and what-changed body",
			evidenceDir: fixtureEvidenceDir,
			agentTitle:  "fix(api): stop writing fixtures to " + fixtureHome + "/tmp",
			agentBody:   "## What Changed\n\n- fixtures now land in the run evidence directory instead of " + fixtureHome + "/tmp",
			wantVisible: []string{"fixtures now land in the run evidence directory instead of ~/tmp"},
		},
		{
			name:        "extracted user intent",
			evidenceDir: fixtureEvidenceDir,
			userIntent:  "Stop the exporter writing into " + fixtureHome + "/Downloads on every run.",
			wantVisible: []string{"Stop the exporter writing into ~/Downloads on every run."},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			content := buildHomePathLeakPRContent(t, tc)
			assertNoHomePathLeak(t, tc, content)
		})
	}
}

// TestPRStep_ExecuteRedactsAbsoluteHomePathsBeforePublishing asserts on the
// bytes actually handed to the provider CLI, not just on the assembly's return
// value, so the redaction boundary cannot be bypassed by the publish path.
func TestPRStep_ExecuteRedactsAbsoluteHomePathsBeforePublishing(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	env, logFile := fakeGH(t, "")

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			payload, err := json.Marshal(prContent{
				Title: "fix(api): keep evidence out of the worktree",
				Body:  "## What Changed\n\n- evidence now lands under " + fixtureEvidenceDir,
			})
			if err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(payload)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.EvidenceDir = fixtureEvidenceDir

	testFindings := findingsJSON(t, types.Findings{
		TestingSummary: "Captured the run header.",
		Artifacts: []types.TestArtifact{{
			Kind:    "command-output",
			Label:   "pytest",
			Content: "rootdir: " + fixtureWorktreePath,
		}},
	})
	insertCompletedStep(t, sctx, types.StepTest, testFindings, "")

	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	published := string(data)
	for _, needle := range homePathLeakNeedles {
		if strings.Contains(published, needle) {
			t.Fatalf("published PR content leaked %q:\n%s", needle, published)
		}
	}
	for _, want := range []string{"rootdir: ~/.no-mistakes/worktrees/ab12cd/1/svc", "evidence now lands under ~/.no-mistakes/evidence/run-1"} {
		if !strings.Contains(published, want) {
			t.Fatalf("expected %q in published PR content:\n%s", want, published)
		}
	}
}

// TestPRStep_BuildPRContentRedactsAfterClampingToHostLimit pins the ordering
// property redaction depends on: it runs last, and because the placeholder is
// never longer than what it replaces it can only shrink an already clamped
// body.
func TestPRStep_BuildPRContentRedactsAfterClampingToHostLimit(t *testing.T) {
	t.Parallel()
	tc := homePathLeakCase{
		evidenceDir: fixtureEvidenceDir,
		agentBody: "## What Changed\n\n- evidence now lands under " + fixtureEvidenceDir + "\n" +
			strings.Repeat("- and a long tail of change notes that overruns the host cap\n", 200),
		wantVisible: []string{"evidence now lands under ~/.no-mistakes/evidence/run-1"},
	}
	limit := scm.MaxPRBodyChars(scm.ProviderAzureDevOps)
	if limit <= 0 {
		t.Skip("provider has no PR body cap to clamp against")
	}
	content := buildHomePathLeakPRContentWithLimit(t, tc, limit)
	assertNoHomePathLeak(t, tc, content)
	if got := scm.PRBodyLen(content.Body); got > limit {
		t.Fatalf("redacted body is %d chars, over the %d cap", got, limit)
	}
}

func buildHomePathLeakPRContent(t *testing.T, tc homePathLeakCase) prContent {
	t.Helper()
	return buildHomePathLeakPRContentWithLimit(t, tc, 0)
}

func buildHomePathLeakPRContentWithLimit(t *testing.T, tc homePathLeakCase, bodyLimit int) prContent {
	t.Helper()
	dir, baseSHA, headSHA := setupGitRepo(t)

	title := tc.agentTitle
	if title == "" {
		title = "fix(api): tighten the evidence path"
	}
	body := tc.agentBody
	if body == "" {
		body = "## What Changed\n\n- tightened where evidence files are written"
	}
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			payload, err := json.Marshal(prContent{Title: title, Body: body})
			if err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(payload)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.UserIntent = tc.userIntent
	if tc.evidenceDir != "" {
		sctx.EvidenceDir = tc.evidenceDir
	}

	testFindings := tc.testFindings
	if len(tc.evidenceFiles) > 0 {
		if err := os.MkdirAll(sctx.EvidenceDir, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, contents := range tc.evidenceFiles {
			if err := os.WriteFile(filepath.Join(sctx.EvidenceDir, name), []byte(contents), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		testFindings = strings.ReplaceAll(testFindings, "%EVIDENCE%", filepath.ToSlash(sctx.EvidenceDir))
	}

	if tc.reviewFindings != "" || tc.fixSummary != "" {
		reviewFindings := tc.reviewFindings
		if reviewFindings == "" {
			reviewFindings = findingsJSON(t, types.Findings{})
		}
		step := insertCompletedStep(t, sctx, types.StepReview, reviewFindings, "")
		if tc.fixSummary != "" {
			fix := tc.fixSummary
			if _, err := sctx.DB.InsertStepRound(step.ID, 2, "auto_fix", nil, &fix, 200); err != nil {
				t.Fatal(err)
			}
		}
	}
	if testFindings != "" || tc.testStepError != "" {
		insertCompletedStep(t, sctx, types.StepTest, testFindings, tc.testStepError)
	}

	content, err := (&PRStep{}).buildPRContent(sctx, "feature", "main", baseSHA, scm.ProviderGitHub, bodyLimit)
	if err != nil {
		t.Fatal(err)
	}
	return content
}

// insertCompletedStep records one finished step plus its initial round. A
// non-empty stepError records a failed step instead, which is how a step's raw
// error text reaches the PR body.
func insertCompletedStep(t *testing.T, sctx *pipeline.StepContext, name types.StepName, findings, stepError string) *db.StepResult {
	t.Helper()
	step, err := sctx.DB.InsertStepResult(sctx.Run.ID, name)
	if err != nil {
		t.Fatal(err)
	}
	if stepError != "" {
		if err := sctx.DB.FailStep(step.ID, stepError, 100); err != nil {
			t.Fatal(err)
		}
		refreshed, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, sr := range refreshed {
			if sr.ID == step.ID {
				return sr
			}
		}
		return step
	}
	if err := sctx.DB.UpdateStepStatus(step.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	if findings != "" {
		if err := sctx.DB.SetStepFindings(step.ID, findings); err != nil {
			t.Fatal(err)
		}
		if _, err := sctx.DB.InsertStepRound(step.ID, 1, "initial", &findings, nil, 500); err != nil {
			t.Fatal(err)
		}
	} else if _, err := sctx.DB.InsertStepRound(step.ID, 1, "initial", nil, nil, 500); err != nil {
		t.Fatal(err)
	}
	return step
}

func assertNoHomePathLeak(t *testing.T, tc homePathLeakCase, content prContent) {
	t.Helper()
	for _, field := range []struct{ name, value string }{{"title", content.Title}, {"body", content.Body}} {
		for _, needle := range homePathLeakNeedles {
			if n := strings.Count(field.value, needle); n > 0 {
				t.Errorf("PR %s leaked %q %d time(s):\n%s", field.name, needle, n, field.value)
			}
		}
	}
	for _, want := range tc.wantVisible {
		if !strings.Contains(content.Body, want) {
			t.Errorf("expected %q to survive redaction in the PR body:\n%s", want, content.Body)
		}
	}
}

func findingsJSON(t *testing.T, findings types.Findings) string {
	t.Helper()
	raw, err := json.Marshal(findings)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TestTestFindingsSchema_DoesNotSolicitAbsolutePaths pins the other half of the
// fix. The renderer only ever accepts an artifact path that resolves under the
// worktree or the run's evidence directory, but the schema used to ask for
// "absolute paths for temporary local evidence files when available" - so the
// pipeline solicited the operator's home directory and then published it.
// Redacting while still asking for it leaves the tool fighting itself.
func TestTestFindingsSchema_DoesNotSolicitAbsolutePaths(t *testing.T) {
	t.Parallel()
	var schema map[string]any
	if err := json.Unmarshal(testFindingsSchema, &schema); err != nil {
		t.Fatalf("test findings schema is not valid JSON: %v", err)
	}
	description := artifactPathDescription(t, schema)
	if strings.Contains(strings.ToLower(description), "absolute") {
		t.Fatalf("artifact path schema still solicits absolute paths: %q", description)
	}
	for _, want := range []string{"repository-relative", "evidence directory"} {
		if !strings.Contains(description, want) {
			t.Fatalf("expected artifact path schema to name %q, got: %q", want, description)
		}
	}
}

func artifactPathDescription(t *testing.T, schema map[string]any) string {
	t.Helper()
	properties, _ := schema["properties"].(map[string]any)
	artifacts, _ := properties["artifacts"].(map[string]any)
	items, _ := artifacts["items"].(map[string]any)
	itemProperties, _ := items["properties"].(map[string]any)
	pathProperty, _ := itemProperties["path"].(map[string]any)
	description, ok := pathProperty["description"].(string)
	if !ok {
		t.Fatalf("artifact path property has no description: %#v", pathProperty)
	}
	return description
}
