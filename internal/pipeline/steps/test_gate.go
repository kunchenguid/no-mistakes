package steps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// This file owns the Test step's diff-class gate: the decision about whether
// the live-evidence agent has to run at all for this run.
//
// The configured commands.test is NOT part of that decision. It runs on every
// run, before this gate is consulted, and this gate can never skip it. What
// the gate bounds is the evidence turn, which a pipeline audit measured at
// ~21 minutes and ~19M tokens per run - 38% of all pipeline tokens - against a
// no-go rate of roughly one round in twenty-three, re-bought in full on every
// decision-only re-run of a branch (median task: four runs).
//
// The gate is OFF unless the repository's trusted .no-mistakes.yaml sets
// test.evidence_gate: diff-class. With it unset the step invokes the agent on
// every run and records no evidence source at all, which is byte-for-byte the
// behavior before this file existed: skipping live validation changes which
// runs get validated, so it is a maintainer's decision rather than a version
// bump's. When it is on, two conditions remove that cost without removing
// evidence:
//
//  1. The run's diff (merge-base with the repository's default branch .. head,
//     the same base the Review and Document steps read; a repository that sets
//     pr.base_branch elsewhere gets a wider diff here, which only ever fails
//     open to running the agent) touches no product file. There is
//     then no live-drivable surface by construction, so the step records the
//     existing no-surface verdict and marks it automatic. Unlike the agent's
//     own no-surface it does not park: the classification is mechanical, so
//     there is nothing for a human to decide.
//  2. This run is not a fix round, and this branch's NEWEST recorded test
//     verdict is a go, earned at head H0 under the same user intent this run
//     carries, with no product file changed between H0 and this head. That
//     verdict still describes this head's product behavior against the same
//     acceptance criteria, so it is recorded again with a pointer to the run
//     that earned it.
//
// A reused verdict is recorded AGAINST H0, never restamped onto this head: it
// says "the product behavior these scenarios proved has not changed", which is
// a weaker and true claim, not "these scenarios ran here". Everything that
// asks whether a particular head was live validated keys on the recorded head,
// so that distinction is what stops a run the agent never drove from
// publishing a live-validation claim.
//
// Everything else runs the agent, and so does any failure to establish either
// condition: the gate fails open to today's behavior rather than guessing.

// testEvidenceDecision is the gate's answer. Source is one of the
// types.TestEvidenceSource* constants; Reason is the one-line human account
// that reaches the step log, axi, and the PR's Testing section. Reused carries
// the prior run's verdict and scenarios when Source is
// types.TestEvidenceSourceReused.
type testEvidenceDecision struct {
	Source string
	Reason string
	Reused Findings
}

// skipsAgent reports whether this decision replaces the live-evidence turn.
//
// It is deliberately a positive test for the two skipping sources rather than
// "not agent": the zero decision means the gate is switched off, and that must
// run the agent AND record nothing, so an off repository's findings payload,
// PR body, and axi output stay identical to a build without the gate.
func (d testEvidenceDecision) skipsAgent() bool {
	return d.Source == types.TestEvidenceSourceNoProductChange ||
		d.Source == types.TestEvidenceSourceReused
}

// resolveTestEvidenceGate decides whether the live-evidence agent runs.
//
// baselineFailed is honored rather than ignored: reusing an earlier go verdict
// while this run's configured test command is red would restate a conclusion
// the run has already contradicted, so a failing baseline always drives a
// fresh evidence turn. The no-product-file conclusion is independent of the
// baseline - it is a fact about the diff - and the baseline's own error
// finding still parks the step on its own.
func resolveTestEvidenceGate(sctx *pipeline.StepContext, baseSHA string, baselineFailed bool) testEvidenceDecision {
	agentDecision := testEvidenceDecision{Source: types.TestEvidenceSourceAgent, Reason: "live-evidence agent drove scenarios in this run"}
	if sctx == nil || sctx.Config == nil {
		return agentDecision
	}
	if sctx.Config.Test.EvidenceGate != config.TestEvidenceGateDiffClass {
		// Not opted in. The zero decision runs the agent and records no
		// evidence source or reason, so nothing downstream can tell this build
		// from one without the gate.
		return testEvidenceDecision{}
	}
	nonProduct := sctx.Config.Test.NonProductPaths

	product, err := changedProductPaths(sctx, baseSHA, nonProduct)
	if err != nil {
		// Fail open: an unreadable diff must not silently skip live
		// validation, and running the agent is exactly today's behavior.
		sctx.Log(fmt.Sprintf("could not classify the changed paths (%v); running the live-evidence agent", err))
		return agentDecision
	}
	if len(product) == 0 {
		return testEvidenceDecision{
			Source: types.TestEvidenceSourceNoProductChange,
			Reason: "no product file in diff",
		}
	}
	if baselineFailed {
		return agentDecision
	}
	if reuse, ok := reusableBranchVerdict(sctx, nonProduct); ok {
		return reuse
	}
	return agentDecision
}

