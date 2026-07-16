package cli

import (
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	toon "github.com/toon-format/toon-go"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/telemetry"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/spf13/cobra"
)

type watchUntil string

const (
	watchUntilAttention watchUntil = "attention"
	watchUntilTerminal  watchUntil = "terminal"
	maxWatchFindings               = 10
)

var watchNow = time.Now

func parseWatchUntil(value string) (watchUntil, error) {
	switch watchUntil(strings.TrimSpace(value)) {
	case "", watchUntilAttention:
		return watchUntilAttention, nil
	case watchUntilTerminal:
		return watchUntilTerminal, nil
	default:
		return "", fmt.Errorf("unknown --until value %q (valid: attention, terminal)", value)
	}
}

func newAxiWatchCmd() *cobra.Command {
	var runID, untilValue string
	cmd := &cobra.Command{Use: "watch", Short: "Watch one run until attention is required or it ends", Args: cobra.NoArgs, SilenceErrors: true, SilenceUsage: true}
	cmd.Flags().StringVar(&runID, "run", "", "run id to watch (required)")
	cmd.Flags().StringVar(&untilValue, "until", "", "attention (default) | terminal")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		return trackReadSurface("axi-watch", telemetry.Fields{"until": strings.TrimSpace(untilValue)}, func() (string, string, error) {
			return runAxiWatch(cmd, strings.TrimSpace(runID), untilValue)
		})
	}
	return cmd
}

func runAxiWatch(cmd *cobra.Command, runID, untilValue string) (string, string, error) {
	if runID == "" {
		return "invalid-run", "", emitError(cmd, 2, "--run is required", "Run `no-mistakes axi watch --run <id> --until attention|terminal`")
	}
	until, err := parseWatchUntil(untilValue)
	if err != nil {
		return "invalid-until", "", emitError(cmd, 2, err.Error())
	}
	p, d, err := openResources()
	if err != nil {
		return watchErrorFingerprint(until, "resources-error"), "", emitError(cmd, 1, err.Error())
	}
	defer d.Close()
	dbRun, err := d.GetRun(runID)
	if err != nil {
		return watchErrorFingerprint(until, "read-error"), "", emitError(cmd, 1, fmt.Sprintf("get run: %v", err))
	}
	if dbRun == nil {
		return watchErrorFingerprint(until, "not-found"), "", emitError(cmd, 1, fmt.Sprintf("run %q not found", runID))
	}
	steps, _ := d.GetStepsByRun(runID)
	initial := runViewFromDB(dbRun, steps)
	if terminalStatus(initial.Status) {
		return watchResultFingerprint(until, initial, "terminal"), "", renderWatchResult(cmd, initial, "terminal")
	}
	if alive, _ := daemon.IsRunning(p); !alive {
		return watchErrorFingerprint(until, "daemon-unavailable"), "", emitError(cmd, 1, "daemon is not running; watch did not start it")
	}
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		return watchErrorFingerprint(until, "connect-error"), "", emitError(cmd, 1, fmt.Sprintf("connect to daemon: %v", err))
	}
	defer client.Close()
	cfg, err := config.LoadGlobal(p.ConfigFile())
	if err != nil {
		cfg = config.DefaultGlobalConfig()
	}
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	read := func() (*ipc.RunInfo, error) { return getRunInfo(client, runID) }
	if run, err := read(); err != nil {
		return watchErrorFingerprint(until, "read-error"), "", emitError(cmd, 1, fmt.Sprintf("read run: %v", err))
	} else if done, reason := watchReason(watchRunView(p, run), cfg.StepQuietWarning, ciLogReader(p)); done && until == watchUntilAttention {
		rv := watchRunView(p, run)
		return watchResultFingerprint(until, rv, reason), "", renderWatchResult(cmd, rv, reason)
	}
	events, cancel, err := ipc.SubscribeContext(ctx, p.Socket(), &ipc.SubscribeParams{RunID: runID})
	if err != nil {
		if ctx.Err() != nil {
			return watchErrorFingerprint(until, "interrupted"), "", renderWatchInterrupted(cmd)
		}
		return watchErrorFingerprint(until, "subscribe-error"), "", emitError(cmd, 1, fmt.Sprintf("subscribe run: %v", err))
	}
	defer cancel()
	quietLatched := false
	for {
		run, err := read()
		if err != nil {
			return watchErrorFingerprint(until, "read-error"), "", emitError(cmd, 1, fmt.Sprintf("read run: %v", err))
		}
		rv := watchRunView(p, run)
		if done, reason := watchReason(rv, cfg.StepQuietWarning, ciLogReader(p)); done {
			if reason == "terminal" {
				return watchResultFingerprint(until, rv, reason), "", renderWatchResult(cmd, rv, reason)
			}
			if until == watchUntilAttention {
				return watchResultFingerprint(until, rv, reason), "", renderWatchResult(cmd, rv, reason)
			}
			if reason == "quiet" {
				quietLatched = true
			}
		}
		var timer <-chan time.Time
		if !quietLatched {
			if delay := watchQuietDelay(rv, cfg.StepQuietWarning); delay >= 0 {
				timer = time.After(delay)
			}
		}
		select {
		case <-ctx.Done():
			return watchErrorFingerprint(until, "interrupted"), "", renderWatchInterrupted(cmd)
		case _, ok := <-events:
			if !ok {
				final, e := read()
				if e != nil {
					return watchErrorFingerprint(until, "reconcile-error"), "", emitError(cmd, 1, fmt.Sprintf("reconcile closed stream: %v", e))
				}
				rv := runViewFromIPC(final)
				if terminalStatus(string(final.Status)) {
					return watchResultFingerprint(until, rv, "terminal"), "", renderWatchResult(cmd, rv, "terminal")
				}
				return watchResultFingerprint(until, rv, "stream-interrupted"), "", renderWatchInterruptedStream(cmd, rv)
			}
			quietLatched = false
		case <-timer:
		}
	}
}

