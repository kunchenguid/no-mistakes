// Package github implements scm.Host backed by the gh CLI.
package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// CmdFactory builds an exec.Cmd in the caller's workdir with the caller's env.
type CmdFactory func(ctx context.Context, name string, args ...string) *exec.Cmd

// Host talks to GitHub through the gh CLI.
type Host struct {
	cmd          CmdFactory
	cliAvailable func() bool
}

// New builds a Host. cliAvailable reports whether the gh binary is
// resolvable on the caller's PATH (possibly overridden by env).
func New(cmd CmdFactory, cliAvailable func() bool) *Host {
	return &Host{cmd: cmd, cliAvailable: cliAvailable}
}

func (h *Host) Provider() scm.Provider { return scm.ProviderGitHub }

func (h *Host) Capabilities() scm.Capabilities {
	return scm.Capabilities{MergeableState: true, FailedCheckLogs: true}
}

func (h *Host) Available(ctx context.Context) error {
	if h.cliAvailable != nil && !h.cliAvailable() {
		return errors.New("gh CLI is not installed")
	}
	if err := h.cmd(ctx, "gh", "auth", "status").Run(); err != nil {
		return errors.New("gh CLI is not authenticated")
	}
	return nil
}

func (h *Host) FindPR(ctx context.Context, branch, _ string) (*scm.PR, error) {
	cmd := h.cmd(ctx, "gh", "pr", "view", branch, "--json", "url", "--jq", ".url")
	out, err := cmd.Output()
	if err != nil {
		return nil, nil
	}
	url := strings.TrimSpace(string(out))
	if url == "" {
		return nil, nil
	}
	pr := &scm.PR{URL: url}
	if num, nerr := scm.ExtractPRNumber(url); nerr == nil {
		pr.Number = num
	}
	return pr, nil
}

func (h *Host) CreatePR(ctx context.Context, branch, base string, content scm.PRContent) (*scm.PR, error) {
	cmd := h.cmd(ctx, "gh", "pr", "create",
		"--head", branch,
		"--base", base,
		"--title", content.Title,
		"--body", content.Body,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("gh pr create: %s: %w", strings.TrimSpace(string(out)), err)
	}
	url := strings.TrimSpace(string(out))
	pr := &scm.PR{URL: url}
	if num, nerr := scm.ExtractPRNumber(url); nerr == nil {
		pr.Number = num
	}
	return pr, nil
}

func (h *Host) UpdatePR(ctx context.Context, pr *scm.PR, content scm.PRContent) (*scm.PR, error) {
	id := pr.Number
	if id == "" {
		id = pr.URL
	}
	cmd := h.cmd(ctx, "gh", "pr", "edit", id, "--title", content.Title, "--body", content.Body)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("gh pr edit: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return pr, nil
}

func (h *Host) GetPRState(ctx context.Context, pr *scm.PR) (scm.PRState, error) {
	cmd := h.cmd(ctx, "gh", "pr", "view", pr.Number, "--json", "state", "--jq", ".state")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("gh pr view: %w", err)
	}
	return normalizePRState(strings.TrimSpace(string(out))), nil
}

func (h *Host) GetChecks(ctx context.Context, pr *scm.PR) ([]scm.Check, error) {
	cmd := h.cmd(ctx, "gh", "pr", "checks", pr.Number, "--json", "name,state,bucket")
	out, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "no checks reported") {
			return nil, nil
		}
		return nil, fmt.Errorf("gh pr checks: %w", err)
	}
	var raw []struct {
		Name   string `json:"name"`
		Bucket string `json:"bucket"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parse CI checks: %w", err)
	}
	checks := make([]scm.Check, 0, len(raw))
	for _, r := range raw {
		checks = append(checks, scm.Check{Name: r.Name, Bucket: scm.CheckBucket(r.Bucket)})
	}
	return checks, nil
}

func (h *Host) GetMergeableState(ctx context.Context, pr *scm.PR) (scm.MergeableState, error) {
	cmd := h.cmd(ctx, "gh", "pr", "view", pr.Number, "--json", "mergeable", "--jq", ".mergeable")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("gh pr view mergeable: %w", err)
	}
	return normalizeMergeableState(strings.TrimSpace(string(out))), nil
}

func (h *Host) FetchFailedCheckLogs(ctx context.Context, _ *scm.PR, branch, _ string, failingNames []string) (string, error) {
	if len(failingNames) == 0 {
		return "", nil
	}
	listCmd := h.cmd(ctx, "gh", "run", "list",
		"--branch", branch,
		"--status", "failure",
		"--limit", "1",
		"--json", "databaseId",
		"--jq", ".[0].databaseId",
	)
	listOut, err := listCmd.Output()
	if err != nil {
		return "", nil
	}
	runID := strings.TrimSpace(string(listOut))
	if runID == "" {
		return "", nil
	}
	viewCmd := h.cmd(ctx, "gh", "run", "view", runID, "--log-failed")
	out, _ := viewCmd.Output()
	return strings.TrimSpace(string(out)), nil
}

func normalizePRState(raw string) scm.PRState {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "OPEN":
		return scm.PRStateOpen
	case "MERGED":
		return scm.PRStateMerged
	case "CLOSED":
		return scm.PRStateClosed
	default:
		return scm.PRState(raw)
	}
}

func normalizeMergeableState(raw string) scm.MergeableState {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "MERGEABLE":
		return scm.MergeableOK
	case "CONFLICTING":
		return scm.MergeableConflict
	case "UNKNOWN", "":
		return scm.MergeablePending
	default:
		return scm.MergeableState(raw)
	}
}
