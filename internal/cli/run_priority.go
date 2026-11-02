package cli

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// priorityPushOptionPrefix carries a run's admission priority (axi run
// --priority) through a git push to the gate's receive hook. A raw
// `git push -o no-mistakes.priority=N` sets it the same way.
const priorityPushOptionPrefix = "no-mistakes.priority="

// formatPriorityPushOption encodes a run priority as a push option. An
// ordinary run formats to no option at all, so its push is unchanged.
func formatPriorityPushOption(priority int) string {
	if priority == types.RunPriorityOrdinary {
		return ""
	}
	return priorityPushOptionPrefix + strconv.Itoa(priority)
}

// parsePriorityPushOptions extracts the run priority push option. The last
// occurrence wins, like the other single-valued options, and a malformed or
// out-of-range value refuses the push notification instead of silently
// queueing the run as ordinary.
func parsePriorityPushOptions(options []string) (int, error) {
	priority := types.RunPriorityOrdinary
	for _, option := range options {
		value, ok := strings.CutPrefix(option, priorityPushOptionPrefix)
		if !ok {
			continue
		}
		parsed, err := parseRunPriority(value)
		if err != nil {
			return 0, fmt.Errorf("priority push option: %w", err)
		}
		priority = parsed
	}
	return priority, nil
}

func parseRunPriority(value string) (int, error) {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("priority must be an integer from %d to %d, got %q", types.RunPriorityOrdinary, types.RunPriorityMax, value)
	}
	if err := types.ValidateRunPriority(parsed); err != nil {
		return 0, err
	}
	return parsed, nil
}

// bindRunPriorityFlag registers --priority on a run-starting command.
func bindRunPriorityFlag(cmd *cobra.Command) {
	cmd.Flags().Int("priority", types.RunPriorityOrdinary, fmt.Sprintf("admission priority of a new run among pending runs (%d-%d, default %d = ordinary); a higher priority starts first when a max_concurrent_runs slot frees, without displacing running work", types.RunPriorityOrdinary, types.RunPriorityMax, types.RunPriorityOrdinary))
}

// runPriorityFromFlags returns the requested priority and whether the caller
// set the flag at all, so a reattach can tell "not asked" from "ordinary".
func runPriorityFromFlags(cmd *cobra.Command) (int, bool, error) {
	flag := cmd.Flags().Lookup("priority")
	if flag == nil || !flag.Changed {
		return types.RunPriorityOrdinary, false, nil
	}
	priority, err := cmd.Flags().GetInt("priority")
	if err != nil {
		return 0, true, err
	}
	if err := types.ValidateRunPriority(priority); err != nil {
		return 0, true, fmt.Errorf("--priority: %w", err)
	}
	return priority, true, nil
}

// conflictingActiveRunPriority refuses an explicit --priority that a reattach
// would discard: the active run keeps the priority it was submitted with, so
// the caller must not believe it queued at another one.
func conflictingActiveRunPriority(run *ipc.RunInfo, requested int, set bool) error {
	if !set || run == nil || run.Priority == requested {
		return nil
	}
	return fmt.Errorf("active run %s was submitted with priority %d, not %d", run.ID, run.Priority, requested)
}
