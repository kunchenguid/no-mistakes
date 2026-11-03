package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"reflect"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/spf13/cobra"
	toon "github.com/toon-format/toon-go"
)

// Keep the public shape separate from DB rows, which also hold private config
// snapshots and internal bookkeeping
type axiRoundsDocument struct {
	SchemaVersion int            `json:"schema_version"`
	RunID         string         `json:"run_id"`
	Step          string         `json:"step"`
	Rounds        []axiRoundView `json:"rounds"`
}

type axiRoundView struct {
	Round              int              `json:"round"`
	Trigger            string           `json:"trigger"`
	Summary            *string          `json:"summary"`
	Findings           []map[string]any `json:"findings"`
	SelectedFindingIDs []string         `json:"selected_finding_ids"`
	SelectionSource    *string          `json:"selection_source"`
	ResponseOrigin     string           `json:"response_origin"`
	UserFindings       []map[string]any `json:"user_findings"`
	FixSummary         *string          `json:"fix_summary"`
	DurationMS         int64            `json:"duration_ms"`
	CreatedAt          int64            `json:"created_at"`
}

func newAxiRoundsCmd() *cobra.Command {
	var step, runID string
	var asJSON bool
	cmd := &cobra.Command{
		Use:           "rounds",
		Short:         "Read a step's recorded round findings and decisions",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAxiRounds(cmd, step, runID, asJSON)
		},
	}
	cmd.Flags().StringVar(&step, "step", "", "step name, including repository gate names (required)")
	cmd.Flags().StringVar(&runID, "run", "", "run ID (default: current branch's active or most recent)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the versioned JSON document instead of TOON")
	return cmd
}

func runAxiRounds(cmd *cobra.Command, step, runID string, asJSON bool) error {
	fail := func(code int, message string) error {
		if !asJSON {
			return emitError(cmd, code, message)
		}
		if err := json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
			SchemaVersion int    `json:"schema_version"`
			Error         string `json:"error"`
		}{1, message}); err != nil {
			return err
		}
		return &exitError{code: code}
	}
	step, runID = strings.TrimSpace(step), strings.TrimSpace(runID)
	if step == "" {
		return fail(2, "--step is required")
	}
	if !validReadableStep(types.StepName(step)) {
		return fail(2, fmt.Sprintf("unknown step %q", step))
	}

	// This query never creates directories, migrates the DB, or contacts the
	// daemon, including when inspecting runs from outside a checkout
	env, err := openAxiEnvWithOptions(axiEnvOptions{explicitRunID: runID, readOnlyDB: true})
	if err != nil {
		return fail(1, err.Error())
	}
	defer env.close()
	branch := ""
	if runID == "" {
		branch, err = currentBranchForRunResolve(cmd.Context())
		if err != nil {
			return fail(1, err.Error())
		}
	}
	run, _, err := resolveRun(env, runID, branch)
	if err != nil {
		return fail(1, err.Error())
	}
	if run == nil {
		if runID != "" {
			return fail(1, fmt.Sprintf("run %q not found", runID))
		}
		return fail(1, "no run found for this branch; use --run <id> to inspect a specific run")
	}
	steps, err := env.d.GetStepsByRun(run.ID)
	if err != nil {
		return fail(1, fmt.Sprintf("load steps: %v", err))
	}
	var selected *db.StepResult
	for _, s := range steps {
		if string(s.StepName) == step {
			selected = s
			break
		}
	}
	if selected == nil {
		return fail(1, fmt.Sprintf("step %q not recorded in run %q", step, run.ID))
	}
	// A carried step's rounds stay on the step it was carried from
	rounds, err := env.d.GetRoundsByStep(selected.RoundsStepID())
	if err != nil {
		return fail(1, fmt.Sprintf("load rounds: %v", err))
	}
	doc := axiRoundsDocument{SchemaVersion: 1, RunID: run.ID, Step: step, Rounds: make([]axiRoundView, 0, len(rounds))}
	for _, r := range rounds {
		view, err := axiRoundFromDB(r)
		if err != nil {
			return fail(1, fmt.Sprintf("round %d: %v", r.Round, err))
		}
		doc.Rounds = append(doc.Rounds, view)
	}
	if asJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(doc)
	}
	// Both encodings share one public shape, including nested finding fields
	data, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	var values map[string]any
	if err := decodeRoundJSON(string(data), &values); err != nil {
		return err
	}
	emitDoc(cmd,
		toon.Field{Key: "schema_version", Value: doc.SchemaVersion},
		toon.Field{Key: "run_id", Value: doc.RunID},
		toon.Field{Key: "step", Value: doc.Step},
		toon.Field{Key: "rounds", Value: exactTOONIntegers(values["rounds"])},
	)
	return nil
}

