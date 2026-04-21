package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// branchNameRules and commitSubjectRules are shared between the single-purpose
// prompts and the combined prompt so behavior stays in lock-step. The commit
// rules mirror the PR title rules in internal/pipeline/steps/pr.go so a commit
// made through the wizard and the PR title generated later feel consistent.
const branchNameRules = `- Use kebab-case.
- Prefer a conventional prefix: "feat/", "fix/", "chore/", "refactor/", "docs/", or "test/".
- Keep it under 40 characters.`

const commitSubjectRules = `- One line only, under 72 characters.
- Use conventional commit format: "type(scope): description" or "type: description". Valid types: feat, fix, docs, style, refactor, perf, test, build, ci, chore, revert. Scope is optional. Do not capitalize the type.
- When including a scope, it MUST be a real package/module name that exists in the codebase (for example, a directory under internal/, cmd/, or the equivalent top-level grouping for this project), identified by inspecting the changed paths. Pick the primary module affected by the change, not a secondary or incidental one.
- Keep the scope at a coarse level, not too granular: a codebase typically has fewer than 10 distinct scopes in use across its history. Prefer a broad module name (e.g. "daemon", "pipeline", "cli") over a narrow file or sub-feature name. If you cannot confidently identify a real primary module, omit the scope and use "type: description".
- Do not invent behavior.`

const branchNamePrompt = `Suggest a short, descriptive git branch name for the current working-tree changes in this repository.

Inspect the state yourself (e.g. git status, git diff HEAD, git diff --staged) in the working directory.

Rules:
` + branchNameRules + `
- Return JSON: {"name":"..."}`

const commitSubjectPrompt = `Suggest a conventional commit subject line summarizing the current working-tree changes.

Inspect the state yourself (e.g. git status, git diff HEAD, git diff --staged) in the working directory.

Rules:
` + commitSubjectRules + `
- Return JSON: {"subject":"..."}`

const branchAndCommitPrompt = `Suggest a git branch name and a conventional commit subject for the current working-tree changes in this repository.

Inspect the state yourself (e.g. git status, git diff HEAD, git diff --staged) in the working directory.

Branch name rules:
` + branchNameRules + `

Commit subject rules:
` + commitSubjectRules + `

Return JSON: {"branch":"...","subject":"..."}`

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

var branchAndCommitSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"branch": {"type": "string"},
		"subject": {"type": "string"}
	},
	"required": ["branch", "subject"]
}`)

type branchSuggestion struct {
	Name string `json:"name"`
}

type commitSuggestion struct {
	Subject string `json:"subject"`
}

type branchAndCommitSuggestion struct {
	Branch  string `json:"branch"`
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

// SuggestBranchAndCommit asks the agent to propose both a git branch name and
// a conventional commit subject for the current working-tree state in a
// single call. Combining the two saves one full agent round-trip when the
// wizard needs both (new branch + dirty tree).
//
// The branch name must be present and sanitizes to a valid git ref; otherwise
// an error is returned. The commit subject is best-effort: if the agent
// returns an empty subject, this function returns an empty string with no
// error so the caller can fall back to SuggestCommitMessage.
func SuggestBranchAndCommit(ctx context.Context, ag Agent, dir string) (branch, subject string, err error) {
	result, err := ag.Run(ctx, RunOpts{
		Prompt:     branchAndCommitPrompt,
		CWD:        dir,
		JSONSchema: branchAndCommitSchema,
	})
	if err != nil {
		return "", "", fmt.Errorf("suggest branch and commit: %w", err)
	}
	var parsed branchAndCommitSuggestion
	if err := unmarshalSuggestion(result, &parsed); err != nil {
		return "", "", fmt.Errorf("parse branch and commit suggestion: %w", err)
	}
	branch = sanitizeBranchName(parsed.Branch)
	if branch == "" {
		return "", "", fmt.Errorf("agent returned empty or unusable branch name")
	}
	subject = sanitizeCommitSubject(parsed.Subject)
	return branch, subject, nil
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
	if !isValidBranchName(s) {
		return ""
	}
	return s
}

func isValidBranchName(name string) bool {
	if name == "" || name == "@" {
		return false
	}
	if strings.HasPrefix(name, "-") || strings.HasPrefix(name, "/") {
		return false
	}
	if strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".") {
		return false
	}
	if strings.Contains(name, "..") || strings.Contains(name, "//") || strings.Contains(name, "@{") {
		return false
	}
	for _, r := range name {
		if r < ' ' || r == 0x7f {
			return false
		}
		switch r {
		case ' ', '~', '^', ':', '?', '*', '[', '\\':
			return false
		}
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
		if strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}

// sanitizeCommitSubject trims whitespace and keeps only the first line.
func sanitizeCommitSubject(raw string) string {
	s := strings.TrimSpace(raw)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	return s
}
