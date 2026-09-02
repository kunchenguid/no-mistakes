package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	toon "github.com/toon-format/toon-go"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/telemetry"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/spf13/cobra"
)

// logTailLines is how many trailing log lines `axi logs` shows without --full.
const logTailLines = 40

func newAxiStatusCmd() *cobra.Command {
	var runID string
	cmd := &cobra.Command{
		Use:           "status",
		Short:         "Show a current-branch or explicitly selected run in detail",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return trackReadSurface("axi-status", telemetry.Fields{
				"explicit_run_id": strings.TrimSpace(runID) != "",
			}, func() (string, string, error) {
				fingerprint, err := runAxiStatus(cmd, runID)
				return fingerprint, "", err
			})
		},
	}
	cmd.Flags().StringVar(&runID, "run", "", "inspect a specific run ID (default: current branch's active or most recent)")
	return cmd
}

// runAxiStatus renders the run status and returns a low-cardinality state
// fingerprint (run id, run status, per-step statuses) used to dedupe the
// command's telemetry across repeated polls.
func runAxiStatus(cmd *cobra.Command, runID string) (string, error) {
	env, err := openAxiQueryEnv(runID)
	if err != nil {
		return "", emitError(cmd, 1, err.Error(), repoInitHelp(err)...)
	}
	defer env.close()

	branch, branchErr := currentBranchForRunResolve(cmd.Context())
	if branchErr != nil && runID == "" {
		return "", emitError(cmd, 1, branchErr.Error())
	}
	run, runs, err := resolveRun(env, runID, branch)
	if err != nil {
		return "", emitError(cmd, 1, err.Error())
	}

	if run == nil {
		if runID != "" {
			return "", emitError(cmd, 1, fmt.Sprintf("run %q not found", runID))
		}
		if branch == "" {
			runs, err = env.d.GetRunsByRepo(env.repo.ID)
			if err != nil {
				return "", emitError(cmd, 1, fmt.Sprintf("list runs: %v", err))
			}
		}
		return emitNoRunForCaller(cmd, env, branch, runs)
	}

	steps, err := env.d.GetStepsByRun(run.ID)
	if err != nil {
		return "", emitError(cmd, 1, fmt.Sprintf("load steps: %v", err))
	}
	rv := runViewFromDB(run, steps, env.d)
	annotateRunView(env, &rv)
	var fields []toon.Field
	// A run reached by an explicit --run may belong to another branch. Say so
	// with the key itself, reusing the home view's other_branch_* vocabulary,
	// so a parser reading `run:` never picks up a run that is provably not
	// this worktree's. Only positive evidence marks it: an undeterminable
	// branch cannot contradict the id the caller asked for.
	runKey := "run"
	foreignRun := branch != "" && run.Branch != branch
	if foreignRun {
		runKey = "other_branch_run"
		fields = append(fields, toon.Field{Key: "current_branch", Value: branch})
	}
	fields = append(fields, runObjectFieldWithKey(runKey, rv))
	if syncField := cachedBranchSyncField(cmd, run.ID); syncField != nil {
		fields = append(fields, *syncField)
	}
	if gate, ok := rv.awaitingStep(); ok {
		// The label above and the commands below deliberately use different
		// evidence rules. Branch equality is enough to label the selected run,
		// but not enough to attach a bare mutation command to it: another active
		// run may exist (or appear) on the same branch, and `axi respond` has no
		// run selector. Keep every explicit selection inspection-only so its run
		// identity cannot be lost between this status read and the next command.
		branchScopedCommandsSafe := runID == ""
		if !branchScopedCommandsSafe {
			fields = append(fields, inspectionOnlyGateFields(gate, run.ID)...)
		} else {
			fields = append(fields, gateFields(gate)...)
		}
	} else if terminalStatus(rv.Status) {
		fields = append(fields, toon.Field{Key: "outcome", Value: outcomeForRun(rv)})
		if run.Error != nil && *run.Error != "" {
			fields = append(fields, toon.Field{Key: "error", Value: *run.Error})
		}
		if rv.CIOverrideReason != "" {
			fields = append(fields, toon.Field{Key: "ci_override_reason", Value: rv.CIOverrideReason})
		}
	}
	emitDoc(cmd, fields...)
	return runStateFingerprint(rv), nil
}

