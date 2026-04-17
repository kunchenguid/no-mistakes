package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

type stubAgent struct {
	result *Result
	err    error

	gotPrompt string
	gotCWD    string
	gotSchema json.RawMessage
}

func (s *stubAgent) Name() string { return "stub" }
func (s *stubAgent) Run(_ context.Context, opts RunOpts) (*Result, error) {
	s.gotPrompt = opts.Prompt
	s.gotCWD = opts.CWD
	s.gotSchema = opts.JSONSchema
	if s.err != nil {
		return nil, s.err
	}
	return s.result, nil
}
func (s *stubAgent) Close() error { return nil }

func TestSuggestBranchName(t *testing.T) {
	ag := &stubAgent{result: &Result{
		Output: json.RawMessage(`{"name":"feat/onboarding-wizard"}`),
	}}
	name, err := SuggestBranchName(context.Background(), ag, "/tmp/repo")
	if err != nil {
		t.Fatalf("SuggestBranchName failed: %v", err)
	}
	if name != "feat/onboarding-wizard" {
		t.Fatalf("expected feat/onboarding-wizard, got %q", name)
	}
	if ag.gotCWD != "/tmp/repo" {
		t.Fatalf("expected CWD to be forwarded, got %q", ag.gotCWD)
	}
	if len(ag.gotSchema) == 0 {
		t.Fatal("expected JSONSchema to be set on RunOpts")
	}
	if ag.gotPrompt == "" {
		t.Fatal("expected prompt to be non-empty")
	}
}

func TestSuggestBranchNameSanitizes(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"spaces to hyphen", "feat new wizard", "feat-new-wizard"},
		{"uppercase to lower", "FEAT/NewThing", "feat/newthing"},
		{"strip quotes", "\"fix/bug\"", "fix/bug"},
		{"collapse dashes", "fix--double---dash", "fix-double-dash"},
		{"strip trailing slash", "feat/", "feat"},
		{"strip bad chars", "fix: thing! @home", "fix-thing-home"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ag := &stubAgent{result: &Result{
				Output: json.RawMessage(`{"name":` + jsonQuote(tc.raw) + `}`),
			}}
			got, err := SuggestBranchName(context.Background(), ag, "/tmp")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("expected %q, got %q", tc.want, got)
			}
		})
	}
}

func TestSuggestBranchNameRejectsInvalidGitRefs(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{name: "double dot", raw: "feat/bug..fix"},
		{name: "double slash", raw: "feat//wizard"},
		{name: "lock suffix", raw: "feat/wizard.lock"},
		{name: "lock path component", raw: "feat/topic.lock/extra"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ag := &stubAgent{result: &Result{
				Output: json.RawMessage(`{"name":` + jsonQuote(tc.raw) + `}`),
			}}
			if _, err := SuggestBranchName(context.Background(), ag, "/tmp"); err == nil {
				t.Fatalf("expected invalid ref %q to be rejected", tc.raw)
			}
		})
	}
}

func TestSuggestBranchNameLengthCapped(t *testing.T) {
	long := "feat/really-long-branch-name-that-exceeds-the-limit-we-enforce-for-clarity"
	ag := &stubAgent{result: &Result{
		Output: json.RawMessage(`{"name":` + jsonQuote(long) + `}`),
	}}
	got, err := SuggestBranchName(context.Background(), ag, "/tmp")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) > 60 {
		t.Fatalf("expected <= 60 chars, got %d: %q", len(got), got)
	}
}

func TestSuggestBranchNameEmpty(t *testing.T) {
	ag := &stubAgent{result: &Result{
		Output: json.RawMessage(`{"name":""}`),
	}}
	_, err := SuggestBranchName(context.Background(), ag, "/tmp")
	if err == nil {
		t.Fatal("expected error for empty name")
	}
}

func TestSuggestBranchNameOnlyInvalidChars(t *testing.T) {
	ag := &stubAgent{result: &Result{
		Output: json.RawMessage(`{"name":"!@#$%"}`),
	}}
	_, err := SuggestBranchName(context.Background(), ag, "/tmp")
	if err == nil {
		t.Fatal("expected error when all chars are stripped")
	}
}

func TestSuggestBranchNameAgentError(t *testing.T) {
	ag := &stubAgent{err: errors.New("boom")}
	_, err := SuggestBranchName(context.Background(), ag, "/tmp")
	if err == nil {
		t.Fatal("expected error from agent failure")
	}
}

func TestSuggestCommitMessage(t *testing.T) {
	ag := &stubAgent{result: &Result{
		Output: json.RawMessage(`{"subject":"feat(cli): add onboarding wizard"}`),
	}}
	got, err := SuggestCommitMessage(context.Background(), ag, "/tmp")
	if err != nil {
		t.Fatalf("SuggestCommitMessage failed: %v", err)
	}
	if got != "feat(cli): add onboarding wizard" {
		t.Fatalf("unexpected subject: %q", got)
	}
}

func TestSuggestCommitMessageTrimsNewlines(t *testing.T) {
	ag := &stubAgent{result: &Result{
		Output: json.RawMessage(`{"subject":"fix: thing\n\nbody"}`),
	}}
	got, err := SuggestCommitMessage(context.Background(), ag, "/tmp")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "fix: thing" {
		t.Fatalf("expected first line only, got %q", got)
	}
}

func TestSuggestCommitMessageEmpty(t *testing.T) {
	ag := &stubAgent{result: &Result{
		Output: json.RawMessage(`{"subject":"   "}`),
	}}
	_, err := SuggestCommitMessage(context.Background(), ag, "/tmp")
	if err == nil {
		t.Fatal("expected error for empty subject")
	}
}

func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
