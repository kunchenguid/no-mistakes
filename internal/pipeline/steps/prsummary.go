package steps

import (
	"bytes"
	"fmt"
	"html"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	maxEmbeddedArtifactBytes       = 16 * 1024
	maxEmbeddedArtifactsTotalBytes = 32 * 1024
	noMistakesPRSignature          = "Updates from [git push no-mistakes](https://github.com/kunchenguid/no-mistakes)"
)

type testingArtifactRenderState struct {
	remainingEmbeddedBytes int
}

type testingSummaryOptions struct {
	githubBlobBase       string
	githubRawBase        string
	includeTestedDetails bool
	compactArtifacts     bool
	summaryParagraph     bool
	omitOutcome          bool
	repoRoot             string
}

// BuildPipelineSummary produces a deterministic markdown section from step results and rounds.
func BuildPipelineSummary(steps []*db.StepResult, rounds map[string][]*db.StepRound) (string, string) {
	if len(steps) == 0 {
		return "", ""
	}

	var detailBlocks []string

	for _, sr := range steps {
		if shouldOmitPipelineStep(sr) {
			continue
		}
		stepRounds := rounds[sr.ID]
		line, detail := buildStepEntry(sr, stepRounds)
		if line != "" && detail != "" {
			detailBlocks = append(detailBlocks, detail)
		}
	}

	if len(detailBlocks) == 0 {
		return "", ""
	}

	var b strings.Builder
	b.WriteString("## Pipeline\n\n")
	b.WriteString(noMistakesPRSignature)
	b.WriteString("\n\n")
	for i, detail := range detailBlocks {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(detail)
	}

	riskLine := extractRiskLine(steps, rounds)
	return b.String(), riskLine
}

// BuildTestingSummary extracts a deterministic Testing section from the test step.
func BuildTestingSummary(steps []*db.StepResult, rounds map[string][]*db.StepRound) string {
	return buildTestingSummary(steps, rounds, testingSummaryOptions{includeTestedDetails: true})
}

func BuildTestingSummaryForPR(steps []*db.StepResult, rounds map[string][]*db.StepRound, upstreamURL, ref, repoRoot string) string {
	opts := testingSummaryOptionsForGitHub(upstreamURL, ref)
	opts.compactArtifacts = true
	opts.summaryParagraph = true
	opts.omitOutcome = true
	opts.repoRoot = repoRoot
	return buildTestingSummary(steps, rounds, opts)
}

func buildTestingSummary(steps []*db.StepResult, rounds map[string][]*db.StepRound, opts testingSummaryOptions) string {
	for _, sr := range steps {
		if sr.StepName != types.StepTest {
			continue
		}

		stepRounds := rounds[sr.ID]
		line, _ := buildStepEntry(sr, stepRounds)
		if line == "" {
			return ""
		}

		testingSummary := collectTestingSummary(sr, stepRounds)
		tested := collectTestingDetails(sr, stepRounds)
		artifacts := collectTestingArtifacts(sr, stepRounds, opts)
		if testingSummary == "" && len(tested) == 0 && len(artifacts) == 0 {
			return "## Testing\n\n- " + line
		}

		var b strings.Builder
		b.WriteString("## Testing\n\n")
		wroteSummary := false
		if testingSummary != "" {
			rendered := renderTestingSummary(testingSummary)
			if rendered != "" {
				writeTestingSummary(&b, rendered, opts)
				wroteSummary = true
			}
		} else if !opts.includeTestedDetails && len(tested) > 0 {
			writeTestingSummary(&b, compactTestedSummary(len(tested)), opts)
			wroteSummary = true
		}
		if opts.includeTestedDetails {
			for _, detail := range tested {
				rendered := renderTestedDetail(detail)
				if rendered == "" {
					continue
				}
				b.WriteString("- ")
				b.WriteString(rendered)
				b.WriteString("\n")
			}
		}
		renderState := testingArtifactRenderState{remainingEmbeddedBytes: maxEmbeddedArtifactsTotalBytes}
		for _, artifact := range artifacts {
			rendered := renderTestingArtifact(artifact, opts, &renderState)
			if rendered == "" {
				continue
			}
			b.WriteString(rendered)
			if !strings.HasSuffix(rendered, "\n") {
				b.WriteString("\n")
			}
		}
		if outcome := buildTestingOutcomeLine(line, stepRounds); shouldRenderTestingOutcome(opts, wroteSummary, outcome) {
			b.WriteString("- ")
			b.WriteString(outcome)
			b.WriteString("\n")
		}

		return strings.TrimSpace(b.String())
	}

	return ""
}

func shouldRenderTestingOutcome(opts testingSummaryOptions, wroteSummary bool, outcome string) bool {
	if outcome == "" {
		return false
	}
	return !opts.omitOutcome || !wroteSummary || !strings.Contains(outcome, "✅ passed")
}

func compactTestedSummary(count int) string {
	if count == 1 {
		return "Completed 1 recorded test check."
	}
	return fmt.Sprintf("Completed %d recorded test checks.", count)
}

