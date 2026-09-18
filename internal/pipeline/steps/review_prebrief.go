package steps

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
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
// before a review turn and contributes two kinds of ADVISORY input to the
// review prompt: a ranked list of surrounding-context files to read first,
// and domain flags that add emphasis clauses. It exists to shorten the cold
// reviewer's unguided search, which is where a cold launch spends most of its
// tokens; it is deliberately not a coverage mechanism.
//
// The invariants that keep this inside VISION R4 and the issue's contract:
//
//   - Complete-change coverage is untouched. The reviewed_paths contract, the
//     prompt's full-pass obligations, and the reviewable set are exactly what
//     they are with the assist off. The pre-brief can only ever ADD reading
//     suggestions and emphasis; no Jev answer can remove a file, an
//     obligation, or a prompt clause.
//   - One owner per judgment (VISION L31). Jev owns two questions nothing
//     else owns: "which surrounding files are most relevant as context"
//     (retrieval ranking; the reviewer remains free to read anything) and
//     "which domain emphasis clauses trigger" (routing). Every verdict -
//     findings, severity, coverage, risk - stays with the session-free
//     reviewer. Per-file scrutiny scores and candidate-defect leads were
//     considered and rejected: both stack a second mechanism on the
//     reviewer's own verdict question, and acting on them to do less work is
//     the fix-delta-only rereview the maintainer already rejected.
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

// jevFindingsMaxBytes clips the sanitized outstanding-findings digest carried
// on a rereview's Jev state.
const jevFindingsMaxBytes = 6 * 1024

// jevMaxIdentifiers caps how many changed-code identifiers are greped for
// candidate context files.
const jevMaxIdentifiers = 8

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

// jevDomainThreshold is the noul at or above which a domain flag fires.
const jevDomainThreshold = 0.6

// jevRelevanceLevels is the ordered rubric for per-candidate relevance
// scores. Level wording follows the TypeSafe score guidance: concrete
// situations that stand on their own.
var jevRelevanceLevels = []string{
	"Unrelated to the change: reviewing the change would not benefit from reading this file",
	"Weakly related: shares a package or naming with the change but no evident coupling to its behavior",
	"Relevant context: references or couples to the changed symbols or behavior, so reading it would inform the review",
	"Essential context: the change cannot be judged correctly without understanding this file",
}

// jevDomain describes one emphasis trigger: a noul question over the change
// digest and the additive clause it contributes when it fires. A clause is
// only ever added; a domain that does not fire leaves the prompt exactly as
// it is without the assist.
type jevDomain struct {
	id       string
	question string
	yesMeans string
	noMeans  string
	emphasis string
}

// jevDomains are the emphasis triggers. Each names a domain the review prompt
// already cares about and sharpens, never widens or narrows, the existing
// obligations.
var jevDomains = []jevDomain{
	{
		id:       "domain_auth",
		question: "Does the change in `change.diff` read, write, return, index, cache, log, or otherwise process potentially protected resources or user data (identity, credentials, authorization decisions, PII, drafts, internal or reviewer-only metadata)?",
		yesMeans: "The changed code touches identity, authorization, or user-data boundaries",
		noMeans:  "The changed code does not touch protected resources or user data",
		emphasis: "the change appears to cross protected-resource or user-data boundaries, so the authorization/privacy tracing obligation above applies with full rigor",
	},
	{
		id:       "domain_concurrency",
		question: "Does the change in `change.diff` introduce or modify concurrency, shared mutable state, or asynchronous lifecycle (goroutines or threads, locks, channels, queues, retries, background work, cancellation)?",
		yesMeans: "The changed code involves concurrency or shared state",
		noMeans:  "The changed code is sequential with no shared mutable state",
		emphasis: "the change appears to involve concurrency or shared state, so trace at least one concrete interleaving or lifecycle ordering through the changed code",
	},
	{
		id:       "domain_contracts",
		question: "Does the change in `change.diff` alter a public API, a wire or serialization format, a storage schema, a configuration surface, or another cross-component contract with producers or consumers outside the changed files?",
		yesMeans: "The changed code alters a contract other code depends on",
		noMeans:  "The changed code is internal to the changed components",
		emphasis: "the change appears to alter a public or cross-component contract, so check every producer and consumer of that contract you can find",
	},
	{
		id:       "domain_errors",
		question: "Does the change in `change.diff` alter error handling, rollback, recovery, or cleanup paths?",
		yesMeans: "The changed code alters failure or cleanup behavior",
		noMeans:  "The changed code does not alter failure or cleanup behavior",
		emphasis: "the change appears to alter error handling or rollback paths, so trace at least one concrete failure through the changed code",
	},
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
	Branch              string `json:"branch"`
	BaseCommit          string `json:"base_commit"`
	DiffStat            string `json:"diff_stat"`
	Diff                string `json:"diff"`
	OutstandingFindings string `json:"outstanding_findings,omitempty"`
}

