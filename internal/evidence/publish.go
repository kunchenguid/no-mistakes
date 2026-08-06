package evidence

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
)

const (
	// maxFiles and maxTotalBytes bound one run's publication. Evidence is
	// agent-produced, so a runaway recording or a stray model download must
	// fail the publish closed rather than push gigabytes into the repository.
	maxFiles      = 500
	maxTotalBytes = int64(256 << 20)
	maxFileBytes  = int64(64 << 20)

	// fetchRef is a scratch ref used to bring the remote evidence tip into the
	// local object store. Only the resolved sha is used afterwards, so a
	// concurrent run overwriting the ref cannot redirect this publish.
	fetchRef = "refs/no-mistakes/evidence-fetch"

	scratchIndexName = "no-mistakes-evidence-index"
)

// Request describes one publication of a run's evidence directory.
type Request struct {
	// RepoDir is any non-bare worktree (or bare gate dir) of the repository.
	// Objects are written into its store and pushed from there; nothing is
	// checked out, so a detached HEAD or a shallow clone is fine - the orphan
	// branch shares no history with the shallow code branches.
	RepoDir string
	// PushURL is the remote the evidence branch is pushed to. It is the same
	// target the pipeline pushes the code branch to, so fork contributions
	// keep their evidence in the fork the PR head lives in.
	PushURL string
	// Branch is the evidence branch name (already normalized).
	Branch string
	// Dir is an optional directory prefix inside the evidence branch.
	Dir string
	// Segments are further in-branch directory segments below Dir, typically
	// the slugged code-branch name.
	Segments []string
	// SourceDir is the local directory holding the run's evidence files.
	SourceDir string
	// Message is the evidence commit message.
	Message string
	// ForbiddenBranches names branches that must never receive evidence (the
	// run's own branch and the repository default branch). The marker check
	// already refuses them; this turns the refusal into a message that names
	// the misconfiguration.
	ForbiddenBranches []string
}

// Result describes what a publication landed on the evidence branch.
type Result struct {
	// Branch is the evidence branch that now holds the files.
	Branch string
	// CommitSHA is the evidence commit to build stable links against.
	CommitSHA string
	// Dir is the in-branch directory the run's files live under, "" at root.
	Dir string
	// Files are the published paths relative to Dir, slash separated.
	Files []string
}

// Publish copies every regular file under req.SourceDir onto the evidence
// branch and pushes it.
//
// The commit is built with plumbing against a scratch index, so the caller's
// worktree, index, and HEAD are never touched. The new commit's parent is the
// tip that was just fetched from the remote, which makes the push a plain
// fast-forward: evidence is only ever appended, and this never force-pushes.
//
// It fails closed rather than guessing: an unreadable remote, a branch that
// exists without the marker file, a lost push race, or a refused push (no
// permission, protected ref) all return an error and publish nothing. The
// caller then leaves the PR body pointing at local paths instead of links that
// would not resolve.
func Publish(ctx context.Context, req Request) (*Result, error) {
	branch, err := NormalizeBranch(req.Branch)
	if err != nil {
		return nil, err
	}
	for _, forbidden := range req.ForbiddenBranches {
		if strings.TrimSpace(forbidden) != "" && strings.EqualFold(strings.TrimSpace(forbidden), branch) {
			return nil, fmt.Errorf("evidence branch %q is also a code branch of this repository; configure a different test.evidence.branch", branch)
		}
	}
	if strings.TrimSpace(req.PushURL) == "" {
		return nil, fmt.Errorf("no push target for evidence branch %q", branch)
	}

	dir := normalizeInBranchDir(req.Dir, req.Segments)
	files, err := collectFiles(req.SourceDir)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, nil
	}

	// One retry covers a concurrent run that advanced the evidence branch
	// between the fetch and the push. A second loss is a real conflict, and
	// resolving it by force would be the one thing this branch must never do.
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		result, err := publishOnce(ctx, req, branch, dir, files)
		if err == nil {
			return result, nil
		}
		lastErr = err
		if !isPushRace(err) {
			return nil, err
		}
	}
	return nil, lastErr
}

