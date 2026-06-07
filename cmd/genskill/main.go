// Command genskill renders the canonical no-mistakes SKILL.md from the
// internal/skill package into skills/no-mistakes/SKILL.md.
//
// Usage:
//
//	go run ./cmd/genskill           # (re)write the skill file
//	go run ./cmd/genskill --check   # fail if the committed file is stale
//
// The --check form is meant for CI so the committed skill never drifts from
// the generator, which is the single source of truth.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kunchenguid/no-mistakes/internal/skill"
)

// outRel is the committed skill path, relative to the repo root, that both
// `npx skills add` discovers and `init` embeds.
var outRel = filepath.Join("skills", skill.Name, "SKILL.md")

func main() {
	check := flag.Bool("check", false, "verify the committed skill matches the generator instead of writing it")
	flag.Parse()

	want := skill.Markdown()

	if *check {
		got, err := os.ReadFile(outRel)
		if err != nil {
			fmt.Fprintf(os.Stderr, "genskill --check: read %s: %v\n", outRel, err)
			os.Exit(1)
		}
		if string(got) != want {
			fmt.Fprintf(os.Stderr, "genskill --check: %s is stale; run `go run ./cmd/genskill` and commit the result\n", outRel)
			os.Exit(1)
		}
		fmt.Printf("genskill: %s is up to date\n", outRel)
		return
	}

	if err := os.MkdirAll(filepath.Dir(outRel), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "genskill: mkdir: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(outRel, []byte(want), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "genskill: write %s: %v\n", outRel, err)
		os.Exit(1)
	}
	fmt.Printf("genskill: wrote %s\n", outRel)
}
