package steps

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/jev"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// This file builds the opt-in Jev review pre-brief (issue #1055, global
// config jev.review_assist). One batched TypeSafe System One evaluation runs
// before a review turn and contributes ADVISORY input to the review prompt: a
// ranked list of surrounding-context files to read first. It exists to
// shorten the cold reviewer's unguided search, which is where a cold launch
// spends most of its tokens; it is deliberately not a coverage mechanism.
//
// The invariants that keep this inside VISION R4 and the issue's contract:
//
//   - Complete-change coverage is untouched. The reviewed_paths contract, the
//     prompt's full-pass obligations, and the reviewable set are exactly what
//     they are with the assist off. The pre-brief can only ever ADD reading
//     suggestions; no Jev answer can remove a file, an obligation, or a
//     prompt clause.
//   - One owner per judgment (VISION L31). Jev owns one question nothing
//     else owns: "which surrounding files are most relevant as context"
//     (retrieval ranking; the reviewer remains free to read anything). Every
//     verdict - findings, severity, coverage, risk - stays with the
//     session-free reviewer. Per-file scrutiny scores and candidate-defect
//     leads were considered and rejected: both stack a second mechanism on
//     the reviewer's own verdict question, and acting on them to do less
//     work is the fix-delta-only rereview the maintainer already rejected.
//   - Fail closed. A disabled flag, a missing TYPESAFE_API_KEY, a network or
//     API error, or an undecodable answer each produce an empty pre-brief,
//     which is byte-identical to today's prompt. The assist can never block
//     or weaken a review.
//   - The reviewer stays a fresh, session-free invocation; the pre-brief is
//     prompt text, not a resumed session, and the fixer session is never
//     involved.
//
// Jev answers are typed numbers, not generated text, so nothing the service
// returns can inject prose into the prompt; the section is formatted from
// paths and scores alone.

// jevDiffMaxBytes clips the unified diff inside the Jev state. Jev's state
// budget is 32k tokens and code tokenizes denser than prose, so the digest,
// candidate evidence, and questions together must stay well under it; an
// oversized request fails closed to a pre-brief-less review anyway.
const jevDiffMaxBytes = 32 * 1024

// jevStatMaxBytes clips the diff --stat summary in the Jev state.
const jevStatMaxBytes = 4 * 1024

// jevMaxIdentifiers caps how many changed-code identifiers are greped for
// candidate context files.
const jevMaxIdentifiers = 8

// jevMinIdentifierLen skips names too short to search for: a one- to
// three-letter name (a loop index, a minified symbol) matches as a whole word
// almost everywhere, so it cannot point at a use site.
const jevMinIdentifierLen = 4

// jevMaxCandidates caps the candidate files sent for ranking.
const jevMaxCandidates = 40

// jevMaxMatchesPerCandidate caps the grep-matched lines kept per candidate as
// coupling evidence, for Jev's judgment and for the reviewer.
const jevMaxMatchesPerCandidate = 4

// jevMaxListed caps how many ranked files the pre-brief lists.
const jevMaxListed = 10

// jevRelevanceThreshold is the minimum probability-weighted relevance score
// (on the 0-3 rubric in jevRelevanceLevels) for a candidate to be listed.
const jevRelevanceThreshold = 2.0

// jevConfidenceThreshold drops a ranked candidate whose score confidence is
// too low to act on; failing toward not listing is the pre-assist behavior.
const jevConfidenceThreshold = 0.3

// jevRelevanceLevels is the ordered rubric for per-candidate relevance
// scores. Level wording follows the TypeSafe score guidance: concrete
// situations that stand on their own.
var jevRelevanceLevels = []string{
	"Unrelated to the change: reviewing the change would not benefit from reading this file",
	"Weakly related: shares a package or naming with the change but no evident coupling to its behavior",
	"Relevant context: references or couples to the changed symbols or behavior, so reading it would inform the review",
	"Essential context: the change cannot be judged correctly without understanding this file",
}

// jevClient is the slice of the TypeSafe client the pre-brief needs, so tests
// can inject a fake.
type jevClient interface {
	Evaluate(ctx context.Context, state any, questions map[string]jev.Question) (*jev.Response, error)
}

// jevCandidate is one surrounding-context file offered for ranking, with the
// coupling evidence code found for it.
type jevCandidate struct {
	Path    string   `json:"path"`
	Matches []string `json:"matches,omitempty"`
}

// jevChangeState is the state one pre-brief evaluation runs over.
type jevChangeState struct {
	Change     jevChangeDigest `json:"change"`
	Candidates []jevCandidate  `json:"candidates"`
}

