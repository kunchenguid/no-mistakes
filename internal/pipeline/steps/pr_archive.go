package steps

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/evidence"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// composeOwnedPRContentWithArchive keeps the ordinary non-truncating composer
// as the default. Only the existing evidence-publication opt-in permits moving
// an oversized generated appendix to a commit-pinned file. Author text is never
// moved, and the live attestation is carried verbatim, not regenerated as a pass.
func composeOwnedPRContentWithArchive(sctx *pipeline.StepContext, parts prOwnedBody, title, appendix string, bodyLimit int) (prContent, error) {
	content, err := composeOwnedPRContent(parts, title, appendix, bodyLimit)
	if !errors.Is(err, errOwnedPRBudget) {
		return content, err
	}
	if sctx == nil || sctx.Config == nil || sctx.Repo == nil || sctx.Run == nil || !sctx.Config.Test.Evidence.StoreInRepo {
		return prContent{}, err
	}
	linkURL := sctx.Repo.UpstreamURL
	if fork := strings.TrimSpace(sctx.Repo.ForkURL); fork != "" {
		linkURL = fork
	}
	repoPath := githubRepoPath(linkURL)
	if repoPath == "" || resolvedProviderForBody(sctx) != scm.ProviderGitHub {
		return prContent{}, fmt.Errorf("%w; lossless report archiving requires GitHub evidence links", err)
	}

	// Sanitize once at the same boundary as ordinary PR publication, then stamp
	// and archive those exact bytes. No source logs or private review files are
	// swept up merely because the generated report needs more room.
	clean := cleanPRAppendix(appendix)
	archive := wrapPRAppendix(clean)
	if _, err := parsePROwnedBody(archive); err != nil {
		return prContent{}, err
	}
	marker := extractPipelineAttestationMarker(clean)
	payload := strings.TrimSuffix(strings.TrimPrefix(marker, pipelineAttestationCommentPrefix), pipelineAttestationCommentClosingToken)
	var attestation pipelineAttestation
	if err := json.Unmarshal([]byte(payload), &attestation); err != nil || attestation.HeadSHA != sctx.Run.HeadSHA {
		return prContent{}, fmt.Errorf("cannot archive PR evidence for a different candidate head")
	}
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(archive)))
	name := "pr-report-" + digest + ".md"
	branch := strings.TrimPrefix(sctx.Run.Branch, "refs/heads/")
	segments := append(evidenceBranchSlug(branch), "pr-reports")
	message, err := sctx.Config.Test.Evidence.RenderCommitMessage(branch, sctx.Run.ID)
	if err != nil {
		return prContent{}, fmt.Errorf("prepare PR report archive: %w", err)
	}

	// Reuse the publisher's directory normalization rather than approximating
	// it when building links. The final URL uses the actual returned directory.
	compact := func(target string) string {
		return joinBlocks(
			fmt.Sprintf("## Validation evidence\n\n[Complete generated report](%s) (snapshot for candidate `%s`).\n\nSHA-256 of the exact archived file: `%s`. No recorded report content was discarded.", target, attestation.HeadSHA, digest),
			"## Pipeline", noMistakesPRSignature, marker,
			issuesSection(sctx, parts.before+"\n"+parts.after),
		)
	}
	// Check the remaining author text BEFORE any remote mutation. Allow room
	// for both SHA-1 and SHA-256 object IDs and the configured archive path.
	// A conservative URL can refuse near-full author text, never truncate it.
	probePath := joinURLPath(sctx.Config.Test.Evidence.Dir, strings.Join(segments, "/")+"/"+name)
	probeURL := "https://github.com/" + repoPath + "/blob/" + strings.Repeat("f", 64) + "/" + probePath
	if _, err := composeOwnedPRContent(parts, title, compact(probeURL), bodyLimit); err != nil {
		return prContent{}, fmt.Errorf("author text and archive reference cannot fit: %w", err)
	}

	dir, err := os.MkdirTemp("", "no-mistakes-pr-report-")
	if err != nil {
		return prContent{}, err
	}
	defer os.RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, name), []byte(archive), 0o600); err != nil {
		return prContent{}, err
	}
	result, err := evidence.Publish(sctx.Ctx, evidence.Request{
		RepoDir: sctx.WorkDir, PushURL: resolvePushURL(sctx),
		Branch: sctx.Config.Test.Evidence.Branch, Dir: sctx.Config.Test.Evidence.Dir,
		Segments: segments, SourceDir: dir, Message: message,
		ForbiddenBranches: []string{branch, sctx.Repo.DefaultBranch},
	})
	if err != nil {
		return prContent{}, fmt.Errorf("archive complete PR report: %w", err)
	}
	if result == nil || result.CommitSHA == "" || len(result.Files) != 1 || result.Files[0] != name {
		return prContent{}, fmt.Errorf("PR report archive did not publish the complete report")
	}
	archivePath := name
	if result.Dir != "" {
		archivePath = result.Dir + "/" + name
	}
	stored, err := git.RunRaw(sctx.Ctx, sctx.WorkDir, "show", result.CommitSHA+":"+archivePath)
	if err != nil || !bytes.Equal(stored, []byte(archive)) {
		return prContent{}, fmt.Errorf("published PR report archive failed byte-integrity verification")
	}
	target := "https://github.com/" + repoPath + "/blob/" + url.PathEscape(result.CommitSHA) + "/" + joinURLPath(result.Dir, name)
	content, err = composeOwnedPRContent(parts, title, compact(target), bodyLimit)
	if err != nil {
		return prContent{}, err
	}
	sctx.Log(fmt.Sprintf("archived complete PR report (%d bytes, sha256=%s) at %s", len(archive), digest, target))
	return content, nil
}
