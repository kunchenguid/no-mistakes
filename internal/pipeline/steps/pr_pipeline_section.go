package steps

import (
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func pipelineSectionFrom(sctx *pipeline.StepContext, provider scm.Provider, owned bool, steps []*db.StepResult, rounds map[string][]*db.StepRound, evidence *evidenceLinks, attachments map[string]string) (pipelineMD, riskLine, testingMD string) {
	policy := pipelineAttestationPolicy{}
	if sctx.Config != nil {
		policy.AllowTestCommandOverride = strings.TrimSpace(sctx.Config.Test.AllowApproveOverFailure)
	}
	pipelineMD, riskLine = buildPipelineSummaryFor(steps, rounds, sctx.Run.HeadSHA, provider, policy)
	if conversationMD := buildReviewConversationSection(sctx); conversationMD != "" && pipelineMD != "" {
		pipelineMD += "\n\n" + conversationMD
	}
	if owned && provider == scm.ProviderBitbucket && pipelineMD != "" {
		pipelineMD += "\n\n```text\n" + buildPipelineAttestationWithPolicy(steps, rounds, sctx.Run.HeadSHA, policy) + "\n```"
	}
	testingMD = buildPRTestingSummary(steps, rounds, sctx.Repo.UpstreamURL, sctx.Run.HeadSHA, sctx.WorkDir, testEvidenceDir(sctx), evidence, provider, attachments)
	return pipelineMD, riskLine, testingMD
}
