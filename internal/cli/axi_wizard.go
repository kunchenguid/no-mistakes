package cli

import (
	"fmt"

	"github.com/spf13/cobra"
	toon "github.com/toon-format/toon-go"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// wizardHistoryLimit caps the standalone-session table by default, high enough
// to cover normal history in one call per the AXI minimal-call convention.
const wizardHistoryLimit = 10

// newAxiWizardCmd shows the standalone Wizard's branch/commit suggestion
// history: routed Candidate attempts recorded under utility scopes, with no
// fabricated pipeline run/step/round rows.
func newAxiWizardCmd() *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "wizard",
		Short: "Show standalone Wizard branch/commit suggestion history",
		Long: "Lists the routed branch/commit suggestion attempts the setup Wizard\n" +
			"recorded under its standalone utility scopes. Active (in-flight) and\n" +
			"completed attempts are both shown. This history never fabricates\n" +
			"pipeline gate rows.",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return trackAxiSurface("axi-wizard", "/axi/wizard", nil, func() error {
				return runAxiWizard(cmd, limit)
			})
		},
	}
	cmd.Flags().IntVar(&limit, "limit", wizardHistoryLimit, "max Wizard sessions to show")
	return cmd
}

// wizardAttemptRow is one routed suggestion attempt in the standalone history.
type wizardAttemptRow struct {
	Session  string `toon:"session"`
	Purpose  string `toon:"purpose"`
	Profile  string `toon:"profile"`
	Runner   string `toon:"runner"`
	Model    string `toon:"model"`
	Effort   string `toon:"effort"`
	Outcome  string `toon:"outcome"`
	Duration int64  `toon:"duration_ms"`
}

func runAxiWizard(cmd *cobra.Command, limit int) error {
	env, err := openAxiEnv(false)
	if err != nil {
		return emitError(cmd, 1, err.Error(), repoInitHelp(err)...)
	}
	defer env.close()

	scopes, err := env.d.GetRecentUtilityScopes(types.UtilityScopeWizard, limit)
	if err != nil {
		return emitError(cmd, 1, fmt.Sprintf("list wizard sessions: %v", err))
	}

	rows := []wizardAttemptRow{}
	active := 0
	for _, scope := range scopes {
		attempts, err := env.d.GetInvocationAttemptsByUtilityScope(scope.ID)
		if err != nil {
			return emitError(cmd, 1, fmt.Sprintf("load wizard attempts: %v", err))
		}
		for _, a := range attempts {
			outcome := "active"
			var duration int64
			if a.Terminal != nil {
				outcome = string(a.Terminal.Outcome)
				duration = a.Terminal.DurationMS
			} else {
				active++
			}
			profile := a.Start.Candidate.Profile
			if a.Start.CandidateKey == types.LegacyCandidateKey {
				profile = "legacy"
			}
			rows = append(rows, wizardAttemptRow{
				Session:  scope.ID,
				Purpose:  string(a.Start.Purpose),
				Profile:  profile,
				Runner:   string(a.Start.Candidate.Runner),
				Model:    a.Start.Candidate.Model,
				Effort:   string(a.Start.Candidate.Effort),
				Outcome:  outcome,
				Duration: duration,
			})
		}
	}

	fields := []toon.Field{
		{Key: "surface", Value: "wizard-suggestion-history"},
		{Key: "sessions", Value: len(scopes)},
		{Key: "active_attempts", Value: active},
	}
	if len(rows) == 0 {
		fields = append(fields, toon.Field{Key: "attempts", Value: "no Wizard suggestion attempts recorded yet"})
	} else {
		fields = append(fields, toon.Field{Key: "attempts", Value: rows})
	}
	emitDoc(cmd, fields...)
	return nil
}
