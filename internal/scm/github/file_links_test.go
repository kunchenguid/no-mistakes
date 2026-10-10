package github

import "testing"

func TestRepositoryFileLinksUsesThePushTargetAndPreservesGitHubURLs(t *testing.T) {
	t.Parallel()
	host := NewWithFork(nil, nil, "github.com", "upstream/widgets", "fork/widgets", false)
	for _, remote := range []string{"https://user:fixture-secret@github.com/fork/widgets.git?token=fixture-token", "git@github.com:fork/widgets.git", "git@github-alias:fork/widgets.git"} {
		blob, raw, ok := host.RepositoryFileLinks(remote)
		if !ok || blob != "https://github.com/fork/widgets/blob/" || raw != "https://raw.githubusercontent.com/fork/widgets/" {
			t.Fatalf("push-target links = %q, %q, %v", blob, raw, ok)
		}
	}
}

func TestRepositoryFileLinksDoesNotInventGitHubEnterpriseURLs(t *testing.T) {
	t.Parallel()
	host := New(nil, nil, "ghe.example", "team/widgets")
	if blob, raw, ok := host.RepositoryFileLinks("https://ghe.example/team/widgets.git"); ok || blob != "" || raw != "" {
		t.Fatalf("unsupported enterprise links = %q, %q, %v", blob, raw, ok)
	}
}
