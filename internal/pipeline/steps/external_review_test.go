package steps

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type reviewProvider struct {
	scm.Host
	provider scm.Provider
}

func (h reviewProvider) Provider() scm.Provider { return h.provider }

func TestRequiredReviewDoesNotLetPipelineRegistrationHidePassingGitLabReview(t *testing.T) {
	for _, tc := range []struct {
		name  string
		noCI  bool
		want  int
		waits bool
	}{
		{name: "trusted no_ci", noCI: true, want: 1},
		{name: "pipeline expected", want: 2, waits: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sctx := &pipeline.StepContext{Config: &config.Config{
				NoCI: tc.noCI, CI: config.CI{RequiredReviews: []string{"coderabbit"}},
			}}
			checks := []scm.Check{
				{Name: "External review: coderabbit", ProviderID: "external-review:coderabbit", ExternalReview: "coderabbit", HeadSHA: "head", Bucket: scm.CheckBucketPass},
				{Name: "Pipeline registration", ProviderID: "gitlab:pipeline-registration", Bucket: scm.CheckBucketPending},
			}
			got, err := requiredReviewChecks(sctx, reviewProvider{provider: scm.ProviderGitLab}, &scm.PR{HeadSHA: "head"}, checks)
			if err != nil {
				t.Fatalf("requiredReviewChecks() error = %v", err)
			}
			if len(got) != tc.want {
				t.Fatalf("checks=%+v, want %d checks", got, tc.want)
			}
			var reviewPass, waiting bool
			for _, check := range got {
				if check.Name == "External review: coderabbit" && check.Bucket == scm.CheckBucketPass {
					reviewPass = true
				}
				if check.Name == "Pipeline registration" {
					waiting = true
				}
			}
			if !reviewPass {
				t.Fatalf("checks=%+v, want passing CodeRabbit review retained", got)
			}
			if waiting != tc.waits {
				t.Fatalf("checks=%+v, pipeline-registration wait present=%v, want %v", got, waiting, tc.waits)
			}
		})
	}
}

func TestRequiredReviewUnsupportedProvidersParkExplicitly(t *testing.T) {
	for _, provider := range []scm.Provider{
		scm.ProviderGitLab, scm.ProviderBitbucket, scm.ProviderGitea, scm.ProviderForgejo,
		scm.ProviderOrigin, scm.PluginProvider("example"),
	} {
		t.Run(string(provider), func(t *testing.T) {
			sctx := &pipeline.StepContext{Config: &config.Config{CI: config.CI{RequiredReviews: []string{"coderabbit"}}}}
			host := reviewProvider{provider: provider}
			checks, err := requiredReviewChecks(sctx, host, &scm.PR{HeadSHA: "current-head"}, nil)
			if err != nil || len(checks) != 1 || checks[0].ExternalReview != "coderabbit" || checks[0].Bucket != scm.CheckBucketFail || checks[0].State != "UNSUPPORTED ON "+strings.ToUpper(string(provider)) {
				t.Fatalf("checks=%+v err=%v", checks, err)
			}
			findings := ciObservationFindings(ciIssues{
				provider: provider, requiredReviews: []string{"coderabbit"}, checks: checks,
				failing: []string{checks[0].Name},
			})
			data, marshalErr := json.Marshal(findings)
			parsed, err := types.ParseFindingsJSON(string(data))
			if marshalErr != nil || err != nil || len(parsed.Items) != 1 || parsed.Items[0].Action != types.ActionAskUser || parsed.Items[0].Category != types.FindingCategoryCIReviewBot {
				t.Fatalf("findings=%+v marshalErr=%v err=%v", parsed, marshalErr, err)
			}
		})
	}
}
