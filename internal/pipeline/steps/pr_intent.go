package steps

import "github.com/kunchenguid/no-mistakes/internal/pipeline"

func publicPRIntent(sctx *pipeline.StepContext) string {
	if sctx != nil && sctx.Config != nil && !sctx.Config.PR.PublishesIntent() {
		return ""
	}
	return cleanedUserIntent(sctx)
}
