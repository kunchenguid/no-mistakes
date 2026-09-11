package config

import (
	"bytes"
	"fmt"
	"path"
	"strings"
	"text/template"
	"text/template/parse"
	"unicode"
	"unicode/utf8"
)

// ValidatePRTemplatePath accepts a literal repository-relative Git path, not a
// filesystem path, glob, ref expression or URL. Empty leaves template mode off.
// Keep this platform-independent: a Windows spelling is unsafe on POSIX too.
func ValidatePRTemplatePath(name string) error {
	if name == "" {
		return nil
	}
	if len(name) > 1024 || strings.TrimSpace(name) != name || strings.HasPrefix(name, "/") || strings.HasPrefix(name, "-") || strings.ContainsAny(name, "\\:") || path.Clean(name) != name {
		return fmt.Errorf("pr.template must be a literal repository-relative path")
	}
	for _, part := range strings.Split(name, "/") {
		if part == "." || part == ".." || part == ".git" || part == "" {
			return fmt.Errorf("pr.template must not traverse directories or name .git")
		}
	}
	if strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return fmt.Errorf("pr.template must not contain control characters")
	}
	return nil
}

const (
	maxPRTitleFormatBytes        = 1024
	maxPRTitleFormatPlaceholders = 16
	maxPRTitleBytes              = 4096
)

type prTitleData struct {
	Branch string
	Title  string
}

func validatePRTitleFormat(format string) error {
	if strings.TrimSpace(format) == "" {
		return fmt.Errorf("pr.title_format must not be empty")
	}
	if len(format) > maxPRTitleFormatBytes {
		return fmt.Errorf("pr.title_format must not exceed %d bytes", maxPRTitleFormatBytes)
	}
	if !utf8.ValidString(format) {
		return fmt.Errorf("pr.title_format must contain valid UTF-8")
	}
	if containsUnsafeFixMessageRune(format) {
		return fmt.Errorf("pr.title_format must not contain control or unsafe Unicode format characters or line separators")
	}
	tmpl, err := template.New("pr.title_format").Option("missingkey=error").Parse(format)
	if err != nil {
		return fmt.Errorf("parse pr.title_format template: %w", err)
	}
	if err := validatePRTitleTemplate(tmpl); err != nil {
		return err
	}
	var rendered bytes.Buffer
	if err := tmpl.Execute(&rendered, prTitleData{Branch: "branch", Title: "title"}); err != nil {
		return fmt.Errorf("render pr.title_format template: %w", err)
	}
	if err := validateRenderedPRTitle(rendered.String()); err != nil {
		return err
	}
	return nil
}

func validatePRTitleTemplate(tmpl *template.Template) error {
	if len(tmpl.Templates()) != 1 || tmpl.Tree == nil || tmpl.Tree.Root == nil {
		return fmt.Errorf("pr.title_format supports only literal text and {{.Branch}} or {{.Title}} placeholders")
	}
	placeholders := 0
	for _, node := range tmpl.Tree.Root.Nodes {
		switch node := node.(type) {
		case *parse.TextNode:
		case *parse.ActionNode:
			if !isPRTitlePlaceholder(node.Pipe) {
				return fmt.Errorf("pr.title_format supports only literal text and {{.Branch}} or {{.Title}} placeholders")
			}
			placeholders++
			if placeholders > maxPRTitleFormatPlaceholders {
				return fmt.Errorf("pr.title_format must not contain more than %d placeholders", maxPRTitleFormatPlaceholders)
			}
		default:
			return fmt.Errorf("pr.title_format supports only literal text and {{.Branch}} or {{.Title}} placeholders")
		}
	}
	return nil
}

func prTitleTemplateUsesBranch(tmpl *template.Template) bool {
	for _, node := range tmpl.Tree.Root.Nodes {
		if action, ok := node.(*parse.ActionNode); ok && isPRTitleField(action.Pipe, "Branch") {
			return true
		}
	}
	return false
}

func isPRTitleField(pipe *parse.PipeNode, want string) bool {
	if pipe == nil || pipe.IsAssign || len(pipe.Decl) != 0 || len(pipe.Cmds) != 1 {
		return false
	}
	command := pipe.Cmds[0]
	if command == nil || len(command.Args) != 1 {
		return false
	}
	field, ok := command.Args[0].(*parse.FieldNode)
	return ok && len(field.Ident) == 1 && field.Ident[0] == want
}

func isPRTitlePlaceholder(pipe *parse.PipeNode) bool {
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
	return field.Ident[0] == "Branch" || field.Ident[0] == "Title"
}

func validateRenderedPRTitle(title string) error {
	if len(title) > maxPRTitleBytes {
		return fmt.Errorf("pr.title_format must not render to more than %d bytes", maxPRTitleBytes)
	}
	if !utf8.ValidString(title) {
		return fmt.Errorf("pr.title_format must render valid UTF-8")
	}
	if containsUnsafeFixMessageRune(title) {
		return fmt.Errorf("pr.title_format must not render control or unsafe Unicode format characters or line separators")
	}
	if strings.TrimSpace(title) == "" {
		return fmt.Errorf("pr.title_format must render to a non-empty title")
	}
	return nil
}

// RequiresBranch reports whether configured PR title format uses the branch
// identifier placeholder.
func (p PR) RequiresBranch() bool {
	if p.TitleFormat == "" {
		return false
	}
	tmpl, err := template.New("pr.title_format").Option("missingkey=error").Parse(p.TitleFormat)
	return err == nil && prTitleTemplateUsesBranch(tmpl)
}

// RenderTitle applies the configured repository PR title format. An empty
// format leaves the caller's title unchanged, preserving conventional-title
// behavior for repositories that configure nothing.
func (p PR) RenderTitle(branch, title string) (string, error) {
	if p.TitleFormat == "" {
		return title, nil
	}
	tmpl, err := template.New("pr.title_format").Option("missingkey=error").Parse(p.TitleFormat)
	if err != nil {
		return "", fmt.Errorf("parse pr.title_format template: %w", err)
	}
	if err := validatePRTitleTemplate(tmpl); err != nil {
		return "", err
	}
	if strings.TrimSpace(branch) == "" && prTitleTemplateUsesBranch(tmpl) {
		return "", fmt.Errorf("pr.title_format requires a non-empty branch identifier")
	}
	var rendered bytes.Buffer
	if err := tmpl.Execute(&rendered, prTitleData{Branch: branch, Title: title}); err != nil {
		return "", fmt.Errorf("render pr.title_format template: %w", err)
	}
	result := strings.TrimSpace(rendered.String())
	if err := validateRenderedPRTitle(result); err != nil {
		return "", err
	}
	return result, nil
}
