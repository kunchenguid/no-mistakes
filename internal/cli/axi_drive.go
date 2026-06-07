package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	toon "github.com/toon-format/toon-go"

	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/spf13/cobra"
)

// drivePollInterval is how often the drive loop re-reads run state. Short
// enough to feel responsive to an agent, long enough to avoid hammering the
// daemon during long agent steps.
const drivePollInterval = 250 * time.Millisecond

// triggerWaitTimeout bounds how long we wait for the daemon to register a run
// after pushing to the gate before falling back to a rerun.
const triggerWaitTimeout = 5 * time.Second

// terminalStatus reports whether a run has reached a final state.
func terminalStatus(status string) bool {
	switch types.RunStatus(status) {
	case types.RunCompleted, types.RunFailed, types.RunCancelled:
		return true
	default:
		return false
	}
}

// outcomeFor maps a terminal run status onto an agent-facing outcome word.
func outcomeFor(status string) string {
	switch types.RunStatus(status) {
	case types.RunCompleted:
		return "passed"
	case types.RunFailed:
		return "failed"
	case types.RunCancelled:
		return "cancelled"
	default:
		return status
	}
}

func newAxiRunCmd() *cobra.Command {
	var autoYes bool
	var skipValue string
	var intent string

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Validate your code changes, blocking until a decision point or the outcome",
		Long: "Triggers a pipeline run for the current branch and drives it. Without\n" +
			"--yes it blocks until the first approval gate (or the final outcome) and\n" +
			"prints it. With --yes it auto-resolves every gate (fixing actionable\n" +
			"findings, then accepting the result) and runs to completion.\n\n" +
			"--intent is required when starting a new run: pass what the user set out\n" +
			"to accomplish (the goal behind the change, not a description of the diff)\n" +
			"so no-mistakes uses it directly instead of inferring it from transcripts.",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return trackCommand("axi-run", func() error {
				skipSteps, err := parseSkipSteps(skipValue)
				if err != nil {
					return emitError(cmd, 2, err.Error(),
						"Valid steps: intent, rebase, review, test, document, lint, push, pr, ci")
				}
				return runAxiRun(cmd, autoYes, skipSteps, intent)
			})
		},
	}
	cmd.Flags().BoolVarP(&autoYes, "yes", "y", false, "auto-resolve every gate (fix findings, then accept) and run to completion")
	cmd.Flags().StringVar(&skipValue, "skip", "", "comma-separated pipeline steps to skip")
	cmd.Flags().StringVar(&intent, "intent", "", "what the user set out to accomplish (not a description of the diff); used instead of inferring from transcripts (required to start a run)")
	return cmd
}

func runAxiRun(cmd *cobra.Command, autoYes bool, skipSteps []types.StepName, intent string) error {
	ctx := cmd.Context()
	env, err := openAxiEnv(true)
	if err != nil {
		return emitError(cmd, 1, err.Error(), repoInitHelp(err)...)
	}
	defer env.close()

	branch, err := git.CurrentBranch(ctx, ".")
	if err != nil {
		return emitError(cmd, 1, fmt.Sprintf("get current branch: %v", err))
	}
	if branch == "HEAD" {
		return emitError(cmd, 1, "detached HEAD: check out a branch before validating",
			"Run `git switch -c <branch>` to put your commits on a branch")
	}

	headSHA, err := git.Run(ctx, ".", "rev-parse", "HEAD")
	if err != nil {
		return emitError(cmd, 1, fmt.Sprintf("get current HEAD: %v", err))
	}

	runID := activeRunID(env, branch, headSHA)
	if runID == "" {
		// Intent is mandatory when starting a run: the agent driving this knows
		// the change's intent, so we take it directly instead of inferring it
		// from transcripts. Reattaching to an in-flight run does not need it.
		if strings.TrimSpace(intent) == "" {
			return emitError(cmd, 2, "--intent is required to start a run",
				`Pass what the user set out to accomplish: no-mistakes axi run --intent "the user's goal"`)
		}
		// Starting a fresh run: apply the same pre-flight the human wizard
		// enforces, but as structured errors the agent acts on rather than
		// silent auto-branching/auto-committing. The gate validates committed
		// history, so a wrong branch or uncommitted work would otherwise be
		// validated incorrectly or not at all.
		if guard := preflightGuard(ctx, env, branch); guard != nil {
			return guard(cmd)
		}
		var err error
		runID, err = triggerRun(ctx, env, branch, headSHA, skipSteps, intent)
		if err != nil {
			return emitError(cmd, 1, err.Error())
		}
	}

	run, err := driveRun(ctx, cmd.ErrOrStderr(), env.client, runID, autoYes)
	if err != nil {
		return emitError(cmd, 1, fmt.Sprintf("drive run: %v", err))
	}
	return renderDriveResult(cmd, run)
}