func publishOnce(ctx context.Context, req Request, branch, dir string, files []collectedFile) (*Result, error) {
	tip, err := remoteTip(ctx, req.RepoDir, req.PushURL, branch)
	if err != nil {
		return nil, err
	}
	if tip != "" {
		if err := assertEvidenceBranch(ctx, req.RepoDir, tip, branch); err != nil {
			return nil, err
		}
	}

	indexDir, err := os.MkdirTemp("", scratchIndexName)
	if err != nil {
		return nil, fmt.Errorf("create evidence index: %w", err)
	}
	defer os.RemoveAll(indexDir)
	env := []string{"GIT_INDEX_FILE=" + filepath.Join(indexDir, "index")}

	if tip != "" {
		if _, err := git.RunWithEnv(ctx, req.RepoDir, env, "read-tree", tip); err != nil {
			return nil, fmt.Errorf("read evidence branch tree: %w", err)
		}
	}
	if err := addMarker(ctx, req.RepoDir, env, indexDir); err != nil {
		return nil, err
	}

	published := make([]string, 0, len(files))
	for _, file := range files {
		inBranch := path.Join(dir, file.Rel)
		blob, err := git.RunWithEnv(ctx, req.RepoDir, env, "hash-object", "-w", "--", file.Abs)
		if err != nil {
			return nil, fmt.Errorf("hash evidence file %s: %w", file.Rel, err)
		}
		if err := addToIndex(ctx, req.RepoDir, env, file.Mode, blob, inBranch); err != nil {
			return nil, err
		}
		published = append(published, file.Rel)
	}

	tree, err := git.RunWithEnv(ctx, req.RepoDir, env, "write-tree")
	if err != nil {
		return nil, fmt.Errorf("write evidence tree: %w", err)
	}

	if tip != "" {
		tipTree, err := git.Run(ctx, req.RepoDir, "rev-parse", "--verify", tip+"^{tree}")
		if err == nil && tipTree == tree {
			// The branch already holds exactly these bytes (a re-run of the
			// same step). Link against the existing commit instead of pushing
			// an empty one.
			return &Result{Branch: branch, CommitSHA: tip, Dir: dir, Files: published}, nil
		}
	}

	commit, err := commitTree(ctx, req.RepoDir, tree, tip, req.Message)
	if err != nil {
		return nil, err
	}
	if _, err := git.Run(ctx, req.RepoDir, "push", req.PushURL, commit+":refs/heads/"+branch); err != nil {
		return nil, fmt.Errorf("push evidence branch %s to %s: %w", branch, safeurl.Redact(req.PushURL), err)
	}
	return &Result{Branch: branch, CommitSHA: commit, Dir: dir, Files: published}, nil
}

// remoteTip returns the remote evidence branch head, fetched into the local
// object store, or "" when the branch does not exist yet. A failure to read
// the remote is an error: publishing onto a branch whose current state is
// unknown is exactly the case that must not proceed.
func remoteTip(ctx context.Context, repoDir, pushURL, branch string) (string, error) {
	out, err := git.Run(ctx, repoDir, "ls-remote", "--heads", pushURL, "refs/heads/"+branch)
	if err != nil {
		return "", fmt.Errorf("read evidence branch %s on %s: %w", branch, safeurl.Redact(pushURL), err)
	}
	if strings.TrimSpace(out) == "" {
		return "", nil
	}
	if _, err := git.Run(ctx, repoDir, "fetch", "--no-tags", "--force", pushURL, "+refs/heads/"+branch+":"+fetchRef); err != nil {
		return "", fmt.Errorf("fetch evidence branch %s: %w", branch, err)
	}
	sha, err := git.Run(ctx, repoDir, "rev-parse", "--verify", fetchRef+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolve fetched evidence branch %s: %w", branch, err)
	}
	return sha, nil
}

// assertEvidenceBranch refuses a branch that exists but is not a no-mistakes
// evidence branch. This is the guard that makes a wrong configured name
// harmless: "main" has no marker file, so evidence is never appended to it.
func assertEvidenceBranch(ctx context.Context, repoDir, tip, branch string) error {
	if _, err := git.Run(ctx, repoDir, "cat-file", "-e", tip+":"+MarkerPath); err != nil {
		return fmt.Errorf("refusing to publish evidence: branch %q already exists and is not a no-mistakes evidence branch (no %s marker at its tip)", branch, MarkerPath)
	}
	return nil
}

func addMarker(ctx context.Context, repoDir string, env []string, indexDir string) error {
	markerFile := filepath.Join(indexDir, "marker")
	if err := os.WriteFile(markerFile, []byte(MarkerContent), 0o644); err != nil {
		return fmt.Errorf("write evidence marker: %w", err)
	}
	blob, err := git.RunWithEnv(ctx, repoDir, env, "hash-object", "-w", "--", markerFile)
	if err != nil {
		return fmt.Errorf("hash evidence marker: %w", err)
	}
	return addToIndex(ctx, repoDir, env, "100644", blob, MarkerPath)
}

