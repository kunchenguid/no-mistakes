package cli

import (
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/spf13/cobra"
)

const (
	statsBoxWidth     = 61
	statsContentWidth = statsBoxWidth - 4
	statsBarWidth     = 30
	statsRepoBarWidth = 10
)

func newStatsCmd() *cobra.Command {
	var agents bool
	var runID string
	cmd := &cobra.Command{
		Use:   "stats",
		Short: "Show historical no-mistakes usage stats",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return trackCommand("stats", func() error {
				_, database, err := openResources()
				if err != nil {
					return err
				}
				defer database.Close()

				if agents || runID != "" {
					return renderAgentPerfReport(cmd.OutOrStdout(), database, runID)
				}

				stats, err := database.GetStats()
				if err != nil {
					return fmt.Errorf("get stats: %w", err)
				}

				fmt.Fprintln(cmd.OutOrStdout(), renderStatsDashboard(stats))
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&agents, "agents", false, "show local agent performance telemetry (per-purpose invocation aggregates)")
	cmd.Flags().StringVar(&runID, "run", "", "show one run's agent invocations and parked time (implies --agents)")
	return cmd
}

// renderAgentPerfReport prints the local performance telemetry: per-purpose
// invocation aggregates, or one run's per-invocation detail with its
// accumulated parked-at-gate time. This is read-only local evidence; none of
// it is sent to remote analytics.
func renderAgentPerfReport(w io.Writer, database *db.DB, runID string) error {
	if runID != "" {
		return renderRunAgentPerf(w, database, runID)
	}

	aggregates, err := database.AgentInvocationAggregates()
	if err != nil {
		return fmt.Errorf("agent invocation aggregates: %w", err)
	}
	if len(aggregates) == 0 {
		fmt.Fprintln(w, "no agent invocations recorded yet")
		return nil
	}

	// Table 1: session modes and token totals.
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "PURPOSE\tCOUNT\tAVG\tTOTAL\tCOLD\tSTARTED\tRESUMED\tFALLBACK\tERRORS\tIN TOK\tOUT TOK\tCACHE READ TOK\tCACHE WRITE TOK\tFRESH IN TOK\tREASON TOK")
	for _, a := range aggregates {
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%s\t%s\t%s\n",
			a.Purpose, a.Count,
			formatMS(a.AvgDurationMS), formatMS(a.TotalDurationMS),
			a.Cold, a.Started, a.Resumed, a.Fallback, a.Errors,
			a.InputTokens, a.OutputTokens, a.CacheReadTokens, optInt64(a.CacheCreationTokens),
			optInt64(a.FreshInputTokens), optInt64(a.ReasoningTokens),
		)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	// Table 2: subprocess-vs-model time and the bounded tool-call histogram.
	// METRICS is how many of COUNT rows carried activity metrics, so a zero can
	// be told apart from missing instrumentation (older rows, other adapters).
	fmt.Fprintln(w)
	tw = tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "PURPOSE\tMETRICS\tSUBPROC\tROUNDTRIPS\tTOOLS\tWAIT\tTEST/LINT\tEDIT\tREAD\tGIT\tOTHER")
	for _, a := range aggregates {
		metricsCov := fmt.Sprintf("%d/%d", a.MetricsRows, a.Count)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			a.Purpose, metricsCov, optMS(a.SubprocessWaitMS),
			optInt64(a.ModelRoundtrips), optInt64(a.ToolCalls),
			optInt64(a.ToolWaitCalls), optInt64(a.ToolTestLintCalls), optInt64(a.ToolEditCalls), optInt64(a.ToolReadCalls), optInt64(a.ToolGitCalls), optInt64(a.ToolOtherCalls),
		)
	}
	return tw.Flush()
}