// activeRunID returns the ID of a non-terminal run for branch and head, or "" if none.
func activeRunID(env *axiEnv, branch, headSHA string) string {
	var active ipc.GetActiveRunResult
	if err := env.client.Call(ipc.MethodGetActiveRun, activeRunLookupParams(env.repo.ID, branch), &active); err != nil {
		return ""
	}
	return activeRunIDForHead(&active, headSHA)
}

func activeRunIDForHead(active *ipc.GetActiveRunResult, headSHA string) string {
	run := activeRunInfoForHead(active.Run, headSHA)
	if run == nil {
		return ""
	}
	return run.ID
}

func activeRunInfoForHead(run *ipc.RunInfo, headSHA string) *ipc.RunInfo {
	if run == nil || terminalStatus(string(run.Status)) || run.HeadSHA != headSHA {
		return nil
	}
	return run
}

// preflightGuard returns an emitter for the first unmet pre-flight condition
// when starting a new run, or nil when the branch is ready to validate. It
// mirrors the wizard's branch/commit hygiene as detect-and-guide: refuse the
// default branch, and refuse an uncommitted working tree, each with the
// command the agent should run.
func preflightGuard(ctx context.Context, env *axiEnv, branch string) func(*cobra.Command) error {
	if env.repo.DefaultBranch != "" && branch == env.repo.DefaultBranch {
		return func(cmd *cobra.Command) error {
			return emitError(cmd, 1, fmt.Sprintf("refusing to validate %q: it is the default branch", branch),
				"Put your changes on a feature branch: `git switch -c <branch>`, then re-run")
		}
	}
	dirty, err := git.HasUncommittedChanges(ctx, ".")
	if err != nil {
		return func(cmd *cobra.Command) error {
			return emitError(cmd, 1, fmt.Sprintf("inspect working tree: %v", err),
				"Run `git status` to check the repository state, then re-run")
		}
	}
	if dirty {
		return func(cmd *cobra.Command) error {
			return emitError(cmd, 1, "uncommitted changes in the working tree",
				"Commit your work before validating: `git add -A && git commit -m \"...\"`, then re-run",
				"Run `git status` to see what is uncommitted")
		}
	}
	return nil
}

// triggerRun starts a fresh run for branch: it pushes the current HEAD through
// the gate to trigger a pipeline, and falls back to a rerun when the push was a
// no-op (the gate already had this commit). Callers must check for an existing
// active run first (see activeRunID) and apply pre-flight guards.
func triggerRun(ctx context.Context, env *axiEnv, branch, headSHA string, skipSteps []types.StepName, intent string) (string, error) {
	pushOptions := formatSkipPushOptions(skipSteps)
	if opt := formatIntentPushOption(intent); opt != "" {
		pushOptions = append(pushOptions, opt)
	}
	pushErr := git.PushWithOptions(ctx, ".", gate.RemoteName, "refs/heads/"+branch, "", false, pushOptions)

	if run, _ := waitForActiveRunForHead(ctx, env.client, env.repo.ID, branch, headSHA, triggerWaitTimeout); run != nil {
		return run.ID, nil
	}
	if !shouldRerunAfterNoActiveRun(pushErr) {
		return "", fmt.Errorf("push %q to gate: %v", branch, pushErr)
	}

	// No run appeared: the push was likely up-to-date. Rerun the latest gate
	// head so `axi run` is still useful when there are no new commits.
	var rr ipc.RerunResult
	if err := env.client.Call(ipc.MethodRerun, rerunParams(env.repo.ID, branch, skipSteps, intent), &rr); err != nil {
		return "", fmt.Errorf("no run started for %q: %v", branch, err)
	}
	return rr.RunID, nil
}

func waitForActiveRunForHead(ctx context.Context, client *ipc.Client, repoID, branch, headSHA string, timeout time.Duration) (*ipc.RunInfo, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()

	poll := time.NewTicker(150 * time.Millisecond)
	defer poll.Stop()

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var result ipc.GetActiveRunResult
		if err := client.Call(ipc.MethodGetActiveRun, &ipc.GetActiveRunParams{RepoID: repoID, Branch: branch}, &result); err != nil {
			return nil, err
		}
		if run := activeRunInfoForHead(result.Run, headSHA); run != nil {
			return run, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, nil
		case <-poll.C:
		}
	}
}

