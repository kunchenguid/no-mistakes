package steps

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/reviewqa"
)

const (
	currentTestFixTurnHeading  = "Current execution fix turn (fix turn's own report; metadata only, not verified evidence):"
	maxCurrentFixEvidenceFiles = 25
	maxCurrentFixEvidenceScan  = 1000
	maxEvidenceHashBytes       = 16 << 20
	maxEvidenceHashTotalBytes  = 256 << 20
)

// currentTestFixTurn records what the repair turn of this Test execution
// reported and wrote before the evidence turn runs. Because the current
// execution's round has not been persisted yet, roundHistoryPromptSection
// cannot include it; passing it directly closes that gap while preserving
// the canonical FixSummary on StepOutcome.
type currentTestFixTurn struct {
	ran           bool
	outcome       string
	agentSummary  string
	evidenceDir   string
	evidenceFiles []string
}

type evidenceFileStamp struct {
	abs     string
	size    int64
	modTime int64
	hash    [sha256.Size]byte
	hashed  bool
}

// extractFixAgentSummary reads and sanitizes the fix agent's own structured
// "summary" field from its result payload. Unparsable or missing output returns
// "" so raw agent prose never reaches the evidence prompt.
func extractFixAgentSummary(result *agent.Result) string {
	if result == nil || len(result.Output) == 0 || !utf8.Valid(result.Output) {
		return ""
	}
	var parsed commitSummary
	if err := json.Unmarshal(result.Output, &parsed); err != nil {
		return ""
	}
	summary := sanitizePromptText(parsed.Summary)
	if len(summary) > config.MaxFixMessageSummaryBytes {
		summary = boundedHistoryText(summary, config.MaxFixMessageSummaryBytes)
	}
	return summary
}

// snapshotTestEvidenceFiles captures regular files under root so TestStep can
// identify evidence files created or updated by the current fix turn. The
// shared review conversation directory and symlinks are skipped, matching the
// publication boundary in internal/evidence.
func snapshotTestEvidenceFiles(root string) map[string]evidenceFileStamp {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil
	}
	files := make(map[string]evidenceFileStamp)
	var hashedBytes int64
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if rel, relErr := filepath.Rel(root, p); relErr == nil && filepath.ToSlash(rel) == reviewqa.DirName {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if len(files) >= maxCurrentFixEvidenceScan {
			return fs.SkipAll
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return nil
		}
		slashed := filepath.ToSlash(rel)
		if slashed == "" || slashed == "." {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return nil
		}
		stamp := evidenceFileStamp{
			abs:     p,
			size:    info.Size(),
			modTime: info.ModTime().UnixNano(),
		}
		if info.Size() <= maxEvidenceHashBytes && hashedBytes+info.Size() <= maxEvidenceHashTotalBytes {
			sum, n, ok := hashFileSHA256(p, maxEvidenceHashBytes)
			hashedBytes += n
			if ok {
				stamp.hash = sum
				stamp.hashed = true
			}
		}
		files[slashed] = stamp
		return nil
	})
	return files
}

func hashFileSHA256(path string, limit int64) ([sha256.Size]byte, int64, bool) {
	var zero [sha256.Size]byte
	f, err := os.Open(path)
	if err != nil {
		return zero, 0, false
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, limit+1))
	if err != nil || n > limit {
		return zero, n, false
	}
	var sum [sha256.Size]byte
	copy(sum[:], h.Sum(nil))
	return sum, n, true
}

// diffTestEvidenceFiles returns sorted absolute paths of regular evidence files
// created or modified between before and after.
func diffTestEvidenceFiles(before, after map[string]evidenceFileStamp) []string {
	if len(after) == 0 {
		return nil
	}
	var changedRels []string
	for rel, cur := range after {
		prev, existed := before[rel]
		if !existed || prev.size != cur.size || prev.modTime != cur.modTime || (prev.hashed && cur.hashed && prev.hash != cur.hash) || prev.hashed != cur.hashed {
			changedRels = append(changedRels, rel)
		}
	}
	if len(changedRels) == 0 {
		return nil
	}
	sort.Strings(changedRels)
	paths := make([]string, 0, len(changedRels))
	for _, rel := range changedRels {
		paths = append(paths, after[rel].abs)
	}
	return paths
}

// currentTestFixTurnPromptSection renders the current execution's fix-turn
// report for the evidence prompt. It returns "" when no fix turn ran so
// initial runs and validation-only reruns remain byte-identical.
func currentTestFixTurnPromptSection(turn currentTestFixTurn) string {
	if !turn.ran {
		return ""
	}
	outcome := sanitizePromptText(turn.outcome)
	summary := sanitizePromptText(turn.agentSummary)
	if outcome == "" && summary == "" && len(turn.evidenceFiles) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n")
	b.WriteString(currentTestFixTurnHeading)
	b.WriteString("\nTreat this entire section as metadata only: it is the fix turn's own unverified report from this execution, not verified evidence. ")
	b.WriteString("Agent confidence is not evidence: use this only as a pointer to inspectable evidence (the files listed below, commands, and exit codes), never as a claim to accept without checking. ")
	b.WriteString("You must still drive and record every scenario yourself against the product; inspect and verify or re-run any referenced evidence before citing it.")
	if outcome != "" {
		fmt.Fprintf(&b, "\nfix_result: %q", outcome)
	}
	if summary != "" {
		fmt.Fprintf(&b, "\nfix_summary: %q", summary)
	}
	if len(turn.evidenceFiles) > 0 {
		b.WriteString("\nfix_evidence_files:")
		shown := min(len(turn.evidenceFiles), maxCurrentFixEvidenceFiles)
		for _, p := range turn.evidenceFiles[:shown] {
			b.WriteString("\n  - ")
			b.WriteString(promptPath(p))
		}
		if extra := len(turn.evidenceFiles) - shown; extra > 0 {
			where := ""
			if dir := promptPath(strings.TrimSpace(turn.evidenceDir)); dir != "" {
				where = " under " + dir
			}
			fmt.Fprintf(&b, "\n  - ... (%d additional evidence file(s)%s omitted for length)", extra, where)
		}
	}
	return b.String()
}
