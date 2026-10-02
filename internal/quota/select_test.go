package quota

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const testBudget = 30 * time.Minute

func highStepRequest() Request {
	return Request{Step: types.StepReview, Class: ClassHigh, Budget: testBudget}
}

func TestParseCandidate(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		agent    types.AgentName
		provider string
		wantErr  string
	}{
		{name: "mapped harness", raw: "claude", agent: types.AgentClaude, provider: "claude"},
		{name: "opencode has its own subscription", raw: "opencode", agent: types.AgentOpenCode, provider: "opencode-go"},
		{name: "explicit provider", raw: "opencode@zai", agent: types.AgentOpenCode, provider: "zai"},
		{name: "explicit provider is trimmed", raw: "  codex @ openai  ", agent: types.AgentCodex, provider: "openai"},
		{name: "harness with no mapping", raw: "pi", agent: types.AgentPi, provider: ""},
		{name: "acp target has no mapping", raw: "acp:omp", agent: types.AgentName("acp:omp"), provider: ""},
		{name: "empty", raw: "   ", wantErr: "must not be empty"},
		{name: "missing provider", raw: "codex@", wantErr: "must name a provider after @"},
		{name: "missing harness", raw: "@zai", wantErr: "must name a harness before @"},
		{name: "auto is a mode", raw: "auto", wantErr: "names a selection mode"},
		{name: "quota-auto is a mode", raw: string(types.AgentQuotaAuto), wantErr: "names a selection mode"},
		{name: "too long", raw: strings.Repeat("a", 100), wantErr: "too long"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			candidate, err := ParseCandidate(tc.raw)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseCandidate(%q): %v", tc.raw, err)
			}
			if candidate.Agent != tc.agent || candidate.Provider != tc.provider {
				t.Fatalf("candidate = %+v, want %s/%s", candidate, tc.agent, tc.provider)
			}
		})
	}
}

// TestDefaultProvider_DocumentsTheHarnessToProviderTable pins the one piece of
// routing configuration that is not the operator's: which provider's allowance a
// harness consumes when the candidate list does not say.
func TestDefaultProvider_DocumentsTheHarnessToProviderTable(t *testing.T) {
	cases := map[types.AgentName]string{
		types.AgentClaude:          "claude",
		types.AgentCodex:           "codex",
		types.AgentGrok:            "grok",
		types.AgentCopilot:         "copilot",
		types.AgentCursor:          "cursor",
		types.AgentOpenCode:        "opencode-go",
		types.AgentDevin:           "devin",
		types.AgentAntigravity:     "agy",
		types.AgentPi:              "",
		types.AgentRovoDev:         "",
		types.AgentName("acp:omp"): "",
	}
	for harness, want := range cases {
		if got := DefaultProvider(harness); got != want {
			t.Errorf("DefaultProvider(%s) = %q, want %q", harness, got, want)
		}
	}
}

func TestSelect_RoutesToTheBestKnownStanding(t *testing.T) {
	// The live shape this feature exists for: one provider nearly untouched,
	// another with nothing left. Either signal reaching the same answer is what
	// makes the choice auditable.
	report := reportOf(
		freshProvider("claude", measurableRow("claude-sonnet-4-5", ClassHigh, 88, 0.0095, RunwayProjectedExhaustion, 8000)),
		freshProvider("codex", measurableRow("gpt-5.1-codex", ClassHigh, 0, -6.98, RunwayExhaustedNow, 0)),
	)
	decision, err := Select([]Candidate{
		{Agent: types.AgentClaude, Provider: "claude"},
		{Agent: types.AgentCodex, Provider: "codex"},
	}, report, highStepRequest())
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if decision.Candidate.Agent != types.AgentClaude {
		t.Fatalf("selected %s, want claude", decision.Candidate.Agent)
	}
	if decision.EvidenceProvider != "claude" || decision.Row.ID != "claude-sonnet-4-5" {
		t.Fatalf("evidence = %s/%s", decision.EvidenceProvider, decision.Row.ID)
	}
	if !strings.Contains(decision.Summary(), "selected claude on provider claude") {
		t.Fatalf("summary = %q", decision.Summary())
	}
	// The loser is kept in the record with the gate that refused it.
	assessments := decision.Assessments
	if len(assessments) != 2 || assessments[1].Gate != GateRunway || !strings.Contains(assessments[1].Reason, "quota exhausted now") {
		t.Fatalf("codex assessment = %+v", assessments[1])
	}
}

