package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// The fake providers merge only the harness's local bare upstream. Their CLI
// lifecycle and raw PR facts share this result, as they do on a real forge.
type stubPRMerge struct {
	Head     string
	Base     string
	Repo     string
	HeadSHA  string
	MergeSHA string
}

func readStubPRMerge(logPath, command string) (*stubPRMerge, error) {
	data, err := os.ReadFile(logPath + ".merged.json")
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var result stubPRMerge
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	head, base, repo := lastStubCreate(logPath, command)
	if base == "" {
		base = "main"
	}
	if result.Head != head || result.Base != base || result.Repo != repo {
		return nil, nil
	}
	return &result, nil
}

func resetStubPRMerge(logPath string) error {
	if err := os.Remove(logPath + ".merged.json"); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func mergeStubPR(logPath, command string) (*stubPRMerge, error) {
	if merged, err := readStubPRMerge(logPath, command); err != nil || merged != nil {
		return merged, err
	}
	targetDir := os.Getenv("FAKEAGENT_PR_UPSTREAM")
	if targetDir == "" || !filepath.IsAbs(targetDir) {
		return nil, fmt.Errorf("provider fixture requires the harness's absolute local upstream")
	}
	info, err := os.Stat(targetDir)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("provider fixture upstream is not a local directory")
	}
	runGit := func(args ...string) (string, error) {
		out, err := exec.Command("git", args...).CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("fixture git %v: %w: %s", args, err, strings.TrimSpace(string(out)))
		}
		return strings.TrimSpace(string(out)), nil
	}
	bare, err := runGit("--git-dir", targetDir, "rev-parse", "--is-bare-repository")
	if err != nil || bare != "true" {
		return nil, fmt.Errorf("provider fixture upstream must be bare: %v", err)
	}
	head, base, repo := lastStubCreate(logPath, command)
	if head == "" {
		return nil, fmt.Errorf("provider fixture has no created PR head")
	}
	if base == "" {
		base = "main"
	}
	if _, err := runGit("check-ref-format", "refs/heads/"+base); err != nil {
		return nil, err
	}
	headSHA, err := runGit("rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	if _, err := runGit("fetch", "--no-tags", targetDir, "refs/heads/"+base); err != nil {
		return nil, err
	}
	targetSHA, err := runGit("rev-parse", "FETCH_HEAD")
	if err != nil {
		return nil, err
	}
	tree, err := runGit("merge-tree", "--write-tree", targetSHA, headSHA)
	if err != nil {
		return nil, err
	}
	mergeSHA, err := runGit("-c", "user.name=E2E provider", "-c", "user.email=e2e-provider@example.com", "commit-tree", tree, "-p", targetSHA, "-p", headSHA, "-m", "Merge fixture PR")
	if err != nil {
		return nil, err
	}
	if _, err := runGit("push", targetDir, mergeSHA+":refs/heads/"+base); err != nil {
		return nil, err
	}
	result := &stubPRMerge{Head: head, Base: base, Repo: repo, HeadSHA: headSHA, MergeSHA: mergeSHA}
	data, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(logPath+".merged.json", data, 0600); err != nil {
		return nil, err
	}
	return result, nil
}
