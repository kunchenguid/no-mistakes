package steps

import (
	"fmt"
	"html"
	"io/fs"
	"net/url"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/reviewqa"
)

// test.evidence.link_base_url links structured local evidence references,
// for an operator who serves the run evidence parent
// (evidence_dir or test.evidence.local_root) over HTTP themselves - a synced
// folder behind a web server, say. A reviewer then opens a screenshot in a new
// tab instead of copying a local path out of the description.
//
// This file is the ONE owner of that rendering. The Testing section's local
// artifact references and transcript sources (renderLocalArtifactReference),
// scenario Evidence cells (renderScenarioEvidenceReferences), and the GitLab
// Evidence section (gitlabEvidenceSection) call renderEvidenceFileLink so
// escaping cannot drift between surfaces. Callers preserve existing remote
// artifact links and free-form prose; an unset base keeps their prior output.
//
// The URL mirrors the served directory layout: <base>/<run-id>/<file>, where
// the run ID is the evidence directory's own name and <file> is the path
// inside it. Hosting, access control, and retention of the served copy are
// the operator's; the pipeline never checks that a link resolves.

// evidenceLinkBase returns the configured base with no trailing slash, or ""
// when the setting is unset.
func evidenceLinkBase(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	return strings.TrimRight(strings.TrimSpace(cfg.Test.Evidence.LinkBaseURL), "/")
}

// evidenceFileURL returns <base>/<run-id>/<file> for a file under the run's
// evidence directory, or "" when the base is unset, the directory is unknown,
// or the file is not inside it. Containment is the same symlink-aware check
// the artifact allowlist uses (artifactPathRelativeToRoot), so an artifact
// that records the real path of a file under a symlinked evidence root is
// still linked; the run segment keeps the configured directory's own name,
// which is what the operator serves. Every path segment is percent-escaped.
func evidenceFileURL(base, evidenceDir, path string) string {
	if base == "" || evidenceDir == "" || path == "" {
		return ""
	}
	runDir := filepath.Clean(evidenceDir)
	runID := filepath.Base(runDir)
	if runID == "" || runID == "." || runID == ".." || runID == string(filepath.Separator) {
		return ""
	}
	rel, ok := artifactPathRelativeToRoot(filepath.Clean(path), runDir)
	if !ok {
		return ""
	}
	segments := []string{url.PathEscape(runID)}
	for _, seg := range strings.Split(filepath.ToSlash(rel), "/") {
		if seg == "" {
			continue
		}
		segments = append(segments, url.PathEscape(seg))
	}
	return base + "/" + strings.Join(segments, "/")
}

// renderEvidenceFileLink is the one rendering of an evidence file mention
// when link_base_url is set: a markdown link whose text is the label. It
// returns "" when the file gets no URL, and the caller keeps today's output.
func renderEvidenceFileLink(base, evidenceDir, path, label string) string {
	target := evidenceFileURL(base, evidenceDir, path)
	if target == "" {
		return ""
	}
	target = strings.NewReplacer(
		"(", "%28", ")", "%29",
		"<", "%3C", ">", "%3E",
		" ", "%20", "\\", "%5C",
		"&", "%26",
	).Replace(target)
	return fmt.Sprintf("[%s](%s)", html.EscapeString(markdownAltText(label)), target)
}

// evidenceFileNames lists the regular files under the run's evidence
// directory as slash-separated paths relative to it, longest first, so a
// scenario cell can be scanned for complete file references - names with
// spaces and nested paths included - instead of whitespace tokens. The
// review conversation directory is never listed: it is never published.
func evidenceFileNames(root string) []string {
	root = resolveArtifactPathSymlinks(filepath.Clean(root))
	var names []string
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if entry.IsDir() {
			if entry.Name() == reviewqa.DirName && path != root {
				return fs.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == "." {
			return nil
		}
		names = append(names, filepath.ToSlash(rel))
		return nil
	})
	sort.Slice(names, func(i, j int) bool {
		if len(names[i]) != len(names[j]) {
			return len(names[i]) > len(names[j])
		}
		return names[i] < names[j]
	})
	return names
}
