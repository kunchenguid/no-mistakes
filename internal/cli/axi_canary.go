package cli

import (
	"fmt"

	"github.com/spf13/cobra"
	toon "github.com/toon-format/toon-go"

	"github.com/kunchenguid/no-mistakes/internal/db"
)

type canaryWorkloadRow struct {
	Run             string `toon:"run"`
	ExecutionMS     int64  `toon:"execution_ms"`
	InvocationMS    int64  `toon:"invocation_ms"`
	Escalations     int    `toon:"escalations"`
	Failovers       int    `toon:"failovers"`
	ChangedFiles    int    `toon:"changed_files"`
	ChangedLines    int    `toon:"changed_lines"`
	InitialFindings int    `toon:"initial_findings"`
}

// newAxiCanaryCmd shows the routing canary: the frozen baseline cohort (the ten
// runs before routing activated) versus the routed cohort (the first ten runs
// after), compared on the execution-only agent-bearing Step-round median. The
// 30% target is advisory and never changes routing or gate outcomes.
func newAxiCanaryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "canary",
		Short: "Show the required routing canary baseline/routed report",
		Long: "Reports the dormant routing canary: the frozen pre-routing baseline\n" +
			"cohort versus the first routed cohort, compared on the execution-only\n" +
			"agent-bearing Step-round median. This report is required. Until both\n" +
			"cohorts are complete, samples are preliminary. Preliminary samples\n" +
			"must not be treated as live results. The 30% improvement target is\n" +
			"advisory only and never changes Profiles, Routes, circuits, or gate outcomes.",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return trackAxiSurface("axi-canary", "/axi/canary", nil, func() error {
				return runAxiCanary(cmd)
			})
		},
	}
	return cmd
}

func runAxiCanary(cmd *cobra.Command) error {
	env, err := openAxiEnv(false)
	if err != nil {
		return emitError(cmd, 1, err.Error(), repoInitHelp(err)...)
	}
	defer env.close()

	report, err := env.d.GetCanaryReport()
	if err != nil {
		return emitError(cmd, 1, fmt.Sprintf("load canary report: %v", err))
	}

	fields := []toon.Field{
		{Key: "surface", Value: "routing-canary"},
		{Key: "report_required", Value: true},
	}
	if !report.Activated {
		fields = append(fields,
			toon.Field{Key: "activated", Value: false},
			toon.Field{Key: "target_reduction", Value: report.TargetReduction},
			toon.Field{Key: "target_advisory", Value: true},
			toon.Field{Key: "state", Value: "dormant: the routing cutover has not activated the canary yet"},
			toon.Field{Key: "comparison_complete", Value: false},
			toon.Field{Key: "result_state", Value: "dormant"},
		)
		fields = append(fields, canaryCohortFields("baseline", report.Baseline)...)
		fields = append(fields, canaryCohortFields("routed", report.Routed)...)
		fields = append(fields, toon.Field{Key: "target_met", Value: "pending"})
		emitDoc(cmd, fields...)
		return nil
	}

	fields = append(fields,
		toon.Field{Key: "activated", Value: true},
		toon.Field{Key: "target_reduction", Value: report.TargetReduction},
		toon.Field{Key: "target_advisory", Value: true},
		toon.Field{Key: "comparison_complete", Value: report.Baseline.Complete && report.Routed.Complete},
		toon.Field{Key: "result_state", Value: canaryResultState(report)},
	)
	fields = append(fields, canaryCohortFields("baseline", report.Baseline)...)
	fields = append(fields, canaryCohortFields("routed", report.Routed)...)

	var met any = "pending"
	if report.Met != nil {
		met = *report.Met
	}
	fields = append(fields, toon.Field{Key: "target_met", Value: met})
	emitDoc(cmd, fields...)
	return nil
}

func canaryResultState(report *db.CanaryReport) string {
	if report.Baseline.Complete && report.Routed.Complete {
		return "complete"
	}
	return "preliminary"
}

func canaryCohortFields(name string, c db.CanaryCohort) []toon.Field {
	escalations, failovers := 0, 0
	workloads := make([]canaryWorkloadRow, len(c.Runs))
	for i, r := range c.Runs {
		escalations += r.Escalations
		failovers += r.Failovers
		workloads[i] = canaryWorkloadRow{
			Run:             r.RunID,
			ExecutionMS:     r.ExecutionMS,
			InvocationMS:    r.InvocationMS,
			Escalations:     r.Escalations,
			Failovers:       r.Failovers,
			ChangedFiles:    r.ChangedFiles,
			ChangedLines:    r.ChangedLines,
			InitialFindings: r.InitialFindings,
		}
	}
	return []toon.Field{
		{Key: name + "_runs", Value: len(c.Runs)},
		{Key: name + "_complete", Value: c.Complete},
		{Key: name + "_median_exec_ms", Value: c.MedianExecMS},
		{Key: name + "_escalations", Value: escalations},
		{Key: name + "_failovers", Value: failovers},
		{Key: name + "_workloads", Value: workloads},
	}
}
