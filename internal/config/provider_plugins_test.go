package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadGlobalParsesProviderPlugins(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadGlobalFromBytes([]byte(`
provider_plugins:
  ssm:
    command: ~/bin/nm-ssm
    args: [--profile, work]
    hosts: ["*.sourcemanager.dev", "Git.Example.com"]
    timeout: 30s
    draft_pull_requests: true
  other:
    command: nm-other
    hosts: [other.example.com]
`))
	if err != nil {
		t.Fatalf("LoadGlobalFromBytes: %v", err)
	}
	ssm := cfg.ProviderPlugins["ssm"]
	if ssm.Command != filepath.Join(home, "bin", "nm-ssm") {
		t.Fatalf("ssm command = %q", ssm.Command)
	}
	if strings.Join(ssm.Args, " ") != "--profile work" || ssm.Timeout != 30*time.Second || !ssm.DraftPullRequests {
		t.Fatalf("ssm = %+v", ssm)
	}
	if strings.Join(ssm.Hosts, ",") != "*.sourcemanager.dev,git.example.com" {
		t.Fatalf("ssm hosts = %v", ssm.Hosts)
	}
	other := cfg.ProviderPlugins["other"]
	if other.Command != "nm-other" || other.Timeout != DefaultProviderPluginTimeout || other.DraftPullRequests {
		t.Fatalf("other = %+v", other)
	}
}

func TestLoadGlobalWithoutProviderPlugins(t *testing.T) {
	for _, contents := range []string{"agent: auto\n", "provider_plugins: {}\n"} {
		cfg, err := LoadGlobalFromBytes([]byte(contents))
		if err != nil {
			t.Fatalf("LoadGlobalFromBytes(%q): %v", contents, err)
		}
		if len(cfg.ProviderPlugins) != 0 {
			t.Fatalf("ProviderPlugins = %#v", cfg.ProviderPlugins)
		}
	}
}

