package intent

import (
	"path/filepath"
	"strings"
	"time"
)

// Match is the chosen session along with its overlap score.
type Match struct {
	Session *Session
	Score   float64
	// Confidence is the score used to rank accepted candidates after applying
	// recency. Score remains the raw file-overlap score surfaced to callers.
	Confidence float64
	// Overlap lists diff files that appeared in this session. Used purely
	// for diagnostics/telemetry.
	Overlap []string
}

type matchOptions struct {
	Threshold float64
	HeadTime  time.Time
	Logf      func(format string, args ...any)
}

// score computes the share of diff files that appear anywhere in the
// session's messages. The score is symmetric in the sense that a session
// that touched many extra unrelated files is not penalized - only the
// portion of *this* diff that the session covered matters.
func score(s *Session, diffFiles []string) (float64, []string) {
	if len(diffFiles) == 0 || len(s.Messages) == 0 {
		return 0, nil
	}

	var mentioned []string
	for _, m := range s.Messages {
		for _, p := range m.FilePaths {
			mentioned = append(mentioned, p)
		}
		// Best-effort scan of assistant text for raw filenames.
		mentioned = append(mentioned, scanFilePathsInText(m.Text)...)
	}

	var overlap []string
	for _, f := range diffFiles {
		for _, p := range mentioned {
			if pathMentionMatchesDiff(p, f) {
				overlap = append(overlap, f)
				break
			}
		}
	}
	return float64(len(overlap)) / float64(len(diffFiles)), overlap
}

func pathMentionMatchesDiff(mention, diffFile string) bool {
	mention = filepath.ToSlash(filepath.Clean(strings.TrimSpace(mention)))
	mention = strings.TrimPrefix(mention, "./")
	diffFile = filepath.ToSlash(filepath.Clean(strings.TrimSpace(diffFile)))
	diffFile = strings.TrimPrefix(diffFile, "./")
	if mention == "" || diffFile == "" || mention == "." || diffFile == "." {
		return false
	}
	if mention == diffFile || strings.HasSuffix(mention, "/"+diffFile) {
		return true
	}
	// Basename-only mentions are useful, but pathful mentions should not match
	// unrelated files that happen to share names like update.go.
	return !strings.Contains(mention, "/") && filepath.Base(diffFile) == mention
}

// pickMatch returns the accepted session with the highest confidence score.
// Acceptance applies the raw threshold, multi-file overlap, and stale-partial
// rules; confidence applies a recency boost, with ties broken by LastActivity.
func pickMatch(sessions []*Session, diffFiles []string, threshold float64) *Match {
	return pickMatchWithOptions(sessions, diffFiles, matchOptions{Threshold: threshold})
}

func pickMatchWithOptions(sessions []*Session, diffFiles []string, opts matchOptions) *Match {
	var best *Match
	for _, s := range sessions {
		sc, overlap := score(s, diffFiles)
		accepted, reason, confidence := acceptMatchCandidate(sc, len(overlap), len(diffFiles), s.LastActivity, opts)
		if opts.Logf != nil {
			decision := "accepted"
			if !accepted {
				decision = "rejected"
			}
			opts.Logf("candidate agent=%s session=%s cwd=%q score %.2f confidence %.2f overlap=%d/%d decision=%s reason=%s",
				s.AgentName, s.SessionID, s.CWD, sc, confidence, len(overlap), len(diffFiles), decision, reason)
		}
		if !accepted {
			continue
		}
		if best == nil || confidence > best.Confidence ||
			(confidence == best.Confidence && s.LastActivity.After(best.Session.LastActivity)) {
			best = &Match{Session: s, Score: sc, Confidence: confidence, Overlap: overlap}
		}
	}
	return best
}

func acceptMatchCandidate(score float64, overlapCount, diffCount int, lastActivity time.Time, opts matchOptions) (bool, string, float64) {
	if diffCount == 0 || overlapCount == 0 {
		return false, "no_overlap", score
	}
	threshold := opts.Threshold
	if diffCount > 1 && threshold < 0.5 {
		threshold = 0.5
	}
	if diffCount > 1 && overlapCount < 2 {
		return false, "single_overlap_multi_file_diff", score
	}
	if score < threshold {
		return false, "below_threshold", score
	}
	confidence := score + recencyBoost(opts.HeadTime, lastActivity)
	if !opts.HeadTime.IsZero() && lastActivity.Before(opts.HeadTime.Add(-24*time.Hour)) && score < 0.8 {
		return false, "stale_partial", confidence
	}
	return true, "matched", confidence
}

func recencyBoost(headTime, lastActivity time.Time) float64 {
	if headTime.IsZero() || lastActivity.IsZero() {
		return 0
	}
	age := headTime.Sub(lastActivity)
	if age < 0 {
		age = -age
	}
	if age <= 2*time.Hour {
		return 0.15
	}
	if age <= 24*time.Hour {
		return 0.05
	}
	return 0
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
