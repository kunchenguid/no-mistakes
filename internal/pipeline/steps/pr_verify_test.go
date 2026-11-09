package steps

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/scm/plugin"
)

func TestPRAppendixVerifyPublicationParity(t *testing.T) {
	_, appendix := ownedFixture(t)
	valid := wrapPRAppendix(appendix)
	legacy := appendix + "\nraw \x1b and literal ^ERROR"
	cases := []struct{ name, body, want string }{
		{"match", valid, "match"},
		{"changed byte", strings.Replace(valid, "Low recorded risk.", "Low recorded Risk.", 1), "mismatch"},
		{"missing end", strings.ReplaceAll(valid, prAppendixEnd, ""), "malformed"},
		{"missing start", strings.ReplaceAll(valid, prAppendixStart, ""), "malformed"},
		{"duplicate", valid + "\n" + valid, "malformed"},
		{"truncated", prAppendixStart, "malformed"},
		{"unowned", "author text", "absent"},
		{"unowned attestation", appendix, "absent"},
		{"legacy subset", strings.ReplaceAll(wrapPRAppendix(legacy), "\x1b", "^["), "match"},
		{"legacy all above cap", strings.ReplaceAll(wrapPRAppendix(appendix+strings.Repeat("\x1b", 13)), "\x1b", "^["), "match"},
		{"open fence", wrapPRAppendix(appendix + "\n```"), "malformed"},
		{"bad attestation", wrapPRAppendix("<!-- no-mistakes-pipeline-attestation:v1 {} -->"), "malformed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := sha256.Sum256([]byte(tc.body))
			got := VerifyPRAppendix(tc.body)
			if got.Verdict != tc.want || got.Reason == "" {
				t.Fatalf("verify = %+v, want %s and a reason", got, tc.want)
			}
			parts, err := parsePROwnedBody(tc.body)
			if (got.Verdict == "match") != (err == nil && parts.managed) {
				t.Fatalf("verification disagrees with publication: %+v, %v", parts, err)
			}
			if err != nil && got.Reason != err.Error() {
				t.Fatalf("reason %q differs from publication %q", got.Reason, err)
			}
			if sha256.Sum256([]byte(tc.body)) != before {
				t.Fatal("verification changed the body")
			}
		})
	}
}

func TestPRAppendixVerifyGitLabScopesRepository(t *testing.T) {
	for _, remote := range []string{"https://gitlab.com/team/sub/target.git", "git@gitlab.com:team/sub/target.git"} {
		t.Run(remote, func(t *testing.T) {
			t.Chdir(t.TempDir())
			if strings.HasPrefix(remote, "https://") {
				for _, args := range [][]string{{"init"}, {"remote", "add", "origin", "https://gitlab.com/team/other.git"}} {
					if out, err := exec.Command(testGitExecutable, args...).CombinedOutput(); err != nil {
						t.Fatalf("git %v: %v: %s", args, err, out)
					}
				}
			}
			bin := fakeCLIBinDir(t)
			linkTestBinary(t, bin, "glab")
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("FAKE_CLI_MODE", "glab")
			t.Setenv("FAKE_CLI_MR_READBACK_JSON", `{"iid":7,"title":"Target MR","description":"author","web_url":"https://gitlab.com/team/sub/target/-/merge_requests/7"}`)
			log := filepath.Join(t.TempDir(), "requests.log")
			t.Setenv("FAKE_CLI_LOG", log)
			reader, err := NewPRAppendixReader(context.Background(), config.Merge(config.DefaultGlobalConfig(), &config.RepoConfig{}), remote)
			if err != nil {
				t.Fatal(err)
			}
			content, err := reader(context.Background(), "7")
			if err != nil || content.Body != "author" {
				t.Fatalf("read = %+v, %v", content, err)
			}
			calls, err := os.ReadFile(log)
			want := "auth status --hostname gitlab.com\nmr view 7 --output json --repo https://gitlab.com/team/sub/target\n"
			if err != nil || string(calls) != want {
				t.Fatalf("requests = %q, %v; want %q", calls, err, want)
			}
		})
	}
}

