package config

import (
	"fmt"
	"path"
	"strings"
	"unicode"
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
