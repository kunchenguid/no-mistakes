package quota

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// stubAgent stands in for a harness adapter. Only the fields the switching agent
// reads are modelled, so the test asserts routing, not adapter behavior.
type stubAgent struct {
	name        string
	runs        int
	closed      bool
	resume      bool
	neutralizes bool
}

func (s *stubAgent) Name() string { return s.name }

func (s *stubAgent) Run(context.Context, agent.RunOpts) (*agent.Result, error) {
	s.runs++
	return &agent.Result{Text: s.name, SessionID: s.name + "-session"}, nil
}

func (s *stubAgent) Close() error {
	s.closed = true
	return nil
}

func (s *stubAgent) SupportsSessionResume() bool { return s.resume }

func (s *stubAgent) SupportsSessionProvider(provider string) bool {
	return s.resume && provider == s.name
}

func (s *stubAgent) ReportsAgentAttempts() bool { return true }

func (s *stubAgent) NeutralizesGateInstructions() bool { return s.neutralizes }

// stubReader serves recorded evidence, and can be flipped mid-test so a boundary
// sees a provider that ran dry while the run was working.
type stubReader struct {
	report Report
	err    error
	calls  int
}

func (r *stubReader) Read(context.Context) (Report, error) {
	r.calls++
	if r.err != nil {
		return Report{}, r.err
	}
	return r.report, nil
}

// switcherFixture wires a switching agent over two catalogue-backed candidates.
type switcherFixture struct {
	switcher *SwitchingAgent
	reader   *stubReader
	built    map[string]*stubAgent
	order    []string
	records  []Record
	// unverified names harnesses whose stub reports no gate neutralization, so a
	// test can exercise the opt-out's fail-closed switch refusal.
	unverified map[string]bool
}

func twoCandidateReport(claudePercent int, claudeRunway string, claudeSeconds int, codexPercent int, codexRunway string, codexSeconds int) Report {
	return reportOf(
		freshProvider("claude", measurableRow("claude-sonnet-4-5", ClassHigh, claudePercent, 0.5, claudeRunway, claudeSeconds)),
		freshProvider("codex", measurableRow("gpt-5.1-codex", ClassHigh, codexPercent, 0.2, codexRunway, codexSeconds)),
	)
}

func newSwitcherFixture(t *testing.T, report Report, tweak func(*Options)) *switcherFixture {
	t.Helper()
	fixture := &switcherFixture{
		reader:     &stubReader{report: report},
		built:      map[string]*stubAgent{},
		unverified: map[string]bool{},
	}
	options := Options{
		Candidates: []Candidate{
			{Agent: types.AgentClaude, Provider: "claude"},
			{Agent: types.AgentCodex, Provider: "codex"},
		},
		Reader:  fixture.reader,
		Budgets: Budgets{Default: testBudget, Review: testBudget, Test: testBudget},
		Build: func(name types.AgentName) (agent.Agent, error) {
			stub := &stubAgent{name: string(name), resume: true, neutralizes: !fixture.unverified[string(name)]}
			fixture.built[string(name)] = stub
			fixture.order = append(fixture.order, string(name))
			return stub, nil
		},
		Record: func(_ context.Context, record Record) { fixture.records = append(fixture.records, record) },
	}
	if tweak != nil {
		tweak(&options)
	}
	switcher, err := NewSwitchingAgent(options)
	if err != nil {
		t.Fatalf("NewSwitchingAgent: %v", err)
	}
	fixture.switcher = switcher
	return fixture
}

func TestNewSwitchingAgent_ValidatesItsWiring(t *testing.T) {
	if _, err := NewSwitchingAgent(Options{Build: func(types.AgentName) (agent.Agent, error) { return &stubAgent{}, nil }}); err == nil {
		t.Error("a candidate list is required")
	}
	if _, err := NewSwitchingAgent(Options{Candidates: []Candidate{{Agent: types.AgentClaude, Provider: "claude"}}}); err == nil {
		t.Error("a harness builder is required")
	}
}

