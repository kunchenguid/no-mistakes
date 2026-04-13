package cli

import (
	"regexp"
	"testing"
)

var ansiEscapeRE = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func TestStyledCommandOutputDoesNotEmitANSIForNonTTY(t *testing.T) {
	setupTestRepo(t)

	out, err := executeCmd("doctor")
	if err != nil {
		t.Fatalf("doctor failed: %v\noutput: %s", err, out)
	}
	if ansiEscapeRE.MatchString(out) {
		t.Fatalf("doctor output should not include ANSI escape sequences, got: %q", out)
	}

	out, err = executeCmd("daemon", "status")
	if err != nil {
		t.Fatalf("daemon status failed: %v\noutput: %s", err, out)
	}
	if ansiEscapeRE.MatchString(out) {
		t.Fatalf("daemon status output should not include ANSI escape sequences, got: %q", out)
	}
}
