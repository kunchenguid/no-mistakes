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
		{name: "SSH URL on a hosted forge", remote: "ssh://git@github.com/Acme/Widget.git", want: "github.com/acme/widget"},
		{name: "absolute SSH URL path", remote: "ssh://git@host/srv/git/team/widget.git", want: "host//srv/git/team/widget"},
		{name: "absolute IPv6 scp path", remote: "git@[2001:db8::1]:/srv/git/team/widget.git", want: "2001:db8::1//srv/git/team/widget"},
		{name: "relative IPv6 scp path", remote: "git@[2001:db8::1]:srv/git/team/widget.git", want: "2001:db8::1/srv/git/team/widget"},
		{name: "malformed IPv6 scp authority", remote: "git@[2001:db8::1:/srv/git/team/widget.git", wantErr: true},
		{name: "Git protocol default port", remote: "git://host:9418/team/repo.git", want: "host/team/repo"},
		{name: "Git protocol nondefault port", remote: "git://host:9419/team/repo.git", want: "host:9419/team/repo"},
		{name: "scp-like SSH", remote: "git@GITHUB.com:Acme/Widget.git", want: "github.com/acme/widget"},
		{name: "without suffix", remote: "https://github.com/acme/widget", want: "github.com/acme/widget"},
		{name: "nested GitLab namespace", remote: "https://gitlab.example.com/group/sub/project.git", want: "gitlab.example.com/group/sub/project"},
		{name: "nested GitLab path with trailing slash", remote: "https://gitlab.example.com/group/sub/project.git/", want: "gitlab.example.com/group/sub/project"},
		{name: "nested GitLab scp remote", remote: "git@gitlab.example.com:group/sub/project.git", want: "gitlab.example.com/group/sub/project"},
		{name: "empty nested path segment", remote: "https://gitlab.example.com/group//sub/project.git", wantErr: true},
		{name: "Azure DevOps HTTPS", remote: "https://dev.azure.com/Acme/Platform/_git/Widget", want: "dev.azure.com/acme/platform/widget"},
		{name: "Azure DevOps SSH", remote: "git@ssh.dev.azure.com:v3/Acme/Platform/Widget", want: "dev.azure.com/acme/platform/widget"},
		{name: "Azure DevOps legacy host", remote: "https://Acme.visualstudio.com/Platform/_git/Widget", want: "dev.azure.com/acme/platform/widget"},
		{name: "encoded path separator", remote: "https://github.com/acme/widget%2Fother.git", wantErr: true},
		{name: "whitespace in repository path", remote: "https://github.com/acme/my widget.git", wantErr: true},
		{name: "single-segment repository path", remote: "https://github.com/widget.git", wantErr: true},
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

func TestMergeForRemote_SCPAbsoluteAndRelativePathsStayDistinct(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		configKey string
		matching  string
		distinct  string
	}{
		{
			name:      "absolute scp config key matches absolute SSH URL",
			configKey: "git@host:/srv/git/team/widget.git",
			matching:  "ssh://git@host/srv/git/team/widget",
			distinct:  "git@host:srv/git/team/widget.git",
		},
		{
			name:      "absolute SSH URL config key matches absolute scp",
			configKey: "ssh://git@host/srv/git/team/widget.git",
			matching:  "git@host:/srv/git/team/widget",
			distinct:  "git@host:srv/git/team/widget.git",
		},
		{
			name:      "relative scp config key rejects absolute SSH URL",
			configKey: "git@host:srv/git/team/widget.git",
			matching:  "git@host:srv/git/team/widget",
			distinct:  "ssh://git@host/srv/git/team/widget.git",
		},
		{
			name:      "absolute IPv6 scp config key matches absolute SSH URL",
			configKey: "git@[2001:db8::1]:/srv/git/team/widget.git",
			matching:  "ssh://git@[2001:db8::1]/srv/git/team/widget",
			distinct:  "git@[2001:db8::1]:srv/git/team/widget.git",
		},
		{
			name:      "absolute IPv6 SSH URL config key matches absolute scp",
			configKey: "ssh://git@[2001:db8::1]/srv/git/team/widget.git",
			matching:  "git@[2001:db8::1]:/srv/git/team/widget",
			distinct:  "git@[2001:db8::1]:srv/git/team/widget.git",
		},
		{
			name:      "relative IPv6 scp config key rejects absolute SSH URL",
			configKey: "git@[2001:db8::1]:srv/git/team/widget.git",
			matching:  "git@[2001:db8::1]:srv/git/team/widget",
			distinct:  "ssh://git@[2001:db8::1]/srv/git/team/widget.git",
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			global, err := LoadGlobalFromBytes([]byte("repository_overrides:\n  '" + tc.configKey + "':\n    commit:\n      fix_message: 'override {{.Summary}}'\n"))
			if err != nil {
				t.Fatalf("LoadGlobalFromBytes(): %v", err)
			}
			for _, candidate := range []struct {
				remote string
				want   string
			}{
				{remote: tc.matching, want: "override summary"},
				{remote: tc.distinct, want: "no-mistakes(review): summary"},
			} {
				merged := MergeForRemote(global, &RepoConfig{}, candidate.remote)
				got, err := merged.Commit.RenderFixMessageForBranch(types.StepReview, "summary", "feature")
				if err != nil {
					t.Fatalf("remote %q: %v", candidate.remote, err)
				}
				if got != candidate.want {
					t.Errorf("remote %q fix subject = %q, want %q", candidate.remote, got, candidate.want)
				}
			}
		})
	}
}

