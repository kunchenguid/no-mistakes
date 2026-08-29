package cli

import (
	"strings"
	"testing"
)

// unconditionalGateMoveClaims are the phrasings that assert the gate
// compare-and-swap as an unconditional consequence of --keep-local.
var unconditionalGateMoveClaims = []string{
	"anchored and the gate branch compare-and-swaps onto the kept head",
	"and points the gate branch at the kept head",
	"the preserved commits stay anchored and the gate follows the kept head",
}

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
// so they describe what an operator or agent actually receives. Each of the
// four surfaces is asserted on its own extracted text: greping the whole help
// blob let one surface satisfy the check on another's behalf, which is how the
// agent-facing flag line stayed unpinned through the first correction.
func TestKeepLocalHelpSurfacesStayConditional(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"human sync", []string{"sync", "--help"}},
		{"axi sync", []string{"axi", "sync", "--help"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := executeCmd(tc.args...)
			if err != nil {
				t.Fatalf("%s: %v\n%s", strings.Join(tc.args, " "), err, out)
			}

			for _, surface := range []struct{ name, text string }{
				{"flag help", keepLocalFlagUsage(t, out)},
				{"long description", helpLongDescription(t, out)},
			} {
				// Every surface must condition the gate move on the gate
				// branch actually naming something else.
				if !strings.Contains(surface.text, "still names a different head") {
					t.Errorf("%s %s states the gate move unconditionally:\n%s", tc.name, surface.name, surface.text)
				}
				for _, overstatement := range unconditionalGateMoveClaims {
					if strings.Contains(surface.text, overstatement) {
						t.Errorf("%s %s carries the unconditional claim %q:\n%s", tc.name, surface.name, overstatement, surface.text)
					}
				}
			}
		})
	}
}

// keepLocalFlagUsage returns the single `--keep-local` usage line from real
// cobra help output, whitespace-normalized. pflag renders one flag per line and
// does not wrap when no width is configured, so the flag's whole description is
// on the one line whose first token is the flag name. That excludes the Long
// description, which mentions `--keep-local` mid-sentence.
func keepLocalFlagUsage(t *testing.T, help string) string {
	t.Helper()
	var found []string
	for _, line := range strings.Split(help, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--keep-local") {
			found = append(found, normalizeHelpText(line))
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one --keep-local flag usage line, got %d:\n%s", len(found), help)
	}
	return found[0]
}

// helpLongDescription returns the command's Long text - everything cobra prints
// before the usage block - whitespace-normalized so hard-wrapped phrases match.
func helpLongDescription(t *testing.T, help string) string {
	t.Helper()
	long, _, ok := strings.Cut(help, "\nUsage:")
	if !ok {
		t.Fatalf("help output has no usage block to delimit the long description:\n%s", help)
	}
	long = normalizeHelpText(long)
	if !strings.Contains(long, "--keep-local") {
		t.Fatalf("long description does not document --keep-local:\n%s", long)
	}
	return long
}

func normalizeHelpText(s string) string { return strings.Join(strings.Fields(s), " ") }
