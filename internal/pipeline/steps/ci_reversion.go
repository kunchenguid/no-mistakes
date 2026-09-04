package steps

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Decision-reversion guard for the CI auto-fix round.
//
// The CI fix agent reasons about a red check as breakage to repair. When the red
// state instead means "a human decision is outstanding", the cheapest repair it
// can find is often to undo the decision: delete the content a guard was
// supposed to cover until the guard's stale evidence fits again, or put back
// code a recorded decision removed. Both were observed on 2026-08-27 - see the
// package tests, which reproduce AGFloorPlanner PR 2891 (commit 674f728c) and
// kunchenguid/firstmate PR 3098 (commit faca4c93).
//
// No wording of the failure message prevents this, because the agent reasons
// about failures rather than pattern-matching them, so the control has to be
// structural. Two things make it possible for such a repair to succeed quietly:
//
//   - The reversion is INVISIBLE downstream. Review, and every consumer of the
//     branch, reads the base..head diff. Undoing the branch's own work leaves
//     that diff looking exactly as it did before the branch made the change, so
//     the re-review after a CI repair has nothing to see. The evidence exists
//     only in the pre-repair..post-repair range, which nothing inspected.
//   - The repair is committed by the pipeline itself, so no human ever compares
//     it against what the branch deliberately decided.
//
// This guard therefore looks at exactly that missing range, before the repair is
// committed, and reports two deterministic, zero-LLM signals:
//
//	restored file   the repair returns a file the branch changed to byte-identical
//	                pre-branch content. The branch's whole contribution to that
//	                file is erased, whatever the repair's stated reason.
//	reinstated line the repair puts back a line that exists exactly once in the
//	                pre-branch file and that the branch removed entirely.
//
// Both compare against the run's base commit, so an ordinary repair - modifying
// a line the branch itself added, or adding new content - restores nothing of
// the pre-branch state and never trips the guard. That precision is the point:
// a guard that parks on every CI repair would be turned off.
//
// Uniqueness in the base file is what keeps the line signal honest. A repair
// that happens to write a `}` or a `return nil` that also appears in the base
// file proves nothing; a line the base file contains exactly once, which the
// branch removed and the repair restored, is a reversion of a specific decision.
//
// The response is to refuse the commit and park for a person, never to repair
// the repair. The proposed changes stay in the run worktree, uncommitted and
// unpushed, so nothing is lost and a person decides.

// maxReversionEvidenceFiles bounds how many files a refusal RENDERS, and
// maxReversionEvidenceLines how many restored lines it quotes per file. The
// evidence is a decision aid, not a diff: a person who needs the rest reads the
// worktree the finding names, and is told how much was left out.
//
// These are display bounds only. A refusal always carries the complete finding,
// because that is what one fix response is checked against.
const (
	maxReversionEvidenceFiles = 20
	maxReversionEvidenceLines = 5
	maxReversionEvidenceWidth = 160
)

// reversionKind names which of the two signals a file tripped.
type reversionKind string

const (
	reversionRestoredFile   reversionKind = "restored to its pre-branch content"
	reversionReinstatedText reversionKind = "reinstates pre-branch content the branch removed"
)

// reversionEvidence is one file's proof that the proposed repair undoes work the
// branch deliberately did. Lines carries the reinstated content, bounded for
// display by boundForDisplay - which is also what a refusal may authorise, so
// the two can never drift apart.
type reversionEvidence struct {
	Path        string
	Kind        reversionKind
	Lines       []string
	elidedLines int
}

func (ev reversionEvidence) equal(other reversionEvidence) bool {
	if ev.Path != other.Path || ev.Kind != other.Kind || len(ev.Lines) != len(other.Lines) {
		return false
	}
	for i := range ev.Lines {
		if ev.Lines[i] != other.Lines[i] {
			return false
		}
	}
	return true
}

// decisionReversionError refuses a CI repair. It carries either per-file
// evidence of a reversion, or the reason the guard could not be evaluated: an
// unevaluable guard fails closed, because silent success is the exact harm this
// exists to prevent, and a park costs one decision while a missed reversion
// costs the decision itself.
type decisionReversionError struct {
	evidence []reversionEvidence
	reason   string
	// truncated records that the finding was larger than a refusal can honestly
	// show. Such a refusal can be read, but never authorised: see authorizes.
	truncated   bool
	elidedFiles int
}

