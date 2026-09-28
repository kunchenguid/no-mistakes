package git

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

// WorktreeAddCoW creates a detached gate worktree using verified files from an
// independent checkout. A bare gate has no checked-out donor of its own.
// cowtree retains an incomplete worktree for inspection when creation fails.
func WorktreeAddCoW(ctx context.Context, gateDir, wtPath, sha, donorPath, cowtreePath string) error {
	for name, path := range map[string]string{
		"gate": gateDir, "worktree": wtPath, "donor": donorPath, "cowtree": cowtreePath,
	} {
		if !filepath.IsAbs(path) {
			return fmt.Errorf("CoW %s path must be absolute", name)
		}
	}
	if !isBareGitDir(gateDir) {
		return fmt.Errorf("CoW gate is not a bare repository")
	}
	cmd := exec.CommandContext(ctx, cowtreePath, "add", "--detach", wtPath, sha)
	cmd.Dir = gateDir
	env := make([]string, 0, len(os.Environ())+3)
	for _, item := range nonInteractiveEnvForContext(ctx, gateDir) {
		if strings.HasPrefix(item, "GIT_DIR=") || strings.HasPrefix(item, "GIT_WORK_TREE=") ||
			strings.HasPrefix(item, "GIT_COMMON_DIR=") || strings.HasPrefix(item, "GIT_INDEX_FILE=") ||
			strings.HasPrefix(item, "GIT_PREFIX=") || strings.HasPrefix(item, "COWTREE_ADD_DONOR=") {
			continue
		}
		env = append(env, item)
	}
	env = append(env, "GIT_DIR="+gateDir, "COWTREE_ADD_DONOR="+donorPath)
	if original := os.Getenv("ATER_COW_ORIGINAL_PATH"); original != "" {
		env = append(env, "PATH="+original)
	}
	cmd.Env = env
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	shellenv.ConfigureShellCommand(cmd)
	err := shellenv.RunShellCommand(cmd)
	if err != nil {
		return fmt.Errorf("cowtree add: %w (incomplete worktree retained for inspection)", err)
	}
	receiptPath, err := Run(ctx, wtPath, "rev-parse", "--git-path", "cowtree-creation")
	if err != nil {
		return fmt.Errorf("locate CoW creation receipt: %w", err)
	}
	data, err := os.ReadFile(receiptPath)
	if err != nil {
		return fmt.Errorf("read CoW creation receipt: %w", err)
	}
	var receipt struct {
		Version       int      `json:"version"`
		Operation     string   `json:"operation"`
		TargetCommit  string   `json:"target_commit"`
		SourceCommits []string `json:"source_commits"`
		ClonedFiles   uint64   `json:"cloned_files"`
	}
	if err := json.Unmarshal(data, &receipt); err != nil {
		return fmt.Errorf("parse CoW creation receipt: %w", err)
	}
	if receipt.Version != 1 || receipt.Operation != "create" || receipt.TargetCommit != sha ||
		len(receipt.SourceCommits) == 0 || receipt.ClonedFiles == 0 {
		return fmt.Errorf("CoW creation receipt does not prove cloning for %s", sha)
	}
	return nil
}
