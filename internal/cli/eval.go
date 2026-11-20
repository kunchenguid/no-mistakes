package cli

import (
	"encoding/json"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
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
		Long:  "Inspect automatically collected review cases, capture runs on demand, ingest confirmed post-PR misses as false-negative gold, and compare agent candidates pinned to an explicit model and reasoning effort. Eval never starts or uses the shared daemon.",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(newEvalCaptureCmd())
	cmd.AddCommand(newEvalMissCmd())
	cmd.AddCommand(newEvalRunCmd())
	cmd.AddCommand(newEvalSetsCmd())
	cmd.AddCommand(newEvalReportCmd())
	cmd.AddCommand(newEvalRelabelCmd())
	cmd.AddCommand(newEvalLabelsCmd())
	cmd.AddCommand(newEvalAdjudicateCmd())
	return cmd
}

func newEvalLabelsCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "labels [case]",
		Short: "Inspect gold labels, provenance, and explicit adjudications",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, store, err := openEvalStore()
			if err != nil {
				return err
			}
			defer store.Close()
			var cases []eval.Case
			if len(args) == 1 {
				c, err := store.CaseByID(args[0])
				if err != nil {
					return err
				}
				cases = []eval.Case{c}
			} else {
				cases, err = store.ListCases("all")
				if err != nil {
					return err
				}
			}
			type labelRow struct {
				CaseID        string           `json:"case_id"`
				Index         int              `json:"index"`
				Gold          eval.FindingGold `json:"gold"`
				Provenance    string           `json:"provenance"`
				EffectiveKind string           `json:"effective_kind"`
			}
			rows := []labelRow{}
			for _, c := range cases {
				for i, gold := range c.Labels.Findings {
					kind := gold.Kind
					if a := gold.Adjudication; a != nil {
						kind = a.Kind
						if a.Excluded {
							kind = "excluded"
						}
					}
					rows = append(rows, labelRow{c.ID, i + 1, gold, gold.GoldProvenance(), kind})
				}
			}
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(rows)
			}
			for _, row := range rows {
				fmt.Fprintf(cmd.OutOrStdout(), "case %s index %d id %q: %s [%s]; captured %s source %q\n  %s:%d %s\n", row.CaseID, row.Index, row.Gold.ID, row.EffectiveKind, row.Provenance, row.Gold.Kind, row.Gold.Source, row.Gold.File, row.Gold.Line, row.Gold.Description)
				if a := row.Gold.Adjudication; a != nil {
					fmt.Fprintf(cmd.OutOrStdout(), "  adjudication at %d: %s\n", a.DecidedAt, a.Reason)
				}
			}
			if len(rows) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no gold labels recorded")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit labels with captured evidence, effective kind, and provenance as JSON")
	return cmd
}

func newEvalAdjudicateCmd() *cobra.Command {
	var finding, kind, reason string
	var index int
	var exclude bool
	cmd := &cobra.Command{
		Use:   "adjudicate <case> --finding <id> (--kind <kind> | --exclude) --reason <text>",
		Short: "Correct, confirm, or exclude local gold without deleting evidence",
		Long:  "Records an explicit local curator decision with a reason. Use --index for ID-less or ambiguous findings shown by eval labels. A later --kind re-includes an excluded label. Reports rescore saved outputs for this case against current labels; source outputs and measurements remain intact.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.Flags().Changed("index") && index <= 0 {
				return fmt.Errorf("--index must be positive (one-based)")
			}
			_, store, err := openEvalStore()
			if err != nil {
				return err
			}
			defer store.Close()
			c, err := store.Adjudicate(cmd.Context(), args[0], finding, index, eval.Adjudication{Kind: kind, Excluded: exclude, Reason: reason})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "adjudicated case %s: %d active gold label(s); captured evidence retained\n", c.ID, len(c.Labels.ActiveFindings()))
			return nil
		},
	}
	cmd.Flags().StringVar(&finding, "finding", "", "exact gold finding ID")
	cmd.Flags().IntVar(&index, "index", 0, "one-based gold label position from eval labels (alternative to --finding)")
	cmd.Flags().StringVar(&kind, "kind", "", "gold kind: true-positive, false-positive, or false-negative")
	cmd.Flags().BoolVar(&exclude, "exclude", false, "exclude the label from scoring and gold-only sets, retaining evidence")
	cmd.Flags().StringVar(&reason, "reason", "", "required explanation for this explicit label decision")
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
	store.SetRepoNames(evalRepoNames(p))
	return p, store, nil
}

// evalRepoNames resolves the repository fingerprints a case carries back to
// human names from the locally registered repositories. It is best effort and
// display-only: when the pipeline database cannot be read, the dashboards fall
// back to the fingerprint rather than failing an eval command that otherwise
// needs no database at all.
//
// The read goes through db.OpenReadOnly, never db.Open. db.Open creates the
// database and runs every migration, so routing a display lookup through it
// made `eval sets`, `eval report`, and `eval run` initialize pipeline state on
// a machine that has none, and migrate the schema of a database a running
// daemon owns. A missing database is the ordinary case here (os.IsNotExist),
// not an error worth reporting: the dashboards simply show fingerprints.
func evalRepoNames(p *paths.Paths) map[string]string {
	database, err := db.OpenReadOnly(p.DB())
	if err != nil {
		return nil
	}
	defer database.Close()
	repos, err := database.GetRepos()
	if err != nil {
		return nil
	}
	return eval.RepoDisplayNames(repos)
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
	cmd.Flags().StringArrayVar(&findings, "finding", nil, "confirmed miss as JSON finding object with id and description, optional file, line, severity (error|warning|info, default error) and action (auto-fix|ask-user|no-op) (repeatable)")
	return cmd
}

func newEvalRunCmd() *cobra.Command {
	var cases string
	var candidateRaw string
	var repeats int
	cmd := &cobra.Command{
		Use:   "run --cases <all|labeled|diversified|tune> --candidate <agent,model=...[,effort=...]>",
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
			out := cmd.OutOrStdout()
			caseCount := 0
			session, evaluations, runErr := eval.Replay(cmd.Context(), store, eval.ReplayOptions{
				Set:       cases,
				Candidate: candidate,
				Repeats:   repeats,
				OnPlan: func(session eval.Session, planned []eval.Case) {
					caseCount = len(planned)
					fmt.Fprintf(out, "replaying %d case(s) x %d repeat(s) with %s on %s (cohort %s)\n\n",
						len(planned), session.Repeats, session.Candidate, session.Set, session.Cohort)
				},
				OnResult: func(evaluation eval.Evaluation, completed, total int) {
					evalRunProgress(out, evaluation, completed, total)
				},
			})
			if len(evaluations) > 0 {
				fmt.Fprintln(out)
				fmt.Fprintln(out, renderEvalRunSummary(session, evaluations, caseCount))
			}
			fmt.Fprintf(out, "local eval session %s: %d replay(s), candidate %s, repeats %d\n", session.ID, len(evaluations), candidate, repeats)
			if runErr != nil {
				return runErr
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&cases, "cases", "", "case set: all, labeled (finding-level gold), diversified (official gold-only holdout), or tune")
	cmd.Flags().StringVar(&candidateRaw, "candidate", "", eval.CandidateUsage())
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
			fmt.Fprintln(cmd.OutOrStdout(), renderEvalSetsDashboard(summaries))
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
