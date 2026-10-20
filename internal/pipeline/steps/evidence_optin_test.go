package steps

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps/internal/stepstest"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const gitLabEvidencePublicationYAML = "providers:\n  gitlab:\n    evidence_publication: true\n"

func strictEvidenceConfig(t *testing.T, globalYAML, repoYAML string) *config.Config {
	t.Helper()
	if globalYAML == "" {
		globalYAML = "{}\n"
	}
	if repoYAML == "" {
		repoYAML = "{}\n"
	}
	global, err := config.LoadGlobalFromBytes([]byte(globalYAML))
	if err != nil {
		t.Fatal(err)
	}
	repo, err := config.LoadRepoFromBytes([]byte(repoYAML))
	if err != nil {
		t.Fatal(err)
	}
	return config.Merge(global, repo)
}

func TestGitLabEvidence_ExistingSettingsDoNotEnablePublication(t *testing.T) {
	t.Parallel()
	for _, settings := range []string{"{}\n", "test:\n  evidence:\n    attach_media: true\n", "test:\n  evidence:\n    store_in_repo: true\n"} {
		t.Run(strings.ReplaceAll(settings, "\n", " "), func(t *testing.T) {
			t.Parallel()
			sctx, remote := newGitLabEvidencePublishContext(t, "feature/evidence", "https://gitlab.example/group/widgets.git", "gitlab.example")
			sctx.Config = strictEvidenceConfig(t, settings, "")
			sctx.Agent = prDraftAgent()
			image := writeEvidenceFile(t, sctx.EvidenceDir, "checkout.png", []byte("png"))
			insertCompletedStep(t, sctx, types.StepTest, screenshotFindings(image), "")
			uploader := &stubMediaUploader{t: t, urls: map[string]string{"checkout.png": "/uploads/abc/checkout.png"}}
			content, err := (&PRStep{mediaUploader: uploader}).buildPRContent(sctx, "feature/evidence", "main", sctx.Run.BaseSHA, scm.ProviderGitLab, 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(uploader.calls) != 0 || !strings.Contains(content.Body, "local file:") || strings.Contains(content.Body, "/uploads/") {
				t.Fatalf("existing settings acquired GitLab upload semantics; uploads=%v body:\n%s", uploader.calls, content.Body)
			}
			if refs := gitCmd(t, remote, "for-each-ref", "--format=%(refname)"); refs != "refs/heads/main" {
				t.Fatalf("existing settings published a GitLab evidence branch: %s", refs)
			}
		})
	}
}

func TestGitLabEvidence_ProviderOptInAndRepositoryOverride(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, global, repo string
		wantUpload         bool
	}{
		{"global opt-in", gitLabEvidencePublicationYAML, "", true},
		{"repository opt-in", "", gitLabEvidencePublicationYAML, true},
		{"repository opt-out", gitLabEvidencePublicationYAML, "providers:\n  gitlab:\n    evidence_publication: false\n", false},
		{"media opt-out", gitLabEvidencePublicationYAML, "test:\n  evidence:\n    attach_media: false\n", false},
		{"branch implies media after opt-in", gitLabEvidencePublicationYAML, "test:\n  evidence:\n    attach_media: false\n    store_in_repo: true\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sctx, remote := newGitLabEvidencePublishContext(t, "feature/evidence", "https://gitlab.example/group/widgets.git", "gitlab.example")
			sctx.Config = strictEvidenceConfig(t, tc.global, tc.repo)
			sctx.Agent = prDraftAgent()
			image := writeEvidenceFile(t, sctx.EvidenceDir, "checkout.png", []byte("png"))
			insertCompletedStep(t, sctx, types.StepTest, screenshotFindings(image), "")
			uploader := &stubMediaUploader{t: t, urls: map[string]string{"checkout.png": "/uploads/abc/checkout.png"}}
			content, err := (&PRStep{mediaUploader: uploader}).buildPRContent(sctx, "feature/evidence", "main", sctx.Run.BaseSHA, scm.ProviderGitLab, 0)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantUpload {
				if len(uploader.calls) != 1 || !strings.Contains(content.Body, "![Checkout screenshot](/uploads/abc/checkout.png)") {
					t.Fatalf("explicit GitLab upload missing; uploads=%v body:\n%s", uploader.calls, content.Body)
				}
			} else if len(uploader.calls) != 0 || !strings.Contains(content.Body, "local file:") {
				t.Fatalf("GitLab opt-out ignored; uploads=%v body:\n%s", uploader.calls, content.Body)
			}
			refs := gitCmd(t, remote, "for-each-ref", "--format=%(refname)")
			if (strings.Contains(refs, "refs/heads/no-mistakes/evidence")) != sctx.Config.Test.Evidence.StoreInRepo {
				t.Fatalf("evidence branch disagrees with explicit settings: %s", refs)
			}
		})
	}
}