func shouldRerunAfterNoActiveRun(pushErr error) bool {
	return pushErr == nil
}

func activeRunLookupParams(repoID, branch string) *ipc.GetActiveRunParams {
	return &ipc.GetActiveRunParams{RepoID: repoID, Branch: branch}
}

func rerunParams(repoID, branch string, skipSteps []types.StepName, intent string) *ipc.RerunParams {
	return &ipc.RerunParams{RepoID: repoID, Branch: branch, SkipSteps: skipSteps, Intent: intent}
}

// driveRun polls a run until it reaches an approval gate or a terminal state,
// streaming step transitions to progress (stderr). When autoApprove is set it
// resolves each gate and continues; otherwise it returns at the first gate so
// the caller can surface it for a human/agent decision.
//
// Auto-resolution means "agree to fix every finding": a gate with actionable
// findings is fixed (every finding selected), and the resulting fix_review is
// accepted; gates with only non-actionable findings are approved. Each step is
// fixed at most once so a finding the fix cannot clear converges to an approval
// instead of looping forever.
func driveRun(ctx context.Context, progress io.Writer, client *ipc.Client, runID string, autoApprove bool) (*ipc.RunInfo, error) {
	pp := &progressPrinter{w: progress, seen: map[string]string{}}
	fixedSteps := map[string]bool{}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		run, err := getRunInfo(client, runID)
		if err != nil {
			return nil, err
		}
		if run == nil {
			return nil, fmt.Errorf("run %s not found", runID)
		}
		pp.update(run)

		rv := runViewFromIPC(run)
		if terminalStatus(rv.Status) {
			return run, nil
		}
		if gate, ok := rv.awaitingStep(); ok {
			if !autoApprove {
				return run, nil
			}
			action, findingIDs := gateResolution(gate, fixedSteps[gate.Name])
			if action == types.ActionFix {
				fixedSteps[gate.Name] = true
			}
			if err := sendRespond(client, runID, types.StepName(gate.Name), action, findingIDs, nil, nil); err != nil {
				return nil, fmt.Errorf("auto-resolve %s: %w", gate.Name, err)
			}
			if err := waitStepLeavesGate(ctx, client, runID, gate.Name, gate.Status); err != nil {
				return nil, err
			}
			continue
		}
		if err := sleepCtx(ctx, drivePollInterval); err != nil {
			return nil, err
		}
	}
}

// gateResolution decides how --yes answers an approval gate. A gate with
// actionable findings (anything other than purely informational "no-op") is
// fixed with every finding selected, unless this step was already fixed once -
// in which case the gate is approved so the run converges instead of looping on
// a finding the fix cannot clear. Gates with only non-actionable findings, no
// findings, or actionable findings that carry no IDs (which a fix would resolve
// to zero selections) are approved.
func gateResolution(gate stepView, alreadyFixed bool) (types.ApprovalAction, []string) {
	if alreadyFixed || gate.Status == string(types.StepStatusFixReview) {
		return types.ActionApprove, nil
	}
	parsed, err := types.ParseFindingsJSON(gate.FindingsJSON)
	if err != nil || !types.HasActionableFindings(parsed) {
		return types.ActionApprove, nil
	}
	ids := make([]string, 0, len(parsed.Items))
	for _, f := range parsed.Items {
		if f.ID != "" {
			ids = append(ids, f.ID)
		}
	}
	if len(ids) == 0 {
		return types.ActionApprove, nil
	}
	return types.ActionFix, ids
}

// waitStepLeavesGate blocks until the named step's status changes away from the
// gate status we just answered, or the run terminates. This prevents a
// double-approve race: respond is asynchronous, so without waiting the next
// poll could still observe the same gate and approve it twice.
func waitStepLeavesGate(ctx context.Context, client *ipc.Client, runID, step, gateStatus string) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		run, err := getRunInfo(client, runID)
		if err != nil {
			return err
		}
		if run == nil || terminalStatus(string(run.Status)) {
			return nil
		}
		for _, s := range run.Steps {
			if string(s.StepName) == step {
				if string(s.Status) != gateStatus {
					return nil
				}
				break
			}
		}
		if err := sleepCtx(ctx, drivePollInterval); err != nil {
			return err
		}
	}
}

func getRunInfo(client *ipc.Client, runID string) (*ipc.RunInfo, error) {
	var result ipc.GetRunResult
	if err := client.Call(ipc.MethodGetRun, &ipc.GetRunParams{RunID: runID}, &result); err != nil {
		return nil, err
	}
	return result.Run, nil
}

