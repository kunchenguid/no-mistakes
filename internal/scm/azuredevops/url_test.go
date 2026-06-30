package azuredevops

import "testing"

func TestParseRemote(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		in          string
		wantOrgURL  string
		wantProject string
		wantRepo    string
		wantOK      bool
	}{
		{
			name:        "https dev.azure.com",
			in:          "https://dev.azure.com/myorg/myproject/_git/myrepo",
			wantOrgURL:  "https://dev.azure.com/myorg",
			wantProject: "myproject",
			wantRepo:    "myrepo",
			wantOK:      true,
		},
		{
			name:        "https with .git suffix",
			in:          "https://dev.azure.com/myorg/myproject/_git/myrepo.git",
			wantOrgURL:  "https://dev.azure.com/myorg",
			wantProject: "myproject",
			wantRepo:    "myrepo",
			wantOK:      true,
		},
		{
			name:        "https with org userinfo prefix",
			in:          "https://myorg@dev.azure.com/myorg/myproject/_git/myrepo",
			wantOrgURL:  "https://dev.azure.com/myorg",
			wantProject: "myproject",
			wantRepo:    "myrepo",
			wantOK:      true,
		},
		{
			name:        "ssh scp form",
			in:          "git@ssh.dev.azure.com:v3/myorg/myproject/myrepo",
			wantOrgURL:  "https://dev.azure.com/myorg",
			wantProject: "myproject",
			wantRepo:    "myrepo",
			wantOK:      true,
		},
		{
			name:        "pr url suffix is ignored",
			in:          "https://dev.azure.com/myorg/myproject/_git/myrepo/pullrequest/42",
			wantOrgURL:  "https://dev.azure.com/myorg",
			wantProject: "myproject",
			wantRepo:    "myrepo",
			wantOK:      true,
		},
		{
			name:        "project name with spaces is decoded",
			in:          "https://dev.azure.com/myorg/My%20Project/_git/myrepo",
			wantOrgURL:  "https://dev.azure.com/myorg",
			wantProject: "My Project",
			wantRepo:    "myrepo",
			wantOK:      true,
		},
		{
			name:        "legacy visualstudio.com",
			in:          "https://myorg.visualstudio.com/myproject/_git/myrepo",
			wantOrgURL:  "https://myorg.visualstudio.com",
			wantProject: "myproject",
			wantRepo:    "myrepo",
			wantOK:      true,
		},
		{
			name:        "legacy visualstudio.com with DefaultCollection",
			in:          "https://myorg.visualstudio.com/DefaultCollection/myproject/_git/myrepo",
			wantOrgURL:  "https://myorg.visualstudio.com",
			wantProject: "myproject",
			wantRepo:    "myrepo",
			wantOK:      true,
		},
		{
			name:        "legacy vs-ssh visualstudio.com",
			in:          "git@vs-ssh.visualstudio.com:v3/myorg/myproject/myrepo",
			wantOrgURL:  "https://myorg.visualstudio.com",
			wantProject: "myproject",
			wantRepo:    "myrepo",
			wantOK:      true,
		},
		{"empty", "", "", "", "", false},
		{"github https is rejected", "https://github.com/owner/repo", "", "", "", false},
		{"github with extra path is rejected", "https://github.com/owner/repo/tree/main", "", "", "", false},
		{"github ssh is rejected", "git@github.com:owner/repo.git", "", "", "", false},
		{"azure missing repo", "https://dev.azure.com/myorg/myproject/_git", "", "", "", false},
		{"azure ssh too few segments", "git@ssh.dev.azure.com:v3/myorg/myproject", "", "", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			orgURL, project, repo, ok := ParseRemote(tc.in)
			if ok != tc.wantOK {
				t.Fatalf("ParseRemote(%q) ok = %v, want %v", tc.in, ok, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if orgURL != tc.wantOrgURL || project != tc.wantProject || repo != tc.wantRepo {
				t.Fatalf("ParseRemote(%q) = (%q, %q, %q), want (%q, %q, %q)",
					tc.in, orgURL, project, repo, tc.wantOrgURL, tc.wantProject, tc.wantRepo)
			}
		})
	}
}

func TestWebPRURL(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		orgURL     string
		project    string
		repo       string
		repoWebURL string
		id         string
		want       string
	}{
		{
			name:    "constructed from org/project/repo",
			orgURL:  "https://dev.azure.com/myorg",
			project: "myproject",
			repo:    "myrepo",
			id:      "42",
			want:    "https://dev.azure.com/myorg/myproject/_git/myrepo/pullrequest/42",
		},
		{
			name:       "prefers repository web url",
			orgURL:     "https://dev.azure.com/myorg",
			project:    "ignored",
			repo:       "ignored",
			repoWebURL: "https://dev.azure.com/myorg/myproject/_git/myrepo",
			id:         "7",
			want:       "https://dev.azure.com/myorg/myproject/_git/myrepo/pullrequest/7",
		},
		{
			name:    "encodes spaces in project",
			orgURL:  "https://dev.azure.com/myorg",
			project: "My Project",
			repo:    "my repo",
			id:      "1",
			want:    "https://dev.azure.com/myorg/My%20Project/_git/my%20repo/pullrequest/1",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := webPRURL(tc.orgURL, tc.project, tc.repo, tc.repoWebURL, tc.id)
			if got != tc.want {
				t.Fatalf("webPRURL() = %q, want %q", got, tc.want)
			}
		})
	}
}
