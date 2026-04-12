package cli

import (
	"bytes"
	"regexp"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
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

	setColorProfileForOutput(new(bytes.Buffer))
	buf := new(bytes.Buffer)
	printRunLine(buf, &db.Run{Branch: "feature", HeadSHA: "1234567890", Status: types.RunCompleted})
	if ansiEscapeRE.MatchString(buf.String()) {
		t.Fatalf("run line output should not include ANSI escape sequences, got: %q", buf.String())
	}

}
