//go:build ignore

// Command: go run ./internal/e2edaemon/reapmain.go
//
// Suite-wrapper reaper entrypoint. Invoked from scripts/e2e.sh on EXIT/INT/TERM.
// Does not claim to survive SIGKILL of the wrapper shell itself; next-run
// recovery (TestMain + pre-reap) covers that case via the on-disk inventory.
package main

import (
	"fmt"
	"os"

	"github.com/kunchenguid/no-mistakes/internal/e2edaemon"
)

func main() {
	inv, err := e2edaemon.Open()
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e-reap: open inventory: %v\n", err)
		os.Exit(0) // best-effort; never fail the suite wrapper hard
	}
	result := inv.ReapAll()
	if len(result.Errors) > 0 {
		for _, e := range result.Errors {
			fmt.Fprintf(os.Stderr, "e2e-reap: %s\n", e)
		}
	}
	if os.Getenv("NM_E2E_REAP_VERBOSE") == "1" {
		fmt.Fprintf(os.Stderr, "e2e-reap: entries=%d stopped=%d killed=%d removed=%d skipped=%d\n",
			result.Entries, result.Stopped, result.Killed, result.Removed, result.Skipped)
	}
}
