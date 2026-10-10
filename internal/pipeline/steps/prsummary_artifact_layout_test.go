package steps

import (
	"context"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// A Markdown embed directly after a list-item line is its lazy continuation.
// Each artifact must start its own block when it is not another list item.
func TestPRTestingSummary_AdjacentMediaStartOutsideThePreviousListItem(t *testing.T) {
	t.Parallel()
	for _, provider := range []scm.Provider{scm.ProviderGitHub, scm.ProviderGitLab} {
		t.Run(string(provider), func(t *testing.T) {
			t.Parallel()
			remote := "https://github.com/example/widgets.git"
			attachments := map[string]string{"a.png": "https://github.com/user-attachments/assets/a", "b.mp4": "https://github.com/user-attachments/assets/b"}
			if provider == scm.ProviderGitLab {
				remote = "https://gitlab.com/group/sub/widgets.git"
				attachments = map[string]string{"a.png": "/uploads/a/a.png", "b.mp4": "/uploads/b/b.mp4"}
			}
			results, rounds := testStepWithArtifacts(`{"kind":"image","label":"First image","path":"a.png"},{"kind":"video","label":"Second recording","path":"b.mp4"}`)
			sctx := &pipeline.StepContext{Ctx: context.Background(), Repo: &db.Repo{UpstreamURL: remote}, Run: &db.Run{HeadSHA: testPipelineHeadSHA}, WorkDir: t.TempDir()}
			body := buildPRTestingSummary(sctx, results, rounds, nil, provider, attachments)
			video := attachments["b.mp4"]
			if provider == scm.ProviderGitLab {
				video = "![Second recording](" + video + ")"
			}
			if !strings.Contains(body, "a.png)\n\n"+video+"\n") {
				t.Fatalf("second embed can become a continuation of the first evidence bullet:\n%s", body)
			}
			if strings.Count(body, "- Evidence:") != 2 {
				t.Fatalf("media lost its own evidence link:\n%s", body)
			}
		})
	}
}
