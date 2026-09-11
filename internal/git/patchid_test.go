package git

import (
	"context"
	"path/filepath"
	"testing"
)

func TestStablePatchIDIsPerFileAndTreatsGlobCharactersLiterally(t *testing.T) {
	dir := initTestRepo(t)
	ctx := context.Background()

	base := run(t, dir, "git", "rev-parse", "HEAD")
	writeFile(t, filepath.Join(dir, "star[fish].txt"), "wildcard file\n")
	writeFile(t, filepath.Join(dir, "starf.txt"), "sibling file\n")
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "-m", "add both files")
	head := run(t, dir, "git", "rev-parse", "HEAD")

	wildcard, err := StablePatchID(ctx, dir, base, head, "star[fish].txt")
	if err != nil {
		t.Fatalf("StablePatchID for the wildcard-named file: %v", err)
	}
	sibling, err := StablePatchID(ctx, dir, base, head, "starf.txt")
	if err != nil {
		t.Fatalf("StablePatchID for the sibling file: %v", err)
	}
	if wildcard == "" || sibling == "" {
		t.Fatalf("empty patch identity: %q and %q", wildcard, sibling)
	}
	if wildcard == sibling {
		t.Fatal("the sibling file's diff was folded into the wildcard-named file's identity")
	}

	// Brackets exercise Git glob matching while remaining valid Windows
	// filenames. Changing only the matching sibling must not change the
	// literal file's patch identity.
	writeFile(t, filepath.Join(dir, "starf.txt"), "changed sibling\n")
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "-m", "change sibling only")
	later := run(t, dir, "git", "rev-parse", "HEAD")
	unchanged, err := StablePatchID(ctx, dir, base, later, "star[fish].txt")
	if err != nil {
		t.Fatalf("StablePatchID after sibling change: %v", err)
	}
	if unchanged != wildcard {
		t.Fatalf("sibling change altered literal file identity: %q, want %q", unchanged, wildcard)
	}

	// A path this commit does not touch has no identity to report.
	untouched, err := StablePatchID(ctx, dir, base, head, "absent.txt")
	if err != nil {
		t.Fatalf("StablePatchID for an untouched path: %v", err)
	}
	if untouched != "" {
		t.Fatalf("untouched path reported identity %q", untouched)
	}
}
