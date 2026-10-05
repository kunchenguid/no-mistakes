package custody

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

var rescueIdentifier = regexp.MustCompile(`^[A-Za-z0-9_-]+([.][A-Za-z0-9_-]+)*$`)
var rescueObjectID = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// PreservePartialWork requires quiescent writers. It uses raw blobs and private
// indexes: no filters, signing, hooks, shared stash, live index or HEAD writes.
// Unsupported states get best-effort content storage but require retention.
func PreservePartialWork(ctx context.Context, dir, runID, step, stopID string) (*types.PartialWork, error) {
	p := &types.PartialWork{Version: 1, RunID: runID, Step: step, StopID: stopID, State: "retained", Path: dir}
	for _, id := range []string{runID, step, stopID} {
		if !rescueIdentifier.MatchString(id) {
			return p, fmt.Errorf("invalid rescue identifier %q", id)
		}
	}
	p.Ref = "refs/no-mistakes/rescue/" + runID + "/" + step + "/" + stopID
	indexTree, tree, err := capturePartialWork(ctx, dir, p)
	if err != nil {
		return p, err
	}
	headTree, err := git.Run(ctx, dir, "rev-parse", p.ParentHead+"^{tree}")
	if err != nil {
		return p, err
	}
	if p.Reason == "" && tree == headTree && indexTree == headTree {
		p.State = "settled"
		p.Ref = ""
		p.IndexSHA = ""
		p.Path = ""
		return p, nil
	}
	// Stop identity records time. Fixed object dates make exact recapture
	// idempotent even when cleanup retries later or inherited dates change.
	identity := []string{"GIT_OPTIONAL_LOCKS=0", "GIT_AUTHOR_NAME=no-mistakes", "GIT_AUTHOR_EMAIL=local@no-mistakes.invalid", "GIT_COMMITTER_NAME=no-mistakes", "GIT_COMMITTER_EMAIL=local@no-mistakes.invalid", "GIT_AUTHOR_DATE=1970-01-01T00:00:00Z", "GIT_COMMITTER_DATE=1970-01-01T00:00:00Z"}
	commit := func(tree string, parents ...string) (string, error) {
		args := []string{"-c", "commit.gpgsign=false", "commit-tree", tree}
		for _, parent := range parents {
			args = append(args, "-p", parent)
		}
		args = append(args, "-m", "no-mistakes partial work "+runID+"/"+step+"/"+stopID)
		return git.RunWithEnv(ctx, dir, identity, args...)
	}
	p.IndexSHA, err = commit(indexTree, p.ParentHead)
	if err != nil {
		return p, err
	}
	p.SHA, err = commit(tree, p.ParentHead, p.IndexSHA)
	if err != nil {
		return p, err
	}
	if err = PreserveRecoveryAnchor(ctx, dir, p.Ref, p.SHA); err != nil {
		return p, err
	}
	if err = ValidatePartialWork(ctx, dir, p); err != nil {
		return p, err
	}
	if p.Reason == "" {
		p.State = "saved"
		p.Path = ""
	}
	return p, nil
}

func PartialWorkUnchanged(ctx context.Context, dir string, p *types.PartialWork) (bool, error) {
	if err := ValidatePartialWork(ctx, dir, p); err != nil {
		return false, err
	}
	current := &types.PartialWork{}
	indexTree, tree, err := capturePartialWork(ctx, dir, current)
	if err != nil {
		return false, err
	}
	if current.Reason != "" || current.ParentHead != p.ParentHead {
		return false, nil
	}
	savedIndex, err := git.Run(ctx, dir, "rev-parse", p.IndexSHA+"^{tree}")
	if err != nil {
		return false, err
	}
	savedWork, err := git.Run(ctx, dir, "rev-parse", p.SHA+"^{tree}")
	if err != nil {
		return false, err
	}
	return indexTree == savedIndex && tree == savedWork, nil
}

