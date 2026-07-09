package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/scm/github"
	"github.com/spf13/cobra"
)

func newEvidenceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "evidence",
		Short: "Manage hosted evidence artifacts",
	}
	cmd.AddCommand(newEvidencePruneCmd())
	return cmd
}

func newEvidencePruneCmd() *cobra.Command {
	var runID string
	var prNumber string

	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Delete secret gists that host PR evidence for a run",
		Long: "Delete secret GitHub gists recorded for a no-mistakes run.\n\n" +
			"The CI monitor normally deletes these gists automatically when it sees the PR merge or close. " +
			"Use this command as a manual fallback for older runs, failed automatic cleanup, or monitors that are no longer running.\n\n" +
			"Warning: deleting these gists makes existing PR screenshots and video evidence links 404.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return trackCommand("evidence-prune", func() error {
				return runEvidencePrune(cmd, runID, prNumber)
			})
		},
	}
	cmd.Flags().StringVar(&runID, "run", "", "run ID whose recorded evidence gists should be deleted")
	cmd.Flags().StringVar(&prNumber, "pr", "", "PR number in the current repository whose recorded evidence gists should be deleted")
	return cmd
}

func runEvidencePrune(cmd *cobra.Command, runID, prNumber string) error {
	runID = strings.TrimSpace(runID)
	prNumber = strings.TrimSpace(strings.TrimPrefix(prNumber, "#"))
	if (runID == "") == (prNumber == "") {
		return fmt.Errorf("pass exactly one of --run or --pr")
	}

	_, d, err := openResources()
	if err != nil {
		return err
	}
	defer d.Close()

	run, repo, err := resolveEvidencePruneTarget(d, runID, prNumber)
	if err != nil {
		return err
	}
	if len(run.EvidenceGistIDs) == 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "No recorded evidence gists for run %s.\n", run.ID)
		return nil
	}
	if scm.DetectProvider(repo.UpstreamURL) != scm.ProviderGitHub {
		return fmt.Errorf("evidence gist pruning is supported only for GitHub repositories")
	}

	host := github.New(
		func(ctx context.Context, name string, args ...string) *exec.Cmd {
			c := exec.CommandContext(ctx, name, args...)
			c.Dir = repo.WorkingPath
			c.Env = os.Environ()
			return c
		},
		func() bool {
			_, err := exec.LookPath("gh")
			return err == nil
		},
		scm.ExtractHost(repo.UpstreamURL),
		github.HostPrefixedSlug(repo.UpstreamURL),
	)
	if err := host.Available(cmd.Context()); err != nil {
		return err
	}

	fmt.Fprintln(cmd.OutOrStdout(), "Warning: deleting evidence gists makes existing PR screenshots and video evidence links 404.")
	for _, id := range run.EvidenceGistIDs {
		if err := host.DeleteGist(cmd.Context(), id); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Deleted evidence gist %s.\n", id)
	}
	if err := d.ClearRunEvidenceGistIDs(run.ID); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Pruned %d evidence gist(s) for run %s.\n", len(run.EvidenceGistIDs), run.ID)
	return nil
}

func resolveEvidencePruneTarget(d *db.DB, runID, prNumber string) (*db.Run, *db.Repo, error) {
	if runID != "" {
		run, err := d.GetRun(runID)
		if err != nil {
			return nil, nil, err
		}
		if run == nil {
			return nil, nil, fmt.Errorf("run %s not found", runID)
		}
		repo, err := d.GetRepo(run.RepoID)
		if err != nil {
			return nil, nil, err
		}
		if repo == nil {
			return nil, nil, fmt.Errorf("repo %s not found for run %s", run.RepoID, run.ID)
		}
		return run, repo, nil
	}

	repo, err := findRepo(d)
	if err != nil {
		return nil, nil, err
	}
	runs, err := d.GetRunsByRepo(repo.ID)
	if err != nil {
		return nil, nil, err
	}
	for _, run := range runs {
		if run.PRURL == nil {
			continue
		}
		number, err := scm.ExtractPRNumber(*run.PRURL)
		if err == nil && number == prNumber {
			return run, repo, nil
		}
	}
	return nil, nil, fmt.Errorf("no run with PR #%s found in current repository", prNumber)
}
