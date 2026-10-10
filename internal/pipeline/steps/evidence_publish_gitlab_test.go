package steps

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/reviewqa"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func newGitLabEvidencePublishContext(t *testing.T, branch, upstream, host string) (*pipeline.StepContext, string) {
	t.Helper()
	sctx, remote := newEvidencePublishContext(t, branch)
	gitCmd(t, sctx.WorkDir, "config", "url."+remote+".insteadOf", upstream)
	gitCmd(t, sctx.WorkDir, "remote", "set-url", "origin", upstream)
	sctx.Repo.UpstreamURL = upstream
	sctx.ForgeContext = &forgecontext.Context{Provider: scm.ProviderGitLab, Host: host}
	return sctx, remote
}

func TestPublishRunEvidence_GitLabPublishesPinnedNestedProjectLinks(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, remote, host, web string }{
		{"private HTTPS with credentials", "https://user:fixture-secret@gitlab.private.example:8443/group/sub/widgets.git?token=fixture-token", "gitlab.private.example", "https://gitlab.private.example:8443/group/sub/widgets"},
		{"resolved SSH alias", "git@gitlab-alias:group/sub/widgets.git", "gitlab.private.example", "https://gitlab.private.example/group/sub/widgets"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sctx, remote := newGitLabEvidencePublishContext(t, "feature/live-evidence", tc.remote, tc.host)
			writeRunEvidence(t, sctx, map[string]string{
				"test log #1 100%.txt":            "GET /health -> 200\n",
				"checkout.png":                    "\x89PNG binary",
				reviewqa.DirName + "/private.txt": "private conversation must not publish",
			})
			headBefore := gitCmd(t, sctx.WorkDir, "rev-parse", "HEAD")
			statusBefore := gitCmd(t, sctx.WorkDir, "status", "--porcelain")
			mainBefore := gitCmd(t, remote, "rev-parse", "main")
			links := publishRunEvidence(sctx)
			if links == nil {
				t.Fatal("GitLab evidence branch was not published")
			}
			tip := gitCmd(t, remote, "rev-parse", "refs/heads/no-mistakes/evidence")
			if parents := gitCmd(t, remote, "show", "-s", "--format=%P", tip); parents != "" {
				t.Fatalf("evidence branch is not orphaned: parents=%s", parents)
			}
			if got := gitCmd(t, remote, "show", tip+":.no-mistakes/evidence/feature/live-evidence/test log #1 100%.txt"); got != "GET /health -> 200" {
				t.Fatalf("remote log bytes = %q", got)
			}
			if tree := gitCmd(t, remote, "ls-tree", "-r", "--name-only", tip); strings.Contains(tree, reviewqa.DirName) {
				t.Fatalf("private review conversation was published: %s", tree)
			}
			logPath := filepath.Join(sctx.EvidenceDir, "test log #1 100%.txt")
			wantBlob := tc.web + "/-/blob/" + tip + "/.no-mistakes/evidence/feature/live-evidence/test%20log%20%231%20100%25.txt"
			if got := links.target(logPath, false); got != wantBlob {
				t.Fatalf("GitLab text link=%q, want %q", got, wantBlob)
			}
			wantRaw := tc.web + "/-/raw/" + tip + "/.no-mistakes/evidence/feature/live-evidence/checkout.png"
			if got := links.target(filepath.Join(sctx.EvidenceDir, "checkout.png"), true); got != wantRaw {
				t.Fatalf("GitLab raw image link=%q, want %q", got, wantRaw)
			}
			results, rounds := testStepWithArtifacts(fmt.Sprintf(`{"kind":"log","label":"HTTP transcript","path":%q}`, logPath))
			body := BuildTestingSummaryForPRWithProvider(results, rounds, sctx.Repo.UpstreamURL, sctx.Run.HeadSHA, sctx.WorkDir, sctx.EvidenceDir, links, scm.ProviderGitLab)
			if !strings.Contains(body, wantBlob) || !strings.Contains(body, "GET /health -> 200") || strings.Contains(body, "local file:") {
				t.Fatalf("remote text home and inline transcript not rendered:\n%s", body)
			}
			if strings.Contains(body, "fixture-secret") || strings.Contains(body, "fixture-token") {
				t.Fatalf("credential reached the published body:\n%s", body)
			}
			if gitCmd(t, sctx.WorkDir, "rev-parse", "HEAD") != headBefore || gitCmd(t, sctx.WorkDir, "status", "--porcelain") != statusBefore || gitCmd(t, remote, "rev-parse", "main") != mainBefore {
				t.Fatal("publication changed code HEAD, index/worktree, or remote main")
			}
			// An unchanged rerender links the existing immutable evidence commit.
			publishRunEvidence(sctx)
			if again := gitCmd(t, remote, "rev-parse", "refs/heads/no-mistakes/evidence"); again != tip {
				t.Fatalf("unchanged evidence advanced the remote: %s -> %s", tip, again)
			}
		})
	}
}

