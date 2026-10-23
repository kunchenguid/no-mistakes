// Package quotarouting is the quota-aware agent-ordering selection unit.
//
// no-mistakes can be configured with an ordered agent list, but it picks them
// in a fixed order and only falls back when an agent cannot start, so a run
// can begin on an agent whose subscription quota is nearly or fully spent and
// then stall mid-run. When the operator opts in (global quota_routing), the
// daemon reads ONE snapshot from the locally installed quota-axi tool at run
// setup and reorders the resolved agent list by quota evidence.
//
// The selection itself is the pure Select function: a parsed snapshot plus the
// candidate list in configured order in, the reordered list plus per-candidate
// evidence out. The semantics mirror firstmate's quota-array dispatch (three
// gates, then spendPriority) with the no-mistakes design's differences: an
// unknown quota keeps an agent eligible but ranked below known-good headroom,
// ties keep the configured order, and nothing here ever blocks a run - the
// worst case is the configured order unchanged.
//
// Nothing in this package sends anything anywhere: the snapshot is read from
// the local quota-axi executable, and the recorded outcome lives in the local
// run database only.
package quotarouting

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Snapshot is the parsed quota-axi --json document. quota-axi reports one row
// per provider (schema 5) or per provider+account (schema 6); both are
// accepted so an older quota-axi keeps working unchanged.
type Snapshot struct {
	SchemaVersion int        `json:"schemaVersion"`
	Providers     []Provider `json:"providers"`
}

// Provider is one quota row from the snapshot.
type Provider struct {
	Provider       string   `json:"provider"`
	AccountKey     string   `json:"accountKey"`
	AccountKeys    []string `json:"accountKeys,omitempty"`
	AccountLocator *struct {
		DelegateEligible bool `json:"delegateEligible"`
	} `json:"accountLocator,omitempty"`
	QuotaSemantics QuotaSemantics `json:"quotaSemantics"`
}

// QuotaSemantics carries the effective availability scopes for a provider row.
type QuotaSemantics struct {
	Status                string                  `json:"status"`
	EffectiveAvailability []EffectiveAvailability `json:"effectiveAvailability"`
}

// EffectiveAvailability is one scope's evidence within a provider row.
type EffectiveAvailability struct {
	Scope                     string   `json:"scope"`
	Status                    string   `json:"status"`
	EffectivePercentRemaining *float64 `json:"effectivePercentRemaining"`
	Runway                    *Runway  `json:"runway"`
	Selection                 *struct {
		Status        string   `json:"status"`
		SpendPriority *float64 `json:"spendPriority"`
	} `json:"selection"`
}

// Runway is a scope's projected-exhaustion evidence.
type Runway struct {
	Status string `json:"status"`
	// UsableRunwaySeconds is present on projected_exhaustion rows that carry a
	// measured figure. Its absence fails the runway gate: the projection
	// passes only when the usable runway exceeds the expected run length.
	UsableRunwaySeconds *float64 `json:"usableRunwaySeconds"`
}

// ParseSnapshot parses and minimally validates a quota-axi --json document.
// Only schemas 5 and 6 are accepted: anything else is a refusal the caller
// resolves to the configured order, never a guess.
func ParseSnapshot(data []byte) (*Snapshot, error) {
	var snapshot Snapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return nil, fmt.Errorf("parse quota-axi snapshot: %w", err)
	}
	if snapshot.SchemaVersion != 5 && snapshot.SchemaVersion != 6 {
		return nil, fmt.Errorf("unsupported quota-axi snapshot schemaVersion %d (supported: 5, 6)", snapshot.SchemaVersion)
	}
	if snapshot.Providers == nil {
		return nil, fmt.Errorf("quota-axi snapshot has no providers array")
	}
	seen := map[string]bool{}
	for _, provider := range snapshot.Providers {
		if strings.TrimSpace(provider.Provider) == "" {
			return nil, fmt.Errorf("quota-axi snapshot has a provider row without a provider label")
		}
		if snapshot.SchemaVersion == 6 && strings.TrimSpace(provider.AccountKey) == "" {
			return nil, fmt.Errorf("provider %s has no accountKey in a schema-6 snapshot", provider.Provider)
		}
		keys := provider.AccountKeys
		if provider.AccountKey != "" {
			keys = append([]string{provider.AccountKey}, keys...)
		}
		rowKeys := map[string]bool{}
		for _, account := range keys {
			if rowKeys[account] {
				continue
			}
			rowKeys[account] = true
			key := provider.Provider + "\x00" + account
			if seen[key] {
				return nil, fmt.Errorf("provider %s repeats accountKey %s", provider.Provider, account)
			}
			seen[key] = true
		}
	}
	return &snapshot, nil
}
