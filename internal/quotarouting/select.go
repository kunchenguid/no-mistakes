package quotarouting

import (
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Candidate is one agent considered for ordering, in the configured order.
type Candidate struct {
	Agent types.AgentName
	// Model is the effective model after native raw overrides, "" when unset.
	// Native claude/codex use its last path segment for scope matching; Pi may
	// use its prefix for credential binding. It never infers a provider.
	Model string
	// Provider is the agent's DECLARED quota_provider from agent_config, ""
	// when none. Select resolves the built-in claude/codex mapping on top of
	// it, so a caller cannot forget the built-ins and nothing is ever inferred
	// from a model name.
	Provider string
}

// Evidence records what the snapshot said about one candidate. It is the
// per-candidate payload recorded on the run and rendered by axi status.
type Evidence struct {
	Agent    types.AgentName `json:"agent"`
	Provider string          `json:"provider,omitempty"`
	// Account is the bound quota-axi account key, when published.
	Account string `json:"account,omitempty"`
	// Scope is the limiting applicable row; empty when nothing was applicable.
	Scope string `json:"scope,omitempty"`
	// Remaining is the limiting row's effectivePercentRemaining, when known.
	Remaining *float64 `json:"remaining,omitempty"`
	// SpendPriority is the candidate's own limiting spendPriority (the minimum
	// across its applicable rows), present only when rankable.
	SpendPriority *float64 `json:"spend_priority,omitempty"`
	// Runway is the limiting row's runway status, when known.
	Runway   string `json:"runway,omitempty"`
	Eligible bool   `json:"eligible"`
	// Rankable marks a candidate whose evidence produced a comparable
	// spendPriority. Unrankable candidates stay eligible and keep the
	// configured order below every ranked one.
	Rankable bool   `json:"rankable"`
	Reason   string `json:"reason"`
}

// Selection is the outcome of one selection over one snapshot.
type Selection struct {
	// Order is the full reordered candidate list: ranked candidates by
	// highest spendPriority first, then unrankable-but-eligible ones, then
	// ineligible ones - each group keeping the configured order within it.
	Order []types.AgentName
	// Choice is the head no-mistakes runs first: the first eligible candidate
	// in Order, falling back to Order's head when nobody measured eligible.
	Choice types.AgentName
	// Evidence is per candidate in the configured (input) order.
	Evidence []Evidence
	// Reason summarizes the ranking in one line for the run record.
	Reason string
}

// EvidenceFor returns the evidence recorded for one agent, or nil.
func (s Selection) EvidenceFor(agent types.AgentName) *Evidence {
	for i := range s.Evidence {
		if types.CanonicalAgentName(s.Evidence[i].Agent) == types.CanonicalAgentName(agent) {
			return &s.Evidence[i]
		}
	}
	return nil
}

// ResolveProvider returns the quota-axi provider id for an agent: the declared
// per-agent quota_provider when set, else the built-in claude and codex
// mapping, else "". Nothing is inferred from a model name: an agent outside
// the built-in mapping without a declaration has no provider, which the
// selection reports as unknown rather than guessing.
func ResolveProvider(agent types.AgentName, declared string) string {
	if declared != "" {
		return declared
	}
	switch agent {
	case types.AgentClaude:
		return "claude"
	case types.AgentCodex:
		return "codex"
	default:
		return ""
	}
}

// Lane returns the quota-axi account key a candidate binds to, mirroring
// firstmate's quota_lane: native codex reads the codex-home account, a Pi
// agent can name a Codex credential or Claude profile with its model prefix;
// other providers use the default row.
func Lane(agent types.AgentName, model, provider string) string {
	provider = ResolveProvider(agent, provider)
	if agent == types.AgentCodex && provider == "codex" {
		return "codex-home"
	}
	if agent != types.AgentPi {
		return ""
	}
	prefix, _, qualified := strings.Cut(model, "/")
	if !qualified {
		return ""
	}
	switch provider {
	case "codex":
		if prefix == "codex-native" {
			return "codex-home"
		}
		return prefix
	case "claude":
		if strings.HasPrefix(prefix, "profile:") || strings.HasPrefix(prefix, "claude-swap:") {
			return prefix
		}
	}
	return ""
}

// Select orders candidates by quota evidence. It never fails and never
// removes a candidate: ineligible agents keep their configured order at the
// back of the list so today's launch-failure fallback still has somewhere to
// go, and a nil snapshot (quota-axi missing, slow, or unreadable is resolved
// by the caller before this point) keeps the configured order throughout.
func Select(snapshot *Snapshot, candidates []Candidate, expectedRunLength time.Duration) Selection {
	selection := Selection{
		Order: make([]types.AgentName, 0, len(candidates)),
	}
	for _, candidate := range candidates {
		selection.Order = append(selection.Order, candidate.Agent)
	}
	if len(candidates) == 0 {
		selection.Reason = "no agents to rank"
		return selection
	}
	if snapshot == nil {
		selection.Reason = "no quota snapshot: configured order"
		selection.Evidence = unknownEvidence(candidates)
		return selection
	}
	resolved := make([]Candidate, len(candidates))
	for i, candidate := range candidates {
		resolved[i] = candidate
		resolved[i].Provider = ResolveProvider(candidate.Agent, candidate.Provider)
	}
	candidates = resolved

	evidence := make([]Evidence, 0, len(candidates))
	for _, candidate := range candidates {
		evidence = append(evidence, evaluate(snapshot, candidate, expectedRunLength))
	}
	selection.Evidence = evidence

	ranked := make([]int, 0, len(candidates))
	unranked := make([]int, 0, len(candidates))
	ineligible := make([]int, 0, len(candidates))
	for i, ev := range evidence {
		switch {
		case !ev.Eligible:
			ineligible = append(ineligible, i)
		case ev.Rankable:
			ranked = append(ranked, i)
		default:
			unranked = append(unranked, i)
		}
	}
	// Stable sorts keep the configured order inside every group, which is
	// exactly the tie-break the design states.
	sort.SliceStable(ranked, func(a, b int) bool {
		return *evidence[ranked[a]].SpendPriority > *evidence[ranked[b]].SpendPriority
	})
	order := make([]types.AgentName, 0, len(candidates))
	for _, i := range append(append(ranked, unranked...), ineligible...) {
		order = append(order, candidates[i].Agent)
	}
	configuredOrder := slices.Equal(selection.Order, order)
	selection.Order = order
	selection.Choice = order[0]

	rankedCount := len(ranked)
	switch {
	case rankedCount == 0 && !configuredOrder:
		selection.Reason = "reordered by quota eligibility; no rankable quota evidence"
	case rankedCount == 0:
		selection.Reason = "no rankable quota evidence: configured order"
	case rankedCount == len(candidates):
		selection.Reason = "ranked by quota evidence (spendPriority)"
	default:
		selection.Reason = "ranked by quota evidence; some candidates unrankable or ineligible"
	}
	return selection
}

func unknownEvidence(candidates []Candidate) []Evidence {
	evidence := make([]Evidence, 0, len(candidates))
	for _, candidate := range candidates {
		evidence = append(evidence, Evidence{
			Agent:    candidate.Agent,
			Provider: candidate.Provider,
			Eligible: true,
			Reason:   "no quota snapshot",
		})
	}
	return evidence
}

// evaluate binds one candidate to its quota-axi row and applies the three
// gates in order: concrete-exhaustion eligibility, measurement, then the
// runway floor. Only candidates clearing every gate are rankable, and their
// spendPriority is the minimum across their applicable scopes (the binding
// row), mirroring firstmate's "three gates, then spendPriority".
func evaluate(snapshot *Snapshot, candidate Candidate, expectedRunLength time.Duration) Evidence {
	evidence := Evidence{
		Agent:    candidate.Agent,
		Provider: candidate.Provider,
		Eligible: true,
	}
	if candidate.Provider == "" {
		evidence.Reason = "no quota_provider declared and none built in; declare quota_provider in agent_config"
		return evidence
	}

	row, bound := bindRow(snapshot, candidate)
	if !bound {
		evidence.Reason = "provider " + candidate.Provider + " is not in the quota snapshot"
		return evidence
	}
	evidence.Account = row.AccountKey
	if evidence.Account == "" && len(row.AccountKeys) > 0 {
		evidence.Account = row.AccountKeys[0]
	}
	if row.QuotaSemantics.Status != "known" && row.QuotaSemantics.Status != "partial" {
		evidence.Reason = "provider " + candidate.Provider + " quota is unmeasured (" + row.QuotaSemantics.Status + ")"
		return evidence
	}

	rows := applicableRows(row, candidate)
	if len(rows) == 0 {
		evidence.Reason = "no applicable quota row for provider " + candidate.Provider
		return evidence
	}

	// Gate 1: concrete proof of exhaustion makes the agent ineligible - quota
	// exhausted now, a known 0%, or a projection that cannot clear the
	// expected run length. Uncertainty below never blocks.
	for _, r := range rows {
		if r.Runway != nil && r.Runway.Status == "exhausted_now" {
			evidence.Scope = r.Scope
			evidence.Remaining = r.EffectivePercentRemaining
			evidence.Runway = r.Runway.Status
			evidence.Eligible = false
			evidence.Reason = "runway exhausted_now at " + r.Scope
			return evidence
		}
	}
	for _, r := range rows {
		if r.Status == "known" && r.EffectivePercentRemaining != nil && *r.EffectivePercentRemaining <= 0 {
			evidence.Scope = r.Scope
			evidence.Remaining = r.EffectivePercentRemaining
			evidence.Eligible = false
			evidence.Reason = "0% remaining at " + r.Scope
			return evidence
		}
	}
	for _, r := range rows {
		if r.Runway == nil || r.Runway.Status != "projected_exhaustion" {
			continue
		}
		evidence.Scope = r.Scope
		evidence.Remaining = r.EffectivePercentRemaining
		evidence.Runway = r.Runway.Status
		if r.Runway.UsableRunwaySeconds == nil || time.Duration(*r.Runway.UsableRunwaySeconds)*time.Second <= expectedRunLength {
			evidence.Eligible = false
			evidence.Reason = "projected exhaustion at " + r.Scope + " does not outlast the expected run length"
			return evidence
		}
	}

	// Gate 2: measurement. Any non-known applicable scope is disclosed
	// uncertainty, never a fabricated number: eligible, unranked.
	for _, r := range rows {
		if r.Status != "known" {
			evidence.Scope = r.Scope
			evidence.Reason = "quota row " + r.Scope + " is " + r.Status + ": not rankable"
			return evidence
		}
	}

	for _, r := range rows {
		if r.Selection == nil || r.Selection.SpendPriority == nil {
			evidence.Scope = r.Scope
			evidence.Reason = "spendPriority missing at " + r.Scope + ": not rankable"
			return evidence
		}
	}

	// Gate 3 then rank: spendPriority is the minimum across the applicable
	// scopes, and a candidate with no comparable scalar stays unranked.
	limiting := rows[0]
	for _, r := range rows[1:] {
		if *r.Selection.SpendPriority < *limiting.Selection.SpendPriority {
			limiting = r
		}
	}
	evidence.Scope = limiting.Scope
	evidence.Remaining = limiting.EffectivePercentRemaining
	if limiting.Runway != nil {
		evidence.Runway = limiting.Runway.Status
	}
	spendPriority := *limiting.Selection.SpendPriority
	evidence.SpendPriority = &spendPriority
	evidence.Rankable = true
	evidence.Reason = "ok"
	return evidence
}

// bindRow finds the provider row covering the candidate's credential, or the
// default account when no credential is named. It reports false when no account
// matches.
func bindRow(snapshot *Snapshot, candidate Candidate) (*Provider, bool) {
	var fallback *Provider
	lane := Lane(candidate.Agent, candidate.Model, candidate.Provider)
	nativeClaude := candidate.Agent == types.AgentClaude && candidate.Provider == "claude"
	for i := range snapshot.Providers {
		row := &snapshot.Providers[i]
		if row.Provider != candidate.Provider {
			continue
		}
		if snapshot.SchemaVersion == 5 && row.AccountKey == "" && len(row.AccountKeys) == 0 && (lane == "" || candidate.Agent != types.AgentPi) {
			return row, true
		}
		if lane != "" && (row.AccountKey == lane || slices.Contains(row.AccountKeys, lane)) {
			return row, true
		}
		if nativeClaude {
			if row.AccountLocator != nil && row.AccountLocator.DelegateEligible {
				return row, true
			}
			if snapshot.SchemaVersion == 5 && row.AccountKey == "" && len(row.AccountKeys) == 1 && strings.HasPrefix(row.AccountKeys[0], "profile:") {
				return row, true
			}
		}
		if fallback == nil && lane == "" && (row.AccountKey == "default" || slices.Contains(row.AccountKeys, "default")) {
			fallback = row
		}
	}
	if fallback != nil {
		return fallback, true
	}
	return nil, false
}

// applicableRows returns the effective quota scopes for the configured model.
func applicableRows(row *Provider, candidate Candidate) []EffectiveAvailability {
	model := ""
	if candidate.Agent == types.AgentClaude || candidate.Agent == types.AgentCodex {
		model = candidate.Model
	}
	bare := model
	if i := strings.LastIndex(bare, "/"); i >= 0 {
		bare = bare[i+1:]
	}
	if row.Provider == "claude" {
		bare = strings.TrimSuffix(bare, "[1m]")
	}
	var rows []EffectiveAvailability
	for _, r := range row.QuotaSemantics.EffectiveAvailability {
		scope := r.Scope
		if row.Provider == "codex" && bare != "" && scope == "model:"+bare {
			return []EffectiveAvailability{r}
		}
		if row.Provider == "claude" && scope == "seven_day_opus" {
			scope = "model:opus"
		}
		modelScope, scopedModel := strings.CutPrefix(scope, "model:")
		switch {
		case scope == "all_models" || scope == "all_products":
			rows = append(rows, r)
		case model != "" && scope == "model:"+bare:
			rows = append(rows, r)
		case row.Provider == "claude" && scopedModel && modelScope != "" && strings.HasPrefix(bare, "claude-"+modelScope+"-"):
			rows = append(rows, r)
		case model != "" && scope == "product:"+bare:
			rows = append(rows, r)
		}
	}
	return rows
}
