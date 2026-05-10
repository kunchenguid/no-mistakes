package cli

import (
	"fmt"
	"math"
	"strings"

	"github.com/charmbracelet/lipgloss"
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
	return &cobra.Command{
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

				stats, err := database.GetStats()
				if err != nil {
					return fmt.Errorf("get stats: %w", err)
				}

				fmt.Fprintln(cmd.OutOrStdout(), renderStatsDashboard(stats))
				return nil
			})
		},
	}
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
	b.WriteString("╭" + strings.Repeat("─", statsBoxWidth-2) + "╮\n")
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
