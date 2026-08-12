package cli

import (
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/eval"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/spf13/cobra"
)

// newEvalCmd deliberately does not call trackCommand. Eval cases and candidate
// results are local code data, so this command surface has no remote telemetry
// event and does not use the daemon.
func newEvalCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "eval",
		Short: "Inspect and locally replay review evaluation cases",
		Long:  "Inspect automatically collected review cases, capture runs on demand, and compare agent+model candidates. Eval never starts or uses the shared daemon.",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(newEvalCaptureCmd())
	cmd.AddCommand(newEvalRunCmd())
	cmd.AddCommand(newEvalSetsCmd())
	cmd.AddCommand(newEvalReportCmd())
	return cmd
}

func newEvalCaptureCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "capture <run>",
		Short: "Capture every review pass from a run into local cases",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, database, err := openResources()
			if err != nil {
				return err
			}
			defer database.Close()
			store, err := eval.Open(p.EvalDir())
			if err != nil {
				return err
			}
			defer store.Close()
			cases, err := eval.Capture(cmd.Context(), store, p, database, args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "captured %d local review case(s)\n", len(cases))
			for _, c := range cases {
				fmt.Fprintf(cmd.OutOrStdout(), "  %s  run %s round %s\n", c.ID, c.SourceRunID, c.SourceRoundID)
			}
			return nil
		},
	}
}

func newEvalRunCmd() *cobra.Command {
	var cases string
	var candidateRaw string
	var repeats int
	cmd := &cobra.Command{
		Use:   "run --cases <all|labeled|diversified> --candidate <agent+model>",
		Short: "Replay captured review passes in an isolated local sandbox",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			candidate, err := eval.ParseCandidate(candidateRaw)
			if err != nil {
				return err
			}
			p, err := paths.New()
			if err != nil {
				return fmt.Errorf("resolve eval paths: %w", err)
			}
			store, err := eval.Open(p.EvalDir())
			if err != nil {
				return err
			}
			defer store.Close()
			session, evaluations, runErr := eval.Replay(cmd.Context(), store, eval.ReplayOptions{Set: cases, Candidate: candidate, Repeats: repeats})
			fmt.Fprintf(cmd.OutOrStdout(), "local eval session %s: %d replay(s), candidate %s, repeats %d\n", session.ID, len(evaluations), candidate, repeats)
			if runErr != nil {
				return runErr
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&cases, "cases", "", "case set: all, labeled, or diversified")
	cmd.Flags().StringVar(&candidateRaw, "candidate", "", "candidate as agent+model (for example codex+gpt-5.4)")
	cmd.Flags().IntVar(&repeats, "repeats", 3, "replays per case (minimum 1)")
	_ = cmd.MarkFlagRequired("cases")
	_ = cmd.MarkFlagRequired("candidate")
	return cmd
}

func newEvalSetsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "sets",
		Short: "Inspect local case-set size, labels, and diversified composition",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := paths.New()
			if err != nil {
				return fmt.Errorf("resolve eval paths: %w", err)
			}
			store, err := eval.Open(p.EvalDir())
			if err != nil {
				return err
			}
			defer store.Close()
			summaries, err := eval.InspectSets(store)
			if err != nil {
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), eval.RenderSets(summaries))
			return nil
		},
	}
}

func newEvalReportCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "report",
		Short: "Report local verdict accuracy, tokens, time, and cost frontier",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := paths.New()
			if err != nil {
				return fmt.Errorf("resolve eval paths: %w", err)
			}
			store, err := eval.Open(p.EvalDir())
			if err != nil {
				return err
			}
			defer store.Close()
			reports, err := eval.Report(store)
			if err != nil {
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), eval.RenderReport(reports))
			return nil
		},
	}
}