// newReversionRefusal bounds a complete finding down to what a refusal shows,
// and marks it truncated when anything had to be left out.
//
// Bounding here, once, is what keeps the displayed refusal and the authorisable
// refusal the same object. Every earlier shape of this code kept a fuller
// finding behind the displayed one, and every one of them had the same defect in
// a different place: a person authorising content the refusal only summarised.
func newReversionRefusal(evidence []reversionEvidence) *decisionReversionError {
	refusal := &decisionReversionError{}
	if len(evidence) > maxReversionEvidenceFiles {
		refusal.elidedFiles = len(evidence) - maxReversionEvidenceFiles
		evidence = evidence[:maxReversionEvidenceFiles]
		refusal.truncated = true
	}
	for _, ev := range evidence {
		bounded := reversionEvidence{Path: ev.Path, Kind: ev.Kind}
		lines := ev.Lines
		if len(lines) > maxReversionEvidenceLines {
			bounded.elidedLines = len(lines) - maxReversionEvidenceLines
			lines = lines[:maxReversionEvidenceLines]
			refusal.truncated = true
		}
		for _, line := range lines {
			clamped := clampEvidenceLine(line)
			if clamped != strings.TrimSpace(line) {
				refusal.truncated = true
			}
			bounded.Lines = append(bounded.Lines, clamped)
		}
		refusal.evidence = append(refusal.evidence, bounded)
	}
	return refusal
}

func (e *decisionReversionError) Error() string {
	if e.reason != "" {
		return "CI repair refused: the decision-reversion guard could not be evaluated: " + e.reason
	}
	var b strings.Builder
	b.WriteString("CI repair refused: it undoes work this branch deliberately did")
	for _, ev := range e.evidence {
		b.WriteString("\n- ")
		b.WriteString(ev.Path)
		b.WriteString(": ")
		b.WriteString(string(ev.Kind))
		for _, line := range ev.Lines {
			b.WriteString("\n    + ")
			b.WriteString(line)
		}
		if ev.elidedLines > 0 {
			b.WriteString(fmt.Sprintf("\n    + and %d more %s", ev.elidedLines, plural(ev.elidedLines, "line")))
		}
	}
	if e.elidedFiles > 0 {
		b.WriteString(fmt.Sprintf("\n- and %d more %s", e.elidedFiles, plural(e.elidedFiles, "file")))
	}
	return b.String()
}

func plural(n int, noun string) string {
	if n == 1 {
		return noun
	}
	return noun + "s"
}

// authorizes reports whether a person's fix response to the receiver also covers
// a later refusal.
//
// The rule is deliberately one line of meaning: a refusal authorises exactly
// what it showed, and nothing else. That is the only rule under which the gate's
// promise - "you are deciding on this" - is literally true.
//
// It rejects a truncated refusal outright. A finding too large to display in
// full cannot be authorised from a summary of it, and the alternative -
// authorising against a complete set held behind the display - is how a person
// comes to approve files and lines they were shown only as a count.
//
// It also rejects a NARROWER later refusal, which costs one extra gate on a rare
// path. Every attempt here to be accommodating about "less than you approved"
// grew a way for something new to ride along with it; showing the person what
// actually happened is worth more than saving them a keystroke.
//
// A nil receiver authorises nothing: without a refusal a person actually read,
// there is no decision to honour. An unevaluable guard is authorised only by the
// identical reason - the missing evidence someone chose to proceed without keeps
// the guard unevaluable, so that case has to be able to move forward, while a
// replacement turn that breaks the guard some other way is new information.
//
// What is authorised is the REVERSION, not the repair. The replacement turn
// writes other content too, and always could; that content is not waved through
// with it, because a committed repair clears the review approval and restarts
// validation from Review, so everything the turn wrote is reviewed on its
// merits. This guard exists only for the one thing that pass cannot see - work
// the branch itself undid, which leaves base..head looking untouched.
//
// Content below the evidence threshold (see reinstatedBaseLines) is therefore
// deliberately absent from the identity as well as from the display. Putting it
// in one but not the other is precisely the split this rule removed: a person
// cannot authorise what a refusal never showed them, and a refusal that showed
// it would fire on every ordinary repair and be turned off.
func (e *decisionReversionError) authorizes(later *decisionReversionError) bool {
	if e == nil || later == nil {
		return false
	}
	if later.reason != "" || e.reason != "" {
		return e.reason == later.reason
	}
	if e.truncated || later.truncated {
		return false
	}
	if len(e.evidence) != len(later.evidence) {
		return false
	}
	for i := range e.evidence {
		if !e.evidence[i].equal(later.evidence[i]) {
			return false
		}
	}
	return true
}

