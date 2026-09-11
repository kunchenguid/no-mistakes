package config

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"text/template"
	"text/template/parse"
	"unicode"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// DefaultFixMessageTemplate preserves the built-in auto-fix commit subject.
const DefaultFixMessageTemplate = "no-mistakes({{.Step}}): {{.Summary}}"

// Limits are byte-based because they bound allocations and the git commit argument.
// The source and placeholder caps keep repository-controlled parsing cheap, while
// the summary and subject caps prevent placeholder expansion from amplifying data.
const (
	maxFixMessageTemplateBytes = 1024
	maxFixMessagePlaceholders  = 16
	maxFixMessageSubjectBytes  = 4096
	maxBranchPatternBytes      = 1024
)

// MaxFixMessageSummaryBytes bounds agent-provided fix summaries before rendering.
const MaxFixMessageSummaryBytes = 4096

// CommitRaw is the YAML representation of auto-fix commit settings.
type CommitRaw struct {
	FixMessage    *string `yaml:"fix_message"`
	BranchPattern *string `yaml:"branch_pattern"`
}

// Commit is the resolved auto-fix commit configuration.
type Commit struct {
	FixMessage    string
	BranchPattern string
}

type fixMessageData struct {
	Step    types.StepName
	Summary string
	Branch  string
}

func validateCommitRaw(raw CommitRaw) error {
	if raw.BranchPattern != nil {
		if err := validateBranchPattern(*raw.BranchPattern); err != nil {
			return err
		}
	}
	if raw.FixMessage == nil {
		return nil
	}
	if strings.TrimSpace(*raw.FixMessage) == "" {
		return fmt.Errorf("commit.fix_message must not be empty")
	}
	commit := Commit{FixMessage: *raw.FixMessage}
	if raw.BranchPattern != nil {
		commit.BranchPattern = *raw.BranchPattern
	}
	for _, step := range []types.StepName{
		types.StepReview,
		types.StepTest,
		types.StepDocument,
		types.StepLint,
	} {
		if _, err := commit.renderFixMessage(step, "apply fixes", "branch", false); err != nil {
			return err
		}
	}
	return nil
}

func validateBranchPattern(pattern string) error {
	if strings.TrimSpace(pattern) == "" {
		return fmt.Errorf("commit.branch_pattern must not be empty")
	}
	if len(pattern) > maxBranchPatternBytes {
		return fmt.Errorf("commit.branch_pattern must not exceed %d bytes", maxBranchPatternBytes)
	}
	if !utf8.ValidString(pattern) {
		return fmt.Errorf("commit.branch_pattern must contain valid UTF-8")
	}
	if containsUnsafeFixMessageRune(pattern) {
		return fmt.Errorf("commit.branch_pattern must not contain control or unsafe Unicode format characters or line separators")
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return fmt.Errorf("parse commit.branch_pattern: %w", err)
	}
	if re.NumSubexp() != 1 {
		return fmt.Errorf("commit.branch_pattern must contain exactly one capture group")
	}
	return nil
}

// RenderFixMessage renders and validates a single-line auto-fix commit subject.
// It preserves the legacy call shape for callers that do not have a branch.
func (c Commit) RenderFixMessage(step types.StepName, summary string) (string, error) {
	return c.renderFixMessage(step, summary, "", true)
}

// RenderFixMessageForBranch renders an auto-fix commit subject with the branch
// value available to the {{.Branch}} placeholder.
func (c Commit) RenderFixMessageForBranch(step types.StepName, summary, branch string) (string, error) {
	return c.renderFixMessage(step, summary, branch, true)
}

