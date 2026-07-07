package steps

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type stagedEvidenceArtifact struct {
	sourcePath string
	filename   string
	rawIndex   *string
	index      int
}

func publishTestingEvidenceGists(sctx *pipeline.StepContext, host scm.Host, steps []*db.StepResult, rounds map[string][]*db.StepRound, opts testingSummaryOptions) {
	if sctx == nil || sctx.Config == nil || !sctx.Config.Test.Evidence.UploadToGist || host == nil || !host.Capabilities().SecretGists {
		return
	}
	gistHost, ok := host.(scm.SecretGistHost)
	if !ok {
		return
	}

	var staged []stagedEvidenceArtifact
	for _, sr := range steps {
		if sr.StepName != types.StepTest {
			continue
		}
		staged = append(staged, collectGistEvidenceArtifacts(sr.FindingsJSON, opts)...)
		for _, round := range rounds[sr.ID] {
			staged = append(staged, collectGistEvidenceArtifacts(round.FindingsJSON, opts)...)
		}
	}
	if len(staged) == 0 {
		return
	}

	stageDir, err := os.MkdirTemp("", "no-mistakes-evidence-gist-*")
	if err != nil {
		slog.Warn("failed to stage PR evidence for gist upload", "err", err)
		return
	}
	defer os.RemoveAll(stageDir)

	paths := make([]string, 0, len(staged))
	used := map[string]bool{}
	for i := range staged {
		filename := uniqueEvidenceFilename(staged[i].filename, used)
		staged[i].filename = filename
		dst := filepath.Join(stageDir, filename)
		data, err := os.ReadFile(staged[i].sourcePath)
		if err != nil {
			slog.Warn("failed to read PR evidence for gist upload", "path", staged[i].sourcePath, "err", err)
			return
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			slog.Warn("failed to stage PR evidence for gist upload", "path", staged[i].sourcePath, "err", err)
			return
		}
		paths = append(paths, dst)
	}

	gist, err := gistHost.CreateSecretGist(sctx.Ctx, paths)
	if err != nil {
		if gist != nil && gist.ID != "" {
			recordEvidenceGistID(sctx, gist.ID)
		}
		sctx.Log(fmt.Sprintf("warning: failed to upload visual evidence to secret gist; keeping local file references: %v", err))
		return
	}
	if gist == nil {
		sctx.Log("warning: secret gist upload returned no result; keeping local file references")
		return
	}
	if gist.ID != "" {
		recordEvidenceGistID(sctx, gist.ID)
	}
	rawURLs := map[string]string{}
	for _, file := range gist.Files {
		if file.Filename != "" && file.RawURL != "" {
			rawURLs[file.Filename] = file.RawURL
		}
	}
	if len(rawURLs) == 0 {
		sctx.Log("warning: secret gist upload returned no raw evidence URLs; keeping local file references")
		return
	}

	for _, item := range staged {
		rawURL := rawURLs[item.filename]
		if rawURL == "" || item.rawIndex == nil {
			continue
		}
		*item.rawIndex = setArtifactURL(*item.rawIndex, item.index, rawURL)
	}
}

func recordEvidenceGistID(sctx *pipeline.StepContext, gistID string) {
	if err := sctx.DB.AddRunEvidenceGistIDs(sctx.Run.ID, []string{gistID}); err != nil {
		slog.Warn("failed to persist evidence gist id", "run", sctx.Run.ID, "gist", gistID, "err", err)
	}
}

func collectGistEvidenceArtifacts(raw *string, opts testingSummaryOptions) []stagedEvidenceArtifact {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return nil
	}
	findings, err := types.ParseFindingsJSON(*raw)
	if err != nil {
		return nil
	}
	var staged []stagedEvidenceArtifact
	for i, artifact := range findings.Artifacts {
		if strings.TrimSpace(artifact.URL) != "" {
			continue
		}
		kind := strings.ToLower(sanitizePromptText(artifact.Kind))
		cleanPath := sanitizeArtifactPath(artifact.Path, opts)
		if cleanPath == "" || !isVisualEvidenceArtifact(kind, cleanPath) {
			continue
		}
		source := artifactFilesystemPath(cleanPath, opts)
		if source == "" {
			continue
		}
		label := sanitizePromptText(artifact.Label)
		if label == "" {
			continue
		}
		staged = append(staged, stagedEvidenceArtifact{
			sourcePath: source,
			filename:   evidenceGistFilename(len(staged)+1, label, source),
			rawIndex:   raw,
			index:      i,
		})
	}
	return staged
}

func setArtifactURL(raw string, index int, rawURL string) string {
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil || index < 0 || index >= len(findings.Artifacts) {
		return raw
	}
	findings.Artifacts[index].URL = rawURL
	data, err := json.Marshal(findings)
	if err != nil {
		return raw
	}
	return string(data)
}

func isVisualEvidenceArtifact(kind, target string) bool {
	return isImageArtifact(kind, target) || isVideoArtifact(kind, target)
}

func evidenceGistFilename(n int, label, source string) string {
	ext := strings.ToLower(filepath.Ext(source))
	if ext == "" {
		ext = ".bin"
	}
	base := sanitizeEvidenceSegment(label)
	if base == "" {
		base = "evidence"
	}
	return fmt.Sprintf("%02d-%s%s", n, base, ext)
}

func uniqueEvidenceFilename(filename string, used map[string]bool) string {
	if !used[filename] {
		used[filename] = true
		return filename
	}
	ext := filepath.Ext(filename)
	base := strings.TrimSuffix(filename, ext)
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s-%d%s", base, i, ext)
		if !used[candidate] {
			used[candidate] = true
			return candidate
		}
	}
}