// sendRespond issues an approval action to the daemon for a step.
func sendRespond(client *ipc.Client, runID string, step types.StepName, action types.ApprovalAction, findingIDs []string, instructions map[string]string, added []types.Finding) error {
	params := &ipc.RespondParams{
		RunID:         runID,
		Step:          step,
		Action:        action,
		FindingIDs:    findingIDs,
		Instructions:  instructions,
		AddedFindings: added,
	}
	var result ipc.RespondResult
	if err := client.Call(ipc.MethodRespond, params, &result); err != nil {
		return err
	}
	if !result.OK {
		return fmt.Errorf("daemon rejected the response")
	}
	return nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// renderDriveResult prints the run snapshot plus either the active gate (exit
// 0, a normal decision point) or the terminal outcome (exit 0 when passed,
// exit 1 when blocked, failed, or cancelled).
func renderDriveResult(cmd *cobra.Command, run *ipc.RunInfo) error {
	rv := runViewFromIPC(run)
	fields := []toon.Field{runObjectField(rv)}

	if gate, ok := rv.awaitingStep(); ok {
		fields = append(fields, gateFields(gate)...)
		emitDoc(cmd, fields...)
		return nil
	}

	fields = append(fields, toon.Field{Key: "outcome", Value: outcomeFor(rv.Status)})
	if run.Error != nil && *run.Error != "" {
		fields = append(fields, toon.Field{Key: "error", Value: *run.Error})
	}
	if rv.PRURL != "" {
		fields = append(fields, toon.Field{Key: "help", Value: []string{fmt.Sprintf("Open the PR: %s", rv.PRURL)}})
	}
	emitDoc(cmd, fields...)

	if rv.Status == string(types.RunCompleted) {
		return nil
	}
	return &exitError{code: 1}
}

func newAxiRespondCmd() *cobra.Command {
	var action, step, findings, instructions, addFinding string
	var autoYes bool

	cmd := &cobra.Command{
		Use:   "respond",
		Short: "Answer the current approval gate and continue the run",
		Long: "Sends approve/fix/skip for the step currently awaiting approval, then\n" +
			"blocks until the next gate or the final outcome.",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return trackCommand("axi-respond", func() error {
				return runAxiRespond(cmd, respondArgs{
					action:       action,
					step:         step,
					findings:     findings,
					instructions: instructions,
					addFinding:   addFinding,
					autoYes:      autoYes,
				})
			})
		},
	}
	cmd.Flags().StringVar(&action, "action", "", "approve | fix | skip (required)")
	cmd.Flags().StringVar(&step, "step", "", "step to respond to (default: the step awaiting approval)")
	cmd.Flags().StringVar(&findings, "findings", "", "comma-separated finding IDs to fix (with --action fix)")
	cmd.Flags().StringVar(&instructions, "instructions", "", "guidance applied to the selected findings (with --action fix)")
	cmd.Flags().StringVar(&addFinding, "add-finding", "", "JSON finding object to add and fix (with --action fix)")
	cmd.Flags().BoolVarP(&autoYes, "yes", "y", false, "auto-resolve every subsequent gate to completion")
	return cmd
}

type respondArgs struct {
	action       string
	step         string
	findings     string
	instructions string
	addFinding   string
	autoYes      bool
}

