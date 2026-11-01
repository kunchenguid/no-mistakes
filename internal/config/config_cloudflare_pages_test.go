package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEffectiveRepoConfig_CloudflarePagesProjectNeverComesFromThePushedBranch(t *testing.T) {
	pushed := "attacker-pages"
	trusted := "team-evidence"
	pushedCfg := &RepoConfig{Test: TestRaw{Evidence: EvidenceRaw{CloudflarePages: CloudflarePagesRaw{Project: &pushed}}}}

	for name, tc := range map[string]struct {
		trusted *RepoConfig
		want    string
	}{
		"no trusted copy":                     {trusted: nil, want: ""},
		"trusted copy without the key":        {trusted: &RepoConfig{}, want: ""},
		"trusted copy naming its own project": {trusted: &RepoConfig{Test: TestRaw{Evidence: EvidenceRaw{CloudflarePages: CloudflarePagesRaw{Project: &trusted}}}}, want: trusted},
	} {
		t.Run(name, func(t *testing.T) {
			for _, allowRepoCommands := range []bool{false, true} {
				effective := EffectiveRepoConfig(pushedCfg, tc.trusted, allowRepoCommands)
				got := Merge(&GlobalConfig{}, effective).Test.Evidence.CloudflarePagesProject
				if got != tc.want {
					t.Fatalf("allow_repo_commands=%t: project = %q, want %q", allowRepoCommands, got, tc.want)
				}
			}
		})
	}
}

func TestMerge_CloudflarePagesProjectIsOffUntilNamedAndATrustedEmptyTurnsTheGlobalOff(t *testing.T) {
	if got := Merge(&GlobalConfig{}, &RepoConfig{}).Test.Evidence.CloudflarePagesProject; got != "" {
		t.Fatalf("default project = %q, want none", got)
	}
	project, empty := "fleet-evidence", ""
	global := &GlobalConfig{Test: TestRaw{Evidence: EvidenceRaw{CloudflarePages: CloudflarePagesRaw{Project: &project}}}}
	if got := Merge(global, &RepoConfig{}).Test.Evidence.CloudflarePagesProject; got != project {
		t.Fatalf("global project = %q, want %q", got, project)
	}
	trustedOff := EffectiveRepoConfig(&RepoConfig{}, &RepoConfig{Test: TestRaw{Evidence: EvidenceRaw{CloudflarePages: CloudflarePagesRaw{Project: &empty}}}}, false)
	if got := Merge(global, trustedOff).Test.Evidence.CloudflarePagesProject; got != "" {
		t.Fatalf("trusted empty project left %q on", got)
	}
}

func TestLoadRepoConfig_FlagShapedCloudflarePagesProjectFailsClosed(t *testing.T) {
	dir := t.TempDir()
	yaml := `
test:
  evidence:
    cloudflare_pages:
      project: "--help"
`
	if err := os.WriteFile(filepath.Join(dir, ".no-mistakes.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadRepo(dir)
	if err == nil {
		t.Fatal("a flag-shaped project name must fail the config")
	}
	if !strings.Contains(err.Error(), "test.evidence.cloudflare_pages.project") || !strings.Contains(err.Error(), "--help") {
		t.Fatalf("error %q does not name the offending key and value", err)
	}
}

func TestLoadGlobalConfig_CloudflarePagesProjectParsed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := `
test:
  evidence:
    cloudflare_pages:
      project: azalialaw-evidence
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	global, err := LoadGlobal(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := Merge(global, &RepoConfig{}).Test.Evidence.CloudflarePagesProject; got != "azalialaw-evidence" {
		t.Fatalf("project = %q", got)
	}
}
