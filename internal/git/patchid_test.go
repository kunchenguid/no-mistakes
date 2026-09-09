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
	writeFile(t, filepath.Join(dir, "star*.txt"), "wildcard file\n")
	writeFile(t, filepath.Join(dir, "starfish.txt"), "sibling file\n")
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "-m", "add both files")
	head := run(t, dir, "git", "rev-parse", "HEAD")

	wildcard, err := StablePatchID(ctx, dir, base, head, "star*.txt")
	if err != nil {
		t.Fatalf("StablePatchID for the wildcard-named file: %v", err)
	}
	sibling, err := StablePatchID(ctx, dir, base, head, "starfish.txt")
	if err != nil {
		t.Fatalf("StablePatchID for the sibling file: %v", err)
	}
	if wildcard == "" || sibling == "" {
		t.Fatalf("empty patch identity: %q and %q", wildcard, sibling)
	}
	if wildcard == sibling {
		t.Fatal("the sibling file's diff was folded into the wildcard-named file's identity")
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
