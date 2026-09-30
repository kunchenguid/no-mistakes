package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/spf13/cobra"
)

// reviewReceipt exposes the durable comparison for a completed run. Callers
// must still reread the live PR and target before acting on this observation.
type reviewReceipt struct {
	RunID        string `json:"run_id"`
	SourceRepo   string `json:"source_repo"`
	SourceBranch string `json:"source_branch"`
	PRURL        string `json:"pr_url"`
	ForgeHeadSHA string `json:"forge_head_sha"`
	LocalHeadSHA string `json:"local_head_sha"`
	TargetBranch string `json:"target_branch"`
	TargetSHA    string `json:"target_sha"`
	MergeBaseSHA string `json:"merge_base_sha"`
	DiffDigest   string `json:"diff_digest"`
	Generation   int64  `json:"generation"`
}

func newAxiReviewReceiptCmd() *cobra.Command {
	var runID string
	cmd := &cobra.Command{
		Use: "review-receipt", Short: "Read a completed run's exact PR comparison as JSON",
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
			receipt, err := buildReviewReceipt(env.d, run)
			if err != nil {
				return emitCIHandoffError(cmd, err.Error())
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(receipt)
		},
	}
	cmd.Flags().StringVar(&runID, "run", "", "exact completed run ID to inspect")
	return cmd
}

func buildReviewReceipt(database *db.DB, run *db.Run) (reviewReceipt, error) {
	if run == nil || run.Status != types.RunCompleted || run.TerminalHeadVerifiedAt == nil {
		return reviewReceipt{}, fmt.Errorf("run has no verified terminal result")
	}
	context, err := database.GetRunPRContext(run.ID)
	if err != nil {
		return reviewReceipt{}, err
	}
	if context == nil || context.LocalHeadSHA != run.HeadSHA || context.TargetSHA == "" || context.DiffDigest == "" {
		return reviewReceipt{}, fmt.Errorf("run has no current exact PR comparison receipt")
	}
	repo, err := database.GetRepo(run.RepoID)
	if err != nil {
		return reviewReceipt{}, fmt.Errorf("read run source repository: %w", err)
	}
	if repo == nil {
		return reviewReceipt{}, fmt.Errorf("run source repository is missing")
	}
	pushURL := repo.PushURL()
	sourceRepo := externalSourceRepository(pushURL)
	sourceBranch := strings.TrimPrefix(run.Branch, "refs/heads/")
	if sourceRepo == "" || sourceBranch == "" {
		return reviewReceipt{}, fmt.Errorf("run source identity is unreadable")
	}
	if context.PRURL != "" {
		sameRepo := context.SourceRepo == sourceRepo
		if scm.ExtractHost(pushURL) == "github.com" {
			sameRepo = strings.EqualFold(context.SourceRepo, sourceRepo)
		}
		if !sameRepo || context.SourceBranch != sourceBranch {
			return reviewReceipt{}, fmt.Errorf("run PR identity conflicts with source repository")
		}
		sourceRepo = context.SourceRepo
	}
	return reviewReceipt{RunID: run.ID, SourceRepo: sourceRepo, SourceBranch: sourceBranch,
		PRURL: context.PRURL, ForgeHeadSHA: context.ForgeHeadSHA, LocalHeadSHA: context.LocalHeadSHA,
		TargetBranch: context.TargetBranch, TargetSHA: context.TargetSHA,
		MergeBaseSHA: context.MergeBaseSHA, DiffDigest: context.DiffDigest,
		Generation: context.Generation}, nil
}
