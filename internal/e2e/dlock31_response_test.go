//go:build e2e

package e2e

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// Only the CLI's exit-1, post-submission wait result is recoverable here.
// Before submission its help says to retry axi respond, not reattach axi run.
func dlock31SubmittedWaitElapsed(err error, output string) bool {
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		return false
	}
	framed := "\n" + output + "\n"
	return strings.Count(framed, "\nerror: ") == 1 &&
		strings.Contains(framed, "\nerror: wait of 1s elapsed while driving the run\n") &&
		strings.Contains(output, "This bounded hold ended; it is not a pipeline failure and does not mean the daemon is dead.") &&
		strings.Contains(output, "Re-run `no-mistakes axi run` to reattach for another 1s")
}

func TestDLOCK31SubmittedWaitElapsed(t *testing.T) {
	exitOne := exec.Command("sh", "-c", "exit 1").Run()
	exitTwo := exec.Command("sh", "-c", "exit 2").Run()
	const elapsed = "error: wait of 1s elapsed while driving the run\nhelp[3]: This bounded hold ended; it is not a pipeline failure and does not mean the daemon is dead.,Run `no-mistakes axi status` to inspect progress,Re-run `no-mistakes axi run` to reattach for another 1s\n"
	for _, tc := range []struct {
		name   string
		err    error
		output string
		want   bool
	}{
		{"submitted bounded hold", exitOne, elapsed, true},
		{"success", nil, elapsed, false},
		{"wrong exit", exitTwo, elapsed, false},
		{"harness timeout", context.DeadlineExceeded, elapsed, false},
		{"real response failure", exitOne, "error: respond to ci: unavailable\n", false},
		{"not yet submitted", exitOne, strings.ReplaceAll(elapsed, "no-mistakes axi run", "no-mistakes axi respond --action approve|fix|skip"), false},
		{"additional real failure", exitOne, elapsed + "error: database unavailable\n", false},
		{"missing help", exitOne, "error: wait of 1s elapsed while driving the run\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := dlock31SubmittedWaitElapsed(tc.err, tc.output); got != tc.want {
				t.Fatalf("accepted=%v, want %v", got, tc.want)
			}
		})
	}
}
