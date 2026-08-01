package main

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestPatchClaudeFixtureStructuredRunRewritesAssistantText(t *testing.T) {
	t.Helper()

	raw := []byte("{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"recorded assistant text\"}]}}\n{\"type\":\"result\",\"result\":\"recorded result\",\"structured_output\":{\"summary\":\"recorded summary\"}}\n")
	patched, err := patchClaudeFixture(raw, Action{
		Text:       "scenario text",
		Structured: map[string]any{"summary": "patched summary"},
	}, nil)
	if err != nil {
		t.Fatalf("patchClaudeFixture: %v", err)
	}

	lines := bytes.Split(bytes.TrimSpace(patched), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("got %d jsonl lines, want 2", len(lines))
	}

	var assistant struct {
		Type    string `json:"type"`
		Message struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(lines[0], &assistant); err != nil {
		t.Fatalf("unmarshal assistant event: %v", err)
	}
	if assistant.Type != "assistant" {
		t.Fatalf("assistant type = %q, want assistant", assistant.Type)
	}
	if len(assistant.Message.Content) != 1 || assistant.Message.Content[0].Text != "scenario text" {
		t.Fatalf("assistant content = %+v, want scenario text", assistant.Message.Content)
	}

	var result struct {
		Type             string          `json:"type"`
		Result           string          `json:"result"`
		StructuredOutput json.RawMessage `json:"structured_output"`
	}
	if err := json.Unmarshal(lines[1], &result); err != nil {
		t.Fatalf("unmarshal result event: %v", err)
	}
	if result.Result != "scenario text" {
		t.Fatalf("result text = %q, want scenario text", result.Result)
	}
	if string(result.StructuredOutput) != `{"summary":"patched summary"}` {
		t.Fatalf("structured_output = %s, want patched payload", result.StructuredOutput)
	}
}

func TestPatchClaudeFixtureAddsAssistantEventWhenMissing(t *testing.T) {
	t.Helper()

	raw := []byte("{\"type\":\"result\",\"result\":\"recorded result\",\"structured_output\":{\"summary\":\"recorded summary\"}}\n")
	patched, err := patchClaudeFixture(raw, Action{
		Text:       "scenario text",
		Structured: map[string]any{"summary": "patched summary"},
	}, nil)
	if err != nil {
		t.Fatalf("patchClaudeFixture: %v", err)
	}

	lines := bytes.Split(bytes.TrimSpace(patched), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("got %d jsonl lines, want assistant + result", len(lines))
	}
	var assistant struct {
		Type    string `json:"type"`
		Message struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(lines[0], &assistant); err != nil {
		t.Fatalf("unmarshal injected assistant event: %v", err)
	}
	if assistant.Type != "assistant" || len(assistant.Message.Content) != 1 || assistant.Message.Content[0].Text != "scenario text" {
		t.Fatalf("injected assistant event = %+v, want scenario text", assistant)
	}
}

func TestMandatedSkillsReadsThePromptsSkillRequirement(t *testing.T) {
	prompt := "- Run the review by invoking the `comprehensive-code-review` skill (Skill tool, no arguments) against the review scope above"
	got := mandatedSkills(prompt)
	if len(got) != 1 || got[0] != "comprehensive-code-review" {
		t.Fatalf("mandatedSkills = %v, want [comprehensive-code-review]", got)
	}
	if got := mandatedSkills("Fix the findings below and commit."); len(got) != 0 {
		t.Fatalf("mandatedSkills on a prompt with no requirement = %v, want none", got)
	}
	dup := "invoke the `comprehensive-code-review` skill; the `comprehensive-code-review` skill is mandatory"
	if got := mandatedSkills(dup); len(got) != 1 {
		t.Fatalf("mandatedSkills = %v, want one entry per skill", got)
	}
}

// TestPatchClaudeFixtureReportsMandatedSkillUse pins the wire shape the claude
// adapter reads skill use from: a Skill tool_use content item in an assistant
// event. Without it every review step fails closed on the skill requirement.
func TestPatchClaudeFixtureReportsMandatedSkillUse(t *testing.T) {
	raw := []byte("{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"recorded\"}]}}\n{\"type\":\"result\",\"result\":\"recorded result\",\"structured_output\":{\"summary\":\"recorded summary\"}}\n")
	patched, err := patchClaudeFixture(raw, Action{
		Text:       "scenario text",
		Structured: map[string]any{"summary": "patched summary"},
	}, []string{"comprehensive-code-review"})
	if err != nil {
		t.Fatalf("patchClaudeFixture: %v", err)
	}
	if got := claudeSkillsUsed(t, patched); len(got) != 1 || got[0] != "comprehensive-code-review" {
		t.Fatalf("skills reported on the wire = %v, want [comprehensive-code-review]", got)
	}
}

// claudeSkillsUsed extracts skill names the way internal/agent's claude adapter
// does, so the fake and the parser cannot drift apart silently.
func claudeSkillsUsed(t *testing.T, stream []byte) []string {
	t.Helper()
	skills := []string{}
	for _, line := range bytes.Split(bytes.TrimSpace(stream), []byte("\n")) {
		var event struct {
			Type    string `json:"type"`
			Message struct {
				Content []struct {
					Type  string `json:"type"`
					Name  string `json:"name"`
					Input struct {
						Skill string `json:"skill"`
					} `json:"input"`
				} `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(line, &event); err != nil || event.Type != "assistant" {
			continue
		}
		for _, c := range event.Message.Content {
			if c.Type == "tool_use" && c.Name == "Skill" && c.Input.Skill != "" {
				skills = append(skills, c.Input.Skill)
			}
		}
	}
	return skills
}

func TestPatchClaudeFixtureStructuredRunPreservesNonTextAssistantContent(t *testing.T) {
	t.Helper()

	raw := []byte("{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"thinking\",\"thinking\":\"recorded thinking\"},{\"type\":\"tool_use\",\"name\":\"Read\"},{\"type\":\"text\",\"text\":\"recorded assistant text\"}]}}\n{\"type\":\"result\",\"result\":\"recorded result\",\"structured_output\":{\"summary\":\"recorded summary\"}}\n")
	patched, err := patchClaudeFixture(raw, Action{
		Text:       "scenario text",
		Structured: map[string]any{"summary": "patched summary"},
	}, nil)
	if err != nil {
		t.Fatalf("patchClaudeFixture: %v", err)
	}

	lines := bytes.Split(bytes.TrimSpace(patched), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("got %d jsonl lines, want 2", len(lines))
	}

	var assistant struct {
		Message struct {
			Content []map[string]any `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(lines[0], &assistant); err != nil {
		t.Fatalf("unmarshal assistant event: %v", err)
	}
	if len(assistant.Message.Content) != 3 {
		t.Fatalf("assistant content len = %d, want 3", len(assistant.Message.Content))
	}
	if assistant.Message.Content[0]["type"] != "thinking" {
		t.Fatalf("first content type = %v, want thinking", assistant.Message.Content[0]["type"])
	}
	if assistant.Message.Content[1]["type"] != "tool_use" {
		t.Fatalf("second content type = %v, want tool_use", assistant.Message.Content[1]["type"])
	}
	if assistant.Message.Content[2]["type"] != "text" || assistant.Message.Content[2]["text"] != "scenario text" {
		t.Fatalf("third content = %+v, want patched text item", assistant.Message.Content[2])
	}
}
