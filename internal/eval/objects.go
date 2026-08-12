package eval

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

// Every case pins the same four commits. They are named rather than positional
// so a pool ref reads as evidence of what it keeps alive.
const (
	refHead          = "head"
	refSourceHead    = "source-head"
	refBase          = "base"
	refTrustedConfig = "trusted-config"
	caseRefNamespace = "refs/no-mistakes/eval/"
)

func caseRefNames() []string {
	return []string{refHead, refSourceHead, refBase, refTrustedConfig}
}

func caseRefPrefix(caseID string) string { return caseRefNamespace + caseID + "/" }

// poolDir is the shared object store for every case captured from one source
// repository.
//
// Cases used to each carry a self-contained Git bundle, which is a full copy of
// the repository's history per review pass: ~8 MB per case for a small repo, so
// a machine reviewing a few dozen changes a day accumulated hundreds of
// megabytes a day of near-identical objects. A bundle cannot be trimmed to only
// the interesting commits, because a bundle built with negative refs records
// prerequisites and Git refuses to unbundle it into a repository that lacks
// them - and the whole point of a case is that it restores into an empty
// throwaway gate.
//
// Sharing one pool per repository fixes that at the source instead: the first
// case pays for the history once, and every later case transfers only the
// objects its own commits introduced (measured at ~8 KB). The case keeps its
// records; the pool keeps its objects; the case's refs are what tie them
// together and what keeps those objects alive against Git's own maintenance.
func (s *Store) poolDir(repoFingerprint string) string {
	name := strings.TrimSpace(repoFingerprint)
	if name == "" {
		name = "unknown"
	}
	return filepath.Join(s.root, "pools", name+".git")
}

// storeCaseObjects copies the commits one case replays from the source gate
// into that repository's pool and pins them under the case's ref prefix.
//
// It never writes to the gate: capture is read-only against live pipeline
// state. The reviewed commit is frequently not the tip of any gate ref by the
// time a case is captured (fix rounds advance the branch, and branches get
// deleted after a merge), and asking a remote for a bare object id depends on
// upload-pack's want policy, which is off by default and version-dependent. So
// the commits are named in a throwaway bare clone of the gate - local clones
// hardlink their objects, so this costs no meaningful disk or time - and the
// pool fetches them from there by refspec, which is ordinary supported Git.
func initializeObjectPool(ctx context.Context, pool string) error {
	if err := git.InitBare(ctx, pool); err != nil {
		return fmt.Errorf("create eval object pool: %w", err)
	}
	// Case refs intentionally carry both the case ID and a descriptive commit
	// name. In a normal Windows temp or user-data path their lock files can
	// exceed Git for Windows' legacy MAX_PATH handling even though the
	// filesystem supports the path. Keep this repository-local so capture does
	// not depend on or modify the operator's global Git configuration.
	if _, err := git.Run(ctx, pool, "config", "core.longpaths", "true"); err != nil {
		return fmt.Errorf("enable long paths in eval object pool: %w", err)
	}
	return nil
}

func (s *Store) storeCaseObjects(ctx context.Context, gateDir, repoFingerprint, caseID string, refs map[string]string) error {
	pool := s.poolDir(repoFingerprint)
	if err := initializeObjectPool(ctx, pool); err != nil {
		return err
	}
	// The staging clone goes under the eval root rather than the system temp
	// directory because both live on the same filesystem as the gate, which is
	// what lets Git hardlink its objects instead of copying them. Across
	// filesystems that clone would be a full byte-for-byte copy of the
	// repository on every single capture.
	tmp, err := os.MkdirTemp(s.root, ".objects-")
	if err != nil {
		return fmt.Errorf("create temporary capture directory: %w", err)
	}
	defer os.RemoveAll(tmp)

	clone := filepath.Join(tmp, "source.git")
	cmd := exec.CommandContext(ctx, "git", "clone", "--bare", "--quiet", gateDir, clone)
	shellenv.ConfigureShellCommand(cmd)
	if err := shellenv.RunShellCommand(cmd); err != nil {
		// This error can carry Git's own output, and the gate's remote may
		// embed a credential, so it is redacted like every other git-run error
		// before it can reach a log.
		return fmt.Errorf("stage source gate objects for capture: %s", safeurl.RedactText(err.Error()))
	}
	prefix := caseRefPrefix(caseID)
	for _, name := range caseRefNames() {
		sha := strings.TrimSpace(refs[name])
		if sha == "" {
			return fmt.Errorf("case %q is missing its %s commit", caseID, name)
		}
		if _, err := git.Run(ctx, clone, "update-ref", prefix+name, sha); err != nil {
			return fmt.Errorf("pin case %s commit: %w", name, err)
		}
	}
	// --no-tags keeps the pool's refs to exactly the case pins. A tag ref
	// would pin objects no case needs and survive every retention pass,
	// quietly making the pool unreclaimable.
	if _, err := git.Run(ctx, pool, "fetch", "--no-tags", clone, "+"+prefix+"*:"+prefix+"*"); err != nil {
		return fmt.Errorf("store case objects: %w", err)
	}
	return nil
}

// restoreCaseObjects fetches one case's pinned commits out of the pool into a
// throwaway gate. The pool is a plain local repository, so this is the same
// ordinary fetch the bundle path used, minus the duplicated history.
func restoreCaseObjects(ctx context.Context, poolPath, gateDir, caseID string) error {
	if err := git.ValidateBareRepository(ctx, poolPath); err != nil {
		return fmt.Errorf("case objects are unavailable: %w", err)
	}
	prefix := caseRefPrefix(caseID)
	if _, err := git.Run(ctx, gateDir, "fetch", "--no-tags", poolPath, "+"+prefix+"*:"+prefix+"*"); err != nil {
		return fmt.Errorf("restore case objects: %w", err)
	}
	return nil
}

// dropCaseObjects releases one case's claim on the pool. Deleting the refs
// makes its objects unreachable while any object another case still pins stays
// alive. It is best effort by design - a pool that is already gone means the
// case's objects are gone too, which is exactly the requested state.
func dropCaseObjects(ctx context.Context, poolPath, caseID string) error {
	if _, err := os.Stat(poolPath); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect eval object pool: %w", err)
	}
	if err := git.ValidateBareRepository(ctx, poolPath); err != nil {
		return fmt.Errorf("validate eval object pool: %w", err)
	}
	prefix := caseRefPrefix(caseID)
	var failures []string
	for _, name := range caseRefNames() {
		if _, err := git.Run(ctx, poolPath, "update-ref", "-d", prefix+name); err != nil {
			failures = append(failures, name)
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("release case %q objects: %s", caseID, strings.Join(failures, ", "))
	}
	refs, err := git.Run(ctx, poolPath, "for-each-ref", "--format=%(refname)", caseRefNamespace)
	if err != nil {
		return fmt.Errorf("inspect remaining eval pool pins: %w", err)
	}
	if strings.TrimSpace(refs) == "" {
		if err := os.RemoveAll(poolPath); err != nil {
			return fmt.Errorf("remove empty eval object pool: %w", err)
		}
		return nil
	}
	// --auto respects Git's own maintenance thresholds, so a retention pass
	// stays cheap: it schedules repacking when the pool has actually grown
	// instead of paying a full repack for every dropped case.
	if _, err := git.Run(ctx, poolPath, "gc", "--auto", "--quiet"); err != nil {
		return fmt.Errorf("reclaim eval pool space: %w", err)
	}
	return nil
}
