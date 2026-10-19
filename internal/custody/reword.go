package custody

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
)

var commitID = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// VerifyReword proves that only commit metadata changed after rebased, the
// head the run's own Rebase step recorded: both histories are linear, start at
// that exact commit, and have the same tree at every position. Equal final
// trees alone would admit dropped intermediate work. This proof grants neither
// review approval nor permission to publish.
func VerifyReword(ctx context.Context, dir, rebased, recorded, rewritten string) error {
	if recorded == rewritten {
		return fmt.Errorf("the rewritten head must differ from the recorded head")
	}
	for _, head := range []string{rebased, recorded, rewritten} {
		if !commitID.MatchString(head) {
			return fmt.Errorf("reword proof requires full commit IDs")
		}
		resolved, err := git.Run(ctx, dir, "rev-parse", "--verify", head+"^{commit}")
		if err != nil || resolved != head {
			return fmt.Errorf("reword commit %s is unavailable", head)
		}
	}
	oldTrees, err := linearTrees(ctx, dir, rebased, recorded)
	if err != nil {
		return err
	}
	newTrees, err := linearTrees(ctx, dir, rebased, rewritten)
	if err != nil {
		return err
	}
	if len(oldTrees) != len(newTrees) {
		return fmt.Errorf("reword changed the number of commits")
	}
	for i := range oldTrees {
		if oldTrees[i] != newTrees[i] {
			return fmt.Errorf("reword changed the tree at commit %d", i+1)
		}
	}
	return nil
}

func linearTrees(ctx context.Context, dir, rebased, head string) ([]string, error) {
	if !isAncestor(ctx, dir, rebased, head) {
		return nil, fmt.Errorf("reword must preserve the recorded post-rebase head as an ancestor")
	}
	// Bound external output, refusing rather than certifying a truncated chain.
	log, err := git.Run(ctx, dir, "log", "--reverse", "--max-count=257", "--format=%H %T %P", rebased+".."+head)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(log), "\n")
	if log == "" || len(lines) > 256 {
		return nil, fmt.Errorf("reword requires between 1 and 256 linear commits")
	}
	parent := rebased
	trees := make([]string, 0, len(lines))
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) != 3 || fields[2] != parent {
			return nil, fmt.Errorf("reword history is not a linear chain from the recorded post-rebase head")
		}
		parent = fields[0]
		trees = append(trees, fields[1])
	}
	return trees, nil
}

// RewordRef retains each history independently, including repeated rewords in
// one run. It is preservation evidence only, never review authority.
func RewordRef(runID, head string) string {
	return "refs/no-mistakes/reword/" + runID + "/" + head
}

func PreserveReword(ctx context.Context, dir, runID, recorded, rewritten string) error {
	for _, head := range []string{recorded, rewritten} {
		if err := PreserveRecoveryAnchor(ctx, dir, RewordRef(runID, head), head); err != nil {
			return err
		}
	}
	return nil
}

// RemapRewordedRange carries the branch's uncertified fixer range across a
// proven reword: a commit after rebased maps to the rewritten commit at the
// same position, anything at or below rebased or already in the rewritten
// history is kept. A missing or unrelated range is the reworded span itself.
func RemapRewordedRange(ctx context.Context, database *db.DB, dir, repoID, branch, runID, rebased, recorded, rewritten string) error {
	rng, err := database.GetUncertifiedPipelineRange(repoID, branch)
	if err != nil {
		return err
	}
	from, to, source := rebased, recorded, runID
	if rng != nil && (isAncestor(ctx, dir, rng.ToSHA, recorded) || isAncestor(ctx, dir, rng.ToSHA, rewritten)) {
		from, to, source = rng.FromSHA, rng.ToSHA, rng.SourceRunID
	}
	if from, err = rewordPosition(ctx, dir, rebased, recorded, rewritten, from); err != nil {
		return err
	}
	if to, err = rewordPosition(ctx, dir, rebased, recorded, rewritten, to); err != nil {
		return err
	}
	return database.UpsertUncertifiedPipelineRange(repoID, branch, from, to, source)
}

func rewordPosition(ctx context.Context, dir, rebased, recorded, rewritten, sha string) (string, error) {
	if isAncestor(ctx, dir, sha, rebased) || isAncestor(ctx, dir, sha, rewritten) {
		return sha, nil
	}
	if !isAncestor(ctx, dir, sha, recorded) {
		return "", fmt.Errorf("uncertified fixer commit %s is outside the reworded history", sha)
	}
	count, err := git.Run(ctx, dir, "rev-list", "--count", sha+".."+recorded)
	if err != nil {
		return "", err
	}
	n, err := strconv.Atoi(strings.TrimSpace(count))
	if err != nil {
		return "", err
	}
	return git.Run(ctx, dir, "rev-parse", "--verify", fmt.Sprintf("%s~%d^{commit}", rewritten, n))
}

func isAncestor(ctx context.Context, dir, ancestor, head string) bool {
	if strings.TrimSpace(ancestor) == "" {
		return false
	}
	_, err := git.Run(ctx, dir, "merge-base", "--is-ancestor", ancestor, head)
	return err == nil
}
