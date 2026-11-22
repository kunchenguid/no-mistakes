package gitlab

import (
	"context"
	"os/exec"
	"reflect"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestPRAppendixVerifyGitLabDefaultReadArgv(t *testing.T) {
	t.Parallel()
	for _, hostName := range []string{"gitlab.example.com", "ssh-alias", "gitlab.example.com:2222", ""} {
		t.Run(hostName, func(t *testing.T) {
			t.Parallel()
			want := []string{"mr", "view", "123", "--output", "json"}
			var got []string
			host := New(func(ctx context.Context, name string, args ...string) *exec.Cmd {
				if name != "glab" {
					t.Fatalf("command = %q, want glab", name)
				}
				got = append([]string(nil), args...)
				return gitlabTestHelperCmd(ctx, name, gitlabTestResponse{
					stdout: `{"iid":123,"title":"Title","description":"Body"}`,
				})
			}, nil, hostName, "group/project")
			if _, err := host.GetPRContent(context.Background(), &scm.PR{Number: "123"}); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("default GitLab read argv = %q, want main's %q", got, want)
			}
		})
	}
}

func TestPRAppendixVerifyGitLabExplicitReadArgv(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		host string
		repo string
	}{
		{"gitlab.example.com", "https://gitlab.example.com/group/project"},
		{"", "group/project"},
	} {
		t.Run(tc.repo, func(t *testing.T) {
			t.Parallel()
			want := []string{"mr", "view", "123", "--output", "json", "--repo", tc.repo}
			var got []string
			host := New(func(ctx context.Context, name string, args ...string) *exec.Cmd {
				if name != "glab" {
					t.Fatalf("command = %q, want glab", name)
				}
				got = append([]string(nil), args...)
				return gitlabTestHelperCmd(ctx, name, gitlabTestResponse{
					stdout: `{"iid":123,"title":"Title","description":"Body","web_url":"https://gitlab.example.com/group/project/-/merge_requests/123"}`,
				})
			}, nil, tc.host, "group/project")
			content, err := host.GetPRRepositoryContent(context.Background(), "123")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("verifier GitLab read argv = %q, want %q", got, want)
			}
			if content.Repository.Host != "gitlab.example.com" || content.Repository.Path != "group/project" {
				t.Fatalf("repository identity = %+v", content.Repository)
			}
		})
	}
}