func TestGitLabEvidence_TrackedLinksRequireProviderOptIn(t *testing.T) {
	t.Parallel()
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			t.Parallel()
			yaml := "{}\n"
			if enabled {
				yaml = gitLabEvidencePublicationYAML
			}
			results, rounds := testStepWithArtifacts(`{"kind":"log","label":"Tracked log","path":"artifacts/server log.txt"}`)
			sctx := &pipeline.StepContext{Ctx: context.Background(), Config: strictEvidenceConfig(t, yaml, ""), Repo: &db.Repo{UpstreamURL: "https://gitlab.example/group/widgets.git"}, Run: &db.Run{HeadSHA: testPipelineHeadSHA}, WorkDir: t.TempDir()}
			body := buildPRTestingSummary(sctx, results, rounds, nil, scm.ProviderGitLab, nil)
			target := "artifacts/server log.txt"
			if enabled {
				target = "https://gitlab.example/group/widgets/-/blob/" + testPipelineHeadSHA + "/artifacts/server%20log.txt"
			}
			if !strings.Contains(body, "[Tracked log]("+target+")") {
				t.Fatalf("tracked-file contract differs from the selected mode:\n%s", body)
			}
		})
	}
}

func TestGitLabEvidence_ExtraMediaTypesStayLegacyUnlessOptedIn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, remote string
		provider     scm.Provider
		optIn        bool
	}{
		{"GitHub", "https://github.com/example/widgets.git", scm.ProviderGitHub, false},
		{"GitHub ignores GitLab opt-in", "https://github.com/example/widgets.git", scm.ProviderGitHub, true},
		{"GitLab", "https://gitlab.example/group/widgets.git", scm.ProviderGitLab, false},
		{"GitLab opted in", "https://gitlab.example/group/widgets.git", scm.ProviderGitLab, true},
		{"Gitea", "https://gitea.example/group/widgets.git", scm.ProviderGitea, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, base, head := setupGitRepo(t)
			sctx := newTestContextWithDBRecords(t, prDraftAgent(), dir, base, head, config.Commands{})
			yaml := "{}\n"
			if tc.optIn {
				yaml = gitLabEvidencePublicationYAML
			}
			sctx.Config = strictEvidenceConfig(t, yaml, "")
			sctx.Repo.UpstreamURL = tc.remote
			var artifacts []string
			uploader := &stubMediaUploader{t: t, urls: map[string]string{}}
			for _, ext := range []string{"bmp", "avif", "m4v", "ogv"} {
				name := "artifact." + ext
				p := writeEvidenceFile(t, sctx.EvidenceDir, name, []byte("plain text in "+ext))
				artifacts = append(artifacts, fmt.Sprintf(`{"label":%q,"path":%q}`, ext, p))
				uploader.urls[name] = "/uploads/abc/" + name
			}
			insertCompletedStep(t, sctx, types.StepTest, `{"findings":[],"artifacts":[`+strings.Join(artifacts, ",")+`]}`, "")
			content, err := (&PRStep{mediaUploader: uploader}).buildPRContent(sctx, "feature", "main", base, tc.provider, 0)
			if err != nil {
				t.Fatal(err)
			}
			optedIn := tc.provider == scm.ProviderGitLab && tc.optIn
			for _, ext := range []string{"bmp", "avif", "m4v", "ogv"} {
				if strings.Contains(content.Body, "plain text in "+ext) == optedIn {
					t.Fatalf("extension changed the unopted text-file rendering:\n%s", content.Body)
				}
				if optedIn && !strings.Contains(content.Body, "!["+ext+"](/uploads/abc/artifact."+ext+")") {
					t.Fatalf("opted-in media was not embedded:\n%s", content.Body)
				}
			}
			if !optedIn && len(uploader.calls) != 0 {
				t.Fatalf("legacy files uploaded without provider opt-in: %v", uploader.calls)
			}
		})
	}
}

