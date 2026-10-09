package steps

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const maxGateFindingsBytes = 1 << 20
const maxGateFindings = 500

type gateFindingWire struct {
	ID          string          `json:"id"`
	Severity    string          `json:"severity"`
	File        string          `json:"file"`
	Line        int             `json:"line"`
	Description string          `json:"description"`
	Action      json.RawMessage `json:"action"`
}

type gateFindingsWire struct {
	Items *[]gateFindingWire `json:"findings"`
}

// readGateFindings distinguishes an untouched file from an explicit report.
// A broken report never falls back to the command's exit-code verdict.
func readGateFindings(path string) ([]Finding, bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, false, fmt.Errorf("read findings file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("findings file must be a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, false, fmt.Errorf("open findings file: %w", err)
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxGateFindingsBytes+1))
	if err != nil {
		return nil, false, fmt.Errorf("read findings file: %w", err)
	}
	if len(raw) > maxGateFindingsBytes {
		return nil, false, fmt.Errorf("findings file exceeds 1 MiB")
	}
	if len(raw) == 0 {
		return nil, false, nil
	}
	var report gateFindingsWire
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&report); err != nil {
		return nil, true, fmt.Errorf("parse findings JSON: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, true, fmt.Errorf("parse findings JSON: trailing data")
	}
	if report.Items == nil {
		return nil, true, fmt.Errorf("missing findings array")
	}
	wireItems := *report.Items
	if len(wireItems) > maxGateFindings {
		return nil, true, fmt.Errorf("findings array exceeds 500 findings")
	}
	items := make([]Finding, 0, len(wireItems))
	ids := make(map[string]bool, len(wireItems))
	for i, wire := range wireItems {
		id := strings.TrimSpace(wire.ID)
		if id == "" || ids[id] {
			return nil, true, fmt.Errorf("finding %d has a missing or duplicate id", i+1)
		}
		if strings.Contains(id, ",") {
			return nil, true, fmt.Errorf("finding %q has an id containing a comma", id)
		}
		if pipeline.IsReservedFindingID(id) {
			return nil, true, fmt.Errorf("finding %q uses a reserved id", id)
		}
		ids[id] = true
		if !isGateFindingSeverity(wire.Severity) {
			return nil, true, fmt.Errorf("finding %q has an invalid severity", id)
		}
		if strings.TrimSpace(wire.Description) == "" {
			return nil, true, fmt.Errorf("finding %q is missing a description", id)
		}
		if wire.Line < 0 {
			return nil, true, fmt.Errorf("finding %q has a negative line", id)
		}
		action := types.ActionAskUser
		if len(wire.Action) > 0 {
			var supplied string
			if err := json.Unmarshal(wire.Action, &supplied); err != nil || !isGateFindingAction(supplied) {
				return nil, true, fmt.Errorf("finding %q has an invalid action", id)
			}
			action = supplied
		}
		items = append(items, Finding{
			ID:          id,
			Severity:    wire.Severity,
			File:        wire.File,
			Line:        wire.Line,
			Description: wire.Description,
			Action:      action,
		})
	}
	return items, true, nil
}

func isGateFindingSeverity(severity string) bool {
	switch severity {
	case types.FindingSeverityError, types.FindingSeverityWarning, types.FindingSeverityInfo:
		return true
	default:
		return false
	}
}

func isGateFindingAction(action string) bool {
	switch action {
	case types.ActionAutoFix, types.ActionAskUser, types.ActionNoOp:
		return true
	default:
		return false
	}
}