func renderRunAgentPerf(w io.Writer, database *db.DB, runID string) error {
	run, err := database.GetRun(runID)
	if err != nil {
		return fmt.Errorf("get run: %w", err)
	}
	if run == nil {
		return fmt.Errorf("run %q not found", runID)
	}
	invocations, err := database.GetAgentInvocationsByRun(runID)
	if err != nil {
		return fmt.Errorf("get agent invocations: %w", err)
	}

	fmt.Fprintf(w, "run %s (%s), parked at gates %s total\n", run.ID, run.Status, formatMS(run.ParkedMS))
	if len(invocations) == 0 {
		fmt.Fprintln(w, "no agent invocations recorded for this run")
		return nil
	}
	fmt.Fprintln(w, "\"-\" means the field was not reported for that invocation (unknown), which is distinct from a recorded 0.")

	// Table 1: session, timing split, activity, workload, and findings.
	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "STEP\tROUND\tPURPOSE\tAGENT\tMODEL\tSESSION\tKEY\tDURATION\tMODEL\tSUBPROC\tRT\tTOOLS (w/t/e/r/g/o)\tFIND\tWORK (f/l)\tFALLBACK\tEXIT")
	for _, inv := range invocations {
		exit := inv.ExitStatus
		if inv.FailureCategory != "" && inv.FailureCategory != inv.ExitStatus {
			exit += "/" + inv.FailureCategory
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			inv.StepName, inv.Round, inv.Purpose, inv.Agent, orUnknown(inv.Model),
			inv.SessionMode, inv.SessionKey,
			formatMS(inv.DurationMS), formatModelTime(inv), optMS(inv.SubprocessWaitMS),
			optInt(inv.ModelRoundtrips), formatToolHistogram(inv), optInt(inv.FindingCount),
			formatWorkload(inv), orUnknown(deref(inv.FallbackReason)), exit,
		)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	// Table 2: per-round token deltas next to the raw (cumulative for resumed
	// sessions) counters, so a cumulative counter cannot be misread as per-round.
	fmt.Fprintln(w)
	tw = tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "STEP\tROUND\tPURPOSE\tSESSION\tΔ IN (round)\tΔ OUT\tΔ CACHE RD\tIN (raw)\tOUT (raw)\tCACHE RD (raw)\tCACHE WR\tFRESH IN\tREASON")
	for _, inv := range invocations {
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%s\t%s\t%d\t%d\t%d\t%s\t%s\t%s\n",
			inv.StepName, inv.Round, inv.Purpose, inv.SessionMode,
			optInt(inv.DeltaInputTokens), optInt(inv.DeltaOutputTokens), optInt(inv.DeltaCacheReadTokens),
			inv.InputTokens, inv.OutputTokens, inv.CacheReadTokens,
			optInt(inv.CacheCreationTokens), optInt(inv.FreshInputTokens), optInt(inv.ReasoningTokens),
		)
	}
	return tw.Flush()
}

func formatMS(ms int64) string {
	return time.Duration(ms * int64(time.Millisecond)).Round(100 * time.Millisecond).String()
}

// optInt renders a nullable count: "-" (unknown) when nil, else the number.
func optInt(p *int) string {
	if p == nil {
		return "-"
	}
	return strconv.Itoa(*p)
}

func optInt64(p *int64) string {
	if p == nil {
		return "-"
	}
	return strconv.FormatInt(*p, 10)
}