// changedProductPaths returns the changed paths this repository counts as
// product or UI files. In fix mode the comparison is against the working tree,
// matching how the review step reads its own changed set. commitAgentFixes has
// already committed and advanced Run.HeadSHA by this point, so the two forms
// normally agree; comparing against the tree additionally counts anything the
// fix turn or the configured test command left modified in it, which only ever
// fails open to running the agent.
func changedProductPaths(sctx *pipeline.StepContext, baseSHA string, nonProduct []string) ([]string, error) {
	rangeArg := baseSHA + ".." + sctx.Run.HeadSHA
	if sctx.Fixing {
		rangeArg = baseSHA
	}
	return diffProductPaths(sctx.Ctx, sctx.WorkDir, nonProduct, rangeArg)
}

func diffProductPaths(ctx context.Context, workDir string, nonProduct []string, rangeArg string) ([]string, error) {
	out, err := git.Run(ctx, workDir, "diff", "--name-only", "-z", "--no-renames", rangeArg)
	if err != nil {
		return nil, err
	}
	var product []string
	for _, file := range changedPathList(out) {
		if !isNonProductPath(file, nonProduct) {
			product = append(product, file)
		}
	}
	return product, nil
}

// isNonProductPath reports whether ANY rule in patterns classifies file as
// something the live-evidence agent cannot drive. An empty pattern list means
// the repository opted every path back into being product code.
func isNonProductPath(file string, patterns []string) bool {
	for _, pattern := range patterns {
		if matchNonProductPattern(file, pattern) {
			return true
		}
	}
	return false
}

// matchNonProductPattern extends the ignore_patterns match rules with a
// leading "**/", which matches at any depth. The plain subtree form
// "testdata/**" only matches at the repository root, and nested fixture
// directories (internal/foo/testdata/...) are the common case, so the defaults
// need a way to say "wherever this directory appears". The extension is local
// to this classification on purpose: ignore_patterns and
// review.path_instructions keep their documented semantics unchanged.
func matchNonProductPattern(file, pattern string) bool {
	rest, anyDepth := strings.CutPrefix(pattern, "**/")
	if !anyDepth {
		return matchIgnorePattern(file, pattern)
	}
	segments := strings.Split(file, "/")
	for i := range segments {
		if matchIgnorePattern(strings.Join(segments[i:], "/"), rest) {
			return true
		}
	}
	return false
}

