package types

import (
	"encoding/json"
	"testing"
)

func TestAllStepsOrder(t *testing.T) {
	steps := AllSteps()
	if len(steps) != 9 {
		t.Fatalf("expected 9 steps, got %d", len(steps))
	}

	expected := []StepName{StepIntent, StepRebase, StepReview, StepTest, StepDocument, StepLint, StepPush, StepPR, StepCI}
	for i, s := range steps {
		if s != expected[i] {
			t.Errorf("step[%d] = %q, want %q", i, s, expected[i])
		}
	}
}

func TestStepNameOrder(t *testing.T) {
	tests := []struct {
		step StepName
		want int
	}{
		{StepIntent, 1},
		{StepRebase, 2},
		{StepReview, 3},
		{StepTest, 4},
		{StepDocument, 5},
		{StepLint, 6},
		{StepPush, 7},
		{StepPR, 8},
		{StepCI, 9},
		{StepName("unknown"), 0},
	}

	for _, tt := range tests {
		if got := tt.step.Order(); got != tt.want {
			t.Errorf("%q.Order() = %d, want %d", tt.step, got, tt.want)
		}
	}
}

func TestStepNameUnmarshalJSON_LegacyBabysit(t *testing.T) {
	var step StepName
	if err := json.Unmarshal([]byte(`"babysit"`), &step); err != nil {
		t.Fatalf("unmarshal step name: %v", err)
	}
	if step != StepCI {
		t.Fatalf("step = %q, want %q", step, StepCI)
	}
}

func TestACPAliasFor(t *testing.T) {
	alias, ok := ACPAliasFor(AgentCursor)
	if !ok {
		t.Fatal("cursor should be registered as an ACP alias")
	}
	if alias.Target != "cursor" {
		t.Fatalf("target = %q, want cursor", alias.Target)
	}
	if alias.DefaultCommand != "cursor-agent acp" {
		t.Fatalf("default command = %q, want cursor-agent acp", alias.DefaultCommand)
	}
	if alias.DefaultCommandBinary() != "cursor-agent" {
		t.Fatalf("default command binary = %q, want cursor-agent", alias.DefaultCommandBinary())
	}
	targetAlias, ok := ACPAliasForTarget("cursor")
	if !ok {
		t.Fatal("cursor target should resolve to an ACP alias")
	}
	if targetAlias.Name != AgentCursor {
		t.Fatalf("target alias name = %q, want %q", targetAlias.Name, AgentCursor)
	}

	aliases := ACPAliases()
	if len(aliases) != 1 {
		t.Fatalf("aliases = %v, want only cursor", aliases)
	}
	aliases[0].Target = "mutated"
	alias, _ = ACPAliasFor(AgentCursor)
	if alias.Target != "cursor" {
		t.Fatalf("ACPAliases should return a copy, target = %q", alias.Target)
	}
}

func TestACPTargetFor(t *testing.T) {
	tests := []struct {
		name       string
		agent      AgentName
		wantTarget string
		wantOK     bool
	}{
		{name: "alias name", agent: AgentCursor, wantTarget: "cursor", wantOK: true},
		{name: "explicit acp alias target", agent: "acp:cursor", wantTarget: "cursor", wantOK: true},
		{name: "explicit acp target", agent: "acp:gemini", wantTarget: "gemini", wantOK: true},
		{name: "native agent", agent: AgentClaude, wantTarget: "", wantOK: false},
		{name: "empty target", agent: "acp:", wantTarget: "", wantOK: false},
		{name: "whitespace in target", agent: "acp:foo bar", wantTarget: "", wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target, ok := ACPTargetFor(tt.agent)
			if target != tt.wantTarget || ok != tt.wantOK {
				t.Fatalf("ACPTargetFor(%q) = (%q, %v), want (%q, %v)", tt.agent, target, ok, tt.wantTarget, tt.wantOK)
			}
		})
	}
}

func TestACPRawCommand(t *testing.T) {
	tests := []struct {
		name      string
		target    string
		overrides map[string]string
		want      string
	}{
		{name: "alias default without overrides", target: "cursor", overrides: nil, want: "cursor-agent acp"},
		{name: "override wins over alias default", target: "cursor", overrides: map[string]string{"cursor": "/opt/cursor/cursor-agent acp"}, want: "/opt/cursor/cursor-agent acp"},
		{name: "override is trimmed", target: "cursor", overrides: map[string]string{"cursor": "  cursor-agent acp  "}, want: "cursor-agent acp"},
		{name: "blank override falls back to alias default", target: "cursor", overrides: map[string]string{"cursor": " \t"}, want: "cursor-agent acp"},
		{name: "unknown target without override", target: "gemini", overrides: nil, want: ""},
		{name: "blank override for unknown target", target: "gemini", overrides: map[string]string{"gemini": "   "}, want: ""},
		{name: "override for unknown target", target: "gemini", overrides: map[string]string{"gemini": "node /tmp/mock-acp.mjs"}, want: "node /tmp/mock-acp.mjs"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ACPRawCommand(tt.target, tt.overrides); got != tt.want {
				t.Fatalf("ACPRawCommand(%q, %v) = %q, want %q", tt.target, tt.overrides, got, tt.want)
			}
		})
	}
}