func axiRoundFromDB(r *db.StepRound) (axiRoundView, error) {
	view := axiRoundView{
		Round: r.Round, Trigger: r.Trigger, SelectionSource: r.SelectionSource,
		ResponseOrigin: string(r.ResponseOrigin.OrUnknown()), FixSummary: r.FixSummary,
		DurationMS: r.DurationMS, CreatedAt: r.CreatedAt,
	}
	if r.FindingsJSON != nil {
		payload, err := decodeRoundFindings(*r.FindingsJSON)
		if err != nil {
			return view, fmt.Errorf("parse findings: %w", err)
		}
		view.Summary, view.Findings = payload.Summary, payload.Findings
	}
	if r.SelectedFindingIDs != nil {
		var ids []*string
		if err := decodeRoundJSON(*r.SelectedFindingIDs, &ids); err != nil {
			return view, fmt.Errorf("parse selected finding IDs: %w", err)
		}
		if err := requireNoNullMembers(ids); err != nil {
			return view, fmt.Errorf("parse selected finding IDs: %w", err)
		}
		if ids != nil {
			view.SelectedFindingIDs = make([]string, len(ids))
			for i, id := range ids {
				view.SelectedFindingIDs[i] = *id
			}
		}
	}
	if r.UserFindingsJSON != nil {
		payload, err := decodeRoundFindings(*r.UserFindingsJSON)
		if err != nil {
			return view, fmt.Errorf("parse user findings: %w", err)
		}
		view.UserFindings = payload.Findings
	}
	return view, nil
}

type roundFindingsPayload struct {
	Summary  *string          `json:"summary"`
	Findings []map[string]any `json:"findings"`
}

// Both columns store a findings object, never a bare array
func decodeRoundFindings(raw string) (roundFindingsPayload, error) {
	var payload roundFindingsPayload
	if err := decodeRoundJSON(raw, &payload); err != nil {
		return payload, err
	}
	return payload, requireNoNullMembers(payload.Findings)
}

// A null member decodes to a zero value that would export as real data
func requireNoNullMembers[T any](members []T) error {
	for i, m := range members {
		if reflect.ValueOf(m).IsNil() {
			return fmt.Errorf("member %d is null", i)
		}
	}
	return nil
}

// Preserve numbers in optional finding fields without float64 rounding
func decodeRoundJSON(raw string, target any) error {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err != nil {
			return err
		}
		return fmt.Errorf("unexpected trailing JSON value")
	}
	return nil
}

// toon-go parses json.Number as float64 but renders a big.Int beyond 2^53 as a
// decimal string, so integers survive TOON exactly
func exactTOONIntegers(v any) any {
	switch val := v.(type) {
	case json.Number:
		if i, ok := new(big.Int).SetString(val.String(), 10); ok {
			return i
		}
	case map[string]any:
		for k, child := range val {
			val[k] = exactTOONIntegers(child)
		}
	case []any:
		for i, child := range val {
			val[i] = exactTOONIntegers(child)
		}
	}
	return v
}