func TestSelect_RanksBySpendPriorityThenRemaining(t *testing.T) {
	report := reportOf(
		freshProvider("zai", measurableRow("glm-4.6", ClassHigh, 33, 1.01, RunwayThroughReset, -1)),
		freshProvider("claude", measurableRow("claude-sonnet-4-5", ClassHigh, 88, -0.01, RunwayThroughReset, -1)),
	)
	decision, err := Select([]Candidate{
		{Agent: types.AgentClaude, Provider: "claude"},
		{Agent: types.AgentOpenCode, Provider: "zai"},
	}, report, highStepRequest())
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	// Spend priority is quota-axi's own selection metric - positive means the
	// allowance would otherwise expire unused - so it outranks a larger remaining
	// percentage on a provider that is on pace to use its own.
	if decision.Candidate.Agent != types.AgentOpenCode {
		t.Fatalf("selected %s, want the higher spend priority", decision.Candidate.Agent)
	}

	// Equal priority falls back to the remaining percentage.
	report = reportOf(
		freshProvider("claude", measurableRow("claude-sonnet-4-5", ClassHigh, 30, 0.5, RunwayThroughReset, -1)),
		freshProvider("zai", measurableRow("glm-4.6", ClassHigh, 70, 0.5, RunwayThroughReset, -1)),
	)
	decision, err = Select([]Candidate{
		{Agent: types.AgentClaude, Provider: "claude"},
		{Agent: types.AgentOpenCode, Provider: "zai"},
	}, report, highStepRequest())
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if decision.Candidate.Provider != "zai" {
		t.Fatalf("selected %s, want the higher remaining percentage", decision.Candidate.Provider)
	}
}

func TestSelect_ConfiguredOrderBreaksTies(t *testing.T) {
	report := reportOf(
		freshProvider("claude", measurableRow("claude-sonnet-4-5", ClassHigh, 50, 0.5, RunwayThroughReset, -1)),
		freshProvider("codex", measurableRow("gpt-5.1-codex", ClassHigh, 50, 0.5, RunwayThroughReset, -1)),
	)
	candidates := []Candidate{
		{Agent: types.AgentCodex, Provider: "codex"},
		{Agent: types.AgentClaude, Provider: "claude"},
	}
	first, err := Select(candidates, report, highStepRequest())
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if first.Candidate.Agent != types.AgentCodex {
		t.Fatalf("selected %s, want the first configured candidate", first.Candidate.Agent)
	}
	// Reversing the list reverses the tie-break, which is what makes the choice
	// reproducible rather than an artifact of map iteration.
	second, err := Select([]Candidate{candidates[1], candidates[0]}, report, highStepRequest())
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if second.Candidate.Agent != types.AgentClaude {
		t.Fatalf("selected %s, want the first configured candidate", second.Candidate.Agent)
	}
}

func TestSelect_KeepsTheCurrentCandidateWhileItIsStillViable(t *testing.T) {
	report := reportOf(
		freshProvider("claude", measurableRow("claude-sonnet-4-5", ClassHigh, 40, 0.1, RunwayThroughReset, -1)),
		freshProvider("zai", measurableRow("glm-4.6", ClassHigh, 90, 2.0, RunwayThroughReset, -1)),
	)
	decision, err := Select([]Candidate{
		{Agent: types.AgentClaude, Provider: "claude"},
		{Agent: types.AgentOpenCode, Provider: "zai"},
	}, report, Request{Step: types.StepLint, Class: ClassMedium, Budget: testBudget, Current: types.AgentClaude})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if !decision.Kept || decision.Candidate.Agent != types.AgentClaude {
		t.Fatalf("decision = %+v, want the current candidate kept", decision)
	}
	if !strings.Contains(decision.Summary(), "kept claude") {
		t.Fatalf("summary = %q", decision.Summary())
	}

	// The same evidence with the current candidate exhausted switches, which is
	// the whole point of re-checking at a boundary.
	report = reportOf(
		freshProvider("claude", measurableRow("claude-sonnet-4-5", ClassHigh, 0, -8, RunwayExhaustedNow, 0)),
		freshProvider("zai", measurableRow("glm-4.6", ClassHigh, 90, 2.0, RunwayThroughReset, -1)),
	)
	decision, err = Select([]Candidate{
		{Agent: types.AgentClaude, Provider: "claude"},
		{Agent: types.AgentOpenCode, Provider: "zai"},
	}, report, Request{Step: types.StepLint, Class: ClassMedium, Budget: testBudget, Current: types.AgentClaude})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if decision.Kept || decision.Candidate.Agent != types.AgentOpenCode {
		t.Fatalf("decision = %+v, want a switch to the viable candidate", decision)
	}
}