func writeTestingSummary(b *strings.Builder, rendered string, opts testingSummaryOptions) {
	if opts.summaryParagraph {
		b.WriteString(rendered)
		b.WriteString("\n\n")
		return
	}
	b.WriteString("- Summary: ")
	b.WriteString(rendered)
	b.WriteString("\n")
}

func testingSummaryOptionsForGitHub(upstreamURL, ref string) testingSummaryOptions {
	repoPath := githubRepoPath(upstreamURL)
	ref = strings.TrimSpace(ref)
	if repoPath == "" || ref == "" || strings.ContainsAny(ref, "\n\r <>[]()\\") {
		return testingSummaryOptions{}
	}
	return testingSummaryOptions{
		githubBlobBase:       "https://github.com/" + repoPath + "/blob/" + url.PathEscape(ref) + "/",
		githubRawBase:        "https://raw.githubusercontent.com/" + repoPath + "/" + url.PathEscape(ref) + "/",
		includeTestedDetails: false,
	}
}

func githubRepoPath(remote string) string {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return ""
	}
	if strings.HasPrefix(remote, "git@github.com:") {
		repo := strings.TrimPrefix(remote, "git@github.com:")
		return cleanGitHubRepoPath(repo)
	}
	parsed, err := url.Parse(remote)
	if err != nil || !strings.EqualFold(parsed.Host, "github.com") {
		return ""
	}
	return cleanGitHubRepoPath(strings.TrimPrefix(parsed.Path, "/"))
}

func cleanGitHubRepoPath(repo string) string {
	repo = strings.TrimSuffix(strings.TrimSpace(repo), ".git")
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return ""
	}
	if strings.ContainsAny(repo, "\n\r <>[]()\\") || strings.Contains(repo, "..") {
		return ""
	}
	return url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1])
}

func collectTestingSummary(sr *db.StepResult, rounds []*db.StepRound) string {
	if summary := testingSummaryFromFindings(sr.FindingsJSON); summary != "" {
		return summary
	}
	for i := len(rounds) - 1; i >= 0; i-- {
		if summary := testingSummaryFromFindings(rounds[i].FindingsJSON); summary != "" {
			return summary
		}
	}
	return ""
}

func testingSummaryFromFindings(raw *string) string {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return ""
	}
	findings, err := types.ParseFindingsJSON(*raw)
	if err != nil {
		return ""
	}
	return sanitizePromptMultilineText(findings.TestingSummary)
}

func collectTestingDetails(sr *db.StepResult, rounds []*db.StepRound) []string {
	seen := map[string]bool{}
	var details []string
	for _, raw := range testingEvidenceFindingsJSON(sr, rounds) {
		details = appendTestingDetails(details, seen, raw)
	}
	return details
}

func collectTestingArtifacts(sr *db.StepResult, rounds []*db.StepRound, opts testingSummaryOptions) []types.TestArtifact {
	seen := map[string]bool{}
	var artifacts []types.TestArtifact
	for _, raw := range testingEvidenceFindingsJSON(sr, rounds) {
		artifacts = appendTestingArtifacts(artifacts, seen, raw, opts)
	}
	return artifacts
}

func testingEvidenceFindingsJSON(sr *db.StepResult, rounds []*db.StepRound) []*string {
	if hasTestingEvidenceMetadata(sr.FindingsJSON) {
		return []*string{sr.FindingsJSON}
	}
	for i := len(rounds) - 1; i >= 0; i-- {
		if hasTestingEvidenceMetadata(rounds[i].FindingsJSON) {
			return []*string{rounds[i].FindingsJSON}
		}
	}
	return nil
}

func hasTestingEvidenceMetadata(raw *string) bool {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return false
	}
	findings, err := types.ParseFindingsJSON(*raw)
	if err != nil {
		return false
	}
	return strings.TrimSpace(findings.TestingSummary) != "" || len(findings.Tested) > 0 || len(findings.Artifacts) > 0
}

func appendTestingArtifacts(artifacts []types.TestArtifact, seen map[string]bool, raw *string, opts testingSummaryOptions) []types.TestArtifact {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return artifacts
	}
	findings, err := types.ParseFindingsJSON(*raw)
	if err != nil {
		return artifacts
	}
	for _, artifact := range findings.Artifacts {
		artifact.Label = sanitizePromptText(artifact.Label)
		artifact.Kind = strings.ToLower(sanitizePromptText(artifact.Kind))
		artifact.Path = sanitizeArtifactPath(artifact.Path, opts)
		artifact.URL = sanitizeArtifactURL(artifact.URL)
		artifact.Content = sanitizePromptMultilineText(artifact.Content)
		key := artifact.Kind + "\x00" + artifact.Label + "\x00" + artifact.Path + "\x00" + artifact.URL + "\x00" + artifact.Content
		if artifact.Label == "" || seen[key] {
			continue
		}
		if artifact.Path == "" && artifact.URL == "" && artifact.Content == "" {
			continue
		}
		seen[key] = true
		artifacts = append(artifacts, artifact)
	}
	return artifacts
}

