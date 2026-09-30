package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/scm/azuredevops"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/spf13/cobra"
)

type externalCIHandoff struct {
	Outcome          string                   `json:"outcome"`
	RunID            string                   `json:"run_id"`
	ExternalCIOwner  string                   `json:"external_ci_owner"`
	SourceRepo       string                   `json:"source_repo"`
	SourceBranch     string                   `json:"source_branch"`
	PendingCISupport []types.PendingCISupport `json:"pending_ci_support"`
}

func newAxiCIHandoffCmd() *cobra.Command {
	var runID string
	cmd := &cobra.Command{
		Use: "ci-handoff", Short: "Read an explicit pending external CI verification handoff as JSON",
		Args: cobra.NoArgs, SilenceErrors: true, SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if runID == "" {
				return emitCIHandoffError(cmd, "--run is required")
			}
			env, err := openAxiQueryEnv(runID)
			if err != nil {
				return emitCIHandoffError(cmd, err.Error())
			}
			defer env.close()
			run, err := env.d.GetRun(runID)
			if err != nil {
				return emitCIHandoffError(cmd, err.Error())
			}
			if run == nil {
				return emitCIHandoffError(cmd, "run not found")
			}
			handoff, err := buildExternalCIHandoff(env.d, run)
			if err != nil {
				return emitCIHandoffError(cmd, err.Error())
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(handoff)
		},
	}
	cmd.Flags().StringVar(&runID, "run", "", "exact run ID to inspect")
	return cmd
}

func emitCIHandoffError(cmd *cobra.Command, message string) error {
	_ = json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]string{"error": message})
	return &exitError{code: 1}
}

func buildExternalCIHandoff(database *db.DB, run *db.Run) (externalCIHandoff, error) {
	if run == nil || run.Status != types.RunCompleted || run.ExternalCIOwner != types.ExternalCIOwnerControllerShipPR {
		return externalCIHandoff{}, fmt.Errorf("run is not completed with explicit controller-ship-pr CI ownership")
	}
	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		return externalCIHandoff{}, err
	}
	status := map[types.StepName]types.StepStatus{}
	for _, step := range steps {
		if _, duplicate := status[step.StepName]; duplicate && (step.StepName == types.StepPush || step.StepName == types.StepPR || step.StepName == types.StepCI) {
			return externalCIHandoff{}, fmt.Errorf("duplicate delivery step in run")
		}
		status[step.StepName] = step.Status
	}
	for _, name := range []types.StepName{types.StepPush, types.StepPR, types.StepCI} {
		if status[name] != types.StepStatusSkipped {
			return externalCIHandoff{}, fmt.Errorf("run %s did not skip %s", run.ID, name)
		}
	}
	claims, err := database.PendingExternalCISupport(run)
	if err != nil {
		return externalCIHandoff{}, err
	}
	if len(claims) == 0 || len(claims) > 128 {
		return externalCIHandoff{}, fmt.Errorf("run must have 1-128 current pending Review CI claims")
	}
	repo, err := database.GetRepo(run.RepoID)
	if err != nil {
		return externalCIHandoff{}, fmt.Errorf("read CI handoff source repository: %w", err)
	}
	if repo == nil {
		return externalCIHandoff{}, fmt.Errorf("CI handoff source repository is missing")
	}
	sourceRepo := externalSourceRepository(repo.PushURL())
	sourceBranch := strings.TrimPrefix(run.Branch, "refs/heads/")
	if sourceRepo == "" || sourceBranch == "" {
		return externalCIHandoff{}, fmt.Errorf("CI handoff source identity is unreadable")
	}
	return externalCIHandoff{Outcome: "pending-external-ci", RunID: run.ID, ExternalCIOwner: run.ExternalCIOwner,
		SourceRepo: sourceRepo, SourceBranch: sourceBranch, PendingCISupport: claims}, nil
}

func externalSourceRepository(pushURL string) string {
	sourceRepo := scm.RepoPath(pushURL)
	if canonical, err := azuredevops.CanonicalSourceRepository(pushURL); err == nil {
		return canonical
	}
	return sourceRepo
}