func TestSelect_AttestsThroughAPinnedModel(t *testing.T) {
	// opencode's own provider is not catalog-backed, which would make it
	// unroutable. Pinning the model it runs is what supplies both the class and
	// the quota scope - the pinned model's own row - so the harness stays routable.
	report := reportOf(
		freshProvider("claude",
			measurableRow("claude-opus-4-5", ClassHigh, 81, -0.47, RunwayProjectedExhaustion, 9301),
			measurableRow("claude-haiku-4-5", ClassMedium, 88, 0.01, RunwayProjectedExhaustion, 9301),
		),
	)
	decision, err := Select([]Candidate{{Agent: types.AgentOpenCode, Provider: "opencode-go"}}, report, Request{
		Step:    types.StepReview,
		Class:   ClassHigh,
		Budget:  testBudget,
		Profile: map[string]agentcfg.Profile{"opencode": {Model: "claude-opus-4-5"}},
	})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if decision.Candidate.Agent != types.AgentOpenCode || decision.EvidenceProvider != "claude" {
		t.Fatalf("decision = %+v, want opencode routed by claude's quota", decision)
	}
	if decision.Row.ID != "claude-opus-4-5" || decision.Attestation != "profile model claude-opus-4-5" {
		t.Fatalf("row = %+v attestation = %q", decision.Row, decision.Attestation)
	}

	// A pin the catalog does not know does not silently claim its class: the
	// provider's own capability is used, and the record says so.
	decision, err = Select([]Candidate{{Agent: types.AgentOpenCode, Provider: "claude"}}, report, Request{
		Step:    types.StepReview,
		Class:   ClassHigh,
		Budget:  testBudget,
		Profile: map[string]agentcfg.Profile{"opencode": {Model: "sonnet"}},
	})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if !strings.Contains(decision.Attestation, `pinned model "sonnet" is not in it`) {
		t.Fatalf("attestation = %q, want the recorded assumption", decision.Attestation)
	}
}

func TestSelect_RefusesAPinnedModelThatCannotServeTheStep(t *testing.T) {
	report := reportOf(freshProvider("claude",
		measurableRow("claude-opus-4-5", ClassHigh, 81, -0.47, RunwayThroughReset, -1),
		measurableRow("claude-haiku-4-5", ClassMedium, 88, 0.01, RunwayThroughReset, -1),
	))
	candidate := []Candidate{{Agent: types.AgentClaude, Provider: "claude"}}

	// A pin the operator chose is judged on that model's own class, not on what
	// the provider could otherwise serve: the pinned model is what will run.
	_, err := Select(candidate, report, Request{
		Step:    types.StepReview,
		Class:   ClassHigh,
		Budget:  testBudget,
		Profile: map[string]agentcfg.Profile{"claude": {Model: "claude-haiku-4-5"}},
	})
	if err == nil || !strings.Contains(err.Error(), "pinned model claude-haiku-4-5 is medium, below the high this step needs") {
		t.Fatalf("error = %v, want the pinned-model class refusal", err)
	}

	// The same pin is fine for a step that asks for a medium class.
	if _, err := Select(candidate, report, Request{
		Step:    types.StepLint,
		Class:   ClassMedium,
		Budget:  testBudget,
		Profile: map[string]agentcfg.Profile{"claude": {Model: "claude-haiku-4-5"}},
	}); err != nil {
		t.Fatalf("a medium step must accept a medium pin: %v", err)
	}
}

func TestSelect_RefusesAPinnedModelWithNoMeasurableScope(t *testing.T) {
	provider := freshProvider("claude")
	provider.Models = []ModelEvidence{{ID: "claude-opus-4-5", Label: "Claude Opus 4.5", Intelligence: ClassHigh}}
	_, err := Select([]Candidate{{Agent: types.AgentClaude, Provider: "claude"}}, reportOf(provider), Request{
		Step:    types.StepReview,
		Class:   ClassHigh,
		Budget:  testBudget,
		Profile: map[string]agentcfg.Profile{"claude": {Model: "claude-opus-4-5"}},
	})
	if err == nil || !strings.Contains(err.Error(), "pinned model claude-opus-4-5 has no measurable quota scope") {
		t.Fatalf("error = %v, want the unmeasurable-pin refusal", err)
	}
}