func appendTestingDetails(details []string, seen map[string]bool, raw *string) []string {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return details
	}
	findings, err := types.ParseFindingsJSON(*raw)
	if err != nil {
		return details
	}
	for _, detail := range findings.Tested {
		clean := sanitizePromptText(detail)
		if clean == "" || seen[clean] {
			continue
		}
		seen[clean] = true
		details = append(details, clean)
	}
	return details
}

func renderTestedDetail(detail string) string {
	clean := sanitizePromptMultilineText(detail)
	if clean == "" {
		return ""
	}
	if strings.HasPrefix(clean, "`") && strings.HasSuffix(clean, "`") && strings.Count(clean, "`") == 2 && !strings.Contains(clean[1:len(clean)-1], "\n") {
		return clean
	}
	if !strings.Contains(clean, "`") && !strings.Contains(clean, "\n") {
		return fmt.Sprintf("`%s`", clean)
	}
	escaped := html.EscapeString(clean)
	escaped = strings.ReplaceAll(escaped, "\n", "&#10;")
	return fmt.Sprintf("<code>%s</code>", escaped)
}

func renderTestingSummary(summary string) string {
	clean := sanitizePromptMultilineText(summary)
	if clean == "" {
		return ""
	}
	// Inline backtick code spans are valid markdown prose and render fine on
	// their own; only newlines or angle brackets need the escaped <code> wrapper.
	if strings.ContainsAny(clean, "\n<>") {
		return renderTestedDetail(clean)
	}
	return clean
}