// reviewPrebriefSection builds the advisory pre-brief for one review turn, or
// "" when the assist is disabled or unavailable. It never returns an error:
// every failure degrades to the same cold, complete review the turn would run
// without the assist, with one log line naming the degradation.
func (s *ReviewStep) reviewPrebriefSection(ctx context.Context, sctx *pipeline.StepContext, baseSHA string, changed []string) string {
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

	state, candidates := buildJevReviewState(ctx, sctx, baseSHA, changed)
	if state == nil {
		logf("jev pre-brief skipped: could not read the change digest; reviewing without a pre-brief")
		return ""
	}
	questions := buildJevQuestions(candidates)
	resp, err := client.Evaluate(ctx, state, questions)
	if err != nil {
		logf("jev pre-brief unavailable (%v); reviewing without a pre-brief", err)
		return ""
	}
	section, listed, flags := formatJevPrebrief(resp, candidates)
	if section == "" {
		logf("jev pre-brief found nothing worth surfacing (model %s, %d input tokens)", resp.Model, resp.Usage.InputTokens)
		return ""
	}
	logf("jev pre-brief: %d of %d context candidates listed, %d domain flag(s) (model %s, %d input tokens)", listed, len(candidates), flags, resp.Model, resp.Usage.InputTokens)
	return section
}

// buildJevReviewState assembles the code-filtered Jev state: the change
// digest (stat plus clipped diff, plus sanitized outstanding findings on a
// rereview) and the candidate surrounding-context files. Nil when the diff
// itself cannot be read, which the caller treats as fail-closed.
func buildJevReviewState(ctx context.Context, sctx *pipeline.StepContext, baseSHA string, changed []string) (*jevChangeState, []jevCandidate) {
	diffArgs := jevDiffArgs(sctx, baseSHA)
	stat, err := git.Run(ctx, sctx.WorkDir, append(diffArgs, "--stat")...)
	if err != nil {
		return nil, nil
	}
	diff, err := git.Run(ctx, sctx.WorkDir, diffArgs...)
	if err != nil {
		return nil, nil
	}
	digest := jevChangeDigest{
		Branch:     sctx.Run.Branch,
		BaseCommit: baseSHA,
		DiffStat:   clipMiddle(stat, jevStatMaxBytes),
		Diff:       clipMiddle(diff, jevDiffMaxBytes),
	}
	if sctx.Fixing && strings.TrimSpace(sctx.PreviousFindings) != "" {
		digest.OutstandingFindings = clipMiddle(sanitizedPreviousFindingsForPrompt(sctx.PreviousFindings), jevFindingsMaxBytes)
	}
	candidates := jevContextCandidates(ctx, sctx, diff, changed)
	return &jevChangeState{Change: digest, Candidates: candidates}, candidates
}

// jevDiffArgs mirrors the changed-files diff range of the review turn: a
// rereview diffs the worktree against the base (fix commits plus uncommitted
// fixer work), an initial review diffs base..head.
func jevDiffArgs(sctx *pipeline.StepContext, baseSHA string) []string {
	if sctx.Fixing {
		return []string{"diff", "--no-renames", baseSHA}
	}
	return []string{"diff", "--no-renames", baseSHA + ".." + sctx.Run.HeadSHA}
}