func addToIndex(ctx context.Context, repoDir string, env []string, mode, blob, inBranchPath string) error {
	if _, err := git.RunWithEnv(ctx, repoDir, env, "update-index", "--add", "--cacheinfo", mode+","+blob+","+inBranchPath); err != nil {
		return fmt.Errorf("add %s to evidence index: %w", inBranchPath, err)
	}
	return nil
}

// commitTree builds the evidence commit. The identity falls back to a
// no-mistakes author when the repository configures none, so publishing never
// fails on "unable to auto-detect email address".
func commitTree(ctx context.Context, repoDir, tree, parent, message string) (string, error) {
	name := configuredIdentity(ctx, repoDir, "user.name", "no-mistakes")
	email := configuredIdentity(ctx, repoDir, "user.email", "no-mistakes@localhost")
	env := []string{
		"GIT_AUTHOR_NAME=" + name,
		"GIT_AUTHOR_EMAIL=" + email,
		"GIT_COMMITTER_NAME=" + name,
		"GIT_COMMITTER_EMAIL=" + email,
	}
	args := []string{"commit-tree", tree}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	if strings.TrimSpace(message) == "" {
		message = "no-mistakes: test evidence"
	}
	args = append(args, "-m", message)
	commit, err := git.RunWithEnv(ctx, repoDir, env, args...)
	if err != nil {
		return "", fmt.Errorf("create evidence commit: %w", err)
	}
	return commit, nil
}

func configuredIdentity(ctx context.Context, repoDir, key, fallback string) string {
	value, err := git.Run(ctx, repoDir, "config", "--get", "--default", "", key)
	if err != nil || strings.TrimSpace(value) == "" {
		return fallback
	}
	return strings.TrimSpace(value)
}

// isPushRace reports whether the failure looks like another writer advancing
// the evidence branch, which is worth exactly one rebuild-and-retry. A
// permission or protected-branch refusal is not, and must surface immediately.
func isPushRace(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "non-fast-forward") ||
		strings.Contains(text, "fetch first") ||
		strings.Contains(text, "stale info")
}

type collectedFile struct {
	Abs  string
	Rel  string
	Mode string
}

// collectFiles lists the regular files under root in a deterministic order.
// Symlinks are skipped: an evidence symlink would publish a pointer to a path
// that only exists on the daemon host.
func collectFiles(root string) ([]collectedFile, error) {
	if strings.TrimSpace(root) == "" {
		return nil, nil
	}
	var files []collectedFile
	var total int64
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.Size() > maxFileBytes {
			return fmt.Errorf("evidence file %s is %d bytes, over the %d byte limit", d.Name(), info.Size(), maxFileBytes)
		}
		total += info.Size()
		if total > maxTotalBytes {
			return fmt.Errorf("evidence exceeds the %d byte publication limit", maxTotalBytes)
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return nil
		}
		slashed := filepath.ToSlash(rel)
		if !safeInBranchPath(slashed) {
			return nil
		}
		mode := "100644"
		if info.Mode()&0o111 != 0 {
			mode = "100755"
		}
		files = append(files, collectedFile{Abs: p, Rel: slashed, Mode: mode})
		if len(files) > maxFiles {
			return fmt.Errorf("evidence has more than %d files", maxFiles)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Rel < files[j].Rel })
	return files, nil
}

// safeInBranchPath rejects paths Git cannot carry verbatim or that would
// escape the run's directory once joined into the branch.
func safeInBranchPath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") {
		return false
	}
	if path.Clean(p) != p {
		return false
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	for _, component := range strings.Split(p, "/") {
		if component == "" || component == "." || component == ".." {
			return false
		}
	}
	return true
}

// normalizeInBranchDir builds the in-branch directory from the configured
// prefix and the extra segments, dropping anything that would escape the
// branch root.
func normalizeInBranchDir(dir string, segments []string) string {
	var parts []string
	for _, raw := range append(strings.Split(filepath.ToSlash(strings.TrimSpace(dir)), "/"), segments...) {
		component := strings.TrimSpace(raw)
		if component == "" || component == "." || component == ".." || strings.Contains(component, "/") {
			continue
		}
		parts = append(parts, component)
	}
	joined := path.Join(parts...)
	if joined == "." || !safeInBranchPath(joined) {
		return ""
	}
	return joined
}
