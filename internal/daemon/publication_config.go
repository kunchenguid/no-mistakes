package daemon

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	gitutil "github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/publication"
	"github.com/kunchenguid/no-mistakes/internal/runenv"
)

const publicationRepoConfigPath = ".no-mistakes.yaml"

// exactPublicationConfig is the narrow configuration projection consumed by
// later publication runtime composition. It contains no Agent or effect port.
type exactPublicationConfig struct {
	Config      *config.Config
	Forge       *forgecontext.Context
	Environment runenv.Overlay
}

type publicationConfigLoadHooks struct {
	AfterConfigReads func()
}

func loadExactPublicationConfig(
	ctx context.Context,
	p *paths.Paths,
	global *config.GlobalConfig,
	repo *db.Repo,
	canonicalRequest []byte,
) (*exactPublicationConfig, error) {
	return loadExactPublicationConfigWithHooks(ctx, p, global, repo, canonicalRequest, publicationConfigLoadHooks{})
}

func loadExactPublicationConfigWithHooks(
	ctx context.Context,
	p *paths.Paths,
	global *config.GlobalConfig,
	repo *db.Repo,
	canonicalRequest []byte,
	hooks publicationConfigLoadHooks,
) (*exactPublicationConfig, error) {
	if p == nil || global == nil || repo == nil {
		return nil, fmt.Errorf("exact publication config requires paths, startup global config, and repository")
	}
	parsed, err := publication.ParseRequest(canonicalRequest)
	if err != nil {
		return nil, fmt.Errorf("parse exact publication request for config: %w", err)
	}
	binding := parsed.Request.Candidate
	if binding.RepositoryID != repo.ID {
		return nil, fmt.Errorf("publication config repository does not match the canonical candidate binding")
	}
	if strings.TrimSpace(repo.WorkingPath) == "" {
		return nil, fmt.Errorf("registered publication repository path is empty")
	}

	gitCtx := exactPublicationGitContext(ctx)
	if err := validateExactPublicationConfigRefs(gitCtx, repo.WorkingPath, binding); err != nil {
		return nil, fmt.Errorf("validate publication config refs before read: %w", err)
	}
	pushedRaw, pushedPresent, err := readPublicationRepoConfigAtCommit(gitCtx, repo.WorkingPath, binding.CommitSHA)
	if err != nil {
		return nil, fmt.Errorf("read pushed publication config at exact H: %w", err)
	}
	trustedRaw, trustedPresent, err := readPublicationRepoConfigAtCommit(gitCtx, repo.WorkingPath, binding.BaseSHA)
	if err != nil {
		return nil, fmt.Errorf("read trusted publication config at exact BaseSHA: %w", err)
	}
	if hooks.AfterConfigReads != nil {
		hooks.AfterConfigReads()
	}
	if err := validateExactPublicationConfigRefs(gitCtx, repo.WorkingPath, binding); err != nil {
		return nil, fmt.Errorf("validate publication config refs after read: %w", err)
	}

	pushed, err := parseExactPublicationRepoConfig(pushedRaw, pushedPresent)
	if err != nil {
		return nil, fmt.Errorf("parse pushed publication config at exact H: %w", err)
	}
	trusted, err := parseExactPublicationRepoConfig(trustedRaw, trustedPresent)
	if err != nil {
		return nil, fmt.Errorf("parse trusted publication config at exact BaseSHA: %w", err)
	}
	allowRepoCommands := trusted.AllowRepoCommands
	effective := config.EffectiveRepoConfig(pushed, trusted, allowRepoCommands)
	merged := config.Merge(global, effective)
	if err := p.ValidateEvidenceRoot(merged.Test.Evidence.LocalRoot); err != nil {
		return nil, fmt.Errorf("validate exact publication evidence root: %w", err)
	}
	merged.TrustedConfigSHA = binding.BaseSHA

	forgeCtx, err := forgecontext.Resolve(ctx, merged.ForgeProfiles, repo.UpstreamURL, repo.ForkURL)
	if err != nil {
		return nil, fmt.Errorf("resolve exact publication forge profile: %w", err)
	}
	if err := validateExactPublicationConfigRefs(gitCtx, repo.WorkingPath, binding); err != nil {
		return nil, fmt.Errorf("revalidate publication config refs before use: %w", err)
	}
	environment := forgeEnvironment(forgeCtx).Clone()
	return &exactPublicationConfig{Config: merged, Forge: forgeCtx, Environment: environment}, nil
}

