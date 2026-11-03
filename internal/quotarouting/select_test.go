package quotarouting

import (
	"encoding/json"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

const defaultExpectedRunLength = 90 * time.Minute

// fixtureSnapshot builds a quota-axi snapshot document from provider rows.
// Each row is a raw JSON object so tests state exactly the wire shape.
func fixtureSnapshot(t *testing.T, schemaVersion int, providers ...string) *Snapshot {
	t.Helper()
	doc := `{"schemaVersion":` + itoa(schemaVersion) + `,"providers":[` + strings.Join(providers, ",") + `]}`
	snapshot, err := ParseSnapshot([]byte(doc))
	if err != nil {
		t.Fatalf("ParseSnapshot(%s): %v", doc, err)
	}
	return snapshot
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// claudeProvider is a healthy, rankable provider row with one provider-wide
// scope unless the test overrides the availability array.
func claudeProvider(t *testing.T, availability string) string {
	t.Helper()
	return `{"provider":"claude","quotaSemantics":{"status":"known","effectiveAvailability":` + availability + `}}`
}

func knownRow(scope string, pct float64, runway string, spendPriority float64) string {
	return `{"scope":"` + scope + `","status":"known","effectivePercentRemaining":` + pctJSON(pct) +
		`,"runway":` + runway +
		`,"selection":{"status":"known","spendPriority":` + pctJSON(spendPriority) + `}}`
}

func pctJSON(p float64) string {
	if p == math.Trunc(p) {
		b, _ := json.Marshal(int64(p))
		return string(b)
	}
	b, _ := json.Marshal(p)
	return string(b)
}

func throughReset() string  { return `{"status":"through_reset","projectionConfidence":"established"}` }
func exhaustedNow() string  { return `{"status":"exhausted_now"}` }
func unknownRunway() string { return `{"status":"unknown"}` }

func projectedExhaustion(usableRunwaySeconds float64) string {
	return `{"status":"projected_exhaustion","usableRunwaySeconds":` + pctJSON(usableRunwaySeconds) + `}`
}

func TestSelect_RanksKnownCandidatesBySpendPriority(t *testing.T) {
	snapshot := fixtureSnapshot(t, 5,
		claudeProvider(t, `[`+knownRow("all_models", 71, throughReset(), 2.6)+`]`),
		`{"provider":"codex","quotaSemantics":{"status":"known","effectiveAvailability":[`+
			knownRow("all_models", 100, throughReset(), 5.1)+`]}}`,
	)
	candidates := []Candidate{
		{Agent: types.AgentClaude},
		{Agent: types.AgentCodex},
	}

	got := Select(snapshot, candidates, defaultExpectedRunLength)

	if len(got.Order) != 2 || got.Order[0] != types.AgentCodex || got.Order[1] != types.AgentClaude {
		t.Fatalf("Order = %v, want [codex claude] by spendPriority 5.1 > 2.6", got.Order)
	}
	if got.Choice != types.AgentCodex {
		t.Fatalf("Choice = %s, want codex", got.Choice)
	}
}

func TestSelect_TieKeepsConfiguredOrder(t *testing.T) {
	snapshot := fixtureSnapshot(t, 5,
		claudeProvider(t, `[`+knownRow("all_models", 50, throughReset(), 3)+`]`),
		`{"provider":"codex","quotaSemantics":{"status":"known","effectiveAvailability":[`+
			knownRow("all_models", 80, throughReset(), 3)+`]}}`,
	)
	// claude is configured first; an equal spendPriority must not displace it.
	candidates := []Candidate{
		{Agent: types.AgentClaude},
		{Agent: types.AgentCodex},
	}

	got := Select(snapshot, candidates, defaultExpectedRunLength)

	if len(got.Order) != 2 || got.Order[0] != types.AgentClaude || got.Order[1] != types.AgentCodex {
		t.Fatalf("Order = %v, want configured order [claude codex] on a tie", got.Order)
	}
	if got.Choice != types.AgentClaude {
		t.Fatalf("Choice = %s, want claude", got.Choice)
	}
}

func TestSelect_HighestSpendPriorityWinsEvenWithLowerRemainingPercent(t *testing.T) {
	snapshot := fixtureSnapshot(t, 5,
		claudeProvider(t, `[`+knownRow("all_models", 12, throughReset(), 1.2)+`]`),
		`{"provider":"codex","quotaSemantics":{"status":"known","effectiveAvailability":[`+
			knownRow("all_models", 3, projectedExhaustion(4*3600), 4.4)+`]}}`,
	)
	candidates := []Candidate{
		{Agent: types.AgentClaude},
		{Agent: types.AgentCodex},
	}

	got := Select(snapshot, candidates, defaultExpectedRunLength)

	if got.Choice != types.AgentCodex {
		t.Fatalf("Choice = %s, want codex: spendPriority ranks, not remaining percent", got.Choice)
	}
}

func TestSelect_ExhaustedNowIsIneligible(t *testing.T) {
	snapshot := fixtureSnapshot(t, 5,
		claudeProvider(t, `[`+knownRow("all_models", 0, exhaustedNow(), 9)+`]`),
		`{"provider":"codex","quotaSemantics":{"status":"known","effectiveAvailability":[`+
			knownRow("all_models", 40, throughReset(), 1)+`]}}`,
	)
	candidates := []Candidate{
		{Agent: types.AgentClaude},
		{Agent: types.AgentCodex},
	}

	got := Select(snapshot, candidates, defaultExpectedRunLength)

	if got.Choice != types.AgentCodex {
		t.Fatalf("Choice = %s, want codex: claude is exhausted now", got.Choice)
	}
	if got.Order[0] != types.AgentCodex || got.Order[1] != types.AgentClaude {
		t.Fatalf("Order = %v, want codex first and exhausted claude last", got.Order)
	}
	ev := got.EvidenceFor(types.AgentClaude)
	if ev == nil || ev.Eligible {
		t.Fatalf("claude evidence = %+v, want eligible=false", ev)
	}
	if ev != nil && !strings.Contains(ev.Reason, "exhausted_now") {
		t.Fatalf("claude reason = %q, want it to name exhausted_now", ev.Reason)
	}
}

func TestSelect_ZeroPercentRemainingIsIneligible(t *testing.T) {
	snapshot := fixtureSnapshot(t, 5,
		claudeProvider(t, `[`+knownRow("all_models", 0, throughReset(), 9)+`]`),
		`{"provider":"codex","quotaSemantics":{"status":"known","effectiveAvailability":[`+
			knownRow("all_models", 40, throughReset(), 1)+`]}}`,
	)
	candidates := []Candidate{
		{Agent: types.AgentClaude},
		{Agent: types.AgentCodex},
	}

	got := Select(snapshot, candidates, defaultExpectedRunLength)

	if got.Choice != types.AgentCodex {
		t.Fatalf("Choice = %s, want codex: claude shows 0%% remaining", got.Choice)
	}
	ev := got.EvidenceFor(types.AgentClaude)
	if ev == nil || ev.Eligible {
		t.Fatalf("claude evidence = %+v, want eligible=false", ev)
	}
	if ev != nil && !strings.Contains(ev.Reason, "0%") {
		t.Fatalf("claude reason = %q, want it to name 0%% remaining", ev.Reason)
	}
}

func TestSelect_ProjectedExhaustionBelowExpectedRunLengthIsIneligible(t *testing.T) {
	// 30 minutes of usable runway against a 90-minute expectation.
	snapshot := fixtureSnapshot(t, 5,
		claudeProvider(t, `[`+knownRow("all_models", 8, projectedExhaustion(30*60), 9)+`]`),
		`{"provider":"codex","quotaSemantics":{"status":"known","effectiveAvailability":[`+
			knownRow("all_models", 40, throughReset(), 1)+`]}}`,
	)
	candidates := []Candidate{
		{Agent: types.AgentClaude},
		{Agent: types.AgentCodex},
	}

	got := Select(snapshot, candidates, defaultExpectedRunLength)

	if got.Choice != types.AgentCodex {
		t.Fatalf("Choice = %s, want codex: 30m runway is below the 90m expectation", got.Choice)
	}
	ev := got.EvidenceFor(types.AgentClaude)
	if ev == nil || ev.Eligible {
		t.Fatalf("claude evidence = %+v, want eligible=false", ev)
	}
}

func TestSelect_ProjectedExhaustionAboveExpectedRunLengthIsRankable(t *testing.T) {
	// Two hours of usable runway clears the 90-minute expectation.
	snapshot := fixtureSnapshot(t, 5,
		claudeProvider(t, `[`+knownRow("all_models", 8, projectedExhaustion(2*3600), 6)+`]`),
	)
	candidates := []Candidate{{Agent: types.AgentClaude}}

	got := Select(snapshot, candidates, defaultExpectedRunLength)

	if got.Choice != types.AgentClaude {
		t.Fatalf("Choice = %s, want claude: 2h runway exceeds the 90m expectation", got.Choice)
	}
	ev := got.EvidenceFor(types.AgentClaude)
	if ev == nil || !ev.Eligible || !ev.Rankable {
		t.Fatalf("claude evidence = %+v, want eligible and rankable", ev)
	}
}

func TestSelect_ProjectedExhaustionWithoutUsableRunwayFailsTheGate(t *testing.T) {
	snapshot := fixtureSnapshot(t, 5,
		`{"provider":"claude","quotaSemantics":{"status":"known","effectiveAvailability":[`+
			`{"scope":"all_models","status":"known","effectivePercentRemaining":8,"runway":{"status":"projected_exhaustion"},"selection":{"status":"known","spendPriority":6}}`+
			`]}}`,
	)
	candidates := []Candidate{{Agent: types.AgentClaude}}

	got := Select(snapshot, candidates, defaultExpectedRunLength)

	// The gate passes only when the usable runway exceeds the expectation, so
	// a projection without a runway figure never passes it.
	if got.Choice != types.AgentClaude {
		t.Fatalf("Choice = %s, want the configured head as a last resort", got.Choice)
	}
	ev := got.EvidenceFor(types.AgentClaude)
	if ev == nil || ev.Eligible {
		t.Fatalf("claude evidence = %+v, want eligible=false", ev)
	}
}

func TestSelect_ThroughResetPassesTheRunwayGate(t *testing.T) {
	snapshot := fixtureSnapshot(t, 5,
		claudeProvider(t, `[`+knownRow("all_models", 1, throughReset(), 2)+`]`),
	)
	candidates := []Candidate{{Agent: types.AgentClaude}}

	got := Select(snapshot, candidates, defaultExpectedRunLength)

	if got.Choice != types.AgentClaude {
		t.Fatalf("Choice = %s, want claude: through_reset reaches its refill", got.Choice)
	}
	ev := got.EvidenceFor(types.AgentClaude)
	if ev == nil || ev.Runway != "through_reset" {
		t.Fatalf("claude evidence = %+v, want runway through_reset", ev)
	}
}

func TestSelect_UnknownRanksBelowKnown(t *testing.T) {
	snapshot := fixtureSnapshot(t, 5,
		claudeProvider(t, `[`+knownRow("all_models", 71, throughReset(), 2.6)+`]`),
		`{"provider":"grok","quotaSemantics":{"status":"unknown","effectiveAvailability":[]}}`,
	)
	candidates := []Candidate{
		// grok is configured first and declares its provider, but the row is
		// unmeasured: it must rank below known-good claude.
		{Agent: types.AgentGrok, Provider: "grok"},
		{Agent: types.AgentClaude},
	}

	got := Select(snapshot, candidates, defaultExpectedRunLength)

	if got.Order[0] != types.AgentClaude || got.Order[1] != types.AgentGrok {
		t.Fatalf("Order = %v, want known claude first and unknown grok below it", got.Order)
	}
	if got.Choice != types.AgentClaude {
		t.Fatalf("Choice = %s, want claude", got.Choice)
	}
	ev := got.EvidenceFor(types.AgentGrok)
	if ev == nil || !ev.Eligible || ev.Rankable {
		t.Fatalf("grok evidence = %+v, want eligible but not rankable", ev)
	}
	if ev != nil && !strings.Contains(ev.Reason, "unmeasured") {
		t.Fatalf("grok reason = %q, want it to say unmeasured", ev.Reason)
	}
}

func TestSelect_AgentWithoutProviderStaysEligibleButUnranked(t *testing.T) {
	snapshot := fixtureSnapshot(t, 5,
		claudeProvider(t, `[`+knownRow("all_models", 71, throughReset(), 2.6)+`]`),
	)
	candidates := []Candidate{
		{Agent: types.AgentOpenCode}, // no built-in mapping, nothing declared
		{Agent: types.AgentClaude},
	}

	got := Select(snapshot, candidates, defaultExpectedRunLength)

	if got.Order[0] != types.AgentClaude || got.Order[1] != types.AgentOpenCode {
		t.Fatalf("Order = %v, want claude first and provider-less opencode below it", got.Order)
	}
	ev := got.EvidenceFor(types.AgentOpenCode)
	if ev == nil || !ev.Eligible || ev.Rankable {
		t.Fatalf("opencode evidence = %+v, want eligible but not rankable", ev)
	}
	if ev != nil && !strings.Contains(ev.Reason, "quota_provider") {
		t.Fatalf("opencode reason = %q, want it to point at quota_provider", ev.Reason)
	}
}

func TestSelect_ProviderMissingFromSnapshotIsUnknown(t *testing.T) {
	snapshot := fixtureSnapshot(t, 5,
		claudeProvider(t, `[`+knownRow("all_models", 71, throughReset(), 0.5)+`]`),
	)
	candidates := []Candidate{
		{Agent: types.AgentCodex}, // built-in codex mapping, no codex row present
		{Agent: types.AgentClaude},
	}

	got := Select(snapshot, candidates, defaultExpectedRunLength)

	if got.Order[0] != types.AgentClaude {
		t.Fatalf("Order = %v, want claude first", got.Order)
	}
	ev := got.EvidenceFor(types.AgentCodex)
	if ev == nil || !ev.Eligible || ev.Rankable {
		t.Fatalf("codex evidence = %+v, want eligible but not rankable", ev)
	}
	if ev != nil && !strings.Contains(ev.Reason, "not in the quota snapshot") {
		t.Fatalf("codex reason = %q, want it to name the missing snapshot row", ev.Reason)
	}
}

func TestSelect_ModelScopedRowAppliesToThatModelOnly(t *testing.T) {
	// fable's own scope is at 0% while the provider-wide scope is healthy: a
	// candidate configured for model org/fable is out, one without a model is in.
	snapshot := fixtureSnapshot(t, 5,
		claudeProvider(t, `[`+
			knownRow("all_models", 71, throughReset(), 3)+`,`+
			knownRow("model:fable", 0, exhaustedNow(), 1)+`]`),
	)
	candidates := []Candidate{
		{Agent: types.AgentClaude, Model: "org/fable"},
		{Agent: types.AgentClaude},
	}

	got := Select(snapshot, candidates, defaultExpectedRunLength)

	if got.Order[0] != types.AgentClaude {
		t.Fatalf("Order = %v, want the model-less candidate first", got.Order)
	}
	fable := got.Evidence[0]
	if fable.Eligible {
		t.Fatalf("fable evidence = %+v, want eligible=false from its own scope", fable)
	}
	if !strings.Contains(fable.Reason, "model:fable") {
		t.Fatalf("fable reason = %q, want it to name the model scope", fable.Reason)
	}
}

func TestSelect_LimitingScopeIsTheMinimumSpendPriorityAcrossApplicableRows(t *testing.T) {
	snapshot := fixtureSnapshot(t, 5,
		claudeProvider(t, `[`+
			knownRow("all_models", 71, throughReset(), 3)+`,`+
			knownRow("model:fable", 40, throughReset(), 1.5)+`]`),
	)
	candidates := []Candidate{{Agent: types.AgentClaude, Model: "org/fable"}}

	got := Select(snapshot, candidates, defaultExpectedRunLength)

	ev := got.EvidenceFor(types.AgentClaude)
	if ev == nil || ev.Scope != "model:fable" || ev.SpendPriority == nil || *ev.SpendPriority != 1.5 {
		t.Fatalf("claude evidence = %+v, want the limiting scope model:fable at 1.5", ev)
	}
}

func TestSelect_Schema6BindsCodexToTheCodexHomeAccount(t *testing.T) {
	snapshot := fixtureSnapshot(t, 6,
		`{"provider":"codex","accountKey":"codex-home","quotaSemantics":{"status":"known","effectiveAvailability":[`+
			knownRow("all_models", 90, throughReset(), 4)+`]}}`,
		`{"provider":"codex","accountKey":"default","quotaSemantics":{"status":"known","effectiveAvailability":[`+
			knownRow("all_models", 5, exhaustedNow(), 2)+`]}}`,
	)
	candidates := []Candidate{{Agent: types.AgentCodex}}

	got := Select(snapshot, candidates, defaultExpectedRunLength)

	ev := got.EvidenceFor(types.AgentCodex)
	if ev == nil || ev.Account != "codex-home" || !ev.Eligible {
		t.Fatalf("codex evidence = %+v, want the codex-home account and eligible", ev)
	}
}

func TestSelect_Schema6PiModelPrefixSelectsThatAccount(t *testing.T) {
	snapshot := fixtureSnapshot(t, 6,
		`{"provider":"codex","accountKey":"codex-home","quotaSemantics":{"status":"known","effectiveAvailability":[`+
			knownRow("all_models", 90, throughReset(), 4)+`]}}`,
		`{"provider":"codex","accountKey":"default","quotaSemantics":{"status":"known","effectiveAvailability":[`+
			knownRow("all_models", 10, exhaustedNow(), 2)+`]}}`,
	)
	candidates := []Candidate{{Agent: types.AgentPi, Provider: "codex", Model: "codex-native/gpt-5.4"}}

	got := Select(snapshot, candidates, defaultExpectedRunLength)

	ev := got.EvidenceFor(types.AgentPi)
	if ev == nil {
		t.Fatalf("pi evidence missing")
	}
	if ev.Account != "codex-home" {
		t.Fatalf("pi account = %q, want the codex-home row", ev.Account)
	}
	if !ev.Eligible {
		t.Fatalf("pi evidence = %+v, want the healthy codex-home row to stay eligible", ev)
	}
}

func TestSelect_Schema6PiAccountPrefixBindsTheNamedAccount(t *testing.T) {
	snapshot := fixtureSnapshot(t, 6,
		`{"provider":"codex","accountKey":"work","quotaSemantics":{"status":"known","effectiveAvailability":[`+
			knownRow("all_models", 90, throughReset(), 4)+`]}}`,
		`{"provider":"codex","accountKey":"default","quotaSemantics":{"status":"known","effectiveAvailability":[`+
			knownRow("all_models", 12, throughReset(), 1)+`]}}`,
	)
	candidates := []Candidate{{Agent: types.AgentPi, Provider: "codex", Model: "work/gpt-5.4"}}

	got := Select(snapshot, candidates, defaultExpectedRunLength)

	ev := got.EvidenceFor(types.AgentPi)
	if ev == nil || ev.Account != "work" || !ev.Eligible {
		t.Fatalf("pi evidence = %+v, want the work account row", ev)
	}
}

func TestSelect_Schema6MissingNamedAccountsStayUnknown(t *testing.T) {
	snapshot := fixtureSnapshot(t, 6,
		`{"provider":"codex","accountKey":"default","quotaSemantics":{"status":"known","effectiveAvailability":[`+knownRow("all_models", 70, throughReset(), 3)+`]}}`)
	for _, candidate := range []Candidate{{Agent: types.AgentCodex}, {Agent: types.AgentPi, Provider: "codex", Model: "missing/gpt-5.4"}} {
		selection := Select(snapshot, []Candidate{candidate}, defaultExpectedRunLength)
		evidence := selection.EvidenceFor(candidate.Agent)
		if evidence == nil || evidence.Account != "" || !evidence.Eligible || evidence.Rankable {
			t.Fatalf("missing named credential must stay unknown: %+v", evidence)
		}
	}
}

func TestSelect_Schema6WithoutAnyRowForTheProviderIsUnknown(t *testing.T) {
	snapshot := fixtureSnapshot(t, 6,
		`{"provider":"claude","accountKey":"default","quotaSemantics":{"status":"known","effectiveAvailability":[`+
			knownRow("all_models", 70, throughReset(), 3)+`]}}`,
	)
	candidates := []Candidate{{Agent: types.AgentCodex}}

	got := Select(snapshot, candidates, defaultExpectedRunLength)

	ev := got.EvidenceFor(types.AgentCodex)
	if ev == nil || !ev.Eligible || ev.Rankable {
		t.Fatalf("codex evidence = %+v, want eligible but not rankable", ev)
	}
}

func TestSelect_UnmeasuredRowKeepsTheCandidateEligibleAndUnranked(t *testing.T) {
	snapshot := fixtureSnapshot(t, 5,
		claudeProvider(t, `[`+
			knownRow("all_models", 71, throughReset(), 3)+`,`+
			`{"scope":"model:opus","status":"unknown"}`+`]`),
	)
	candidates := []Candidate{{Agent: types.AgentClaude, Model: "org/opus"}}

	got := Select(snapshot, candidates, defaultExpectedRunLength)

	ev := got.EvidenceFor(types.AgentClaude)
	if ev == nil || !ev.Eligible || ev.Rankable {
		t.Fatalf("claude evidence = %+v, want eligible but not rankable", ev)
	}
	if ev != nil && !strings.Contains(ev.Reason, "not rankable") {
		t.Fatalf("claude reason = %q, want it to say not rankable", ev.Reason)
	}
}

func TestSelect_MissingSpendPriorityIsUnranked(t *testing.T) {
	snapshot := fixtureSnapshot(t, 5,
		`{"provider":"claude","quotaSemantics":{"status":"known","effectiveAvailability":[`+
			`{"scope":"all_models","status":"known","effectivePercentRemaining":71,"runway":`+throughReset()+`}`+
			`]}}`,
	)
	candidates := []Candidate{{Agent: types.AgentClaude}}

	got := Select(snapshot, candidates, defaultExpectedRunLength)

	ev := got.EvidenceFor(types.AgentClaude)
	if ev == nil || !ev.Eligible || ev.Rankable {
		t.Fatalf("claude evidence = %+v, want eligible but not rankable", ev)
	}
}

func TestSelect_AllIneligibleKeepsTheConfiguredOrder(t *testing.T) {
	snapshot := fixtureSnapshot(t, 5,
		claudeProvider(t, `[`+knownRow("all_models", 0, exhaustedNow(), 9)+`]`),
		`{"provider":"codex","quotaSemantics":{"status":"known","effectiveAvailability":[`+
			knownRow("all_models", 0, exhaustedNow(), 5)+`]}}`,
	)
	candidates := []Candidate{
		{Agent: types.AgentClaude},
		{Agent: types.AgentCodex},
	}

	got := Select(snapshot, candidates, defaultExpectedRunLength)

	if len(got.Order) != 2 || got.Order[0] != types.AgentClaude || got.Order[1] != types.AgentCodex {
		t.Fatalf("Order = %v, want the configured order when nobody is eligible", got.Order)
	}
	if got.Choice != types.AgentClaude {
		t.Fatalf("Choice = %s, want the configured head as last resort", got.Choice)
	}
}

func TestSelect_NilSnapshotKeepsTheConfiguredOrder(t *testing.T) {
	candidates := []Candidate{
		{Agent: types.AgentClaude},
		{Agent: types.AgentCodex},
	}

	got := Select(nil, candidates, defaultExpectedRunLength)

	if len(got.Order) != 2 || got.Order[0] != types.AgentClaude {
		t.Fatalf("Order = %v, want the configured order without a snapshot", got.Order)
	}
	ev := got.EvidenceFor(types.AgentClaude)
	if ev == nil || !ev.Eligible || ev.Rankable {
		t.Fatalf("claude evidence = %+v, want eligible but unranked without evidence", ev)
	}
}

func TestParseSnapshot_RejectsGarbage(t *testing.T) {
	for name, doc := range map[string]string{
		"not json":        `nonsense`,
		"not an object":   `[1,2,3]`,
		"no providers":    `{"schemaVersion":5}`,
		"unknown schema":  `{"schemaVersion":99,"providers":[]}`,
		"provider label":  `{"schemaVersion":5,"providers":[{"quotaSemantics":{"status":"known","effectiveAvailability":[]}}]}`,
		"missing nothing": `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseSnapshot([]byte(doc)); err == nil {
				t.Fatalf("ParseSnapshot(%s) = nil error, want a parse refusal", doc)
			}
		})
	}
}

func TestParseSnapshot_AcceptsSchema5And6(t *testing.T) {
	for _, schema := range []int{5, 6} {
		if _, err := ParseSnapshot([]byte(`{"schemaVersion":` + itoa(schema) + `,"providers":[]}`)); err != nil {
			t.Fatalf("ParseSnapshot(schema %d) = %v, want accepted", schema, err)
		}
	}
}

func TestResolveProvider_BuiltInMappingOnly(t *testing.T) {
	if got := ResolveProvider(types.AgentClaude, ""); got != "claude" {
		t.Fatalf("claude provider = %q, want claude", got)
	}
	if got := ResolveProvider(types.AgentCodex, ""); got != "codex" {
		t.Fatalf("codex provider = %q, want codex", got)
	}
	if got := ResolveProvider(types.AgentOpenCode, ""); got != "" {
		t.Fatalf("opencode provider = %q, want empty: nothing may be inferred from the model name", got)
	}
	if got := ResolveProvider(types.AgentOpenCode, "openrouter"); got != "openrouter" {
		t.Fatalf("declared provider = %q, want openrouter", got)
	}
	// A declared provider always wins over the built-in mapping.
	if got := ResolveProvider(types.AgentClaude, "openrouter"); got != "openrouter" {
		t.Fatalf("declared provider = %q, want openrouter to win", got)
	}
}

func TestLane_CodexReadsTheCodexHomeAccount(t *testing.T) {
	if got := Lane(types.AgentCodex, "", "codex"); got != "codex-home" {
		t.Fatalf("codex lane = %q, want codex-home", got)
	}
}

func TestLane_PiCodexReadsTheModelPrefixAccount(t *testing.T) {
	if got := Lane(types.AgentPi, "openai-codex-work/gpt-5.4", "codex"); got != "openai-codex-work" {
		t.Fatalf("pi lane = %q, want openai-codex-work", got)
	}
	if got := Lane(types.AgentPi, "codex-native/gpt-5.4", "codex"); got != "codex-home" {
		t.Fatalf("pi lane = %q, want codex-home for the codex-native adapter", got)
	}
	if got := Lane(types.AgentPi, "gpt-5.4", "codex"); got != "" {
		t.Fatalf("pi lane = %q, want empty without a model prefix", got)
	}
}

func TestLane_OtherAgentsUseTheDefaultRow(t *testing.T) {
	if got := Lane(types.AgentClaude, "org/fable", "claude"); got != "" {
		t.Fatalf("claude lane = %q, want empty (the default row)", got)
	}
}

func TestSelectClaudeOpusIncludesLegacyAndFamilyScopes(t *testing.T) {
	for _, scope := range []string{"seven_day_opus", "model:opus"} {
		for _, tc := range []struct {
			model    string
			eligible bool
			choice   types.AgentName
		}{
			{"opus", false, types.AgentCodex}, {"opus[1m]", false, types.AgentCodex},
			{"claude-opus-4-8", false, types.AgentCodex}, {"claude-opus-4-8[1m]", false, types.AgentCodex},
			{"anthropic/claude-opus-4-8[1m]", false, types.AgentCodex},
			{"sonnet", true, types.AgentClaude}, {"sonnet[1m]", true, types.AgentClaude},
			{"claude-sonnet-4-8", true, types.AgentClaude}, {"", true, types.AgentClaude},
		} {
			t.Run(scope+"/"+tc.model, func(t *testing.T) {
				snapshot := fixtureSnapshot(t, 5, claudeProvider(t, `[`+knownRow("all_models", 80, throughReset(), 3)+`,`+knownRow(scope, 0, exhaustedNow(), 0)+`]`), `{"provider":"codex","quotaSemantics":{"status":"known","effectiveAvailability":[`+knownRow("all_models", 80, throughReset(), 2)+`]}}`)
				selection := Select(snapshot, []Candidate{{Agent: types.AgentClaude, Model: tc.model}, {Agent: types.AgentCodex}}, defaultExpectedRunLength)
				evidence := selection.EvidenceFor(types.AgentClaude)
				if selection.Choice != tc.choice || evidence.Eligible != tc.eligible {
					t.Fatalf("selection=%+v evidence=%+v want choice=%s eligible=%v", selection, evidence, tc.choice, tc.eligible)
				}
				if !tc.eligible && evidence.Scope != scope {
					t.Fatalf("source scope=%s want=%s", evidence.Scope, scope)
				}
			})
		}
	}
}

func TestSelectBindsMergedCredentialMembership(t *testing.T) {
	for _, schema := range []int{5, 6} {
		own := ""
		if schema == 6 {
			own = `"accountKey":"work",`
		}
		for _, candidate := range []Candidate{
			{Agent: types.AgentCodex},
			{Agent: types.AgentPi, Provider: "codex", Model: "codex-native/gpt-5.4"},
			{Agent: types.AgentPi, Provider: "codex", Model: "openai-codex-work/gpt-5.4"},
			{Agent: types.AgentOpenCode, Provider: "codex"},
		} {
			snapshot := fixtureSnapshot(t, schema,
				`{"provider":"codex",`+own+`"accountKeys":["work","codex-home","openai-codex-work","default"],"quotaSemantics":{"status":"known","effectiveAvailability":[`+knownRow("all_models", 0, exhaustedNow(), 0)+`]}}`,
				`{"provider":"claude","accountKey":"default","quotaSemantics":{"status":"known","effectiveAvailability":[`+knownRow("all_models", 80, throughReset(), 2)+`]}}`)
			selection := Select(snapshot, []Candidate{candidate, {Agent: types.AgentClaude}}, defaultExpectedRunLength)
			evidence := selection.EvidenceFor(candidate.Agent)
			if selection.Choice != types.AgentClaude || evidence.Eligible || evidence.Account != "work" {
				t.Fatalf("schema=%d candidate=%+v selection=%+v evidence=%+v, want exhausted merged work account", schema, candidate, selection, evidence)
			}
		}
	}
}

func TestSelectClaudeUsesTheSelectedProfileRatherThanADefaultSibling(t *testing.T) {
	selected := `{"provider":"claude","accountKey":"profile:selected","accountKeys":["profile:selected"],"accountLocator":{"kind":"config-dir","path":"/profiles/selected","delegateEligible":true},"quotaSemantics":{"status":"known","effectiveAvailability":[` + knownRow("all_models", 0, exhaustedNow(), 0) + `]}}`
	sibling := `{"provider":"claude","accountKey":"profile:sibling","accountKeys":["profile:sibling"],"accountLocator":{"kind":"config-dir","path":"/profiles/sibling","delegateEligible":false},"quotaSemantics":{"status":"known","effectiveAvailability":[` + knownRow("all_models", 80, throughReset(), 3) + `]}}`
	codex := `{"provider":"codex","accountKey":"codex-home","quotaSemantics":{"status":"known","effectiveAvailability":[` + knownRow("all_models", 80, throughReset(), 2) + `]}}`
	for _, rows := range [][]string{{selected, sibling, codex}, {sibling, selected, codex}} {
		snapshot := fixtureSnapshot(t, 6, rows...)
		selection := Select(snapshot, []Candidate{{Agent: types.AgentClaude}, {Agent: types.AgentCodex}}, defaultExpectedRunLength)
		evidence := selection.EvidenceFor(types.AgentClaude)
		if selection.Choice != types.AgentCodex || evidence.Eligible || evidence.Account != "profile:selected" {
			t.Fatalf("selection=%+v evidence=%+v, want exhausted selected profile", selection, evidence)
		}
		selection = Select(snapshot, []Candidate{{Agent: types.AgentPi, Provider: "claude", Model: "profile:sibling/opus"}, {Agent: types.AgentCodex}}, defaultExpectedRunLength)
		evidence = selection.EvidenceFor(types.AgentPi)
		if selection.Choice != types.AgentPi || !evidence.Rankable || evidence.Account != "profile:sibling" {
			t.Fatalf("explicit Pi identity must bind its named profile: %+v", selection)
		}
	}
}

func TestParseSnapshotRejectsOverlappingCredentialMembership(t *testing.T) {
	for _, schema := range []int{5, 6} {
		data := `{"schemaVersion":` + itoa(schema) + `,"providers":[{"provider":"codex","accountKey":"work","accountKeys":["work","codex-home"]},{"provider":"codex","accountKey":"codex-home","accountKeys":["codex-home"]}]}`
		if _, err := ParseSnapshot([]byte(data)); err == nil {
			t.Fatalf("schema=%d: overlapping accounts must not select whichever row happens to appear first", schema)
		}
	}
}

func TestSelectMissingPriorityInAnyScopeCannotBeatMeasuredNegativePriority(t *testing.T) {
	for _, missing := range []string{`"selection":null`, `"selection":{}`, `"selection":{"status":"unknown"}`} {
		for _, reverse := range []bool{false, true} {
			incomplete := `{"scope":"model:opus","status":"known","effectivePercentRemaining":80,"runway":{"status":"through_reset"},` + missing + `}`
			rows := []string{knownRow("all_models", 80, throughReset(), -1), incomplete}
			if reverse {
				rows[0], rows[1] = rows[1], rows[0]
			}
			snapshot := fixtureSnapshot(t, 5, claudeProvider(t, `[`+strings.Join(rows, ",")+`]`), `{"provider":"codex","quotaSemantics":{"status":"known","effectiveAvailability":[`+knownRow("all_models", 80, throughReset(), -2)+`]}}`)
			selection := Select(snapshot, []Candidate{{Agent: types.AgentClaude, Model: "opus"}, {Agent: types.AgentCodex}}, defaultExpectedRunLength)
			evidence := selection.EvidenceFor(types.AgentClaude)
			if selection.Choice != types.AgentCodex || !evidence.Eligible || evidence.Rankable || evidence.SpendPriority != nil {
				t.Fatalf("incomplete scope must be unranked: selection=%+v evidence=%+v", selection, evidence)
			}
		}
	}
}

func TestSelectClaudeFamilyScopesApplyToVersionedModels(t *testing.T) {
	for _, family := range []string{"opus", "sonnet", "haiku"} {
		snapshot := fixtureSnapshot(t, 5, claudeProvider(t, `[`+knownRow("all_models", 80, throughReset(), 3)+`,`+knownRow("model:"+family, 0, exhaustedNow(), 0)+`]`))
		selection := Select(snapshot, []Candidate{{Agent: types.AgentClaude, Model: "anthropic/claude-" + family + "-4-8"}}, defaultExpectedRunLength)
		evidence := selection.EvidenceFor(types.AgentClaude)
		if evidence.Eligible || evidence.Scope != "model:"+family {
			t.Fatalf("family=%s evidence=%+v", family, evidence)
		}
	}
}

func TestSelectReasonDistinguishesEligibilityReorderingFromConfiguredOrder(t *testing.T) {
	snapshot := fixtureSnapshot(t, 5,
		`{"provider":"codex","quotaSemantics":{"status":"known","effectiveAvailability":[{"scope":"all_models","status":"known","effectivePercentRemaining":0}]}}`,
		`{"provider":"claude","quotaSemantics":{"status":"known","effectiveAvailability":[{"scope":"all_models","status":"known","effectivePercentRemaining":0}]}}`)
	for _, tc := range []struct {
		name             string
		configured, want []types.AgentName
		reordered        bool
	}{
		{"unknown replaces exhausted head", []types.AgentName{types.AgentCodex, types.AgentOpenCode}, []types.AgentName{types.AgentOpenCode, types.AgentCodex}, true},
		{"unknown already first", []types.AgentName{types.AgentOpenCode, types.AgentCodex}, []types.AgentName{types.AgentOpenCode, types.AgentCodex}, false},
		{"head unchanged but later candidate moves", []types.AgentName{types.AgentOpenCode, types.AgentCodex, types.AgentGrok}, []types.AgentName{types.AgentOpenCode, types.AgentGrok, types.AgentCodex}, true},
		{"unknown candidates already before exhausted", []types.AgentName{types.AgentOpenCode, types.AgentGrok, types.AgentCodex}, []types.AgentName{types.AgentOpenCode, types.AgentGrok, types.AgentCodex}, false},
		{"all unknown", []types.AgentName{types.AgentOpenCode, types.AgentGrok}, []types.AgentName{types.AgentOpenCode, types.AgentGrok}, false},
		{"all exhausted", []types.AgentName{types.AgentCodex, types.AgentClaude}, []types.AgentName{types.AgentCodex, types.AgentClaude}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidates := make([]Candidate, 0, len(tc.configured))
			for _, name := range tc.configured {
				candidates = append(candidates, Candidate{Agent: name})
			}
			result := Select(snapshot, candidates, defaultExpectedRunLength)
			if !slices.Equal(result.Order, tc.want) || result.Choice != tc.want[0] {
				t.Fatalf("order=%v choice=%s want=%v", result.Order, result.Choice, tc.want)
			}
			for _, evidence := range result.Evidence {
				if evidence.Rankable {
					t.Fatalf("fixture unexpectedly rankable: %+v", evidence)
				}
			}
			if strings.Contains(result.Reason, "configured order") == tc.reordered {
				t.Fatalf("reordered=%v reason=%q", tc.reordered, result.Reason)
			}
			if tc.reordered && !strings.Contains(result.Reason, "eligibility") {
				t.Fatalf("reason does not explain eligibility reordering: %q", result.Reason)
			}
		})
	}
}

func TestSelectUsesPublishedCredentialMembershipInBothSchemas(t *testing.T) {
	for _, schema := range []int{5, 6} {
		own := ""
		if schema == 6 {
			own = `"accountKey":"work",`
		}
		snapshot := fixtureSnapshot(t, schema, `{"provider":"codex",`+own+`"accountKeys":["work","openai-codex-work"],"quotaSemantics":{"status":"known","effectiveAvailability":[`+knownRow("all_models", 0, exhaustedNow(), 0)+`]}}`)
		for _, tc := range []struct {
			model    string
			eligible bool
			account  string
		}{
			{"openai-codex-work/gpt-5.4", false, "work"},
			{"missing/gpt-5.4", true, ""},
			{"codex-native/gpt-5.4", true, ""},
		} {
			selection := Select(snapshot, []Candidate{{Agent: types.AgentPi, Provider: "codex", Model: tc.model}}, defaultExpectedRunLength)
			evidence := selection.EvidenceFor(types.AgentPi)
			if evidence.Eligible != tc.eligible || evidence.Account != tc.account || evidence.Rankable {
				t.Fatalf("schema=%d model=%s evidence=%+v", schema, tc.model, evidence)
			}
		}
	}
}

func TestSelectDefaultProviderQuotaDoesNotDependOnOtherAccountExpansion(t *testing.T) {
	for _, tc := range []struct{ provider, model string }{
		{"kimi", "kimi-coding/kimi-for-coding"},
		{"grok", "xai/grok-4"},
		{"openrouter", "openrouter/anthropic/claude-sonnet-4"},
		{"claude", "anthropic/claude-sonnet-4"},
	} {
		for _, schema := range []int{5, 6} {
			for _, pct := range []float64{0, 80} {
				own := ""
				if schema == 6 {
					own = `"accountKey":"default",`
				}
				row := `{"provider":"` + tc.provider + `",` + own + `"accountKeys":["default"],"quotaSemantics":{"status":"known","effectiveAvailability":[` + knownRow("all_models", pct, throughReset(), 3) + `]}}`
				providers := []string{row}
				if schema == 6 {
					providers = append(providers, `{"provider":"codex","accountKey":"codex-home","accountKeys":["codex-home"],"quotaSemantics":{"status":"unknown"}}`, `{"provider":"codex","accountKey":"openai-codex-work","accountKeys":["openai-codex-work"],"quotaSemantics":{"status":"unknown"}}`)
				}
				selection := Select(fixtureSnapshot(t, schema, providers...), []Candidate{{Agent: types.AgentPi, Provider: tc.provider, Model: tc.model}}, defaultExpectedRunLength)
				evidence := selection.EvidenceFor(types.AgentPi)
				wantEligible := pct > 0
				if evidence.Eligible != wantEligible || evidence.Rankable != wantEligible || evidence.Account != "default" {
					t.Fatalf("provider=%s schema=%d percent=%g evidence=%+v", tc.provider, schema, pct, evidence)
				}
			}
		}
	}
}

func TestSelectLegacySchema5DoesNotAssumeANamedPiCredential(t *testing.T) {
	snapshot := fixtureSnapshot(t, 5, `{"provider":"codex","quotaSemantics":{"status":"known","effectiveAvailability":[`+knownRow("all_models", 80, throughReset(), 3)+`]}}`)
	selection := Select(snapshot, []Candidate{{Agent: types.AgentPi, Provider: "codex", Model: "missing/gpt-5.4"}, {Agent: types.AgentCodex}}, defaultExpectedRunLength)
	if selection.Choice != types.AgentCodex || selection.EvidenceFor(types.AgentPi).Rankable {
		t.Fatalf("legacy account identity was guessed: %+v", selection)
	}
}

func TestSelectClaudeContextSuffixPreservesQuotaScopes(t *testing.T) {
	for _, tc := range []struct{ model, scope string }{
		{"opus[1m]", "model:opus"}, {"opus[1m]", "seven_day_opus"},
		{"sonnet[1m]", "model:sonnet"},
		{"claude-sonnet-4-6[1m]", "model:sonnet"},
		{"claude-opus-4-6[1m]", "model:claude-opus-4-6"},
		{"anthropic/claude-sonnet-4-6[1m]", "model:claude-sonnet-4-6"},
	} {
		snapshot := fixtureSnapshot(t, 5, claudeProvider(t, `[`+knownRow("all_models", 80, throughReset(), 3)+`,`+knownRow(tc.scope, 0, exhaustedNow(), 0)+`]`), `{"provider":"codex","quotaSemantics":{"status":"known","effectiveAvailability":[`+knownRow("all_models", 80, throughReset(), 2)+`]}}`)
		selection := Select(snapshot, []Candidate{{Agent: types.AgentClaude, Model: tc.model}, {Agent: types.AgentCodex}}, defaultExpectedRunLength)
		evidence := selection.EvidenceFor(types.AgentClaude)
		if selection.Choice != types.AgentCodex || evidence.Eligible || evidence.Scope != tc.scope {
			t.Fatalf("model=%s scope=%s selection=%+v", tc.model, tc.scope, selection)
		}
	}
}

func TestSelectNativeClaudeBindsSingleSelectedSchema5Profile(t *testing.T) {
	for _, tc := range []struct {
		percent  float64
		eligible bool
		choice   types.AgentName
	}{
		{0, false, types.AgentOpenCode},
		{80, true, types.AgentClaude},
	} {
		snapshot := fixtureSnapshot(t, 5, `{"provider":"claude","accountKeys":["profile:selected"],"quotaSemantics":{"status":"known","effectiveAvailability":[`+knownRow("all_models", tc.percent, throughReset(), 3)+`]}}`)
		selection := Select(snapshot, []Candidate{{Agent: types.AgentClaude}, {Agent: types.AgentOpenCode}}, defaultExpectedRunLength)
		evidence := selection.EvidenceFor(types.AgentClaude)
		if selection.Choice != tc.choice || evidence.Eligible != tc.eligible || evidence.Rankable != tc.eligible || evidence.Account != "profile:selected" {
			t.Fatalf("percent=%g selection=%+v", tc.percent, selection)
		}
		for _, named := range []struct {
			model, account string
			eligible       bool
		}{
			{"profile:selected/opus", "profile:selected", tc.eligible},
			{"profile:missing/opus", "", true},
		} {
			selection = Select(snapshot, []Candidate{{Agent: types.AgentPi, Provider: "claude", Model: named.model}}, defaultExpectedRunLength)
			evidence = selection.EvidenceFor(types.AgentPi)
			if evidence.Account != named.account || evidence.Eligible != named.eligible {
				t.Fatalf("model=%s evidence=%+v", named.model, evidence)
			}
		}
	}
}

func TestSelectNativeClaudeNeverBorrowsUnselectedAccounts(t *testing.T) {
	for _, tc := range []struct {
		schema int
		keys   []string
	}{
		{5, []string{"claude-swap:work"}},
		{6, []string{"claude-swap:work", "claude-swap:personal"}},
		{6, []string{"claude-swap:personal", "claude-swap:work"}},
		{6, []string{"profile:unselected", "profile:other"}},
	} {
		for _, pct := range []float64{0, 80} {
			providers := []string{`{"provider":"codex","accountKey":"codex-home","accountKeys":["codex-home"],"quotaSemantics":{"status":"known","effectiveAvailability":[` + knownRow("all_models", 70, throughReset(), 1) + `]}}`}
			for _, key := range tc.keys {
				own := ""
				if tc.schema == 6 {
					own = `"accountKey":"` + key + `",`
				}
				providers = append(providers, `{"provider":"claude",`+own+`"accountKeys":["`+key+`"],"quotaSemantics":{"status":"known","effectiveAvailability":[`+knownRow("all_models", pct, throughReset(), 3)+`]}}`)
			}
			snapshot := fixtureSnapshot(t, tc.schema, providers...)
			selection := Select(snapshot, []Candidate{{Agent: types.AgentClaude}, {Agent: types.AgentCodex}}, defaultExpectedRunLength)
			evidence := selection.EvidenceFor(types.AgentClaude)
			if selection.Choice != types.AgentCodex || !evidence.Eligible || evidence.Rankable || evidence.Account != "" {
				t.Fatalf("schema=%d keys=%v percent=%g selection=%+v", tc.schema, tc.keys, pct, selection)
			}
			selection = Select(snapshot, []Candidate{{Agent: types.AgentPi, Provider: "claude", Model: tc.keys[0] + "/opus"}}, defaultExpectedRunLength)
			evidence = selection.EvidenceFor(types.AgentPi)
			if evidence.Account != tc.keys[0] || evidence.Eligible != (pct > 0) {
				t.Fatalf("named Pi credential lost its membership binding: %+v", evidence)
			}
		}
	}
}

func TestSelectCodexMatchingScopeOwnsEffectiveVerdict(t *testing.T) {
	const conflict = `{"scope":"model:gpt-5.1-codex","status":"unknown","boundedBy":["primary","model:gpt-5.1-codex"],"boundConflict":{"exhaustedWindowIds":["primary"],"liveWindowIds":["model:gpt-5.1-codex"]},"runway":{"status":"unknown","unmeasurableWindowIds":["primary","model:gpt-5.1-codex"]},"selection":{"status":"unknown","unmeasurableWindowIds":["primary","model:gpt-5.1-codex"]}}`
	const unknownModel = `{"scope":"model:gpt-5.1-codex","status":"unknown","runway":{"status":"unknown"},"selection":{"status":"unknown"}}`
	for _, tc := range []struct {
		name, broad, model string
		eligible, rankable bool
		choice             int
		priority           float64
	}{
		{"conflict overrides inherited exhaustion", knownRow("all_models", 0, exhaustedNow(), -5), conflict, true, false, 0, 0},
		{"conflict overrides inherited zero", knownRow("all_models", 0, unknownRunway(), -5), conflict, true, false, 0, 0},
		{"model uncertainty overrides broad projection", knownRow("all_models", 20, projectedExhaustion(600), -5), unknownModel, true, false, 0, 0},
		{"model priority governs ranking", knownRow("all_models", 80, throughReset(), -5), knownRow("model:gpt-5.1-codex", 40, throughReset(), 2), true, true, 0, 2},
		{"model remains measured despite broad uncertainty", `{"scope":"all_models","status":"unknown"}`, knownRow("model:gpt-5.1-codex", 40, throughReset(), 2), true, true, 0, 2},
		{"model exhaustion still binds", knownRow("all_models", 80, throughReset(), 3), knownRow("model:gpt-5.1-codex", 0, exhaustedNow(), -5), false, false, 1, 0},
		{"model zero still binds", knownRow("all_models", 80, throughReset(), 3), knownRow("model:gpt-5.1-codex", 0, unknownRunway(), -5), false, false, 1, 0},
		{"model projection still binds", knownRow("all_models", 80, throughReset(), 3), knownRow("model:gpt-5.1-codex", 40, projectedExhaustion(600), -5), false, false, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, schema := range []int{5, 6} {
				for _, candidate := range []Candidate{
					{Agent: types.AgentCodex, Model: "gpt-5.1-codex"},
					{Agent: types.AgentCodex, Model: "openai/gpt-5.1-codex"},
				} {
					for _, reverse := range []bool{false, true} {
						rows := []string{tc.broad, tc.model}
						if reverse {
							rows[0], rows[1] = rows[1], rows[0]
						}
						own := ""
						if schema == 6 {
							own = `"accountKey":"codex-home",`
						}
						snapshot := fixtureSnapshot(t, schema, `{"provider":"codex",`+own+`"accountKeys":["codex-home","openai-codex-work"],"quotaSemantics":{"status":"known","effectiveAvailability":[`+strings.Join(rows, ",")+`]}}`)
						candidates := []Candidate{candidate, {Agent: types.AgentOpenCode}}
						selection := Select(snapshot, candidates, defaultExpectedRunLength)
						evidence := selection.EvidenceFor(candidate.Agent)
						if selection.Choice != candidates[tc.choice].Agent || evidence.Eligible != tc.eligible || evidence.Rankable != tc.rankable || evidence.Scope != "model:gpt-5.1-codex" {
							t.Fatalf("schema=%d candidate=%+v reversed=%v selection=%+v", schema, candidate, reverse, selection)
						}
						if tc.rankable {
							if evidence.SpendPriority == nil || *evidence.SpendPriority != tc.priority {
								t.Fatalf("wrong model priority: %+v", evidence)
							}
						} else if evidence.SpendPriority != nil {
							t.Fatalf("unrankable scope fabricated a priority: %+v", evidence)
						}
					}
				}
			}
		})
	}
}

func TestSelectCodexKeepsAccountBoundsWithoutAMatchingModelScope(t *testing.T) {
	for _, model := range []string{"", "gpt-other", "openai/gpt-other"} {
		snapshot := fixtureSnapshot(t, 5, `{"provider":"codex","accountKeys":["codex-home"],"quotaSemantics":{"status":"known","effectiveAvailability":[`+knownRow("all_models", 0, exhaustedNow(), -5)+`,`+knownRow("model:gpt-5.1-codex", 80, throughReset(), 3)+`]}}`)
		selection := Select(snapshot, []Candidate{{Agent: types.AgentCodex, Model: model}, {Agent: types.AgentOpenCode}}, defaultExpectedRunLength)
		evidence := selection.EvidenceFor(types.AgentCodex)
		if selection.Choice != types.AgentOpenCode || evidence.Eligible || evidence.Scope != "all_models" {
			t.Fatalf("model=%s selection=%+v", model, selection)
		}
	}
}

func TestSelectOtherAgentsUseOnlyProviderWideQuotaScopes(t *testing.T) {
	for _, tc := range []struct {
		agent           types.AgentName
		provider, model string
		scopes          []string
	}{
		{types.AgentAntigravity, "agy", "", []string{"gemini", "claude_gpt"}},
		{types.AgentDevin, "devin", "", []string{"included_quota"}},
		{types.AgentName("acp:devin"), "devin", "", []string{"included_quota"}},
		{types.AgentOpenCode, "openrouter", "openrouter/opus", []string{"model:opus"}},
		{types.AgentGrok, "grok", "xai/grok-4", []string{"product:grok-4"}},
		{types.AgentPi, "commandcode", "commandcode/claude-sonnet-4-6", []string{"included_credits", "model:claude-sonnet-4-6"}},
		{types.AgentPi, "claude", "anthropic/opus", []string{"model:opus", "seven_day_opus"}},
		{types.AgentPi, "codex", "codex-native/gpt-5.1-codex", []string{"model:gpt-5.1-codex"}},
		{types.AgentPi, "agy", "google-antigravity/gemini-3-pro", []string{"gemini", "claude_gpt", "model:gemini-3-pro"}},
		{types.AgentPi, "elevenlabs", "", []string{"included_characters"}},
		{types.AgentPi, "higgsfield", "", []string{"included_credits"}},
	} {
		for _, policy := range []struct {
			name, wide                 string
			scopedPercent, widePercent float64
			eligible, rankable         bool
			choice                     int
		}{
			{"vendor exhaustion stays unknown", "", 0, 0, true, false, 0},
			{"vendor headroom stays unknown", "", 80, 0, true, false, 0},
			{"account headroom ignores vendor exhaustion", "all_models", 0, 80, true, true, 0},
			{"account exhaustion ignores vendor headroom", "all_models", 80, 0, false, false, 1},
			{"product headroom ignores vendor exhaustion", "all_products", 0, 80, true, true, 0},
			{"product exhaustion ignores vendor headroom", "all_products", 80, 0, false, false, 1},
		} {
			t.Run(string(tc.agent)+"/"+tc.provider+"/"+policy.name, func(t *testing.T) {
				for _, schema := range []int{5, 6} {
					var rows []string
					scopedRunway := throughReset()
					if policy.scopedPercent == 0 {
						scopedRunway = exhaustedNow()
					}
					for _, scope := range tc.scopes {
						rows = append(rows, knownRow(scope, policy.scopedPercent, scopedRunway, 4))
					}
					if policy.wide != "" {
						wideRunway := throughReset()
						if policy.widePercent == 0 {
							wideRunway = exhaustedNow()
						}
						rows = append(rows, knownRow(policy.wide, policy.widePercent, wideRunway, 2))
					}
					key := "default"
					if tc.provider == "codex" {
						key = "codex-home"
					}
					own := ""
					if schema == 6 {
						own = `"accountKey":"` + key + `",`
					}
					snapshot := fixtureSnapshot(t, schema, `{"provider":"`+tc.provider+`",`+own+`"accountKeys":["`+key+`"],"quotaSemantics":{"status":"known","effectiveAvailability":[`+strings.Join(rows, ",")+`]}}`)
					candidates := []Candidate{{Agent: tc.agent, Provider: tc.provider, Model: tc.model}, {Agent: types.AgentCopilot}}
					selection := Select(snapshot, candidates, defaultExpectedRunLength)
					evidence := selection.EvidenceFor(tc.agent)
					if selection.Choice != candidates[policy.choice].Agent || evidence.Eligible != policy.eligible || evidence.Rankable != policy.rankable || evidence.Scope != policy.wide {
						t.Fatalf("schema=%d selection=%+v", schema, selection)
					}
					if policy.rankable && (evidence.SpendPriority == nil || *evidence.SpendPriority != 2) {
						t.Fatalf("vendor scope changed the provider-wide priority: %+v", evidence)
					}
				}
			})
		}
	}
}

func TestSelectAntigravityWithoutAModelDoesNotCombineQuotaGroups(t *testing.T) {
	for _, schema := range []int{5, 6} {
		for _, geminiExhausted := range []bool{false, true} {
			gemini, claude := knownRow("gemini", 80, throughReset(), 4), knownRow("claude_gpt", 0, exhaustedNow(), -5)
			if geminiExhausted {
				gemini, claude = knownRow("gemini", 0, exhaustedNow(), -5), knownRow("claude_gpt", 80, throughReset(), 4)
			}
			own := ""
			if schema == 6 {
				own = `"accountKey":"default",`
			}
			snapshot := fixtureSnapshot(t, schema, `{"provider":"agy",`+own+`"accountKeys":["default"],"quotaSemantics":{"status":"known","effectiveAvailability":[`+gemini+`,`+claude+`]}}`)
			selection := Select(snapshot, []Candidate{{Agent: types.AgentAntigravity, Provider: "agy"}, {Agent: types.AgentOpenCode}}, defaultExpectedRunLength)
			evidence := selection.EvidenceFor(types.AgentAntigravity)
			if !slices.Equal(selection.Order, []types.AgentName{types.AgentAntigravity, types.AgentOpenCode}) || !evidence.Eligible || evidence.Rankable || evidence.Scope != "" || evidence.SpendPriority != nil {
				t.Fatalf("schema=%d gemini exhausted=%v selection=%+v", schema, geminiExhausted, selection)
			}
		}
	}
}