// buildJevQuestions packs the domain Nouls and one relevance Score per
// candidate into a single batched request.
func buildJevQuestions(candidates []jevCandidate) map[string]jev.Question {
	questions := make(map[string]jev.Question, len(jevDomains)+len(candidates))
	for _, d := range jevDomains {
		questions[d.id] = jev.Question{
			Type:         "noul",
			Instructions: d.question,
			Criteria:     jev.NoulCriteria{True: d.yesMeans, False: d.noMeans},
		}
	}
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
// "" when nothing clears a threshold, so an uneventful pre-screen adds no
// prompt noise. Also returns the listed-candidate and domain-flag counts for
// the log line.
func formatJevPrebrief(resp *jev.Response, candidates []jevCandidate) (string, int, int) {
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
	sort.SliceStable(listing, func(i, j int) bool { return listing[i].score > listing[j].score })
	if len(listing) > jevMaxListed {
		listing = listing[:jevMaxListed]
	}
	var flags []string
	for _, d := range jevDomains {
		answer, ok := resp.Answers[d.id]
		if ok && answer.Type == "noul" && answer.Noul >= jevDomainThreshold {
			flags = append(flags, d.emphasis)
		}
	}
	if len(listing) == 0 && len(flags) == 0 {
		return "", 0, 0
	}
	var b strings.Builder
	b.WriteString("\n\nPre-brief (advisory output of a fast pre-screen model; claims, not evidence):\n")
	b.WriteString("Every obligation above is unchanged: read and judge every changed file yourself, and treat each statement below as a hint to verify, never as a finding.\n")
	if len(listing) > 0 {
		b.WriteString("- Surrounding context ranked most relevant to this change; consider reading it first, and explore beyond it as the change requires:\n")
		for _, item := range listing {
			b.WriteString("  - " + item.path + "\n")
		}
	}
	for _, flag := range flags {
		b.WriteString("- Domain flag: " + flag + ".\n")
	}
	return b.String(), len(listing), len(flags)
}

// definitionPattern extracts the introduced or modified named entities from
// added diff lines across common languages, so their use sites can be found
// with git grep. Definitions are the stable handle a change offers; call-site
// shaped tokens are far noisier.
var definitionPattern = regexp.MustCompile(`(?:func|def|function|fn|sub|type|class|struct|enum|interface|trait|record|const|var|let)\s+(?:\([^)]*\)\s*)?([A-Za-z_][A-Za-z0-9_]*)`)

// jevIdentifiers returns up to jevMaxIdentifiers distinct defined names from
// the diff, in first-appearance order. Three line shapes carry them: added
// definition lines (what the change introduces), hunk-header trailing context
// and unchanged context lines (the enclosing definition a behavior-only edit
// modifies), and removed lines (what the change replaces). Candidates built
// from these are advisory and ranked downstream, so a nearby-but-unrelated
// definition costs at most a low-ranked candidate.
func jevIdentifiers(diff string) []string {
	seen := map[string]bool{}
	var ids []string
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
		case strings.HasPrefix(line, "@@"):
			if _, rest, ok := strings.Cut(line, "@@"); ok {
				// rest follows the first @@; the trailing context follows the
				// second one.
				if _, context, ok := strings.Cut(rest, "@@"); ok {
					ids = appendDefinitions(ids, seen, context)
				}
			}
		case strings.HasPrefix(line, "+"), strings.HasPrefix(line, "-"), strings.HasPrefix(line, " "):
			ids = appendDefinitions(ids, seen, line[1:])
		}
	}
	if len(ids) > jevMaxIdentifiers {
		ids = ids[:jevMaxIdentifiers]
	}
	return ids
}

