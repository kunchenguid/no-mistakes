package daemon

import (
	"context"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/worktrees"
)

// HandleCustodyOperation is the sole mutation ingress. Branch locks reserve
// custody and publication identities, an exact generation confirms ownership,
// and archive-before-CAS followed by a fenced stamp releases it. A fresh run
// can then reclaim that same existing lane; no wrapper edits daemon records.
func (m *RunManager) HandleCustodyOperation(ctx context.Context, p *ipc.CustodyOperationParams) (*ipc.CustodyOperationResult, error) {
	if p.Action != "release" && p.Action != "reconcile" && p.Action != "rebind" {
		return nil, fmt.Errorf("unknown custody operation")
	}
	run, err := m.db.GetRun(p.RunID)
	if err != nil || run == nil || run.RepoID != p.RepoID {
		return nil, fmt.Errorf("selected run does not belong to the registered repository")
	}
	custodyBranch, publicationBranch := run.Branch, run.PublishBranch()
	branches := []string{custodyBranch, publicationBranch}
	if p.Action == "rebind" {
		if p.PublicationBranch == "" || strings.HasPrefix(p.PublicationBranch, "-") {
			return nil, fmt.Errorf("an existing publication branch is required")
		}
		if _, err := git.Run(ctx, p.WorkDir, "check-ref-format", "refs/heads/"+p.PublicationBranch); err != nil {
			return nil, fmt.Errorf("invalid publication branch")
		}
		branches = append(branches, p.PublicationBranch)
	}
	var result *ipc.CustodyOperationResult
	action := func() (string, error) {
		if m.shuttingDown.Load() {
			return "", fmt.Errorf("daemon is shutting down")
		}
		repo, err := m.db.GetRepo(p.RepoID)
		if err != nil || repo == nil {
			return "", fmt.Errorf("registered repository is unavailable")
		}
		run, err = m.db.GetRun(p.RunID)
		if err != nil || run == nil {
			return "", fmt.Errorf("selected run is unavailable")
		}
		if run.Branch != custodyBranch || run.PublishBranch() != publicationBranch {
			return "", fmt.Errorf("run binding changed before branch reservation; inspect and retry")
		}
		root, err := git.FindGitRoot(p.WorkDir)
		if err != nil {
			return "", fmt.Errorf("caller is not a Git worktree")
		}
		main, err := git.FindMainRepoRoot(root)
		if err != nil || !samePath(main, repo.WorkingPath) {
			return "", fmt.Errorf("caller worktree does not belong to the registered repository")
		}
		branch, err := git.CurrentBranch(ctx, root)
		if err != nil || branch != run.Branch {
			return "", fmt.Errorf("caller branch does not match the selected custody run")
		}
		head, err := git.HeadSHA(ctx, root)
		if err != nil || head != p.HeadSHA {
			return "", fmt.Errorf("caller HEAD changed; retry from the intended branch")
		}
		runs, err := m.db.GetRunsByRepo(repo.ID)
		if err != nil {
			return "", err
		}
		for _, candidate := range runs {
			if candidate.Branch == branch {
				if candidate.ID != run.ID {
					return "", fmt.Errorf("selected run is not the latest custody generation")
				}
				break
			}
		}
		cfg, err := config.LoadGlobal(m.paths.ConfigFile())
		if err != nil {
			return "", err
		}
		service := &branchsync.Service{DB: m.db, Repo: repo, Paths: m.paths, WorkDir: root, GateDir: m.paths.RepoDir(repo.ID), RemoteTimeout: cfg.BranchSyncRemoteTimeout}
		if p.Action != "rebind" {
			for _, candidate := range runs {
				if !candidate.Status.Terminal() && candidate.ID != run.ID &&
					(candidate.Branch == branch || candidate.PublishBranch() == branch || candidate.Branch == publicationBranch || candidate.PublishBranch() == publicationBranch) {
					return "", fmt.Errorf("a live publication owner reserves this branch; custody was not released")
				}
			}
			// A run cannot be terminal in the DB while its executor still owns
			// worktree cleanup or a Git subprocess.
			m.mu.Lock()
			_, live := m.executors[run.ID]
			m.mu.Unlock()
			if live {
				return "", fmt.Errorf("run executor is still live; wait for it to finish before releasing custody")
			}
			pr, err := m.existingPublicationPR(ctx, repo, run, root, publicationBranch)
			if err != nil {
				return "", err
			}
			if pr == nil || run.PRURL == nil || *run.PRURL == "" || pr.URL != *run.PRURL {
				return "", fmt.Errorf("custody release requires the same recorded open PR; replacement is refused")
			}
			if pr.HeadSHA == "" || pr.HeadSHA != head {
				return "", fmt.Errorf("existing PR head does not equal the caller's published head")
			}
			state := service.ReleasePublished(ctx, run.ID, head, p.Action == "reconcile", func(checkCtx context.Context) error {
				again, err := m.existingPublicationPR(checkCtx, repo, run, root, publicationBranch)
				if err != nil || again == nil || again.URL != *run.PRURL || again.HeadSHA != head {
					return fmt.Errorf("existing PR head changed")
				}
				return nil
			})
			if !state.Recovered {
				return "", fmt.Errorf("%s", state.Error)
			}
			result = &ipc.CustodyOperationResult{RunID: run.ID, State: "released", Branch: branch, HeadSHA: head, PRURL: pr.URL}
			return run.ID, nil
		}
		if p.PublicationBranch == repo.DefaultBranch {
			return "", fmt.Errorf("cannot rebind publication to the default branch")
		}
		if run.Status.Terminal() {
			return "", fmt.Errorf("publication rebinding requires a parked live run")
		}
		pushURL := branchsync.ResolvePushURL(ctx, root, repo)
		bind := func() error {
			fresh, err := m.db.GetRun(run.ID)
			if err != nil || fresh == nil || fresh.PushActive || fresh.Status != run.Status || fresh.HeadSHA != run.HeadSHA {
				return fmt.Errorf("publication is currently in progress")
			}
			run = fresh
			pr, err := m.existingPublicationPR(ctx, repo, run, root, p.PublicationBranch)
			if err != nil || pr == nil {
				return fmt.Errorf("publication requires a verified existing PR")
			}
			remoteCtx, cancel := context.WithTimeout(ctx, cfg.BranchSyncRemoteTimeout)
			ref := "refs/heads/" + p.PublicationBranch
			live, err := git.LsRemote(remoteCtx, root, pushURL, ref)
			cancel()
			if err != nil || live == "" || live != pr.HeadSHA {
				return fmt.Errorf("existing PR and publication branch heads disagree or cannot be verified")
			}
			managed := worktrees.RecordedDir(m.paths, run.WorktreePath(), run.RepoID, run.ID)
			remoteCtx, cancel = context.WithTimeout(ctx, cfg.BranchSyncRemoteTimeout)
			err = git.FetchRemoteRef(remoteCtx, managed, pushURL, ref, live)
			cancel()
			if err != nil {
				return fmt.Errorf("import publication head: %w", err)
			}
			current, err := git.HeadSHA(ctx, managed)
			if err != nil || current != run.HeadSHA {
				return fmt.Errorf("managed HEAD differs from the recorded generation; reconcile it before rebinding")
			}
			if _, err := git.Run(ctx, managed, "merge-base", "--is-ancestor", live, current); err != nil {
				return fmt.Errorf("publication rebinding would rewrite existing PR history; merge its head first")
			}
			// Re-read the provider and remote, not merely the stored PR URL.
			again, err := m.existingPublicationPR(ctx, repo, run, root, p.PublicationBranch)
			if err != nil || again == nil || again.URL != pr.URL || again.HeadSHA != live {
				return fmt.Errorf("PR changed during publication rebinding")
			}
			remoteCtx, cancel = context.WithTimeout(ctx, cfg.BranchSyncRemoteTimeout)
			remoteHead, err := git.LsRemote(remoteCtx, root, pushURL, ref)
			cancel()
			if err != nil || remoteHead != live {
				return fmt.Errorf("publication branch changed during rebinding")
			}
			callerBranch, branchErr := git.CurrentBranch(ctx, root)
			callerHead, headErr := git.HeadSHA(ctx, root)
			if branchErr != nil || headErr != nil || callerBranch != run.Branch || callerHead != p.HeadSHA {
				return fmt.Errorf("caller branch or HEAD changed during rebinding")
			}
			if err := m.db.RebindPublication(repo, run, p.PublicationBranch, pr.URL, branchsync.TargetFingerprint(repo.PushURL())); err != nil {
				return err
			}
			result = &ipc.CustodyOperationResult{RunID: run.ID, State: "confirmed", Branch: p.PublicationBranch, HeadSHA: live, PRURL: pr.URL}
			return nil
		}
		m.mu.Lock()
		executor := m.executors[run.ID]
		m.mu.Unlock()
		if executor == nil {
			return "", fmt.Errorf("live run has no executor; reconcile its lifecycle before rebinding")
		}
		err = executor.WhileParked(bind)
		return run.ID, err
	}
	_, err = m.withBranchLock(run.RepoID, branches[0], action, branches[1:]...)
	if err == nil && result != nil {
		if fresh, readErr := m.db.GetRun(result.RunID); readErr == nil && fresh != nil {
			status := string(fresh.Status)
			m.broadcast(ipc.Event{Type: ipc.EventRunUpdated, RunID: fresh.ID, RepoID: fresh.RepoID, Branch: &fresh.Branch, Status: &status, PRURL: fresh.PRURL})
		}
	}
	return result, err
}

