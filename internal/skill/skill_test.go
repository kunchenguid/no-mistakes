package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMarkdownFrontmatter(t *testing.T) {
	md := Markdown()
	if !strings.HasPrefix(md, "---\n") {
		t.Fatalf("SKILL.md must start with YAML frontmatter, got:\n%s", md[:min(40, len(md))])
	}
	for _, want := range []string{
		"name: " + Name + "\n",
		"description: " + Description + "\n",
		"user-invocable: true\n",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("frontmatter missing %q", want)
		}
	}
	// Frontmatter block must be closed before the body.
	if strings.Count(md, "---\n") < 2 {
		t.Errorf("frontmatter not closed with a second --- delimiter")
	}
	if !strings.Contains(md, "no-mistakes axi run") {
		t.Errorf("body should document the axi run command")
	}
	// The user-level install is a genuine user installation, so it must stay
	// discoverable: the internal marker that hid the old vendored repo copies
	// must not come back.
	if strings.Contains(md, "internal: true") {
		t.Errorf("Markdown() must not be marked internal")
	}
}

func TestBodyDocumentsTaskFirstFlow(t *testing.T) {
	md := Markdown()
	for _, want := range []string{
		"## Two ways to invoke",
		"feature branch",
		"Inspect `git status` before you change or commit anything",
		"commit only the changes that belong to the user's task",
		"passing the user's task as your `--intent`",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("body should document the task-first flow: missing %q", want)
		}
	}
}

func TestBodyDocumentsAxiGateGuidance(t *testing.T) {
	md := Markdown()
	for _, want := range []string{
		"inspect it with `no-mistakes axi status`",
		"drive it with `no-mistakes axi respond`",
		"when it still matches your current `HEAD`",
		"**Review auto-fix is disabled by default**",
		"blocking and",
		"ask-user review findings park for your decision",
		"`auto_fix.review > 0`",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("body should document AXI gate guidance: missing %q", want)
		}
	}
	if strings.Contains(md, "drive it to an outcome with `axi respond`") {
		t.Errorf("body should not tell agents to resume non-parked runs with axi respond")
	}
}

func TestInstallWritesBothPaths(t *testing.T) {
	root := t.TempDir()
	written, err := Install(root)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	wantRel := expectedInstallEntrypoints()
	if len(written) != len(wantRel) {
		t.Fatalf("written = %v, want %v", written, wantRel)
	}
	for i, rel := range wantRel {
		if written[i] != rel {
			t.Errorf("written[%d] = %q, want %q", i, written[i], rel)
		}
		assertInstalledSkill(t, root, rel)
	}
}

// TestInstallUserWritesUnderHome proves the init entry point resolves the
// user's home directory and installs there, never into the working directory.
func TestInstallUserWritesUnderHome(t *testing.T) {
	home := t.TempDir()
	// os.UserHomeDir reads HOME on Unix and USERPROFILE on Windows; set both
	// so the test isolates the real home directory on every platform.
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	written, err := InstallUser()
	if err != nil {
		t.Fatalf("InstallUser: %v", err)
	}
	if len(written) != len(InstallBases)*2 {
		t.Fatalf("written = %v, want no-mistakes and improve-codebase under each base", written)
	}
	for _, base := range InstallBases {
		assertInstalledNoMistakes(t, filepath.Join(home, base, Name, "SKILL.md"))
		assertInstalledBundledImproveCodebase(t, filepath.Join(home, base, "improve-codebase"))
	}
}

