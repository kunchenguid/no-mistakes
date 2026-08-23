package cli

import (
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/committrailer"
	"github.com/kunchenguid/no-mistakes/internal/telemetry"
)

func TestAxiRunHelpDocumentsCommitTrailer(t *testing.T) {
	out, err := executeCmd("axi", "run", "--help")
	if err != nil {
		t.Fatalf("axi run --help: %v\n%s", err, out)
	}
	for _, want := range []string{"--commit-trailer", "Co-Authored-By"} {
		if !strings.Contains(out, want) {
			t.Fatalf("axi run help missing %q:\n%s", want, out)
		}
	}
}

func TestAxiRunRejectsMalformedCommitTrailerBeforeRepoOpen(t *testing.T) {
	t.Setenv("NM_HOME", t.TempDir())
	chdir(t, t.TempDir())

	out, err := executeCmd(
		"axi", "run",
		"--intent", "ship the thing",
		"--commit-trailer", "Co-Authored-By: --no-verify",
	)
	if err == nil {
		t.Fatalf("axi run accepted malformed commit trailer:\n%s", out)
	}
	if !strings.Contains(out, "invalid commit trailer") {
		t.Fatalf("error should identify the malformed trailer before repo setup errors, got:\n%s", out)
	}
}

func TestAxiRunPageviewCarriesCommitTrailerFlag(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("NM_HOME", t.TempDir())
	chdir(t, tmpDir)

	recorder := &telemetryRecorder{}
	restore := telemetry.SetDefaultForTesting(recorder)
	defer restore()

	_, _ = executeCmd(
		"axi", "run",
		"--intent", "ship it",
		"--commit-trailer", "Co-Authored-By: Phiora Agent <agent@phiora.test>",
	)

	event := recorder.find("pageview", "path", "/axi/run")
	if event == nil {
		t.Fatal("expected /axi/run pageview")
	}
	if got := event.fields["has_commit_trailer"]; got != true {
		t.Fatalf("has_commit_trailer = %v, want true", got)
	}
}

func TestCommitTrailerPushOptionRoundTrip(t *testing.T) {
	trailers, err := committrailer.ParseMany([]string{
		"Co-Authored-By: Phiora Agent <agent@phiora.test>",
		"Reviewed-by: Reviewer <reviewer@phiora.test>",
	})
	if err != nil {
		t.Fatal(err)
	}

	opts := formatCommitTrailerPushOptions(trailers)
	if len(opts) != 2 {
		t.Fatalf("formatCommitTrailerPushOptions() = %#v, want two options", opts)
	}
	got, err := parseCommitTrailerPushOptions(append([]string{"no-mistakes.skip=test"}, opts...))
	if err != nil {
		t.Fatalf("parseCommitTrailerPushOptions() error = %v", err)
	}
	if !reflect.DeepEqual(got, trailers) {
		t.Fatalf("round-trip = %#v, want %#v", got, trailers)
	}
}

func TestParseCommitTrailerPushOptionsRejectsMalformedTrailer(t *testing.T) {
	opt := commitTrailerPushOptionPrefix + "Q28tQXV0aG9yZWQtQnk6IC0tbm8tdmVyaWZ5"
	if _, err := parseCommitTrailerPushOptions([]string{opt}); err == nil {
		t.Fatal("parseCommitTrailerPushOptions accepted an option-like value")
	}
}
