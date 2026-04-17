package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

const branchNamePrompt = `Suggest a short, descriptive git branch name for the current working-tree changes in this repository.

Inspect the state yourself (e.g. git status, git diff HEAD, git diff --staged) in the working directory.

Rules:
- Use kebab-case.
- Prefer a conventional prefix: "feat/", "fix/", "chore/", "refactor/", "docs/", or "test/".
- Keep it under 40 characters.
- Return JSON: {"name":"..."}`

const commitSubjectPrompt = `Suggest a conventional commit subject line summarizing the current working-tree changes.

Inspect the state yourself (e.g. git status, git diff HEAD, git diff --staged) in the working directory.

Rules:
- One line only.
- Use conventional commit style: "type(scope): description".
- Keep it under 72 characters.
- Return JSON: {"subject":"..."}`

var branchNameSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"name": {"type": "string"}
	},
	"required": ["name"]
}`)

var commitSubjectSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"subject": {"type": "string"}
	},
	"required": ["subject"]
}`)

type branchSuggestion struct {
	Name string `json:"name"`
}

type commitSuggestion struct {
	Subject string `json:"subject"`
}

// SuggestBranchName asks the agent to propose a short git branch name for the
// current working-tree state in dir. The suggestion is sanitized so it's safe
// to pass to `git checkout -b`.
func SuggestBranchName(ctx context.Context, ag Agent, dir string) (string, error) {
	result, err := ag.Run(ctx, RunOpts{
		Prompt:     branchNamePrompt,
		CWD:        dir,
		JSONSchema: branchNameSchema,
	})
	if err != nil {
		return "", fmt.Errorf("suggest branch name: %w", err)
	}
	var parsed branchSuggestion
	if err := unmarshalSuggestion(result, &parsed); err != nil {
		return "", fmt.Errorf("parse branch name suggestion: %w", err)
	}
	name := sanitizeBranchName(parsed.Name)
	if name == "" {
		return "", fmt.Errorf("agent returned empty or unusable branch name")
	}
	return name, nil
}

// SuggestCommitMessage asks the agent to propose a single-line commit subject
// summarizing the current working-tree state at dir.
func SuggestCommitMessage(ctx context.Context, ag Agent, dir string) (string, error) {
	result, err := ag.Run(ctx, RunOpts{
		Prompt:     commitSubjectPrompt,
		CWD:        dir,
		JSONSchema: commitSubjectSchema,
	})
	if err != nil {
		return "", fmt.Errorf("suggest commit message: %w", err)
	}
	var parsed commitSuggestion
	if err := unmarshalSuggestion(result, &parsed); err != nil {
		return "", fmt.Errorf("parse commit message suggestion: %w", err)
	}
	subject := sanitizeCommitSubject(parsed.Subject)
	if subject == "" {
		return "", fmt.Errorf("agent returned empty commit subject")
	}
	return subject, nil
}

func unmarshalSuggestion(result *Result, v any) error {
	if result == nil {
		return fmt.Errorf("agent returned no result")
	}
	if len(result.Output) > 0 {
		return json.Unmarshal(result.Output, v)
	}
	if result.Text != "" {
		return json.Unmarshal([]byte(result.Text), v)
	}
	return fmt.Errorf("agent returned no output")
}

// sanitizeBranchName normalizes an agent-suggested branch name: lowercase,
// ASCII-only, alphanumerics plus - / . separators, length-capped.
func sanitizeBranchName(raw string) string {
	s := strings.TrimSpace(strings.Trim(raw, "\"'"))
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '/', r == '.':
			b.WriteRune(r)
		case r == '-', r == '_', r == ' ':
			b.WriteRune('-')
		}
	}
	s = b.String()
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	s = strings.Trim(s, "-/.")
	if len(s) > 60 {
		s = s[:60]
		s = strings.Trim(s, "-/.")
	}
	return s
}

// sanitizeCommitSubject trims whitespace and keeps only the first line.
func sanitizeCommitSubject(raw string) string {
	s := strings.TrimSpace(raw)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	return s
}