type jevChangeDigest struct {
	Branch     string `json:"branch"`
	BaseCommit string `json:"base_commit"`
	DiffStat   string `json:"diff_stat"`
	Diff       string `json:"diff"`
}

// reviewPrebriefSection builds the advisory pre-brief for one review turn, or
// "" when the assist is disabled or unavailable. It never returns an error:
// every failure degrades to the same cold, complete review the turn would run
// without the assist, with one log line naming the degradation.
func (s *ReviewStep) reviewPrebriefSection(ctx context.Context, sctx *pipeline.StepContext, baseSHA string, changed, reviewable []string) string {
	if sctx.Config == nil || !sctx.Config.Jev.ReviewAssist {
		return ""
	}
	logf := func(format string, args ...any) {
		if sctx.Log != nil {
			sctx.Log(fmt.Sprintf(format, args...))
		}
	}
	client := s.jev
	if client == nil {
		client = jev.NewClientFromEnv()
	}
	if client == nil {
		logf("jev review assist is enabled but %s is not set; reviewing without a pre-brief", jev.EnvKey)
		return ""
	}

	state, candidates := buildJevReviewState(ctx, sctx, baseSHA, changed, reviewable)
	if state == nil {
		logf("jev pre-brief skipped: could not read the change digest; reviewing without a pre-brief")
		return ""
	}
	if len(candidates) == 0 {
		logf("jev pre-brief skipped: no surrounding-context candidates to rank; reviewing without a pre-brief")
		return ""
	}
	resp, err := client.Evaluate(ctx, state, buildJevQuestions(candidates))
	if err != nil {
		logf("jev pre-brief unavailable (%v); reviewing without a pre-brief", err)
		return ""
	}
	section, listed := formatJevPrebrief(resp, candidates)
	logf("jev pre-brief: %d of %d context candidates listed (model %s, %d input tokens)", listed, len(candidates), resp.Model, resp.Usage.InputTokens)
	return section
}

// buildJevReviewState assembles the code-filtered Jev state: the change
// digest (stat plus clipped diff of the reviewable paths only, so content
// ignore_patterns excludes from the review never crowds the clipped digest)
// and the candidate surrounding-context files. Nil when the diff itself
// cannot be read, which the caller treats as fail-closed.
func buildJevReviewState(ctx context.Context, sctx *pipeline.StepContext, baseSHA string, changed, reviewable []string) (*jevChangeState, []jevCandidate) {
	stat, err := git.Run(ctx, sctx.WorkDir, jevDiffArgs(sctx, baseSHA, reviewable, "--stat")...)
	if err != nil {
		return nil, nil
	}
	diff, err := git.Run(ctx, sctx.WorkDir, jevDiffArgs(sctx, baseSHA, reviewable)...)
	if err != nil {
		return nil, nil
	}
	digest := jevChangeDigest{
		Branch:     sctx.Run.Branch,
		BaseCommit: baseSHA,
		DiffStat:   clipMiddle(stat, jevStatMaxBytes),
		Diff:       clipMiddle(diff, jevDiffMaxBytes),
	}
	candidates := jevContextCandidates(ctx, sctx.WorkDir, diff, changed, reviewable)
	return &jevChangeState{Change: digest, Candidates: candidates}, candidates
}

// jevDiffArgs mirrors the changed-files diff range of the review turn,
// limited to the reviewable paths: a rereview diffs the worktree against the
// base (fix commits plus uncommitted fixer work), an initial review diffs
// base..head. opts are diff options placed before the range.
func jevDiffArgs(sctx *pipeline.StepContext, baseSHA string, reviewable []string, opts ...string) []string {
	rev := baseSHA + ".." + sctx.Run.HeadSHA
	if sctx.Fixing {
		rev = baseSHA
	}
	args := append([]string{"diff", "--no-renames"}, opts...)
	args = append(args, rev, "--")
	for _, p := range reviewable {
		args = append(args, ":(literal)"+p)
	}
	return args
}

// buildJevQuestions packs one relevance Score per candidate into a single
// batched request.
func buildJevQuestions(candidates []jevCandidate) map[string]jev.Question {
	questions := make(map[string]jev.Question, len(candidates))
	for i := range candidates {
		id := fmt.Sprintf("ctx_%d", i)
		questions[id] = jev.Question{
			Type: "score",
			Instructions: fmt.Sprintf(
				"You are ranking supporting files for a code review. The change under review is in `change`. How relevant is the file `candidates[%d]` as surrounding context for reviewing that change? Judge from its path and the shown matched lines, which are the places it references the changed symbols.", i),
			Criteria: jevRelevanceLevels,
		}
	}
	return questions
}