func TestPRAppendixVerifyForgejoNumberedRead(t *testing.T) {
	t.Chdir(t.TempDir())
	bin := fakeCLIBinDir(t)
	linkTestBinary(t, bin, "forgejo-axi")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FORGEJO_BASE_URL", "https://code.example:3443/git")
	t.Setenv("FORGEJO_TOKEN", "")
	t.Setenv("FAKE_CLI_MODE", "pr-appendix-verify")
	t.Setenv("FAKE_CLI_STATUS_JSON", `{"host":{"url":"https://code.example:3443/git","api_url":"https://code.example:3443/git/api/v1"},"auth":{"configured":true,"authenticated":true,"source":"test"},"capabilities":{"pull_requests":true,"probe":{"source":"swagger","complete":true}}}`)
	log := filepath.Join(t.TempDir(), "requests.log")
	t.Setenv("FAKE_CLI_LOG", log)
	reader, err := NewPRAppendixReader(context.Background(), config.Merge(config.DefaultGlobalConfig(), &config.RepoConfig{}), "https://code.example:3443/git/team/target.git")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		response string
		wantErr  bool
	}{
		{`{"status":200,"data":{"number":7,"title":"Target PR","body":"author","html_url":"https://code.example:3443/git/team/target/pulls/7"}}`, false},
		{`{"status":200,"data":{"number":8,"title":"Other PR","body":"author"}}`, true},
	} {
		t.Setenv("FAKE_CLI_PR_CONTENT_JSON", tc.response)
		content, err := reader(context.Background(), "7")
		if (err != nil) != tc.wantErr || (!tc.wantErr && content.Body != "author") {
			t.Fatalf("read = %+v, %v; want error %t", content, err, tc.wantErr)
		}
	}
	calls, err := os.ReadFile(log)
	want := "status --base-url https://code.example:3443/git --json\n" +
		strings.Repeat("api GET repos/team/target/pulls/7 --base-url https://code.example:3443/git --json\n", 2)
	if err != nil || string(calls) != want {
		t.Fatalf("requests = %q, %v; want %q", calls, err, want)
	}
}

func TestPRAppendixVerifyPluginHandshake(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		wantErr      bool
	}{
		{"valid", `{"protocol_version":1,"max_pr_body_chars":0,"capabilities":{}}`, false},
		{"unsupported protocol", `{"protocol_version":99,"max_pr_body_chars":0,"capabilities":{}}`, true},
		{"missing body limit", `{"protocol_version":1,"capabilities":{}}`, true},
		{"invalid JSON", `not JSON`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := fakeCLIBinDir(t)
			linkTestBinary(t, bin, "appendix-provider")
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("FAKE_CLI_MODE", "pr-appendix-verify")
			t.Setenv("FAKE_CLI_STATUS_JSON", tc.status)
			t.Setenv("FAKE_CLI_PR_CONTENT_JSON", `{"pr":{"number":"7","url":"https://git.example.com/team/target/pulls/7"},"title":"Target PR","body":"author"}`)
			log := filepath.Join(t.TempDir(), "requests.log")
			t.Setenv("FAKE_CLI_LOG", log)
			cfg := config.Merge(config.DefaultGlobalConfig(), &config.RepoConfig{})
			cfg.ProviderPlugins = config.ProviderPlugins{"test": {Command: "appendix-provider", Hosts: []string{"git.example.com"}}}
			reader, err := NewPRAppendixReader(context.Background(), cfg, "https://git.example.com/team/target.git")
			if tc.wantErr {
				if !errors.Is(err, plugin.ErrProtocol) || reader != nil {
					t.Fatalf("reader = %v, error = %v; want protocol refusal", reader == nil, err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				content, err := reader(context.Background(), "7")
				if err != nil || content.Body != "author" {
					t.Fatalf("read = %+v, %v", content, err)
				}
			}
			calls, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
			wantCalls := 2
			if tc.wantErr {
				wantCalls = 1
			}
			if len(lines) != wantCalls || !strings.HasPrefix(lines[0], "status ") ||
				(!tc.wantErr && !strings.HasPrefix(lines[1], "pr view 7 ")) {
				t.Fatalf("unexpected requests: %s", calls)
			}
		})
	}
}