// publicationFinder is the part of a provider that custody needs in order to
// choose which PR to verify. It is its own interface so the identity choice is
// testable without a provider binary, a daemon fixture, or a real repository.
type publicationFinder interface {
	FindPR(ctx context.Context, branch, base string) (*scm.PR, error)
}

// publicationPRIdentity chooses the PR a custody operation must verify. A run
// that rebound publication is bound to the PR it recorded, so that identity
// wins for the branch it was recorded against. Discovery searches by branch
// alone, so a sibling open PR on the same source branch - a second PR at a
// different base, say - can be returned instead, and refusing on that sibling
// would strand custody in a lane whose PR is open at the exact published head.
// Binding already prefers a still-open recorded PR over a discovered sibling,
// and release and reconcile have to agree with it or they read a different PR
// than the one they published to. A rebind names a branch the run is not bound
// to yet, which is a request for that branch's PR, so there discovery stands;
// so it stands for a run that never recorded a destination at all. A failed
// lookup with nothing to fall back on refuses rather than guessing.
func publicationPRIdentity(ctx context.Context, host publicationFinder, run *db.Run, branch string) (*scm.PR, error) {
	recorded := ""
	// PublishBranch is the branch this run's recorded URL was published to, so
	// it is the only branch on which that URL may outrank a fresh lookup. Were
	// the preference keyed on the URL alone, a parked run that already has a PR
	// could never rebind to a different one: the stale identity would be
	// returned, the head proof would compare it against the requested branch,
	// and a valid rebind would refuse.
	if run != nil && run.PRURL != nil && run.PublishBranch() == branch {
		recorded = strings.TrimSpace(*run.PRURL)
	}
	pr, err := host.FindPR(ctx, branch, "")
	if recorded != "" {
		if err == nil && pr != nil && pr.URL != "" && pr.URL == recorded {
			return pr, nil
		}
		return &scm.PR{URL: recorded}, nil
	}
	if err != nil || pr == nil || pr.URL == "" {
		return nil, fmt.Errorf("publication requires an existing open PR on the configured target branch")
	}
	return pr, nil
}

