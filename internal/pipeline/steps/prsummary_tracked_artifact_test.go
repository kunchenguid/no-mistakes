package steps

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestPRStep_TrackedGitLabArtifactsUseImmutableSourceLinks(t *testing.T) {
	t.Parallel()
	for _, absolute := range []bool{false, true} {
		t.Run(fmt.Sprintf("absolute=%v", absolute), func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, _ := setupGitRepo(t)
			if err := os.Mkdir(filepath.Join(dir, "artifacts"), 0o755); err != nil {
				t.Fatal(err)
			}
			name := "artifacts/server log.txt"
			if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(name)), []byte("GET /health 200\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, dir, "add", "artifacts")
			gitCmd(t, dir, "commit", "-m", "add tracked transcript")
			head := gitCmd(t, dir, "rev-parse", "HEAD")
			sctx := newTestContextWithDBRecords(t, prDraftAgent(), dir, baseSHA, head, config.Commands{})
			sctx.Repo.UpstreamURL = "git@gitlab-alias:group/sub/widgets.git"
			sctx.ForgeContext = &forgecontext.Context{Provider: scm.ProviderGitLab, Host: "gitlab.example"}
			artifactPath := name
			if absolute {
				artifactPath = filepath.Join(dir, filepath.FromSlash(name))
			}
			findings := fmt.Sprintf(`{"findings":[],"summary":"","testing_summary":"Tracked transcript collected.","artifacts":[{"kind":"log","label":"Tracked log","path":%q}]}`, artifactPath)
			insertCompletedStep(t, sctx, types.StepTest, findings, "")
			content, err := (&PRStep{}).buildPRContent(sctx, "feature", "main", baseSHA, scm.ProviderGitLab, 0)
			if err != nil {
				t.Fatal(err)
			}
			want := "https://gitlab.example/group/sub/widgets/-/blob/" + head + "/artifacts/server%20log.txt"
			if !strings.Contains(content.Body, "[Tracked log]("+want+")") {
				t.Fatalf("tracked artifact lost immutable source identity:\n%s", content.Body)
			}
			if strings.Contains(content.Body, "[Tracked log](artifacts/") || strings.Contains(content.Body, "GET /health 200") {
				t.Fatalf("code-tree artifact was treated as local evidence or left relative:\n%s", content.Body)
			}
			if refs := gitCmd(t, dir, "for-each-ref", "--format=%(refname)", "refs/heads/no-mistakes/evidence"); refs != "" {
				t.Fatalf("tracked file was copied to an evidence branch without opt-in: %s", refs)
			}
		})
	}
}

func TestTrackedArtifactTargetsUseForgeRawAndBlobRoutes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		remote, web string
		provider    scm.Provider
	}{
		{"https://user:fixture-secret@gitlab.example:8443/group/sub/widgets.git?token=fixture", "https://gitlab.example:8443/group/sub/widgets/-/", scm.ProviderGitLab},
		{"https://github.com/example/widgets.git", "https://github.com/example/widgets/", scm.ProviderGitHub},
	} {
		results, rounds := testStepWithArtifacts(`{"kind":"log","label":"Tracked log","path":"artifacts/server.log"}`)
		body := BuildTestingSummaryForPRWithProvider(results, rounds, tc.remote, testPipelineHeadSHA, t.TempDir(), "", nil, tc.provider)
		want := tc.web + "blob/" + testPipelineHeadSHA + "/artifacts/server.log"
		if !strings.Contains(body, want) || strings.Contains(body, "fixture-secret") || strings.Contains(body, "token=fixture") {
			t.Fatalf("tracked source URL or credential stripping failed:\n%s", body)
		}
	}
}
