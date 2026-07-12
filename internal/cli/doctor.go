package cli

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/kunchenguid/no-mistakes/internal/winproc"
	"github.com/spf13/cobra"
)

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

				fmt.Fprintln(w)
				fmt.Fprintf(w, "  %s\n", sCyan.Render("Routing"))

				globalCfg := &config.GlobalConfig{Routing: config.DefaultRoutingConfig()}
				routingResolved := p != nil
				if p != nil {
					if gc, gerr := config.LoadGlobal(p.ConfigFile()); gerr != nil {
						fail("config        ", gerr.Error())
						allOK = false
						routingResolved = false
					} else {
						globalCfg = gc
					}
				}
				effectiveRepoCfg := &config.RepoConfig{}
				if workDir, rerr := git.FindGitRoot("."); rerr == nil {
					defaultBranch := git.DefaultBranch(cmd.Context(), workDir, "origin")
					trustedRepoCfg, terr := loadPinnedTrustedRepoConfig(cmd.Context(), workDir, defaultBranch)
					if terr != nil {
						fail("repo config   ", terr.Error())
						allOK = false
						routingResolved = false
					} else {
						effectiveRepoCfg = config.EffectiveRepoConfig(&config.RepoConfig{}, trustedRepoCfg)
					}
				}
				effectiveCfg := config.Merge(globalCfg, effectiveRepoCfg)
				routing := effectiveCfg.Routing
				routingValid := true
				if err := routing.Validate(); err != nil {
					fail("contract      ", err.Error())
					allOK = false
					routingValid = false
				} else {
					ok("contract      ", fmt.Sprintf("valid: %d profiles, %d purposes routed", len(routing.Profiles), len(routing.Routes)))
				}
				availableRunners := make(map[types.Runner]bool, len(routing.Runners))
				for _, name := range sortedDoctorRunners(routing.Runners) {
					spec := routing.Runners[name]
					label := fmt.Sprintf("%-14s", name)
					if path, lerr := exec.LookPath(spec.Executable); lerr != nil {
						warn(label, fmt.Sprintf("%s not found (%s provider)", spec.Executable, spec.FailureDomain))
					} else {
						availableRunners[name] = true
						ok(label, fmt.Sprintf("%s (%s provider)", path, spec.FailureDomain))
					}
				}
				if routingValid {
					for _, profile := range unavailableDoctorProfiles(routing, availableRunners) {
						fail("profile       ", fmt.Sprintf("%s has no available candidates", profile))
						allOK = false
					}
				}

				if p == nil {
					fail("gate validation", "unavailable: data directory could not be resolved")
					allOK = false
				} else if !routingResolved {
					fail("gate validation", "unavailable: effective routing configuration could not be resolved")
					allOK = false
				} else if err := effectiveCfg.ValidateRunnable(exec.LookPath); err != nil {
					fail("gate validation", err.Error())
					allOK = false
				} else {
					ok("gate validation", "every routed profile has a runnable candidate")
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

// sortedDoctorRunners returns the routing runner names in a stable order so the
// doctor report is deterministic.
func sortedDoctorRunners(m map[types.Runner]config.RunnerSpec) []types.Runner {
	out := make([]types.Runner, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// unavailableDoctorProfiles returns every Profile referenced by an effective
// Route whose Candidates all resolve to unavailable runner executables.
func unavailableDoctorProfiles(routing config.RoutingConfig, available map[types.Runner]bool) []config.ProfileName {
	required := make(map[config.ProfileName]struct{})
	for _, route := range routing.Routes {
		for _, profile := range route {
			required[profile] = struct{}{}
		}
	}
	names := make([]config.ProfileName, 0, len(required))
	for profile := range required {
		names = append(names, profile)
	}
	sort.Slice(names, func(i, j int) bool { return names[i] < names[j] })

	unavailable := names[:0]
	for _, name := range names {
		hasAvailableCandidate := false
		for _, candidate := range routing.Profiles[name].Candidates {
			if available[candidate.Runner] {
				hasAvailableCandidate = true
				break
			}
		}
		if !hasAvailableCandidate {
			unavailable = append(unavailable, name)
		}
	}
	return unavailable
}