// reusableBranchVerdict returns a reuse decision when this branch's NEWEST
// recorded test verdict is a go that still covers this head's product files.
//
// Only that newest verdict is consulted, and it is never skipped past. An
// older go necessarily spans a wider diff, so if the newest go does not cover
// this head no earlier one can; and a newer no-go, inconclusive, or no-surface
// entry is this branch's own latest evidence contradicting any earlier go, so
// reusing behind it would publish a conclusion the branch has already moved
// past.
//
// The rest of the rules are deliberately narrow for the same reason. Only a go
// verdict is reusable: every other conclusion is about a state a human has not
// resolved, and restating it would skip the decision rather than the cost.
// Only the same branch is consulted, because a verdict is evidence about one
// line of development. And the diff that proves nothing product-relevant moved
// is taken from the head the prior verdict actually names, so a verdict
// recorded for a head that is no longer reachable simply fails the git read
// and does not reuse.
//
// That diff is the ONLY way a reuse is accepted, including the decision-only
// re-run where the prior verdict names this very head: git reports an empty
// diff for a commit against itself, so the general path already reaches that
// decision. One acceptance path costs a git invocation against the turn this
// gate exists to avoid, and it fails in the safe direction - an unreadable
// repository declines the reuse and runs the agent rather than granting one
// without ever reading the tree.
//
// Same-intent is the fourth narrowing condition, and it is about what the
// verdict MEANS rather than what the product does. The evidence turn derives
// its scenarios from the run's user intent, and an --intent supplied one is
// AUTHORITATIVE acceptance criteria the change must satisfy, so a verdict
// earned under intent A says nothing about intent B even at a byte-identical
// product state: republishing it would publish go for criteria no scenario
// ever exercised. sameRunIntent therefore fails open on any absence.
//
// A fix round is the fifth, and it declines reuse outright. A Test fix round
// exists precisely because THIS run's own evidence reported a problem, and
// GetBranchTestEvidence deliberately excludes the current run, so every
// verdict reachable from here PREDATES the finding being fixed. The run's own
// verdict is the authoritative one about this branch; preferring an older
// run's go inverts the gate, and publishing a go that a live turn contradicted
// minutes earlier is worse than paying for the turn again. The
// no-product-file conclusion is untouched by this: it is a fact about the
// diff rather than a claim about a verdict, and it stays correct in a fix
// round.
func reusableBranchVerdict(sctx *pipeline.StepContext, nonProduct []string) (testEvidenceDecision, bool) {
	if sctx.DB == nil || sctx.Run == nil || sctx.Fixing {
		return testEvidenceDecision{}, false
	}
	prior, err := sctx.DB.GetBranchTestEvidence(sctx.Run.RepoID, sctx.Run.Branch, sctx.Run.ID)
	if err != nil {
		sctx.Log(fmt.Sprintf("could not read this branch's earlier test evidence (%v); running the live-evidence agent", err))
		return testEvidenceDecision{}, false
	}
	if prior == nil {
		return testEvidenceDecision{}, false
	}
	if !sameRunIntent(sctx.Run.Intent, prior.Intent) {
		return testEvidenceDecision{}, false
	}
	findings, parseErr := types.ParseFindingsJSON(prior.FindingsJSON)
	if parseErr != nil || findings.Verdict != types.TestVerdictGo || findings.TestedHeadSHA == "" {
		return testEvidenceDecision{}, false
	}
	product, diffErr := diffProductPaths(sctx.Ctx, sctx.WorkDir, nonProduct, findings.TestedHeadSHA+".."+sctx.Run.HeadSHA)
	if diffErr != nil {
		sctx.Log(fmt.Sprintf("could not diff against the head run %s validated (%v); running the live-evidence agent", prior.RunID, diffErr))
		return testEvidenceDecision{}, false
	}
	if len(product) > 0 {
		return testEvidenceDecision{}, false
	}
	return reuseDecision(prior.RunID, findings), true
}

// sameRunIntent reports whether two runs were validated against the same
// acceptance criteria. An absent intent on EITHER side is a difference, not a
// match: "no recorded intent" is an unknown, and two unknowns are not evidence
// of being the same. Whitespace is trimmed so reflowing the same text is not
// read as a changed criterion.
func sameRunIntent(current *string, prior string) bool {
	if current == nil {
		return false
	}
	mine := strings.TrimSpace(*current)
	return mine != "" && mine == strings.TrimSpace(prior)
}

func reuseDecision(priorRunID string, prior Findings) testEvidenceDecision {
	// The originating run is carried structurally rather than parsed back out
	// of the predecessor's reason: a reused verdict is itself reusable, and
	// naming the run that actually drove the agent keeps every link in a chain
	// pointing at one stable run instead of at whichever intermediate it
	// happened to read from. Intermediates do hold copies of the carried files
	// (carryArtifactFiles puts one in every reusing run's directory), but
	// which intermediate a verdict came through is an accident of scheduling,
	// and the origin is what the PR's provenance line has to name.
	origin := strings.TrimSpace(prior.EvidenceOriginRunID)
	if origin == "" {
		origin = priorRunID
	}
	reused := Findings{
		Scenarios: prior.Scenarios,
		// The artifacts come along so the PR can still SHOW the evidence
		// behind the verdict it publishes. gatedTestOutcome copies the
		// originating run's evidence directory into this run's before
		// publication, and clears this list if that is not possible, so the
		// body never cites a file it does not carry.
		Artifacts:           prior.Artifacts,
		EvidenceOriginRunID: origin,
		Verdict:             types.TestVerdictGo,
		// The head is the one whose product behavior these scenarios were
		// actually driven against, carried forward rather than restamped onto
		// this run's head. It is what keeps the reuse honest: every consumer
		// that asks "was THIS head live validated" compares against this
		// field, so restamping would make a run that drove nothing claim a
		// live turn. See gatedTestOutcome.
		TestedHeadSHA: prior.TestedHeadSHA,
	}
	return testEvidenceDecision{
		Source: types.TestEvidenceSourceReused,
		Reason: fmt.Sprintf(
			"product files unchanged since %s; reused from run %s",
			shortSHA(prior.TestedHeadSHA),
			origin,
		),
		Reused: reused,
	}
}

