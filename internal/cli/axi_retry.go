package cli

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	toon "github.com/toon-format/toon-go"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

// newAxiRetryCmd continues a run that failed on an agent invocation error.
//
// --run does not select a run. The retried run is always the current branch's
// latest run, as for every mutating axi command; --run is the operator's
// confirmation of which failed run they inspected, and the daemon refuses
// when it names any other.
func newAxiRetryCmd() *cobra.Command {
	var runID string
	var wait time.Duration
	cmd := &cobra.Command{
		Use:   "retry",
		Short: "Continue a run that failed on an agent invocation error",
		Long: "Continues the current branch's latest run when it failed because an agent\n" +
			"invocation itself failed - a provider error, a usage limit, a transport\n" +
			"failure, or the agent process exiting - instead of starting the whole\n" +
			"pipeline again with `no-mistakes rerun`.\n\n" +
			"A new run starts at the failed run's preserved head, so every correction\n" +
			"the pipeline already committed is kept. The steps before the failed one\n" +
			"are carried from the failed run. The failed step restarts from the\n" +
			"beginning - a failed agent turn is never replayed - so a failed Review\n" +
			"reviews the resulting head again in full before Test runs. Every later\n" +
			"step runs normally.\n\n" +
			"The retry refuses, and changes nothing, unless every proof holds: --run\n" +
			"names the branch's latest run; that run failed, nothing is active on the\n" +
			"branch, and it never pushed; its record shows exactly one failed step,\n" +
			"failed on an agent invocation error, at or before Test; its preserved head\n" +
			"is still anchored in the local gate; the working tree is clean at exactly\n" +
			"that head (adopt it first with `no-mistakes axi sync --recover`); and the\n" +
			"run's model profile and the repository's gates are unchanged, and a carried\n" +
			"Review's repository configuration, recorded global configuration, agent,\n" +
			"and model are still the ones a new run would use. A failure\n" +
			"about the change itself - a review finding, a failing test, a refused\n" +
			"policy, a human decision - is never retried.\n\n" +
			"The new run records which run, step, and round it continues. Like `axi\n" +
			"run`, retry then blocks until the next gate, CI-ready decision point, or\n" +
			"final outcome, bounded by --wait (default 8m).",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return trackAxiSurface("axi-retry", "/axi/retry", nil, func() error {
				return runAxiRetry(cmd, strings.TrimSpace(runID), wait)
			})
		},
	}
	cmd.Flags().StringVar(&runID, "run", "", "id of the failed run, which must be the current branch's latest run (required)")
	bindAxiWaitFlag(cmd, &wait)
	return cmd
}

func runAxiRetry(cmd *cobra.Command, runID string, wait time.Duration) error {
	if runID == "" {
		return emitError(cmd, 2, "--run is required",
			"Run `no-mistakes axi status` to read the failed run's id")
	}
	if err := validateAxiWait(wait); err != nil {
		return emitError(cmd, 2, err.Error(), "Pass a positive duration such as --wait 8m")
	}

	ctx := cmd.Context()
	env, err := openAxiDaemonEnv()
	if err != nil {
		return emitError(cmd, 1, err.Error(), repoInitHelp(err)...)
	}
	defer env.close()

	branch, err := git.CurrentBranch(ctx, ".")
	if err != nil {
		return emitError(cmd, 1, fmt.Sprintf("get current branch: %v", err))
	}
	if branch == "HEAD" {
		return emitError(cmd, 1, "not on a branch", "Check out the failed run's branch, then retry")
	}
	callerHead, err := rerunCallerHead(ctx)
	if err != nil {
		return emitError(cmd, 1, err.Error())
	}

	var result ipc.RetryRunResult
	if err := env.client.Call(ipc.MethodRetryRun, &ipc.RetryRunParams{
		RepoID:        env.repo.ID,
		Branch:        branch,
		RunID:         runID,
		CallerHeadSHA: callerHead,
	}, &result); err != nil {
		var rpcErr *ipc.RPCError
		if errors.As(err, &rpcErr) && rpcErr.Code == ipc.ErrMethodNotFound {
			return emitError(cmd, 1, "the running daemon is too old to retry a run",
				"Restart it with `no-mistakes daemon restart` so the current binary serves it")
		}
		return emitError(cmd, 1, err.Error(),
			"Run `no-mistakes axi status` to inspect the failed run and the branch's custody")
	}

	lead := []toon.Field{
		{Key: "retried", Value: true},
		{Key: "retry_of", Value: result.SourceRunID},
		{Key: "restarted_step", Value: string(result.Step)},
		{Key: "failed_round", Value: result.SourceRound},
	}
	driveCtx, cancel, err := boundAxiWait(ctx, wait)
	if err != nil {
		return emitError(cmd, 2, err.Error(), "Pass a positive duration such as --wait 8m")
	}
	defer cancel()
	final, ciReady, err := driveRun(driveCtx, cmd.ErrOrStderr(), env.client, env.p.Socket(), result.RunID, false)
	if err != nil {
		if isAxiWaitElapsed(ctx, driveCtx, err) {
			return emitAxiWaitElapsed(cmd, wait, "no-mistakes axi run", lead...)
		}
		return emitError(cmd, 1, fmt.Sprintf("follow the retried run %s: %v", result.RunID, err),
			"The retry started; run `no-mistakes axi run` to reattach to it")
	}
	return renderDriveResult(cmd, final, ciReady, lead...)
}
