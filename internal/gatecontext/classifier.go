// Package gatecontext owns the authoritative classification of callers that
// are executing inside an active no-mistakes validation step.
package gatecontext

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const ErrorCode = "nested_gate_context"

// RefusalMessage is the stable, privacy-safe error contract used at daemon
// ingress and non-AXI command boundaries.
func RefusalMessage(result Result) string {
	target := "an active no-mistakes validation step"
	if result.RunID != "" && result.Phase != "" {
		target = fmt.Sprintf("no-mistakes run %s, phase %s", result.RunID, result.Phase)
	} else if result.RunID != "" {
		target = "no-mistakes run " + result.RunID
	}
	return fmt.Sprintf("%s: refusing pipeline control from %s; return control to the outer executor", ErrorCode, target)
}

// Request contains the independently observable signals for one caller.
// MarkerPresent is diagnostic only. SkipManagedGit is reserved for managed Git
// hooks, whose cwd is necessarily the gate itself and therefore cannot identify
// the process that initiated the push.
type Request struct {
	CWD            string
	PeerPID        int
	DaemonPID      int
	MarkerPresent  bool
	SkipManagedGit bool
}

// Result is safe to render to an agent. It intentionally carries no paths or
// process details.
type Result struct {
	Nested           bool
	ManagedGit       bool
	AgentDescendant  bool
	DaemonDescendant bool
	MarkerPresent    bool
	RunID            string
	Phase            types.StepName
}

// Inspector combines canonical managed Git identity with authenticated IPC
// peer ancestry. ParentPID exists for deterministic tests; production callers
// leave it nil to use the platform process table.
type Inspector struct {
	DB        *db.DB
	Paths     *paths.Paths
	ParentPID func(int) (int, error)
}

// Inspect classifies a caller without mutating repositories, runs, refs,
// remotes, worktrees, or the database.
func (i Inspector) Inspect(ctx context.Context, req Request) (Result, error) {
	result := Result{MarkerPresent: req.MarkerPresent}
	if i.Paths == nil {
		return result, fmt.Errorf("gate execution context: paths are required")
	}

	var managedRepoID string
	var worktreeRoot string
	if !req.SkipManagedGit && strings.TrimSpace(req.CWD) != "" {
		commonDir, top, ok := gitIdentity(ctx, req.CWD)
		if ok {
			worktreeRoot = top
			id, managed, err := i.registeredManagedCommonDir(commonDir)
			if err != nil {
				return result, err
			}
			if managed {
				managedRepoID = id
				result.ManagedGit = true
				result.Nested = true
			}
		}
	}

	active, err := i.activeAgentSteps()
	if err != nil {
		return result, err
	}
	if req.PeerPID > 0 && (len(active) > 0 || req.DaemonPID > 0) {
		parent := i.ParentPID
		if parent == nil {
			parent = processParentPID
		}
		chain, err := ancestry(req.PeerPID, parent)
		if err != nil {
			return result, fmt.Errorf("gate execution context: inspect authenticated peer ancestry: %w", err)
		}
		if req.DaemonPID > 0 && req.PeerPID != req.DaemonPID && chain[req.DaemonPID] {
			result.Nested = true
			result.DaemonDescendant = true
		}
		var matches []activeAgentStep
		for _, step := range active {
			if step.agentPID > 0 && chain[step.agentPID] {
				matches = append(matches, step)
			}
		}
		if len(matches) > 0 {
			result.Nested = true
			result.AgentDescendant = true
		}
		// A persistent adapter server can be shared by concurrent steps. Name
		// the enclosing run only when authenticated ancestry identifies exactly
		// one active phase; refusal itself does not depend on this metadata.
		if len(matches) == 1 {
			result.RunID = matches[0].runID
			result.Phase = matches[0].phase
		}
	}

	// Canonical topology remains authoritative when a marker is removed or an
	// adapter omits it. Attach run/phase only when the worktree path and active
	// DB record agree exactly; otherwise omit them rather than guessing.
	if result.ManagedGit && result.RunID == "" && managedRepoID != "" && worktreeRoot != "" {
		for _, step := range active {
			if step.repoID != managedRepoID {
				continue
			}
			want := i.Paths.WorktreeDir(step.repoID, step.runID)
			if sameCanonicalPath(worktreeRoot, want) {
				result.RunID = step.runID
				result.Phase = step.phase
				break
			}
		}
	}
	return result, nil
}

type activeAgentStep struct {
	runID    string
	repoID   string
	phase    types.StepName
	agentPID int
}

func (i Inspector) activeAgentSteps() ([]activeAgentStep, error) {
	if i.DB == nil {
		return nil, nil
	}
	runs, err := i.DB.GetActiveRuns()
	if err != nil {
		return nil, fmt.Errorf("gate execution context: list active runs: %w", err)
	}
	var out []activeAgentStep
	for _, run := range runs {
		steps, err := i.DB.GetStepsByRun(run.ID)
		if err != nil {
			return nil, fmt.Errorf("gate execution context: list steps for active run: %w", err)
		}
		for _, step := range steps {
			if !activeStepStatus(step.Status) {
				continue
			}
			pid := 0
			if step.AgentPID != nil {
				pid = *step.AgentPID
			}
			out = append(out, activeAgentStep{runID: run.ID, repoID: run.RepoID, phase: step.StepName, agentPID: pid})
		}
	}
	return out, nil
}

func activeStepStatus(status types.StepStatus) bool {
	switch status {
	case types.StepStatusRunning, types.StepStatusFixing, types.StepStatusAwaitingApproval, types.StepStatusFixReview:
		return true
	default:
		return false
	}
}

func (i Inspector) registeredManagedCommonDir(commonDir string) (string, bool, error) {
	common := canonicalPath(commonDir)
	reposDir := canonicalPath(i.Paths.ReposDir())
	if filepath.Dir(common) != reposDir {
		return "", false, nil
	}
	base := filepath.Base(common)
	if !strings.HasSuffix(base, ".git") || base == ".git" || !git.LooksLikeBareRepository(common) {
		return "", false, nil
	}
	id := strings.TrimSuffix(base, ".git")
	if i.DB == nil {
		return "", false, nil
	}
	repo, err := i.DB.GetRepo(id)
	if err != nil {
		return "", false, fmt.Errorf("gate execution context: verify managed gate: %w", err)
	}
	if repo == nil || !sameCanonicalPath(common, i.Paths.RepoDir(repo.ID)) {
		return "", false, nil
	}
	return repo.ID, true, nil
}

func gitIdentity(ctx context.Context, cwd string) (commonDir, top string, ok bool) {
	common, err := git.Run(ctx, cwd, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", "", false
	}
	root, err := git.Run(ctx, cwd, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", "", false
	}
	return strings.TrimSpace(common), strings.TrimSpace(root), true
}

func ancestry(pid int, parent func(int) (int, error)) (map[int]bool, error) {
	chain := make(map[int]bool)
	for depth := 0; pid > 0 && depth < 256; depth++ {
		if chain[pid] {
			break
		}
		chain[pid] = true
		if pid == 1 {
			break
		}
		next, err := parent(pid)
		if err != nil {
			return nil, err
		}
		pid = next
	}
	return chain, nil
}

func canonicalPath(path string) string {
	path = filepath.Clean(path)
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return filepath.Clean(path)
}

func sameCanonicalPath(a, b string) bool {
	return canonicalPath(a) == canonicalPath(b)
}

// MarkerPresent reports the coarse diagnostic marker emitted by adapters.
// Classification never rejects on this signal alone.
func MarkerPresent() bool {
	_, ok := os.LookupEnv(agent.GateRoleEnvVar)
	return ok
}
