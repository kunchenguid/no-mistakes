package agent

import (
	"reflect"
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
// agent_args_override) is preserved AND the neutralization flags are still
// appended after it, so model selection and project-instruction suppression
// both take effect.
func TestBuildOpencodeServeArgs_OptOutPreservesPinnedModel(t *testing.T) {
	got := buildOpencodeServeArgs([]string{"--model", "ollama-cloud/glm-5.2"}, 9999, true)
	want := []string{
		"serve",
		"--model", "ollama-cloud/glm-5.2",
		"--hostname", "127.0.0.1",
		"--port", "9999",
		"--print-logs",
		"--no-project-instructions",
		"--pure",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("buildOpencodeServeArgs(opt-out+model) = %v, want %v", got, want)
	}
	// Sanity: the managed model arg survives and the neutralization flags
	// come after it (last-wins ordering).
	if i := indexOf(got, "--model"); i < 0 || i+1 >= len(got) || got[i+1] != "ollama-cloud/glm-5.2" {
		t.Errorf("pinned model must be preserved, got %v", got)
	}
	if j := indexOf(got, "--no-project-instructions"); j < 0 || j <= indexOf(got, "--model") {
		t.Errorf("--no-project-instructions must appear after the model arg, got %v", got)
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
