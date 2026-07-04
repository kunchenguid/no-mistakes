package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// writeProfile lays down a profile directory under nmHome/profiles/<name>/ with
// the given profile.yaml body and extra files, returning the profile dir.
func writeProfile(t *testing.T, nmHome, name, profileYAML string, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(nmHome, "profiles", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "profile.yaml"), []byte(profileYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	for rel, content := range files {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func newProfileManager(nmHome string) *RunManager {
	return &RunManager{paths: paths.WithRoot(nmHome)}
}

// A missing profile directory / profile.yaml fails loud: loadProfile returns an
// error so the run fails at start rather than silently dropping to the default
// pipeline.
func TestLoadProfile_MissingFailsLoud(t *testing.T) {
	m := newProfileManager(t.TempDir())
	if _, _, err := m.loadProfile("team-ios"); err == nil {
		t.Fatal("expected an error for a missing profile (fail closed)")
	}
}

// An unparsable profile.yaml fails loud too.
func TestLoadProfile_UnparsableFailsLoud(t *testing.T) {
	nmHome := t.TempDir()
	writeProfile(t, nmHome, "team-ios", "steps: [oops\n", nil)
	m := newProfileManager(nmHome)
	if _, _, err := m.loadProfile("team-ios"); err == nil {
		t.Fatal("expected an error for an unparsable profile.yaml")
	}
}

// An unsafe profile name is rejected before any filesystem read.
func TestLoadProfile_UnsafeNameRejected(t *testing.T) {
	m := newProfileManager(t.TempDir())
	for _, bad := range []string{"../escape", "team/ios", "Team", "", ".hidden"} {
		if _, _, err := m.loadProfile(bad); err == nil {
			t.Errorf("expected an error for unsafe profile name %q", bad)
		}
	}
}

func TestLoadProfile_ParsesSteps(t *testing.T) {
	nmHome := t.TempDir()
	writeProfile(t, nmHome, "team-ios", "version: 2\nsteps:\n  - rebase\n  - push\n", nil)
	m := newProfileManager(nmHome)
	profile, dir, err := m.loadProfile("team-ios")
	if err != nil {
		t.Fatalf("loadProfile: %v", err)
	}
	if profile.Version != 2 {
		t.Errorf("version = %d, want 2", profile.Version)
	}
	if len(profile.Steps) != 2 {
		t.Fatalf("steps = %d, want 2", len(profile.Steps))
	}
	if dir != m.paths.ProfileDir("team-ios") {
		t.Errorf("dir = %q, want %q", dir, m.paths.ProfileDir("team-ios"))
	}
}

// A profile's skill body is read from the profile directory on disk, never from
// a repo worktree. The body content proves the disk read happened.
func TestLoadProfileSkillBodies_ReadsFromProfileDir(t *testing.T) {
	nmHome := t.TempDir()
	const body = "---\nname: ios-review\nmode: review\n---\nFlag force unwraps."
	dir := writeProfile(t, nmHome, "team-ios",
		"steps:\n  - name: ios-review\n    type: skill\n    skill: skills/ios-review.md\n    mode: review\n",
		map[string]string{"skills/ios-review.md": body})

	specs := []config.StepSpec{
		{Name: "rebase"},
		{Name: "ios-review", Skill: "skills/ios-review.md", Mode: "review"},
	}
	got := loadProfileSkillBodies(dir, specs, "test-run")
	if got[0].SkillBody != "" {
		t.Errorf("non-skill spec should get no body, got %q", got[0].SkillBody)
	}
	if !strings.Contains(got[1].SkillBody, "Flag force unwraps") {
		t.Errorf("skill body = %q, want the profile-dir file content", got[1].SkillBody)
	}
}

// A skill path that escapes the profile directory is refused: the body stays
// empty (the step will then park with a misconfiguration finding) and no file
// outside the profile dir is read.
func TestLoadProfileSkillBodies_PathEscapeRefused(t *testing.T) {
	nmHome := t.TempDir()
	// A secret file a step must not be able to read via path traversal.
	secret := filepath.Join(nmHome, "secret.md")
	if err := os.WriteFile(secret, []byte("TOP SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := writeProfile(t, nmHome, "team-ios", "steps: []\n", nil)

	specs := []config.StepSpec{
		{Name: "sneaky", Skill: "../secret.md", Mode: "review"},
	}
	got := loadProfileSkillBodies(dir, specs, "test-run")
	if got[0].SkillBody != "" {
		t.Fatalf("SECURITY: escaping skill path was read: %q", got[0].SkillBody)
	}
}

// A missing profile skill file yields an empty body (the step parks), not an error.
func TestLoadProfileSkillBodies_MissingFileEmptyBody(t *testing.T) {
	nmHome := t.TempDir()
	dir := writeProfile(t, nmHome, "team-ios", "steps: []\n", nil)
	specs := []config.StepSpec{{Name: "ios-review", Skill: "skills/absent.md", Mode: "review"}}
	got := loadProfileSkillBodies(dir, specs, "test-run")
	if got[0].SkillBody != "" {
		t.Errorf("want empty body for a missing skill file, got %q", got[0].SkillBody)
	}
}

func TestLoadProfileStepInstructions_ReadsFromProfileDir(t *testing.T) {
	nmHome := t.TempDir()
	dir := writeProfile(t, nmHome, "team-ios", "steps: []\n",
		map[string]string{"instructions/swift.md": "Prefer guard-let."})
	specs := []config.StepSpec{
		{Name: "review", Instructions: []string{"instructions/swift.md"}},
	}
	got := loadProfileStepInstructions(dir, specs, "test-run")
	if !strings.Contains(got, "Prefer guard-let") {
		t.Errorf("instructions = %q, want the profile-dir file content", got)
	}
}

func TestLoadProfileStepInstructions_PathEscapeSkipped(t *testing.T) {
	nmHome := t.TempDir()
	if err := os.WriteFile(filepath.Join(nmHome, "secret.md"), []byte("SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := writeProfile(t, nmHome, "team-ios", "steps: []\n", nil)
	specs := []config.StepSpec{{Name: "review", Instructions: []string{"../secret.md"}}}
	if got := loadProfileStepInstructions(dir, specs, "test-run"); got != "" {
		t.Fatalf("SECURITY: escaping instruction path was read: %q", got)
	}
}

func TestProfilePathWithinDir(t *testing.T) {
	dir := "/home/u/.no-mistakes/profiles/team-ios"
	cases := []struct {
		rel  string
		want bool
	}{
		{"skills/review.md", true},
		{"a/b/c.md", true},
		{"../secret.md", false},
		{"../../etc/passwd", false},
		{"/etc/passwd", false},
		{"", false},
	}
	for _, c := range cases {
		if _, ok := profilePathWithinDir(dir, c.rel); ok != c.want {
			t.Errorf("profilePathWithinDir(%q) safe=%v, want %v", c.rel, ok, c.want)
		}
	}
}

func TestJoinInstructionSections(t *testing.T) {
	if got := joinInstructionSections("", ""); got != "" {
		t.Errorf("empty inputs = %q, want empty", got)
	}
	if got := joinInstructionSections("A", ""); got != "A" {
		t.Errorf("= %q, want A", got)
	}
	if got := joinInstructionSections("A", "B"); got != "A\n\nB" {
		t.Errorf("= %q, want A\\n\\nB", got)
	}
}