func TestMergeForRemote_GitDefaultPortMatchesOmittedPort(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		configKey string
		matching  string
	}{
		{
			name:      "configured default port",
			configKey: "git://host:9418/team/repo.git",
			matching:  "git://host/team/repo.git",
		},
		{
			name:      "registered default port",
			configKey: "git://host/team/repo.git",
			matching:  "git://host:9418/team/repo",
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			global, err := LoadGlobalFromBytes([]byte("repository_overrides:\n  '" + tc.configKey + "':\n    commit:\n      fix_message: 'override {{.Summary}}'\n"))
			if err != nil {
				t.Fatalf("LoadGlobalFromBytes(): %v", err)
			}
			merged := MergeForRemote(global, &RepoConfig{}, tc.matching)
			got, err := merged.Commit.RenderFixMessageForBranch(types.StepReview, "summary", "feature")
			if err != nil {
				t.Fatalf("remote %q: %v", tc.matching, err)
			}
			if got != "override summary" {
				t.Fatalf("remote %q fix subject = %q, want override summary", tc.matching, got)
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

func TestMergeForRemote_MatchesNestedProviderRemotePaths(t *testing.T) {
	t.Parallel()

	global, err := LoadGlobalFromBytes([]byte(`repository_overrides:
  https://gitlab.example.com/group/sub/project.git:
    commit:
      fix_message: 'gitlab {{.Branch}}: {{.Summary}}'
  https://dev.azure.com/Acme/Platform/_git/Widget:
    commit:
      fix_message: 'azure {{.Branch}}: {{.Summary}}'
`))
	if err != nil {
		t.Fatalf("LoadGlobalFromBytes() rejected nested provider remotes: %v", err)
	}

	for _, tc := range []struct {
		name   string
		remote string
		want   string
	}{
		{name: "GitLab subgroup SSH remote", remote: "git@gitlab.example.com:group/sub/project.git", want: "gitlab feature/PROJ-123: summary"},
		{name: "Azure DevOps SSH remote", remote: "git@ssh.dev.azure.com:v3/acme/platform/widget", want: "azure feature/PROJ-123: summary"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			merged := MergeForRemote(global, &RepoConfig{}, tc.remote)
			got, err := merged.Commit.RenderFixMessageForBranch(types.StepReview, "summary", "feature/PROJ-123")
			if err != nil {
				t.Fatalf("RenderFixMessageForBranch(%q): %v", tc.remote, err)
			}
			if got != tc.want {
				t.Fatalf("remote %q rendered fix subject = %q, want %q", tc.remote, got, tc.want)
			}
		})
	}

	unmatched := MergeForRemote(global, &RepoConfig{}, "git@gitlab.example.com:group/other/project.git")
	got, err := unmatched.Commit.RenderFixMessageForBranch(types.StepReview, "summary", "feature/PROJ-123")
	if err != nil {
		t.Fatal(err)
	}
	if want := "no-mistakes(review): summary"; got != want {
		t.Fatalf("distinct GitLab subgroup rendered fix subject = %q, want default %q", got, want)
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