// detectDecisionReversion compares the repair's proposed state - the run
// worktree - against the head the branch had before the fix round started.
//
// The comparison is anchored on preHeadSHA rather than on HEAD because a fix
// agent is free to commit its own work: with HEAD as the anchor, a repair that
// committed itself would compare clean against its own commit and the guard
// would see nothing at all. Every path into commitRepair - a repair the pipeline
// commits, and one the agent already committed - is measured from the same
// pre-repair anchor.
//
// It runs before anything is staged, so a refusal leaves the branch head, the
// index, and the recorded run state untouched.
func detectDecisionReversion(sctx *pipeline.StepContext, baseSHA, preHeadSHA string) ([]reversionEvidence, error) {
	baseSHA = strings.TrimSpace(baseSHA)
	if baseSHA == "" {
		return nil, fmt.Errorf("the run records no base commit to compare the repair against")
	}
	if _, err := stepGitRun(sctx, "rev-parse", "--verify", "--quiet", baseSHA+"^{commit}"); err != nil {
		return nil, fmt.Errorf("base commit %s is not readable in the run worktree", baseSHA)
	}
	preHeadSHA = strings.TrimSpace(preHeadSHA)
	if preHeadSHA == "" {
		return nil, fmt.Errorf("the run records no head commit to compare the repair against")
	}
	if _, err := stepGitRun(sctx, "rev-parse", "--verify", "--quiet", preHeadSHA+"^{commit}"); err != nil {
		return nil, fmt.Errorf("pre-repair head %s is not readable in the run worktree", preHeadSHA)
	}
	// The fork point, not the recorded base, is what "pre-branch content" means.
	// runs.base_sha can name a base-branch commit the branch was never built on
	// - a rebase, or a base that advanced during the run - and comparing against
	// that would call an upstream change the branch never made "the branch's
	// own work". A merge-base failure is not fatal: the recorded base is still
	// the best answer available, and it errs towards asking a person.
	if forkPoint, err := stepGitRun(sctx, "merge-base", baseSHA, preHeadSHA); err == nil && forkPoint != "" {
		baseSHA = forkPoint
	}

	repaired, err := repairChangedPaths(sctx, preHeadSHA)
	if err != nil {
		return nil, err
	}
	if len(repaired) == 0 {
		return nil, nil
	}
	// Only the branch's own contribution is protected, so the branch's diff is
	// both the candidate set and the source of truth for what each file looked
	// like before and after the branch touched it. One `diff --raw` reports the
	// status, both blob ids and both file modes together, which is what makes
	// "absent" unambiguous: it is a zero mode, never a git call that failed.
	branchChanges, err := branchChangedFiles(sctx, baseSHA, preHeadSHA)
	if err != nil {
		return nil, err
	}
	// "Is this file back to its pre-branch content?" is asked of git rather than
	// answered by comparing the worktree's bytes to the blob's. Git applies the
	// same checkout and clean filters to both sides, so the answer holds where a
	// filter is configured - on a Windows daemon with core.autocrlf, a byte
	// comparison would find every text file different from its blob and the
	// signal would silently never fire.
	differsFromBase, err := worktreePathsDifferingFromBase(sctx, baseSHA, repaired)
	if err != nil {
		return nil, err
	}

	var evidence []reversionEvidence
	for _, path := range repaired {
		change, ok := branchChanges[path]
		if !ok {
			// The branch never touched this file, so the repair cannot be
			// undoing branch work in it.
			continue
		}
		if !differsFromBase[path] {
			evidence = append(evidence, reversionEvidence{Path: path, Kind: reversionRestoredFile})
			continue
		}
		if !change.comparable() {
			// A submodule pointer or any other non-blob entry has no text to
			// line up. Nothing here can prove a reinstatement either way.
			continue
		}
		base, err := blobContent(sctx, change.baseBlob)
		if err != nil {
			return nil, err
		}
		pre, err := blobContent(sctx, change.headBlob)
		if err != nil {
			return nil, err
		}
		post, _, err := worktreeFileContent(sctx.WorkDir, path)
		if err != nil {
			return nil, err
		}
		if lines := reinstatedBaseLines(base, pre, post); len(lines) > 0 {
			evidence = append(evidence, reversionEvidence{Path: path, Kind: reversionReinstatedText, Lines: lines})
		}
	}
	// Deliberately NOT truncated to the display cap: the full set is the
	// authorisation identity, and Error/reversionDetail bound what is rendered.
	sort.Slice(evidence, func(i, j int) bool { return evidence[i].Path < evidence[j].Path })
	return evidence, nil
}

