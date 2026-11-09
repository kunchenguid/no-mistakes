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
	if err := os.WriteFile(filepath.Join(tea, "hosts.yml"), []byte("https://gitea.example:\n  token: synthetic\n"), 0600); err != nil {
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
	for _, provider := range []string{"github", "gitlab", "origin", "forgejo", "gitea", "azure", "bitbucket", "plugin"} {
		for _, kind := range []string{"match", "wrong repository", "absent", "wrong project"} {
			if kind == "wrong project" && provider != "azure" {
				continue
			}
			t.Run(provider+"/"+kind, func(t *testing.T) {
				cfg := config.Merge(config.DefaultGlobalConfig(), &config.RepoConfig{})
				repo := "https://" + provider + ".example/team/target.git"
				path := "team/target"
				if kind == "wrong repository" {
					path = "team/other"
				}
				link := func(prefix, suffix string) string {
					if kind == "absent" {
						return ""
					}
					return prefix + path + suffix
				}
				payload := map[string]any{"number": 7, "title": "PR", "body": "author"}
				switch provider {
				case "github":
					repo = "https://github.com/team/target.git"
					payload["url"] = link("https://github.com/", "/pull/7")
				case "gitlab":
					repo = "https://gitlab.com/team/target.git"
					payload = map[string]any{"iid": 7, "title": "PR", "description": "author", "web_url": link("https://gitlab.com/", "/-/merge_requests/7")}
				case "origin":
					repo = "https://origin.cursor.com/team/target.git"
					payload = map[string]any{"number": 7, "title": "PR", "description": "author", "url": link("https://cursor.com/codebase/", "/pull/7")}
				case "forgejo":
					t.Setenv("FORGEJO_BASE_URL", "https://forgejo.example")
					t.Setenv("FAKE_CLI_STATUS_JSON", `{"host":{"url":"https://forgejo.example","api_url":"https://forgejo.example/api/v1"},"auth":{"configured":true,"authenticated":true,"source":"test"},"capabilities":{"pull_requests":true,"probe":{"source":"swagger","complete":true}}}`)
					payload["html_url"] = link("https://forgejo.example/", "/pulls/7")
					payload = map[string]any{"status": 200, "data": payload}
				case "gitea":
					payload["html_url"] = link("https://gitea.example/", "/pulls/7")
				case "azure":
					repo = "https://dev.azure.com/org/proj/_git/target"
					name, project := "target", "proj"
					if kind == "wrong repository" {
						name = "other"
					}
					if kind == "wrong project" {
						project = "other"
					}
					identity := map[string]any{"name": name, "project": map[string]string{"name": project}, "webUrl": "https://dev.azure.com/org/" + project + "/_git/" + name}
					if kind == "absent" {
						identity = nil
					}
					payload = map[string]any{"pullRequestId": 7, "title": "PR", "description": "author", "repository": identity}
				case "bitbucket":
					repo = "https://bitbucket.org/team/target.git"
					payload = map[string]any{"id": 7, "title": "PR", "summary": map[string]string{"raw": "author"}, "links": map[string]any{"html": map[string]string{"href": link("https://bitbucket.org/", "/pull-requests/7")}}}
				case "plugin":
					repo = "https://plugin.example/team/target.git"
					cfg.ProviderPlugins = config.ProviderPlugins{"identity": {Command: "appendix-provider", Hosts: []string{"plugin.example"}}}
					t.Setenv("FAKE_CLI_STATUS_JSON", `{"protocol_version":1,"max_pr_body_chars":0,"capabilities":{}}`)
					payload = map[string]any{"pr": map[string]string{"number": "7", "url": link("https://plugin.example/", "/pulls/7")}, "title": "PR", "body": "author"}
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
				cmd.SetArgs([]string{"--repo", repo, "--pr", "7"})
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
