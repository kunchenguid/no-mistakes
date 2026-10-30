package skill

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Exercise the same home-based installer init uses, without starting a daemon
// or touching the developer's actual skill installations.
func TestInstallPreservesManagedSkills(t *testing.T) {
	for _, layout := range []string{"individual", "parent_and_individual", "both_individual"} {
		t.Run(layout, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			home := filepath.Join(root, "home")
			external := filepath.Join(root, "registry")
			mkdirAll(t, external)
			original := "externally managed skill\n"
			skillFile := filepath.Join(external, "SKILL.md")
			if err := os.WriteFile(skillFile, []byte(original), 0o644); err != nil {
				t.Fatal(err)
			}
			git := func(args ...string) string {
				t.Helper()
				cmd := exec.Command("git", append([]string{"-C", external}, args...)...)
				cmd.Env = append(os.Environ(), "GIT_CONFIG_COUNT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+filepath.Join(root, "no-global-config"))
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("git %v: %v: %s", args, err, out)
				}
				return string(out)
			}
			git("init")
			git("add", "SKILL.md") // The index is the baseline; no identity or signing is needed.
			if layout == "parent_and_individual" {
				mkdirAll(t, filepath.Join(home, ".agents", "skills"))
				mkdirAll(t, filepath.Join(home, ".claude"))
				symlink(t, filepath.Join("..", ".agents", "skills"), filepath.Join(home, ".claude", "skills"))
				symlink(t, external, filepath.Join(home, ".agents", "skills", Name))
			} else {
				mkdirAll(t, filepath.Join(home, ".claude", "skills"))
				symlink(t, external, filepath.Join(home, ".claude", "skills", Name))
				if layout == "both_individual" {
					mkdirAll(t, filepath.Join(home, ".agents", "skills"))
					symlink(t, external, filepath.Join(home, ".agents", "skills", Name))
				}
			}
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			for i := 0; i < 2; i++ {
				report, err := InstallUserWithReport()
				if err != nil {
					t.Fatal(err)
				}
				wantManaged := 2
				if layout == "individual" {
					wantManaged = 1
				}
				if len(report.Managed) != wantManaged || len(report.Written) != 2-wantManaged {
					t.Fatalf("unexpected install report: %+v", report)
				}
				for _, rel := range report.Managed {
					if !strings.HasSuffix(rel, Name) {
						t.Errorf("managed path does not name skill directory: %q", rel)
					}
				}
				data, err := os.ReadFile(skillFile)
				if err != nil {
					t.Fatal(err)
				}
				if string(data) != original {
					t.Errorf("external skill overwritten on install %d", i+1)
				}
				if diff := git("diff", "--", "SKILL.md"); diff != "" {
					t.Errorf("external worktree dirtied: %s", diff)
				}
				for _, base := range InstallBases {
					data, err := os.ReadFile(filepath.Join(home, base, Name, "SKILL.md"))
					if err != nil {
						t.Fatal(err)
					}
					want := original
					if layout == "individual" && strings.HasPrefix(base, ".agents") {
						want = Markdown()
					}
					if string(data) != want {
						t.Errorf("wrong content via %s", base)
					}
				}
			}
		})
	}
}

func TestInstallSkipsUnreadableManagedSkills(t *testing.T) {
	for _, targetState := range []string{"empty", "missing"} {
		for _, managed := range [][]string{{".claude"}, {".agents"}, {".claude", ".agents"}} {
			t.Run(targetState+"_"+strings.Join(managed, "_"), func(t *testing.T) {
				root := t.TempDir()
				target := filepath.Join(root, "registry")
				if targetState == "empty" {
					mkdirAll(t, target)
				}
				var wantManaged, wantWritten []string
				for _, agent := range []string{".claude", ".agents"} {
					rel := filepath.Join(agent, "skills", Name)
					if slices.Contains(managed, agent) {
						mkdirAll(t, filepath.Dir(filepath.Join(root, rel)))
						symlink(t, target, filepath.Join(root, rel))
						wantManaged = append(wantManaged, rel)
					} else {
						mkdirAll(t, filepath.Join(root, rel))
						if err := os.WriteFile(filepath.Join(root, rel, "SKILL.md"), []byte("stale"), 0o644); err != nil {
							t.Fatal(err)
						}
						wantWritten = append(wantWritten, filepath.Join(rel, "SKILL.md"))
					}
				}
				for i := 0; i < 2; i++ {
					report, err := InstallWithReport(root)
					if err != nil {
						t.Fatal(err)
					}
					if !slices.Equal(report.Managed, wantManaged) || !slices.Equal(report.Written, wantWritten) {
						t.Fatalf("unexpected report: %+v", report)
					}
					for _, rel := range wantManaged {
						if got, err := os.Readlink(filepath.Join(root, rel)); err != nil || got != target {
							t.Fatalf("managed link was changed: target = %q, error = %v", got, err)
						}
					}
					for _, rel := range wantWritten {
						data, err := os.ReadFile(filepath.Join(root, rel))
						if err != nil || string(data) != Markdown() {
							t.Fatalf("independent copy not refreshed at %s: %v", rel, err)
						}
					}
					if targetState == "missing" {
						if _, err := os.Lstat(target); !os.IsNotExist(err) {
							t.Fatalf("installer changed missing target: %v", err)
						}
					} else if entries, err := os.ReadDir(target); err != nil || len(entries) != 0 {
						t.Fatalf("installer changed empty target: entries = %v, error = %v", entries, err)
					}
				}
			})
		}
	}
}