func TestSwitchingAgent_StartSelectsTheBestStandingCandidate(t *testing.T) {
	// The live shape: claude has headroom, codex is dry. The run must open on
	// claude and record why.
	fixture := newSwitcherFixture(t, twoCandidateReport(88, RunwayProjectedExhaustion, 8000, 0, RunwayExhaustedNow, 0), nil)
	if err := fixture.switcher.Start(context.Background(), types.StepReview); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := fixture.switcher.Name(); got != "claude" {
		t.Fatalf("selected %q, want claude", got)
	}
	if len(fixture.order) != 1 || fixture.order[0] != "claude" {
		t.Fatalf("built %v, want only the selected harness", fixture.order)
	}
	result, err := fixture.switcher.Run(context.Background(), agent.RunOpts{})
	if err != nil || result.Text != "claude" {
		t.Fatalf("run = %v/%v, want the selected harness", result, err)
	}
	if len(fixture.records) != 1 {
		t.Fatalf("records = %d, want the opening selection recorded", len(fixture.records))
	}
	record := fixture.records[0]
	if record.Reason != ReasonRunStart || record.Step != types.StepReview {
		t.Fatalf("record = %+v", record)
	}
	if !strings.Contains(record.EvidencePayload(), `"agent":"claude"`) {
		t.Fatalf("payload = %s", record.EvidencePayload())
	}
}

func TestSwitchingAgent_StartFailsClosedWithoutEvidence(t *testing.T) {
	fixture := newSwitcherFixture(t, twoCandidateReport(0, RunwayExhaustedNow, 0, 0, RunwayExhaustedNow, 0), nil)
	err := fixture.switcher.Start(context.Background(), types.StepReview)
	var noCandidate *NoCandidateError
	if !errors.As(err, &noCandidate) {
		t.Fatalf("error = %v, want *NoCandidateError", err)
	}
	if len(fixture.order) != 0 {
		t.Fatalf("built %v, want no harness at all", fixture.order)
	}
	if _, runErr := fixture.switcher.Run(context.Background(), agent.RunOpts{}); runErr == nil {
		t.Fatal("running without a selection must fail")
	}

	// Evidence that cannot be read at all is the same outcome: the run does not
	// start on a guess.
	fixture = newSwitcherFixture(t, Report{}, nil)
	fixture.reader.err = errors.New(`exec: "quota-axi": executable file not found in $PATH`)
	if err := fixture.switcher.Start(context.Background(), types.StepReview); err == nil || !strings.Contains(err.Error(), "executable file not found") {
		t.Fatalf("error = %v, want the read failure surfaced", err)
	}
}

func TestSwitchingAgent_SwitchesWhenTheCurrentHarnessRunsDry(t *testing.T) {
	fixture := newSwitcherFixture(t, twoCandidateReport(88, RunwayProjectedExhaustion, 8000, 0, RunwayExhaustedNow, 0), nil)
	if err := fixture.switcher.Start(context.Background(), types.StepReview); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// The run works for a while; claude then runs out mid-run, which is exactly
	// the scenario that used to strand every step behind it.
	fixture.reader.report = twoCandidateReport(0, RunwayExhaustedNow, 0, 70, RunwayThroughReset, -1)
	if err := fixture.switcher.Reassess(context.Background(), types.StepPush); err != nil {
		t.Fatalf("Reassess: %v", err)
	}
	if got := fixture.switcher.Name(); got != "codex" {
		t.Fatalf("selected %q, want the switch to codex", got)
	}
	if !fixture.built["claude"].closed {
		t.Error("the superseded harness must be closed")
	}
	record := fixture.records[len(fixture.records)-1]
	if record.Reason != ReasonStepBoundary || record.Step != types.StepPush || record.Decision.Kept {
		t.Fatalf("record = %+v", record)
	}
	result, err := fixture.switcher.Run(context.Background(), agent.RunOpts{})
	if err != nil || result.Text != "codex" {
		t.Fatalf("run = %v/%v, want the switched harness", result, err)
	}
	if fixture.reader.calls != 2 {
		t.Fatalf("evidence reads = %d, want one per selection point", fixture.reader.calls)
	}
}

