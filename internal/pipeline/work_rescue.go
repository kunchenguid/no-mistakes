package pipeline

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/procreap"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

var ErrWorkRetained = errors.New("partial work requires retention")

func (sctx *StepContext) CheckWorkRescue() error {
	if sctx == nil || sctx.DB == nil || sctx.Run == nil || sctx.WorkDir == "" {
		return nil
	}
	p, err := sctx.DB.LatestWorkRescue(sctx.Run.ID)
	if err != nil {
		return fmt.Errorf("%w: cannot read partial work: %v", ErrWorkRetained, err)
	}
	if p == nil {
		return nil
	}
	if p.State == "saved" {
		if err := custody.ValidatePartialWork(sctx.Ctx, sctx.WorkDir, p); err != nil {
			return fmt.Errorf("%w: cannot verify partial work: %v", ErrWorkRetained, err)
		}
		return nil
	}
	return fmt.Errorf("%w at %s: %s (%s)", ErrWorkRetained, p.Path, p.State, p.Reason)
}

func (sctx *StepContext) beginAgentRescue(opts agent.RunOpts) (*types.PartialWork, error) {
	if sctx == nil || sctx.DB == nil || sctx.Run == nil || sctx.WorkDir == "" {
		return nil, nil
	}
	if err := sctx.CheckWorkRescue(); err != nil {
		return nil, err
	}
	head, err := git.HeadSHA(sctx.Ctx, sctx.WorkDir)
	if err != nil {
		return nil, err
	}
	step := "agent"
	if sctx.StepResultID != "" {
		s, e := sctx.DB.GetStepResult(sctx.StepResultID)
		if e != nil {
			return nil, e
		}
		if s != nil {
			step = string(s.StepName)
		}
	} else if purpose := strings.Split(opts.Purpose, "-")[0]; purpose != "" {
		step = purpose
	}
	selection := sha256.Sum256([]byte(sctx.PreviousFindings))
	return sctx.DB.BeginWorkRescue(sctx.Run, step, fmt.Sprintf("%x", selection), head, sctx.WorkDir)
}

func (sctx *StepContext) finishAgentRescue(p *types.PartialWork, invocationErr error, activity *agentActivity) error {
	if p == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if invocationErr == nil {
		unfinished, err := custody.GitOperationInProgress(ctx, sctx.WorkDir)
		if err == nil && !unfinished {
			p.State = "settled"
			p.Reason = ""
			p.Path = ""
			return sctx.DB.SaveWorkRescue(p)
		}
		invocationErr = errors.Join(errors.New("agent left an unfinished Git operation"), err)
	}
	activity.mu.Lock()
	quiescent := activity.launched && activity.exited
	activity.mu.Unlock()
	return PreserveRunWork(ctx, sctx.DB, sctx.Run, sctx.WorkDir, p, invocationErr.Error(), quiescent)
}

// PreserveRunWork is the shared stopped-invocation and final-deletion backstop.
// A saved ref is local evidence. A retained state refuses removal and editing.
func PreserveRunWork(ctx context.Context, d *db.DB, run *db.Run, dir string, p *types.PartialWork, reason string, quiescent bool) error {
	if run == nil {
		return fmt.Errorf("cannot preserve worktree %s without a run", dir)
	}
	sweepErr := procreap.Quiesce(ctx, procreap.Options{Worktrees: []procreap.Worktree{{Dir: dir, RepoID: run.RepoID, RunID: run.ID}}, Scopes: []string{dir}})
	var inspectErr error
	if p == nil {
		recorded, err := d.GetRun(run.ID)
		if err != nil || recorded == nil || recorded.HeadSHA == "" {
			return fmt.Errorf("cannot read recorded head; retained %s: %v", dir, err)
		}
		head, err := git.HeadSHA(ctx, dir)
		if err != nil {
			return err
		}
		if recorded.HeadSHA != head {
			inspectErr = fmt.Errorf("cleanup HEAD changed from recorded head %s to %s; retain for continuity reconciliation", recorded.HeadSHA, head)
		}
		if sweepErr == nil && quiescent && inspectErr == nil {
			previous, err := d.LatestWorkRescue(run.ID)
			if err != nil {
				return fmt.Errorf("cannot read partial work; retained %s: %w", dir, err)
			}
			if previous != nil {
				if previous.State != "saved" {
					return fmt.Errorf("partial work retained %s: %s", dir, previous.Reason)
				}
				unchanged, err := custody.PartialWorkUnchanged(ctx, dir, previous)
				inspectErr = err
				if err == nil && unchanged {
					return nil
				}
			}
		}
		p, err = d.BeginWorkRescue(run, "cleanup", "", head, dir)
		if err != nil {
			return err
		}
	}
	if sweepErr != nil {
		quiescent = false
	}
	snapshot, e := custody.PreservePartialWork(ctx, dir, p.RunID, p.Step, p.StopID)
	e = errors.Join(e, inspectErr)
	if snapshot != nil {
		p.Ref = snapshot.Ref
		p.SHA = snapshot.SHA
		p.IndexSHA = snapshot.IndexSHA
		p.State = snapshot.State
		p.Reason = snapshot.Reason
		if snapshot.ParentHead != p.ParentHead {
			e = errors.Join(e, fmt.Errorf("worktree HEAD changed during invocation; retain for continuity reconciliation"))
		}
	}
	if !quiescent {
		e = errors.Join(e, errors.New("writer shutdown could not be verified"), sweepErr)
	}
	if e != nil {
		p.State = "retained"
		p.Reason = e.Error()
	}
	if p.State == "saved" || p.State == "settled" {
		p.Path = ""
	} else {
		p.Path = dir
	}
	if saveErr := d.SaveWorkRescue(p); saveErr != nil {
		return errors.Join(e, fmt.Errorf("persist partial work; retained %s: %w", dir, saveErr))
	}
	if p.State != "saved" && p.State != "settled" {
		return errors.Join(e, fmt.Errorf("partial work retained %s: %s (stop: %s)", dir, p.Reason, reason))
	}
	return nil
}
