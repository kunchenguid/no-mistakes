package intent

import (
	"path/filepath"
	"strings"
)

// Match is the chosen session along with its overlap score.
type Match struct {
	Session *Session
	Score   float64
	// Overlap lists diff files that appeared in this session. Used purely
	// for diagnostics/telemetry.
	Overlap []string
}

// score computes the share of diff files that appear anywhere in the
// session's messages. The score is symmetric in the sense that a session
// that touched many extra unrelated files is not penalized - only the
// portion of *this* diff that the session covered matters.
func score(s *Session, diffFiles []string) (float64, []string) {
	if len(diffFiles) == 0 || len(s.Messages) == 0 {
		return 0, nil
	}

	mentioned := make(map[string]bool)
	for _, m := range s.Messages {
		for _, p := range m.FilePaths {
			for _, n := range normalizedPathVariants(p) {
				mentioned[n] = true
			}
		}
		// Best-effort scan of assistant text for raw filenames.
		for _, p := range scanFilePathsInText(m.Text) {
			for _, n := range normalizedPathVariants(p) {
				mentioned[n] = true
			}
		}
	}

	var overlap []string
	for _, f := range diffFiles {
		for _, n := range normalizedPathVariants(f) {
			if mentioned[n] {
				overlap = append(overlap, f)
				break
			}
		}
	}
	return float64(len(overlap)) / float64(len(diffFiles)), overlap
}

// pickMatch returns the highest-scoring session at or above the threshold.
// Ties are broken by most recent LastActivity.
func pickMatch(sessions []*Session, diffFiles []string, threshold float64) *Match {
	var best *Match
	for _, s := range sessions {
		sc, overlap := score(s, diffFiles)
		if sc < threshold {
			continue
		}
		if best == nil || sc > best.Score ||
			(sc == best.Score && s.LastActivity.After(best.Session.LastActivity)) {
			best = &Match{Session: s, Score: sc, Overlap: overlap}
		}
	}
	return best
}

// normalizedPathVariants returns a small set of normalized forms for a
// path so that absolute, repo-relative, and basename mentions can match.
func normalizedPathVariants(p string) []string {
	if p == "" {
		return nil
	}
	cleaned := filepath.ToSlash(filepath.Clean(strings.TrimSpace(p)))
	cleaned = strings.TrimPrefix(cleaned, "./")
	variants := map[string]bool{cleaned: true}
	if base := filepath.Base(cleaned); base != "" && base != "." {
		variants[base] = true
	}
	out := make([]string, 0, len(variants))
	for v := range variants {
		out = append(out, v)
	}
	return out
}

// scanFilePathsInText extracts plausible file paths from prose. The regex
// is intentionally permissive - false positives don't matter because the
// matcher only treats them as candidates, not ground truth.
func scanFilePathsInText(text string) []string {
	if text == "" {
		return nil
	}
	var out []string
	for _, tok := range filePathTokens.FindAllString(text, -1) {
		tok = strings.Trim(tok, "\"'`,;:()[]{}<>")
		if tok == "" {
			continue
		}
		// Require an extension or path separator to avoid matching prose words.
		if strings.ContainsAny(tok, "/\\") || strings.Contains(tok, ".") {
			out = append(out, tok)
		}
	}
	return out
}