func TestSwitchingAgent_KeepsAHarnessThatStillPassesEveryGate(t *testing.T) {
	// claude is viable but codex has the better standing; the boundary keeps
	// claude. Switching costs the run its session and prompt continuity, so it is
	// reserved for a harness that stopped being usable.
	fixture := newSwitcherFixture(t, twoCandidateReport(88, RunwayProjectedExhaustion, 8000, 40, RunwayThroughReset, -1), nil)
	if err := fixture.switcher.Start(context.Background(), types.StepReview); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if fixture.switcher.Name() != "claude" {
		t.Fatalf("start selected %q", fixture.switcher.Name())
	}
	before := len(fixture.records)
	if err := fixture.switcher.Reassess(context.Background(), types.StepLint); err != nil {
		t.Fatalf("Reassess: %v", err)
	}
	if fixture.switcher.Name() != "claude" {
		t.Fatalf("boundary switched to %q, want the harness kept", fixture.switcher.Name())
	}
	if len(fixture.records) != before {
		t.Fatalf("a kept boundary recorded %d rows, want none", len(fixture.records)-before)
	}
	if len(fixture.order) != 1 {
		t.Fatalf("built %v, want no new harness", fixture.order)
	}
}

func TestSwitchingAgent_KeepsTheHarnessWhenEvidenceCannotBeReadMidRun(t *testing.T) {
	fixture := newSwitcherFixture(t, twoCandidateReport(88, RunwayProjectedExhaustion, 8000, 40, RunwayThroughReset, -1), nil)
	if err := fixture.switcher.Start(context.Background(), types.StepReview); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// A boundary is not the run's opening choice: losing the evidence tool must
	// not strand a run that can still work.
	fixture.reader.err = errors.New("quota-axi: connection reset")
	if err := fixture.switcher.Reassess(context.Background(), types.StepPush); err != nil {
		t.Fatalf("boundary must keep the harness in use, got: %v", err)
	}
	if fixture.switcher.Name() != "claude" {
		t.Fatalf("selected %q, want the harness kept", fixture.switcher.Name())
	}
}

func TestSwitchingAgent_FailsTheRunWhenNothingIsRoutableAtABoundary(t *testing.T) {
	fixture := newSwitcherFixture(t, twoCandidateReport(88, RunwayProjectedExhaustion, 8000, 0, RunwayExhaustedNow, 0), nil)
	if err := fixture.switcher.Start(context.Background(), types.StepReview); err != nil {
		t.Fatalf("Start: %v", err)
	}
	fixture.reader.report = twoCandidateReport(0, RunwayExhaustedNow, 0, 0, RunwayExhaustedNow, 0)
	err := fixture.switcher.Reassess(context.Background(), types.StepTest)
	var noCandidate *NoCandidateError
	if !errors.As(err, &noCandidate) {
		t.Fatalf("error = %v, want the per-candidate report", err)
	}
	if !strings.Contains(err.Error(), "claude@claude: quota exhausted now") {
		t.Fatalf("report = %v", err)
	}
}

func TestSwitchingAgent_SkipsTheReassessmentOfItsOwnStartStep(t *testing.T) {
	fixture := newSwitcherFixture(t, twoCandidateReport(88, RunwayProjectedExhaustion, 8000, 0, RunwayExhaustedNow, 0), nil)
	if err := fixture.switcher.Start(context.Background(), types.StepReview); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// The executor reassesses the first step immediately after the opening
	// selection; re-reading evidence for a decision just taken would spend a vendor
	// read to reach the same answer.
	if err := fixture.switcher.Reassess(context.Background(), types.StepReview); err != nil {
		t.Fatalf("Reassess: %v", err)
	}
	if fixture.reader.calls != 1 {
		t.Fatalf("evidence reads = %d, want the start step skipped", fixture.reader.calls)
	}
	// The skip is one-shot: a later restart of the same step is reassessed.
	if err := fixture.switcher.Reassess(context.Background(), types.StepReview); err != nil {
		t.Fatalf("Reassess: %v", err)
	}
	if fixture.reader.calls != 2 {
		t.Fatalf("evidence reads = %d, want the second visit reassessed", fixture.reader.calls)
	}
}

