package steps

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

func TestAutomaticSkipReasons(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name            string
		step            pipeline.Step
		unknownProvider bool
	}{
		{"PR without provider", &PRStep{}, true},
		{"CI without provider", &CIStep{}, true},
		{"CI without PR URL", &CIStep{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, base, head := setupGitRepo(t)
			ctx := newTestContextWithDBRecords(t, nil, dir, base, head, config.Commands{})
			ctx.Env = fakeCIGH(t, "OPEN", "[]")
			if tc.unknownProvider {
				ctx.Repo.UpstreamURL = ""
			}
			outcome, err := tc.step.Execute(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !outcome.Skipped || outcome.SkipReason == "" || outcome.NeedsApproval || outcome.ExitCode != 0 {
				t.Fatalf("expected an explained skip without a failure or gate: %+v", outcome)
			}
			if !tc.unknownProvider && outcome.SkipReason != "no PR URL found" {
				t.Fatalf("skip reason = %q", outcome.SkipReason)
			}
		})
	}
}
