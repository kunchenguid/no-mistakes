package config

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"
	"text/template/parse"
	"unicode"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// DefaultFixMessageTemplate preserves the built-in auto-fix commit subject.
const DefaultFixMessageTemplate = "no-mistakes({{.Step}}): {{.Summary}}"

// CommitRaw is the YAML representation of auto-fix commit settings.
type CommitRaw struct {
	FixMessage *string `yaml:"fix_message"`
}

// Commit is the resolved auto-fix commit configuration.
type Commit struct {
	FixMessage string
}

type fixMessageData struct {
	Step    types.StepName
	Summary string
}

func validateCommitRaw(raw CommitRaw) error {
	if raw.FixMessage == nil {
		return nil
	}
	if strings.TrimSpace(*raw.FixMessage) == "" {
		return fmt.Errorf("commit.fix_message must not be empty")
	}
	for _, step := range []types.StepName{
		types.StepReview,
		types.StepTest,
		types.StepDocument,
		types.StepLint,
	} {
		if _, err := (Commit{FixMessage: *raw.FixMessage}).RenderFixMessage(step, "apply fixes"); err != nil {
			return err
		}
	}
	return nil
}

// RenderFixMessage renders and validates a single-line auto-fix commit subject.
func (c Commit) RenderFixMessage(step types.StepName, summary string) (string, error) {
	source := c.FixMessage
	if source == "" {
		source = DefaultFixMessageTemplate
	}
	summary = strings.Join(strings.Fields(summary), " ")
	tmpl, err := template.New("commit.fix_message").Option("missingkey=error").Parse(source)
	if err != nil {
		return "", fmt.Errorf("parse commit.fix_message template: %w", err)
	}
	if err := validateFixMessageTemplate(tmpl); err != nil {
		return "", err
	}
	var rendered bytes.Buffer
	if err := tmpl.Execute(&rendered, fixMessageData{Step: step, Summary: summary}); err != nil {
		return "", fmt.Errorf("render commit.fix_message template: %w", err)
	}
	message := rendered.String()
	if containsUnsafeFixMessageRune(message) {
		return "", fmt.Errorf("commit.fix_message must not contain control characters or Unicode line separators")
	}
	message = strings.TrimSpace(message)
	if message == "" {
		return "", fmt.Errorf("commit.fix_message must render to a non-empty message")
	}
	return message, nil
}

func containsUnsafeFixMessageRune(message string) bool {
	for _, r := range message {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			return true
		}
	}
	return false
}

func validateFixMessageTemplate(tmpl *template.Template) error {
	if len(tmpl.Templates()) != 1 || tmpl.Tree == nil || tmpl.Tree.Root == nil {
		return fmt.Errorf("commit.fix_message supports only literal text and {{.Step}} or {{.Summary}} placeholders")
	}
	for _, node := range tmpl.Tree.Root.Nodes {
		switch node := node.(type) {
		case *parse.TextNode:
		case *parse.ActionNode:
			if !isFixMessagePlaceholder(node.Pipe) {
				return fmt.Errorf("commit.fix_message supports only literal text and {{.Step}} or {{.Summary}} placeholders")
			}
		default:
			return fmt.Errorf("commit.fix_message supports only literal text and {{.Step}} or {{.Summary}} placeholders")
		}
	}
	return nil
}

func isFixMessagePlaceholder(pipe *parse.PipeNode) bool {
	if pipe == nil || pipe.IsAssign || len(pipe.Decl) != 0 || len(pipe.Cmds) != 1 {
		return false
	}
	command := pipe.Cmds[0]
	if command == nil || len(command.Args) != 1 {
		return false
	}
	field, ok := command.Args[0].(*parse.FieldNode)
	if !ok || len(field.Ident) != 1 {
		return false
	}
	return field.Ident[0] == "Step" || field.Ident[0] == "Summary"
}