// carryOriginEvidence copies the originating run's evidence artifacts into this
// run's evidence directory, which is the only directory publication reads.
//
// The originating run's directory is its run ID under the same shared root as
// this run's (see Executor.runEvidenceDir). That path is resolved here and
// never published: it is a host filesystem path, the PR redactor only removes
// home directories, and test.evidence.local_root may be any absolute path.
func carryOriginEvidence(sctx *pipeline.StepContext, originRunID string, artifacts []types.TestArtifact) ([]types.TestArtifact, error) {
	originRunID = strings.TrimSpace(originRunID)
	if originRunID == "" {
		return nil, errors.New("no originating run recorded")
	}
	// The run ID becomes one path segment under the shared evidence root, so
	// it is validated as one rather than trusted. A findings payload is parsed
	// by the same function whether it came from the database or from an agent
	// turn, so a traversal here would let a turn name any directory on the
	// host and have its contents copied into a published evidence directory.
	// test.go clears the field on the agent path; this is the second gate.
	if originRunID != filepath.Base(originRunID) || originRunID == "." || originRunID == ".." || strings.ContainsAny(originRunID, `/\`) {
		return nil, fmt.Errorf("originating run %q is not a single path segment", originRunID)
	}
	dest := testEvidenceDir(sctx)
	if dest == "" {
		return nil, errors.New("this run has no evidence directory")
	}
	// A verdict evidenced only by url or content artifacts has no file to
	// copy, so it never depends on the originating directory at all.
	if !anyFileBackedArtifact(artifacts) {
		return artifacts, nil
	}
	return carryArtifactFiles(artifacts, filepath.Join(filepath.Dir(dest), originRunID), dest)
}

// anyFileBackedArtifact reports whether any artifact names a local file, which
// is the only kind the copy exists for.
func anyFileBackedArtifact(artifacts []types.TestArtifact) bool {
	for _, artifact := range artifacts {
		if strings.TrimSpace(artifact.Path) != "" {
			return true
		}
	}
	return false
}

// carryArtifactFiles copies the files the carried artifacts NAME, one at a
// time, and points each survivor at its copy in THIS run's evidence directory.
//
// Only the named files are copied, never the originating directory, because
// that directory is the run's evidence ROOT rather than an artifact bucket:
// the review conversation lives at <evidence dir>/review (reviewqa.Dir) and
// anything else the pipeline keeps per run lands there too, so a wholesale copy
// replaced this run's own state with an earlier run's - publishing a review
// conversation belonging to a different review. Nothing outside the artifact
// list is the carry's business.
//
// The paths matter as much as the files. The evidence prompt has the agent
// record paths exactly where it wrote them, so a file artifact is absolute
// under the run directory that produced it - and the PR renderer accepts an
// absolute artifact path only when it resolves under the repository root or
// under THIS run's evidence directory (sanitizeAbsoluteArtifactPath). Carried
// unchanged, every such path is dropped at render time and the carry silently
// does nothing. Note that evidenceRoot here is the SHARED root holding every
// run's directory (dest's parent), not prsummary.go's
// testingSummaryOptions.evidenceRoot, which is this one run's directory; the
// two meet only through sanitizeAbsoluteArtifactPath, which is why a carried
// path has to end up under dest.
//
// Which run directory a recorded path names cannot be assumed. On the third run
// of a branch the predecessor was itself a reuse, so its paths already name ITS
// directory rather than the originating one - the case the origin pointer
// exists for. So a path is reduced to its remainder below the run-directory
// segment, which is the same remainder under the originating run's directory,
// and the copy's own success is then what decides whether it can be cited:
// that makes the carry correct for any predecessor without reasoning about
// which one it was, and it is what "never cite a file this PR does not carry"
// actually requires.
//
// Only a path artifact needs a file. The prompt offers url and content
// artifacts too, and the renderer shows those with nothing on disk, so they
// are carried untouched and a verdict evidenced entirely that way needs no
// copy at all.
func carryArtifactFiles(artifacts []types.TestArtifact, originDir, dest string) ([]types.TestArtifact, error) {
	evidenceRoot := filepath.Dir(dest)
	kept := make([]types.TestArtifact, 0, len(artifacts))
	copied := map[string]bool{}
	fileBacked, survived := 0, 0
	// The first copy failure is what an operator asks about - most often the
	// originating directory aged out under test.evidence.retention - so it is
	// carried into the rejection rather than swallowed with the artifact.
	var firstErr error
	for _, artifact := range artifacts {
		recorded := strings.TrimSpace(artifact.Path)
		if recorded == "" {
			// A url or content artifact: nothing to copy, nothing to rebase.
			kept = append(kept, artifact)
			continue
		}
		fileBacked++
		rel, ok := carriedEvidenceRelPath(recorded, evidenceRoot)
		if !ok {
			continue
		}
		dstPath := filepath.Join(dest, rel)
		if !copied[rel] {
			if err := copyEvidenceFile(filepath.Join(originDir, rel), dstPath); err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			copied[rel] = true
		}
		if !regularFileAt(dstPath) {
			continue
		}
		if filepath.IsAbs(recorded) {
			artifact.Path = dstPath
		}
		kept = append(kept, artifact)
		survived++
	}
	// A carry "failed" only when files were expected and none of them arrived.
	if fileBacked > 0 && survived == 0 {
		if firstErr != nil {
			return nil, fmt.Errorf("no carried file artifact arrived in this run's evidence directory: %w", firstErr)
		}
		return nil, errors.New("no carried file artifact arrived in this run's evidence directory")
	}
	return kept, nil
}

// carriedEvidenceRelPath reduces one recorded artifact path to its location
// inside a run's evidence directory, reporting false for anything that does not
// sit inside one. A relative path is already relative to that directory; an
// absolute one is taken relative to the shared evidence root with its
// run-directory segment dropped.
func carriedEvidenceRelPath(recorded, evidenceRoot string) (string, bool) {
	if !filepath.IsAbs(recorded) {
		rel := filepath.Clean(recorded)
		if rel == "." || escapesDir(rel) {
			return "", false
		}
		return rel, true
	}
	rel, err := filepath.Rel(evidenceRoot, filepath.Clean(recorded))
	if err != nil || escapesDir(rel) {
		// Outside the evidence root entirely: no run directory holds it.
		return "", false
	}
	segments := strings.Split(rel, string(filepath.Separator))
	if len(segments) < 2 {
		// The evidence root itself, or a bare run directory, names no file.
		return "", false
	}
	return filepath.Join(segments[1:]...), true
}

// escapesDir reports whether a cleaned relative path leads out of the directory
// it is relative to.
func escapesDir(rel string) bool {
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// copyEvidenceFile copies one evidence file, refusing anything that is not a
// regular file and refusing to write a destination that already exists.
//
// Both refusals are security ones. Evidence is written by the live-evidence
// agent, so a link in that directory is agent-influenced input; copied here it
// would survive into a directory the pipeline PUBLISHES, and the media upload
// reads artifact files by path, so a link to any readable host file would put
// that file's contents in a public pull request. The same argument rules out
// other irregular entries: a device or fifo has no place in evidence and a
// reader could block on one. And the destination is opened with O_EXCL rather
// than truncated: the recorded paths come from another run, and whatever THIS
// run already wrote at one of them is not the carry's to destroy.
func copyEvidenceFile(srcPath, dstPath string) error {
	info, err := os.Lstat(srcPath)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", srcPath)
	}
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o700); err != nil {
		return err
	}
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(dstPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return err
	}
	defer dst.Close()
	if _, err := io.Copy(dst, src); err != nil {
		return err
	}
	return dst.Chmod(info.Mode().Perm())
}

// regularFileAt reports whether path is a real file rather than a link,
// directory, or device. os.Stat would FOLLOW a link and report on its target,
// so an artifact pointing through one would read as backed and be published;
// copyEvidenceFile already declines to carry a link, and this is the second
// gate on citing one.
func regularFileAt(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular()
}

// gatedTestOutcome builds the step outcome for a run whose live-evidence agent
// was skipped. It is the tail of TestStep.Execute minus the analyzer: the
// configured command's own result still decides the exit code and its
// findings, new test files a fix round wrote are still recorded, and the
// recorded verdict still runs through verdictFindings so a future verdict that
// must park still parks.
//
// newTestsFromFix is what the fix turn saw before commitAgentFixes ran, and it
// cannot be recomputed here: detectNewTestFiles reads uncommitted status only,
// so by this point the fixer's own files are committed and invisible to it.
func gatedTestOutcome(
	sctx *pipeline.StepContext,
	gate testEvidenceDecision,
	tested []string,
	baselineFindings []Finding,
	baselineSummary string,
	baselineExitCode int,
	fixSummary string,
	newTestsFromFix []string,
) (*pipeline.StepOutcome, error) {
	findings := gate.Reused
	if gate.Source == types.TestEvidenceSourceNoProductChange {
		findings.Verdict = types.TestVerdictNoSurface
	}
	findings.Tested = append([]string(nil), tested...)
	// Only stamp this run's head when the decision does not already name the
	// head its evidence belongs to. A reused verdict carries the head its
	// scenarios were driven at, and that difference is load-bearing:
	// attestedLiveValidation omits live_validation entirely when the recorded
	// head is not the published one, so a run that drove nothing publishes no
	// live-validation claim rather than a restamped one. An automatic
	// no-surface DOES belong to this head - it is a fact about this diff, and
	// it carries no scenarios - so it takes the stamp.
	if findings.TestedHeadSHA == "" {
		findings.TestedHeadSHA = sctx.Run.HeadSHA
	}
	findings.EvidenceSource = gate.Source
	findings.EvidenceReason = gate.Reason
	// A reused verdict publishes the scenarios it was earned with, so the PR
	// has to be able to SHOW their evidence: a verdict with nothing visible
	// behind it is a worse claim than a stale one. The artifacts live in the
	// originating run's evidence directory, and publication only ever reads
	// THIS run's, so they are copied across here, before the PR step runs.
	// Failing that, the artifact list is cleared and the renderer drops the
	// scenario table with it, so the body never cites a file it does not
	// carry. Retention (default 14 days) ages evidence directories out, so a
	// missing source is expected rather than exceptional.
	// Only a verdict that HAS file artifacts needs them carried. One that
	// evidenced its scenarios with commands rather than files has nothing to
	// copy and nothing to suppress, so its table renders as it always did.
	if len(findings.Artifacts) > 0 {
		carried, err := carryOriginEvidence(sctx, findings.EvidenceOriginRunID, findings.Artifacts)
		if err != nil {
			// Nothing backs the scenarios here, so they go with their
			// artifacts: the verdict line and the reuse reason still name the
			// run that holds the evidence, and no table cites a file this PR
			// cannot show. Retention ages evidence directories out, so this
			// is an ordinary outcome rather than an error for the run.
			sctx.Log(fmt.Sprintf("could not carry run %s's evidence forward (%v); publishing the verdict without its scenario table", findings.EvidenceOriginRunID, err))
			findings.Artifacts = nil
			findings.Scenarios = nil
		} else {
			findings.Artifacts = carried
		}
	}
	findings.TestingSummary = gate.Reason
	findings.Summary = baselineSummary
	findings.Items = append(append([]Finding(nil), baselineFindings...), verdictFindings(findings)...)

	for _, f := range mergeNewTestFiles(newTestsFromFix, detectNewTestFiles(sctx.Ctx, sctx.WorkDir)) {
		findings.Items = append(findings.Items, Finding{
			Severity:    types.FindingSeverityInfo,
			Action:      types.ActionNoOp,
			File:        f,
			Description: fmt.Sprintf("new test file written by agent: %s", f),
		})
	}

	needsApproval := hasBlockingFindings(findings.Items)
	findingsJSON, err := json.Marshal(findings)
	if err != nil {
		return nil, fmt.Errorf("marshal test findings: %w", err)
	}
	return &pipeline.StepOutcome{
		NeedsApproval: needsApproval,
		AutoFixable:   needsApproval,
		Findings:      string(findingsJSON),
		ExitCode:      baselineExitCode,
		FixSummary:    fixSummary,
	}, nil
}
