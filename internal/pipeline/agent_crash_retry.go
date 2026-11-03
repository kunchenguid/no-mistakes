package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func retryableAgentCrash(ctx context.Context, err error, configuredTimeouts bool) bool {
	if ctx.Err() != nil {
		return false
	}
	// Includes an adapter's exhausted transient-retry loop: its underlying
	// process exit must not receive a second retry budget here.
	if agent.IsUsageLimitReached(err) || agent.IsTerminalProviderError(err) {
		return false
	}
	var budgetCut *agentInvocationError
	if errors.As(err, &budgetCut) {
		return !configuredTimeouts && budgetCut.bound != ""
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if !errors.Is(err, ErrAgentInvocationFailed) {
		return false
	}
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr)
}

// CheckAgentCrashRetryBase binds the actual diff base at the shared resolver,
// before any agent runs. A fresh fetch on a retry (or a later validation step)
// must not silently change the scope that retained steps already validated.
// The unconfigured run has no pin and follows its existing base resolution.
func (sctx *StepContext) CheckAgentCrashRetryBase(base string) error {
	if sctx.crashRetryBaseSHA == nil {
		return nil
	}
	if *sctx.crashRetryBaseSHA == "" {
		*sctx.crashRetryBaseSHA = base
		return nil
	}
	if base != *sctx.crashRetryBaseSHA {
		return errors.New("agent crash retry refused: effective validation base changed; start a fresh run for full validation")
	}
	return nil
}

func crashRetryStep(name types.StepName) bool {
	switch name {
	case types.StepReview, types.StepTest, types.StepDocument, types.StepLint:
		return true
	default:
		// Delivery and conflict resolvers can have outward side effects that a
		// local same-SHA proof cannot establish are safe to replay.
		return false
	}
}

// crashRetryProof is deliberately ephemeral: retries are in the same live
// execution only. Daemon recovery and manual continuation get no reuse credit.
// Hashes are compared, never logged or persisted as a second source of truth.
type crashRetryProof [sha256.Size]byte

func (e *Executor) crashRetryProof(sctx *StepContext) (crashRetryProof, error) {
	var zero crashRetryProof
	ctx := sctx.Ctx
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	head, err := git.HeadSHA(ctx, sctx.WorkDir)
	if err != nil {
		return zero, err
	}
	live, err := e.db.GetRun(sctx.Run.ID)
	if err != nil {
		return zero, err
	}
	if live.Status != types.RunRunning || head != live.HeadSHA || head != sctx.Run.HeadSHA || live.BaseSHA != sctx.Run.BaseSHA {
		return zero, errors.New("run or worktree head/base changed")
	}
	validationBase := live.BaseSHA
	if e.crashRetryBaseSHA != "" {
		validationBase = e.crashRetryBaseSHA
	}
	if !git.IsZeroSHA(validationBase) {
		if _, err := git.Run(ctx, sctx.WorkDir, "cat-file", "-e", validationBase+"^{commit}"); err != nil {
			return zero, err
		}
	}
	// Include ignored files: a crashed agent can leave partial work there too.
	status, err := git.Run(ctx, sctx.WorkDir, "status", "--porcelain=v1", "--untracked-files=all", "--ignored")
	if err != nil {
		return zero, err
	}
	if status != "" {
		return zero, errors.New("worktree is not clean (including ignored files)")
	}
	hash := sha256.New()
	encoder := json.NewEncoder(hash)
	steps, err := e.db.GetStepsByRun(live.ID)
	if err != nil {
		return zero, err
	}
	for _, value := range []any{live.ID, live.HeadSHA, live.BaseSHA, sctx.Config, e.config} {
		if err := encoder.Encode(value); err != nil {
			return zero, err
		}
	}
	// A completed step's persisted findings and rounds are its validation
	// evidence. Ignore only the current running step's activity timestamps.
	for _, step := range steps {
		if step.ID == sctx.StepResultID {
			continue
		}
		rounds, err := e.db.GetRoundsByStep(step.ID)
		if err != nil {
			return zero, err
		}
		if err := encoder.Encode(step); err != nil {
			return zero, err
		}
		if err := encoder.Encode(rounds); err != nil {
			return zero, err
		}
	}
	if sctx.EvidenceDir != "" {
		err = filepath.WalkDir(sctx.EvidenceDir, func(path string, entry fs.DirEntry, walkErr error) error {
			if errors.Is(walkErr, fs.ErrNotExist) && path == sctx.EvidenceDir {
				return nil
			}
			if walkErr != nil {
				return walkErr
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			rel, err := filepath.Rel(sctx.EvidenceDir, path)
			if err != nil {
				return err
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			// Test creates its evidence directory before invoking the agent.
			// Empty directory creation is not new validation evidence: hash
			// only files, so missing and empty trees are equivalent.
			if entry.IsDir() {
				return nil
			}
			if err := encoder.Encode([]any{rel, info.Mode()}); err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("evidence contains a non-regular file")
			}
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			fileHash := sha256.New()
			_, copyErr := io.Copy(fileHash, file)
			closeErr := file.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
			return encoder.Encode(fileHash.Sum(nil))
		})
		if err != nil {
			return zero, err
		}
	}
	copy(zero[:], hash.Sum(nil))
	return zero, nil
}

func (e *Executor) executeWithCrashRetry(step Step, sctx *StepContext, remaining *int) (*StepOutcome, error) {
	if !crashRetryStep(step.Name()) || (sctx.Fixing && !sctx.FinalizingAnswers) || (*remaining <= 0 && e.crashRetryBaseSHA == "") {
		return step.Execute(sctx)
	}
	sctx.crashRetryBaseSHA = &e.crashRetryBaseSHA
	defer func() { sctx.crashRetryBaseSHA = nil }()
	if *remaining <= 0 || sctx.FinalizingAnswers || sctx.CoverageContinuation != nil {
		return step.Execute(sctx)
	}
	proof, proofErr := e.crashRetryProof(sctx)
	for {
		outcome, err := step.Execute(sctx)
		if err == nil || *remaining <= 0 {
			return outcome, err
		}
		if outcome != nil && (outcome.ExitCode != 0 || outcome.NeedsApproval) {
			return outcome, err
		}
		if !retryableAgentCrash(sctx.Ctx, err, e.config.AgentTimeoutConfiguredFor(step.Name())) {
			return outcome, err
		}
		if proofErr != nil {
			sctx.Log("agent crash retry refused: initial same-SHA/config/evidence proof unavailable")
			return outcome, err
		}
		after, checkErr := e.crashRetryProof(sctx)
		if checkErr != nil || after != proof {
			sctx.Log("agent crash retry refused: head, base, config, worktree or evidence changed; full validation required")
			return outcome, err
		}
		*remaining--
		if sctx.Sessions != nil {
			sctx.Sessions.Forget(SessionRoleReviewer)
		}
		sctx.Log(fmt.Sprintf("agent_invocation: retrying %s in the same run (%d retries remaining)", step.Name(), *remaining))
	}
}