func runAxiRespond(cmd *cobra.Command, ra respondArgs) error {
	ctx := cmd.Context()

	act := types.ApprovalAction(strings.TrimSpace(ra.action))
	switch act {
	case types.ActionApprove, types.ActionFix, types.ActionSkip:
	case "":
		return emitError(cmd, 2, "--action is required",
			"Run `no-mistakes axi respond --action approve|fix|skip`")
	default:
		return emitError(cmd, 2, fmt.Sprintf("unknown action %q", ra.action),
			"Valid actions: approve, fix, skip")
	}

	env, err := openAxiEnv(true)
	if err != nil {
		return emitError(cmd, 1, err.Error(), repoInitHelp(err)...)
	}
	defer env.close()
	branch, err := git.CurrentBranch(ctx, ".")
	if err != nil {
		return emitError(cmd, 1, fmt.Sprintf("get current branch: %v", err))
	}

	var active ipc.GetActiveRunResult
	if err := env.client.Call(ipc.MethodGetActiveRun, activeRunLookupParams(env.repo.ID, branch), &active); err != nil {
		return emitError(cmd, 1, fmt.Sprintf("get active run: %v", err))
	}
	if active.Run == nil {
		return emitError(cmd, 1, "no active run to respond to",
			"Run `no-mistakes axi run` to start one")
	}
	runID := active.Run.ID

	run, err := getRunInfo(env.client, runID)
	if err != nil || run == nil {
		return emitError(cmd, 1, fmt.Sprintf("load run: %v", err))
	}
	rv := runViewFromIPC(run)

	stepName := types.StepName(strings.TrimSpace(ra.step))
	if stepName == "" {
		gate, ok := rv.awaitingStep()
		if !ok {
			return emitError(cmd, 1, "no step is awaiting approval",
				"Run `no-mistakes axi status` to see the run state")
		}
		stepName = types.StepName(gate.Name)
	}

	findingIDs := splitCSV(ra.findings)
	var instructions map[string]string
	var added []types.Finding

	if act == types.ActionFix {
		if len(findingIDs) == 0 && ra.addFinding == "" {
			return emitError(cmd, 2, "--action fix requires --findings <id,...> or --add-finding <json>",
				"Run `no-mistakes axi status` to list finding IDs")
		}
		if note := strings.TrimSpace(ra.instructions); note != "" && len(findingIDs) > 0 {
			instructions = make(map[string]string, len(findingIDs))
			for _, id := range findingIDs {
				instructions[id] = note
			}
		}
		if ra.addFinding != "" {
			f, err := parseAddFinding(ra.addFinding)
			if err != nil {
				return emitError(cmd, 2, fmt.Sprintf("invalid --add-finding: %v", err),
					`Expected a JSON object, e.g. {"description":"...","action":"auto-fix"}`)
			}
			added = append(added, f)
		}
	}

	if err := sendRespond(env.client, runID, stepName, act, findingIDs, instructions, added); err != nil {
		return emitError(cmd, 1, fmt.Sprintf("respond to %s: %v", stepName, err))
	}

	// Let the executor consume the response before we re-read state, so we
	// don't immediately observe the same gate we just answered.
	if err := waitStepLeavesGate(ctx, env.client, runID, string(stepName), gateStatusFor(rv, string(stepName))); err != nil {
		return emitError(cmd, 1, fmt.Sprintf("wait for %s: %v", stepName, err))
	}

	final, err := driveRun(ctx, cmd.ErrOrStderr(), env.client, runID, ra.autoYes)
	if err != nil {
		return emitError(cmd, 1, fmt.Sprintf("drive run: %v", err))
	}
	return renderDriveResult(cmd, final)
}

// gateStatusFor returns the current status of step in rv, defaulting to the
// awaiting-approval status so the post-respond wait still functions if the step
// was not found.
func gateStatusFor(rv runView, step string) string {
	for _, s := range rv.Steps {
		if s.Name == step {
			return s.Status
		}
	}
	return string(types.StepStatusAwaitingApproval)
}

func newAxiAbortCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "abort",
		Short:         "Cancel the active pipeline run",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return trackCommand("axi-abort", func() error {
				return runAxiAbort(cmd)
			})
		},
	}
	return cmd
}

func runAxiAbort(cmd *cobra.Command) error {
	ctx := cmd.Context()
	env, err := openAxiEnv(true)
	if err != nil {
		return emitError(cmd, 1, err.Error(), repoInitHelp(err)...)
	}
	defer env.close()
	branch, err := git.CurrentBranch(ctx, ".")
	if err != nil {
		return emitError(cmd, 1, fmt.Sprintf("get current branch: %v", err))
	}

	var active ipc.GetActiveRunResult
	if err := env.client.Call(ipc.MethodGetActiveRun, activeRunLookupParams(env.repo.ID, branch), &active); err != nil {
		return emitError(cmd, 1, fmt.Sprintf("get active run: %v", err))
	}

	if active.Run == nil {
		// Idempotent: nothing to abort is a successful no-op.
		emitDoc(cmd,
			toon.Field{Key: "aborted", Value: false},
			toon.Field{Key: "detail", Value: "no active run (no-op)"},
		)
		return nil
	}

	var result ipc.CancelRunResult
	if err := env.client.Call(ipc.MethodCancelRun, &ipc.CancelRunParams{RunID: active.Run.ID}, &result); err != nil {
		return emitError(cmd, 1, fmt.Sprintf("abort run: %v", err))
	}
	emitDoc(cmd,
		toon.Field{Key: "aborted", Value: true},
		toon.Field{Key: "run", Value: active.Run.ID},
		toon.Field{Key: "branch", Value: active.Run.Branch},
	)
	return nil
}

func splitCSV(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
