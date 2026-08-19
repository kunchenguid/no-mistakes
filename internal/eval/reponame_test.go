package eval

import (
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
)

// A case identifies its repository only by the fingerprint of the redacted
// upstream URL, so a dashboard resolves names by fingerprinting the locally
// registered repositories the same way capture did.
func TestRepoDisplayNamesKeyResolvedNamesByCaptureFingerprint(t *testing.T) {
	repos := []*db.Repo{
		{ID: "a", WorkingPath: filepath.Join("/tmp", "clone-a"), UpstreamURL: "https://github.com/kunchenguid/no-mistakes.git"},
		{ID: "b", WorkingPath: filepath.Join("/tmp", "clone-b"), UpstreamURL: "git@example.test:org/other.git"},
		{ID: "c", WorkingPath: filepath.Join("/tmp", "clone-c"), UpstreamURL: "https://example.test/single-segment"},
		{ID: "d"},
		nil,
	}
	names := RepoDisplayNames(repos)

	cases := map[string]string{
		fingerprint("https://github.com/kunchenguid/no-mistakes.git"): "kunchenguid/no-mistakes",
		fingerprint("git@example.test:org/other.git"):                 "org/other",
		fingerprint("https://example.test/single-segment"):            "clone-c",
	}
	for print, want := range cases {
		if got := names[print]; got != want {
			t.Fatalf("names[%s] = %q, want %q", print, got, want)
		}
	}
	if got := names[fingerprint("https://example.test/never/registered.git")]; got != "" {
		t.Fatalf("unregistered repository resolved to %q, want no entry", got)
	}
	if got := names[fingerprint("")]; got != "" {
		t.Fatalf("repository without an upstream URL resolved to %q, want no entry", got)
	}
}

// A repository with a credentialled URL is fingerprinted from the redacted
// form, exactly as capture stores it, so it still resolves.
func TestRepoDisplayNamesResolveCredentialledUpstreamURL(t *testing.T) {
	repo := &db.Repo{ID: "a", WorkingPath: "/tmp/clone", UpstreamURL: "https://user:token@github.com/kunchenguid/no-mistakes.git"}
	names := RepoDisplayNames([]*db.Repo{repo})
	if got := names[fingerprint(repo.UpstreamURL)]; got != "kunchenguid/no-mistakes" {
		t.Fatalf("name = %q, want the slug of the credentialled upstream URL", got)
	}
}

func TestRepoDisplayNameFallsBackFromSlugToPathToID(t *testing.T) {
	tests := []struct {
		name string
		repo *db.Repo
		want string
	}{
		{"slug", &db.Repo{ID: "id", WorkingPath: "/tmp/clone", UpstreamURL: "https://github.com/owner/name"}, "owner/name"},
		{"working path", &db.Repo{ID: "id", WorkingPath: filepath.Join("/tmp", "clone")}, "clone"},
		{"id", &db.Repo{ID: "id"}, "id"},
		{"nil", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RepoDisplayName(tt.repo); got != tt.want {
				t.Fatalf("RepoDisplayName = %q, want %q", got, tt.want)
			}
		})
	}
}

// An unresolved fingerprint keeps its short opaque form: a dashboard never
// blanks a repository it cannot name.
func TestStoreRepoDisplayFallsBackToTheShortFingerprint(t *testing.T) {
	store := &Store{}
	unknown := fingerprint("https://example.test/org/repo")
	if got := store.repoDisplay(unknown); got != shortFingerprint(unknown) {
		t.Fatalf("repoDisplay = %q, want the short fingerprint", got)
	}
	store.SetRepoNames(map[string]string{unknown: "org/repo"})
	if got := store.repoDisplay(unknown); got != "org/repo" {
		t.Fatalf("repoDisplay = %q, want the resolved name", got)
	}
	store.SetRepoNames(map[string]string{unknown: "   "})
	if got := store.repoDisplay(unknown); got != shortFingerprint(unknown) {
		t.Fatalf("repoDisplay = %q, want the short fingerprint for a blank name", got)
	}
}
