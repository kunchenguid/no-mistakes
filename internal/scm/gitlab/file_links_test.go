package gitlab

import "testing"

func TestRepositoryFileLinksUsesFullGitLabProjectAndResolvedHost(t *testing.T) {
	t.Parallel()
	host := New(nil, nil, "gitlab.example", "group/sub/project")
	for _, tc := range []struct{ remote, base string }{
		{"https://user:fixture-secret@gitlab.example:8443/group/sub/project.git?private_token=fixture-token", "https://gitlab.example:8443/group/sub/project"},
		{"git@gitlab-alias:group/sub/project.git", "https://gitlab.example/group/sub/project"},
		{"ssh://git@gitlab.example:2222/group/sub/project.git", "https://gitlab.example/group/sub/project"},
	} {
		blob, raw, ok := host.RepositoryFileLinks(tc.remote)
		if !ok || blob != tc.base+"/-/blob/" || raw != tc.base+"/-/raw/" {
			t.Fatalf("GitLab project links = %q, %q, %v; base=%s", blob, raw, ok, tc.base)
		}
	}
}

func TestRepositoryFileLinksRefusesAnotherGitLabProject(t *testing.T) {
	t.Parallel()
	host := New(nil, nil, "gitlab.example", "group/sub/project")
	if blob, raw, ok := host.RepositoryFileLinks("https://gitlab.example/group/other.git"); ok || blob != "" || raw != "" {
		t.Fatalf("wrong-project links = %q, %q, %v", blob, raw, ok)
	}
}
