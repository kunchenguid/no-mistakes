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
	"strings"

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

// writeDir renders every skill file into dir, creating it if needed, and
// removes anything else: dir is generated output, so a file the generator no
// longer produces is a retired or renamed reference that must not survive.
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
	orphans, err := orphanNames(dir)
	if err != nil {
		return err
	}
	for _, name := range orphans {
		path := filepath.Join(dir, name)
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("remove %s: %w", path, err)
		}
	}
	return nil
}

// checkDir reports the first committed skill file that is missing, stale, or
// no longer generated at all.
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
	orphans, err := orphanNames(dir)
	if err != nil {
		return err
	}
	if len(orphans) > 0 {
		return fmt.Errorf("%s contains files the generator does not produce (%s); run `go run ./cmd/genskill` and commit the result",
			dir, strings.Join(orphans, ", "))
	}
	return nil
}

// orphanNames lists the entries of dir that skill.Files() does not produce.
func orphanNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read dir %s: %w", dir, err)
	}
	generated := make(map[string]bool, len(skill.Files()))
	for _, f := range skill.Files() {
		generated[f.Name] = true
	}
	var orphans []string
	for _, e := range entries {
		if !generated[e.Name()] {
			orphans = append(orphans, e.Name())
		}
	}
	return orphans, nil
}
