package cli

import (
	"encoding/hex"
	"fmt"
	"strings"

	toon "github.com/toon-format/toon-go"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/spf13/cobra"
)

// branchStepRow separates an unrecorded required step from a pending step.
// The pinned plan, not current repository configuration, owns row membership.
type branchStepRow struct {
	Position int    `toon:"position"`
	Step     string `toon:"step"`
	Recorded bool   `toon:"recorded"`
	Status   any    `toon:"status"`
}

type branchRunRow struct {
	ID          string  `toon:"id"`
	CreatedAt   int64   `toon:"created_at"`
	Status      string  `toon:"status"`
	CurrentStep *string `toon:"current_step"`
}

func runAxiBranchStatus(cmd *cobra.Command, branch string) error {
	// --branch is an exact ref name, not a revision expression or a request to
	// resolve the checked-out branch. check-ref-format reads no remote state.
	if branch == "" || strings.TrimSpace(branch) != branch || branch == "HEAD" || strings.HasPrefix(branch, "-") {
		return emitError(cmd, 2, "--branch must name an exact branch")
	}
	if _, err := git.Run(cmd.Context(), ".", "check-ref-format", "refs/heads/"+branch); err != nil {
		return emitError(cmd, 2, "--branch must name a valid branch ref")
	}
	p, err := paths.New()
	if err != nil {
		return emitError(cmd, 1, err.Error())
	}
	// Unlike pipeline-control resources, this query never creates directories
	// or migrates storage. Missing or incompatible storage is an error.
	database, err := db.OpenReadOnly(p.DB())
	if err != nil {
		return emitError(cmd, 1, fmt.Sprintf("open branch snapshot: %v", err))
	}
	defer database.Close()
	repo, err := findRepo(database)
	if err != nil {
		return emitError(cmd, 1, err.Error(), repoInitHelp(err)...)
	}
	snapshot, err := database.GetBranchSnapshot(cmd.Context(), repo.ID, branch)
	if err != nil {
		return emitError(cmd, 1, err.Error())
	}
	fields, err := branchStatusFields(repo.ID, branch, snapshot)
	if err != nil {
		return emitError(cmd, 1, fmt.Sprintf("invalid branch snapshot: %v", err))
	}
	emitDoc(cmd, fields...)
	return nil
}

func branchStatusFields(repoID, branch string, snapshot []db.BranchRunSnapshot) ([]toon.Field, error) {
	inventory := make([]branchRunRow, 0, len(snapshot))
	others := make([]string, 0)
	var newestID any
	var selected any
	seen := make(map[string]bool, len(snapshot))
	for i, item := range snapshot {
		r := item.Run
		if err := validateBranchRun(r, repoID, branch); err != nil {
			return nil, err
		}
		if seen[r.ID] {
			return nil, fmt.Errorf("duplicate run %q", r.ID)
		}
		seen[r.ID] = true
		if i > 0 {
			previous := snapshot[i-1].Run
			if r.CreatedAt > previous.CreatedAt || (r.CreatedAt == previous.CreatedAt && r.ID >= previous.ID) {
				return nil, fmt.Errorf("runs are not in creation order")
			}
		}
		plan, current, err := branchPlan(item)
		if err != nil {
			return nil, fmt.Errorf("run %s: %w", r.ID, err)
		}
		inventory = append(inventory, branchRunRow{r.ID, r.CreatedAt, string(r.Status), current})
		if i > 0 && (r.Status == types.RunRunning || r.Status == types.RunPending) {
			others = append(others, r.ID)
		}
		if i == 0 {
			newestID = r.ID
			var outcome any
			if r.Status.Terminal() {
				// A nil database keeps this conversion confined to snapshot data.
				outcome = outcomeForRun(runViewFromDB(r, item.Steps, nil))
			}
			selected = toon.Object{Fields: []toon.Field{
				{Key: "id", Value: r.ID},
				{Key: "repository_id", Value: r.RepoID},
				{Key: "branch", Value: r.Branch},
				{Key: "created_at", Value: r.CreatedAt},
				{Key: "status", Value: string(r.Status)},
				{Key: "current_step", Value: nullableValue(current)},
				{Key: "outcome", Value: outcome},
				{Key: "head_sha", Value: r.HeadSHA},
				{Key: "reviewed_head_sha", Value: nullableValue(r.ReviewApprovedHeadSHA)},
				{Key: "last_pushed_sha", Value: nullableValue(r.LastPushedSHA)},
				{Key: "last_pushed_at", Value: nullableValue(r.LastPushedAt)},
				{Key: "push_generation", Value: nullableValue(r.PushGeneration)},
				{Key: "push_active", Value: r.PushActive},
				{Key: "push_target_kind", Value: nullableValue(r.PushTargetKind)},
				{Key: "push_target_fingerprint", Value: nullableValue(r.PushTargetFingerprint)},
				{Key: "push_ref", Value: nullableValue(r.PushRef)},
				{Key: "pr", Value: nullableValue(r.PRURL)},
				{Key: "steps", Value: plan},
			}}
		}
	}
	return []toon.Field{
		{Key: "schema", Value: "branch-publication.v1"},
		{Key: "scope", Value: toon.NewObject(toon.Field{Key: "repository_id", Value: repoID}, toon.Field{Key: "branch", Value: branch})},
		{Key: "inventory_complete", Value: true},
		{Key: "creation_order", Value: "created_at DESC, id DESC"},
		{Key: "newest_run_id", Value: newestID},
		{Key: "run", Value: selected},
		{Key: "runs", Value: inventory},
		{Key: "other_running_run_ids", Value: others},
	}, nil
}

