package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// recordingAgent captures the RunOpts it was invoked with.
type recordingAgent struct {
	name      string
	gotOpts   RunOpts
	runCalls  int
	closed    bool
	resumable bool
}

func (r *recordingAgent) Name() string { return r.name }

func (r *recordingAgent) Run(_ context.Context, opts RunOpts) (*Result, error) {
	r.runCalls++
	r.gotOpts = opts
	return &Result{Text: "ok"}, nil
}

func (r *recordingAgent) Close() error {
	r.closed = true
	return nil
}

func (r *recordingAgent) SupportsSessionResume() bool { return r.resumable }

func TestWithSteering_PrependsPreamble(t *testing.T) {
	inner := &recordingAgent{name: "claude"}
	evidenceRoot := filepath.Join(t.TempDir(), "evidence")
	steered := WithSteering(inner, evidenceRoot)

	const userPrompt = "Fix the failing test in foo_test.go"
	if _, err := steered.Run(context.Background(), RunOpts{Prompt: userPrompt, CWD: "/tmp/wt"}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !strings.HasPrefix(inner.gotOpts.Prompt, WorktreeSteering(evidenceRoot)) {
		t.Errorf("prompt did not start with steering preamble:\n%q", inner.gotOpts.Prompt)
	}
	if !strings.HasSuffix(inner.gotOpts.Prompt, userPrompt) {
		t.Errorf("original prompt not preserved at end:\n%q", inner.gotOpts.Prompt)
	}
	// Other opts must pass through untouched.
	if inner.gotOpts.CWD != "/tmp/wt" {
		t.Errorf("CWD = %q, want /tmp/wt", inner.gotOpts.CWD)
	}
}

func TestWithSteering_PassesThroughNameAndClose(t *testing.T) {
	inner := &recordingAgent{name: "codex"}
	steered := WithSteering(inner, t.TempDir())

	if steered.Name() != "codex" {
		t.Errorf("Name() = %q, want codex", steered.Name())
	}
	if err := steered.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !inner.closed {
		t.Error("Close() did not propagate to inner agent")
	}
}

func TestWithSteering_DoesNotDoubleWrap(t *testing.T) {
	inner := &recordingAgent{name: "pi"}
	evidenceRoot := t.TempDir()
	once := WithSteering(inner, evidenceRoot)
	twice := WithSteering(once, evidenceRoot)

	const userPrompt = "do the thing"
	if _, err := twice.Run(context.Background(), RunOpts{Prompt: userPrompt}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := strings.Count(inner.gotOpts.Prompt, WorktreeSteering(evidenceRoot)); got != 1 {
		t.Errorf("steering preamble appeared %d times, want 1:\n%q", got, inner.gotOpts.Prompt)
	}
}

func TestWithSteering_ForwardsSessionCapability(t *testing.T) {
	steered := WithSteering(&recordingAgent{name: "codex", resumable: true}, t.TempDir())
	if !SupportsSessionResume(steered) {
		t.Fatal("steered resumable agent must remain resumable")
	}
}

// TestSteeringNamesTheConfiguredEvidenceRoot pins the preamble to the evidence
// root its caller supplied. The preamble used to rebuild that path itself from
// os.TempDir(), independently of the test step that actually creates the
// directory - two copies of one fact, either of which could move without the
// other. Asserting that two different roots produce two different preambles is
// what makes that drift impossible to reintroduce silently.
func TestSteeringNamesTheConfiguredEvidenceRoot(t *testing.T) {
	first := filepath.Join(t.TempDir(), "evidence")
	second := filepath.Join(t.TempDir(), "elsewhere")

	firstPreamble := WorktreeSteering(first)
	if !strings.Contains(firstPreamble, first) {
		t.Fatalf("steering preamble does not allow the configured evidence directory %q:\n%s", first, firstPreamble)
	}
	if strings.Contains(firstPreamble, os.TempDir()+string(filepath.Separator)+"no-mistakes-evidence") {
		t.Fatalf("steering preamble still names the legacy shared temp directory:\n%s", firstPreamble)
	}

	secondPreamble := WorktreeSteering(second)
	if !strings.Contains(secondPreamble, second) {
		t.Fatalf("steering preamble ignored the second evidence directory %q:\n%s", second, secondPreamble)
	}
	if firstPreamble == secondPreamble {
		t.Fatal("steering preamble is identical for two different evidence roots; it is not reading the supplied root")
	}
}

// TestSteeringPromptsAgentsWithTheSameRootTheRunWritesTo is the drift check
// between the two consumers of the evidence path: an agent steered for a run
// must be told about the very directory that run's steps hand it.
func TestSteeringPromptsAgentsWithTheSameRootTheRunWritesTo(t *testing.T) {
	evidenceRoot := filepath.Join(t.TempDir(), "evidence")
	runDir := filepath.Join(evidenceRoot, "run-123")

	inner := &recordingAgent{name: "claude"}
	steered := WithSteering(inner, evidenceRoot)
	if _, err := steered.Run(context.Background(), RunOpts{Prompt: "write evidence to " + runDir}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !strings.Contains(inner.gotOpts.Prompt, evidenceRoot) {
		t.Fatalf("agent was never told its evidence root %q:\n%s", evidenceRoot, inner.gotOpts.Prompt)
	}
	before, _, ok := strings.Cut(inner.gotOpts.Prompt, "write evidence to")
	if !ok || !strings.Contains(before, evidenceRoot) {
		t.Fatalf("steering preamble did not name the evidence root ahead of the task prompt:\n%s", inner.gotOpts.Prompt)
	}
}

func TestWorktreeSteering_AllowsEphemeralToolchainWrites(t *testing.T) {
	preamble := WorktreeSteering(t.TempDir())
	prompt := strings.ToLower(preamble)
	for _, want := range []string{"ephemeral", "toolchain", "temp", "cache"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("steering preamble does not mention %q writes:\n%s", want, preamble)
		}
	}
}

func TestWorktreeSteering_DescribesSoftBoundary(t *testing.T) {
	preamble := WorktreeSteering(t.TempDir())
	prompt := strings.ToLower(preamble)
	for _, want := range []string{"soft boundary", "prompt steering", "not true enforcement"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("steering preamble does not mention %q:\n%s", want, preamble)
		}
	}
}

// TestWorktreeSteering_BoundsHostFilesystemSearch pins the host-search boundary
// added after a Test run spent its whole 30-minute budget on an agent-authored
// `find / -maxdepth 4` that blocked in a macOS automount at 0% CPU, so no
// scenario ever ran. The preamble that allows out-of-worktree reads must name
// whole-root searches as disallowed (including the exact `find /` shape and the
// `mdfind /` spotlight variant), keep bounded worktree/evidence/repo-local reads
// allowed, and spell out a missing-tool fallback that stays role-neutral: only
// the Test step has scenarios and an "untested" state, so the shared preamble
// asks the role to report the missing tool in its own normal result.
func TestWorktreeSteering_BoundsHostFilesystemSearch(t *testing.T) {
	preamble := WorktreeSteering(filepath.Join(t.TempDir(), "evidence"))
	normalized := strings.Join(strings.Fields(preamble), " ")
	for _, want := range []string{
		"Do not search the host filesystem",
		"Never run a filesystem-wide search such as `find /` or `mdfind /`",
		"never hunt the machine for an installed tool",
		"does not make a whole-root search bounded",
		"Bounded searches inside the worktree",
		"external evidence path a prompt explicitly names",
		"repository-local path you were given remain fine",
		"not on PATH and no repository-local path is supplied",
		"report the missing tool and the work it blocked in your normal result",
		"with the concrete reason, and stop there",
	} {
		if !strings.Contains(normalized, want) {
			t.Errorf("steering preamble missing host-search boundary %q:\n%s", want, preamble)
		}
	}
	// The shared fallback must not claim a scenario/untested shape that only the
	// Test step's schema supports; the Test prompt owns that wording.
	if strings.Contains(normalized, "untested") || strings.Contains(normalized, "scenario") {
		t.Errorf("shared steering fallback is not role-neutral (mentions scenario/untested):\n%s", preamble)
	}
	// The pre-existing read allowance must survive: this narrows an unbounded
	// host search, it does not ban reading outside the worktree.
	if !strings.Contains(preamble, "You may read files outside the worktree and run read-only commands") {
		t.Errorf("steering preamble dropped the out-of-worktree read allowance:\n%s", preamble)
	}
}
