package git

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"
)

func TestStablePatchIDIsPerFileAndTreatsGlobCharactersLiterally(t *testing.T) {
	for _, tc := range []struct {
		name      string
		literal   string
		sibling   string
		posixOnly bool
	}{
		{name: "bracket_literal", literal: "star[ab].txt", sibling: "stara.txt"},
		{name: "asterisk_literal", literal: "star*.txt", sibling: "starfish.txt", posixOnly: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.posixOnly && runtime.GOOS == "windows" {
				t.Skip("Windows filenames cannot contain an asterisk")
			}
			dir := initTestRepo(t)
			ctx := context.Background()

			base := run(t, dir, "git", "rev-parse", "HEAD")
			writeFile(t, filepath.Join(dir, tc.literal), "wildcard file\n")
			writeFile(t, filepath.Join(dir, tc.sibling), "sibling file\n")
			run(t, dir, "git", "add", "-A")
			run(t, dir, "git", "commit", "-m", "add both files")
			head := run(t, dir, "git", "rev-parse", "HEAD")

			wildcard, err := StablePatchID(ctx, dir, base, head, tc.literal)
			if err != nil {
				t.Fatalf("StablePatchID for the wildcard-named file: %v", err)
			}
			sibling, err := StablePatchID(ctx, dir, base, head, tc.sibling)
			if err != nil {
				t.Fatalf("StablePatchID for the sibling file: %v", err)
			}
			if wildcard == "" || sibling == "" {
				t.Fatalf("empty patch identity: %q and %q", wildcard, sibling)
			}
			if wildcard == sibling {
				t.Fatal("the sibling file's diff was folded into the wildcard-named file's identity")
			}

			writeFile(t, filepath.Join(dir, tc.sibling), "changed sibling\n")
			run(t, dir, "git", "add", "-A")
			run(t, dir, "git", "commit", "-m", "change sibling only")
			later := run(t, dir, "git", "rev-parse", "HEAD")
			unchanged, err := StablePatchID(ctx, dir, base, later, tc.literal)
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
		})
	}
}