// formatJevPrebrief renders the advisory prompt section from typed answers.
// "" when nothing clears the thresholds, so an uneventful pre-screen adds no
// prompt noise. Also returns the listed-candidate count for the log line.
func formatJevPrebrief(resp *jev.Response, candidates []jevCandidate) (string, int) {
	type ranked struct {
		path  string
		score float64
	}
	var listing []ranked
	for i, c := range candidates {
		answer, ok := resp.Answers[fmt.Sprintf("ctx_%d", i)]
		if !ok || answer.Type != "score" {
			continue
		}
		if answer.Score >= jevRelevanceThreshold && answer.Confidence >= jevConfidenceThreshold {
			listing = append(listing, ranked{path: c.Path, score: answer.Score})
		}
	}
	if len(listing) == 0 {
		return "", 0
	}
	sort.SliceStable(listing, func(i, j int) bool { return listing[i].score > listing[j].score })
	if len(listing) > jevMaxListed {
		listing = listing[:jevMaxListed]
	}
	var b strings.Builder
	b.WriteString("\n\nPre-brief (advisory output of a fast pre-screen model; claims, not evidence):\n")
	b.WriteString("Every obligation above is unchanged: read and judge every changed file yourself, and treat each statement below as a hint to verify, never as a finding.\n")
	b.WriteString("- Surrounding context ranked most relevant to this change; consider reading it first, and explore beyond it as the change requires:\n")
	for _, item := range listing {
		b.WriteString("  - " + item.path + "\n")
	}
	return b.String(), len(listing)
}

// definitionPattern extracts the name a definition line introduces, across
// common languages, so its use sites can be found with git grep. It is
// anchored at the start of the line (after indentation and common modifiers)
// so prose - a comment saying "we let the user" - never yields a name.
// Definitions are the stable handle a change offers; call-site shaped tokens
// are far noisier.
var definitionPattern = regexp.MustCompile(`^\s*(?:(?:export|default|pub|public|private|protected|static|async|abstract|final)\s+)*(?:func|def|function|fn|sub|type|class|struct|enum|interface|trait|record|const|var|let)\s+(?:\([^)]*\)\s*)?([A-Za-z_][A-Za-z0-9_]*)`)

// jevIdentifiers returns up to jevMaxIdentifiers distinct defined names from
// the diff, in first-appearance order. Three line shapes carry them: added
// definition lines (what the change introduces), hunk-header trailing context
// and unchanged context lines (the enclosing definition a behavior-only edit
// modifies), and removed lines (what the change replaces). Prose files are
// skipped, and names shorter than jevMinIdentifierLen are dropped. Candidates
// built from these are advisory and ranked downstream, so a
// nearby-but-unrelated definition costs at most a low-ranked candidate.
func jevIdentifiers(diff string) []string {
	seen := map[string]bool{}
	var ids []string
	prose := false
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			// The header line ends with the post-image path.
			prose = isProsePath(line)
		case prose:
		case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
		case strings.HasPrefix(line, "@@"):
			if _, rest, ok := strings.Cut(line, "@@"); ok {
				// rest follows the first @@; the trailing context follows the
				// second one.
				if _, context, ok := strings.Cut(rest, "@@"); ok {
					ids = appendDefinition(ids, seen, context)
				}
			}
		case strings.HasPrefix(line, "+"), strings.HasPrefix(line, "-"), strings.HasPrefix(line, " "):
			ids = appendDefinition(ids, seen, line[1:])
		}
	}
	if len(ids) > jevMaxIdentifiers {
		ids = ids[:jevMaxIdentifiers]
	}
	return ids
}

// isProsePath reports whether a path names a documentation or prose file,
// whose lines are sentences rather than definitions.
func isProsePath(p string) bool {
	switch strings.ToLower(path.Ext(strings.Trim(p, `"`))) {
	case ".md", ".mdx", ".markdown", ".rst", ".txt", ".adoc":
		return true
	}
	return false
}

// appendDefinition appends the name definitionPattern finds in text.
func appendDefinition(ids []string, seen map[string]bool, text string) []string {
	match := definitionPattern.FindStringSubmatch(text)
	if match == nil {
		return ids
	}
	name := match[1]
	if len(name) < jevMinIdentifierLen || seen[name] {
		return ids
	}
	seen[name] = true
	return append(ids, name)
}

