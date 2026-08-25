package agent

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/runenv"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestGitSafeEnvAppliesRunOverlayBeforeGateEnvironment(t *testing.T) {
	t.Setenv("GH_TOKEN", "ambient-token")
	env := gitSafeEnvWithOverlay("/work/dir", runenv.Overlay{
		Set:   map[string]string{"GH_CONFIG_DIR": "/profiles/personal"},
		Unset: []string{"GH_TOKEN"},
	})
	resolved := resolveAgentEnv(env)

	if _, ok := resolved["GH_TOKEN"]; ok {
		t.Fatal("GH_TOKEN remained in agent environment")
	}
	if got := resolved["GH_CONFIG_DIR"]; got != "/profiles/personal" {
		t.Fatalf("GH_CONFIG_DIR = %q, want /profiles/personal", got)
	}
	if got := resolved[GateRoleEnvVar]; got != "1" {
		t.Fatalf("%s = %q, want 1", GateRoleEnvVar, got)
	}
}

func resolveAgentEnv(env []string) map[string]string {
	m := map[string]string{}
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		m[k] = v
	}
	return m
}

func TestGitSafeEnv_DisablesInteractiveGit(t *testing.T) {
	t.Setenv("GIT_EDITOR", "vim")

	resolved := resolveAgentEnv(gitSafeEnv("/work/dir"))

	if resolved["GIT_EDITOR"] != "true" {
		t.Errorf("GIT_EDITOR = %q, want \"true\"", resolved["GIT_EDITOR"])
	}
	if resolved["GIT_SEQUENCE_EDITOR"] != "true" {
		t.Errorf("GIT_SEQUENCE_EDITOR = %q, want \"true\"", resolved["GIT_SEQUENCE_EDITOR"])
	}
	if resolved["GIT_TERMINAL_PROMPT"] != "0" {
		t.Errorf("GIT_TERMINAL_PROMPT = %q, want \"0\"", resolved["GIT_TERMINAL_PROMPT"])
	}
}

// TestGitSafeEnv_CouplesPWDToWorkdir guards the regression where assigning
// cmd.Env dropped os/exec's automatic PWD=cmd.Dir, making os.Getwd in the agent
// report a symlink-resolved path instead of the worktree path.
func TestGitSafeEnv_CouplesPWDToWorkdir(t *testing.T) {
	t.Setenv("PWD", "/somewhere/else")

	resolved := resolveAgentEnv(gitSafeEnv("/work/dir"))

	if runtime.GOOS == "windows" || runtime.GOOS == "plan9" {
		if resolved["PWD"] != "/somewhere/else" {
			t.Errorf("PWD = %q, want ambient PWD on %s", resolved["PWD"], runtime.GOOS)
		}
		return
	}

	if resolved["PWD"] != "/work/dir" {
		t.Errorf("PWD = %q, want \"/work/dir\"", resolved["PWD"])
	}
}

// TestGitSafeEnv_StampsGateRoleMarker locks in the ambient-authority containment
// marker: every spawned gate agent must carry NO_MISTAKES_GATE=1 so a
// cooperating orchestration harness in the target repo can recognize the gate
// agent and refuse to let it drive the fleet. If this regresses, a gate agent
// validating a firstmate-shaped repo becomes indistinguishable from a real
// fleet operator.
func TestGitSafeEnv_StampsGateRoleMarker(t *testing.T) {
	resolved := resolveAgentEnv(gitSafeEnv("/work/dir"))
	if resolved[GateRoleEnvVar] != "1" {
		t.Errorf("%s = %q, want \"1\"", GateRoleEnvVar, resolved[GateRoleEnvVar])
	}
}

func TestGitSafeEnvIsObservedBySpawnedProcess(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestAgentEnvProbe$")
	cmd.Env = gitSafeEnv(t.TempDir(), []string{"NM_HOME=/isolated/eval", GateRoleEnvVar + "=0", "NM_TEST_ENV_PROBE=1"})
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("run environment probe: %v", err)
	}
	if got := string(output); !strings.HasPrefix(got, "/isolated/eval|1") {
		t.Fatalf("spawned environment = %q, want isolated home and gate marker", got)
	}
}

