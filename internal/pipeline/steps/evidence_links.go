package steps

import (
	"fmt"
	"html"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/config"
)

// test.evidence.link_base_url turns every evidence file the PR body mentions
// into a link, for an operator who serves the run evidence parent
// (evidence_dir or test.evidence.local_root) over HTTP themselves - a synced
// folder behind a web server, say. A reviewer then opens a screenshot in a new
// tab instead of copying a local path out of the description.
//
// This file is the ONE owner of that rendering. The Testing section's local
// artifact references (renderLocalArtifactReference) and the GitLab Evidence
// section (gitlabEvidenceSection) both call renderEvidenceFileLink, so the two
// surfaces cannot drift apart, and an unset base returns "" from every helper
// here so each caller keeps today's rendering byte for byte.
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
// or the file is not inside it. Every path segment is percent-escaped.
func evidenceFileURL(base, evidenceDir, path string) string {
	if base == "" || evidenceDir == "" || path == "" {
		return ""
	}
	runDir := filepath.Clean(evidenceDir)
	runID := filepath.Base(runDir)
	if runID == "" || runID == "." || runID == ".." || runID == string(filepath.Separator) {
		return ""
	}
	rel, err := filepath.Rel(runDir, filepath.Clean(path))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
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
	).Replace(target)
	return fmt.Sprintf("[%s](%s)", html.EscapeString(markdownAltText(label)), target)
}