func TestGitLabEvidence_UnoptedSummaryPreservesLegacyBlockBoundary(t *testing.T) {
	t.Parallel()
	results, rounds := testStepWithArtifacts(`{"label":"Notes","url":"https://example.test/notes"},{"kind":"image","label":"Image","url":"https://example.test/shot.png"}`)
	body := BuildTestingSummary(results, rounds)
	if !strings.Contains(body, "[Notes](https://example.test/notes)\n**Image**\n\n![Image](https://example.test/shot.png)") {
		t.Fatalf("unopted artifact blocks gained a new separating blank line:\n%s", body)
	}
}

func TestGitLabEvidence_UnoptedGitHubPathsKeepLegacyTargets(t *testing.T) {
	t.Parallel()
	results, rounds := testStepWithArtifacts(`{"label":"Log","path":"artifacts/server log.txt"}`)
	for _, tc := range []struct{ remote, target string }{
		{"https://github.com/example/widgets.git", "https://github.com/example/widgets/blob/" + testPipelineHeadSHA + "/artifacts/server log.txt"},
		{"git@github-alias:example/widgets.git", "artifacts/server log.txt"},
		{"ssh://git@github.com:2222/example/widgets.git", "artifacts/server log.txt"},
	} {
		sctx := &pipeline.StepContext{Ctx: context.Background(), Config: strictEvidenceConfig(t, gitLabEvidencePublicationYAML, ""), Repo: &db.Repo{UpstreamURL: tc.remote}, Run: &db.Run{HeadSHA: testPipelineHeadSHA}, WorkDir: t.TempDir()}
		body := buildPRTestingSummary(sctx, results, rounds, nil, scm.ProviderGitHub, nil)
		if !strings.Contains(body, "[Log]("+tc.target+")") {
			t.Fatalf("legacy GitHub path target changed for %s:\n%s", tc.remote, body)
		}
	}
}

func TestGitLabEvidence_UnoptedSummaryNeedsNoForgeCLI(t *testing.T) {
	binDir := stepstest.FakeCLIBinDir(t)
	stepstest.LinkFakeCLI(t, binDir, "ssh")
	logFile := filepath.Join(t.TempDir(), "forge-probes.log")
	t.Setenv("PATH", binDir)
	t.Setenv("FAKE_CLI_LOG", logFile)
	t.Setenv("FAKE_CLI_MODE", "record-success")
	results, rounds := testStepWithArtifacts(`{"label":"Log","path":"server.log"}`)
	body := BuildTestingSummaryForPR(results, rounds, "git@unopted-evidence-never-probe:group/widgets.git", testPipelineHeadSHA, t.TempDir(), "", nil)
	if !strings.Contains(body, "[Log](server.log)") {
		t.Fatalf("legacy repository path was not retained:\n%s", body)
	}
	if data, err := os.ReadFile(logFile); !os.IsNotExist(err) {
		t.Fatalf("unopted rendering invoked forge detection: %s (read error: %v)", data, err)
	}
}