func (c Commit) renderFixMessage(step types.StepName, summary, branch string, resolveBranch bool) (string, error) {
	source := c.FixMessage
	if source == "" {
		source = DefaultFixMessageTemplate
	}
	if len(source) > maxFixMessageTemplateBytes {
		return "", fmt.Errorf("commit.fix_message must not exceed %d bytes", maxFixMessageTemplateBytes)
	}
	if !utf8.ValidString(source) {
		return "", fmt.Errorf("commit.fix_message must contain valid UTF-8")
	}
	if containsUnsafeFixMessageRune(source) {
		return "", fmt.Errorf("commit.fix_message must not contain control or unsafe Unicode format characters or line separators")
	}
	if len(summary) > MaxFixMessageSummaryBytes {
		return "", fmt.Errorf("commit.fix_message summary must not exceed %d bytes", MaxFixMessageSummaryBytes)
	}
	if !utf8.ValidString(summary) {
		return "", fmt.Errorf("commit.fix_message summary must contain valid UTF-8")
	}
	summary = strings.Join(strings.Fields(summary), " ")
	if containsUnsafeFixMessageRune(summary) {
		return "", fmt.Errorf("commit.fix_message summary must not contain control or unsafe Unicode format characters or line separators")
	}
	tmpl, err := template.New("commit.fix_message").Option("missingkey=error").Parse(source)
	if err != nil {
		return "", fmt.Errorf("parse commit.fix_message template: %w", err)
	}
	if err := validateFixMessageTemplate(tmpl); err != nil {
		return "", err
	}
	if fixMessageTemplateUses(tmpl, "Branch") {
		if resolveBranch {
			branch, err = c.BranchValue(branch)
			if err != nil {
				return "", err
			}
		}
	}
	data := fixMessageData{Step: step, Summary: summary, Branch: branch}
	predictedBytes, err := predictFixMessageBytes(tmpl, data)
	if err != nil {
		return "", err
	}
	var rendered bytes.Buffer
	rendered.Grow(predictedBytes)
	if err := tmpl.Execute(&rendered, data); err != nil {
		return "", fmt.Errorf("render commit.fix_message template: %w", err)
	}
	message := rendered.String()
	if len(message) > maxFixMessageSubjectBytes {
		return "", fmt.Errorf("commit.fix_message must not render to more than %d bytes", maxFixMessageSubjectBytes)
	}
	if !utf8.ValidString(message) {
		return "", fmt.Errorf("commit.fix_message must render valid UTF-8")
	}
	if containsUnsafeFixMessageRune(message) {
		return "", fmt.Errorf("commit.fix_message must not contain control or unsafe Unicode format characters or line separators")
	}
	message = strings.TrimSpace(message)
	if message == "" {
		return "", fmt.Errorf("commit.fix_message must render to a non-empty message")
	}
	return message, nil
}

// BranchValue returns the branch value exposed to commit and PR title
// templates. BranchPattern, when configured, must capture the identifier in
// its only capture group; otherwise the normalized branch name is returned.
func (c Commit) BranchValue(branch string) (string, error) {
	branch = strings.TrimSpace(strings.TrimPrefix(branch, "refs/heads/"))
	if c.BranchPattern == "" {
		if branch == "" {
			return "", fmt.Errorf("commit template requires a non-empty branch")
		}
		return branch, nil
	}
	re, err := regexp.Compile(c.BranchPattern)
	if err != nil {
		return "", fmt.Errorf("parse commit.branch_pattern: %w", err)
	}
	match := re.FindStringSubmatch(branch)
	if len(match) < 2 || strings.TrimSpace(match[1]) == "" {
		return "", fmt.Errorf("commit.branch_pattern did not find an identifier in branch %q", branch)
	}
	value := strings.TrimSpace(match[1])
	if !utf8.ValidString(value) || containsUnsafeFixMessageRune(value) {
		return "", fmt.Errorf("commit.branch_pattern captured an invalid branch identifier")
	}
	return value, nil
}

func containsUnsafeFixMessageRune(message string) bool {
	for _, r := range message {
		if unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) ||
			r == '\u2028' || r == '\u2029' || isUnsafeInvisibleFixMessageRune(r) {
			return true
		}
	}
	return false
}

