package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
)

// recordCanaryCohort offers a completed run to the routing canary. It is
// advisory-only observability: it never affects the run outcome, and does
// nothing until the routing cutover activates the policy (the canary stays
// dormant while IsCanaryActivated is false). Changed file/line counts are
// computed from the run's diff where available.
func (e *Executor) recordCanaryCohort(ctx context.Context, run *db.Run, workDir string) {
	activated, err := e.db.IsCanaryActivated()
	if err != nil || !activated {
		return
	}
	files, lines := canaryChangedStats(ctx, workDir, run.BaseSHA, run.HeadSHA)
	if _, err := e.db.RecordRoutedRunInCanary(run.ID, files, lines); err != nil {
		slog.Warn("canary intake failed", "run", run.ID, "error", err)
	}
}

// maybeActivateCanary freezes the routing canary before the first clean routed
// gate can be accepted. The activation transaction persists the routing
// fingerprint and the durable completion fence together with the frozen
// pre-routing baseline. Any activation check or persistence failure is returned
// so the caller can reject completion rather than admitting an untracked routed
// result.
func (e *Executor) maybeActivateCanary(ctx context.Context, run *db.Run, workDir string) error {
	activated, err := e.db.IsCanaryActivated()
	if err != nil {
		return fmt.Errorf("check canary activation: %w", err)
	}
	if activated {
		return nil
	}
	fingerprint := ""
	if e.config != nil {
		fingerprint = e.config.Routing.Fingerprint()
	}
	changed := func(baseSHA, headSHA string) (int, int, bool) {
		files, lines := canaryChangedStats(ctx, workDir, baseSHA, headSHA)
		if files < 0 {
			return 0, 0, false
		}
		return files, lines, true
	}
	if _, err := e.db.ActivateCanary(fingerprint, changed); err != nil {
		return fmt.Errorf("persist canary activation: %w", err)
	}
	return nil
}

// canaryChangedStats returns the changed-file and changed-line counts for a
// run's diff, or (-1, -1) when the diff is unavailable. Binary files count
// toward files but contribute no line deltas.
func canaryChangedStats(ctx context.Context, workDir, baseSHA, headSHA string) (files, lines int) {
	if baseSHA == "" || headSHA == "" {
		return -1, -1
	}
	out, err := git.Run(ctx, workDir, "diff", "--numstat", baseSHA+".."+headSHA)
	if err != nil {
		return -1, -1
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 {
			continue
		}
		files++
		if add, err := strconv.Atoi(fields[0]); err == nil {
			lines += add
		}
		if del, err := strconv.Atoi(fields[1]); err == nil {
			lines += del
		}
	}
	return files, lines
}
