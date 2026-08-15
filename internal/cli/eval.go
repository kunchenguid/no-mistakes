package cli

import (
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/config"
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
		Long:  "Inspect automatically collected review cases, capture runs on demand, ingest confirmed post-PR misses as false-negative gold, and compare agent+model candidates. Eval never starts or uses the shared daemon.",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(newEvalCaptureCmd())
	cmd.AddCommand(newEvalMissCmd())
	cmd.AddCommand(newEvalRunCmd())
	cmd.AddCommand(newEvalSetsCmd())
	cmd.AddCommand(newEvalReportCmd())
	cmd.AddCommand(newEvalRelabelCmd())
	return cmd
}

func openEvalStore() (*paths.Paths, *eval.Store, error) {
	p, err := paths.New()
	if err != nil {
		return nil, nil, fmt.Errorf("resolve eval paths: %w", err)
	}
	store, err := eval.Open(p.EvalDir())
	if err != nil {
		return nil, nil, err
	}
	cfg, err := config.LoadGlobal(p.ConfigFile())
	if err != nil {
		_ = store.Close()
		return nil, nil, err
	}
	store.SetDiversifiedSize(cfg.Eval.DiversifiedSize)
	return p, store, nil
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
			if cfg, cfgErr := config.LoadGlobal(p.ConfigFile()); cfgErr == nil {
				store.SetDiversifiedSize(cfg.Eval.DiversifiedSize)
			}
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

func newEvalMissCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "miss",
		Short: "Ingest confirmed post-PR review misses as false-negative gold",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(newEvalMissIngestCmd())
	return cmd
}

func newEvalMissIngestCmd() *cobra.Command {
	var findings []string
	cmd := &cobra.Command{
		Use:   "ingest <run> --finding <json>",
		Short: "Capture a green review pass and label confirmed post-PR misses as false-negative gold",
		Long:  "Reads confirmed post-PR misses from --finding JSON (repeatable), not from GitHub comments or an external ledger. Captures the named run if needed, then writes false-negative gold onto the last completed non-blocking review pass.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(findings) == 0 {
				return fmt.Errorf("at least one --finding is required")
			}
			misses := make([]eval.FindingGold, 0, len(findings))
			for _, raw := range findings {
				miss, err := eval.ParsePostPRMissFinding(raw)
				if err != nil {
					return err
				}
				misses = append(misses, miss)
			}
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
			result, err := eval.IngestPostPRMiss(cmd.Context(), store, p, database, args[0], misses)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "ingested %d false-negative gold finding(s) into case %s (%d total)\n", result.Added, result.CaseID, result.Total)
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&findings, "finding", nil, "confirmed miss as JSON finding object (repeatable)")
	return cmd
}

func newEvalRunCmd() *cobra.Command {
	var cases string
	var candidateRaw string
	var repeats int
	cmd := &cobra.Command{
		Use:   "run --cases <all|labeled|diversified|tune> --candidate <agent+model>",
		Short: "Replay captured review passes and score findings against gold",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			candidate, err := eval.ParseCandidate(candidateRaw)
			if err != nil {
				return err
			}
			_, store, err := openEvalStore()
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
	cmd.Flags().StringVar(&cases, "cases", "", "case set: all, labeled (finding-level gold), diversified (official gold-only holdout), or tune")
	cmd.Flags().StringVar(&candidateRaw, "candidate", "", "candidate as agent+model (for example codex+gpt-5.4)")
	cmd.Flags().IntVar(&repeats, "repeats", 3, "replays per case (minimum 1)")
	_ = cmd.MarkFlagRequired("cases")
	_ = cmd.MarkFlagRequired("candidate")
	return cmd
}

func newEvalSetsCmd() *cobra.Command {
	var refresh bool
	cmd := &cobra.Command{
		Use:   "sets",
		Short: "Inspect local case-set size, finding-level gold, and composition",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, store, err := openEvalStore()
			if err != nil {
				return err
			}
			defer store.Close()
			if refresh {
				if _, err := store.RefreshDiversified(); err != nil {
					return err
				}
			}
			summaries, err := eval.InspectSets(store)
			if err != nil {
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), eval.RenderSets(summaries))
			return nil
		},
	}
	cmd.Flags().BoolVar(&refresh, "refresh-diversified", false, "rebuild the official diversified pin set from current gold")
	return cmd
}

func newEvalReportCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "report",
		Short: "Report local true-positive / false-negative scores, tokens, and cost",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, store, err := openEvalStore()
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

func newEvalRelabelCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "relabel [run]",
		Short: "Refresh gold labels when a source PR has merged after capture",
		Args:  cobra.MaximumNArgs(1),
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
			if cfg, cfgErr := config.LoadGlobal(p.ConfigFile()); cfgErr == nil {
				store.SetDiversifiedSize(cfg.Eval.DiversifiedSize)
			}
			var cases []eval.Case
			if len(args) == 1 {
				cases, err = eval.RelabelRun(cmd.Context(), store, p, database, args[0])
			} else {
				cases, err = eval.RelabelAll(cmd.Context(), store, p, database)
			}
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "relabeled %d local review case(s)\n", len(cases))
			return nil
		},
	}
}
