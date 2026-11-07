package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/runenv"
)

func captureSubmissionIdentity(ctx context.Context, client *ipc.Client, dir string) (*git.CommitIdentity, error) {
	if err := client.RequireCommitIdentity(); err != nil {
		return nil, err
	}
	identity, err := git.CaptureCommitIdentity(ctx, dir)
	if err != nil {
		return nil, err
	}
	return &identity, nil
}

// A receive hook runs inside the bare gate. Resolve the registered submitting
// checkout, and discard receive-pack's repository-routing variables only. The
// pushing process's identity and config environment must still reach Git.
func captureHookIdentity(ctx context.Context, client *ipc.Client, p *paths.Paths, gateDir string) (*git.CommitIdentity, error) {
	database, err := db.OpenReadOnly(p.DB())
	if err != nil {
		return nil, err
	}
	defer database.Close()
	repo, err := database.GetRepo(strings.TrimSuffix(filepath.Base(gateDir), ".git"))
	if err != nil {
		return nil, err
	}
	if repo == nil {
		return nil, fmt.Errorf("unknown repo for gate %s", gateDir)
	}
	ctx = git.WithEnvironment(ctx, runenv.Overlay{Unset: []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES"}})
	listing, err := git.Run(ctx, repo.WorkingPath, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return nil, err
	}
	if err := client.RequireCommitIdentity(); err != nil {
		return nil, err
	}
	var identity *git.CommitIdentity
	for _, entry := range strings.Split(listing, "\x00\x00") {
		if entry == "" {
			continue
		}
		fields := strings.Split(entry, "\x00")
		checkout, ok := strings.CutPrefix(fields[0], "worktree ")
		if !ok || checkout == "" {
			return nil, fmt.Errorf("cannot prove the pushing checkout identity: invalid worktree listing")
		}
		prunable := false
		for _, field := range fields[1:] {
			if field == "prunable" || strings.HasPrefix(field, "prunable ") {
				prunable = true
				break
			}
		}
		if prunable {
			continue
		}
		candidate, err := git.CaptureCommitIdentity(ctx, checkout)
		if err != nil {
			return nil, fmt.Errorf("cannot prove the pushing checkout identity: %w", err)
		}
		if identity != nil && *identity != candidate {
			return nil, fmt.Errorf("cannot prove the pushing checkout identity: worktree identities differ; submit with no-mistakes axi run")
		}
		if identity == nil {
			identity = &candidate
		}
	}
	if identity == nil {
		return nil, fmt.Errorf("cannot prove the pushing checkout identity: no live checkouts")
	}
	return identity, nil
}

const commitIdentityPushOptionPrefix = "no-mistakes.commit-identity="

func formatCommitIdentityPushOption(identity git.CommitIdentity) string {
	data, _ := json.Marshal(identity)
	return commitIdentityPushOptionPrefix + base64.StdEncoding.EncodeToString(data)
}

func parseCommitIdentityPushOptions(options []string) (*git.CommitIdentity, error) {
	var identity *git.CommitIdentity
	for _, option := range options {
		encoded, ok := strings.CutPrefix(option, commitIdentityPushOptionPrefix)
		if !ok {
			continue
		}
		if identity != nil {
			return nil, fmt.Errorf("duplicate commit identity push option")
		}
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("decode commit identity push option: %w", err)
		}
		identity = new(git.CommitIdentity)
		if err := json.Unmarshal(data, identity); err != nil {
			return nil, fmt.Errorf("parse commit identity push option: %w", err)
		}
		if err := identity.Validate(); err != nil {
			return nil, err
		}
	}
	return identity, nil
}
