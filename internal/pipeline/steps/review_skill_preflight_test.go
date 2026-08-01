package steps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/skill"
)

func mustMkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func writeSkill(t *testing.T, dir string) {
	t.Helper()
	mustMkdirAll(t, dir)
	if err := os.WriteFile(filepath.Join(dir, skill.SkillFile), []byte("# skill\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func pinReviewSkillHome(t *testing.T, home string) {
	t.Helper()
	previous := reviewSkillHomeDir
	reviewSkillHomeDir = func() (string, error) { return home, nil }
	t.Cleanup(func() { reviewSkillHomeDir = previous })
}

// A machine with a skill library but no comprehensive-code-review must fail
// before the first review turn, and say the skill is missing rather than
// blaming the agent for ignoring the prompt two turns later.
func TestAssertReviewSkillInstalled_MissingSkillFailsWithSetupError(t *testing.T) {
	home := t.TempDir()
	writeSkill(t, filepath.Join(home, ".claude", "skills", "some-other-skill"))
	pinReviewSkillHome(t, home)

	err := assertReviewSkillInstalled(t.TempDir(), "claude")
	if err == nil {
		t.Fatal("expected an error when the required review skill is not installed")
	}
	if !strings.Contains(err.Error(), requiredReviewSkill) || !strings.Contains(err.Error(), "not installed for agent claude") {
		t.Errorf("error = %q, want it to name the skill and the agent", err)
	}
}

// Every layout an agent can actually resolve the skill from must satisfy the
// preflight, or a correctly set up machine fails its runs.
func TestAssertReviewSkillInstalled_AcceptsEveryResolvableLayout(t *testing.T) {
	tests := []struct {
		name    string
		relPath string
		inRepo  bool
	}{
		{"user claude skills", filepath.Join(".claude", "skills", requiredReviewSkill), false},
		{"user agents skills", filepath.Join(".agents", "skills", requiredReviewSkill), false},
		{"user linked plugin", filepath.Join(".claude", "plugins", "reviewer", "skills", requiredReviewSkill), false},
		{"user marketplace plugin cache", filepath.Join(".claude", "plugins", "cache", "marketplace", "reviewer", "2.3.0", "skills", requiredReviewSkill), false},
		{"project level", filepath.Join(".claude", "skills", requiredReviewSkill), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			workDir := t.TempDir()
			root := home
			if tt.inRepo {
				root = workDir
			}
			// The mandate only applies where a skill library exists, so every
			// layout case must be judgeable or it passes vacuously.
			mustMkdirAll(t, filepath.Join(home, ".claude", "skills"))
			writeSkill(t, filepath.Join(root, tt.relPath))
			pinReviewSkillHome(t, home)

			if err := assertReviewSkillInstalled(workDir, "claude"); err != nil {
				t.Errorf("assertReviewSkillInstalled() = %v, want nil", err)
			}
		})
	}
}

// A plugin directory shaped like the skill but carrying no SKILL.md is not a
// resolvable install, so the preflight must still fail.
func TestAssertReviewSkillInstalled_PluginDirWithoutSkillFileFails(t *testing.T) {
	home := t.TempDir()
	mustMkdirAll(t, filepath.Join(home, ".claude", "skills"))
	mustMkdirAll(t, filepath.Join(home, ".claude", "plugins", "cache", "marketplace", "reviewer", "2.3.0", "skills", requiredReviewSkill))
	pinReviewSkillHome(t, home)

	if err := assertReviewSkillInstalled(t.TempDir(), "claude"); err == nil {
		t.Fatal("expected an error when the plugin skill directory carries no SKILL.md")
	}
}

// A host that has never run `no-mistakes init` has no skill directory to read
// an absence from, so the preflight has nothing to judge. Every initialized
// machine has the install bases and is judged unconditionally.
func TestAssertReviewSkillInstalled_NoSkillLibraryDoesNotFailTheRun(t *testing.T) {
	pinReviewSkillHome(t, t.TempDir())

	if err := assertReviewSkillInstalled(t.TempDir(), "codex"); err != nil {
		t.Errorf("assertReviewSkillInstalled() = %v, want nil when no skill library exists", err)
	}
}
