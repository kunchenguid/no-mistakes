package conventional

import "testing"

func TestTightenTitleKeepsReleaseTypes(t *testing.T) {
	t.Parallel()

	tests := []string{
		"feat(cli): add onboarding wizard",
		"fix: improve command output",
		"fix(api)!: require auth token",
	}

	for _, tc := range tests {
		t.Run(tc, func(t *testing.T) {
			t.Parallel()
			if got := TightenTitle(tc); got != tc {
				t.Fatalf("TightenTitle(%q) = %q", tc, got)
			}
		})
	}
}

func TestTightenTitleKeepsConventionalNonReleaseTypes(t *testing.T) {
	t.Parallel()

	tests := []string{
		"refactor: improve CLI output",
		"docs: add user-facing export command",
		"chore(cli)!: improve UI behavior",
	}

	for _, tc := range tests {
		t.Run(tc, func(t *testing.T) {
			t.Parallel()
			if got := TightenTitle(tc); got != tc {
				t.Fatalf("TightenTitle(%q) = %q", tc, got)
			}
		})
	}
}

func TestTightenTitleKeepsNonProductImpactTypes(t *testing.T) {
	t.Parallel()

	tests := []string{
		"docs: update README",
		"docs: update CLI command documentation",
		"refactor: simplify internal retry loop",
		"test: cover config parsing",
	}

	for _, tc := range tests {
		t.Run(tc, func(t *testing.T) {
			t.Parallel()
			if got := TightenTitle(tc); got != tc {
				t.Fatalf("TightenTitle(%q) = %q", tc, got)
			}
		})
	}
}

func TestTightenTitlePrefixesNonConventionalTitles(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		title string
		want  string
	}{
		{name: "new feature", title: "add export command", want: "feat: add export command"},
		{name: "direct fix verb", title: "fix login redirect", want: "fix: fix login redirect"},
		{name: "direct correction verb", title: "correct cache invalidation", want: "fix: correct cache invalidation"},
		{name: "user-facing fix", title: "Improve pipeline header UX", want: "fix: Improve pipeline header UX"},
		{name: "documentation", title: "update README", want: "docs: update README"},
		{name: "generic internal", title: "tidy retry helper", want: "chore: tidy retry helper"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := TightenTitle(tc.title); got != tc.want {
				t.Fatalf("TightenTitle(%q) = %q, want %q", tc.title, got, tc.want)
			}
		})
	}
}

func TestIsTitle(t *testing.T) {
	t.Parallel()

	tests := []struct {
		title string
		want  bool
	}{
		{title: "feat: add export", want: true},
		{title: "fix(cli)!: change output", want: true},
		{title: "add export", want: false},
		{title: "Feat: add export", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.title, func(t *testing.T) {
			t.Parallel()
			if got := IsTitle(tc.title); got != tc.want {
				t.Fatalf("IsTitle(%q) = %v, want %v", tc.title, got, tc.want)
			}
		})
	}
}