// optMS renders a nullable duration: "-" when nil, else a rounded duration.
func optMS(p *int64) string {
	if p == nil {
		return "-"
	}
	return formatMS(*p)
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func orUnknown(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// formatModelTime shows model/reasoning wall-clock (duration minus subprocess
// wait). It is unknown when the invocation reported no subprocess-wait split.
func formatModelTime(inv db.AgentInvocation) string {
	if inv.SubprocessWaitMS == nil {
		return "-"
	}
	return formatMS(agent.ModelTimeMS(inv.DurationMS, *inv.SubprocessWaitMS))
}

// formatToolHistogram renders "total w/t/e/r/g/o" or "-" when the invocation
// reported no activity metrics.
func formatToolHistogram(inv db.AgentInvocation) string {
	if inv.ToolCalls == nil {
		return "-"
	}
	return fmt.Sprintf("%d %s/%s/%s/%s/%s/%s", *inv.ToolCalls,
		optInt(inv.ToolWaitCalls), optInt(inv.ToolTestLintCalls), optInt(inv.ToolEditCalls),
		optInt(inv.ToolReadCalls), optInt(inv.ToolGitCalls), optInt(inv.ToolOtherCalls))
}

// formatWorkload renders "files/lines" or "-" when unknown.
func formatWorkload(inv db.AgentInvocation) string {
	if inv.WorkloadFiles == nil && inv.WorkloadLines == nil {
		return "-"
	}
	return fmt.Sprintf("%s/%s", optInt(inv.WorkloadFiles), optInt(inv.WorkloadLines))
}

func renderStatsDashboard(stats *db.Stats) string {
	var lines []string
	lines = append(lines, "")
	lines = append(lines, centeredStatsBlock(strings.Split(banner, "\n"))...)
	lines = append(lines, "", "")

	rescueRate := ratio(stats.RescueRuns, stats.TotalRuns)
	fixRate := ratio(stats.FixedFindings, stats.ReportedFindings)
	repoDetail := "across all repos"
	if stats.TotalRepos > 0 {
		repoDetail = fmt.Sprintf("across %d repos", stats.TotalRepos)
	}
	lines = append(lines,
		metricStatsLine("Total changes", fmt.Sprintf("%d", stats.TotalRuns), repoDetail),
		metricStatsLine("Rescued changes", fmt.Sprintf("%d", stats.RescueRuns), "mistake caught + fixed"),
		metricStatsLine("Rescue rate", percent(rescueRate), progressBar(rescueRate, statsBarWidth)),
		"",
		"  Mistakes",
		metricStatsLine("Reported", fmt.Sprintf("%d", stats.ReportedFindings), progressBar(ratio(stats.ReportedFindings, stats.ReportedFindings), statsBarWidth)),
		metricStatsLine("Fixed", percent(fixRate), progressBar(fixRate, statsBarWidth)),
		"",
		"  Fixes by step",
	)

	maxStepFixes := maxStepFixedFindings(stats.StepStats)
	for _, step := range pipelineOrderedStepStats(stats.StepStats) {
		if step.FixedFindings == 0 {
			continue
		}
		lines = append(lines, metricStatsLine(string(step.StepName), fmt.Sprintf("%d", step.FixedFindings), progressBar(ratio(step.FixedFindings, maxStepFixes), statsBarWidth)))
	}

	lines = append(lines, "", "  Top repos")
	maxRepoFixes := maxRepoFixedFindings(stats.RepoStats)
	repoCount := 0
	for _, repo := range stats.RepoStats {
		if repo.Runs == 0 {
			continue
		}
		lines = append(lines, repoStatsLine(repo, maxRepoFixes))
		repoCount++
		if repoCount == 3 {
			break
		}
	}
	if repoCount == 0 {
		lines = append(lines, "  no runs yet")
	}
	lines = append(lines, "")

	return renderStatsBox(lines)
}

func renderStatsBox(lines []string) string {
	var b strings.Builder
	eyebrow := " git push no-mistakes "
	b.WriteString("╭─" + eyebrow + strings.Repeat("─", statsBoxWidth-3-lipgloss.Width(eyebrow)) + "╮\n")
	for _, line := range lines {
		b.WriteString(renderStatsBoxLine(line))
		b.WriteByte('\n')
	}
	b.WriteString("╰" + strings.Repeat("─", statsBoxWidth-2) + "╯")
	return b.String()
}

func renderStatsBoxLine(line string) string {
	width := lipgloss.Width(line)
	if width > statsContentWidth {
		line = truncateStatsLine(line, statsContentWidth)
		width = lipgloss.Width(line)
	}
	return "│ " + line + strings.Repeat(" ", statsContentWidth-width) + " │"
}

func centerStatsLine(line string) string {
	width := lipgloss.Width(line)
	if width >= statsContentWidth {
		return line
	}
	return strings.Repeat(" ", (statsContentWidth-width)/2) + line
}

func centeredStatsBlock(lines []string) []string {
	maxWidth := 0
	for _, line := range lines {
		if width := lipgloss.Width(line); width > maxWidth {
			maxWidth = width
		}
	}
	if maxWidth >= statsContentWidth {
		return lines
	}
	indent := strings.Repeat(" ", (statsContentWidth-maxWidth)/2)
	centered := make([]string, 0, len(lines))
	for _, line := range lines {
		centered = append(centered, indent+sCyan.Render(line))
	}
	return centered
}

func metricStatsLine(label, value, detail string) string {
	return fmt.Sprintf("  %-16s %5s   %s", label, value, detail)
}

func repoStatsLine(repo db.RepoStats, maxFixes int) string {
	name := truncateStatsLine(repo.DisplayName(), 16)
	return fmt.Sprintf("  %-16s %5d rescue %5d fixes   %s", name, repo.RescueRuns, repo.FixedFindings, progressBar(ratio(repo.FixedFindings, maxFixes), statsRepoBarWidth))
}

func progressBar(value float64, width int) string {
	if value < 0 {
		value = 0
	}
	if value > 1 {
		value = 1
	}
	filled := int(math.Round(value * float64(width)))
	if filled > width {
		filled = width
	}
	return sGreen.Render(strings.Repeat("█", filled)) + sDim.Render(strings.Repeat("░", width-filled))
}

func percent(value float64) string {
	return fmt.Sprintf("%d%%", int(math.Round(value*100)))
}

func ratio(value, total int) float64 {
	if total <= 0 {
		return 0
	}
	return float64(value) / float64(total)
}

func maxStepFixedFindings(stats []db.StepStats) int {
	maxValue := 0
	for _, stat := range stats {
		if stat.FixedFindings > maxValue {
			maxValue = stat.FixedFindings
		}
	}
	return maxValue
}

func pipelineOrderedStepStats(stats []db.StepStats) []db.StepStats {
	byStep := make(map[types.StepName]db.StepStats, len(stats))
	for _, stat := range stats {
		byStep[stat.StepName] = stat
	}
	ordered := make([]db.StepStats, 0, len(stats))
	seen := make(map[types.StepName]bool, len(stats))
	for _, step := range types.AllSteps() {
		stat, ok := byStep[step]
		if !ok {
			continue
		}
		ordered = append(ordered, stat)
		seen[step] = true
	}
	for _, stat := range stats {
		if seen[stat.StepName] {
			continue
		}
		ordered = append(ordered, stat)
	}
	return ordered
}

func maxRepoFixedFindings(stats []db.RepoStats) int {
	maxValue := 0
	for _, stat := range stats {
		if stat.FixedFindings > maxValue {
			maxValue = stat.FixedFindings
		}
	}
	return maxValue
}

func truncateStatsLine(value string, width int) string {
	if lipgloss.Width(value) <= width {
		return value
	}
	runes := []rune(value)
	for len(runes) > 0 && lipgloss.Width(string(runes)) > width {
		runes = runes[:len(runes)-1]
	}
	return string(runes)
}