func TestPRStep_GitLabKeepsMediaEmbedAndCommitPinnedEvidenceLinks(t *testing.T) {
	t.Parallel()
	sctx, remote := newGitLabEvidencePublishContext(t, "feature/media", "https://gitlab.example/group/sub/widgets.git", "gitlab.example")
	sctx.Agent = prDraftAgent()
	writeRunEvidence(t, sctx, map[string]string{"checkout.png": "\x89PNG binary", "transcript.txt": "Checkout succeeded\n"})
	image := filepath.Join(sctx.EvidenceDir, "checkout.png")
	text := filepath.Join(sctx.EvidenceDir, "transcript.txt")
	findings := fmt.Sprintf(`{"findings":[],"summary":"","testing_summary":"Checkout verified.","artifacts":[{"kind":"screenshot","label":"Checkout screenshot","path":%q},{"kind":"log","label":"Checkout transcript","path":%q}]}`, image, text)
	insertCompletedStep(t, sctx, types.StepTest, findings, "")
	markdown := "![Checkout screenshot](/uploads/abc/checkout.png)"
	uploader := &stubMediaUploader{t: t, urls: map[string]string{"checkout.png": "/uploads/abc/checkout.png"}}
	content, err := (&PRStep{mediaUploader: uploader}).buildPRContent(sctx, "feature/media", "main", sctx.Run.BaseSHA, scm.ProviderGitLab, 0)
	if err != nil {
		t.Fatal(err)
	}
	tip := gitCmd(t, remote, "rev-parse", "refs/heads/no-mistakes/evidence")
	base := "https://gitlab.example/group/sub/widgets/-/blob/" + tip + "/.no-mistakes/evidence/feature/media/"
	for _, expected := range []string{markdown, base + "checkout.png", base + "transcript.txt", "Checkout succeeded"} {
		if !strings.Contains(content.Body, expected) {
			t.Fatalf("missing %q in GitLab MR body:\n%s", expected, content.Body)
		}
	}
	if len(uploader.calls) != 1 || strings.Contains(content.Body, "local file:") {
		t.Fatalf("publication did not combine media and evidence branch; calls=%v body:\n%s", uploader.calls, content.Body)
	}
}

func TestPublishRunEvidence_GitLabUnimplementedForkRoutingPublishesNothing(t *testing.T) {
	t.Parallel()
	sctx, remote := newGitLabEvidencePublishContext(t, "feature/media", "https://gitlab.example/group/widgets.git", "gitlab.example")
	sctx.Repo.ForkURL = "https://gitlab.example/fork/widgets.git"
	writeRunEvidence(t, sctx, map[string]string{"transcript.txt": "Checkout succeeded\n"})
	if links := publishRunEvidence(sctx); links != nil {
		t.Fatalf("legacy GitLab fork must retain existing refusal, links=%+v", links)
	}
	if refs := gitCmd(t, remote, "for-each-ref", "--format=%(refname)"); refs != "refs/heads/main" {
		t.Fatalf("ref changed despite unsupported fork routing: %s", refs)
	}
}

func TestPRStep_GitLabBranchAndMediaFailuresAreIndependent(t *testing.T) {
	t.Parallel()
	for _, failBranch := range []bool{false, true} {
		t.Run(fmt.Sprintf("branch_failure=%v", failBranch), func(t *testing.T) {
			t.Parallel()
			sctx, remote := newGitLabEvidencePublishContext(t, "feature/media", "https://gitlab.example/group/widgets.git", "gitlab.example")
			sctx.Agent = prDraftAgent()
			writeRunEvidence(t, sctx, map[string]string{"checkout.png": "\x89PNG binary"})
			image := filepath.Join(sctx.EvidenceDir, "checkout.png")
			insertCompletedStep(t, sctx, types.StepTest, screenshotFindings(image), "")
			markdown := "![Checkout screenshot](/uploads/abc/checkout.png)"
			uploader := &stubMediaUploader{t: t, urls: map[string]string{"checkout.png": "/uploads/abc/checkout.png"}}
			if failBranch {
				// Publishing evidence to the code branch must be refused.
				sctx.Config.Test.Evidence.Branch = "main"
			} else {
				uploader.err = errors.New("media upload failed")
			}
			mainBefore := gitCmd(t, remote, "rev-parse", "main")
			content, err := (&PRStep{mediaUploader: uploader}).buildPRContent(sctx, "feature/media", "main", sctx.Run.BaseSHA, scm.ProviderGitLab, 0)
			if err != nil {
				t.Fatal(err)
			}
			if gitCmd(t, remote, "rev-parse", "main") != mainBefore {
				t.Fatal("refused evidence publication changed main")
			}
			if failBranch {
				if !strings.Contains(content.Body, markdown) || strings.Contains(content.Body, "/-/blob/") {
					t.Fatalf("branch failure removed media or minted a dead file link:\n%s", content.Body)
				}
			} else {
				tip := gitCmd(t, remote, "rev-parse", "refs/heads/no-mistakes/evidence")
				link := "https://gitlab.example/group/widgets/-/blob/" + tip + "/.no-mistakes/evidence/feature/media/checkout.png"
				if !strings.Contains(content.Body, link) || strings.Contains(content.Body, "/uploads/") || strings.Contains(content.Body, "local file:") {
					t.Fatalf("media failure removed the published branch link:\n%s", content.Body)
				}
			}
		})
	}
}
