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
		Short:         "Show the active (or most recent) run in detail",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return trackAxiSurface("axi-status", "/axi/status", telemetry.Fields{
				"explicit_run_id": strings.TrimSpace(runID) != "",
			}, func() error {
				return runAxiStatus(cmd, runID)
			})
		},
	}
	cmd.Flags().StringVar(&runID, "run", "", "inspect a specific run ID (default: active or most recent)")
	return cmd
}

func runAxiStatus(cmd *cobra.Command, runID string) error {
	env, err := openAxiEnv(false)
	if err != nil {
		return emitError(cmd, 1, err.Error(), repoInitHelp(err)...)
	}
	defer env.close()

	run, err := resolveRun(env, runID, currentBranchForRunResolve(cmd.Context()))
	if err != nil {
		return emitError(cmd, 1, err.Error())
	}

	if run == nil {
		if runID != "" {
			return emitError(cmd, 1, fmt.Sprintf("run %q not found", runID))
		}
		emitDoc(cmd,
			toon.Field{Key: "runs", Value: "0 runs yet in this repository"},
			toon.Field{Key: "help", Value: []string{startRunHelp()}},
		)
		return nil
	}

	steps, err := env.d.GetStepsByRun(run.ID)
	if err != nil {
		return emitError(cmd, 1, fmt.Sprintf("load steps: %v", err))
	}
	rv := runViewFromDB(run, steps)
	annotateRunView(env, &rv)
	fields := []toon.Field{runObjectField(rv)}
	if gate, ok := rv.awaitingStep(); ok {
		fields = append(fields, gateFields(gate)...)
	} else if terminalStatus(rv.Status) {
		fields = append(fields, toon.Field{Key: "outcome", Value: outcomeFor(rv.Status)})
		if run.Error != nil && *run.Error != "" {
			fields = append(fields, toon.Field{Key: "error", Value: *run.Error})
		}
	}
	emitDoc(cmd, fields...)
	return nil
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

func noRunLogsHelp() string {
	return startRunHelp()
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
			return trackAxiSurface("axi-logs", "/axi/logs", telemetry.Fields{
				"step":            sanitizeAxiTelemetryStep(step),
				"full":            full,
				"explicit_run_id": strings.TrimSpace(runID) != "",
			}, func() error {
				return runAxiLogs(cmd, step, runID, full)
			})
		},
	}
	cmd.Flags().StringVar(&step, "step", "", "step name: intent, rebase, review, test, document, lint, push, pr, ci (required)")
	cmd.Flags().StringVar(&runID, "run", "", "run ID (default: active or most recent)")
	cmd.Flags().BoolVar(&full, "full", false, "show the entire log instead of the tail")
	return cmd
}

func runAxiLogs(cmd *cobra.Command, step, runID string, full bool) error {
	step = strings.TrimSpace(step)
	if step == "" {
		return emitError(cmd, 2, "--step is required",
			"Valid steps: intent, rebase, review, test, document, lint, push, pr, ci")
	}
	if !validStep(types.StepName(step)) {
		return emitError(cmd, 2, fmt.Sprintf("unknown step %q", step),
			"Valid steps: intent, rebase, review, test, document, lint, push, pr, ci")
	}

	env, err := openAxiEnv(false)
	if err != nil {
		return emitError(cmd, 1, err.Error(), repoInitHelp(err)...)
	}
	defer env.close()

	run, err := resolveRun(env, runID, currentBranchForRunResolve(cmd.Context()))
	if err != nil {
		return emitError(cmd, 1, err.Error())
	}
	if run == nil {
		return emitError(cmd, 1, "no run found to read logs from",
			noRunLogsHelp())
	}

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
			return nil
		}
		return emitError(cmd, 1, fmt.Sprintf("read log: %v", err))
	}

	lines := splitLogLines(string(data))
	shown := lines
	if !full && len(lines) > logTailLines {
		shown = lines[len(lines)-logTailLines:]
		fields = append(fields,
			toon.Field{Key: "lines", Value: fmt.Sprintf("%d of %d total (tail)", len(shown), len(lines))},
			toon.Field{Key: "log", Value: logRows(shown)},
			toon.Field{Key: "help", Value: []string{fmt.Sprintf("Run `no-mistakes axi logs --step %s --full` to see the entire log", step)}},
		)
		emitDoc(cmd, fields...)
		return nil
	}
	fields = append(fields,
		toon.Field{Key: "lines", Value: fmt.Sprintf("%d total", len(lines))},
		toon.Field{Key: "log", Value: logRows(shown)},
	)
	emitDoc(cmd, fields...)
	return nil
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

// resolveRun picks the run to inspect: an explicit ID, else the active run,
// else the most recent run for the repo. Returns (nil, nil) when none exist.
func resolveRun(env *axiEnv, runID, branch string) (*db.Run, error) {
	if runID != "" {
		run, err := env.d.GetRun(runID)
		if err != nil {
			return nil, fmt.Errorf("get run: %w", err)
		}
		return run, nil
	}
	if branch != "" {
		active, err := env.d.GetActiveRun(env.repo.ID, branch)
		if err != nil {
			return nil, fmt.Errorf("get active run: %w", err)
		}
		if active != nil {
			return active, nil
		}
		runs, err := env.d.GetRunsByRepo(env.repo.ID)
		if err != nil {
			return nil, fmt.Errorf("list runs: %w", err)
		}
		for _, run := range runs {
			if run.Branch == branch {
				return run, nil
			}
		}
	}
	active, err := env.d.GetActiveRun(env.repo.ID, "")
	if err != nil {
		return nil, fmt.Errorf("get active run: %w", err)
	}
	if active != nil {
		return active, nil
	}
	runs, err := env.d.GetRunsByRepo(env.repo.ID)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	if len(runs) == 0 {
		return nil, nil
	}
	return runs[0], nil
}

func currentBranchForRunResolve(ctx context.Context) string {
	branch, err := git.CurrentBranch(ctx, ".")
	if err != nil || branch == "HEAD" {
		return ""
	}
	return branch
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
