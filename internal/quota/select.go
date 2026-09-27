package quota

import (
	"fmt"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Candidate is one entry of the operator's ordered candidate list: the harness
// to run, and the quota-axi provider whose evidence says how much of it is left.
type Candidate struct {
	Agent    types.AgentName
	Provider string
}

// String renders the candidate the way the operator wrote it.
func (c Candidate) String() string {
	if c.Provider == "" {
		return string(c.Agent)
	}
	return string(c.Agent) + "@" + c.Provider
}

// maxCandidateToken bounds an operator-supplied name in recorded evidence and
// error text. Both halves come from the operator's own config, so this is a
// sanity bound rather than a trust boundary.
const maxCandidateToken = 64

// providerByAgent maps a harness onto the provider whose allowance it consumes.
//
// The mapping is deliberately explicit and small. Most harnesses ARE their
// vendor's provider. opencode is the one that is not: it is a harness whose own
// subscription is what quota-axi reports as opencode-go. pi, rovodev and every
// acp target have no single provider at all - their quota belongs to whichever
// vendor the operator pointed them at - so they carry no mapping and an
// operator who wants one routed by quota writes it as agent@provider.
var providerByAgent = map[types.AgentName]string{
	types.AgentClaude:      "claude",
	types.AgentCodex:       "codex",
	types.AgentGrok:        "grok",
	types.AgentCopilot:     "copilot",
	types.AgentCursor:      "cursor",
	types.AgentOpenCode:    "opencode-go",
	types.AgentDevin:       "devin",
	types.AgentAntigravity: "agy",
}

// DefaultProvider returns the built-in provider for a harness, or "" when it has
// none.
func DefaultProvider(name types.AgentName) string { return providerByAgent[name] }

// ParseCandidate parses one candidate list entry: "agent" uses the built-in
// provider mapping, "agent@provider" states it explicitly. The explicit form is
// what makes the mapping overridable without a second config key - opencode
// against a Z.AI subscription is opencode@zai, which routes by Z.AI's quota while
// still running the opencode harness.
func ParseCandidate(raw string) (Candidate, error) {
	entry := strings.TrimSpace(raw)
	if entry == "" {
		return Candidate{}, fmt.Errorf("agent candidate must not be empty")
	}
	agentPart, providerPart, explicit := strings.Cut(entry, "@")
	agent := types.AgentName(strings.TrimSpace(agentPart))
	provider := strings.TrimSpace(providerPart)
	if agent == "" {
		return Candidate{}, fmt.Errorf("agent candidate %q must name a harness before @", entry)
	}
	if len(agent) > maxCandidateToken || len(provider) > maxCandidateToken {
		return Candidate{}, fmt.Errorf("agent candidate %q is too long", entry)
	}
	if agent == types.AgentAuto || agent == types.AgentQuotaAuto {
		return Candidate{}, fmt.Errorf("agent candidate %q names a selection mode, not a harness: list the harnesses quota-auto may choose between", entry)
	}
	if explicit {
		if provider == "" {
			return Candidate{}, fmt.Errorf("agent candidate %q must name a provider after @", entry)
		}
		return Candidate{Agent: agent, Provider: provider}, nil
	}
	return Candidate{Agent: agent, Provider: providerByAgent[agent]}, nil
}

// Request states what the next agent turn needs. Class and Budget are the step's
// requirements; Profile is the operator's own per-harness pin, which is the
// selection's other source of evidence (see evidenceSource).
type Request struct {
	Step    types.StepName
	Class   Class
	Budget  time.Duration
	Profile map[string]agentcfg.Profile
	// Current is the candidate the run is already using, or "" at run start.
	// When the current candidate still passes every gate, Select keeps it: an
	// engine switch costs the run its harness-native session and its prompt
	// continuity, so it is reserved for a candidate that stopped being viable
	// rather than spent on a marginal standing difference.
	Current types.AgentName
}

// Gate names the requirement a candidate failed. An empty Gate means the
// candidate is eligible.
type Gate string

const (
	// GateMapping is the structural gate: the candidate has no quota-axi
	// provider to look up at all.
	GateMapping Gate = "provider-mapping"
	// GateEligibility is "provider set up and credential usable".
	GateEligibility Gate = "eligibility"
	// GateClass is "reasoning-class fit for the step".
	GateClass Gate = "reasoning-class"
	// GateRunway is "runway feasibility versus the step's budget".
	GateRunway Gate = "runway"
)

// Assessment is one candidate's outcome, kept for the report and the record even
// when the candidate lost, because "why not this one" is the operator's question
// when routing goes somewhere unexpected.
type Assessment struct {
	Candidate Candidate
	Gate      Gate
	Reason    string
	// Row is the model row the candidate was judged on. It is the zero value for
	// a candidate that never reached the runway gate.
	Row ModelEvidence
	// EvidenceProvider is the provider whose allowance the winning row burns. It
	// differs from Candidate.Provider when the candidate was routed through a
	// pinned model that another provider's quota backs.
	EvidenceProvider string
	// Attestation names where the reasoning-class fit came from: the tool's own
	// model catalog or the operator's declared pin. Empty when unfitted.
	Attestation string
}

// Eligible reports whether the candidate passed every gate.
func (a Assessment) Eligible() bool { return a.Gate == "" }

// Decision is the routing outcome plus the evidence behind it.
type Decision struct {
	Candidate Candidate
	Row       ModelEvidence
	// EvidenceProvider is the provider whose quota backs Row.
	EvidenceProvider string
	Attestation      string
	Assessments      []Assessment
	GeneratedAt      time.Time
	// Kept is true when the run's current candidate still passed every gate, so
	// this boundary changed nothing.
	Kept bool
}

// Summary renders the decision as one operator-facing line.
func (d Decision) Summary() string {
	verb := "selected"
	if d.Kept {
		verb = "kept"
	}
	line := fmt.Sprintf("%s %s", verb, d.Candidate.Agent)
	if d.EvidenceProvider != "" {
		line += " on provider " + d.EvidenceProvider
	}
	if d.Row.ID != "" {
		line += fmt.Sprintf(" (model %s, %s", d.Row.ID, d.Row.Standings())
		if d.Attestation != "" {
			line += ", class attested by " + d.Attestation
		}
		line += ")"
	}
	return line
}

// Select applies the three gates to the operator's ordered candidate list and
// returns the candidate to run. Only the guards that decide routing are here;
// the report itself is produced either way, so a run that fails closed still
// tells the operator what each candidate's evidence was.
func Select(candidates []Candidate, report Report, req Request) (Decision, error) {
	decision := Decision{GeneratedAt: report.GeneratedAt}
	eligible := make([]Assessment, 0, len(candidates))
	for _, candidate := range candidates {
		assessment := assess(candidate, report, req)
		decision.Assessments = append(decision.Assessments, assessment)
		if assessment.Eligible() {
			eligible = append(eligible, assessment)
		}
	}
	if len(eligible) == 0 {
		return decision, &NoCandidateError{
			Step:        req.Step,
			Class:       req.Class,
			Budget:      req.Budget,
			Assessments: decision.Assessments,
		}
	}

	chosen := eligible[0]
	if req.Current != "" {
		for _, assessment := range eligible {
			if assessment.Candidate.Agent == req.Current {
				decision.Kept = true
				chosen = assessment
				break
			}
		}
	}
	if !decision.Kept {
		for _, assessment := range eligible[1:] {
			// Strictly better only: equal standing keeps the earlier candidate, so
			// the configured order breaks ties and the choice stays reproducible.
			if assessment.Row.betterThan(chosen.Row) {
				chosen = assessment
			}
		}
	}
	decision.Candidate = chosen.Candidate
	decision.Row = chosen.Row
	decision.EvidenceProvider = chosen.EvidenceProvider
	decision.Attestation = chosen.Attestation
	return decision, nil
}

// evidenceSource is where one candidate's quota evidence and class attestation
// come from.
type evidenceSource struct {
	provider ProviderEvidence
	// rows are the candidate's usable rows: exactly one for a pinned model, and
	// every catalog row meeting the step's class otherwise.
	rows []ModelEvidence
	// attestation names how the class fit was established, for the record.
	attestation string
}

// findModel locates one catalog model row, preferring the candidate's own mapped
// provider so a model id two providers both list resolves the same way every time.
func findModel(report Report, candidate Candidate, id string) (ProviderEvidence, ModelEvidence, bool) {
	if candidate.Provider != "" {
		if provider, ok := report.Providers[candidate.Provider]; ok {
			if row, ok := provider.row(id); ok {
				return provider, row, true
			}
		}
	}
	for _, name := range report.ProviderNames() {
		provider := report.Providers[name]
		if row, ok := provider.row(id); ok {
			return provider, row, true
		}
	}
	return ProviderEvidence{}, ModelEvidence{}, false
}

// evidenceSource resolves a candidate's evidence, in gate order.
//
// A pinned model is authoritative when the catalog knows it: agent_config names
// exactly what the harness will run, and that model's catalog row carries the
// quota scope the model actually consumes. This is what makes a harness the
// catalog does not describe - opencode, pi, copilot - routable at all: pin its
// model to a catalog id and the harness is routed by the quota that model burns,
// with the model's own class as the reasoning-class attestation.
//
// Without a usable pin, the candidate is judged through its provider mapping: the
// harness consumes that provider's allowance, and is attested at whatever class
// the provider's catalog reaches. A pin the catalog does not know does not stop
// the candidate; it is recorded as an assumption instead, because capability
// attestation and class attestation of a specific unknown model are different
// claims and the operator should see which one was made.
func resolveEvidence(candidate Candidate, report Report, profile agentcfg.Profile, required Class) (evidenceSource, Gate, string) {
	pinned := strings.TrimSpace(profile.Model)
	if pinned != "" {
		if provider, row, ok := findModel(report, candidate, pinned); ok {
			if ok, reason := provider.usable(); !ok {
				return evidenceSource{}, GateEligibility, reason
			}
			if !row.Measurable {
				return evidenceSource{}, GateEligibility, fmt.Sprintf("pinned model %s has no measurable quota scope", row.ID)
			}
			// The pinned model is what will run, so its own class - not the
			// provider's best - is what has to serve this step.
			if !row.Intelligence.Meets(required) {
				return evidenceSource{}, GateClass, fmt.Sprintf("pinned model %s is %s, below the %s this step needs", row.ID, row.Intelligence, required)
			}
			return evidenceSource{
				provider:    provider,
				rows:        []ModelEvidence{row},
				attestation: "profile model " + row.ID,
			}, "", ""
		}
	}
	mappingNote := ""
	if candidate.Provider == "" {
		mappingNote = fmt.Sprintf("harness %s has no quota-axi provider mapping", candidate.Agent)
		if pinned != "" {
			mappingNote += fmt.Sprintf(" and pinned model %q is not in quota-axi's model catalog", pinned)
		}
		return evidenceSource{}, GateMapping, mappingNote + fmt.Sprintf("; write it as %s@<provider>, or pin agent_config.%s.model to a catalog model id", candidate.Agent, candidate.Agent)
	}
	provider, ok := report.Providers[candidate.Provider]
	if !ok {
		reason := fmt.Sprintf("provider %s is not in quota-axi's models report", candidate.Provider)
		if pinned != "" {
			reason += fmt.Sprintf(" and pinned model %q is not in its catalog", pinned)
		}
		return evidenceSource{}, GateEligibility, reason
	}
	if ok, reason := provider.usable(); !ok {
		return evidenceSource{}, GateEligibility, reason
	}
	if ok, reason := provider.measurable(); !ok {
		return evidenceSource{}, GateEligibility, reason
	}
	rows := provider.rowsAt(required)
	if len(rows) == 0 {
		return evidenceSource{}, GateClass, classRefusal(provider, pinned, required)
	}
	attestation := "quota-axi catalog"
	if pinned != "" {
		attestation = fmt.Sprintf("quota-axi catalog (pinned model %q is not in it)", pinned)
	}
	return evidenceSource{provider: provider, rows: rows, attestation: attestation}, "", ""
}

// assess runs the gates in order: a candidate is judged on the first requirement
// it cannot meet, because that is the one an operator has to fix.
func assess(candidate Candidate, report Report, req Request) Assessment {
	assessment := Assessment{Candidate: candidate}
	source, gate, reason := resolveEvidence(candidate, report, req.Profile[string(candidate.Agent)], req.Class)
	if gate != "" {
		assessment.Gate = gate
		assessment.Reason = reason
		return assessment
	}

	best := source.rows[0]
	bestFeasible, _ := best.Feasible(req.Budget)
	for _, row := range source.rows[1:] {
		feasible, _ := row.Feasible(req.Budget)
		switch {
		case feasible != bestFeasible:
			if feasible {
				best, bestFeasible = row, true
			}
		case feasible && row.betterThan(best):
			best = row
		}
	}
	if !bestFeasible {
		// Report the refusal on the candidate's strongest row: that is the row an
		// operator would have to see grow for this candidate to become viable.
		strongest := source.rows[0]
		for _, row := range source.rows[1:] {
			if row.betterThan(strongest) {
				strongest = row
			}
		}
		_, reason := strongest.Feasible(req.Budget)
		assessment.Gate = GateRunway
		assessment.Reason = fmt.Sprintf("%s (%s)", reason, strongest.Standings())
		return assessment
	}
	assessment.Row = best
	assessment.EvidenceProvider = source.provider.Provider
	assessment.Attestation = source.attestation
	return assessment
}

// classRefusal explains a class-gate refusal. A provider that reaches no class at
// all has no usable attestation, which is a different failure from one that
// reaches a lower class, and the two have different remedies.
func classRefusal(provider ProviderEvidence, pinned string, required Class) string {
	pinNote := ""
	if pinned != "" {
		pinNote = fmt.Sprintf(" (pinned model %q is not in the catalog)", pinned)
	}
	if best := provider.bestAttested(); best != "" {
		return fmt.Sprintf("provider attests at most %s, below the %s this step needs%s", best, required, pinNote)
	}
	return "provider has no intelligence-catalog attestation" + pinNote
}

// NoCandidateError is the fail-closed outcome: no candidate could be routed to,
// with every candidate's refusal attached. It is the run's error, so it has to
// read as a report rather than as a stack trace.
type NoCandidateError struct {
	Step        types.StepName
	Class       Class
	Budget      time.Duration
	Assessments []Assessment
}

func (e *NoCandidateError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "quota-auto found no eligible agent for step %q (needs class %s, budget %s):", e.Step, e.Class, HumanDuration(e.Budget))
	for _, assessment := range e.Assessments {
		fmt.Fprintf(&b, "\n  - %s: %s (%s gate)", assessment.Candidate, assessment.Reason, assessment.Gate)
	}
	b.WriteString("\nfix one candidate's evidence, or set agent to a pinned harness, then rerun")
	return b.String()
}
