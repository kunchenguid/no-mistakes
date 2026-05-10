package db

import (
	"fmt"
	"path/filepath"
	"slices"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Stats summarizes historical no-mistakes usage across all repositories.
type Stats struct {
	TotalRepos       int
	TotalRuns        int
	PullRequests     int
	RescueRuns       int
	ReportedFindings int
	FixedFindings    int
	StepStats        []StepStats
	RepoStats        []RepoStats
}

// StepStats summarizes reported and fixed findings for one pipeline step.
type StepStats struct {
	StepName         types.StepName
	ReportedFindings int
	FixedFindings    int
}

// RepoStats summarizes historical usage for one repository.
type RepoStats struct {
	RepoID           string
	WorkingPath      string
	Runs             int
	RescueRuns       int
	ReportedFindings int
	FixedFindings    int
}

// DisplayName returns a compact repository name for terminal reports.
func (r RepoStats) DisplayName() string {
	name := filepath.Base(r.WorkingPath)
	if name == "." || name == string(filepath.Separator) || name == "" {
		return r.WorkingPath
	}
	return name
}

// GetStats aggregates historical usage across all repositories.
func (d *DB) GetStats() (*Stats, error) {
	repos, err := d.getRepos()
	if err != nil {
		return nil, err
	}

	stats := &Stats{TotalRepos: len(repos)}
	stepStats := map[types.StepName]*StepStats{}

	for _, repo := range repos {
		repoStats := RepoStats{RepoID: repo.ID, WorkingPath: repo.WorkingPath}
		runs, err := d.GetRunsByRepo(repo.ID)
		if err != nil {
			return nil, err
		}
		repoStats.Runs = len(runs)
		stats.TotalRuns += len(runs)

		for _, run := range runs {
			if run.PRURL != nil && *run.PRURL != "" {
				stats.PullRequests++
			}

			runReported, runFixed, err := d.aggregateRunStats(run.ID, stepStats)
			if err != nil {
				return nil, err
			}
			stats.ReportedFindings += runReported
			stats.FixedFindings += runFixed
			repoStats.ReportedFindings += runReported
			repoStats.FixedFindings += runFixed
			if runReported > 0 && runFixed > 0 {
				stats.RescueRuns++
				repoStats.RescueRuns++
			}
		}

		stats.RepoStats = append(stats.RepoStats, repoStats)
	}

	for _, step := range stepStats {
		if step.ReportedFindings == 0 && step.FixedFindings == 0 {
			continue
		}
		stats.StepStats = append(stats.StepStats, *step)
	}
	sortStepStats(stats.StepStats)
	sortRepoStats(stats.RepoStats)

	return stats, nil
}

func (d *DB) aggregateRunStats(runID string, stepStats map[types.StepName]*StepStats) (int, int, error) {
	steps, err := d.GetStepsByRun(runID)
	if err != nil {
		return 0, 0, err
	}

	runReported := 0
	runFixed := 0
	for _, step := range steps {
		rounds, err := d.GetRoundsByStep(step.ID)
		if err != nil {
			return 0, 0, err
		}
		reported, final := stepFindingCounts(step, rounds)
		fixed := reported - final
		if fixed < 0 {
			fixed = 0
		}

		runReported += reported
		runFixed += fixed
		stat := stepStats[step.StepName]
		if stat == nil {
			stat = &StepStats{StepName: step.StepName}
			stepStats[step.StepName] = stat
		}
		stat.ReportedFindings += reported
		stat.FixedFindings += fixed
	}

	return runReported, runFixed, nil
}

func stepFindingCounts(step *StepResult, rounds []*StepRound) (reported int, final int) {
	if len(rounds) == 0 {
		count := findingsCount(step.FindingsJSON)
		return count, count
	}
	return findingsCount(rounds[0].FindingsJSON), findingsCount(rounds[len(rounds)-1].FindingsJSON)
}

func findingsCount(raw *string) int {
	if raw == nil || *raw == "" {
		return 0
	}
	findings, err := types.ParseFindingsJSON(*raw)
	if err != nil {
		return 0
	}
	return len(findings.Items)
}

func sortStepStats(stats []StepStats) {
	slices.SortFunc(stats, func(a, b StepStats) int {
		if a.FixedFindings != b.FixedFindings {
			return b.FixedFindings - a.FixedFindings
		}
		if a.ReportedFindings != b.ReportedFindings {
			return b.ReportedFindings - a.ReportedFindings
		}
		return a.StepName.Order() - b.StepName.Order()
	})
}

func sortRepoStats(stats []RepoStats) {
	slices.SortFunc(stats, func(a, b RepoStats) int {
		if a.RescueRuns != b.RescueRuns {
			return b.RescueRuns - a.RescueRuns
		}
		if a.FixedFindings != b.FixedFindings {
			return b.FixedFindings - a.FixedFindings
		}
		if a.Runs != b.Runs {
			return b.Runs - a.Runs
		}
		if a.WorkingPath < b.WorkingPath {
			return -1
		}
		if a.WorkingPath > b.WorkingPath {
			return 1
		}
		return 0
	})
}

func (d *DB) getRepos() ([]*Repo, error) {
	rows, err := d.sql.Query(`SELECT id, working_path, upstream_url, default_branch, created_at FROM repos ORDER BY working_path`)
	if err != nil {
		return nil, fmt.Errorf("get repos: %w", err)
	}
	defer rows.Close()

	var repos []*Repo
	for rows.Next() {
		repo := &Repo{}
		if err := rows.Scan(&repo.ID, &repo.WorkingPath, &repo.UpstreamURL, &repo.DefaultBranch, &repo.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan repo: %w", err)
		}
		repos = append(repos, repo)
	}
	return repos, rows.Err()
}