func renderTestingArtifact(artifact types.TestArtifact, opts testingSummaryOptions, state *testingArtifactRenderState) string {
	label := sanitizePromptText(artifact.Label)
	if label == "" {
		return ""
	}
	if opts.compactArtifacts {
		return renderCompactTestingArtifact(artifact, opts, label, state)
	}
	target := artifact.URL
	if target == "" {
		target = artifactTargetForPath(artifact, opts)
	}
	localPath := localArtifactPath(artifact.Path, opts)
	fileText, hasFile := embeddedArtifactText(artifact, opts, state)
	caption := artifact.Content
	fenceBody, descriptionLine := caption, ""
	if hasFile {
		fenceBody, descriptionLine = fileText, caption
	}

	var b strings.Builder
	if target != "" && isImageArtifact(artifact.Kind, target) {
		b.WriteString(fmt.Sprintf("**%s**\n\n![%s](%s)\n", html.EscapeString(label), markdownAltText(label), target))
	} else if target != "" && isVideoArtifact(artifact.Kind, target) {
		b.WriteString(fmt.Sprintf("**%s**\n\n<video src=\"%s\" controls></video>\n", html.EscapeString(label), html.EscapeString(target)))
	} else if !hasFile {
		if target != "" {
			b.WriteString(fmt.Sprintf("- Evidence: [%s](%s)\n", html.EscapeString(label), target))
		} else if localPath != "" {
			b.WriteString(renderLocalArtifactLine(label, localPath))
		}
	}
	if descriptionLine != "" {
		if b.Len() > 0 && !strings.HasSuffix(b.String(), "\n\n") {
			b.WriteString("\n")
		}
		b.WriteString(renderTestedDetail(descriptionLine))
		b.WriteString("\n")
	}
	if fenceBody != "" {
		if b.Len() > 0 && !strings.HasSuffix(b.String(), "\n\n") {
			b.WriteString("\n")
		}
		b.WriteString(fmt.Sprintf("**%s**\n\n```text\n%s\n```\n", html.EscapeString(label), escapeMarkdownFence(fenceBody)))
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

func renderCompactTestingArtifact(artifact types.TestArtifact, opts testingSummaryOptions, label string, state *testingArtifactRenderState) string {
	target := artifact.URL
	if target == "" {
		target = artifactLinkTargetForPath(artifact, opts)
	}
	localPath := localArtifactPath(artifact.Path, opts)
	fileText, hasFile := embeddedArtifactText(artifact, opts, state)
	caption := artifact.Content

	if target == "" && localPath == "" && caption == "" && !hasFile {
		return ""
	}

	// No embeddable text: render a link or local-file reference (images, videos, binaries).
	if caption == "" && !hasFile {
		if target != "" {
			return fmt.Sprintf("- Evidence: [%s](%s)\n", html.EscapeString(label), target)
		}
		return renderLocalArtifactLine(label, localPath)
	}

	fenceBody, descriptionLine := caption, ""
	if hasFile {
		fenceBody, descriptionLine = fileText, caption
	}

	var b strings.Builder
	b.WriteString("<details>\n")
	b.WriteString(fmt.Sprintf("<summary>Evidence: %s</summary>\n\n", html.EscapeString(label)))
	if target != "" {
		b.WriteString(fmt.Sprintf("Source: [%s](%s)\n\n", html.EscapeString(label), target))
	} else if !hasFile && localPath != "" {
		b.WriteString(renderLocalArtifactReference("Source", label, localPath))
		b.WriteString("\n")
	}
	if descriptionLine != "" {
		b.WriteString(renderTestedDetail(descriptionLine))
		b.WriteString("\n\n")
	}
	b.WriteString(fmt.Sprintf("```text\n%s\n```\n", escapeMarkdownFence(fenceBody)))
	b.WriteString("</details>\n")
	return b.String()
}

// embeddedArtifactText reads a file artifact and returns its text content,
// truncated from the middle when it exceeds maxEmbeddedArtifactBytes. ok is
// false when the artifact has no path, points at an image/video, resolves
// outside the allowed roots, is missing, empty, or is not UTF-8 text.
func embeddedArtifactText(artifact types.TestArtifact, opts testingSummaryOptions, state *testingArtifactRenderState) (string, bool) {
	if artifact.Path == "" {
		return "", false
	}
	if state == nil || state.remainingEmbeddedBytes <= 0 {
		return "", false
	}
	if isImageArtifact(artifact.Kind, artifact.Path) || isVideoArtifact(artifact.Kind, artifact.Path) {
		return "", false
	}
	fsPath := artifactFilesystemPath(artifact.Path, opts)
	if fsPath == "" {
		return "", false
	}
	text, err := readEmbeddedArtifactText(fsPath)
	if err != nil {
		return "", false
	}
	text = strings.TrimRight(text, "\n")
	if text == "" {
		return "", false
	}
	if len(text) > state.remainingEmbeddedBytes {
		return "", false
	}
	state.remainingEmbeddedBytes -= len(text)
	return text, true
}

func artifactFilesystemPath(p string, opts testingSummaryOptions) string {
	if p == "" {
		return ""
	}
	if !filepath.IsAbs(p) {
		return ""
	}
	if _, ok := artifactPathRelativeToRoot(p, testEvidenceRoot()); !ok {
		return ""
	}
	return p
}

func readEmbeddedArtifactText(fsPath string) (string, error) {
	info, err := os.Stat(fsPath)
	if err != nil || info.IsDir() {
		return "", err
	}
	if info.Size() <= int64(maxEmbeddedArtifactBytes) {
		data, err := os.ReadFile(fsPath)
		if err != nil || !looksLikeTextArtifact(data) {
			return "", err
		}
		return string(data), nil
	}

	file, err := os.Open(fsPath)
	if err != nil {
		return "", err
	}
	defer file.Close()

	headSize := maxEmbeddedArtifactBytes / 2
	tailSize := maxEmbeddedArtifactBytes - headSize
	head := make([]byte, headSize)
	if _, err := file.ReadAt(head, 0); err != nil {
		return "", err
	}
	tail := make([]byte, tailSize)
	if _, err := file.ReadAt(tail, info.Size()-int64(tailSize)); err != nil {
		return "", err
	}
	head = trimUTF8End(head)
	tail = trimUTF8Start(tail)
	if !looksLikeTextArtifact(head) || !looksLikeTextArtifact(tail) {
		return "", nil
	}
	omitted := info.Size() - int64(len(head)+len(tail))
	return string(head) + fmt.Sprintf("\n\n... [%d bytes truncated] ...\n\n", omitted) + string(tail), nil
}

func looksLikeTextArtifact(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	if bytes.IndexByte(data, 0) != -1 {
		return false
	}
	return utf8.Valid(data)
}

func trimUTF8End(data []byte) []byte {
	for len(data) > 0 && !utf8.Valid(data) {
		data = data[:len(data)-1]
	}
	return data
}

func trimUTF8Start(data []byte) []byte {
	for len(data) > 0 && !utf8.Valid(data) {
		_, size := utf8.DecodeRune(data)
		if size <= 0 {
			return nil
		}
		data = data[size:]
	}
	return data
}

func artifactTargetForPath(artifact types.TestArtifact, opts testingSummaryOptions) string {
	repoPath := repoRelativeArtifactPath(artifact.Path, opts)
	if repoPath == "" {
		return ""
	}
	if opts.githubBlobBase == "" || opts.githubRawBase == "" {
		return repoPath
	}
	if isImageArtifact(artifact.Kind, repoPath) || isVideoArtifact(artifact.Kind, repoPath) {
		return opts.githubRawBase + repoPath
	}
	return opts.githubBlobBase + repoPath
}

func artifactLinkTargetForPath(artifact types.TestArtifact, opts testingSummaryOptions) string {
	repoPath := repoRelativeArtifactPath(artifact.Path, opts)
	if repoPath == "" {
		return ""
	}
	if opts.githubBlobBase == "" {
		return repoPath
	}
	return opts.githubBlobBase + repoPath
}

func sanitizeArtifactPath(target string, opts testingSummaryOptions) string {
	clean := strings.TrimSpace(target)
	if clean == "" || clean != sanitizePromptText(target) || strings.ContainsAny(clean, "\n\r<>[]()`") {
		return ""
	}
	if filepath.IsAbs(clean) {
		return sanitizeAbsoluteArtifactPath(clean, opts)
	}
	if strings.HasPrefix(clean, "/") || strings.HasPrefix(clean, "~") || strings.Contains(clean, ":") || strings.Contains(clean, "\\") {
		return ""
	}
	cleanedPath := path.Clean(clean)
	if cleanedPath == "." || cleanedPath != clean || cleanedPath == ".." || strings.HasPrefix(cleanedPath, "../") {
		return ""
	}
	return clean
}

func sanitizeAbsoluteArtifactPath(clean string, opts testingSummaryOptions) string {
	cleanedPath := filepath.Clean(clean)
	if cleanedPath != clean {
		return ""
	}
	if _, ok := artifactPathRelativeToRoot(cleanedPath, opts.repoRoot); ok {
		return cleanedPath
	}
	if _, ok := artifactPathRelativeToRoot(cleanedPath, testEvidenceRoot()); ok {
		return cleanedPath
	}
	return ""
}

func repoRelativeArtifactPath(target string, opts testingSummaryOptions) string {
	if target == "" {
		return ""
	}
	if !filepath.IsAbs(target) {
		return target
	}
	rel, ok := artifactPathRelativeToRoot(target, opts.repoRoot)
	if !ok {
		return ""
	}
	return filepath.ToSlash(rel)
}

func localArtifactPath(target string, opts testingSummaryOptions) string {
	if target == "" || !filepath.IsAbs(target) {
		return ""
	}
	if _, ok := artifactPathRelativeToRoot(target, opts.repoRoot); ok {
		return ""
	}
	return target
}

func artifactPathRelativeToRoot(target, root string) (string, bool) {
	root = strings.TrimSpace(root)
	if target == "" || root == "" {
		return "", false
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", false
	}
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return "", false
	}
	rootAbs = filepath.Clean(rootAbs)
	targetAbs = filepath.Clean(targetAbs)
	rootAbs = resolveArtifactPathSymlinks(rootAbs)
	targetAbs = resolveArtifactPathSymlinks(targetAbs)
	if !sameVolume(rootAbs, targetAbs) {
		return "", false
	}
	rel, err := filepath.Rel(rootAbs, targetAbs)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

func resolveArtifactPathSymlinks(target string) string {
	if resolved, err := filepath.EvalSymlinks(target); err == nil {
		return filepath.Clean(resolved)
	}
	for candidate := target; ; candidate = filepath.Dir(candidate) {
		resolved, err := filepath.EvalSymlinks(candidate)
		if err == nil {
			rel, err := filepath.Rel(candidate, target)
			if err != nil || rel == "." {
				return filepath.Clean(resolved)
			}
			return filepath.Clean(filepath.Join(resolved, rel))
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			return target
		}
	}
}

func sameVolume(a, b string) bool {
	return strings.EqualFold(filepath.VolumeName(a), filepath.VolumeName(b)) || filepath.VolumeName(a) == "" || filepath.VolumeName(b) == ""
}

func renderLocalArtifactLine(label, localPath string) string {
	return renderLocalArtifactReference("- Evidence", label, localPath)
}

func renderLocalArtifactReference(prefix, label, localPath string) string {
	return fmt.Sprintf("%s: %s (local file: <code>%s</code>)\n", prefix, html.EscapeString(label), html.EscapeString(localPath))
}

func sanitizeArtifactURL(target string) string {
	clean := strings.TrimSpace(target)
	if clean == "" || clean != sanitizePromptText(target) || strings.ContainsAny(clean, "\n\r <>[]()\"'") {
		return ""
	}
	parsed, err := url.ParseRequestURI(clean)
	if err != nil || parsed.Host == "" {
		return ""
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
		return clean
	default:
		return ""
	}
}

func markdownAltText(label string) string {
	label = strings.ReplaceAll(label, "[", "(")
	label = strings.ReplaceAll(label, "]", ")")
	return label
}

func isImageArtifact(kind, target string) bool {
	if kind == "screenshot" || kind == "gif" || kind == "image" {
		return true
	}
	lower := strings.ToLower(target)
	for _, suffix := range []string{".png", ".jpg", ".jpeg", ".gif", ".webp", ".svg"} {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	return false
}

func isVideoArtifact(kind, target string) bool {
	if kind == "video" || kind == "recording" {
		return true
	}
	lower := strings.ToLower(target)
	for _, suffix := range []string{".mp4", ".webm", ".mov"} {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	return false
}

func escapeMarkdownFence(content string) string {
	return strings.ReplaceAll(content, "```", "`` `")
}

func buildTestingOutcomeLine(summaryLine string, rounds []*db.StepRound) string {
	outcome := strings.TrimSpace(strings.Replace(summaryLine, "**Test** - ", "", 1))
	if outcome == "" {
		return ""
	}
	if len(rounds) == 0 {
		return "Outcome: " + outcome
	}
	runLabel := "1 run"
	if len(rounds) != 1 {
		runLabel = fmt.Sprintf("%d runs", len(rounds))
	}
	totalDuration := int64(0)
	for _, r := range rounds {
		totalDuration += r.DurationMS
	}
	if totalDuration > 0 {
		return fmt.Sprintf("Outcome: %s across %s (%s)", outcome, runLabel, formatTestingDuration(totalDuration))
	}
	return fmt.Sprintf("Outcome: %s across %s", outcome, runLabel)
}

func formatTestingDuration(ms int64) string {
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	d := time.Duration(ms) * time.Millisecond
	if d < time.Minute {
		seconds := float64(ms) / 1000
		if ms%1000 == 0 {
			return fmt.Sprintf("%ds", ms/1000)
		}
		return fmt.Sprintf("%.1fs", seconds)
	}
	return d.Round(time.Second).String()
}

func buildStepEntry(sr *db.StepResult, rounds []*db.StepRound) (statusLine, detailBlock string) {
	name := stepDisplayName(sr.StepName)
	buildDetail := func(line string) (string, string) {
		return line, buildStepDetails(line, sr, rounds)
	}

	switch sr.Status {
	case types.StepStatusPending:
		return buildDetail(fmt.Sprintf("⏳ **%s** - pending", name))
	case types.StepStatusRunning:
		return buildDetail(fmt.Sprintf("⏳ **%s** - running", name))
	case types.StepStatusAwaitingApproval:
		return buildDetail(fmt.Sprintf("⏸️ **%s** - awaiting approval", name))
	case types.StepStatusFixing:
		return buildDetail(fmt.Sprintf("🔄 **%s** - auto-fixing", name))
	case types.StepStatusFixReview:
		return buildDetail(fmt.Sprintf("⏸️ **%s** - review fix", name))
	case types.StepStatusFailed:
		return buildDetail(fmt.Sprintf("❌ **%s** - failed", name))
	}

	if sr.Status == types.StepStatusSkipped {
		return buildDetail(fmt.Sprintf("⏭️ **%s** - skipped", name))
	}

	// Parse the final findings on the step result (last state).
	var finalFindings *types.Findings
	finalFindingsParsed := sr.FindingsJSON == nil
	if sr.FindingsJSON != nil {
		if f, err := types.ParseFindingsJSON(*sr.FindingsJSON); err == nil {
			finalFindings = &f
			finalFindingsParsed = true
		}
	}

	// Parse initial round findings (round 1) for the full story.
	var initialFindings *types.Findings
	if len(rounds) > 0 && rounds[0].FindingsJSON != nil {
		if f, err := types.ParseFindingsJSON(*rounds[0].FindingsJSON); err == nil {
			initialFindings = &f
		}
	}

	// Parse latest round findings for risk fallback when final state is cleared.
	var latestRoundFindings *types.Findings
	if len(rounds) > 0 {
		last := rounds[len(rounds)-1]
		if last.FindingsJSON != nil {
			if f, err := types.ParseFindingsJSON(*last.FindingsJSON); err == nil {
				latestRoundFindings = &f
			}
		}
	}

	hadFindings := initialFindings != nil && len(initialFindings.Items) > 0
	hasFinalFindings := finalFindings != nil && len(finalFindings.Items) > 0
	hasAnyRoundFindings := roundsHaveFindings(rounds)
	hasRoundParseFailure := roundsHaveParseFailure(rounds)
	hadAnyFindings := hadFindings || hasFinalFindings || hasAnyRoundFindings
	hasUnreadableFinalFindings := sr.FindingsJSON != nil && !finalFindingsParsed
	wasFixed := hadFindings && len(rounds) > 1 && !hasUnreadableFinalFindings && !hasFinalFindings
	riskLevel := ""
	if sr.StepName == types.StepReview {
		src := finalFindings
		if src == nil && !hasUnreadableFinalFindings {
			src = latestRoundFindings
		}
		if src != nil {
			riskLevel = src.RiskLevel
		}
	}

	// Unreadable final findings - can't make claims about the outcome.
	if hasUnreadableFinalFindings {
		return buildDetail(fmt.Sprintf("⚠️ **%s** - findings unavailable", name))
	}

	if sr.StepName == types.StepReview && (riskLevel == "medium" || riskLevel == "high") && !hadAnyFindings {
		return buildDetail(fmt.Sprintf("%s **%s** - %s risk", riskEmoji(riskLevel), name, riskLevel))
	}

	if !hadAnyFindings && !hasRoundParseFailure {
		if len(rounds) == 0 {
			return buildDetail(fmt.Sprintf("⚠️ **%s** - findings unavailable", name))
		}
		return buildDetail(fmt.Sprintf("✅ **%s** - passed", name))
	}

	if hasRoundParseFailure && !hadAnyFindings {
		return buildDetail(fmt.Sprintf("⚠️ **%s** - findings unavailable", name))
	}

	if wasFixed {
		result := buildFixResultText(rounds)
		line := fmt.Sprintf("🔧 **%s** - %s ✅", name, result)
		return buildDetail(line)
	}

	currentFindings := initialFindings
	if hasFinalFindings {
		currentFindings = finalFindings
	}

	// Had findings and the final state still contains them - approved as-is.
	count := countFindingsBySeverity(currentFindings)
	line := fmt.Sprintf("⚠️ **%s** - %s", name, count)
	return buildDetail(line)
}

func extractRiskLine(steps []*db.StepResult, rounds map[string][]*db.StepRound) string {
	for _, sr := range steps {
		if sr.StepName != types.StepReview {
			continue
		}

		var finalFindings *types.Findings
		hasUnreadableFinal := false
		if sr.FindingsJSON != nil {
			if f, err := types.ParseFindingsJSON(*sr.FindingsJSON); err == nil {
				finalFindings = &f
			} else {
				hasUnreadableFinal = true
			}
		}

		src := finalFindings
		if src == nil && !hasUnreadableFinal {
			stepRounds := rounds[sr.ID]
			if len(stepRounds) > 0 {
				last := stepRounds[len(stepRounds)-1]
				if last.FindingsJSON != nil {
					if f, err := types.ParseFindingsJSON(*last.FindingsJSON); err == nil {
						src = &f
					}
				}
			}
		}

		if src == nil || src.RiskLevel == "" {
			return ""
		}

		emoji := riskEmoji(src.RiskLevel)
		label := capitalizeRisk(src.RiskLevel)
		if src.RiskRationale != "" {
			return fmt.Sprintf("%s %s: %s", emoji, label, src.RiskRationale)
		}
		return fmt.Sprintf("%s %s", emoji, label)
	}
	return ""
}

func capitalizeRisk(level string) string {
	if level == "" {
		return level
	}
	return strings.ToUpper(level[:1]) + level[1:]
}

func riskEmoji(level string) string {
	switch level {
	case "low":
		return "✅"
	case "medium":
		return "⚠️"
	case "high":
		return "🚨"
	default:
		return "ℹ️"
	}
}

func roundsHaveFindings(rounds []*db.StepRound) bool {
	for _, r := range rounds {
		if r.FindingsJSON == nil {
			continue
		}
		f, err := types.ParseFindingsJSON(*r.FindingsJSON)
		if err != nil {
			continue
		}
		if len(f.Items) > 0 {
			return true
		}
	}

	return false
}

func roundsHaveParseFailure(rounds []*db.StepRound) bool {
	for _, r := range rounds {
		if r.FindingsJSON == nil {
			continue
		}
		if _, err := types.ParseFindingsJSON(*r.FindingsJSON); err != nil {
			return true
		}
	}

	return false
}

func buildFixResultText(rounds []*db.StepRound) string {
	// Count findings in round 1.
	var initialCount int
	if len(rounds) > 0 && rounds[0].FindingsJSON != nil {
		if f, err := types.ParseFindingsJSON(*rounds[0].FindingsJSON); err == nil {
			initialCount = len(f.Items)
		}
	}

	// Categorize fix rounds. Legacy "user_fix" rounds are rendered as auto-fix.
	autoFixRounds := 0
	for _, r := range rounds[1:] {
		if r.IsFixRound() {
			autoFixRounds++
		}
	}

	noun := "issue"
	if initialCount != 1 {
		noun = "issues"
	}

	parts := []string{fmt.Sprintf("%d %s found", initialCount, noun)}

	if autoFixRounds > 1 {
		parts = append(parts, fmt.Sprintf("auto-fixed (%d)", autoFixRounds))
	} else if autoFixRounds == 1 {
		parts = append(parts, "auto-fixed")
	}

	return strings.Join(parts, " → ")
}

// buildStepDetails renders the collapsible body for a step as an
// issue -> fix -> outcome narrative rather than a round-by-round log. Each
// round is shown as the review state observed at its end; a fix round is
// prefixed with the fix the agent applied (its commit summary) so a reader can
// see what was wrong and what was done about it without mentally replaying
// "rounds".
func buildStepDetails(summaryLine string, sr *db.StepResult, rounds []*db.StepRound) string {
	var b strings.Builder
	b.WriteString("<details>\n")
	b.WriteString(fmt.Sprintf("<summary>%s</summary>\n\n", summaryLine))

	if len(rounds) == 0 {
		writeStepStatusDetail(&b, sr)
		b.WriteString("</details>\n")
		return b.String()
	}

	// True only when the step recorded final findings but no round captured
	// them - a data gap we must not paper over as "no issues found".
	missingRoundFindingsData := sr.FindingsJSON != nil && !roundsHaveFindings(rounds) && !roundsHaveParseFailure(rounds)

	for _, r := range rounds {
		isFixRound := r.IsFixRound()
		if isFixRound {
			b.WriteString(fixRoundLine(r))
			b.WriteString("\n")
		}

		if r.FindingsJSON == nil {
			switch {
			case missingRoundFindingsData:
				b.WriteString("findings not recorded\n\n")
			case isFixRound:
				b.WriteString("✅ Re-checked - no issues remain.\n\n")
			default:
				b.WriteString("✅ No issues found.\n\n")
			}
			continue
		}

		findings, err := types.ParseFindingsJSON(*r.FindingsJSON)
		if err != nil {
			b.WriteString("failed to parse findings\n\n")
			continue
		}

		if len(findings.Items) == 0 {
			if isFixRound {
				b.WriteString("✅ Re-checked - no issues remain.\n")
			} else {
				b.WriteString("✅ No issues found.\n")
			}
			writeTestedDetails(&b, sr, &findings)
			b.WriteString("\n")
			continue
		}

		// A fix round that still has findings means the fix did not fully
		// land; label what remained so the chain reads as fix -> still open.
		if isFixRound {
			b.WriteString(fmt.Sprintf("%s still open:\n", countFindingsBySeverity(&findings)))
		}
		writeFindingItems(&b, sr, &findings)
		b.WriteString("\n")
	}

	b.WriteString("</details>\n")
	return b.String()
}

// fixRoundLine renders the one-line summary of the fix the agent applied in a
// fix round, falling back to a generic note when no summary was captured.
func fixRoundLine(r *db.StepRound) string {
	summary := ""
	if r.FixSummary != nil {
		summary = strings.TrimSpace(*r.FixSummary)
	}
	if summary == "" {
		return "🔧 Fix applied."
	}
	return fmt.Sprintf("🔧 Fix: %s", html.EscapeString(summary))
}

// writeFindingItems renders each finding as a `file:line - description` bullet,
// followed by any test command details for the test step.
func writeFindingItems(b *strings.Builder, sr *db.StepResult, findings *types.Findings) {
	for _, f := range findings.Items {
		emoji := severityEmoji(f.Severity)
		loc := ""
		if f.File != "" {
			loc = fmt.Sprintf("`%s", html.EscapeString(f.File))
			if f.Line > 0 {
				loc += fmt.Sprintf(":%d", f.Line)
			}
			loc += "` - "
		}
		b.WriteString(fmt.Sprintf("- %s %s%s\n", emoji, loc, html.EscapeString(f.Description)))
	}
	writeTestedDetails(b, sr, findings)
}

// writeTestedDetails lists the commands the test step exercised. It is a no-op
// for non-test steps.
func writeTestedDetails(b *strings.Builder, sr *db.StepResult, findings *types.Findings) {
	if sr.StepName != types.StepTest {
		return
	}
	for _, detail := range findings.Tested {
		rendered := renderTestedDetail(detail)
		if rendered == "" {
			continue
		}
		b.WriteString(fmt.Sprintf("- %s\n", rendered))
	}
}

func writeStepStatusDetail(b *strings.Builder, sr *db.StepResult) {
	switch sr.Status {
	case types.StepStatusPending:
		b.WriteString("Step has not started yet.\n\n")
	case types.StepStatusRunning:
		b.WriteString("Step is currently running.\n\n")
	case types.StepStatusAwaitingApproval:
		b.WriteString("Waiting for user approval.\n\n")
	case types.StepStatusFixing:
		b.WriteString("Agent is currently applying fixes.\n\n")
	case types.StepStatusFixReview:
		b.WriteString("Waiting to review the latest fix.\n\n")
	case types.StepStatusSkipped:
		b.WriteString("Step was skipped.\n\n")
	case types.StepStatusFailed:
		if sr.Error != nil && strings.TrimSpace(*sr.Error) != "" {
			b.WriteString(html.EscapeString(strings.TrimSpace(*sr.Error)))
			b.WriteString("\n\n")
			return
		}
		b.WriteString("Step failed.\n\n")
	case types.StepStatusCompleted:
		b.WriteString("No round details recorded.\n\n")
	default:
		b.WriteString("Status unavailable.\n\n")
	}
}

func shouldOmitPipelineStep(sr *db.StepResult) bool {
	if sr == nil {
		return false
	}

	return sr.StepName == types.StepPR || sr.StepName == types.StepCI
}

func countFindingsBySeverity(findings *types.Findings) string {
	if findings == nil || len(findings.Items) == 0 {
		return "0 issues"
	}

	counts := map[string]int{}
	for _, f := range findings.Items {
		counts[f.Severity]++
	}

	total := len(findings.Items)
	noun := "issue"
	if total != 1 {
		noun = "issues"
	}

	// If all same severity, just show count + severity.
	if len(counts) == 1 {
		for sev, n := range counts {
			noun := sev
			if n != 1 {
				noun += "s"
			}
			return fmt.Sprintf("%d %s", n, noun)
		}
	}

	// Mixed severities: "3 issues (1 error, 2 warnings)"
	var parts []string
	for _, sev := range []string{"error", "warning", "info"} {
		if n, ok := counts[sev]; ok {
			label := sev
			if n != 1 {
				label += "s"
			}
			parts = append(parts, fmt.Sprintf("%d %s", n, label))
		}
	}
	return fmt.Sprintf("%d %s (%s)", total, noun, strings.Join(parts, ", "))
}

func severityEmoji(severity string) string {
	switch severity {
	case "error":
		return "🚨"
	case "warning":
		return "⚠️"
	case "info":
		return "ℹ️"
	default:
		return "-"
	}
}

func stepDisplayName(name types.StepName) string {
	switch name {
	case types.StepRebase:
		return "Rebase"
	case types.StepReview:
		return "Review"
	case types.StepTest:
		return "Test"
	case types.StepDocument:
		return "Document"
	case types.StepLint:
		return "Lint"
	case types.StepPush:
		return "Push"
	case types.StepPR:
		return "PR"
	case types.StepCI:
		return "CI"
	default:
		return string(name)
	}
}