func watchResultFingerprint(until watchUntil, rv runView, reason string) string {
	return string(until) + "|" + reason + "|" + runStateFingerprint(rv)
}

func watchErrorFingerprint(until watchUntil, outcome string) string {
	return string(until) + "|" + outcome
}

func watchRunView(p *paths.Paths, run *ipc.RunInfo) runView {
	rv := runViewFromIPC(run)
	applyLastActivityFallback(p, &rv)
	return rv
}

func watchReason(rv runView, quiet time.Duration, logs func(string) []string) (bool, string) {
	for i := range rv.Steps {
		rv.Steps[i].QuietWarning = quiet
	}
	if terminalStatus(rv.Status) {
		return true, "terminal"
	}
	if _, ok := rv.awaitingStep(); ok {
		return true, "gate"
	}
	if ciReadyToMerge(rv, logs(rv.ID)) {
		return true, "checks-passed"
	}
	if watchQuietDelay(rv, quiet) == 0 {
		return true, "quiet"
	}
	return false, ""
}
func watchQuietDelay(rv runView, quiet time.Duration) time.Duration {
	if quiet <= 0 {
		return -1
	}
	var next time.Duration = -1
	now := watchNow()
	for _, s := range rv.Steps {
		if (s.Status == "running" || s.Status == "fixing") && s.LastActivityAt != nil {
			d := time.Unix(*s.LastActivityAt, 0).Add(quiet).Sub(now)
			if d < 0 {
				d = 0
			}
			if next < 0 || d < next {
				next = d
			}
		}
	}
	return next
}
func renderWatchResult(cmd *cobra.Command, rv runView, reason string) error {
	fields := []toon.Field{runObjectField(rv), {Key: "watch", Value: toon.NewObject(toon.Field{Key: "stop", Value: reason}, toon.Field{Key: "terminal", Value: terminalStatus(rv.Status)}, toon.Field{Key: "supervision", Value: "active_agent_required"}, toon.Field{Key: "auto_resumed", Value: false})}}
	if gate, ok := rv.awaitingStep(); ok {
		fields = append(fields, watchGateFields(gate)...)
	} else if terminalStatus(rv.Status) {
		fields = append(fields, toon.Field{Key: "outcome", Value: outcomeFor(rv.Status)})
		if rv.Error != "" {
			fields = append(fields, toon.Field{Key: "error", Value: rv.Error})
		}
	}
	emitDoc(cmd, fields...)
	if rv.Status == "failed" || rv.Status == "cancelled" {
		return &exitError{code: 1}
	}
	return nil
}

// watchGateFields keeps a long-lived watch result bounded. Full finding detail
// remains available through axi logs, while the first findings are enough to
// identify the gate and decide whether to reattach or escalate.
func watchGateFields(gate stepView) []toon.Field {
	parsed, _ := types.ParseFindingsJSON(gate.FindingsJSON)
	gfields := []toon.Field{
		{Key: "step", Value: gate.Name},
		{Key: "status", Value: gate.Status},
		{Key: "findings_total", Value: len(parsed.Items)},
	}
	if parsed.Summary != "" {
		gfields = append(gfields, toon.Field{Key: "summary", Value: parsed.Summary})
	}
	if parsed.RiskLevel != "" {
		gfields = append(gfields, toon.Field{Key: "risk", Value: parsed.RiskLevel})
	}
	rows := make([]findingRow, 0, min(len(parsed.Items), maxWatchFindings))
	for i, f := range parsed.Items {
		if i == maxWatchFindings {
			break
		}
		rows = append(rows, findingRow{ID: f.ID, Severity: f.Severity, File: f.File, Action: f.Action, Description: truncate(f.Description, maxFindingDesc)})
	}
	gfields = append(gfields, toon.Field{Key: "findings", Value: rows})
	if len(parsed.Items) > maxWatchFindings {
		gfields = append(gfields, toon.Field{Key: "findings_truncated", Value: true})
	}
	return []toon.Field{
		{Key: "gate", Value: toon.NewObject(gfields...)},
		{Key: "help", Value: []string{
			"Run `no-mistakes axi respond --action approve` to accept this step and continue",
			"Run `no-mistakes axi respond --action fix --findings <ids>` to have the pipeline fix the selected findings (do not edit files yourself)",
			"Run `no-mistakes axi respond --action skip` to skip this step",
			fmt.Sprintf("Run `no-mistakes axi logs --step %s --full` to read the full step log", gate.Name),
			"Keep this active agent turn in the foreground: after responding, start `no-mistakes axi watch --run <id> --until attention` again for the same run.",
		}},
	}
}
func renderWatchInterrupted(cmd *cobra.Command) error {
	emitDoc(cmd, toon.Field{Key: "watch", Value: toon.NewObject(toon.Field{Key: "stop", Value: "interrupted"}, toon.Field{Key: "terminal", Value: false}, toon.Field{Key: "supervision", Value: "active_agent_required"}, toon.Field{Key: "auto_resumed", Value: false})})
	return &exitError{code: 130}
}
func renderWatchInterruptedStream(cmd *cobra.Command, rv runView) error {
	emitDoc(cmd, runObjectField(rv), toon.Field{Key: "watch", Value: toon.NewObject(toon.Field{Key: "stop", Value: "stream-interrupted"}, toon.Field{Key: "terminal", Value: false}, toon.Field{Key: "supervision", Value: "active_agent_required"}, toon.Field{Key: "auto_resumed", Value: false})})
	return &exitError{code: 1}
}
