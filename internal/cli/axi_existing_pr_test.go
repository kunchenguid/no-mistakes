package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

const axiExistingPRURL = "https://github.com/upstream/widgets/pull/168"

func TestAxiExistingPRFlagRejectsUnsupportedCombinations(t *testing.T) {
	for _, extra := range [][]string{{"--base-branch", "release"}, {"--skip", "pr"}, {"--launch-nonce", "nonce", "--validation-generation", "gen"}} {
		args := append([]string{"axi", "run", "--existing-pr", axiExistingPRURL}, extra...)
		out, err := executeCmd(args...)
		if err == nil || !strings.Contains(out, "cannot be combined") {
			t.Fatalf("args=%v error=%v output=%s", args, err, out)
		}
	}
	for _, url := range []string{"", "168", "https://other.example/upstream/widgets/pull/168"} {
		if out, err := executeCmd("axi", "run", "--existing-pr", url); err == nil {
			t.Fatalf("accepted %q: %s", url, out)
		}
	}
}

// Retiring an association starts no run, so a flag that describes a run - above
// all --intent, which describes work the caller believes is about to be
// validated - is refused rather than silently dropped. Flags that only shape
// how a run is driven are not refused: a harness that always appends them must
// still be able to retire.
func TestAxiRetireExistingPRRejectsLaunchFlags(t *testing.T) {
	for _, extra := range [][]string{
		{"--intent", "revalidate after retiring"},
		{"--skip", "lint"},
		{"--base-branch", "release"},
		{"--launch-nonce", "nonce", "--validation-generation", "gen"},
		{"--existing-pr", axiExistingPRURL},
	} {
		args := append([]string{"axi", "run", "--retire-existing-pr"}, extra...)
		out, err := executeCmd(args...)
		if err == nil || !strings.Contains(out, "cannot be combined") {
			t.Fatalf("args=%v error=%v output=%s", args, err, out)
		}
		if !strings.Contains(out, extra[0]) {
			t.Fatalf("failure did not name the discarded flag %s: %s", extra[0], out)
		}
	}
	// --yes and --wait shape how a run is driven, not what it validates, so a
	// harness that appends them to every command can still retire.
	for _, extra := range [][]string{{"--yes"}, {"--wait", "10m"}, {"--yes", "--wait", "10m"}} {
		args := append([]string{"axi", "run", "--retire-existing-pr"}, extra...)
		out, _ := executeCmd(args...)
		if strings.Contains(out, "cannot be combined") {
			t.Fatalf("args=%v refused a drive-only flag: %s", args, out)
		}
	}
}

func TestAxiExistingPRReattachmentMustMatchPin(t *testing.T) {
	for _, target := range []string{"", axiExistingPRURL, "https://github.com/upstream/widgets/pull/169"} {
		t.Run(target, func(t *testing.T) {
			fx := newAxiTimeoutFixture(t, axiTimeoutOpts{})
			fx.setGetActive(func(context.Context) (*ipc.RunInfo, error) {
				r := fx.running()
				if target != "" {
					r.ExistingPRURL = &target
				}
				return r, nil
			})
			fx.setGetRun(func(context.Context, int) (*ipc.RunInfo, error) { return fx.completed(), nil })
			out, err := executeCmd("axi", "run", "--existing-pr", axiExistingPRURL, "--wait", "3s")
			if target == axiExistingPRURL {
				if err != nil {
					t.Fatalf("reattach failed: %v %s", err, out)
				}
			} else if err == nil || !strings.Contains(out, "refusing to reattach") {
				t.Fatalf("mismatched reattach err=%v output=%s", err, out)
			}
		})
	}
}

func TestAxiExistingPROldDaemonCannotFallBackToOrdinaryLaunch(t *testing.T) {
	fx := newAxiTimeoutFixture(t, axiTimeoutOpts{})
	fx.setGetActive(func(context.Context) (*ipc.RunInfo, error) { return nil, nil })
	out, err := executeCmd("axi", "run", "--existing-pr", axiExistingPRURL, "--intent", "validate upstream contribution")
	if err == nil || !strings.Contains(out, ipc.MethodStartExistingPRRun) {
		t.Fatalf("expected dedicated unsupported-method refusal, got %v\n%s", err, out)
	}
}
