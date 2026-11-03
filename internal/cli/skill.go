package cli

import (
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/skill"
	"github.com/spf13/cobra"
)

func newSkillCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skill",
		Short: "Manage the /no-mistakes agent skill",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "install",
		Short: "Install or refresh the user-level agent skill",
		Long: "Install or refresh /no-mistakes in ~/.claude/skills and ~/.agents/skills.\n" +
			"Works outside a Git repository, without registering a gate or starting the daemon.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			written, err := installUserSkill()
			if err != nil {
				return err
			}
			for _, path := range written {
				fmt.Fprintf(cmd.OutOrStdout(), "Installed ~/%s\n", path)
			}
			return nil
		},
	})
	return cmd
}

// installUserSkill is shared by init's best-effort install and skill install.
func installUserSkill() ([]string, error) {
	written, err := skill.InstallUser()
	if err != nil {
		return written, fmt.Errorf("install user skill: %w", err)
	}
	return written, nil
}