// branchChange is one file's before/after identity across the branch's own work.
// A zero mode means the file did not exist on that side.
type branchChange struct {
	baseMode string
	headMode string
	baseBlob string
	headBlob string
}

const gitAbsentMode = "000000"

// comparable reports whether both sides are ordinary files or symlinks, the only
// entries with content to line up. Git spells those 100644, 100755 and 120000; a
// 160000 submodule pointer has none.
func (c branchChange) comparable() bool {
	for _, mode := range []string{c.baseMode, c.headMode} {
		switch mode {
		case gitAbsentMode, "100644", "100755", "120000":
		default:
			return false
		}
	}
	return true
}

// branchChangedFiles reports every file the branch changed between the fork
// point and its current head. Renames are deliberately not detected: a rename
// pair says nothing about whether a repair restored pre-branch content, and the
// plain add/delete pair it decomposes into answers that question directly.
func branchChangedFiles(sctx *pipeline.StepContext, baseSHA, preHeadSHA string) (map[string]branchChange, error) {
	// --no-abbrev is load-bearing: the default raw output abbreviates blob ids,
	// and an abbreviation that turns out to be ambiguous in a large repository
	// would fail the cat-file below, which fails the guard closed and parks a
	// run for no reason. It is also hash-length agnostic, unlike --abbrev=40.
	raw, err := stepGitRunRaw(sctx, "diff", "--raw", "--no-abbrev", "-z", "--no-renames", baseSHA, preHeadSHA)
	if err != nil {
		return nil, fmt.Errorf("read the branch's own changes: %w", err)
	}
	changes := make(map[string]branchChange)
	fields := strings.Split(raw, "\x00")
	for i := 0; i+1 < len(fields); i += 2 {
		info, path := fields[i], fields[i+1]
		if !strings.HasPrefix(info, ":") || path == "" {
			continue
		}
		// ":<srcmode> <dstmode> <srcsha> <dstsha> <status>"
		parts := strings.Fields(strings.TrimPrefix(info, ":"))
		if len(parts) < 5 {
			return nil, fmt.Errorf("read the branch's own changes: unrecognized diff record %q", info)
		}
		changes[path] = branchChange{baseMode: parts[0], headMode: parts[1], baseBlob: parts[2], headBlob: parts[3]}
	}
	return changes, nil
}

// worktreePathsDifferingFromBase reports which of the given paths the working
// tree still differs from base on. A candidate absent from the result is back to
// its pre-branch content, deletion and creation included, because git decides
// that rather than a byte comparison. --name-only always exits 0, so a genuine
// git failure is still an error rather than a silent "no differences".
func worktreePathsDifferingFromBase(sctx *pipeline.StepContext, baseSHA string, paths []string) (map[string]bool, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	args := append([]string{"diff", "--name-only", "-z", baseSHA, "--"}, paths...)
	raw, err := stepGitRunRaw(sctx, args...)
	if err != nil {
		return nil, fmt.Errorf("compare the repair against the branch's base: %w", err)
	}
	differing := make(map[string]bool, len(paths))
	for _, path := range strings.Split(raw, "\x00") {
		if path != "" {
			differing[path] = true
		}
	}
	return differing, nil
}