func (m *RunManager) existingPublicationPR(ctx context.Context, repo *db.Repo, run *db.Run, dir, branch string) (*scm.PR, error) {
	if m.publicationPR != nil {
		return m.publicationPR(ctx, repo, run, dir, branch)
	}
	cfg, err := config.LoadGlobal(m.paths.ConfigFile())
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, cfg.BranchSyncRemoteTimeout)
	defer cancel()
	forge, err := forgecontext.Resolve(ctx, cfg.ForgeProfiles, repo.UpstreamURL, repo.ForkURL)
	if err != nil {
		return nil, err
	}
	sctx := &pipeline.StepContext{Ctx: ctx, Repo: repo, Run: run, WorkDir: dir, Config: config.Merge(cfg, &config.RepoConfig{}), ForgeContext: forge, Env: forgeEnvironment(forge).Apply(nil)}
	host, reason := steps.PublicationHost(sctx)
	if host == nil {
		return nil, fmt.Errorf("publication provider unavailable: %s", reason)
	}
	pr, err := publicationPRIdentity(ctx, host, run, branch)
	if err != nil {
		return nil, err
	}
	state, err := host.GetPRState(ctx, pr)
	if err != nil || state != scm.PRStateOpen {
		return nil, fmt.Errorf("publication PR is closed, merged or unverifiable")
	}
	reader, ok := host.(scm.PRHeadReader)
	if !ok {
		return nil, fmt.Errorf("publication provider cannot prove the existing PR head; custody is unchanged")
	}
	pr.HeadSHA, err = reader.GetPRHeadSHA(ctx, pr, branch)
	if err != nil || pr.HeadSHA == "" {
		return nil, fmt.Errorf("existing PR head is unverifiable; custody is unchanged")
	}
	return pr, nil
}
