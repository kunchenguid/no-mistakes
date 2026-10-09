package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The review step's question-channel instruction, verbatim enough to locate
// the run's conversation directory in the prompt it hands the reviewer:
//
//   - You have a question channel: <dir>/questions.ndjson (you append) and ...
//
// The fake reviewer learns where to write the same way a real one does, from
// the prompt itself, so a scenario can never write somewhere the product did
// not name. That is also why this is not routed through the scenario `edits`
// path, which deliberately refuses anything outside the worktree: the run's
// evidence directory is outside it by design, and the protocol section calls
// writing there an explicit, required exception.
const (
	questionChannelMarker  = "question channel: "
	questionChannelTrailer = "/questions.ndjson"
)

// reviewConversationDirFromPrompt returns the run's review conversation
// directory as named by the review prompt, or "" when the prompt carries no
// question channel (the conversation is off, which is the default).
func reviewConversationDirFromPrompt(prompt string) string {
	i := strings.Index(prompt, questionChannelMarker)
	if i < 0 {
		return ""
	}
	rest := prompt[i+len(questionChannelMarker):]
	j := strings.Index(rest, questionChannelTrailer)
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// askQuestions appends each raw questions.ndjson line to the run's review
// conversation, exactly as the agent's own file tools would. The lines are
// written verbatim so a scenario controls which fields are present - above all
// which are ABSENT, since the reader's absent-field branches are real
// behaviour a helper that defaulted them could never exercise.
func askQuestions(prompt string, lines []string) error {
	if len(lines) == 0 {
		return nil
	}
	dir := reviewConversationDirFromPrompt(prompt)
	if dir == "" {
		return fmt.Errorf("scenario asks a review question but the prompt names no question channel")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create review conversation dir: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "questions.ndjson"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open questions file: %w", err)
	}
	defer f.Close()
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if _, err := f.WriteString(strings.TrimRight(line, "\n") + "\n"); err != nil {
			return fmt.Errorf("append question: %w", err)
		}
	}
	return nil
}

// The test step's evidence-directory instruction, in both the plain and the
// "published to the evidence branch" wordings. Both end the line with ": <dir>".
const evidenceDirMarker = "evidence files into this evidence directory, never into the worktree"

// evidenceDirFromPrompt returns the run's evidence directory as named by the
// test prompt, or "" when the prompt does not name one.
func evidenceDirFromPrompt(prompt string) string {
	i := strings.Index(prompt, evidenceDirMarker)
	if i < 0 {
		return ""
	}
	line := prompt[i:]
	if j := strings.IndexByte(line, '\n'); j >= 0 {
		line = line[:j]
	}
	k := strings.LastIndex(line, ": ")
	if k < 0 {
		return ""
	}
	return strings.TrimSpace(line[k+2:])
}

// writeEvidence drops scenario-supplied test evidence into the run's evidence
// directory, which is where the steering preamble tells a real test agent to
// put it and is outside the worktree by design. Like askQuestions, the
// destination comes from the prompt rather than the scenario.
func writeEvidence(prompt string, files []EvidenceFile) error {
	if len(files) == 0 {
		return nil
	}
	dir := evidenceDirFromPrompt(prompt)
	if dir == "" {
		return fmt.Errorf("scenario writes test evidence but the prompt names no evidence directory")
	}
	for _, f := range files {
		if strings.TrimSpace(f.Path) == "" {
			continue
		}
		full := filepath.Join(dir, filepath.Clean(f.Path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return fmt.Errorf("create evidence dir: %w", err)
		}
		if err := os.WriteFile(full, []byte(f.Content), 0o644); err != nil {
			return fmt.Errorf("write evidence %s: %w", f.Path, err)
		}
	}
	return nil
}