func TestSwitchingAgent_RefusesAnUnverifiedSwitchUnderTheOptOut(t *testing.T) {
	fixture := newSwitcherFixture(t, twoCandidateReport(88, RunwayProjectedExhaustion, 8000, 0, RunwayExhaustedNow, 0), func(options *Options) {
		options.RequireNeutralized = true
	})
	fixture.unverified["codex"] = true
	if err := fixture.switcher.Start(context.Background(), types.StepReview); err != nil {
		t.Fatalf("Start: %v", err)
	}
	fixture.reader.report = twoCandidateReport(0, RunwayExhaustedNow, 0, 70, RunwayThroughReset, -1)
	err := fixture.switcher.Reassess(context.Background(), types.StepPush)
	if err == nil || !strings.Contains(err.Error(), "does not neutralize") || !strings.Contains(err.Error(), "disable_project_settings") {
		t.Fatalf("error = %v, want the opt-out refusal", err)
	}
	if fixture.switcher.Name() != "claude" {
		t.Fatalf("agent = %q, want the refused switch to leave the run on claude", fixture.switcher.Name())
	}
	if !fixture.built["codex"].closed {
		t.Error("the refused harness must be closed")
	}
}

func TestSwitchingAgent_SessionSupportSpansEveryCandidate(t *testing.T) {
	fixture := newSwitcherFixture(t, twoCandidateReport(88, RunwayProjectedExhaustion, 8000, 40, RunwayThroughReset, -1), nil)
	if err := fixture.switcher.Start(context.Background(), types.StepReview); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !fixture.switcher.SupportsSessionResume() {
		t.Error("session resume must mirror the harness in use")
	}
	// A session minted by a candidate that is not currently selected must stay
	// resumable: quota may route back to it, and the daemon's recovery check must
	// not refuse the run in the meantime.
	if !fixture.switcher.SupportsSessionProvider("codex") {
		t.Error("session providers must cover the candidate set")
	}
	if fixture.switcher.SupportsSessionProvider("grok") {
		t.Error("a harness that is not a candidate owns no session")
	}
	if fixture.switcher.SupportsSessionProvider("") {
		t.Error("an empty provider is never supported")
	}
	if !agent.ReportsAgentAttempts(fixture.switcher) {
		t.Error("attempt reporting must mirror the harness in use")
	}
}

func TestSwitchingAgent_CloseClosesTheHarnessInUse(t *testing.T) {
	fixture := newSwitcherFixture(t, twoCandidateReport(88, RunwayProjectedExhaustion, 8000, 0, RunwayExhaustedNow, 0), nil)
	if err := fixture.switcher.Start(context.Background(), types.StepReview); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := fixture.switcher.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !fixture.built["claude"].closed {
		t.Error("Close must close the harness in use")
	}
	if fixture.switcher.Name() != "" {
		t.Errorf("Name = %q after Close, want empty", fixture.switcher.Name())
	}
	if fixture.switcher.NeutralizesGateInstructions() {
		t.Error("an unselected switching agent must fail closed on neutralization")
	}
}

func TestAXIReader_ReadsTheModelsJoinAndSurfacesFailures(t *testing.T) {
	var gotArgs []string
	var gotBinary string
	reader := &AXIReader{Path: "/opt/tools/quota-axi"}
	reader.run = func(_ context.Context, binary string, args ...string) ([]byte, []byte, error) {
		gotBinary = binary
		gotArgs = args
		return []byte(`{"models":[{"provider":"claude","id":"claude-sonnet-4-5","intelligence":"high","state":{"status":"fresh"},"effective":{"scope":"all_models","status":"known","effectivePercentRemaining":88,"runway":{"status":"through_reset"},"selection":{"status":"known","spendPriority":0.01}}}]}`), nil, nil
	}
	report, err := reader.Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if gotBinary != "/opt/tools/quota-axi" || strings.Join(gotArgs, " ") != "models --json" {
		t.Fatalf("ran %q %v, want the models join", gotBinary, gotArgs)
	}
	if len(report.Providers) != 1 || report.Providers["claude"].Models[0].PercentRemaining == nil {
		t.Fatalf("report = %+v", report)
	}

	reader.run = func(context.Context, string, ...string) ([]byte, []byte, error) {
		return nil, []byte("error: no providers configured\nmore detail"), errors.New("exit status 1")
	}
	_, err = reader.Read(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no providers configured") {
		t.Fatalf("error = %v, want the tool's own diagnostic", err)
	}
	if strings.Contains(err.Error(), "more detail") {
		t.Fatalf("error = %v, want a single bounded line of stderr", err)
	}
}
