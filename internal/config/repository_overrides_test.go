package config

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestLoadGlobal_AcceptsPerRepositoryCommitAndTitleOverrides(t *testing.T) {
	t.Parallel()

	cfg, err := LoadGlobalFromBytes([]byte(`repository_overrides:
  https://github.com/acme/widget.git:
    commit:
      branch_pattern: '([A-Z]+-[0-9]+)'
      branch_replacement: '${1}'
      fix_message: '{{.Branch}}: {{.Summary}}'
    pr:
      title_format: '{{.Branch}}: {{.Title}}'
`))
	if err != nil {
		t.Fatalf("LoadGlobalFromBytes() rejected per-repository machine-local overrides: %v", err)
	}
	override, ok := cfg.RepositoryOverrides["github.com/acme/widget"]
	if !ok {
		t.Fatalf("RepositoryOverrides keys = %v, want normalized remote key", cfg.RepositoryOverrides)
	}
	if override.Commit.BranchPattern == nil || *override.Commit.BranchPattern != `([A-Z]+-[0-9]+)` {
		t.Fatalf("commit.branch_pattern = %v, want configured pattern", override.Commit.BranchPattern)
	}
	if override.Commit.BranchReplacement == nil || *override.Commit.BranchReplacement != "${1}" {
		t.Fatalf("commit.branch_replacement = %v, want configured replacement", override.Commit.BranchReplacement)
	}
	if override.Commit.FixMessage == nil || *override.Commit.FixMessage != "{{.Branch}}: {{.Summary}}" {
		t.Fatalf("commit.fix_message = %v, want configured template", override.Commit.FixMessage)
	}
	if override.PR.TitleFormat == nil || *override.PR.TitleFormat != "{{.Branch}}: {{.Title}}" {
		t.Fatalf("pr.title_format = %v, want configured template", override.PR.TitleFormat)
	}
}

func TestNormalizeRepositoryRemote(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		remote  string
		want    string
		wantErr bool
	}{
		{name: "HTTPS with user, case, and git suffix", remote: "https://token@GitHub.COM/Acme/Widget.git", want: "github.com/acme/widget"},
		{name: "uppercase git suffix", remote: "https://github.com/acme/widget.GIT", want: "github.com/acme/widget"},
		{name: "SSH URL", remote: "ssh://git@github.com/Acme/Widget.git", want: "github.com/acme/widget"},
		{name: "scp-like SSH", remote: "git@GITHUB.com:Acme/Widget.git", want: "github.com/acme/widget"},
		{name: "without suffix", remote: "https://github.com/acme/widget", want: "github.com/acme/widget"},
		{name: "nested repository path", remote: "https://github.com/acme/team/widget.git", wantErr: true},
		{name: "encoded path separator", remote: "https://github.com/acme/widget%2Fother.git", wantErr: true},
		{name: "whitespace in repository path", remote: "https://github.com/acme/my widget.git", wantErr: true},
		{name: "unsupported scheme", remote: "file:///tmp/widget.git", wantErr: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := normalizeRepositoryRemote(tt.remote)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("normalizeRepositoryRemote(%q) = %q, want error", tt.remote, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeRepositoryRemote(%q): %v", tt.remote, err)
			}
			if got != tt.want {
				t.Fatalf("normalizeRepositoryRemote(%q) = %q, want %q", tt.remote, got, tt.want)
			}
		})
	}
}

func TestLoadGlobal_RejectsInvalidRepositoryOverrides(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"invalid remote": `repository_overrides:
  file:///tmp/widget.git:
    commit:
      fix_message: '{{.Summary}}'
`,
		"duplicate normalized remotes": `repository_overrides:
  https://github.com/acme/widget.git:
    commit:
      fix_message: '{{.Summary}}'
  git@GITHUB.com:Acme/Widget:
    commit:
      fix_message: '{{.Summary}}'
`,
		"invalid branch pattern": `repository_overrides:
  https://github.com/acme/widget:
    commit:
      branch_pattern: '['
`,
		"replacement without pattern": `repository_overrides:
  https://github.com/acme/widget:
    commit:
      branch_replacement: 'PROJ-${1}'
`,
		"invalid fix template": `repository_overrides:
  https://github.com/acme/widget:
    commit:
      fix_message: '{{.Unknown}}'
`,
		"invalid title template": `repository_overrides:
  https://github.com/acme/widget:
    pr:
      title_format: '{{.Unknown}}'
`,
		"unknown nested field": `repository_overrides:
  https://github.com/acme/widget:
    pr:
      title: wrong
`,
	}
	for name, data := range tests {
		name, data := name, data
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := LoadGlobalFromBytes([]byte(data)); err == nil {
				t.Fatal("LoadGlobalFromBytes() accepted invalid repository override")
			}
		})
	}
}