// appendDefinitions appends the names definitionPattern finds in text.
func appendDefinitions(ids []string, seen map[string]bool, text string) []string {
	for _, match := range definitionPattern.FindAllStringSubmatch(text, -1) {
		name := match[1]
		if !seen[name] {
			seen[name] = true
			ids = append(ids, name)
		}
	}
	return ids
}

// jevContextCandidates builds the candidate surrounding-context set without
// any model: use sites of the identifiers the change introduces or modifies
// (with the matched lines as coupling evidence), plus same-directory siblings
// and test counterparts of the changed files. Changed files themselves are
// never candidates - the reviewer reads them regardless - and the set is
// capped at jevMaxCandidates.
func jevContextCandidates(ctx context.Context, sctx *pipeline.StepContext, diff string, changed []string) []jevCandidate {
	changedSet := make(map[string]bool, len(changed))
	for _, p := range changed {
		changedSet[p] = true
	}
	candidates := map[string]*jevCandidate{}
	var order []string
	add := func(path string) *jevCandidate {
		if changedSet[path] {
			return nil
		}
		if c, ok := candidates[path]; ok {
			return c
		}
		if len(order) >= jevMaxCandidates {
			return nil
		}
		c := &jevCandidate{Path: path}
		candidates[path] = c
		order = append(order, path)
		return c
	}

	// Use sites of the changed definitions, with the matching lines as
	// evidence. git grep exits 1 when nothing matches; that is not an error
	// for an advisory candidate list.
	if ids := jevIdentifiers(diff); len(ids) > 0 {
		args := []string{"grep", "-n", "-I", "-F"}
		for _, id := range ids {
			args = append(args, "-e", id)
		}
		out, err := git.RunRaw(ctx, sctx.WorkDir, args...)
		if err != nil {
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
				out = nil
			}
		}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			file, matched, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			c := add(file)
			if c == nil {
				continue
			}
			if len(c.Matches) < jevMaxMatchesPerCandidate {
				c.Matches = append(c.Matches, clipMiddle(strings.TrimSpace(matched), 140))
			}
		}
	}

	// Same-directory siblings and test counterparts, path-only. One ls-files
	// read is cheaper than walking directories and respects the worktree.
	if len(order) < jevMaxCandidates {
		if tracked, err := git.Run(ctx, sctx.WorkDir, "ls-files"); err == nil {
			trackedFiles := strings.Split(tracked, "\n")
			for _, changedPath := range changed {
				dir := path.Dir(changedPath)
				counterpart := testCounterpart(changedPath)
				for _, f := range trackedFiles {
					if len(order) >= jevMaxCandidates {
						break
					}
					if f == changedPath || changedSet[f] {
						continue
					}
					if path.Dir(f) == dir || (counterpart != "" && f == counterpart) {
						add(f)
					}
				}
			}
		}
	}

	result := make([]jevCandidate, 0, len(order))
	for _, p := range order {
		result = append(result, *candidates[p])
	}
	return result
}

// testCounterpart maps a file to its conventional test twin in the same
// directory, or "" when the name has no common test spelling. It mirrors the
// naming rules in isTestFile.
func testCounterpart(file string) string {
	dir := path.Dir(file)
	base := path.Base(file)
	switch {
	case strings.HasSuffix(base, "_test.go"):
		return dir + "/" + strings.TrimSuffix(base, "_test.go") + ".go"
	case strings.HasSuffix(base, ".go"):
		return dir + "/" + strings.TrimSuffix(base, ".go") + "_test.go"
	case strings.HasSuffix(base, ".py"):
		name := strings.TrimSuffix(base, ".py")
		if strings.HasPrefix(name, "test_") {
			return dir + "/" + strings.TrimPrefix(name, "test_") + ".py"
		}
		return dir + "/test_" + name + ".py"
	case strings.HasSuffix(base, ".ts"):
		if strings.HasSuffix(base, ".test.ts") {
			return dir + "/" + strings.TrimSuffix(base, ".test.ts") + ".ts"
		}
		return dir + "/" + strings.TrimSuffix(base, ".ts") + ".test.ts"
	}
	return ""
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
