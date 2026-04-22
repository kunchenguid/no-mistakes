package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Scenario is a list of canned responses matched against the prompt text.
// The first entry whose Match substring appears in the prompt wins. The
// final unconditional response is the default (matches everything).
type Scenario struct {
	Actions []Action `yaml:"actions"`
}

// Action describes a single canned response. Match is a substring tested
// against the prompt; an empty Match always matches and is treated as a
// catch-all when listed last. Edits are applied to the working directory
// before the response is emitted, so subsequent pipeline steps see the
// changes (this is how a "fix" round actually mutates files).
type Action struct {
	Match string `yaml:"match"`

	// Structured is the JSON body returned in the structured-output slot
	// (claude.result.structured_output, opencode.info.structured, or the
	// agent_message.text payload for codex). Encoded back to JSON when
	// emitted, so YAML authors can write it inline without escaping.
	Structured map[string]any `yaml:"structured,omitempty"`

	// Text is the human-readable response shown alongside structured
	// output. Defaults to a generic acknowledgement.
	Text string `yaml:"text,omitempty"`

	// Edits are file modifications applied in CWD before responding.
	Edits []Edit `yaml:"edits,omitempty"`
}

// Edit performs a Replace of Old with New in Path. If Old is empty the
// whole file is overwritten with New. If the file does not exist it is
// created.
type Edit struct {
	Path string `yaml:"path"`
	Old  string `yaml:"old,omitempty"`
	New  string `yaml:"new,omitempty"`
}

func loadScenario(path string) (*Scenario, error) {
	if path == "" {
		return defaultScenario(), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read scenario %q: %w", path, err)
	}
	var s Scenario
	if err := yaml.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse scenario %q: %w", path, err)
	}
	return &s, nil
}

// defaultScenario returns an "everything is clean" response that satisfies
// every JSON schema no-mistakes hands to an agent: empty findings array,
// low risk, a populated tested array for the test step.
func defaultScenario() *Scenario {
	return &Scenario{
		Actions: []Action{{
			Text: "no issues found",
			Structured: map[string]any{
				"findings":        []any{},
				"summary":         "no issues found",
				"risk_level":      "low",
				"risk_rationale":  "no risks detected in the diff",
				"tested":          []string{"fakeagent: simulated test run"},
				"testing_summary": "simulated tests passed",
				"title":           "feat: fakeagent change",
				"body":            "## Summary\nfakeagent canned PR body",
			},
		}},
	}
}

// Match returns the first action whose Match substring is contained in the
// prompt. An empty Match matches everything, so a single trailing entry
// can serve as the catch-all.
func (s *Scenario) Match(prompt string) Action {
	for _, a := range s.Actions {
		if a.Match == "" || strings.Contains(prompt, a.Match) {
			return a
		}
	}
	return Action{Text: "no matching scenario"}
}

// applyEdits mutates files under CWD (which is the worktree no-mistakes
// pointed the agent at). Errors are logged to stderr but not fatal so a
// scenario with a stale path doesn't kill the whole run.

func applyEdits(edits []Edit) error {
	var errs []error
	for _, e := range edits {
		if e.Path == "" {
			continue
		}
		if e.Old == "" {
			if err := os.WriteFile(e.Path, []byte(e.New), 0o644); err != nil {
				err = fmt.Errorf("write %s: %w", e.Path, err)
				fmt.Fprintf(os.Stderr, "fakeagent: %v\n", err)
				errs = append(errs, err)
			}
			continue
		}
		data, err := os.ReadFile(e.Path)
		if err != nil {
			err = fmt.Errorf("read %s: %w", e.Path, err)
			fmt.Fprintf(os.Stderr, "fakeagent: %v\n", err)
			errs = append(errs, err)
			continue
		}
		if !strings.Contains(string(data), e.Old) {
			err = fmt.Errorf("replace %s: old text not found", e.Path)
			fmt.Fprintf(os.Stderr, "fakeagent: %v\n", err)
			errs = append(errs, err)
			continue
		}
		updated := strings.Replace(string(data), e.Old, e.New, 1)
		if err := os.WriteFile(e.Path, []byte(updated), 0o644); err != nil {
			err = fmt.Errorf("write %s: %w", e.Path, err)
			fmt.Fprintf(os.Stderr, "fakeagent: %v\n", err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// structuredJSON marshals an action's Structured map. Empty structured
// becomes an empty object so the parser sees something parseable.
func (a Action) structuredJSON() []byte {
	if a.Structured == nil {
		return []byte("{}")
	}
	data, err := json.Marshal(a.Structured)
	if err != nil {
		return []byte("{}")
	}
	return data
}

func (a Action) textOrDefault() string {
	if a.Text != "" {
		return a.Text
	}
	return "ok"
}
