package update

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/lifecycle"
)

func (u *updater) confirmActiveRunsBeforeUpdate() error {
	runs, err := lifecycle.ActiveRuns(u.paths)
	if err != nil {
		return fmt.Errorf("check active pipeline runs: %w", err)
	}
	if len(runs) == 0 {
		return nil
	}

	u.writeActiveRunWarning(runs)
	if u.force {
		fmt.Fprintln(u.stderrWriter(), "FORCE: continuing update and daemon restart despite active pipeline runs")
		return nil
	}

	return fmt.Errorf("refusing update because %d active pipeline runs are in progress; pass --force to stop/restart the daemon anyway", len(runs))
}

func (u *updater) writeActiveRunWarning(runs []*db.Run) {
	runWord := "runs"
	if len(runs) == 1 {
		runWord = "run"
	}
	fmt.Fprintf(u.stderrWriter(), "warning: update will restart the daemon while %d active pipeline %s are in progress\n", len(runs), runWord)
	fmt.Fprint(u.stderrWriter(), lifecycle.RunList(runs))
	fmt.Fprintln(u.stderrWriter(), "continuing can cause these pipelines to fail")
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
