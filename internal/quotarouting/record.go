package quotarouting

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Record is the durable, local-only outcome of one run's quota-aware agent
// ordering. It is written on the run row at run setup, amended when a
// mid-run quota switch moves the run to the next agent, and rendered by
// `no-mistakes axi status`. It carries the candidates, their evidence, the
// choice, and the reason the configured order was kept when it was.
type Record struct {
	// Version guards future shape changes.
	Version int `json:"version"`
	// Enabled records whether quota routing is active for this run. A recovery
	// pins on the record, not the live setting, so turning the feature off
	// mid-run cannot silently retarget a run that already chose.
	Enabled bool `json:"enabled"`
	// Configured is the resolved agent list before reordering.
	Configured []string `json:"configured"`
	// Order is the list the run actually uses.
	Order []string `json:"order"`
	// Choice is the head agent the run starts on.
	Choice string `json:"choice"`
	// Reason says why the order is what it is: the ranking summary, or why
	// the configured order was kept (quota-axi missing, slow, unreadable, or
	// a Pi candidate without quota_provider).
	Reason string `json:"reason"`
	// Candidates carries per-candidate evidence in the configured order.
	Candidates []Evidence `json:"candidates,omitempty"`
	// Switches lists the mid-run quota-exhaustion switches in order.
	Switches []Switch `json:"switches,omitempty"`
}

// Switch records one mid-run move from a quota-exhausted agent to the next
// agent in the chosen order, which always continues with a fresh session and
// never moves back.
type Switch struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason"`
	At     int64  `json:"at"`
}

// RecordVersion is the current Record shape.
const RecordVersion = 1

// NewRecord builds the record for one selection.
func NewRecord(enabled bool, configured, order []types.AgentName, selection Selection) *Record {
	record := &Record{
		Version:    RecordVersion,
		Enabled:    enabled,
		Configured: agentNamesText(configured),
		Order:      agentNamesText(order),
		Choice:     string(selection.Choice),
		Reason:     selection.Reason,
		Candidates: selection.Evidence,
	}
	return record
}

// NewFailureRecord builds the record for a run that kept the configured order
// because quota evidence was unavailable (missing, slow, or unreadable
// quota-axi) or because reordering was disabled (Pi without quota_provider).
func NewFailureRecord(enabled bool, configured []types.AgentName, reason string) *Record {
	return &Record{
		Version:    RecordVersion,
		Enabled:    enabled,
		Configured: agentNamesText(configured),
		Order:      agentNamesText(configured),
		Choice:     firstAgentName(configured),
		Reason:     reason,
	}
}

// AppendSwitch returns the record with one mid-run switch appended. It never
// mutates the receiver's slices.
func (r *Record) AppendSwitch(from, to types.AgentName, reason string, at time.Time) *Record {
	next := *r
	next.Switches = append(append([]Switch{}, r.Switches...), Switch{
		From:   string(from),
		To:     string(to),
		Reason: reason,
		At:     at.Unix(),
	})
	return &next
}

// ParseRecord decodes the record stored on a run row. An empty payload means
// the run never recorded one (feature off, or a run from before the feature);
// anything unreadable is an error the caller must not paper over.
func ParseRecord(payload string) (*Record, error) {
	if strings.TrimSpace(payload) == "" {
		return nil, nil
	}
	var record Record
	if err := json.Unmarshal([]byte(payload), &record); err != nil {
		return nil, fmt.Errorf("parse quota routing record: %w", err)
	}
	return &record, nil
}

// OrderedAgents applies the record's stored order to a resolved agent list:
// agents the record names keep the recorded relative order, agents it does
// not know (dropped between runs, no longer runnable) keep their resolved
// order at the end. It never adds or removes candidates.
func (r *Record) OrderedAgents(resolved []types.AgentName) []types.AgentName {
	position := make(map[types.AgentName]int, len(r.Order))
	for i, name := range r.Order {
		position[types.CanonicalAgentName(types.AgentName(name))] = i
	}
	// Agents the record does not name (configured after the run started, or
	// never a candidate because no provider maps them) sort after every
	// named one in their resolved relative order - never beside the head,
	// which a plain zero default would give them.
	unranked := len(r.Order)
	rank := func(name types.AgentName) int {
		if pos, ok := position[types.CanonicalAgentName(name)]; ok {
			return pos
		}
		return unranked
	}
	out := append([]types.AgentName{}, resolved...)
	sort.SliceStable(out, func(a, b int) bool {
		return rank(out[a]) < rank(out[b])
	})
	return out
}

func (r *Record) CurrentAgent() string {
	if len(r.Switches) > 0 {
		return r.Switches[len(r.Switches)-1].To
	}
	return r.Choice
}

func agentNamesText(names []types.AgentName) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, string(name))
	}
	return out
}

func firstAgentName(names []types.AgentName) string {
	if len(names) == 0 {
		return ""
	}
	return string(names[0])
}