func TestInstallIsIdempotent(t *testing.T) {
	root := t.TempDir()
	if _, err := Install(root); err != nil {
		t.Fatalf("first install: %v", err)
	}
	if _, err := Install(root); err != nil {
		t.Fatalf("second install: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, ".claude", "skills", Name, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != Markdown() {
		t.Errorf("content drifted after re-install")
	}
	assertInstalledBundledImproveCodebase(t, filepath.Join(root, ".claude", "skills", "improve-codebase"))
}

// TestInstallSymlinkLayouts covers home directories that consolidate the two
// skill bases with a symlink. `.claude/skills` may link to `.agents/skills`,
// the whole `.claude` dir may link to `.agents`, or the link may point the
// other way. In every case Install must succeed and the skill must be
// reachable via both logical bases - including when the symlink target dir
// does not exist yet.
func TestInstallSymlinkLayouts(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, root string)
	}{
		{
			name: "claude_skills_link_target_exists",
			setup: func(t *testing.T, root string) {
				mkdirAll(t, filepath.Join(root, ".agents", "skills"))
				mkdirAll(t, filepath.Join(root, ".claude"))
				symlink(t, filepath.Join("..", ".agents", "skills"), filepath.Join(root, ".claude", "skills"))
			},
		},
		{
			name: "claude_skills_link_target_missing",
			setup: func(t *testing.T, root string) {
				mkdirAll(t, filepath.Join(root, ".claude"))
				symlink(t, filepath.Join("..", ".agents", "skills"), filepath.Join(root, ".claude", "skills"))
			},
		},
		{
			name: "claude_dir_link",
			setup: func(t *testing.T, root string) {
				mkdirAll(t, filepath.Join(root, ".agents"))
				symlink(t, ".agents", filepath.Join(root, ".claude"))
			},
		},
		{
			name: "agents_skills_link_reverse",
			setup: func(t *testing.T, root string) {
				mkdirAll(t, filepath.Join(root, ".claude", "skills"))
				mkdirAll(t, filepath.Join(root, ".agents"))
				symlink(t, filepath.Join("..", ".claude", "skills"), filepath.Join(root, ".agents", "skills"))
			},
		},
		{
			name: "agents_dir_link_reverse",
			setup: func(t *testing.T, root string) {
				mkdirAll(t, filepath.Join(root, ".claude"))
				symlink(t, ".claude", filepath.Join(root, ".agents"))
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			tt.setup(t, root)

			written, err := Install(root)
			if err != nil {
				t.Fatalf("Install: %v", err)
			}

			// Every reported path must be readable with current content.
			for _, rel := range written {
				assertInstalledSkill(t, root, rel)
			}

			// The skills must be discoverable via both logical bases no matter
			// which side carries the symlink.
			for _, base := range InstallBases {
				assertInstalledNoMistakes(t, filepath.Join(root, base, Name, "SKILL.md"))
				assertInstalledBundledImproveCodebase(t, filepath.Join(root, base, "improve-codebase"))
			}
		})
	}
}

