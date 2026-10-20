package scm

import "testing"

func TestRepositoryWebURLPreservesWebIdentityWithoutCredentials(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ remote, host, want string }{
		{"https://user:fixture-secret@gitlab.example:8443/group/sub/project.git?private_token=fixture-token#fragment", "gitlab.example", "https://gitlab.example:8443/group/sub/project"},
		{"git@gitlab-alias:group/sub/project.git", "gitlab.example", "https://gitlab.example/group/sub/project"},
		{"ssh://git@gitlab.example:2222/group/sub/project.git", "gitlab.example", "https://gitlab.example/group/sub/project"},
		{"http://gitlab.example:8080/group/sub/project.git", "gitlab.example", "http://gitlab.example:8080/group/sub/project"},
		{"https://gitlab.example/group/sub/project%231.git", "gitlab.example", "https://gitlab.example/group/sub/project%231"},
	} {
		got, ok := RepositoryWebURL(tc.remote, tc.host)
		if !ok || got != tc.want {
			t.Errorf("RepositoryWebURL() = %q, %v; want %q", got, ok, tc.want)
		}
	}
}

func TestRepositoryWebURLRefusesAmbiguousRepositoryPaths(t *testing.T) {
	t.Parallel()
	for _, remote := range []string{"", "group/project", "/tmp/group/project", "file:///tmp/group/project", "ftp://gitlab.example/group/project", "https://gitlab.example/group/../project.git", "https://gitlab.example/group", `C:\Users\me\project`} {
		if got, ok := RepositoryWebURL(remote, "gitlab.example"); ok || got != "" {
			t.Errorf("RepositoryWebURL(%q) = %q, %v; want no link", remote, got, ok)
		}
	}
}
