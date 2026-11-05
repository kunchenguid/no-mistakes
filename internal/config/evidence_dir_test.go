package config

import (
	"strings"
	"testing"
)

func TestLoadGlobalConfig_EvidenceDir(t *testing.T) {
	global, err := LoadGlobalFromBytes([]byte("evidence_dir: ~/Dropbox/no-mistakes-evidence\n"))
	if err != nil {
		t.Fatal(err)
	}
	if global.EvidenceDir != "~/Dropbox/no-mistakes-evidence" {
		t.Fatalf("evidence_dir = %q", global.EvidenceDir)
	}
	merged := Merge(global, &RepoConfig{})
	if merged.Test.Evidence.LocalRoot != "" {
		t.Fatalf("evidence_dir leaked into local_root %q", merged.Test.Evidence.LocalRoot)
	}
}

func TestLoadRepoConfig_EvidenceDirIsGlobalOnly(t *testing.T) {
	_, err := LoadRepoFromBytes([]byte("evidence_dir: /tmp/evidence\n"))
	if err == nil || !strings.Contains(err.Error(), "evidence_dir is global-only") {
		t.Fatalf("repo evidence_dir error = %v, want a global-only refusal", err)
	}
}
