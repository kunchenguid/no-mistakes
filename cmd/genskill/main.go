// Command genskill renders the canonical no-mistakes skill files from the
// internal/skill package into skills/no-mistakes/ (SKILL.md plus the reference
// files it discloses). The same set is what `no-mistakes init` installs into
// the user-level agent skill directories, so the committed files and the
// installed copies never drift.
//
// Usage:
//
//	go run ./cmd/genskill           # (re)write the skill files
//	go run ./cmd/genskill --check   # fail if any committed file is stale
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

func main() {
	check := flag.Bool("check", false, "verify the committed skill matches the generator instead of writing it")
	flag.Parse()

	// The canonical public skill that `npx skills add` discovers, relative to
	// the repo root.
	dir := filepath.Join("skills", skill.Name)

	if *check {
		if err := checkDir(dir); err != nil {
			fmt.Fprintf(os.Stderr, "genskill --check: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("genskill: %s is up to date\n", dir)
		return
	}

	if err := writeDir(dir); err != nil {
		fmt.Fprintf(os.Stderr, "genskill: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("genskill: wrote %s\n", dir)
}

// writeDir renders every skill file into dir, creating it if needed.
func writeDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	for _, f := range skill.Files() {
		path := filepath.Join(dir, f.Name)
		if err := os.WriteFile(path, []byte(f.Content), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
	}
	return nil
}

// checkDir reports the first committed skill file that is missing or stale.
func checkDir(dir string) error {
	for _, f := range skill.Files() {
		path := filepath.Join(dir, f.Name)
		got, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w; run `go run ./cmd/genskill` and commit the result", path, err)
		}
		if string(got) != f.Content {
			return fmt.Errorf("%s is stale; run `go run ./cmd/genskill` and commit the result", path)
		}
	}
	return nil
}