func nullableValue[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

func branchPlan(item db.BranchRunSnapshot) ([]branchStepRow, *string, error) {
	gates, err := config.ParseGates(item.GatesJSON)
	if err != nil {
		return nil, nil, err
	}
	plan := make([]branchStepRow, 0, len(types.AllSteps())+len(gates))
	for _, core := range types.AllSteps() {
		plan = append(plan, branchStepRow{Step: string(core)})
		for _, gate := range gates {
			if gate.After == core {
				plan = append(plan, branchStepRow{Step: string(gate.StepName())})
			}
		}
	}
	byName := make(map[types.StepName]*db.StepResult, len(item.Steps))
	for _, step := range item.Steps {
		if step == nil || step.ID == "" || step.RunID != item.Run.ID || step.StepOrder != step.StepName.Order() {
			return nil, nil, fmt.Errorf("malformed step record")
		}
		if byName[step.StepName] != nil {
			return nil, nil, fmt.Errorf("duplicate step %q", step.StepName)
		}
		switch step.Status {
		case types.StepStatusPending, types.StepStatusRunning, types.StepStatusAwaitingApproval, types.StepStatusFixing, types.StepStatusFixReview, types.StepStatusCompleted, types.StepStatusSkipped, types.StepStatusFailed:
		default:
			return nil, nil, fmt.Errorf("unknown step status %q", step.Status)
		}
		byName[step.StepName] = step
	}
	var active, pending *string
	for i := range plan {
		row := &plan[i]
		row.Position = i + 1
		step := byName[types.StepName(row.Step)]
		if step == nil {
			continue
		}
		delete(byName, types.StepName(row.Step))
		row.Recorded, row.Status = true, string(step.Status)
		name := string(row.Step)
		switch step.Status {
		case types.StepStatusRunning, types.StepStatusFixing, types.StepStatusAwaitingApproval, types.StepStatusFixReview:
			if active != nil {
				return nil, nil, fmt.Errorf("multiple active steps")
			}
			active = &name
		case types.StepStatusPending:
			if pending == nil {
				pending = &name
			}
		}
	}
	if len(byName) != 0 {
		return nil, nil, fmt.Errorf("step is absent from pinned plan")
	}
	if item.Run.Status.Terminal() {
		return plan, nil, nil
	}
	if active != nil {
		return plan, active, nil
	}
	return plan, pending, nil
}

func validateBranchRun(r *db.Run, repoID, branch string) error {
	if r == nil || r.ID == "" || r.RepoID != repoID || r.Branch != branch || r.CreatedAt <= 0 {
		return fmt.Errorf("malformed run identity or creation time")
	}
	switch r.Status {
	case types.RunPending, types.RunRunning, types.RunCompleted, types.RunFailed, types.RunCancelled, types.RunCIMonitorInterrupted:
	default:
		return fmt.Errorf("run %s: unknown status %q", r.ID, r.Status)
	}
	for _, sha := range []*string{&r.HeadSHA, r.ReviewApprovedHeadSHA, r.LastPushedSHA} {
		if sha != nil && (!fullObjectID(*sha) || strings.Trim(*sha, "0") == "") {
			return fmt.Errorf("run %s: malformed head SHA", r.ID)
		}
	}
	if r.PushTargetKind != nil && *r.PushTargetKind != "upstream" && *r.PushTargetKind != "fork" {
		return fmt.Errorf("run %s: unknown push target kind", r.ID)
	}
	if r.PushTargetFingerprint != nil && (len(*r.PushTargetFingerprint) != 64 || !hexValue(*r.PushTargetFingerprint)) {
		return fmt.Errorf("run %s: malformed push target fingerprint", r.ID)
	}
	if r.PushRef != nil && *r.PushRef != "refs/heads/"+branch {
		return fmt.Errorf("run %s: push ref does not match branch", r.ID)
	}
	if (r.LastPushedAt != nil && *r.LastPushedAt <= 0) || (r.PushGeneration != nil && *r.PushGeneration <= 0) {
		return fmt.Errorf("run %s: malformed publication time or generation", r.ID)
	}
	return nil
}

func fullObjectID(s string) bool { return (len(s) == 40 || len(s) == 64) && hexValue(s) }

func hexValue(s string) bool {
	_, err := hex.DecodeString(s)
	return err == nil
}
