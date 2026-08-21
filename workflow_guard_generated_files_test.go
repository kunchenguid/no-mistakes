package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestGuardGeneratedFilesWorkflowCoversReleasePleaseArtifacts pins the list of
// guarded paths. If release-please starts managing more files, add them here
// and to the workflow together.
func TestGuardGeneratedFilesWorkflowCoversReleasePleaseArtifacts(t *testing.T) {
	data, err := os.ReadFile(".github/workflows/guard-generated-files.yml")
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}
	content := string(data)

	guarded := []string{
		"CHANGELOG.md",
		".release-please-manifest.json",
	}
	for _, path := range guarded {
		if !strings.Contains(content, path) {
			t.Errorf("workflow must guard %q", path)
		}
		if _, err := os.Stat(path); err != nil {
			t.Errorf("guarded path %q not present in repo: %v", path, err)
		}
	}
}

// TestGuardGeneratedFilesWorkflowExemptsReleasePlease ensures the release
// pipeline's own PR (which legitimately modifies the generated files) is
// always allowed through.
func TestGuardGeneratedFilesWorkflowExemptsReleasePlease(t *testing.T) {
	data, err := os.ReadFile(".github/workflows/guard-generated-files.yml")
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}
	content := string(data)

	for _, login := range []string{"github-actions[bot]", "release-please[bot]"} {
		needle := "github.event.pull_request.user.login != '" + login + "'"
		if !strings.Contains(content, needle) {
			t.Errorf("workflow must exempt %q via %q", login, needle)
		}
	}
}

// TestGuardGeneratedFilesWorkflowUsesGitDiffWithFullHistory pins the
// git-based file-diff approach. Using the API would add a permission surface
// (pull-requests: read), rate-limit exposure, and pagination concerns; the
// git three-dot diff matches exactly what GitHub shows in "Files changed".
func TestGuardGeneratedFilesWorkflowUsesGitDiffWithFullHistory(t *testing.T) {
	data, err := os.ReadFile(".github/workflows/guard-generated-files.yml")
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, "actions/checkout") {
		t.Errorf("workflow must check out the repo to run git diff locally")
	}
	if !strings.Contains(content, "fetch-depth: 0") {
		t.Errorf("workflow must use fetch-depth: 0 so merge-base for base...head is available")
	}
	if !strings.Contains(content, `git diff --name-only "${BASE_SHA}...${HEAD_SHA}"`) {
		t.Errorf("workflow must use 'git diff --name-only base...head' (three-dot) for PR file list")
	}
	if strings.Contains(content, "gh api") {
		t.Errorf("workflow must not fall back to the GitHub API for file listing")
	}
	if strings.Contains(content, "pull-requests:") {
		t.Errorf("workflow must not request pull-requests permission once switched to git diff")
	}
}

// TestGuardGeneratedFilesWorkflowTriggersOnPushedCommits ensures the guard
// re-runs when new commits are pushed to a PR (the synchronize event), so a
// contributor cannot open a clean PR then push a commit that edits CHANGELOG.md.
func TestGuardGeneratedFilesWorkflowTriggersOnPushedCommits(t *testing.T) {
	data, err := os.ReadFile(".github/workflows/guard-generated-files.yml")
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}
	content := string(data)

	for _, typ := range []string{"opened", "synchronize", "reopened"} {
		if !strings.Contains(content, typ) {
			t.Errorf("workflow must trigger on pull_request type %q", typ)
		}
	}
}

