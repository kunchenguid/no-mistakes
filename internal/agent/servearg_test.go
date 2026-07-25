package agent

import (
	"reflect"
	"strings"
	"testing"
)

func TestBuildRovodevServeArgs_Default(t *testing.T) {
	got := buildRovodevServeArgs(nil, 51234)
	want := []string{"rovodev", "serve", "--disable-session-token", "51234"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("buildRovodevServeArgs(nil) = %v, want %v", got, want)
	}
}

func TestBuildRovodevServeArgs_ExtraArgsInserted(t *testing.T) {
	got := buildRovodevServeArgs([]string{"--profile", "work"}, 51234)
	want := []string{
		"rovodev", "serve",
		"--profile", "work",
		"--disable-session-token", "51234",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("buildRovodevServeArgs = %v, want %v", got, want)
	}
}

func TestBuildOpencodeServeArgs_Default(t *testing.T) {
	got := buildOpencodeServeArgs(nil, 9999, false)
	want := []string{
		"serve",
		"--hostname", "127.0.0.1",
		"--port", "9999",
		"--print-logs",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("buildOpencodeServeArgs(nil) = %v, want %v", got, want)
	}
}

func TestBuildOpencodeServeArgs_ExtraArgsInserted(t *testing.T) {
	got := buildOpencodeServeArgs([]string{"--model", "gpt-5"}, 9999, false)
	want := []string{
		"serve",
		"--model", "gpt-5",
		"--hostname", "127.0.0.1",
		"--port", "9999",
		"--print-logs",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("buildOpencodeServeArgs = %v, want %v", got, want)
	}
}

// TestBuildOpencodeServeArgs_OptOutAddsNeutralizationFlags proves the exact
// `opencode serve` argv carries --no-project-instructions and --pure LAST when
// the trusted opt-out is on, so yargs last-wins enforces them over any operator
// override.
func TestBuildOpencodeServeArgs_OptOutAddsNeutralizationFlags(t *testing.T) {
	got := buildOpencodeServeArgs(nil, 9999, true)
	want := []string{
		"serve",
		"--hostname", "127.0.0.1",
		"--port", "9999",
		"--print-logs",
		"--no-project-instructions",
		"--pure",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("buildOpencodeServeArgs(opt-out) = %v, want %v", got, want)
	}
}

// TestBuildOpencodeServeArgs_OptOutPreservesPinnedModel proves an explicit
// operator model argument (e.g. --model ollama-cloud/glm-5.2 from
// agent_args_override) is extracted from the serve argv (because opencode serve
// does not accept --model) and the neutralization flags are still appended.
// The model is routed to the session creation API by opencodeExtractModel +
// createSession, not the serve argv.
func TestBuildOpencodeServeArgs_OptOutPreservesPinnedModel(t *testing.T) {
	// --model is extracted before buildOpencodeServeArgs sees extraArgs.
	serveArgs, model, err := opencodeExtractModel([]string{"--model", "ollama-cloud/glm-5.2"})
	if err != nil {
		t.Fatalf("opencodeExtractModel: %v", err)
	}
	if model != "ollama-cloud/glm-5.2" {
		t.Errorf("opencodeExtractModel must return the model value, got %q", model)
	}
	for _, a := range serveArgs {
		if a == "--model" || strings.HasPrefix(a, "--model=") {
			t.Errorf("--model must be stripped from serve args, got %v", serveArgs)
		}
	}
	got := buildOpencodeServeArgs(serveArgs, 9999, true)
	want := []string{
		"serve",
		"--hostname", "127.0.0.1",
		"--port", "9999",
		"--print-logs",
		"--no-project-instructions",
		"--pure",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("buildOpencodeServeArgs(opt-out, model extracted) = %v, want %v", got, want)
	}
}

// TestBuildOpencodeServeArgs_OptOutManagedFlagsWinOverOperator proves the
// managed neutralization flags are appended after operator extraArgs so yargs
// last-wins enforces them even if the operator tried to defeat the opt-out.
// There is no operator flag that re-enables project instructions once
// --no-project-instructions is in effect, so this is defense-in-depth.
func TestBuildOpencodeServeArgs_OptOutManagedFlagsWinOverOperator(t *testing.T) {
	// Operator passes --pure=false (attempt to re-enable plugins); managed
	// --pure is appended after it and wins.
	got := buildOpencodeServeArgs([]string{"--pure=false"}, 9999, true)
	want := []string{
		"serve",
		"--pure=false",
		"--hostname", "127.0.0.1",
		"--port", "9999",
		"--print-logs",
		"--no-project-instructions",
		"--pure",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("buildOpencodeServeArgs(opt-out+defeat) = %v, want %v", got, want)
	}
	// Managed --pure is the last --pure occurrence -> wins.
	lastPure := -1
	for i, a := range got {
		if a == "--pure" {
			lastPure = i
		}
	}
	if lastPure < 0 || lastPure <= indexOf(got, "--pure=false") {
		t.Errorf("managed --pure must be the last --pure token, got %v", got)
	}
}

// TestBuildOpencodeServeArgs_NoOptOutUnchanged proves that without the opt-out
// the argv carries neither neutralization flag, so normal non-review OpenCode
// behavior is unchanged (backward-compat).
func TestBuildOpencodeServeArgs_NoOptOutUnchanged(t *testing.T) {
	got := buildOpencodeServeArgs([]string{"--model", "gpt-5"}, 9999, false)
	for _, a := range got {
		if a == "--no-project-instructions" || a == "--pure" {
			t.Errorf("neutralization flag %q must NOT appear without opt-out, got %v", a, got)
		}
	}
}

func indexOf(args []string, flag string) int {
	for i, a := range args {
		if a == flag {
			return i
		}
	}
	return -1
}