// jevContextCandidates builds the candidate surrounding-context set without
// any model: use sites of the identifiers the change introduces or modifies
// (with the matched lines as coupling evidence), then same-directory siblings
// of the reviewable files. Changed files themselves are never candidates -
// the reviewer reads them regardless - and the set is capped at
// jevMaxCandidates.
//
// Use sites are ranked by how specific their matches are: each identifier
// contributes 1/N to every file it appears in, N being how many files it
// appears in. A file referencing a rare changed name outranks one that only
// shares a ubiquitous name, so the cap keeps the tightest coupling rather
// than whatever sorts first by path.
func jevContextCandidates(ctx context.Context, workDir, diff string, changed, reviewable []string) []jevCandidate {
	changedSet := make(map[string]bool, len(changed))
	for _, p := range changed {
		changedSet[p] = true
	}
	ids := jevIdentifiers(diff)

	// git grep -l prints each matching path once, so the output is bounded by
	// the number of tracked files however common the name is. git grep exits
	// 1 when nothing matches; that is not an error for an advisory list.
	score := map[string]float64{}
	for _, id := range ids {
		files := nulSeparated(jevGrep(ctx, workDir, "-l", "-z", "-w", "-F", "-I", "-e", id))
		for _, f := range files {
			if !changedSet[f] {
				score[f] += 1 / float64(len(files))
			}
		}
	}
	order := make([]string, 0, len(score))
	for f := range score {
		order = append(order, f)
	}
	sort.Slice(order, func(i, j int) bool {
		if score[order[i]] != score[order[j]] {
			return score[order[i]] > score[order[j]]
		}
		return order[i] < order[j]
	})
	if len(order) > jevMaxCandidates {
		order = order[:jevMaxCandidates]
	}
	candidates := make([]jevCandidate, len(order))
	index := make(map[string]int, len(order))
	for i, f := range order {
		candidates[i] = jevCandidate{Path: f}
		index[f] = i
	}

	// Matched lines as coupling evidence, searched only in the kept files.
	if len(candidates) > 0 {
		args := []string{"-n", "-z", "-w", "-F", "-I"}
		for _, id := range ids {
			args = append(args, "-e", id)
		}
		args = append(args, "--")
		for _, f := range order {
			args = append(args, ":(literal)"+f)
		}
		for _, line := range strings.Split(string(jevGrep(ctx, workDir, args...)), "\n") {
			parts := strings.SplitN(line, "\x00", 3)
			if len(parts) != 3 {
				continue
			}
			i, ok := index[parts[0]]
			if !ok || len(candidates[i].Matches) >= jevMaxMatchesPerCandidate {
				continue
			}
			candidates[i].Matches = append(candidates[i].Matches, clipMiddle(parts[1]+": "+strings.TrimSpace(parts[2]), 140))
		}
	}

	// Same-directory siblings, path-only. One ls-files read is cheaper than
	// walking directories and respects the worktree.
	if len(candidates) < jevMaxCandidates {
		if tracked, err := git.RunRaw(ctx, workDir, "ls-files", "-z"); err == nil {
			trackedFiles := nulSeparated(tracked)
			for _, changedPath := range reviewable {
				dir := path.Dir(changedPath)
				for _, f := range trackedFiles {
					if len(candidates) >= jevMaxCandidates {
						break
					}
					if _, seen := index[f]; seen || changedSet[f] || path.Dir(f) != dir {
						continue
					}
					index[f] = len(candidates)
					candidates = append(candidates, jevCandidate{Path: f})
				}
			}
		}
	}
	return candidates
}

// jevGrep runs git grep, treating "no match" (exit 1) and every other
// failure as no output.
func jevGrep(ctx context.Context, workDir string, args ...string) []byte {
	out, err := git.RunRaw(ctx, workDir, append([]string{"grep"}, args...)...)
	if err != nil {
		return nil
	}
	return out
}

// nulSeparated splits NUL-terminated git output.
func nulSeparated(out []byte) []string {
	if len(out) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
}

// clipMiddle keeps the head and tail of text within maxBytes, marking the
// omission, so a clipped digest still shows both the change's start and its
// end.
func clipMiddle(text string, maxBytes int) string {
	if len(text) <= maxBytes {
		return text
	}
	head := maxBytes / 2
	tail := maxBytes - head
	return text[:head] + fmt.Sprintf("\n... [%d bytes omitted] ...\n", len(text)-maxBytes) + text[len(text)-tail:]
}
