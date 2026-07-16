package cli

import (
	"fmt"
	"strings"

	toon "github.com/toon-format/toon-go"

	"github.com/kunchenguid/no-mistakes/internal/telemetry"
	"github.com/spf13/cobra"
)

type routeDecisionRow struct {
	ID              string `toon:"id"`
	Step            string `toon:"step"`
	Round           int    `toon:"round"`
	RequestedAgent  string `toon:"requested_harness"`
	EffectiveAgent  string `toon:"effective_harness"`
	RequestedModel  string `toon:"requested_model"`
	EffectiveModel  string `toon:"effective_model"`
	RequestedEffort string `toon:"requested_effort"`
	EffectiveEffort string `toon:"effective_effort"`
	Policy          string `toon:"policy_version"`
	Phase           string `toon:"phase"`
	Risk            string `toon:"risk"`
	Reason          string `toon:"reason"`
	SourceConfig    string `toon:"source_configuration"`
	Generation      string `toon:"configuration_generation"`
	Repository      string `toon:"repository"`
	PromptTransport string `toon:"prompt_transport"`
	CreatedAt       int64  `toon:"created_at"`
}

type routeResultRow struct {
	ID        string `toon:"id"`
	Step      string `toon:"step"`
	Round     int    `toon:"round"`
	Phase     string `toon:"phase"`
	Risk      string `toon:"risk"`
	CreatedAt int64  `toon:"created_at"`
}

func newAxiRouteEvidenceCmd() *cobra.Command {
	var runID string
	cmd := &cobra.Command{
		Use:           "route-evidence",
		Short:         "Show immutable route decisions and completed review classifications",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return trackReadSurface("axi-route-evidence", telemetry.Fields{
				"explicit_run_id": strings.TrimSpace(runID) != "",
			}, func() (string, string, error) {
				fingerprint, err := runAxiRouteEvidence(cmd, runID)
				return fingerprint, "", err
			})
		},
	}
	cmd.Flags().StringVar(&runID, "run", "", "run ID (default: active or most recent)")
	return cmd
}

func runAxiRouteEvidence(cmd *cobra.Command, runID string) (string, error) {
	env, err := openAxiEnv(false)
	if err != nil {
		return "", emitError(cmd, 1, err.Error(), repoInitHelp(err)...)
	}
	defer env.close()

	run, err := resolveRun(env, runID, currentBranchForRunResolve(cmd.Context()))
	if err != nil {
		return "", emitError(cmd, 1, err.Error())
	}
	if run == nil {
		return "", emitError(cmd, 1, "no run found to read route evidence from", startRunHelp())
	}
	decisions, err := env.d.RouteDecisions(run.ID)
	if err != nil {
		return "", emitError(cmd, 1, fmt.Sprintf("load route decisions: %v", err))
	}
	results, err := env.d.RouteResults(run.ID)
	if err != nil {
		return "", emitError(cmd, 1, fmt.Sprintf("load route results: %v", err))
	}
	decisionRows := make([]routeDecisionRow, 0, len(decisions))
	for _, decision := range decisions {
		decisionRows = append(decisionRows, routeDecisionRow{
			ID: decision.ID, Step: decision.StepName, Round: decision.Round,
			RequestedAgent: decision.RequestedHarness, EffectiveAgent: decision.EffectiveHarness,
			RequestedModel: decision.RequestedModel, EffectiveModel: decision.EffectiveModel,
			RequestedEffort: decision.RequestedEffort, EffectiveEffort: decision.EffectiveEffort,
			Policy: decision.PolicyVersion, Phase: decision.Phase, Risk: decision.Risk,
			Reason: decision.Reason, SourceConfig: decision.SourceConfiguration,
			Generation: decision.ConfigurationGeneration, Repository: decision.Repository,
			PromptTransport: decision.PromptTransport, CreatedAt: decision.CreatedAt,
		})
	}
	resultRows := make([]routeResultRow, 0, len(results))
	for _, result := range results {
		resultRows = append(resultRows, routeResultRow{
			ID: result.ID, Step: result.StepName, Round: result.Round,
			Phase: result.Phase, Risk: result.Risk, CreatedAt: result.CreatedAt,
		})
	}
	emitDoc(cmd,
		toon.Field{Key: "run", Value: run.ID},
		toon.Field{Key: "route_decisions", Value: decisionRows},
		toon.Field{Key: "review_results", Value: resultRows},
	)
	return run.ID + "|route-decisions:" + fmt.Sprint(len(decisionRows)) + "|review-results:" + fmt.Sprint(len(resultRows)), nil
}