// blobContent reads a blob by id. An all-zero id is git's spelling of "this side
// has no file", which is content-free rather than an error.
func blobContent(sctx *pipeline.StepContext, blob string) (string, error) {
	if blob == "" || strings.Trim(blob, "0") == "" {
		return "", nil
	}
	content, err := stepGitRunRaw(sctx, "cat-file", "blob", blob)
	if err != nil {
		return "", fmt.Errorf("read blob %s: %w", blob, err)
	}
	return content, nil
}

// reinstatedBaseLines returns the lines that exist exactly once in the
// pre-branch file, that the branch removed entirely, and that the proposed
// repair puts back.
//
// Uniqueness is the evidence threshold, not an oversight. A repair that writes
// back a `}` or a `return nil` the base file also contains elsewhere has proved
// nothing about a decision, and reporting it would fire on ordinary repairs
// until the guard was disabled. The threshold applies identically to the round a
// person reads and to the round that follows it, and everything under it is
// still reviewed on its merits by the revalidation a committed repair triggers.
func reinstatedBaseLines(base, pre, post string) []string {
	baseCounts := lineCounts(base)
	if len(baseCounts) == 0 {
		return nil
	}
	preCounts := lineCounts(pre)
	postCounts := lineCounts(post)

	var restored []string
	for line, count := range baseCounts {
		if count != 1 || preCounts[line] != 0 || postCounts[line] == 0 {
			continue
		}
		if !distinctiveLine(line) {
			continue
		}
		restored = append(restored, line)
	}
	sort.Strings(restored)
	return restored
}

func lineCounts(content string) map[string]int {
	if content == "" {
		return nil
	}
	counts := make(map[string]int)
	for _, line := range strings.Split(strings.TrimSuffix(content, "\n"), "\n") {
		counts[strings.TrimRight(line, "\r")]++
	}
	return counts
}