func TestSelect_RefusesUnmarketableEvidencePerCandidate(t *testing.T) {
	report := reportOf(
		freshProvider("claude", measurableRow("claude-sonnet-4-5", ClassMedium, 88, 0.01, RunwayThroughReset, -1)),
		ProviderEvidence{Provider: "grok", State: "auth_required", Models: []ModelEvidence{{ID: "grok-4", Intelligence: ClassHigh}}},
		ProviderEvidence{Provider: "mimo", State: providerStateFresh, Models: []ModelEvidence{{ID: "mimo-v2.5-pro", Intelligence: ClassHigh}}},
	)
	_, err := Select([]Candidate{
		{Agent: types.AgentClaude, Provider: "claude"},
		{Agent: types.AgentCodex, Provider: "codex"},
		{Agent: types.AgentGrok, Provider: "grok"},
		{Agent: types.AgentPi, Provider: "mimo"},
		{Agent: types.AgentOpenCode, Provider: "opencode-go"},
		{Agent: types.AgentName("acp:omp")},
	}, report, highStepRequest())
	if err == nil {
		t.Fatal("expected the selection to fail closed")
	}
	var noCandidate *NoCandidateError
	if !errors.As(err, &noCandidate) {
		t.Fatalf("error = %T, want *NoCandidateError", err)
	}
	message := err.Error()
	for _, want := range []string{
		`no eligible agent for step "review" (needs class high, budget 30m)`,
		"claude@claude: provider attests at most medium, below the high this step needs",
		"codex@codex: provider codex is not in quota-axi's models report",
		"grok@grok: provider state auth_required",
		"pi@mimo: no measurable quota scope",
		"opencode@opencode-go: provider opencode-go is not in quota-axi's models report",
		"acp:omp: harness acp:omp has no quota-axi provider mapping",
		"fix one candidate's evidence",
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("report missing %q:\n%s", want, message)
		}
	}
	if len(noCandidate.Assessments) != 6 {
		t.Fatalf("assessments = %d, want one per candidate", len(noCandidate.Assessments))
	}
}

func TestSelect_ClassGateUsesTheStepRequirement(t *testing.T) {
	report := reportOf(freshProvider("claude", measurableRow("claude-haiku-4-5", ClassMedium, 88, 0.01, RunwayThroughReset, -1)))
	candidate := []Candidate{{Agent: types.AgentClaude, Provider: "claude"}}

	if _, err := Select(candidate, report, Request{Step: types.StepDocument, Class: ClassMedium, Budget: testBudget}); err != nil {
		t.Fatalf("a medium step must accept a medium provider: %v", err)
	}
	_, err := Select(candidate, report, Request{Step: types.StepReview, Class: ClassHigh, Budget: testBudget})
	if err == nil || !strings.Contains(err.Error(), "below the high this step needs") {
		t.Fatalf("error = %v, want the class refusal", err)
	}
}

func TestSelect_RunwayGateUsesTheStepBudget(t *testing.T) {
	report := reportOf(freshProvider("claude", measurableRow("claude-sonnet-4-5", ClassHigh, 40, 0.01, RunwayProjectedExhaustion, 900)))
	candidate := []Candidate{{Agent: types.AgentClaude, Provider: "claude"}}

	// The same evidence is viable for a short step and refused for a long one,
	// which is what makes the budget a gate rather than a constant.
	if _, err := Select(candidate, report, Request{Step: types.StepLint, Class: ClassHigh, Budget: 5 * time.Minute}); err != nil {
		t.Fatalf("a 15m runway must cover a 5m budget: %v", err)
	}
	_, err := Select(candidate, report, Request{Step: types.StepReview, Class: ClassHigh, Budget: 30 * time.Minute})
	if err == nil || !strings.Contains(err.Error(), "runway 15m is shorter than the 30m step budget") {
		t.Fatalf("error = %v, want the runway refusal", err)
	}
}

func TestEvidencePayload_IsBoundedAndComplete(t *testing.T) {
	report := reportOf(
		freshProvider("claude", measurableRow("claude-sonnet-4-5", ClassHigh, 88, 0.0095, RunwayThroughReset, -1)),
		freshProvider("codex", measurableRow("gpt-5.1-codex", ClassHigh, 0, -6.98, RunwayExhaustedNow, 0)),
	)
	decision, err := Select([]Candidate{
		{Agent: types.AgentClaude, Provider: "claude"},
		{Agent: types.AgentCodex, Provider: "codex"},
	}, report, highStepRequest())
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	payload := Record{
		Step:      types.StepReview,
		Reason:    ReasonRunStart,
		Decision:  decision,
		ReadError: strings.Repeat("x", maxEvidenceText*2),
	}.EvidencePayload()

	for _, want := range []string{
		`"step":"review"`,
		`"reason":"run-start"`,
		`"agent":"claude"`,
		`"provider":"claude"`,
		`"model":"claude-sonnet-4-5"`,
		`"verdict":"eligible"`,
		`"candidate":"codex@codex"`,
		`"verdict":"runway"`,
		// A refused candidate carries its standings in the refusal text, which is
		// what makes the record readable without the vendor report.
		`0% remaining, spend priority -6.98, runway exhausted_now 0s`,
		`"read_error"`,
	} {
		if !strings.Contains(payload, want) {
			t.Fatalf("payload missing %q:\n%s", want, payload)
		}
	}
	if len(payload) > 4096 {
		t.Fatalf("payload is unbounded: %d bytes", len(payload))
	}
}
