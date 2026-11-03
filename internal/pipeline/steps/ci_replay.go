package steps

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// ciCarriesReviewOnReplay reports whether a CI rebase repair may keep the run's
// review approval when it is a provable pure replay. It is the resolved
// ci.carry_review_on_replay policy, and it never applies under
// ci.revalidate_repairs, which keeps meaning "every repair re-passes Review".
func ciCarriesReviewOnReplay(sctx *pipeline.StepContext) bool {
	return sctx.Config != nil && sctx.Config.CI.CarryReviewOnReplay && !sctx.Config.CI.RevalidateRepairs
}

// reviewAnchoredGate returns the first repository gate that runs after Review,
// or "". Carrying the approval restarts validation at Test, so such a gate
// would be skipped for the replayed head; its presence keeps the full restart.
func reviewAnchoredGate(sctx *pipeline.StepContext) string {
	if sctx.Config == nil {
		return ""
	}
	for _, gate := range sctx.Config.Gates {
		if gate.After == types.StepReview {
			return gate.Name
		}
	}
	return ""
}

// pureReplayGap returns why replayedHead cannot be proven to be a pure replay
// of the commits approved at reviewedHead onto base, or "" when it can.
//
// A pure replay is exactly what an unattended `git rebase base` produces when
// no commit stops: the reviewed commits (the linear range from their merge
// base with base up to reviewedHead) reappear in order as a linear chain whose
// first parent is base and whose tip is replayedHead, and for every pair:
//
//   - the reviewed commit replays onto its new parent with no conflict, to
//     exactly the tree the replayed commit records (`git merge-tree`), so no
//     conflict was resolved and nothing was added, dropped, or edited; and
//   - the two commits are patch-identical (`git patch-id --verbatim` over the
//     full binary diff, so whitespace, mode, and binary changes all count).
//
// Every other shape - a different commit count, a merge anywhere in either
// range, a new base other than base, an empty commit, a conflict, a changed
// patch, an extra commit on top - is not a pure replay. So is any git error
// while checking: the cost of a wrong yes is an unreviewed diff reaching the
// PR, so the proof fails closed.
func pureReplayGap(sctx *pipeline.StepContext, reviewedHead, base, replayedHead string) string {
	base = strings.TrimSpace(base)
	if base == "" {
		return "the rebase target could not be resolved"
	}
	resolvedBase, err := stepGitRun(sctx, "rev-parse", "--verify", base+"^{commit}")
	if err != nil {
		return fmt.Sprintf("the rebase target %s could not be resolved", shortObjectID(base))
	}
	forkPoint, err := stepGitRun(sctx, "merge-base", reviewedHead, resolvedBase)
	if err != nil {
		return "the reviewed head shares no history with the rebase target"
	}
	reviewed, err := linearChain(sctx, forkPoint, reviewedHead)
	if err != nil {
		return fmt.Sprintf("the reviewed commits are not a linear chain: %v", err)
	}
	replayed, err := linearChain(sctx, resolvedBase, replayedHead)
	if err != nil {
		return fmt.Sprintf("the repaired head is not a linear chain on the rebase target %s: %v", shortObjectID(resolvedBase), err)
	}
	if len(reviewed) == 0 {
		return "the reviewed head has no commits above the rebase target"
	}
	if len(reviewed) != len(replayed) {
		return fmt.Sprintf("%d reviewed commits but %d commits on the repaired head", len(reviewed), len(replayed))
	}
	parent := resolvedBase
	for i := range reviewed {
		if reason := replayedCommitGap(sctx, reviewed[i], replayed[i], parent); reason != "" {
			return reason
		}
		parent = replayed[i]
	}
	return ""
}

// linearChain lists the commits in from..to oldest first and fails unless they
// form a single parent chain that starts on from and ends at to.
func linearChain(sctx *pipeline.StepContext, from, to string) ([]string, error) {
	out, err := stepGitRun(sctx, "rev-list", "--reverse", "--topo-order", "--parents", from+".."+to)
	if err != nil {
		return nil, err
	}
	var chain []string
	parent := from
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 {
			return nil, fmt.Errorf("commit %s has %d parents", shortObjectID(fields[0]), len(fields)-1)
		}
		if fields[1] != parent {
			return nil, fmt.Errorf("commit %s does not build on %s", shortObjectID(fields[0]), shortObjectID(parent))
		}
		chain = append(chain, fields[0])
		parent = fields[0]
	}
	if len(chain) > 0 && parent != strings.TrimSpace(to) {
		resolved, err := stepGitRun(sctx, "rev-parse", "--verify", to+"^{commit}")
		if err != nil || resolved != parent {
			return nil, fmt.Errorf("the chain does not end at %s", shortObjectID(to))
		}
	}
	return chain, nil
}

// replayedCommitGap proves one pair of a pure replay; see pureReplayGap.
func replayedCommitGap(sctx *pipeline.StepContext, reviewed, replayed, newParent string) string {
	wantTree, err := stepGitRun(sctx, "merge-tree", "--write-tree", "--merge-base="+reviewed+"^", newParent, reviewed)
	if err != nil {
		return fmt.Sprintf("reviewed commit %s does not replay onto %s without a conflict", shortObjectID(reviewed), shortObjectID(newParent))
	}
	gotTree, err := stepGitRun(sctx, "rev-parse", "--verify", replayed+"^{tree}")
	if err != nil || firstLine(wantTree) != gotTree {
		return fmt.Sprintf("commit %s is not the conflict-free replay of reviewed commit %s", shortObjectID(replayed), shortObjectID(reviewed))
	}
	reviewedID, err := commitPatchID(sctx, reviewed)
	if err != nil || reviewedID == "" {
		return fmt.Sprintf("reviewed commit %s has no patch identity", shortObjectID(reviewed))
	}
	replayedID, err := commitPatchID(sctx, replayed)
	if err != nil || replayedID != reviewedID {
		return fmt.Sprintf("commit %s is not patch-identical to reviewed commit %s", shortObjectID(replayed), shortObjectID(reviewed))
	}
	return ""
}

// commitPatchID returns `git patch-id --verbatim` of one commit's full diff
// against its parent, or "" for an empty commit. --verbatim keeps whitespace;
// --binary and --full-index make binary content part of the identity, and
// external diff drivers and textconv are off so local config cannot change it.
func commitPatchID(sctx *pipeline.StepContext, commit string) (string, error) {
	diff, err := stepGitRunRaw(sctx, "diff-tree", "-p", "--no-commit-id", "--full-index", "--binary", "--no-renames", "--no-ext-diff", "--no-textconv", commit)
	if err != nil {
		return "", err
	}
	if diff == "" {
		return "", nil
	}
	cmd := stepCmd(sctx, "git", "patch-id", "--verbatim")
	cmd.Env = git.NonInteractiveEnvFrom(cmd.Env, sctx.WorkDir)
	cmd.Stdin = strings.NewReader(diff)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git patch-id: %w", err)
	}
	return firstField(stdout.String()), nil
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(line)
}

func firstField(s string) string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}
