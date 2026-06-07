package update

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
)

func (u *updater) confirmActiveRunsBeforeUpdate() error {
	runs, err := u.activeRuns()
	if err != nil {
		return fmt.Errorf("check active pipeline runs: %w", err)
	}
	if len(runs) == 0 {
		return nil
	}

	u.writeActiveRunWarning(runs)
	if u.assumeYes {
		fmt.Fprintln(u.stderrWriter(), "continuing because -y was provided")
		return nil
	}

	fmt.Fprint(u.stderrWriter(), "Continue with update and restart the daemon? [y/N] ")
	if readYes(u.stdin) {
		return nil
	}
	return fmt.Errorf("update cancelled because %d active pipeline runs are in progress", len(runs))
}

func (u *updater) activeRuns() ([]*db.Run, error) {
	if u == nil || u.paths == nil {
		return nil, nil
	}
	dbPath := u.paths.DB()
	if _, err := os.Stat(dbPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat database: %w", err)
	}

	database, err := db.Open(dbPath)
	if err != nil {
		return nil, err
	}
	defer database.Close()
	return database.GetActiveRuns()
}

func (u *updater) writeActiveRunWarning(runs []*db.Run) {
	runWord := "runs"
	if len(runs) == 1 {
		runWord = "run"
	}
	fmt.Fprintf(u.stderrWriter(), "warning: update will restart the daemon while %d active pipeline %s are in progress\n", len(runs), runWord)
	fmt.Fprintln(u.stderrWriter(), "active pipeline runs:")
	for _, run := range runs {
		fmt.Fprintf(u.stderrWriter(), "  %s  %s  %s  %s\n", run.ID, run.Status, run.Branch, shortRunSHA(run.HeadSHA))
	}
	fmt.Fprintln(u.stderrWriter(), "continuing can cause these pipelines to fail")
}

func shortRunSHA(sha string) string {
	if len(sha) <= 8 {
		return sha
	}
	return sha[:8]
}

func readYes(input io.Reader) bool {
	if input == nil {
		input = os.Stdin
	}
	response, err := bufio.NewReader(input).ReadString('\n')
	if err != nil && response == "" {
		return false
	}
	answer := strings.ToLower(strings.TrimSpace(response))
	return answer == "y" || answer == "yes"
}