// emitNoRunForCaller answers `axi status` when the caller has no run of its
// own: the current branch has never had one, or the caller has a detached HEAD.
// It never substitutes some other branch's run. It names the branch it looked
// for, lists the repository's recent runs so a deliberate
// `--run <id>` inspection is one step away, and provides next-step help.
func emitNoRunForCaller(cmd *cobra.Command, env *axiEnv, branch string, runs []*db.Run) (string, error) {
	branchDisplay := branch
	if branchDisplay == "" {
		branchDisplay = "unknown"
	}
	fields := []toon.Field{{Key: "current_branch", Value: branchDisplay}}
	if branch != "" {
		fields = append(fields, toon.Field{Key: "runs_on_current_branch", Value: 0})
	}
	fields = append(fields, runsFields(runs, recentRunsHomeLimit)...)

	var help []string
	switch {
	case branch == "":
		help = append(help, "This worktree has no current branch (detached HEAD), so no run can be attributed to it; inspect a specific run with `no-mistakes axi status --run <id>`, or check out a branch first")
	case len(runs) > 0:
		help = append(help, startRunHelp(), "No run exists for this branch; every run listed above is on another branch - inspect one deliberately with `no-mistakes axi status --run <id>`")
	default:
		help = append(help, startRunHelp())
	}
	fields = append(fields, toon.Field{Key: "help", Value: help})
	emitDoc(cmd, fields...)
	return env.repo.ID + "|no-run-for:" + branchDisplay + "|runs:" + renderedRunsFingerprint(runs, recentRunsHomeLimit), nil
}

// runStateFingerprint summarizes a run's observable state for telemetry
// dedupe: any run/step status transition changes the fingerprint.
func runStateFingerprint(rv runView) string {
	var b strings.Builder
	b.WriteString(rv.ID)
	b.WriteByte('|')
	b.WriteString(rv.Branch)
	b.WriteByte('|')
	b.WriteString(rv.Status)
	b.WriteByte('|')
	b.WriteString(rv.HeadSHA)
	b.WriteByte('|')
	b.WriteString(rv.PRURL)
	for _, step := range rv.Steps {
		b.WriteByte('|')
		b.WriteString(step.Name)
		b.WriteByte(':')
		b.WriteString(step.Status)
	}
	return b.String()
}

func annotateRunView(env *axiEnv, rv *runView) {
	if env == nil || rv == nil {
		return
	}
	quietWarning := configQuietWarning(env)
	for i := range rv.Steps {
		step := &rv.Steps[i]
		step.QuietWarning = quietWarning
		if step.ID != "" {
			if stats, err := env.d.StepRoundStats(step.ID); err == nil {
				step.RoundCount = stats.TotalRounds
				step.FixRoundCount = stats.FixRounds
				step.PendingFixSource = stats.PendingFixSource
			}
		}
		if step.LastActivityAt == nil {
			logPath := filepath.Join(env.p.RunLogDir(rv.ID), step.Name+".log")
			if info, err := os.Stat(logPath); err == nil {
				ts := info.ModTime().Unix()
				step.LastActivityAt = &ts
				step.LastActivity = "step log updated"
			}
		}
	}
}

func configQuietWarning(env *axiEnv) time.Duration {
	if env == nil || env.cfg == nil || env.cfg.StepQuietWarning <= 0 {
		return 0
	}
	return env.cfg.StepQuietWarning
}

func startRunHelp() string {
	return `Run no-mistakes axi run --intent "the user's goal" --yes to validate the current branch`
}

func noRunLogsHelp() []string {
	return []string{
		startRunHelp(),
		"To read another branch's run, name it: `no-mistakes axi logs --run <id> --step <step>`",
	}
}

