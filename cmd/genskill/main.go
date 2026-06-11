// Command genskill renders the canonical no-mistakes SKILL.md from the
// internal/skill package into skills/no-mistakes/SKILL.md, plus this repo's
// own vendored copy in .agents/skills (marked internal so skill discovery
// only surfaces the canonical one).
//
// Usage:
//
//	go run ./cmd/genskill           # (re)write the skill files
//	go run ./cmd/genskill --check   # fail if a committed file is stale
//
// The --check form is meant for CI so the committed skills never drift from
// the generator, which is the single source of truth.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kunchenguid/no-mistakes/internal/skill"
)

// outputs are the committed skill files, relative to the repo root. The first
// is the canonical public skill that `npx skills add` discovers; the second is
// the vendored copy init installs here, identical to what init writes into any
// target repo (.claude/skills is a symlink to .agents/skills in this repo).
var outputs = []struct {
	rel  string
	want func() string
}{
	{filepath.Join("skills", skill.Name, "SKILL.md"), skill.Markdown},
	{filepath.Join(".agents", "skills", skill.Name, "SKILL.md"), skill.InstalledMarkdown},
}

func main() {
	check := flag.Bool("check", false, "verify the committed skills match the generator instead of writing them")
	flag.Parse()

	for _, out := range outputs {
		want := out.want()

		if *check {
			got, err := os.ReadFile(out.rel)
			if err != nil {
				fmt.Fprintf(os.Stderr, "genskill --check: read %s: %v\n", out.rel, err)
				os.Exit(1)
			}
			if string(got) != want {
				fmt.Fprintf(os.Stderr, "genskill --check: %s is stale; run `go run ./cmd/genskill` and commit the result\n", out.rel)
				os.Exit(1)
			}
			fmt.Printf("genskill: %s is up to date\n", out.rel)
			continue
		}

		if err := os.MkdirAll(filepath.Dir(out.rel), 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "genskill: mkdir: %v\n", err)
			os.Exit(1)
		}
		if err := os.WriteFile(out.rel, []byte(want), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "genskill: write %s: %v\n", out.rel, err)
			os.Exit(1)
		}
		fmt.Printf("genskill: wrote %s\n", out.rel)
	}
}
