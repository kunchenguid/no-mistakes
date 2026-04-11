package cli

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/spf13/cobra"
)

func newDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check system health and dependencies",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			w := cmd.OutOrStdout()
			allOK := true

			// 1. Check git.
			if _, err := exec.LookPath("git"); err != nil {
				fmt.Fprintf(w, "  git:            not found\n")
				allOK = false
			} else {
				out, err := exec.Command("git", "--version").Output()
				if err != nil {
					fmt.Fprintf(w, "  git:            error (%v)\n", err)
					allOK = false
				} else {
					fmt.Fprintf(w, "  git:            %s", out)
				}
			}

			// 2. Check gh CLI (optional but useful for PR/babysit steps).
			if _, err := exec.LookPath("gh"); err != nil {
				fmt.Fprintf(w, "  gh:             not found (optional, needed for PR/babysit)\n")
			} else {
				fmt.Fprintf(w, "  gh:             ok\n")
			}

			// 3. Check data directory.
			p, err := paths.New()
			if err != nil {
				fmt.Fprintf(w, "  data directory: error resolving paths (%v)\n", err)
				allOK = false
			} else if _, err := os.Stat(p.Root()); os.IsNotExist(err) {
				fmt.Fprintf(w, "  data directory: not found (%s)\n", p.Root())
				allOK = false
			} else {
				fmt.Fprintf(w, "  data directory: %s\n", p.Root())
			}

			// 4. Check database.
			if p != nil {
				if _, err := os.Stat(p.DB()); os.IsNotExist(err) {
					fmt.Fprintf(w, "  database:       not found (will be created on first use)\n")
				} else {
					d, err := db.Open(p.DB())
					if err != nil {
						fmt.Fprintf(w, "  database:       error (%v)\n", err)
						allOK = false
					} else {
						d.Close()
						fmt.Fprintf(w, "  database:       ok\n")
					}
				}
			}

			// 5. Check daemon status.
			if p != nil {
				alive, _ := daemon.IsRunning(p)
				if alive {
					fmt.Fprintf(w, "  daemon:         running\n")
				} else {
					fmt.Fprintf(w, "  daemon:         stopped\n")
				}
			}

			// 6. Check agent binaries.
			agents := []struct {
				name   string
				binary string
			}{
				{"claude", "claude"},
				{"codex", "codex"},
				{"rovodev", "acli"},
				{"opencode", "opencode"},
			}
			fmt.Fprintf(w, "\nagent binaries:\n")
			for _, a := range agents {
				if path, err := exec.LookPath(a.binary); err != nil {
					fmt.Fprintf(w, "  %-14s  not found\n", a.name+":")
				} else {
					fmt.Fprintf(w, "  %-14s  %s\n", a.name+":", path)
				}
			}

			if !allOK {
				fmt.Fprintf(w, "\nsome checks failed\n")
			}

			return nil
		},
	}
}