// TestGuardGeneratedFilesCheckScriptDistinguishesReleaseCommitsFromHandEdits
// runs the workflow's check script against real git history. Official
// release-please commits may update CHANGELOG.md; a human commit that edits
// the same file must still fail.
func TestGuardGeneratedFilesCheckScriptDistinguishesReleaseCommitsFromHandEdits(t *testing.T) {
	script := guardGeneratedFilesCheckScript(t)
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is required to execute the ubuntu-latest workflow script")
	}

	t.Run("official release commit is allowed", func(t *testing.T) {
		dir := initGuardGeneratedFilesRepo(t)
		base := guardGit(t, dir, "rev-parse", "HEAD")
		writeGuardFile(t, dir, "CHANGELOG.md", "# Changelog\n\n## 1.53.0\n")
		writeGuardFile(t, dir, ".release-please-manifest.json", `{".":"1.53.0"}`+"\n")
		guardGit(t, dir, "add", "CHANGELOG.md", ".release-please-manifest.json")
		guardGitAuthor(t, dir, "github-actions[bot]", "41898282+github-actions[bot]@users.noreply.github.com",
			"commit", "-m", "chore(main): release 1.53.0")
		writeGuardFile(t, dir, "feature.txt", "other work\n")
		guardGit(t, dir, "add", "feature.txt")
		guardGit(t, dir, "commit", "-m", "feat: unrelated change")
		head := guardGit(t, dir, "rev-parse", "HEAD")

		if err := runGuardGeneratedFilesScript(t, bash, script, dir, base, head); err != nil {
			t.Fatalf("official release-please changelog update should pass: %v", err)
		}
	})

	t.Run("human changelog edit is rejected", func(t *testing.T) {
		dir := initGuardGeneratedFilesRepo(t)
		base := guardGit(t, dir, "rev-parse", "HEAD")
		writeGuardFile(t, dir, "CHANGELOG.md", "# Changelog\n\n## hand-edited\n")
		guardGit(t, dir, "add", "CHANGELOG.md")
		guardGit(t, dir, "commit", "-m", "docs: hand-edit changelog")
		head := guardGit(t, dir, "rev-parse", "HEAD")

		err := runGuardGeneratedFilesScript(t, bash, script, dir, base, head)
		if err == nil {
			t.Fatal("human changelog edit should fail the guard")
		}
		if !strings.Contains(err.Error(), "This PR modifies release-please-generated files: CHANGELOG.md") {
			t.Fatalf("guard error = %v, want hand-edit rejection naming CHANGELOG.md", err)
		}
	})
}

func guardGeneratedFilesCheckScript(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(".github", "workflows", "guard-generated-files.yml"))
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}
	var wf wfDoc
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("parse workflow: %v", err)
	}
	job, ok := wf.Jobs["check"]
	if !ok {
		t.Fatal("guard workflow has no check job")
	}
	for _, step := range job.Steps {
		if step.Name == "Check PR does not modify release-please-generated files" {
			if strings.TrimSpace(step.Run) == "" {
				t.Fatal("guard check step has empty run script")
			}
			return step.Run
		}
	}
	t.Fatal("guard workflow is missing the generated-files check step")
	return ""
}

func initGuardGeneratedFilesRepo(t *testing.T) string {
	t.Helper()
	t.Setenv("GIT_CONFIG_COUNT", "0")
	dir := t.TempDir()
	guardGit(t, dir, "init", "-b", "main")
	guardGit(t, dir, "config", "user.name", "human")
	guardGit(t, dir, "config", "user.email", "human@example.com")
	guardGit(t, dir, "config", "core.autocrlf", "false")
	writeGuardFile(t, dir, "CHANGELOG.md", "# Changelog\n\n## 1.50.0\n")
	writeGuardFile(t, dir, ".release-please-manifest.json", `{".":"1.50.0"}`+"\n")
	writeGuardFile(t, dir, "README.md", "repo\n")
	guardGit(t, dir, "add", ".")
	guardGit(t, dir, "commit", "-m", "initial")
	return dir
}

func writeGuardFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, rel), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func guardGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return guardGitAuthor(t, dir, "", "", args...)
}

func guardGitAuthor(t *testing.T, dir, name, email string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_COUNT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+filepath.Join(dir, ".unused-global-gitconfig"),
	)
	if name != "" {
		cmd.Env = append(cmd.Env,
			"GIT_AUTHOR_NAME="+name,
			"GIT_AUTHOR_EMAIL="+email,
			"GIT_COMMITTER_NAME="+name,
			"GIT_COMMITTER_EMAIL="+email,
		)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func runGuardGeneratedFilesScript(t *testing.T, bash, script, dir, base, head string) error {
	t.Helper()
	cmd := exec.Command(bash, "-c", script)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_COUNT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+filepath.Join(dir, ".unused-global-gitconfig"),
		"BASE_SHA="+base,
		"HEAD_SHA="+head,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return &guardScriptError{err: err, out: strings.TrimSpace(string(out))}
	}
	return nil
}

type guardScriptError struct {
	err error
	out string
}

func (e *guardScriptError) Error() string {
	if e.out == "" {
		return e.err.Error()
	}
	return e.out + "\n" + e.err.Error()
}
