package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/kunchenguid/no-mistakes/internal/testguidance"
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

// TestMarkdownFrontmatterParsesAsYAML is the guard the substring checks above
// cannot be: Markdown() builds the frontmatter by concatenation, so a
// Description containing a colon-space, a leading quote or bracket, or a
// trailing " #" comment marker emits YAML that no longer means what it reads
// like. The agent then loads the skill with a missing or truncated trigger
// description and silently stops firing, and genskill --check still passes
// because the committed file matches the equally broken generated one. Parse
// the real thing and require every field to round-trip.
func TestMarkdownFrontmatterParsesAsYAML(t *testing.T) {
	md := Markdown()
	parts := strings.SplitN(md, "---\n", 3)
	if len(parts) < 3 {
		t.Fatalf("Markdown() has no closed frontmatter block")
	}
	var fm struct {
		Name          string `yaml:"name"`
		Description   string `yaml:"description"`
		UserInvocable bool   `yaml:"user-invocable"`
	}
	if err := yaml.Unmarshal([]byte(parts[1]), &fm); err != nil {
		t.Fatalf("frontmatter is not valid YAML: %v\n%s", err, parts[1])
	}
	if fm.Name != Name {
		t.Errorf("name round-trip: got %q, want %q", fm.Name, Name)
	}
	if fm.Description != Description {
		t.Errorf("description round-trip: got %q, want %q", fm.Description, Description)
	}
	if !fm.UserInvocable {
		t.Errorf("user-invocable must parse as the boolean true")
	}
}

func TestBodyIncludesGeneratedGateStepGuard(t *testing.T) {
	md := Markdown()
	for _, want := range []string{
		"## Active validation-step boundary",
		"must inspect, fix, and return only its assigned phase",
		"`error.code: nested_gate_context`",
		"return control to the outer executor",
		"`no-mistakes axi status`",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("installed skill guard snapshot missing %q", want)
		}
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
	if !strings.Contains(md, testguidance.Rule) {
		t.Errorf("task-first skill missing shared test-quality guidance:\n%s", md)
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

// TestFilesShipDisclosedReferences pins the multi-file contract: SKILL.md
// leads, every reference file ships beside it, and SKILL.md reaches each one
// by its relative path so the disclosed material stays reachable.
func TestFilesShipDisclosedReferences(t *testing.T) {
	files := Files()
	if len(files) < 2 {
		t.Fatalf("Files() = %d file(s), want SKILL.md plus reference files", len(files))
	}
	if files[0].Name != SkillFile || files[0].Content != Markdown() {
		t.Fatalf("Files()[0] must be %s rendered from Markdown(), got %q", SkillFile, files[0].Name)
	}
	md := Markdown()
	for _, f := range files[1:] {
		if f.Content == "" {
			t.Errorf("reference file %s is empty", f.Name)
		}
		if !strings.Contains(md, "("+f.Name+")") {
			t.Errorf("SKILL.md does not point at reference file %s by relative path", f.Name)
		}
	}
	for _, want := range []string{ReadingOutputFile, SyncRecoveryFile} {
		if !hasFile(files, want) {
			t.Errorf("Files() missing %s", want)
		}
	}
}

// TestBundleCoversEveryShippedFile proves the guidance-sync surface sees the
// disclosed reference content, not just SKILL.md.
func TestBundleCoversEveryShippedFile(t *testing.T) {
	bundle := Bundle()
	for _, f := range Files() {
		if !strings.Contains(bundle, f.Content) {
			t.Errorf("Bundle() is missing the content of %s", f.Name)
		}
	}
}

func TestInstallWritesEveryFileToBothBases(t *testing.T) {
	root := t.TempDir()
	written, err := Install(root)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	var wantRel []string
	for _, base := range []string{".claude", ".agents"} {
		for _, f := range Files() {
			wantRel = append(wantRel, filepath.Join(base, "skills", Name, f.Name))
		}
	}
	if len(written) != len(wantRel) {
		t.Fatalf("written = %v, want %v", written, wantRel)
	}
	for i, rel := range wantRel {
		if written[i] != rel {
			t.Errorf("written[%d] = %q, want %q", i, written[i], rel)
		}
	}
	assertInstalledContent(t, root)
}

// assertInstalledContent checks every shipped file is readable with current
// content through both logical bases.
func assertInstalledContent(t *testing.T, root string) {
	t.Helper()
	for _, base := range InstallBases {
		for _, f := range Files() {
			path := filepath.Join(root, base, Name, f.Name)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			if string(data) != f.Content {
				t.Errorf("%s content does not match the generator", path)
			}
		}
	}
}

func hasFile(files []File, name string) bool {
	for _, f := range files {
		if f.Name == name {
			return true
		}
	}
	return false
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
	if len(written) != len(InstallBases)*len(Files()) {
		t.Fatalf("written = %v, want one path per file per base", written)
	}
	assertInstalledContent(t, home)
}

func TestInstallIsIdempotent(t *testing.T) {
	root := t.TempDir()
	if _, err := Install(root); err != nil {
		t.Fatalf("first install: %v", err)
	}
	if _, err := Install(root); err != nil {
		t.Fatalf("second install: %v", err)
	}
	assertInstalledContent(t, root)
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
				if _, err := os.ReadFile(filepath.Join(root, rel)); err != nil {
					t.Fatalf("read reported %s: %v", rel, err)
				}
			}

			// The skill must be discoverable via both logical bases no matter
			// which side carries the symlink.
			assertInstalledContent(t, root)
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
}

// TestInstallRestoresStaleAndDeletedReferenceFiles covers the same upgrade
// path for disclosed reference files: SKILL.md points at them by relative
// path, so a hand-edited or deleted one must come back on the next install.
func TestInstallRestoresStaleAndDeletedReferenceFiles(t *testing.T) {
	root := t.TempDir()
	if _, err := Install(root); err != nil {
		t.Fatalf("first install: %v", err)
	}
	dir := filepath.Join(root, ".claude", "skills", Name)
	if err := os.WriteFile(filepath.Join(dir, ReadingOutputFile), []byte("stale reference\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, SyncRecoveryFile)); err != nil {
		t.Fatal(err)
	}

	if _, err := Install(root); err != nil {
		t.Fatalf("refresh install: %v", err)
	}
	assertInstalledContent(t, root)
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
		t.Skipf("symlinks unavailable: %v", err)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
