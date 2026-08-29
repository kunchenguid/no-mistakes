package cli

import (
	"strings"
	"testing"
)

// TestKeepLocalHelpSurfacesStayConditional pins the four operator-facing
// descriptions of `--keep-local` to what the code actually does.
//
// This exists because the drift it catches already happened twice. The flag
// help claimed unconditionally that the gate branch compare-and-swaps onto the
// kept head, which is false for every path that returns before recoverKeepLocal
// is reached: the equal/ahead branch stamps custody without touching the gate,
// the settlement returns early when the gate branch is proven absent or no gate
// is configured, and recoverKeepLocal skips its whole block when the gate
// already names the kept head. The first correction fixed three of the four
// surfaces and missed the `axi sync` flag - the agent-facing one - because a
// help string is an executable contract that had no executable check.
//
// The assertions drive the real cobra help output rather than reading source,
// so they describe what an operator or agent actually receives.
func TestKeepLocalHelpSurfacesStayConditional(t *testing.T) {
	for _, tc := range []struct{ name, command string }{
		{"human sync", "sync"},
		{"axi sync", "axi"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out string
			var err error
			if tc.command == "sync" {
				out, err = executeCmd("sync", "--help")
			} else {
				out, err = executeCmd("axi", "sync", "--help")
			}
			if err != nil {
				t.Fatalf("%s --help: %v\n%s", tc.command, err, out)
			}
			if !strings.Contains(out, "--keep-local") {
				t.Fatalf("%s help does not document --keep-local:\n%s", tc.command, out)
			}
			// Every surface must condition the gate move on the gate branch
			// actually naming something else.
			if !strings.Contains(out, "still names a different head") {
				t.Errorf("%s help states the gate move unconditionally:\n%s", tc.command, out)
			}
			// And must not assert the swap as an unconditional consequence.
			for _, overstatement := range []string{
				"anchored and the gate branch compare-and-swaps onto the kept head",
				"and points the gate branch at the kept head",
			} {
				if strings.Contains(out, overstatement) {
					t.Errorf("%s help carries the unconditional claim %q:\n%s", tc.command, overstatement, out)
				}
			}
		})
	}
}