func TestMergeForRemote_RepositoryAndRemoteMatchPrecedence(t *testing.T) {
	t.Parallel()

	global, err := LoadGlobalFromBytes([]byte(`commit:
  branch_pattern: '^BUG/([0-9]+)$'
  branch_replacement: 'BUG-${1}'
  fix_message: 'global {{.Branch}}: {{.Summary}}'
repository_overrides:
  https://github.com/acme/widget.git:
    commit:
      branch_pattern: '^PROJ/([0-9]+)$'
      branch_replacement: 'PROJ-${1}'
      fix_message: 'machine {{.Branch}}: {{.Summary}}'
    pr:
      title_format: 'machine {{.Branch}}: {{.Title}}'
`))
	if err != nil {
		t.Fatal(err)
	}
	matching := MergeForRemote(global, &RepoConfig{}, "ssh://git@github.com/acme/widget.git")
	got, err := matching.Commit.RenderFixMessageForBranch(types.StepReview, "summary", "PROJ/123")
	if err != nil {
		t.Fatal(err)
	}
	if want := "machine PROJ-123: summary"; got != want {
		t.Fatalf("matching commit subject = %q, want %q", got, want)
	}
	got, err = matching.PR.RenderTitle(mustBranchValue(t, matching.Commit, "PROJ/123"), "title")
	if err != nil {
		t.Fatal(err)
	}
	if want := "machine PROJ-123: title"; got != want {
		t.Fatalf("matching PR title = %q, want %q", got, want)
	}

	repo, err := LoadRepoFromBytes([]byte(`commit:
  branch_pattern: '^ISSUE/([0-9]+)$'
  fix_message: 'repo {{.Summary}}'
pr:
  title_format: 'repo {{.Title}}'
`))
	if err != nil {
		t.Fatal(err)
	}
	repoWins := MergeForRemote(global, repo, "https://github.com/acme/widget")
	got, err = repoWins.Commit.RenderFixMessageForBranch(types.StepReview, "summary", "ISSUE/123")
	if err != nil {
		t.Fatal(err)
	}
	if want := "repo summary"; got != want {
		t.Fatalf("repository commit subject = %q, want %q", got, want)
	}
	got, err = repoWins.PR.RenderTitle(mustBranchValue(t, repoWins.Commit, "ISSUE/123"), "title")
	if err != nil {
		t.Fatal(err)
	}
	if want := "repo title"; got != want {
		t.Fatalf("repository PR title = %q, want %q", got, want)
	}

	unmatched := MergeForRemote(global, &RepoConfig{}, "https://github.com/acme/other")
	got, err = unmatched.Commit.RenderFixMessageForBranch(types.StepReview, "summary", "BUG/123")
	if err != nil {
		t.Fatal(err)
	}
	if want := "global BUG-123: summary"; got != want {
		t.Fatalf("non-matching commit subject = %q, want global default %q", got, want)
	}
}

func mustBranchValue(t *testing.T, commit Commit, branch string) string {
	t.Helper()
	value, err := commit.BranchValue(branch)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestMergeForRemote_RenderFixCommitMatchingAndDefault(t *testing.T) {
	t.Parallel()

	global, err := LoadGlobalFromBytes([]byte(`repository_overrides:
  https://github.com/acme/widget.GIT:
    commit:
      branch_pattern: '([A-Z]+-[0-9]+)'
      fix_message: '{{.Branch}}: {{.Summary}}'
`))
	if err != nil {
		t.Fatal(err)
	}

	for _, remote := range []string{
		"https://github.com/acme/widget",
		"https://github.com/acme/widget.git",
		"https://github.com/acme/widget.GIT",
		"https://GitHub.com/ACME/Widget.git",
		"ssh://git@github.com/Acme/Widget.git",
		"git@github.com:acme/widget.git",
	} {
		matching := MergeForRemote(global, &RepoConfig{}, remote)
		got, err := matching.Commit.RenderFixMessageForBranch(types.StepReview, "summary", "feature/PROJ-123")
		if err != nil {
			t.Fatalf("remote %q: %v", remote, err)
		}
		if want := "PROJ-123: summary"; got != want {
			t.Fatalf("remote %q fix subject = %q, want %q", remote, got, want)
		}
	}

	nonMatching := MergeForRemote(global, &RepoConfig{}, "git@github.com:acme/other.git")
	got, err := nonMatching.Commit.RenderFixMessageForBranch(types.StepReview, "summary", "feature/PROJ-123")
	if err != nil {
		t.Fatal(err)
	}
	if want := "no-mistakes(review): summary"; got != want {
		t.Fatalf("non-matching fix subject = %q, want built-in default %q", got, want)
	}
}