// distinctiveLine excludes lines that carry no identity of their own: blank
// lines, pure punctuation such as a closing brace, and anything that is not
// valid text. Restoring one of those is not evidence of a decision being undone.
func distinctiveLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || !utf8.ValidString(trimmed) {
		return false
	}
	for _, r := range trimmed {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

// clampEvidenceLine bounds one quoted line, cutting on a rune boundary so the
// evidence never renders as a broken character.
func clampEvidenceLine(line string) string {
	line = strings.TrimSpace(line)
	if len(line) <= maxReversionEvidenceWidth {
		return line
	}
	cut := maxReversionEvidenceWidth
	for cut > 0 && !utf8.RuneStart(line[cut]) {
		cut--
	}
	return line[:cut] + "..."
}

// repairChangedPaths lists every path the repair touched since the pre-repair
// head: tracked modifications and deletions, staged or not and committed or not,
// plus files it created.
func repairChangedPaths(sctx *pipeline.StepContext, preHeadSHA string) ([]string, error) {
	tracked, err := stepGitRunRaw(sctx, "diff", "--name-only", "-z", preHeadSHA)
	if err != nil {
		return nil, fmt.Errorf("list repaired paths: %w", err)
	}
	untracked, err := stepGitRunRaw(sctx, "ls-files", "-z", "--others", "--exclude-standard")
	if err != nil {
		return nil, fmt.Errorf("list added paths: %w", err)
	}
	seen := make(map[string]struct{})
	var paths []string
	for _, chunk := range []string{tracked, untracked} {
		for _, path := range strings.Split(chunk, "\x00") {
			if path == "" {
				continue
			}
			if _, ok := seen[path]; ok {
				continue
			}
			seen[path] = struct{}{}
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	return paths, nil
}

// worktreeFileContent reads the proposed content of one path.
//
// A symlink is read as its target, which is exactly what git stores in the blob,
// so the two sides compare like for like. Anything else that is not a regular
// file (a directory where a file was, a device node) is not comparable and is
// reported as an error rather than as "absent": treating it as absent would let
// an unreadable entry look byte-identical to a file the base did not have, which
// is evidence the guard has not actually got.
func worktreeFileContent(workDir, path string) (string, bool, error) {
	full := filepath.Join(workDir, filepath.FromSlash(path))
	info, err := os.Lstat(full)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("read %s: %w", path, err)
	}
	switch {
	case info.Mode().IsRegular():
		data, err := os.ReadFile(full)
		if err != nil {
			return "", false, fmt.Errorf("read %s: %w", path, err)
		}
		return string(data), true, nil
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(full)
		if err != nil {
			return "", false, fmt.Errorf("read %s: %w", path, err)
		}
		return target, true, nil
	default:
		return "", false, fmt.Errorf("read %s: not a regular file or symlink", path)
	}
}

// ciFixReversionOutcome converts a refused repair into a bounded ask-user gate,
// and returns nil for every other result so ordinary fix failures keep their
// existing warn-and-retry behaviour.
func ciFixReversionOutcome(sctx *pipeline.StepContext, issueDesc string, err error) *pipeline.StepOutcome {
	var reversion *decisionReversionError
	if err == nil || !errors.As(err, &reversion) {
		return nil
	}
	sctx.Log(reversion.Error())

	description := fmt.Sprintf(
		"The CI auto-fix round proposed a repair for %s whose effect is to undo work this branch deliberately did, so it was not committed. %s "+
			"A red check can mean a decision is outstanding rather than that something is broken, and undoing the decision is invisible in the branch's own diff, "+
			"so this is refused rather than committed. The proposed changes are still in the run worktree at %s, uncommitted and unpushed. "+
			"Approve or skip to leave the branch as it is, or resolve the check outside the pipeline. "+
			"If undoing that work really is what you want, a fix response authorises exactly the reversion listed above and nothing else: it runs another repair round, "+
			"and a round that would undo anything different parks again with its own evidence. "+
			"What that round writes beyond this reversion is not waved through with it - a committed repair restarts validation from Review, so the rest is reviewed on its merits.%s",
		issueDesc, reversionDetail(reversion), sctx.WorkDir, truncatedAuthorizationNote(reversion))

	findings := Findings{
		Summary: "CI auto-fix round would undo the branch's own work",
		Items: []Finding{{
			Severity:    "blocking",
			Description: description,
			Action:      types.ActionAskUser,
		}},
	}
	findingsJSON, _ := json.Marshal(findings)
	return &pipeline.StepOutcome{
		NeedsApproval: true,
		Findings:      string(findingsJSON),
	}
}

// truncatedAuthorizationNote says so when a finding was too large to list in
// full. Such a refusal cannot be authorised at the gate, because a fix response
// would otherwise approve the part it never showed.
func truncatedAuthorizationNote(reversion *decisionReversionError) string {
	if !reversion.truncated {
		return ""
	}
	return " This finding is too large to list in full, so it cannot be authorised here at all: a fix response would approve the part above that is only summarised. " +
		"Narrow the repair, or resolve the check outside the pipeline."
}

func reversionDetail(reversion *decisionReversionError) string {
	if reversion.reason != "" {
		return "The guard that proves this could not be evaluated (" + reversion.reason + "), so the repair fails closed."
	}
	var parts []string
	for _, ev := range reversion.evidence {
		part := ev.Path + " would be " + string(ev.Kind)
		if len(ev.Lines) > 0 {
			part += " (for example: " + strings.Join(ev.Lines, " / ")
			if ev.elidedLines > 0 {
				part += fmt.Sprintf(", and %d more %s", ev.elidedLines, plural(ev.elidedLines, "line"))
			}
			part += ")"
		}
		parts = append(parts, part)
	}
	if reversion.elidedFiles > 0 {
		parts = append(parts, fmt.Sprintf("and %d more %s", reversion.elidedFiles, plural(reversion.elidedFiles, "file")))
	}
	return "Evidence: " + strings.Join(parts, "; ") + "."
}