func TestLoadGlobalRejectsInvalidProviderPlugins(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{name: "uppercase name", yaml: "  SSM: {command: nm-ssm, hosts: [a.example.com]}", wantErr: "name must be"},
		{name: "built-in name", yaml: "  gitlab: {command: nm-ssm, hosts: [a.example.com]}", wantErr: "built-in provider name"},
		{name: "missing command", yaml: "  ssm: {hosts: [a.example.com]}", wantErr: "command is required"},
		{name: "relative command", yaml: "  ssm: {command: bin/nm-ssm, hosts: [a.example.com]}", wantErr: "absolute or start with ~/"},
		{name: "dot relative command", yaml: "  ssm: {command: ./nm-ssm, hosts: [a.example.com]}", wantErr: "absolute or start with ~/"},
		{name: "no hosts", yaml: "  ssm: {command: nm-ssm}", wantErr: "at least one host"},
		{name: "empty host", yaml: `  ssm: {command: nm-ssm, hosts: [""]}`, wantErr: "must not be empty"},
		{name: "scheme", yaml: "  ssm: {command: nm-ssm, hosts: [\"https://a.example.com\"]}", wantErr: "not a host name"},
		{name: "port", yaml: "  ssm: {command: nm-ssm, hosts: [\"a.example.com:443\"]}", wantErr: "not a host name"},
		{name: "path", yaml: "  ssm: {command: nm-ssm, hosts: [a.example.com/team]}", wantErr: "not a host name"},
		{name: "user", yaml: "  ssm: {command: nm-ssm, hosts: [git@a.example.com]}", wantErr: "not a host name"},
		{name: "bare wildcard", yaml: "  ssm: {command: nm-ssm, hosts: [\"*\"]}", wantErr: "not a host name"},
		{name: "inner wildcard", yaml: "  ssm: {command: nm-ssm, hosts: [\"a.*.com\"]}", wantErr: "not a host name"},
		{name: "duplicate within plugin", yaml: "  ssm: {command: nm-ssm, hosts: [a.example.com, A.example.com]}", wantErr: "already claimed by provider_plugins.ssm"},
		{name: "duplicate across plugins", yaml: "  aaa: {command: nm-a, hosts: [a.example.com]}\n  bbb: {command: nm-b, hosts: [a.example.com]}", wantErr: "already claimed by provider_plugins.aaa"},
		{name: "bad timeout", yaml: "  ssm: {command: nm-ssm, hosts: [a.example.com], timeout: soon}", wantErr: "provider_plugins.ssm.timeout"},
		{name: "zero timeout", yaml: "  ssm: {command: nm-ssm, hosts: [a.example.com], timeout: 0s}", wantErr: "provider_plugins.ssm.timeout"},
		{name: "unknown field", yaml: "  ssm: {command: nm-ssm, hosts: [a.example.com], token: x}", wantErr: "token"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadGlobalFromBytes([]byte("provider_plugins:\n" + tt.yaml + "\n"))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("LoadGlobalFromBytes error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadGlobalRejectsHostClaimedByForgeProfileAndPlugin(t *testing.T) {
	gitlabDir := filepath.ToSlash(filepath.Join(t.TempDir(), "glab"))
	for _, pattern := range []string{"git.example.com", "*.example.com"} {
		t.Run(pattern, func(t *testing.T) {
			contents := fmt.Sprintf("forge_profiles:\n  git.example.com:\n    glab_config_dir: %q\n"+
				"provider_plugins:\n  ssm:\n    command: nm-ssm\n    hosts: [%q]\n", gitlabDir, pattern)
			_, err := LoadGlobalFromBytes([]byte(contents))
			if err == nil || !strings.Contains(err.Error(), "also claimed by provider_plugins.ssm") {
				t.Fatalf("LoadGlobalFromBytes error = %v", err)
			}
		})
	}
}

func TestProviderPluginsSelect(t *testing.T) {
	plugins := ProviderPlugins{
		"exact":  {Hosts: []string{"git.corp.example.com"}},
		"broad":  {Hosts: []string{"*.example.com"}},
		"narrow": {Hosts: []string{"*.corp.example.com"}},
		"alias":  {Hosts: []string{"work-ssm"}},
	}
	never := func() string { t.Fatal("resolved host consulted after a raw match"); return "" }
	tests := []struct {
		raw      string
		resolved func() string
		want     string
	}{
		{raw: "git.corp.example.com", resolved: never, want: "exact"},
		{raw: "GIT.CORP.EXAMPLE.COM", resolved: never, want: "exact"},
		{raw: "ci.corp.example.com", resolved: never, want: "narrow"},
		{raw: "ci.example.com", resolved: never, want: "broad"},
		{raw: "example.com", resolved: func() string { return "" }, want: ""},
		{raw: "badexample.com", resolved: func() string { return "" }, want: ""},
		{raw: "work-ssm", resolved: never, want: "alias"},
		{raw: "my-alias", resolved: func() string { return "x.example.com" }, want: "broad"},
		{raw: "github.com", resolved: func() string { return "github.com" }, want: ""},
		{raw: "github.com", resolved: nil, want: ""},
	}
	for _, tt := range tests {
		got, ok := plugins.Select(tt.raw, tt.resolved)
		if got != tt.want || ok != (tt.want != "") {
			t.Errorf("Select(%q) = %q, %v; want %q", tt.raw, got, ok, tt.want)
		}
	}
	if _, ok := ProviderPlugins(nil).Select("git.corp.example.com", never); ok {
		t.Fatal("nil plugins selected a host")
	}
}

func TestMergeTakesProviderPluginsFromGlobalOnly(t *testing.T) {
	global, err := LoadGlobalFromBytes([]byte("provider_plugins:\n  ssm:\n    command: nm-ssm\n    hosts: [git.example.com]\n"))
	if err != nil {
		t.Fatal(err)
	}
	repo, err := LoadRepoFromBytes([]byte("provider_plugins:\n  evil:\n    command: /tmp/evil\n    hosts: [git.example.com]\n"))
	if err != nil {
		t.Fatalf("LoadRepoFromBytes: %v", err)
	}
	merged := Merge(global, repo)
	if len(merged.ProviderPlugins) != 1 || merged.ProviderPlugins["ssm"].Command != "nm-ssm" {
		t.Fatalf("merged ProviderPlugins = %#v", merged.ProviderPlugins)
	}
	repoOnly := Merge(DefaultGlobalConfig(), repo)
	if len(repoOnly.ProviderPlugins) != 0 {
		t.Fatalf("repository config configured a provider plugin: %#v", repoOnly.ProviderPlugins)
	}
}
