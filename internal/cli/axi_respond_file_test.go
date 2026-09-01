package cli

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestResolveRespondText_VerbatimFilePreservation(t *testing.T) {
	content := "run `git checkout `main`` and avoid $(rm -rf .)\n  quoted \"paths\" here"
	p := filepath.Join(t.TempDir(), "instructions.txt")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := &cobra.Command{}
	got, err := resolveRespondText(cmd, "instructions", "", p)
	if err != nil {
		t.Fatalf("resolveRespondText returned error: %v", err)
	}
	if got != content {
		t.Fatalf("verbatim preservation failed\n got: %q\nwant: %q", got, content)
	}
}

func TestResolveRespondText_Stdin(t *testing.T) {
	content := "fix the backticked `command` and $(literal) exactly"
	cmd := &cobra.Command{}
	cmd.SetIn(strings.NewReader(content))
	got, err := resolveRespondText(cmd, "add-finding", "", "-")
	if err != nil {
		t.Fatalf("resolveRespondText returned error: %v", err)
	}
	if got != content {
		t.Fatalf("stdin preservation failed\n got: %q\nwant: %q", got, content)
	}
}

func TestResolveRespondText_InlinePassedThrough(t *testing.T) {
	cmd := &cobra.Command{}
	got, err := resolveRespondText(cmd, "instructions", "inline note", "")
	if err != nil {
		t.Fatalf("resolveRespondText returned error: %v", err)
	}
	if got != "inline note" {
		t.Fatalf("inline pass-through failed: got %q", got)
	}
}

func TestResolveRespondText_MissingFile(t *testing.T) {
	cmd := &cobra.Command{}
	if _, err := resolveRespondText(cmd, "instructions", "", filepath.Join(t.TempDir(), "nope.txt")); err == nil {
		t.Fatal("expected an error for a missing file")
	}
}

func TestAxiRespond_InlineAndFileAreMutuallyExclusive(t *testing.T) {
	p := filepath.Join(t.TempDir(), "respond.txt")
	if err := os.WriteFile(p, []byte("from file"), 0o644); err != nil {
		t.Fatal(err)
	}

	flagPairs := []struct {
		name       string
		inlineFlag string
		fileFlag   string
	}{
		{name: "instructions", inlineFlag: "instructions", fileFlag: "instructions-file"},
		{name: "add finding", inlineFlag: "add-finding", fileFlag: "add-finding-file"},
	}
	values := []struct {
		name        string
		inlineValue string
		fileValue   string
	}{
		{name: "both nonempty", inlineValue: "inline", fileValue: p},
		{name: "empty inline", inlineValue: "", fileValue: p},
		{name: "empty file", inlineValue: "inline", fileValue: ""},
	}
	for _, pair := range flagPairs {
		for _, value := range values {
			t.Run(pair.name+"/"+value.name, func(t *testing.T) {
				out, err := executeCmd(
					"axi", "respond", "--action", "fix",
					"--"+pair.inlineFlag+"="+value.inlineValue,
					"--"+pair.fileFlag+"="+value.fileValue,
				)
				assertExitCode(t, err, 2)
				if !strings.Contains(out, "mutually exclusive") {
					t.Fatalf("expected a clear mutual-exclusion error, got %q", out)
				}
			})
		}
	}
}

func TestRunAxiRespond_StdinOnlyReadOnce(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.SetOut(io.Discard)
	cmd.SetIn(strings.NewReader("shared"))
	err := runAxiRespond(cmd, respondArgs{
		action:           "fix",
		instructionsFile: "-",
		addFindingFile:   "-",
	})
	assertExitCode(t, err, 2)
}

func assertExitCode(t *testing.T, err error, want int) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error, got nil")
	}
	var ee *exitError
	if !errors.As(err, &ee) {
		t.Fatalf("expected *exitError, got %T: %v", err, err)
	}
	if ee.code != want {
		t.Fatalf("expected exit code %d, got %d (err=%v)", want, ee.code, err)
	}
}
