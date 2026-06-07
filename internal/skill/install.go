package skill

import (
	"os"
	"path/filepath"
)

// InstallBases are the per-agent skill parent directories, relative to a repo
// root, that init populates. `.claude/skills` is Claude Code's native location;
// `.agents/skills` is the vendor-neutral convention other agents read.
var InstallBases = []string{
	filepath.Join(".claude", "skills"),
	filepath.Join(".agents", "skills"),
}

// Install writes SKILL.md into each agent skills directory under repoRoot,
// creating directories as needed. It returns the repo-relative paths written so
// the caller can report them. Writing is idempotent: re-running overwrites with
// identical content.
func Install(repoRoot string) ([]string, error) {
	content := []byte(Markdown())
	written := make([]string, 0, len(InstallBases))
	for _, base := range InstallBases {
		rel := filepath.Join(base, Name, "SKILL.md")
		path := filepath.Join(repoRoot, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return written, err
		}
		if err := os.WriteFile(path, content, 0o644); err != nil {
			return written, err
		}
		written = append(written, rel)
	}
	return written, nil
}
