package steps

import (
	"context"
	"encoding/json"

	"os"
	"path/filepath"
	"runtime"
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
	fixtureWorktreePath = fixtureHome + "/.no-mistakes/worktrees/ab12cd/1/svc"
	fixtureWindowsHome  = `C:\Users\testuser`
)

// fixtureEvidenceDir is a synthetic evidence directory in the RUNNING
// platform's spelling, unlike the fixtures above, which are literal strings
// that only ever have to survive redaction.
//
// A case that exercises the artifact `path` field needs the real allowlist to
// accept the path: sanitizeArtifactPath requires filepath.IsAbs and
// filepath.Clean(p) == p for this platform, and a POSIX root satisfies neither
// on Windows. With a POSIX fixture the artifact is dropped before rendering, so
// the case stops testing redaction and quietly asserts nothing - which is
// exactly how these passed on Linux and macOS while failing on windows-git.
var fixtureEvidenceDir = filepath.Join(fixtureNativeHome(), ".no-mistakes", "evidence", "run-1")

func fixtureNativeHome() string {
	if runtime.GOOS == "windows" {
		return fixtureWindowsHome
	}
	return fixtureHome
}

// fixtureRedactedPath is what fixtureEvidenceDir renders as once the home
// prefix is replaced, in the running platform's separators.
func fixtureRedactedPath(elem ...string) string {
	return "~" + string(filepath.Separator) + filepath.Join(elem...)
}

// assertFixturePathIsRenderable fails loudly when a fixture path could not
// survive the allowlist, instead of letting the case pass vacuously.
func assertFixturePathIsRenderable(t *testing.T, p string) {
	t.Helper()
	if !filepath.IsAbs(p) {
		t.Fatalf("fixture path %q is not absolute on %s; the artifact allowlist would drop it", p, runtime.GOOS)
	}
	if filepath.Clean(p) != p {
		t.Fatalf("fixture path %q is not already clean on %s (want %q); the artifact allowlist would drop it", p, runtime.GOOS, filepath.Clean(p))
	}
}

// homePathLeakNeedles are checked against every assembled PR body regardless of
// which shape a case exercised. A shape-specific assertion would only catch the
// shape someone already thought of.
var homePathLeakNeedles = []string{
	fixtureHome,
	"/Users/testuser",
	fixtureWindowsHome,
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
					Path:  filepath.Join(fixtureEvidenceDir, "pytest.log"),
				}},
			}),
			wantVisible: []string{"pytest output", fixtureRedactedPath(".no-mistakes", "evidence", "run-1", "pytest.log")},
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
					Path:  "%EVIDENCEFILE%",
				}},
			}),
			wantVisible: []string{"rootdir: ~/.no-mistakes/worktrees/ab12cd/1/svc", "1 passed in 0.10s"},
		},
		{
			name:        "macOS home root",
			evidenceDir: fixtureEvidenceDir,
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
			testStepError: "open " + filepath.Join(fixtureEvidenceDir, "pytest.log") + ": no such file or directory",
			wantVisible:   []string{"open " + fixtureRedactedPath(".no-mistakes", "evidence", "run-1", "pytest.log") + ": no such file or directory"},
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
	for _, want := range []string{"rootdir: ~/.no-mistakes/worktrees/ab12cd/1/svc", "evidence now lands under " + fixtureRedactedPath(".no-mistakes", "evidence", "run-1")} {
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
		wantVisible: []string{"evidence now lands under " + fixtureRedactedPath(".no-mistakes", "evidence", "run-1")},
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
		// Substitute the whole native path, JSON-escaped: a ToSlash'd Windows
		// path is absolute but not filepath.Clean, which the allowlist rejects.
		evidenceFile := filepath.Join(sctx.EvidenceDir, "pytest.log")
		assertFixturePathIsRenderable(t, evidenceFile)
		testFindings = strings.ReplaceAll(testFindings, "%EVIDENCEFILE%", strings.ReplaceAll(evidenceFile, `\`, `\\`))
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

// TestTestFindingsSchema_KeepsEvidenceDirectoryPathsReportable is the other
// side of the schema contract, and the correction to a first attempt that
// closed the loop too far.
//
// The renderer's allowlist for an absolute artifact path is the worktree or the
// run's evidence directory (see sanitizeArtifactPath); a path under neither is
// dropped rather than rendered. The evidence directory defaults to
// <NM_HOME>/evidence/<run-id> and NM_HOME defaults to a directory under the
// operator's home, so a schema clause forbidding home directory paths outright
// would forbid the one path an evidence artifact has to report - an obedient
// agent would drop its own evidence.
//
// Keeping that path reportable is safe because publication safety belongs to
// the redaction boundary in pr.go, not to the schema. The schema's job is only
// to stop soliciting paths from elsewhere on the machine.
func TestTestFindingsSchema_KeepsEvidenceDirectoryPathsReportable(t *testing.T) {
	t.Parallel()

	var schema map[string]any
	if err := json.Unmarshal(testFindingsSchema, &schema); err != nil {
		t.Fatalf("test findings schema is not valid JSON: %v", err)
	}
	// Any mention of a home directory here is wrong in both directions: it
	// either solicits the path the boundary has to strip, or forbids the one
	// the renderer requires.
	if description := artifactPathDescription(t, schema); strings.Contains(strings.ToLower(description), "home director") {
		t.Fatalf("artifact path schema must not rule on home directories, that is the redaction boundary's job: %q", description)
	}

	t.Run("evidence directory path is rendered, not dropped", func(t *testing.T) {
		t.Parallel()
		tc := homePathLeakCase{
			evidenceDir: fixtureEvidenceDir,
			testFindings: findingsJSON(t, types.Findings{
				TestingSummary: "Captured the failing request/response pair.",
				Artifacts: []types.TestArtifact{{
					Kind:  "log",
					Label: "captured session",
					Path:  filepath.Join(fixtureEvidenceDir, "session.log"),
				}},
			}),
			wantVisible: []string{
				"captured session",
				fixtureRedactedPath(".no-mistakes", "evidence", "run-1", "session.log"),
			},
		}
		content := buildHomePathLeakPRContent(t, tc)
		assertNoHomePathLeak(t, tc, content)
	})

	t.Run("path outside the worktree and evidence directory is still dropped", func(t *testing.T) {
		t.Parallel()
		tc := homePathLeakCase{
			evidenceDir: fixtureEvidenceDir,
			testFindings: findingsJSON(t, types.Findings{
				TestingSummary: "Captured the failing request/response pair.",
				Artifacts: []types.TestArtifact{{
					Kind:  "log",
					Label: "stray artifact",
					Path:  "/var/tmp/elsewhere/session.log",
				}},
			}),
		}
		content := buildHomePathLeakPRContent(t, tc)
		assertNoHomePathLeak(t, tc, content)
		for _, unwanted := range []string{"stray artifact", "/var/tmp/elsewhere/session.log"} {
			if strings.Contains(content.Body, unwanted) {
				t.Fatalf("expected %q to be dropped by the artifact path allowlist:\n%s", unwanted, content.Body)
			}
		}
	})
}