func isUnsafeInvisibleFixMessageRune(r rune) bool {
	// Keep this policy explicit so legitimate ZWNJ and ZWJ text shaping remains
	// available while known invisible spoofing controls stay forbidden.
	switch r {
	case '\u00ad', // soft hyphen
		'\u180e', // Mongolian vowel separator
		'\u200b', // zero width space
		'\u2060', // word joiner
		'\ufeff': // zero width no-break space / byte order mark
		return true
	}
	return r >= '\u2061' && r <= '\u2064' || // invisible mathematical operators
		r >= '\u206a' && r <= '\u206f' || // deprecated bidi formatting controls
		r >= '\ufff9' && r <= '\ufffb' || // interlinear annotation controls
		r >= '\U000e0000' && r <= '\U000e007f' // deprecated language tags and tag characters
}

func validateFixMessageTemplate(tmpl *template.Template) error {
	if len(tmpl.Templates()) != 1 || tmpl.Tree == nil || tmpl.Tree.Root == nil {
		return fmt.Errorf("commit.fix_message supports only literal text and {{.Step}}, {{.Summary}}, or {{.Branch}} placeholders")
	}
	placeholders := 0
	for _, node := range tmpl.Tree.Root.Nodes {
		switch node := node.(type) {
		case *parse.TextNode:
		case *parse.ActionNode:
			if !isFixMessagePlaceholder(node.Pipe) {
				return fmt.Errorf("commit.fix_message supports only literal text and {{.Step}}, {{.Summary}}, or {{.Branch}} placeholders")
			}
			placeholders++
			if placeholders > maxFixMessagePlaceholders {
				return fmt.Errorf("commit.fix_message must not contain more than %d placeholders", maxFixMessagePlaceholders)
			}
		default:
			return fmt.Errorf("commit.fix_message supports only literal text and {{.Step}}, {{.Summary}}, or {{.Branch}} placeholders")
		}
	}
	return nil
}

func predictFixMessageBytes(tmpl *template.Template, data fixMessageData) (int, error) {
	size := 0
	for _, node := range tmpl.Tree.Root.Nodes {
		nodeBytes := 0
		switch node := node.(type) {
		case *parse.TextNode:
			nodeBytes = len(node.Text)
		case *parse.ActionNode:
			name, ok := fixMessagePlaceholderName(node.Pipe)
			if !ok {
				return 0, fmt.Errorf("commit.fix_message supports only literal text and {{.Step}}, {{.Summary}}, or {{.Branch}} placeholders")
			}
			switch name {
			case "Step":
				nodeBytes = len(data.Step)
			case "Summary":
				nodeBytes = len(data.Summary)
			case "Branch":
				nodeBytes = len(data.Branch)
			}
		default:
			return 0, fmt.Errorf("commit.fix_message supports only literal text and {{.Step}}, {{.Summary}}, or {{.Branch}} placeholders")
		}
		if nodeBytes > maxFixMessageSubjectBytes-size {
			return 0, fmt.Errorf("commit.fix_message must not render to more than %d bytes", maxFixMessageSubjectBytes)
		}
		size += nodeBytes
	}
	return size, nil
}

func isFixMessagePlaceholder(pipe *parse.PipeNode) bool {
	_, ok := fixMessagePlaceholderName(pipe)
	return ok
}

func fixMessagePlaceholderName(pipe *parse.PipeNode) (string, bool) {
	if pipe == nil || pipe.IsAssign || len(pipe.Decl) != 0 || len(pipe.Cmds) != 1 {
		return "", false
	}
	command := pipe.Cmds[0]
	if command == nil || len(command.Args) != 1 {
		return "", false
	}
	field, ok := command.Args[0].(*parse.FieldNode)
	if !ok || len(field.Ident) != 1 {
		return "", false
	}
	name := field.Ident[0]
	return name, name == "Step" || name == "Summary" || name == "Branch"
}

func fixMessageTemplateUses(tmpl *template.Template, name string) bool {
	for _, node := range tmpl.Tree.Root.Nodes {
		if action, ok := node.(*parse.ActionNode); ok {
			if placeholder, ok := fixMessagePlaceholderName(action.Pipe); ok && placeholder == name {
				return true
			}
		}
	}
	return false
}
