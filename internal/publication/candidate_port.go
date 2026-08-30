package publication

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/kunchenguid/no-mistakes/internal/db"
	gitutil "github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/runenv"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// GitCandidatePortOptions are the durable state and No-Mistakes-owned root
// used to materialize disposable publication candidates.
type GitCandidatePortOptions struct {
	DB   *db.DB
	Root string
}

type candidateViewKey struct {
	publicationID string
	step          types.StepName
}

type preparedCandidateView struct {
	CandidateStepView
	containerDir string
	sourceDir    string
	candidateRef string
	commitSHA    string
	treeSHA      string
	contractPath string
	contractSHA  string
}

// GitCandidatePort owns independent Git repositories for publication defense.
// It never creates linked worktree metadata or refs in a registered checkout.
type GitCandidatePort struct {
	db   *db.DB
	root string

	mu    sync.Mutex
	views map[candidateViewKey]*preparedCandidateView
}

// NewGitCandidatePort prepares the private candidate root without touching a
// registered repository.
func NewGitCandidatePort(options GitCandidatePortOptions) (*GitCandidatePort, error) {
	if options.DB == nil {
		return nil, fmt.Errorf("candidate port database is required")
	}
	if strings.TrimSpace(options.Root) == "" {
		return nil, fmt.Errorf("candidate port root is required")
	}
	root, err := filepath.Abs(options.Root)
	if err != nil {
		return nil, fmt.Errorf("resolve candidate root: %w", err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create candidate root: %w", err)
	}
	info, err := os.Lstat(root)
	if err != nil {
		return nil, fmt.Errorf("inspect candidate root: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("candidate root is not a private directory")
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return nil, fmt.Errorf("protect candidate root: %w", err)
	}
	return &GitCandidatePort{
		db:    options.DB,
		root:  root,
		views: make(map[candidateViewKey]*preparedCandidateView),
	}, nil
}

// PrepareStep creates a fresh, independent checkout of exact commit H and a
// separate writable scratch directory. The checkout and its Git metadata are
// made read-only only after all admission checks have passed.
func (p *GitCandidatePort) PrepareStep(ctx context.Context, publicationID string, step types.StepName) (CandidateStepView, error) {
	if !candidateGuardedStep(step) {
		return CandidateStepView{}, fmt.Errorf("step %s does not use a guarded candidate", step)
	}
	key := candidateViewKey{publicationID: publicationID, step: step}

	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.views[key]; exists {
		return CandidateStepView{}, fmt.Errorf("candidate view already prepared for publication %s step %s", publicationID, step)
	}

	publication, parsed, repo, err := p.loadBinding(publicationID)
	if err != nil {
		return CandidateStepView{}, err
	}
	if err := p.ensureRootSeparateFromRepo(repo.WorkingPath); err != nil {
		return CandidateStepView{}, err
	}
	ctx = isolatedGitContext(ctx)
	if err := verifyRegisteredCandidate(ctx, repo.WorkingPath, publication.CandidateRef, publication.HeadSHA, publication.TreeSHA); err != nil {
		return CandidateStepView{}, err
	}

	container, err := os.MkdirTemp(p.root, ".candidate-")
	if err != nil {
		return CandidateStepView{}, fmt.Errorf("create candidate container: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = removeCandidateContainer(container)
		}
	}()

	worktree := filepath.Join(container, "candidate")
	scratch := filepath.Join(container, "scratch")
	hooks := filepath.Join(container, "empty-hooks")
	if err := os.Mkdir(scratch, 0o700); err != nil {
		return CandidateStepView{}, fmt.Errorf("create candidate scratch: %w", err)
	}
	if err := os.Mkdir(hooks, 0o500); err != nil {
		return CandidateStepView{}, fmt.Errorf("create empty candidate hooks directory: %w", err)
	}

	if _, err := gitutil.Run(ctx, container,
		"clone", "--no-local", "--no-checkout", "--no-tags", "--template=", "--",
		repo.WorkingPath, worktree,
	); err != nil {
		return CandidateStepView{}, fmt.Errorf("clone private candidate: %w", err)
	}
	if err := verifyIndependentGitDir(worktree); err != nil {
		return CandidateStepView{}, err
	}
	if _, err := gitutil.Run(ctx, worktree, "-c", "core.hooksPath="+hooks, "checkout", "--detach", publication.HeadSHA); err != nil {
		return CandidateStepView{}, fmt.Errorf("checkout exact candidate commit: %w", err)
	}
	if _, err := gitutil.Run(ctx, worktree, "remote", "remove", "origin"); err != nil {
		return CandidateStepView{}, fmt.Errorf("detach candidate from registered source: %w", err)
	}
	if err := rejectEscapingSymlinks(worktree); err != nil {
		return CandidateStepView{}, err
	}

	if err := verifyRegisteredCandidate(ctx, repo.WorkingPath, publication.CandidateRef, publication.HeadSHA, publication.TreeSHA); err != nil {
		return CandidateStepView{}, fmt.Errorf("registered candidate changed during materialization: %w", err)
	}
	if err := verifyPrivateCandidate(ctx, worktree, publication.HeadSHA, publication.TreeSHA); err != nil {
		return CandidateStepView{}, err
	}
	contractRaw, err := gitutil.RunRaw(ctx, worktree, "show", publication.HeadSHA+":"+parsed.Request.WorkContract.Path)
	if err != nil {
		return CandidateStepView{}, fmt.Errorf("read WorkContract from exact candidate: %w", err)
	}
	if sha256HexBytes(contractRaw) != parsed.Request.WorkContract.SHA256 {
		return CandidateStepView{}, fmt.Errorf("WorkContract raw-byte SHA-256 does not match publication binding")
	}

	prepared := &preparedCandidateView{
		CandidateStepView: CandidateStepView{
			WorktreeDir:     worktree,
			ScratchDir:      scratch,
			WorkContractRaw: bytes.Clone(contractRaw),
		},
		containerDir: container,
		sourceDir:    repo.WorkingPath,
		candidateRef: publication.CandidateRef,
		commitSHA:    publication.HeadSHA,
		treeSHA:      publication.TreeSHA,
		contractPath: parsed.Request.WorkContract.Path,
		contractSHA:  parsed.Request.WorkContract.SHA256,
	}
	if _, err := inspectPreparedCandidate(ctx, prepared); err != nil {
		return CandidateStepView{}, err
	}
	if err := makeTreeReadOnly(worktree); err != nil {
		return CandidateStepView{}, fmt.Errorf("make candidate read-only: %w", err)
	}
	if _, err := inspectPreparedCandidate(ctx, prepared); err != nil {
		return CandidateStepView{}, fmt.Errorf("inspect read-only candidate: %w", err)
	}

	p.views[key] = prepared
	cleanup = false
	return cloneCandidateStepView(prepared.CandidateStepView), nil
}

// Inspect returns the tamper-evident snapshot of a prepared view while also
// rechecking that the registered source ref still names exact H/tree.
func (p *GitCandidatePort) Inspect(ctx context.Context, publicationID string, step types.StepName) (CandidateSnapshot, error) {
	key := candidateViewKey{publicationID: publicationID, step: step}
	p.mu.Lock()
	defer p.mu.Unlock()
	prepared := p.views[key]
	if prepared == nil {
		return CandidateSnapshot{}, fmt.Errorf("candidate view is not prepared for publication %s step %s", publicationID, step)
	}
	ctx = isolatedGitContext(ctx)
	if err := verifyRegisteredCandidate(ctx, prepared.sourceDir, prepared.candidateRef, prepared.commitSHA, prepared.treeSHA); err != nil {
		return CandidateSnapshot{}, err
	}
	return inspectPreparedCandidate(ctx, prepared)
}

// CheckUpToDate is the protected profile's read-only replacement for Rebase.
// It accepts only the currently prepared Rebase view, revalidates exact H and
// its tree, and proves that the bound, stable live BaseRef commit is an
// ancestor of H.
// It never fetches, rebases, checks out, updates a ref, or writes either repo.
func (p *GitCandidatePort) CheckUpToDate(ctx context.Context, publicationID string, view CandidateStepView) error {
	key := candidateViewKey{publicationID: publicationID, step: types.StepRebase}
	p.mu.Lock()
	defer p.mu.Unlock()

	prepared := p.views[key]
	if prepared == nil || !sameCandidateStepView(prepared.CandidateStepView, view) {
		return fmt.Errorf("freshness view is not the active Rebase candidate for publication %s", publicationID)
	}
	publication, parsed, repo, err := p.loadBinding(publicationID)
	if err != nil {
		return err
	}
	if prepared.sourceDir != repo.WorkingPath || prepared.candidateRef != publication.CandidateRef ||
		prepared.commitSHA != publication.HeadSHA || prepared.treeSHA != publication.TreeSHA {
		return fmt.Errorf("freshness view does not match the durable publication binding")
	}
	ctx = isolatedGitContext(ctx)
	if _, err := inspectPreparedCandidate(ctx, prepared); err != nil {
		return fmt.Errorf("inspect exact freshness view: %w", err)
	}
	if err := verifyRegisteredCandidate(ctx, repo.WorkingPath, publication.CandidateRef, publication.HeadSHA, publication.TreeSHA); err != nil {
		return fmt.Errorf("revalidate registered candidate for freshness: %w", err)
	}

	baseCommit, err := exactCommitAtRef(ctx, repo.WorkingPath, parsed.Request.Candidate.BaseRef)
	if err != nil {
		return err
	}
	if baseCommit != publication.BaseSHA || baseCommit != parsed.Request.Candidate.BaseSHA {
		return fmt.Errorf("candidate base ref drifted: got %s, want exact base %s", baseCommit, publication.BaseSHA)
	}
	if _, err := gitutil.Run(ctx, repo.WorkingPath, "merge-base", "--is-ancestor", baseCommit, publication.HeadSHA); err != nil {
		return fmt.Errorf("candidate requires rebase: exact base %s is not an ancestor of H %s", baseCommit, publication.HeadSHA)
	}
	baseAfter, err := exactCommitAtRef(ctx, repo.WorkingPath, parsed.Request.Candidate.BaseRef)
	if err != nil {
		return err
	}
	if baseAfter != publication.BaseSHA {
		return fmt.Errorf("candidate base ref changed during freshness check")
	}
	if err := verifyRegisteredCandidate(ctx, repo.WorkingPath, publication.CandidateRef, publication.HeadSHA, publication.TreeSHA); err != nil {
		return fmt.Errorf("registered candidate changed during freshness check: %w", err)
	}
	if _, err := inspectPreparedCandidate(ctx, prepared); err != nil {
		return fmt.Errorf("freshness view changed during check: %w", err)
	}
	return nil
}

// DisposeStep removes only the private container created by PrepareStep. It
// restores write permission on directories (not files or symlinks), which is
// sufficient for unlinking and cannot chmod a hard-linked source file.
func (p *GitCandidatePort) DisposeStep(_ context.Context, publicationID string, step types.StepName) error {
	key := candidateViewKey{publicationID: publicationID, step: step}
	p.mu.Lock()
	defer p.mu.Unlock()
	prepared := p.views[key]
	if prepared == nil {
		return nil
	}
	if err := removeCandidateContainer(prepared.containerDir); err != nil {
		return fmt.Errorf("dispose candidate view: %w", err)
	}
	delete(p.views, key)
	return nil
}

func (p *GitCandidatePort) loadBinding(publicationID string) (*db.Publication, ParsedRequest, *db.Repo, error) {
	publication, err := p.db.GetPublication(publicationID)
	if err != nil {
		return nil, ParsedRequest{}, nil, err
	}
	if publication == nil {
		return nil, ParsedRequest{}, nil, fmt.Errorf("publication %s is not registered", publicationID)
	}
	parsed, err := ParseRequest(publication.CanonicalRequest)
	if err != nil {
		return nil, ParsedRequest{}, nil, fmt.Errorf("parse stored publication request: %w", err)
	}
	request := parsed.Request
	if parsed.PublicationID != publication.PublicationID || publication.PublicationID != publicationID ||
		request.Candidate.RepositoryID != publication.RepoID ||
		request.Candidate.HeadRef != publication.CandidateRef ||
		request.Candidate.BaseRef != publication.BaseRef ||
		request.Candidate.BaseSHA != publication.BaseSHA ||
		request.Candidate.CommitSHA != publication.HeadSHA ||
		request.Candidate.TreeSHA != publication.TreeSHA {
		return nil, ParsedRequest{}, nil, fmt.Errorf("stored publication binding is inconsistent")
	}
	repo, err := p.db.GetRepo(publication.RepoID)
	if err != nil {
		return nil, ParsedRequest{}, nil, err
	}
	if repo == nil {
		return nil, ParsedRequest{}, nil, fmt.Errorf("registered repository %s was not found", publication.RepoID)
	}
	return publication, parsed, repo, nil
}

func (p *GitCandidatePort) ensureRootSeparateFromRepo(repoPath string) error {
	root, err := filepath.EvalSymlinks(p.root)
	if err != nil {
		return fmt.Errorf("resolve candidate root: %w", err)
	}
	repo, err := filepath.EvalSymlinks(repoPath)
	if err != nil {
		return fmt.Errorf("resolve registered repository: %w", err)
	}
	if pathsOverlap(root, repo) {
		return fmt.Errorf("candidate root and registered repository must be disjoint")
	}
	return nil
}

func pathsOverlap(a, b string) bool {
	within := func(path, parent string) bool {
		rel, err := filepath.Rel(parent, path)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	return within(a, b) || within(b, a)
}

func verifyRegisteredCandidate(ctx context.Context, sourceDir, ref, commitSHA, treeSHA string) error {
	target, exists, err := gitutil.ExactRefTarget(ctx, sourceDir, ref)
	if err != nil {
		return fmt.Errorf("resolve registered candidate ref: %w", err)
	}
	if !exists {
		return fmt.Errorf("registered candidate ref %s does not exist", ref)
	}
	if target != commitSHA {
		return fmt.Errorf("registered candidate ref drifted: got %s, want %s", target, commitSHA)
	}
	actualTree, err := gitutil.Run(ctx, sourceDir, "rev-parse", "--verify", commitSHA+"^{tree}")
	if err != nil {
		return fmt.Errorf("resolve registered candidate tree: %w", err)
	}
	if actualTree != treeSHA {
		return fmt.Errorf("registered candidate tree drifted: got %s, want %s", actualTree, treeSHA)
	}
	return nil
}

func exactCommitAtRef(ctx context.Context, sourceDir, ref string) (string, error) {
	target, exists, err := gitutil.ExactRefTarget(ctx, sourceDir, ref)
	if err != nil {
		return "", fmt.Errorf("resolve candidate base ref: %w", err)
	}
	if !exists {
		return "", fmt.Errorf("candidate base ref %s does not exist", ref)
	}
	commit, err := gitutil.ResolveRef(ctx, sourceDir, ref)
	if err != nil {
		return "", fmt.Errorf("candidate base ref %s is not a commit: %w", ref, err)
	}
	if commit != target {
		return "", fmt.Errorf("candidate base ref %s does not directly name one exact commit", ref)
	}
	return commit, nil
}

func verifyIndependentGitDir(worktree string) error {
	gitDir := filepath.Join(worktree, ".git")
	info, err := os.Lstat(gitDir)
	if err != nil {
		return fmt.Errorf("inspect private candidate Git directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("private candidate does not own an independent Git directory")
	}
	if _, err := os.Lstat(filepath.Join(gitDir, "objects", "info", "alternates")); err == nil {
		return fmt.Errorf("private candidate uses shared Git object storage")
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect private candidate alternates: %w", err)
	}
	return nil
}

func rejectEscapingSymlinks(worktree string) error {
	return filepath.WalkDir(worktree, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink == 0 {
			return nil
		}
		target, err := os.Readlink(path)
		if err != nil {
			return fmt.Errorf("read candidate symlink: %w", err)
		}
		if filepath.IsAbs(target) {
			return fmt.Errorf("candidate symlink %s has an absolute target", path)
		}
		resolved := filepath.Clean(filepath.Join(filepath.Dir(path), target))
		relative, err := filepath.Rel(worktree, resolved)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return fmt.Errorf("candidate symlink %s escapes its disposable view", path)
		}
		return nil
	})
}

func verifyPrivateCandidate(ctx context.Context, worktree, commitSHA, treeSHA string) error {
	head, err := gitutil.ResolveRef(ctx, worktree, "HEAD")
	if err != nil {
		return err
	}
	if head != commitSHA {
		return fmt.Errorf("private candidate HEAD is %s, want %s", head, commitSHA)
	}
	tree, err := gitutil.Run(ctx, worktree, "rev-parse", "--verify", "HEAD^{tree}")
	if err != nil {
		return fmt.Errorf("resolve private candidate tree: %w", err)
	}
	if tree != treeSHA {
		return fmt.Errorf("private candidate tree is %s, want %s", tree, treeSHA)
	}
	return nil
}

func inspectPreparedCandidate(ctx context.Context, prepared *preparedCandidateView) (CandidateSnapshot, error) {
	if err := verifyPrivateCandidate(ctx, prepared.WorktreeDir, prepared.commitSHA, prepared.treeSHA); err != nil {
		return CandidateSnapshot{}, err
	}
	contractRaw, err := gitutil.RunRaw(ctx, prepared.WorktreeDir, "show", prepared.commitSHA+":"+prepared.contractPath)
	if err != nil {
		return CandidateSnapshot{}, fmt.Errorf("read candidate WorkContract: %w", err)
	}
	if sha256HexBytes(contractRaw) != prepared.contractSHA {
		return CandidateSnapshot{}, fmt.Errorf("candidate WorkContract raw-byte SHA-256 drifted")
	}

	trackedClean, indexClean, untrackedClean, err := candidateCleanState(ctx, prepared.WorktreeDir)
	if err != nil {
		return CandidateSnapshot{}, err
	}
	refs, err := gitutil.RunRaw(ctx, prepared.WorktreeDir, "for-each-ref", "--format=%(refname)%00%(objectname)%00%(objecttype)")
	if err != nil {
		return CandidateSnapshot{}, fmt.Errorf("snapshot candidate refs: %w", err)
	}
	replaceRefs, err := gitutil.RunRaw(ctx, prepared.WorktreeDir, "for-each-ref", "--format=%(refname)%00%(objectname)", "refs/replace")
	if err != nil {
		return CandidateSnapshot{}, fmt.Errorf("snapshot candidate replace refs: %w", err)
	}
	config, err := os.ReadFile(filepath.Join(prepared.WorktreeDir, ".git", "config"))
	if err != nil {
		return CandidateSnapshot{}, fmt.Errorf("snapshot candidate config: %w", err)
	}
	return CandidateSnapshot{
		CommitSHA:         prepared.commitSHA,
		TreeSHA:           prepared.treeSHA,
		TrackedClean:      trackedClean,
		IndexClean:        indexClean,
		UntrackedClean:    untrackedClean,
		RefsSHA256:        sha256HexBytes(refs),
		ConfigSHA256:      sha256HexBytes(config),
		ReplaceRefsSHA256: sha256HexBytes(replaceRefs),
	}, nil
}

func candidateCleanState(ctx context.Context, worktree string) (bool, bool, bool, error) {
	raw, err := gitutil.RunRaw(ctx, worktree, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignore-submodules=none")
	if err != nil {
		return false, false, false, fmt.Errorf("inspect candidate cleanliness: %w", err)
	}
	trackedClean, indexClean, untrackedClean := true, true, true
	for _, record := range bytes.Split(raw, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		if len(record) < 2 {
			return false, false, false, fmt.Errorf("malformed candidate status record")
		}
		if bytes.HasPrefix(record, []byte("??")) {
			untrackedClean = false
			continue
		}
		if bytes.HasPrefix(record, []byte("!!")) {
			continue
		}
		if record[0] != ' ' {
			indexClean = false
		}
		if record[1] != ' ' {
			trackedClean = false
		}
	}
	return trackedClean, indexClean, untrackedClean, nil
}

func makeTreeReadOnly(root string) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		mode := info.Mode().Perm() &^ 0o222
		if entry.IsDir() {
			mode |= 0o500
		}
		return os.Chmod(path, mode)
	})
}

func removeCandidateContainer(container string) error {
	// Removing read-only files only requires writable parent directories. Do
	// not chmod files: a hostile hard link must never change a source mode.
	_ = filepath.WalkDir(container, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() && entry.Type()&os.ModeSymlink == 0 {
			_ = os.Chmod(path, 0o700)
		}
		return nil
	})
	return os.RemoveAll(container)
}

func cloneCandidateStepView(view CandidateStepView) CandidateStepView {
	view.WorkContractRaw = bytes.Clone(view.WorkContractRaw)
	return view
}

func sameCandidateStepView(expected, actual CandidateStepView) bool {
	return expected.WorktreeDir == actual.WorktreeDir &&
		expected.ScratchDir == actual.ScratchDir &&
		bytes.Equal(expected.WorkContractRaw, actual.WorkContractRaw)
}

func sha256HexBytes(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func isolatedGitContext(ctx context.Context) context.Context {
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
