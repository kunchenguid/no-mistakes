package cli

import (
	"strings"
	"testing"
)

func TestUpdateCommandDevBuild(t *testing.T) {
	out, err := executeCmd("update")
	if err != nil {
		t.Fatalf("update failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "self-update unavailable for development builds") {
		t.Fatalf("unexpected update output: %s", out)
	}
}