func newAxiLogsCmd() *cobra.Command {
	var step, runID string
	var full bool
	cmd := &cobra.Command{
		Use:           "logs",
		Short:         "Show the log output of one pipeline step",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return trackReadSurface("axi-logs", telemetry.Fields{
				"step":            sanitizeAxiTelemetryStep(step),
				"full":            full,
				"explicit_run_id": strings.TrimSpace(runID) != "",
			}, func() (string, string, error) {
				fingerprint, err := runAxiLogs(cmd, step, runID, full)
				return fingerprint, "", err
			})
		},
	}
	cmd.Flags().StringVar(&step, "step", "", "step name: intent, rebase, review, test, document, lint, push, pr, ci (required)")
	cmd.Flags().StringVar(&runID, "run", "", "run ID (default: current branch's active or most recent)")
	cmd.Flags().BoolVar(&full, "full", false, "show the entire log instead of the tail")
	return cmd
}

// runAxiLogs renders a step log and returns a run+step telemetry fingerprint:
// repeated reads of the same step's log carry no distinct analytics signal,
// so only switching run or step (or the heartbeat) re-emits.
func runAxiLogs(cmd *cobra.Command, step, runID string, full bool) (string, error) {
	step = strings.TrimSpace(step)
	if step == "" {
		return "", emitError(cmd, 2, "--step is required",
			"Valid steps: intent, rebase, review, test, document, lint, push, pr, ci")
	}
	if !validStep(types.StepName(step)) {
		return "", emitError(cmd, 2, fmt.Sprintf("unknown step %q", step),
			"Valid steps: intent, rebase, review, test, document, lint, push, pr, ci")
	}

	env, err := openAxiQueryEnv(runID)
	if err != nil {
		return "", emitError(cmd, 1, err.Error(), repoInitHelp(err)...)
	}
	defer env.close()

	branch, branchErr := currentBranchForRunResolve(cmd.Context())
	if branchErr != nil && runID == "" {
		return "", emitError(cmd, 1, branchErr.Error())
	}
	run, _, err := resolveRun(env, runID, branch)
	if err != nil {
		return "", emitError(cmd, 1, err.Error())
	}
	if run == nil {
		if runID != "" {
			return "", emitError(cmd, 1, fmt.Sprintf("run %q not found", runID))
		}
		help := noRunLogsHelp()
		if branch == "" {
			help = []string{"This worktree has no current branch (detached HEAD), so no run can be attributed to it; inspect a specific run with `no-mistakes axi logs --run <id> --step <step>`, or check out a branch first"}
		}
		return "", emitError(cmd, 1, "no run found for this branch to read logs from",
			help...)
	}
	steps, err := env.d.GetStepsByRun(run.ID)
	if err != nil {
		return "", emitError(cmd, 1, fmt.Sprintf("load steps: %v", err))
	}
	fingerprint := runStateFingerprint(runViewFromDB(run, steps, env.d)) + "|log:" + step

	path := filepath.Join(env.p.RunLogDir(run.ID), step+".log")
	data, err := os.ReadFile(path)
	fields := []toon.Field{
		{Key: "step", Value: step},
		{Key: "run", Value: run.ID},
	}
	if err != nil {
		if os.IsNotExist(err) {
			fields = append(fields, toon.Field{Key: "log", Value: fmt.Sprintf("no log recorded for step %q in this run", step)})
			emitDoc(cmd, fields...)
			return fingerprint, nil
		}
		return "", emitError(cmd, 1, fmt.Sprintf("read log: %v", err))
	}

	lines := splitLogLines(string(data))
	shown := lines
	if !full && len(lines) > logTailLines {
		shown = lines[len(lines)-logTailLines:]
		selectedRunID := ""
		if runID != "" {
			selectedRunID = run.ID
		}
		fields = append(fields,
			toon.Field{Key: "lines", Value: fmt.Sprintf("%d of %d total (tail)", len(shown), len(lines))},
			toon.Field{Key: "log", Value: logRows(shown)},
			toon.Field{Key: "help", Value: []string{fmt.Sprintf("Run `%s` to see the entire log", axiLogsFullCommand(step, selectedRunID))}},
		)
		emitDoc(cmd, fields...)
		return fingerprint, nil
	}
	fields = append(fields,
		toon.Field{Key: "lines", Value: fmt.Sprintf("%d total", len(lines))},
		toon.Field{Key: "log", Value: logRows(shown)},
	)
	emitDoc(cmd, fields...)
	return fingerprint, nil
}

