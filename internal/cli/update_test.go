package cli

import (
	"strings"
	"testing"
)

func TestUpdateCommandDevBuild(t *testing.T) {
	isolateUpdateCommand(t)

	out, err := executeCmd("update")
	if err != nil {
		t.Fatalf("update failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "self-update unavailable for development builds") {
		t.Fatalf("unexpected update output: %s", out)
	}
}

func TestUpdateCommandBetaFlag(t *testing.T) {
	isolateUpdateCommand(t)

	out, err := executeCmd("update", "--beta")
	if err != nil {
		t.Fatalf("update --beta failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "self-update unavailable for development builds") {
		t.Fatalf("unexpected update output: %s", out)
	}
}

func TestUpdateCommandYesFlag(t *testing.T) {
	isolateUpdateCommand(t)

	out, err := executeCmd("update", "-y")
	if err != nil {
		t.Fatalf("update -y failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "self-update unavailable for development builds") {
		t.Fatalf("unexpected update output: %s", out)
	}
}

func isolateUpdateCommand(t *testing.T) {
	t.Helper()
	t.Setenv("NM_HOME", t.TempDir())
}
