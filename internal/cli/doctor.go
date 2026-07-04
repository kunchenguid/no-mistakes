package cli

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/winproc"
	"github.com/spf13/cobra"
)

// ghVersionRE extracts the dotted version from `gh --version`'s first line,
// e.g. "gh version 2.45.0 (2025-07-18 ...)" -> "2" "45" "0".
var ghVersionRE = regexp.MustCompile(`gh version (\d+)\.(\d+)\.(\d+)`)

// ghVersionPredatesChecksJSON reports whether raw (the output of
// `gh --version`) is older than gh v2.50.0, the release that added
// `gh pr checks --json` (cli/cli#9079). no-mistakes' CI monitoring falls back
// to the statusCheckRollup path on older gh either way, so this is purely an
// informational doctor notice, never a failure. An unparseable version
// string is treated as current (predates=false) so doctor never warns on a
// gh variant it doesn't understand.
func ghVersionPredatesChecksJSON(raw string) (version string, predates bool) {
	m := ghVersionRE.FindStringSubmatch(raw)
	if m == nil {
		return "", false
	}
	version = fmt.Sprintf("%s.%s.%s", m[1], m[2], m[3])
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	if major > 2 || (major == 2 && minor >= 50) {
		return version, false
	}
	return version, true
}

func newDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check system health and dependencies",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return trackCommandStatus("doctor", func() (string, error) {
				w := cmd.OutOrStdout()
				allOK := true

				ok := func(label, detail string) {
					fmt.Fprintf(w, "  %s %s  %s\n", sGreen.Render("✓"), sDim.Render(label), detail)
				}
				warn := func(label, detail string) {
					fmt.Fprintf(w, "  %s %s  %s\n", sYellow.Render("–"), sDim.Render(label), detail)
				}
				fail := func(label, detail string) {
					fmt.Fprintf(w, "  %s %s  %s\n", sRed.Render("✗"), sDim.Render(label), detail)
				}

				fmt.Fprintf(w, "  %s\n", sCyan.Render("System"))

				if _, err := exec.LookPath("git"); err != nil {
					fail("git           ", "not found")
					allOK = false
				} else {
					gitCmd := exec.Command("git", "--version")
					winproc.Harden(gitCmd)
					out, err := gitCmd.Output()
					if err != nil {
						fail("git           ", fmt.Sprintf("error (%v)", err))
						allOK = false
					} else {
						ok("git           ", strings.TrimSpace(string(out)))
					}
				}

				if _, err := exec.LookPath("gh"); err != nil {
					warn("gh            ", "not found "+sDim.Render("(optional, needed for PR/CI)"))
				} else {
					ok("gh            ", "ok")
					ghCmd := exec.Command("gh", "--version")
					winproc.Harden(ghCmd)
					if out, verr := ghCmd.Output(); verr == nil {
						if version, predates := ghVersionPredatesChecksJSON(string(out)); predates {
							warn("gh version    ", fmt.Sprintf(
								"%s: 'pr checks --json' unavailable (added in 2.50.0); CI monitoring will use the statusCheckRollup fallback",
								version))
						}
					}
				}

				if _, err := exec.LookPath("az"); err != nil {
					warn("az            ", "not found "+sDim.Render("(optional, needed for Azure DevOps PR/CI)"))
				} else {
					ok("az            ", "ok")
				}

				p, err := paths.New()
				if err != nil {
					fail("data directory", fmt.Sprintf("error resolving paths (%v)", err))
					allOK = false
				} else if _, err := os.Stat(p.Root()); os.IsNotExist(err) {
					fail("data directory", fmt.Sprintf("not found (%s)", p.Root()))
					allOK = false
				} else {
					ok("data directory", p.Root())
				}

				if p != nil {
					if _, err := os.Stat(p.DB()); os.IsNotExist(err) {
						warn("database      ", "not found "+sDim.Render("(will be created on first use)"))
					} else {
						d, err := db.Open(p.DB())
						if err != nil {
							fail("database      ", fmt.Sprintf("error (%v)", err))
							allOK = false
						} else {
							d.Close()
							ok("database      ", "ok")
						}
					}
				}

				if p != nil {
					alive, _ := daemon.IsRunning(p)
					if alive {
						ok("daemon        ", "running")
					} else {
						warn("daemon        ", "stopped")
					}
				}

				agents := []struct {
					name   string
					binary string
				}{
					{"claude", "claude"},
					{"codex", "codex"},
					{"rovodev", "acli"},
					{"opencode", "opencode"},
					{"pi", "pi"},
					{"copilot", "copilot"},
					{"acpx", "acpx"},
				}
				fmt.Fprintln(w)
				fmt.Fprintf(w, "  %s\n", sCyan.Render("Agents"))
				for _, a := range agents {
					label := fmt.Sprintf("%-14s", a.name)
					if path, err := exec.LookPath(a.binary); err != nil {
						warn(label, "not found")
					} else {
						ok(label, path)
					}
				}

				if p == nil {
					fail("gate validation", "unavailable: data directory could not be resolved")
					allOK = false
				} else {
					globalCfg, err := config.LoadGlobal(p.ConfigFile())
					if err != nil {
						fail("gate validation", fmt.Sprintf("unavailable: load config (%v)", err))
						allOK = false
					} else {
						cfg := config.Merge(globalCfg, &config.RepoConfig{})
						if err := cfg.ResolveAgent(cmd.Context(), exec.LookPath); err != nil {
							fail("gate validation", err.Error())
							allOK = false
						} else {
							ok("gate validation", fmt.Sprintf("%s is runnable", cfg.Agent))
						}
					}
				}

				if !allOK {
					fmt.Fprintln(w)
					fmt.Fprintf(w, "  %s\n", sRed.Render("some checks failed"))
					return "error", nil
				}

				return "success", nil
			})
		},
	}
}
