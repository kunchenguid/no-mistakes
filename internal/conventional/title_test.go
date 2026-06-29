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

func TestExtractTicket(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		branch  string
		pattern string
		want    string
	}{
		{"web ticket", "WEB-12345-refresh-readme", `WEB-\d+`, "WEB-12345"},
		{"refs prefix", "refs/heads/WEB-7", `WEB-\d+`, "WEB-7"},
		{"no match falls through", "docs/readme-refresh", `WEB-\d+`, ""},
		{"empty pattern disables", "WEB-1", "", ""},
		{"invalid pattern is safe", "WEB-1", `WEB-(`, ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ExtractTicket(tc.branch, tc.pattern); got != tc.want {
				t.Fatalf("ExtractTicket(%q, %q) = %q, want %q", tc.branch, tc.pattern, got, tc.want)
			}
		})
	}
}

func TestApplyTicketPrefix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		title  string
		ticket string
		want   string
	}{
		{"strips conventional type", "docs: refresh README setup", "WEB-12345", "WEB-12345: refresh README setup"},
		{"strips type with scope", "feat(cli): add wizard", "WEB-9", "WEB-9: add wizard"},
		{"plain title gets prefix", "refresh README setup", "WEB-1", "WEB-1: refresh README setup"},
		{"already prefixed is unchanged", "WEB-1: refresh README", "WEB-1", "WEB-1: refresh README"},
		{"empty ticket leaves title", "docs: refresh README", "", "docs: refresh README"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ApplyTicketPrefix(tc.title, tc.ticket); got != tc.want {
				t.Fatalf("ApplyTicketPrefix(%q, %q) = %q, want %q", tc.title, tc.ticket, got, tc.want)
			}
		})
	}
}