// TestEverySupportedAdapterCarriesTheRunOverlayAndGateMarker executes the
// environment every adapter NewWithOptions can build. Native one-shot adapters
// spawn their command directly, OpenCode and Rovo Dev hand the same overlay to
// the shared managed-server launcher, and Cursor plus arbitrary ACP targets go
// through acpx - so both the containment marker and the run-scoped forge
// overlay are asserted per adapter rather than trusted to stay in sync.
func TestEverySupportedAdapterCarriesTheRunOverlayAndGateMarker(t *testing.T) {
	t.Setenv("GH_TOKEN", "ambient-must-not-leak")
	t.Setenv(GateRoleEnvVar, "0")

	overlay := runenv.Overlay{
		Set:   map[string]string{"GH_CONFIG_DIR": "/profiles/personal"},
		Unset: []string{"GH_TOKEN"},
	}

	for _, name := range []types.AgentName{
		types.AgentClaude,
		types.AgentCodex,
		types.AgentGrok,
		types.AgentRovoDev,
		types.AgentOpenCode,
		types.AgentPi,
		types.AgentCopilot,
		types.AgentCursor,
		types.AgentAntigravity,
		types.AgentName("acp:some-target"),
	} {
		t.Run(string(name), func(t *testing.T) {
			created, err := NewWithOptions(name, "fake-bin", nil, Options{Environment: overlay})
			if err != nil {
				t.Fatalf("NewWithOptions(%s): %v", name, err)
			}
			scoped, ok := created.(interface {
				gitSafeEnv(dir string, extra ...[]string) []string
				overlay() runenv.Overlay
			})
			if !ok {
				t.Fatalf("%s (%T) does not carry the run-scoped subprocess context", name, created)
			}

			resolved := resolveAgentEnv(scoped.gitSafeEnv("/work/dir"))
			if got := resolved["GH_CONFIG_DIR"]; got != "/profiles/personal" {
				t.Errorf("GH_CONFIG_DIR = %q, want /profiles/personal", got)
			}
			if _, leaked := resolved["GH_TOKEN"]; leaked {
				t.Error("ambient GH_TOKEN survived into the agent environment")
			}
			if got := resolved[GateRoleEnvVar]; got != "1" {
				t.Errorf("%s = %q, want \"1\"", GateRoleEnvVar, got)
			}

			// The managed-server route reaches its child through this overlay;
			// TestStartServerWithPortAppliesForgeEnvironment spawns the process
			// that observes it.
			handed := scoped.overlay()
			if got := handed.Set["GH_CONFIG_DIR"]; got != "/profiles/personal" {
				t.Errorf("overlay handed to the managed-server launcher set GH_CONFIG_DIR=%q", got)
			}
			if len(handed.Unset) != 1 || handed.Unset[0] != "GH_TOKEN" {
				t.Errorf("overlay handed to the managed-server launcher unsets %v, want [GH_TOKEN]", handed.Unset)
			}
		})
	}
}

// TestGitSafeEnvIsTheEmptyOverlayCase pins the free helper as exactly the
// zero-overlay spelling of the overlay-aware one, so a caller that has no
// run-scoped forge context cannot drift onto a different environment policy.
func TestGitSafeEnvIsTheEmptyOverlayCase(t *testing.T) {
	extra := []string{"NM_HOME=/isolated/eval"}
	plain := gitSafeEnv("/work/dir", extra)
	overlaid := gitSafeEnvWithOverlay("/work/dir", runenv.Overlay{}, extra)

	if !slices.Equal(plain, overlaid) {
		t.Fatalf("gitSafeEnv = %v, want the empty-overlay result %v", plain, overlaid)
	}
	resolved := resolveAgentEnv(plain)
	if resolved[GateRoleEnvVar] != "1" {
		t.Errorf("%s = %q, want \"1\"", GateRoleEnvVar, resolved[GateRoleEnvVar])
	}
	if resolved["NM_HOME"] != "/isolated/eval" {
		t.Errorf("NM_HOME = %q, want /isolated/eval", resolved["NM_HOME"])
	}
}

func TestAgentEnvProbe(t *testing.T) {
	if os.Getenv("NM_TEST_ENV_PROBE") != "1" {
		return
	}
	fmt.Printf("%s|%s", os.Getenv("NM_HOME"), os.Getenv(GateRoleEnvVar))
}

// TestGitSafeEnv_GateMarkerWinsOverAmbient guards that a target repo (or a
// confused parent) cannot pre-empt the marker with its own ambient value: the
// stamp is appended last, and exec resolves duplicate keys to the last
// occurrence.
func TestGitSafeEnv_GateMarkerWinsOverAmbient(t *testing.T) {
	t.Setenv(GateRoleEnvVar, "0")
	resolved := resolveAgentEnv(gitSafeEnv("/work/dir"))
	if resolved[GateRoleEnvVar] != "1" {
		t.Errorf("%s = %q, want \"1\" (managed stamp must win over ambient)", GateRoleEnvVar, resolved[GateRoleEnvVar])
	}
}
