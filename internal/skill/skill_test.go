package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	// Check the generated, installed skill's guidance contract.
	md := skillReference(t, "getting-started")
	for _, want := range []string{
		"## Two ways to invoke",
		"feature branch",
		"Inspect `git status` before you change or commit anything",
		"commit only the changes that belong to the user's task",
		"passing the user's task as explicit intent",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("body should document the task-first flow: missing %q", want)
		}
	}
	if !strings.Contains(md, testguidance.Rule) {
		t.Errorf("task-first skill missing shared test-quality guidance:\n%s", md)
	}
}

// These expectations pin the task-first writing contract, independently of
// the shared constant used by the renderer. They do not test model behavior.
func TestBodyDocumentsIndependentTestOracles(t *testing.T) {
	md := strings.Join(strings.Fields(skillReference(t, "getting-started")), " ")
	for _, want := range []string{
		"Use an independent oracle",
		"expected result must come from somewhere other than the code under test",
		"a specification, worked example, published constant, external contract, or independently justified property",
		"Name the public behavior, the oracle's source, and a plausible wrong behavior the test would reject",
		"Do not only check your own mocks, compare a result to itself, or copy the implementation's expected-value rule",
		"External-boundary mocks, constants, snapshots, and computed expectations remain legitimate when they check an independent contract",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("task-first skill missing independent-oracle guidance %q:\n%s", want, md)
		}
	}
}

func TestBodyDocumentsAxiGateGuidance(t *testing.T) {
	md := skillReference(t, "getting-started") + skillReference(t, "gates")
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

func skillReference(t *testing.T, name string) string {
	t.Helper()
	path := "references/" + name + ".md"
	for _, file := range Files() {
		if file.Path == path {
			return file.Content
		}
	}
	t.Fatalf("skill reference %s is missing", path)
	return ""
}

func TestInstallWritesBothPaths(t *testing.T) {
	root := t.TempDir()
	written, err := Install(root)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	var wantRel []string
	for _, base := range InstallBases {
		for _, file := range Files() {
			wantRel = append(wantRel, filepath.Join(base, Name, filepath.FromSlash(file.Path)))
		}
	}
	if len(written) != len(wantRel) {
		t.Fatalf("written = %v, want %v", written, wantRel)
	}
	for i, rel := range wantRel {
		if written[i] != rel {
			t.Errorf("written[%d] = %q, want %q", i, written[i], rel)
		}
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if string(data) != Files()[i%len(Files())].Content {
			t.Errorf("%s content does not match generated document", rel)
		}
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
	if len(written) != len(InstallBases)*len(Files()) {
		t.Fatalf("written = %v, want every document per base", written)
	}
	for _, base := range InstallBases {
		data, err := os.ReadFile(filepath.Join(home, base, Name, "SKILL.md"))
		if err != nil {
			t.Fatalf("skill not installed under home at %s: %v", base, err)
		}
		if string(data) != Markdown() {
			t.Errorf("%s content does not match Markdown()", base)
		}
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
			for i, rel := range written {
				data, err := os.ReadFile(filepath.Join(root, rel))
				if err != nil {
					t.Fatalf("read reported %s: %v", rel, err)
				}
				if string(data) != Files()[i%len(Files())].Content {
					t.Errorf("%s content does not match generated document", rel)
				}
			}

			// The skill must be discoverable via both logical bases no matter
			// which side carries the symlink.
			for _, base := range InstallBases {
				p := filepath.Join(root, base, Name, "SKILL.md")
				data, err := os.ReadFile(p)
				if err != nil {
					t.Fatalf("skill not reachable via %s: %v", base, err)
				}
				if string(data) != Markdown() {
					t.Errorf("%s content does not match Markdown()", base)
				}
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
}

// The public package contract is a short entry point linking to four documents.
// Exercise init's user-level installation entry point twice with both stale
// files and missing files, then follow a recovery link through each logical
// agent installation. This is an automated installer test, not a live CLI run.
func TestInstallRefreshesCompleteSkillSet(t *testing.T) {
	root := t.TempDir()
	// Isolate os.UserHomeDir on both Unix and Windows.
	t.Setenv("HOME", root)
	t.Setenv("USERPROFILE", root)
	paths := []string{
		"SKILL.md",
		"references/getting-started.md",
		"references/gates.md",
		"references/recovery.md",
		"references/commands.md",
	}
	if _, err := InstallUser(); err != nil {
		t.Fatal(err)
	}
	for _, base := range InstallBases {
		for i, path := range paths {
			full := filepath.Join(root, base, Name, filepath.FromSlash(path))
			data, err := os.ReadFile(full)
			if err != nil || len(data) == 0 {
				t.Fatalf("initial install %s: bytes=%d, error=%v", full, len(data), err)
			}
			if i%2 == 0 {
				err = os.WriteFile(full, []byte("stale"), 0o644)
			} else {
				err = os.Remove(full)
			}
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	written, err := InstallUser()
	if err != nil {
		t.Fatal(err)
	}
	if len(written) != len(InstallBases)*len(paths) {
		t.Fatalf("refresh wrote %d documents, want %d", len(written), len(InstallBases)*len(paths))
	}
	for _, base := range InstallBases {
		for _, file := range Files() {
			data, err := os.ReadFile(filepath.Join(root, base, Name, filepath.FromSlash(file.Path)))
			if err != nil || string(data) != file.Content {
				t.Errorf("refresh %s/%s: content mismatch or error %v", base, file.Path, err)
			}
		}
		core, err := os.ReadFile(filepath.Join(root, base, Name, "SKILL.md"))
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range paths[1:] {
			if !strings.Contains(string(core), "]("+path+")") {
				t.Errorf("core does not link to %s", path)
			}
		}
		data, err := os.ReadFile(filepath.Join(root, base, Name, "references", "recovery.md"))
		if err != nil || !strings.Contains(string(data), "next_action.command") {
			t.Errorf("following the recovery link lost the exact-command guidance: %v", err)
		}
	}
}

func TestCoreStaysShortAndKeepsSafetyRules(t *testing.T) {
	md := Markdown()
	if len(md) > 7500 {
		t.Errorf("core is %d bytes, want at most 7500; move detail into references", len(md))
	}
	for _, want := range []string{
		"Never stop, restart, or update it",
		"full\ndescription verbatim",
		"Never hand-edit fixes",
		"never advances\n   on its own",
		"Review questions need `axi answer`",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("core lost safety guidance %q", want)
		}
	}
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
