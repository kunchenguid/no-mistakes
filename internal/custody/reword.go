package custody

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/git"
)

var commitID = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// VerifyReword proves that only commit metadata changed after submitted: both
// histories are linear, start at that exact commit, and have the same tree at
// every position. Equal final trees alone would admit dropped intermediate work.
// This proof grants neither review approval nor permission to publish.
func VerifyReword(ctx context.Context, dir, submitted, recorded, rewritten string) error {
	if recorded == rewritten {
		return fmt.Errorf("the rewritten head must differ from the recorded head")
	}
	for _, head := range []string{submitted, recorded, rewritten} {
		if !commitID.MatchString(head) {
			return fmt.Errorf("reword proof requires full commit IDs")
		}
		resolved, err := git.Run(ctx, dir, "rev-parse", "--verify", head+"^{commit}")
		if err != nil || resolved != head {
			return fmt.Errorf("reword commit %s is unavailable", head)
		}
	}
	oldTrees, err := linearTrees(ctx, dir, submitted, recorded)
	if err != nil {
		return err
	}
	newTrees, err := linearTrees(ctx, dir, submitted, rewritten)
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

func linearTrees(ctx context.Context, dir, submitted, head string) ([]string, error) {
	if _, err := git.Run(ctx, dir, "merge-base", "--is-ancestor", submitted, head); err != nil {
		return nil, fmt.Errorf("reword must preserve the submitted commit as an ancestor")
	}
	// Bound external output, refusing rather than certifying a truncated chain.
	log, err := git.Run(ctx, dir, "log", "--reverse", "--max-count=257", "--format=%H %T %P", submitted+".."+head)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(log), "\n")
	if log == "" || len(lines) > 256 {
		return nil, fmt.Errorf("reword requires between 1 and 256 linear commits")
	}
	parent := submitted
	trees := make([]string, 0, len(lines))
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) != 3 || fields[2] != parent {
			return nil, fmt.Errorf("reword history is not a linear chain from the submitted head")
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