// TestInstallOverwritesStaleContent guards the upgrade path: an older SKILL.md
// left by a previous binary version must be refreshed to current content when
// Install runs again.
func TestInstallOverwritesStaleContent(t *testing.T) {
	root := t.TempDir()
	stale := filepath.Join(root, ".claude", "skills", Name, "SKILL.md")
	mkdirAll(t, filepath.Dir(stale))
	if err := os.WriteFile(stale, []byte("---\nname: "+Name+"\n---\nstale body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(root); err != nil {
		t.Fatalf("Install: %v", err)
	}
	data, err := os.ReadFile(stale)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != Markdown() {
		t.Errorf("stale SKILL.md was not refreshed to current content")
	}

	staleBundled := filepath.Join(root, ".claude", "skills", "improve-codebase", "SKILL.md")
	if err := os.WriteFile(staleBundled, []byte("---\nname: improve-codebase\n---\nstale body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(root); err != nil {
		t.Fatalf("Install: %v", err)
	}
	assertInstalledBundledImproveCodebase(t, filepath.Dir(staleBundled))
}

func TestInstallRejectsSymlinkCycle(t *testing.T) {
	root := t.TempDir()
	symlink(t, ".agents", filepath.Join(root, ".claude"))
	symlink(t, ".claude", filepath.Join(root, ".agents"))

	if _, err := Install(root); err == nil {
		t.Fatalf("Install succeeded with cyclic skill directory symlinks")
	}
}

// TestVendored covers the legacy-detection helper init uses to tell users a
// repo still carries a vendored skill copy from an older no-mistakes version.
func TestVendored(t *testing.T) {
	t.Run("clean_repo", func(t *testing.T) {
		if got := Vendored(t.TempDir()); len(got) != 0 {
			t.Errorf("Vendored on a clean repo = %v, want none", got)
		}
	})

	t.Run("both_copies", func(t *testing.T) {
		root := t.TempDir()
		for _, base := range InstallBases {
			dir := filepath.Join(root, base, Name)
			mkdirAll(t, dir)
			if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("legacy"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		want := []string{
			filepath.Join(".claude", "skills", Name, "SKILL.md"),
			filepath.Join(".agents", "skills", Name, "SKILL.md"),
		}
		got := Vendored(root)
		if len(got) != len(want) {
			t.Fatalf("Vendored = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("Vendored[%d] = %q, want %q", i, got[i], want[i])
			}
		}
	})

	t.Run("single_copy", func(t *testing.T) {
		root := t.TempDir()
		dir := filepath.Join(root, ".agents", "skills", Name)
		mkdirAll(t, dir)
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("legacy"), 0o644); err != nil {
			t.Fatal(err)
		}
		got := Vendored(root)
		if len(got) != 1 || got[0] != filepath.Join(".agents", "skills", Name, "SKILL.md") {
			t.Errorf("Vendored = %v, want only the .agents copy", got)
		}
	})

	t.Run("unrelated_skill_ignored", func(t *testing.T) {
		root := t.TempDir()
		dir := filepath.Join(root, ".claude", "skills", "other-skill")
		mkdirAll(t, dir)
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("other"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := Vendored(root); len(got) != 0 {
			t.Errorf("Vendored must ignore unrelated skills, got %v", got)
		}
	})
}

func mkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func expectedInstallEntrypoints() []string {
	return []string{
		filepath.Join(".claude", "skills", Name, "SKILL.md"),
		filepath.Join(".agents", "skills", Name, "SKILL.md"),
		filepath.Join(".claude", "skills", "improve-codebase", "SKILL.md"),
		filepath.Join(".agents", "skills", "improve-codebase", "SKILL.md"),
	}
}

func assertInstalledSkill(t *testing.T, root, rel string) {
	t.Helper()
	switch filepath.Base(filepath.Dir(rel)) {
	case Name:
		assertInstalledNoMistakes(t, filepath.Join(root, rel))
	case "improve-codebase":
		assertInstalledBundledImproveCodebase(t, filepath.Dir(filepath.Join(root, rel)))
	default:
		t.Fatalf("unexpected installed skill path %s", rel)
	}
}

func assertInstalledNoMistakes(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read no-mistakes skill at %s: %v", path, err)
	}
	if string(data) != Markdown() {
		t.Errorf("%s content does not match Markdown()", path)
	}
}

func assertInstalledBundledImproveCodebase(t *testing.T, dir string) {
	t.Helper()
	skillPath := filepath.Join(dir, "SKILL.md")
	data, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatalf("read improve-codebase skill at %s: %v", skillPath, err)
	}
	want, err := bundledSkills.ReadFile("bundled/improve-codebase/SKILL.md")
	if err != nil {
		t.Fatalf("read embedded improve-codebase skill: %v", err)
	}
	if string(data) != string(want) {
		t.Errorf("%s content does not match embedded improve-codebase skill", skillPath)
	}

	// Guard against accidentally installing only the entrypoint without the
	// supporting instruction files the standalone package references.
	for _, rel := range []string{
		"CHANGE-SET-QUALITY.md",
		"CODE-HEALTH.md",
		"LENS-ORCHESTRATION.md",
		filepath.Join("agents", "openai.yaml"),
	} {
		supportFile := filepath.Join(dir, rel)
		if _, err := os.Stat(supportFile); err != nil {
			t.Fatalf("bundled improve-codebase support file missing at %s: %v", supportFile, err)
		}
	}
}

func TestBundledImproveCodebaseSkillHasStandaloneReadOnlyPackage(t *testing.T) {
	data, err := bundledSkills.ReadFile("bundled/improve-codebase/SKILL.md")
	if err != nil {
		t.Fatalf("read embedded improve-codebase skill: %v", err)
	}
	text := string(data)
	for _, want := range []string{
		"Standalone package with no required external tooling or hosted services",
		"This skill is audit-only by default",
		"Run one read-only, project-agnostic audit through five built-in lenses",
		"LENS-ORCHESTRATION.md",
		"CODE-HEALTH.md",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("bundled improve-codebase skill missing %q", want)
		}
	}
}

func TestBundledImproveCodebaseMatchesPublicSkill(t *testing.T) {
	bundled := readSkillTree(t, filepath.Join("bundled", "improve-codebase"))
	public := readSkillTree(t, filepath.Join("..", "..", "skills", "improve-codebase"))

	for path, want := range public {
		if got, ok := bundled[path]; !ok {
			t.Fatalf("bundled improve-codebase skill missing %s", path)
		} else if got != want {
			t.Fatalf("bundled improve-codebase skill drifted from public copy at %s", path)
		}
	}
	for path := range bundled {
		if _, ok := public[path]; !ok {
			t.Fatalf("bundled improve-codebase skill has extra file %s", path)
		}
	}
}

func readSkillTree(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = string(data)
		return nil
	}); err != nil {
		t.Fatalf("read skill tree %s: %v", root, err)
	}
	return files
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