// logRows wraps log lines as single-column rows so the encoder renders them as
// a block array (one line per row) rather than a single inline row.
func logRows(lines []string) []logRow {
	rows := make([]logRow, len(lines))
	for i, l := range lines {
		rows[i] = logRow{Line: l}
	}
	return rows
}

// resolveRun picks the run to inspect: an explicit ID, else the caller's
// current-branch active run, else that branch's most recent run. It returns a
// nil run when the caller's branch has no run of its own, and when the caller
// has a detached HEAD - a detached HEAD owns no branch, so no run can be
// attributed to it.
//
// It deliberately does NOT fall back to the repository's active or most recent
// run on some other branch. One clone commonly has several worktrees sitting on
// different branches, and that fallback handed every branch without a run of
// its own another branch's run under exactly the key a run of the caller's own
// gets - so a terminal run read as though the caller's own work had failed.
// Inspecting a run that is not this branch's is what `--run <id>` is for.
func resolveRun(env *axiEnv, runID, branch string) (*db.Run, []*db.Run, error) {
	if runID != "" {
		run, err := env.d.GetRun(runID)
		if err != nil {
			return nil, nil, fmt.Errorf("get run: %w", err)
		}
		return run, nil, nil
	}
	if branch == "" {
		return nil, nil, nil
	}
	active, err := env.d.GetActiveRun(env.repo.ID, branch)
	if err != nil {
		return nil, nil, fmt.Errorf("get active run: %w", err)
	}
	if active != nil {
		return active, nil, nil
	}
	runs, err := env.d.GetRunsByRepo(env.repo.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("list runs: %w", err)
	}
	for _, run := range runs {
		if run.Branch == branch {
			return run, runs, nil
		}
	}
	return nil, runs, nil
}

func currentBranchForRunResolve(ctx context.Context) (string, error) {
	branch, err := git.CurrentBranch(ctx, ".")
	if err != nil {
		return "", fmt.Errorf("determine current branch: %w", err)
	}
	if branch == "HEAD" {
		return "", nil
	}
	if branch == "" {
		return "", fmt.Errorf("determine current branch: git returned an empty branch name")
	}
	return branch, nil
}

func splitLogLines(s string) []string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// parseAddFinding decodes a user-authored finding from a JSON object string.
func parseAddFinding(raw string) (types.Finding, error) {
	var f types.Finding
	if err := json.Unmarshal([]byte(raw), &f); err != nil {
		return types.Finding{}, err
	}
	if strings.TrimSpace(f.Description) == "" {
		return types.Finding{}, fmt.Errorf("description is required")
	}
	return f, nil
}

// progressPrinter emits step and run status transitions to stderr so a human
// or agent watching the command sees liveness without parsing stdout.
type progressPrinter struct {
	w         io.Writer
	seen      map[string]string
	runStatus string
}

func (p *progressPrinter) update(run *ipc.RunInfo) {
	if p.w == nil {
		return
	}
	if string(run.Status) != p.runStatus {
		p.runStatus = string(run.Status)
		fmt.Fprintf(p.w, "run: %s\n", p.runStatus)
	}
	for _, s := range run.Steps {
		name := string(s.StepName)
		status := string(s.Status)
		if status == string(types.StepStatusPending) {
			continue
		}
		if p.seen[name] != status {
			p.seen[name] = status
			fmt.Fprintf(p.w, "  %s: %s\n", name, status)
		}
	}
}
