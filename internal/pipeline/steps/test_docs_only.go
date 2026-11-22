package steps

import (
	"bytes"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func documentationOnlyDiff(sctx *pipeline.StepContext, base string, evidence *types.DocumentationApplicability) (bool, error) {
	head := sctx.Run.HeadSHA
	if evidence == nil || !evidence.DocsOnly || !evidence.NoExecutableExamples || !evidence.NoLiveSurface || strings.TrimSpace(evidence.Reason) == "" || evidence.BaseSHA != base || evidence.HeadSHA != head {
		return false, nil
	}
	actual, err := git.Run(sctx.Ctx, sctx.WorkDir, "rev-parse", "HEAD")
	if err != nil {
		return false, err
	}
	if actual != head {
		return false, nil
	}
	status, err := git.RunRaw(sctx.Ctx, sctx.WorkDir, "status", "--porcelain=v1", "--untracked-files=all", "-z")
	if err != nil {
		return false, err
	}
	if len(status) != 0 {
		return false, nil
	}
	raw, err := git.RunRaw(sctx.Ctx, sctx.WorkDir, "diff", "--raw", "--no-abbrev", "--no-renames", "--no-ext-diff", "-z", base, head, "--")
	if err != nil {
		return false, err
	}
	entries := bytes.Split(raw, []byte{0})
	if len(entries) < 3 || len(entries) > 257 || len(entries)%2 != 1 || len(entries[len(entries)-1]) != 0 {
		return false, nil
	}
	paths := make(map[string]bool, (len(entries)-1)/2)
	for i := 0; i < len(entries)-1; i += 2 {
		fields := strings.Fields(string(entries[i]))
		name := string(entries[i+1])
		if len(fields) != 5 || !strings.HasPrefix(fields[0], ":") || !documentationPath(name) || paths[name] {
			return false, nil
		}
		paths[name] = true
		oldMode, newMode := strings.TrimPrefix(fields[0], ":"), fields[1]
		switch fields[4] {
		case "A":
			if oldMode != "000000" || newMode != "100644" {
				return false, nil
			}
		case "D":
			if oldMode != "100644" || newMode != "000000" {
				return false, nil
			}
		case "M":
			if oldMode != "100644" || newMode != "100644" {
				return false, nil
			}
		default:
			return false, nil
		}
		for j, mode := range []string{oldMode, newMode} {
			if mode == "000000" {
				continue
			}
			blob := fields[2+j]
			sizeText, err := git.Run(sctx.Ctx, sctx.WorkDir, "cat-file", "-s", blob)
			if err != nil {
				return false, err
			}
			size, err := strconv.Atoi(sizeText)
			if err != nil || size < 0 || size > 256*1024 {
				return false, nil
			}
			content, err := git.RunRaw(sctx.Ctx, sctx.WorkDir, "cat-file", "blob", blob)
			if err != nil {
				return false, err
			}
			if len(content) != size {
				return false, nil
			}
			if !documentationProse(string(content)) {
				return false, nil
			}
		}
	}
	if len(evidence.ReviewedPaths) != len(paths) {
		return false, nil
	}
	for _, name := range evidence.ReviewedPaths {
		if !paths[name] {
			return false, nil
		}
		delete(paths, name)
	}
	return true, nil
}

func documentationPath(name string) bool {
	if path.Clean(name) != name || strings.ContainsAny(name, "\\\r\n\t") {
		return false
	}
	parts := strings.Split(strings.ToLower(name), "/")
	for _, part := range parts {
		// These documents can be instructions consumed by agents or executable
		// examples, rather than human-facing documentation.
		switch part {
		case "agents.md", "claude.md", "gemini.md", "skill.md", "context.md", "prompts", "skills", "examples", "snippets", "fixtures", "scripts", "config", "workflows":
			return false
		}
		if strings.HasPrefix(part, ".") {
			return false
		}
	}
	ext := path.Ext(name)
	if ext != ".md" && ext != ".txt" {
		return false
	}
	if len(parts) == 1 {
		switch parts[0] {
		case "readme.md", "readme.txt", "contributing.md", "changelog.md", "license.md", "license.txt", "authors.md":
			return true
		}
		return false
	}
	return parts[0] == "docs" || parts[0] == "doc" || parts[0] == "documentation"
}

var documentationLink = regexp.MustCompile(`\[[^\]\n]+\]\((?:https?://[a-zA-Z0-9./_#?&%=-]+|[a-zA-Z0-9./_#-]+)\)`)

var documentationList = regexp.MustCompile(`^(?:[+*-] |[0-9]{1,9}[.)] )`)

func documentationProse(text string) bool {
	if !utf8.ValidString(text) || strings.ContainsAny(text, "`~<>{}\\$=|&") || strings.Contains(text, "![") {
		return false
	}
	for _, r := range text {
		if unicode.IsControl(r) && r != '\n' && r != '\r' {
			return false
		}
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if strings.ContainsRune(text, '\r') {
		return false
	}
	for _, line := range strings.Split(text, "\n") {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "    ") || strings.HasPrefix(line, "\t") || strings.TrimSpace(line) != line || documentationList.MatchString(line) || strings.HasPrefix(line, ".. ") || strings.HasPrefix(line, "#!") || line == "---" || line == "+++" {
			return false
		}
		if strings.HasPrefix(line, "#") {
			heading := strings.TrimLeft(line, "#")
			if len(line)-len(heading) > 6 || !strings.HasPrefix(heading, " ") || strings.TrimSpace(heading) == "" {
				return false
			}
			line = strings.TrimSpace(heading)
		}
		withoutLinks := documentationLink.ReplaceAllString(line, "link")
		if strings.ContainsAny(withoutLinks, "[]():;!") {
			if strings.HasSuffix(withoutLinks, "!") && !strings.ContainsAny(strings.TrimSuffix(withoutLinks, "!"), "[]():;!") {
				continue
			}
			return false
		}
	}
	return true
}

func documentationNotApplicableEvidence(base, head string) string {
	return fmt.Sprintf("Live validation not applicable: the analyzer assessed the complete %s..%s diff as documentation without executable examples or a live surface, and independent Git and syntax guards passed. The evidence verdict remains no-surface; configured tests and other validation findings still apply.", base, head)
}
