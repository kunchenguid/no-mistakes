package steps

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/evidence"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/reviewqa"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// evidenceLinks describes a published evidence commit well enough to turn a
// local artifact path into a stable PR link.
type evidenceLinks struct {
	// blobBase and rawBase are the provider URL prefixes for the evidence
	// COMMIT, not the branch: a later run may overwrite the same paths at the
	// branch tip, and an old PR must keep showing the bytes it was reviewed
	// against.
	blobBase string
	rawBase  string
	// root is the local directory the artifacts were published from.
	root string
	// dir is the run's directory inside the evidence branch, "" at root.
	dir string
	// files is the set of published paths relative to root (slash separated).
	files map[string]bool
}

// publishRunEvidence copies this run's evidence directory onto the repository's
// orphan evidence branch and returns the forge's file links for the PR body. It
// returns nil when evidence is not opted in, the forge has no file-link
// support, there is nothing to publish, or publication failed. The renderer
// then omits evidence-branch links that would not resolve; it may still use an
// uploaded media attachment, otherwise the artifact keeps its local-path
// rendering.
func publishRunEvidence(sctx *pipeline.StepContext) *evidenceLinks {
	if sctx == nil || sctx.Config == nil || sctx.Repo == nil || sctx.Run == nil || !sctx.Config.Test.Evidence.StoreInRepo {
		return nil
	}
	sourceDir := testEvidenceDir(sctx)
	if sourceDir == "" {
		return nil
	}
	branch := strings.TrimPrefix(sctx.Run.Branch, "refs/heads/")
	segments := evidenceBranchSlug(branch)
	if len(segments) == 0 {
		segments = []string{sctx.Run.ID}
	}
	// Links are built from the registered repository URL, never from the push
	// URL: the latter can carry an embedded credential, and a PR body must
	// never publish one. Without a link base there is nothing to gain from
	// publishing, so the branch is not pushed at all rather than pushed and
	// never referenced. The forge adapter builds the blob and raw URL prefixes.
	blobPrefix, rawPrefix, supported := repositoryFileLinks(sctx, resolvedProvider(sctx), sctx.Repo.PushURL())
	if !supported {
		sctx.Log("test evidence branch not published: this forge has no supported file links; media attachments are handled separately")
		return nil
	}

	result, err := evidence.Publish(sctx.Ctx, evidence.Request{
		RepoDir:   sctx.WorkDir,
		PushURL:   resolvePushURL(sctx),
		Branch:    sctx.Config.Test.Evidence.Branch,
		Dir:       sctx.Config.Test.Evidence.Dir,
		Segments:  segments,
		SourceDir: sourceDir,
		// The review conversation lives in this same directory but is NOT test
		// evidence and must never be published: the operator's questions and
		// answers would land on the orphan branch verbatim and permanently,
		// with none of the bounding or home-path redaction the deliberate
		// PR-body rendering applies. The name comes from the package that owns
		// the location, so the two cannot drift.
		ExcludeDirs:       []string{reviewqa.DirName},
		Message:           fmt.Sprintf("no-mistakes: evidence for %s (run %s)", branch, sctx.Run.ID),
		ForbiddenBranches: []string{branch, sctx.Repo.DefaultBranch},
	})
	if err != nil {
		sctx.Log(fmt.Sprintf("test evidence branch not published, media attachments are handled separately: %v", err))
		return nil
	}
	if result == nil || result.CommitSHA == "" {
		return nil
	}
	sctx.Log(fmt.Sprintf("published %d test evidence file(s) to %s (%s)", len(result.Files), result.Branch, result.CommitSHA))

	links := &evidenceLinks{root: sourceDir, dir: result.Dir, files: map[string]bool{}}
	for _, file := range result.Files {
		links.files[file] = true
	}
	links.blobBase = blobPrefix + url.PathEscape(result.CommitSHA) + "/"
	links.rawBase = rawPrefix + url.PathEscape(result.CommitSHA) + "/"
	return links
}

// repositoryFileLinks returns the forge's blob and raw URL prefixes. Source-head
// artifact links, branch publication, and the Test prompt share this check.
func repositoryFileLinks(sctx *pipeline.StepContext, provider scm.Provider, remoteURL string) (string, string, bool) {
	host, _ := buildHost(sctx, provider)
	linker, ok := host.(scm.RepositoryFileLinker)
	if !ok {
		return "", "", false
	}
	return linker.RepositoryFileLinks(remoteURL)
}

// target returns the PR link for an artifact path published by this run, or ""
// when the path is not one of them. raw selects the rendering host used for
// images and videos so they display inline instead of as a page link.
func (e *evidenceLinks) target(artifactPath string, raw bool) string {
	if e == nil || e.blobBase == "" || artifactPath == "" {
		return ""
	}
	rel, ok := artifactPathRelativeToRoot(artifactPath, e.root)
	if !ok {
		return ""
	}
	slashed := filepath.ToSlash(rel)
	if !e.files[slashed] {
		return ""
	}
	base := e.blobBase
	if raw {
		base = e.rawBase
	}
	return base + joinURLPath(e.dir, slashed)
}

func joinURLPath(dir, rel string) string {
	joined := strings.TrimPrefix(rel, "/")
	if dir != "" {
		joined = strings.TrimSuffix(dir, "/") + "/" + joined
	}
	segments := strings.Split(joined, "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}
	return strings.Join(segments, "/")
}
