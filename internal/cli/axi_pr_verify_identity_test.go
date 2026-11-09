package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
)

// The oracle is the requested repository, independent of every adapter's
// interpretation of a numbered PR. A foreign or absent identity must never
// reach the command's verdict output, even when title and body are valid.
func TestPRAppendixVerifyRepositoryIdentity(t *testing.T) {
	_, source, _, _ := runtime.Caller(0)
	module := filepath.Clean(filepath.Join(filepath.Dir(source), "../.."))
	bin := t.TempDir()
	helper := filepath.Join(bin, "helper")
	if runtime.GOOS == "windows" {
		helper += ".exe"
	}
	build := exec.Command("go", "build", "-o", helper, "./internal/pipeline/fakecli")
	build.Dir = module
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build non-race fake CLI: %v: %s", err, out)
	}
	for _, name := range []string{"gh", "glab", "origin", "az", "tea", "forgejo-axi", "appendix-provider"} {
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		if err := os.Link(helper, filepath.Join(bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_CLI_MODE", "pr-appendix-identity")
	t.Chdir(t.TempDir())
	t.Setenv("GH_CONFIG_DIR", t.TempDir())
	t.Setenv("GLAB_CONFIG_DIR", t.TempDir())
	tea := t.TempDir()
	t.Setenv("TEA_CONFIG_DIR", tea)
	if err := os.WriteFile(filepath.Join(tea, "hosts.yml"), []byte("hosts:\n  https://gitea.example:\n    token: synthetic\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NO_MISTAKES_BITBUCKET_EMAIL", "synthetic@example.test")
	t.Setenv("NO_MISTAKES_BITBUCKET_API_TOKEN", "synthetic")
	var raw string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("write request: %s", r.Method)
			w.WriteHeader(405)
			return
		}
		_, _ = fmt.Fprint(w, raw)
	}))
	defer server.Close()
	t.Setenv("NO_MISTAKES_BITBUCKET_API_BASE_URL", server.URL)
	for _, tc := range []struct {
		provider, name, repo, webBase, path string
	}{
		{"github", "https", "https://github.com/team/target.git", "https://github.com", "team/target"},
		{"github", "case", "https://github.com/team/target.git", "https://github.com", "Team/Target"},
		{"github", "scp web port", "git@enterprise.github.com:team/target.git", "https://enterprise.github.com:3000", "team/target"},
		{"github", "ssh web port", "ssh://git@enterprise.github.com:2222/team/target.git", "https://enterprise.github.com:3000", "team/target"},
		{"github", "https web port", "https://enterprise.github.com:3000/team/target.git", "https://enterprise.github.com:3000", "team/target"},
		{"gitlab", "https", "https://gitlab.com/team/target.git", "https://gitlab.com", "team/target"},
		{"gitlab", "case", "https://gitlab.com/team/target.git", "https://gitlab.com", "Team/Target"},
		{"gitlab", "scp web port", "git@enterprise.gitlab.com:team/target.git", "https://enterprise.gitlab.com:3000", "team/target"},
		{"gitlab", "ssh web port", "ssh://git@enterprise.gitlab.com:2222/team/target.git", "https://enterprise.gitlab.com:3000", "team/target"},
		{"gitlab", "https web port", "https://enterprise.gitlab.com:3000/team/target.git", "https://enterprise.gitlab.com:3000", "team/target"},
		{"origin", "https", "https://origin.cursor.com/team/target.git", "https://cursor.com/codebase", "team/target"},
		{"forgejo", "https", "https://forgejo.example/team/target.git", "https://forgejo.example", "team/target"},
		{"forgejo", "scp web port", "git@forgejo.example:team/target.git", "https://forgejo.example:3000", "team/target"},
		{"forgejo", "https web port", "https://forgejo.example:3000/team/target.git", "https://forgejo.example:3000", "team/target"},
		{"gitea", "https", "https://gitea.example/team/target.git", "https://gitea.example", "team/target"},
		{"gitea", "scp web port", "git@gitea.example:team/target.git", "https://gitea.example:3000", "team/target"},
		{"gitea", "ssh web port", "ssh://git@gitea.example:2222/team/target.git", "https://gitea.example:3000", "team/target"},
		{"gitea", "https web port", "https://gitea.example:3000/team/target.git", "https://gitea.example:3000", "team/target"},
		{"azure", "https", "https://dev.azure.com/org/proj/_git/target", "https://dev.azure.com/org", "proj/target"},
		{"azure", "case", "https://dev.azure.com/org/PROJ/_git/target", "https://dev.azure.com/ORG", "proj/TARGET"},
		{"azure", "legacy https", "https://myorg.visualstudio.com/proj/_git/target", "https://myorg.visualstudio.com", "proj/target"},
		{"azure", "legacy scp", "git@vs-ssh.visualstudio.com:v3/myorg/proj/target", "https://myorg.visualstudio.com", "proj/target"},
		{"azure", "scp", "git@ssh.dev.azure.com:v3/org/proj/target", "https://dev.azure.com/org", "proj/target"},
		{"bitbucket", "https", "https://bitbucket.org/team/target.git", "https://bitbucket.org", "team/target"},
		{"plugin", "https", "https://plugin.example/team/target.git", "https://plugin.example", "team/target"},
		{"plugin", "https port", "https://plugin.example:8443/team/target.git", "https://plugin.example:8443", "team/target"},
		{"plugin", "web alias", "https://plugin.example:8443/team/target.git", "https://web.example:3000", "Team/Target"},
	} {
		kinds := []string{"match", "wrong repository", "absent"}
		if tc.provider == "azure" {
			kinds = append(kinds, "wrong project")
		}
		if tc.provider == "github" || tc.provider == "gitlab" || tc.provider == "gitea" || tc.provider == "forgejo" {
			kinds = append(kinds, "wrong host")
			if strings.HasPrefix(tc.repo, "https://") {
				kinds = append(kinds, "wrong port")
			}
		}
		for _, kind := range kinds {
			t.Run(tc.provider+"/"+tc.name+"/"+kind, func(t *testing.T) {
				cfg := config.Merge(config.DefaultGlobalConfig(), &config.RepoConfig{})
				path, webBase := tc.path, tc.webBase
				if kind == "wrong repository" {
					owner, _, _ := strings.Cut(path, "/")
					path = owner + "/other"
				}
				if kind == "wrong host" {
					webBase = "https://other.example:3000"
				}
				if kind == "wrong port" {
					webBase = strings.TrimSuffix(webBase, ":3000") + ":4000"
				}
				link := func(suffix string) string {
					if kind == "absent" {
						return ""
					}
					return webBase + "/" + path + suffix
				}
				payload := map[string]any{"number": 7, "title": "PR", "body": "author"}
				switch tc.provider {
				case "github":
					payload["url"] = link("/pull/7")
				case "gitlab":
					payload = map[string]any{"iid": 7, "title": "PR", "description": "author", "web_url": link("/-/merge_requests/7")}
				case "origin":
					payload = map[string]any{"number": 7, "title": "PR", "description": "author", "url": link("/pull/7")}
				case "forgejo":
					t.Setenv("FORGEJO_BASE_URL", tc.webBase)
					status := map[string]any{
						"host":         map[string]string{"url": tc.webBase, "api_url": tc.webBase + "/api/v1"},
						"auth":         map[string]any{"configured": true, "authenticated": true, "source": "test"},
						"capabilities": map[string]any{"pull_requests": true, "probe": map[string]any{"source": "swagger", "complete": true}},
					}
					data, err := json.Marshal(status)
					if err != nil {
						t.Fatal(err)
					}
					t.Setenv("FAKE_CLI_STATUS_JSON", string(data))
					payload["html_url"] = link("/pulls/7")
					payload = map[string]any{"status": 200, "data": payload}
				case "gitea":
					payload["html_url"] = link("/pulls/7")
				case "azure":
					project, name, _ := strings.Cut(path, "/")
					if kind == "wrong project" {
						project = "other"
					}
					identity := map[string]any{"name": name, "project": map[string]string{"name": project}, "webUrl": webBase + "/" + project + "/_git/" + name}
					if kind == "absent" {
						identity = nil
					}
					payload = map[string]any{"pullRequestId": 7, "title": "PR", "description": "author", "repository": identity}
				case "bitbucket":
					payload = map[string]any{"id": 7, "title": "PR", "summary": map[string]string{"raw": "author"}, "links": map[string]any{"html": map[string]string{"href": link("/pull-requests/7")}}}
				case "plugin":
					cfg.ProviderPlugins = config.ProviderPlugins{"identity": {Command: "appendix-provider", Hosts: []string{"plugin.example"}}}
					t.Setenv("FAKE_CLI_STATUS_JSON", `{"protocol_version":1,"max_pr_body_chars":0,"capabilities":{}}`)
					payload = map[string]any{"pr": map[string]string{"number": "7", "url": link("/pulls/7")}, "title": "PR", "body": "author"}
				}
				data, err := json.Marshal(payload)
				if err != nil {
					t.Fatal(err)
				}
				raw = string(data)
				t.Setenv("FAKE_CLI_PR_CONTENT_JSON", raw)
				cmd := newAxiPRVerifyCmdWithReader(func(ctx context.Context, repo string) (steps.PRAppendixReader, error) {
					return steps.NewPRAppendixReader(ctx, cfg, repo)
				})
				cmd.SetArgs([]string{"--repo", tc.repo, "--pr", "7"})
				var out bytes.Buffer
				cmd.SetOut(&out)
				err = cmd.Execute()
				if kind == "match" {
					if err != nil || !strings.Contains(out.String(), "verdict: absent") {
						t.Fatalf("matching identity: %v: %s", err, &out)
					}
				} else if err == nil || strings.Contains(out.String(), "verdict:") || !strings.Contains(out.String(), "error:") {
					t.Fatalf("foreign/absent identity must refuse with reason and no verdict: %v: %s", err, &out)
				}
			})
		}
	}
}