func capturePartialWork(ctx context.Context, dir string, p *types.PartialWork) (string, string, error) {
	head, err := git.HeadSHA(ctx, dir)
	if err != nil {
		return "", "", err
	}
	p.ParentHead = head
	gitDir, err := git.Run(ctx, dir, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return "", "", err
	}
	scratch, err := os.MkdirTemp(gitDir, "rescue-index-")
	if err != nil {
		return "", "", err
	}
	defer os.RemoveAll(scratch)
	indexEnv := []string{"GIT_INDEX_FILE=" + filepath.Join(scratch, "index"), "GIT_OPTIONAL_LOCKS=0"}
	run := func(args ...string) (string, error) {
		return git.RunWithEnv(ctx, dir, indexEnv, append([]string{"-c", "core.fsmonitor=false"}, args...)...)
	}
	// Stage-zero entries preserve staged content. Conflict entries are omitted
	// only from the best-effort snapshot; the original index must then survive.
	entries, err := git.RunRaw(ctx, dir, "-c", "core.fsmonitor=false", "ls-files", "--stage", "-z")
	if err != nil {
		return "", "", err
	}
	if _, err = run("read-tree", "--empty"); err != nil {
		return "", "", err
	}
	add := func(mode, blob, path string) error {
		_, e := run("update-index", "--add", "--cacheinfo", mode, blob, path)
		return e
	}
	indexModes := map[string]string{}
	for _, entry := range strings.Split(string(entries), "\x00") {
		if entry == "" {
			continue
		}
		parts := strings.SplitN(entry, "\t", 2)
		if len(parts) != 2 {
			return "", "", fmt.Errorf("invalid index entry")
		}
		fields := strings.Fields(parts[0])
		if len(fields) != 3 {
			return "", "", fmt.Errorf("invalid index metadata")
		}
		if fields[2] != "0" {
			p.Reason = "unmerged index"
			continue
		}
		if fields[0] == "160000" {
			p.Reason = "nested repository or submodule requires original checkout"
		}
		indexModes[parts[1]] = fields[0]
		if err = add(fields[0], fields[1], parts[1]); err != nil {
			return "", "", err
		}
	}
	indexTree, err := run("write-tree")
	if err != nil {
		return "", "", err
	}
	if _, err = run("read-tree", "--empty"); err != nil {
		return "", "", err
	}
	files, err := git.RunRaw(ctx, dir, "-c", "core.fsmonitor=false", "ls-files", "--cached", "--others", "--exclude-standard", "-z")
	if err != nil {
		return "", "", err
	}
	filemode, err := git.Run(ctx, dir, "config", "--type=bool", "--default", "true", "--get", "core.filemode")
	if err != nil {
		return "", "", err
	}
	symlinks, err := git.Run(ctx, dir, "config", "--type=bool", "--default", "true", "--get", "core.symlinks")
	if err != nil {
		return "", "", err
	}
	seen := map[string]bool{}
	for _, name := range strings.Split(string(files), "\x00") {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		components := strings.Split(filepath.FromSlash(name), string(filepath.Separator))
		parent := dir
		for _, component := range components[:len(components)-1] {
			parent = filepath.Join(parent, component)
			info, e := os.Lstat(parent)
			if os.IsNotExist(e) {
				break
			}
			if e != nil {
				return "", "", e
			}
			if !info.IsDir() {
				return "", "", fmt.Errorf("unsupported ancestor path %s requires original checkout", parent)
			}
		}
		abs := filepath.Join(dir, filepath.FromSlash(name))
		info, e := os.Lstat(abs)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return "", "", e
		}
		mode := "100644"
		content := abs
		if info.Mode()&os.ModeSymlink != 0 {
			target, e := os.Readlink(abs)
			if e != nil {
				return "", "", e
			}
			content = filepath.Join(scratch, "link")
			if e = os.WriteFile(content, []byte(target), 0o600); e != nil {
				return "", "", e
			}
			mode = "120000"
		} else if info.IsDir() {
			p.Reason = "nested repository or submodule requires original checkout"
			continue
		} else if !info.Mode().IsRegular() {
			p.Reason = "unsupported file type"
			continue
		} else if symlinks == "false" && indexModes[name] == "120000" {
			mode = "120000"
		} else if (filemode == "false" && indexModes[name] == "100755") || (filemode != "false" && info.Mode()&0o111 != 0) || (indexModes[name] == "" && info.Mode()&0o111 != 0) {
			mode = "100755"
		}
		blob, e := run("hash-object", "-w", "--no-filters", "--", content)
		if e != nil {
			return "", "", e
		}
		if e = add(mode, blob, name); e != nil {
			return "", "", e
		}
	}
	ignored, err := git.RunRaw(ctx, dir, "-c", "core.fsmonitor=false", "ls-files", "--others", "--ignored", "--exclude-standard", "-z")
	if err != nil {
		return "", "", err
	}
	if len(ignored) > 0 {
		p.Reason = "ignored files require original checkout"
	}
	if unfinished, e := GitOperationInProgress(ctx, dir); e != nil {
		return "", "", e
	} else if unfinished {
		p.Reason = "unfinished Git operation"
	}
	tree, err := run("write-tree")
	if err != nil {
		return "", "", err
	}
	return indexTree, tree, nil
}

// ValidatePartialWork binds the exact non-symbolic ref and both parents.
func ValidatePartialWork(ctx context.Context, dir string, p *types.PartialWork) error {
	if p == nil || p.Version != 1 || p.Ref != "refs/no-mistakes/rescue/"+p.RunID+"/"+p.Step+"/"+p.StopID {
		return fmt.Errorf("unknown or cross-bound rescue")
	}
	for _, id := range []string{p.RunID, p.Step, p.StopID} {
		if !rescueIdentifier.MatchString(id) {
			return fmt.Errorf("invalid rescue identity")
		}
	}
	for _, sha := range []string{p.SHA, p.ParentHead, p.IndexSHA} {
		if !rescueObjectID.MatchString(sha) {
			return fmt.Errorf("invalid rescue object ID")
		}
	}
	if target, e := git.Run(ctx, dir, "symbolic-ref", "-q", p.Ref); e == nil {
		return fmt.Errorf("symbolic rescue ref %s targets %s", p.Ref, target)
	}
	sha, e := git.Run(ctx, dir, "rev-parse", "--verify", p.Ref+"^{commit}")
	if e != nil {
		return e
	}
	if sha != p.SHA {
		return fmt.Errorf("rescue ref %s moved", p.Ref)
	}
	parents, e := git.Run(ctx, dir, "show", "-s", "--format=%P", p.SHA)
	if e != nil {
		return e
	}
	if parents != p.ParentHead+" "+p.IndexSHA {
		return fmt.Errorf("rescue parent mismatch")
	}
	parent, e := git.Run(ctx, dir, "show", "-s", "--format=%P", p.IndexSHA)
	if e != nil {
		return e
	}
	if parent != p.ParentHead {
		return fmt.Errorf("rescue index parent mismatch")
	}
	return nil
}