func parseExactPublicationRepoConfig(raw []byte, present bool) (*config.RepoConfig, error) {
	if !present {
		return &config.RepoConfig{}, nil
	}
	parsed, err := config.LoadRepoFromBytes(raw)
	if err != nil {
		return nil, err
	}
	return parsed, nil
}

func validateExactPublicationConfigRefs(ctx context.Context, repoDir string, binding publication.CandidateBinding) error {
	headTarget, exists, err := gitutil.ExactRefTarget(ctx, repoDir, binding.HeadRef)
	if err != nil {
		return fmt.Errorf("resolve exact CandidateRef: %w", err)
	}
	if !exists || headTarget != binding.CommitSHA {
		return fmt.Errorf("CandidateRef does not name exact H")
	}
	headCommit, err := gitutil.ResolveRef(ctx, repoDir, binding.CommitSHA)
	if err != nil || headCommit != binding.CommitSHA {
		return fmt.Errorf("exact H is not a directly named commit")
	}
	tree, err := gitutil.Run(ctx, repoDir, "rev-parse", "--verify", binding.CommitSHA+"^{tree}")
	if err != nil {
		return fmt.Errorf("resolve exact H tree: %w", err)
	}
	if tree != binding.TreeSHA {
		return fmt.Errorf("exact H tree does not match the canonical candidate binding")
	}

	baseTarget, exists, err := gitutil.ExactRefTarget(ctx, repoDir, binding.BaseRef)
	if err != nil {
		return fmt.Errorf("resolve exact BaseRef: %w", err)
	}
	if !exists || baseTarget != binding.BaseSHA {
		return fmt.Errorf("BaseRef does not name exact BaseSHA")
	}
	baseCommit, err := gitutil.ResolveRef(ctx, repoDir, binding.BaseSHA)
	if err != nil || baseCommit != binding.BaseSHA {
		return fmt.Errorf("exact BaseSHA is not a directly named commit")
	}
	if _, err := gitutil.Run(ctx, repoDir, "merge-base", "--is-ancestor", binding.BaseSHA, binding.CommitSHA); err != nil {
		return fmt.Errorf("exact BaseSHA is not an ancestor of H")
	}
	return nil
}

// readPublicationRepoConfigAtCommit reads only an ordinary blob selected by
// one exact commit tree. An absent entry is a legitimate empty config; a
// symlink, submodule, tree, malformed listing, or unreadable blob fails closed.
func readPublicationRepoConfigAtCommit(ctx context.Context, repoDir, commitSHA string) ([]byte, bool, error) {
	listing, err := gitutil.RunRaw(ctx, repoDir, "ls-tree", "-z", "--full-tree", commitSHA, "--", publicationRepoConfigPath)
	if err != nil {
		return nil, false, err
	}
	if len(listing) == 0 {
		return nil, false, nil
	}
	entries := bytes.Split(listing, []byte{0})
	if len(entries) != 2 || len(entries[1]) != 0 {
		return nil, false, fmt.Errorf("exact publication config tree entry is ambiguous")
	}
	metadata, pathBytes, ok := bytes.Cut(entries[0], []byte{'\t'})
	if !ok || string(pathBytes) != publicationRepoConfigPath {
		return nil, false, fmt.Errorf("exact publication config tree entry is malformed")
	}
	fields := strings.Fields(string(metadata))
	if len(fields) != 3 || (fields[0] != "100644" && fields[0] != "100755") || fields[1] != "blob" {
		return nil, false, fmt.Errorf("exact publication config is not an ordinary file blob")
	}
	raw, err := gitutil.RunRaw(ctx, repoDir, "cat-file", "blob", fields[2])
	if err != nil {
		return nil, false, err
	}
	return bytes.Clone(raw), true, nil
}

func exactPublicationGitContext(ctx context.Context) context.Context {
	unset := []string{
		"GIT_ALTERNATE_OBJECT_DIRECTORIES",
		"GIT_COMMON_DIR",
		"GIT_DIR",
		"GIT_INDEX_FILE",
		"GIT_NAMESPACE",
		"GIT_OBJECT_DIRECTORY",
		"GIT_PREFIX",
		"GIT_REPLACE_REF_BASE",
		"GIT_WORK_TREE",
	}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "GIT_CONFIG_") {
			unset = append(unset, key)
		}
	}
	return gitutil.WithEnvironment(ctx, runenv.Overlay{
		Set:   map[string]string{"GIT_NO_REPLACE_OBJECTS": "1"},
		Unset: unset,
	})
}
