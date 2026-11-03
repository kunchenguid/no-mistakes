//go:build e2e && !windows

package steps

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// This integration driver uses the production renderer, SQLite, git publication,
// and an installed gh. The wrapper records argv and execs gh without substituting
// its response. No agent, uploader, forge, or credential is mocked.
func TestPRMediaOptOutWithRealGH(t *testing.T) {
	realGH, err := exec.LookPath("gh")
	if err != nil {
		t.Skip("live credential-boundary check needs gh on PATH")
	}
	home := t.TempDir()
	bin := t.TempDir()
	calls := filepath.Join(home, "gh-calls.txt")
	wrapper := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$NM_MEDIA_GH_CALLS\"\nexec \"$NM_MEDIA_REAL_GH\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	t.Setenv("GH_CONFIG_DIR", filepath.Join(home, "gh"))
	for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"} {
		t.Setenv(key, "")
	}
	t.Setenv("GH_HOST", "github.com")
	t.Setenv("GH_PROMPT_DISABLED", "1")
	t.Setenv("NM_MEDIA_GH_CALLS", calls)
	t.Setenv("NM_MEDIA_REAL_GH", realGH)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	evidence := os.Getenv("NM_ATTACH_MEDIA_EVIDENCE")

	for _, tc := range []struct {
		name          string
		store, attach bool
		repoOverride  bool
	}{
		{name: "opt-out-with-evidence-branch", store: true},
		{name: "opt-out-with-local-evidence"},
		{name: "repo-opt-out-overrides-global-opt-in", store: true, repoOverride: true},
		{name: "enabled-refused-token-control", store: true, attach: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sctx, remote := newEvidencePublishContext(t, "feature/add-login")
			sctx.Agent = nil // Rendering evidence must not need a draft agent.
			globalYAML := fmt.Sprintf("test:\n  evidence:\n    store_in_repo: %t\n    attach_media: %t\n", tc.store, tc.attach || tc.repoOverride)
			global, err := config.LoadGlobalFromBytes([]byte(globalYAML))
			if err != nil {
				t.Fatal(err)
			}
			repoYAML := "{}"
			if tc.repoOverride {
				repoYAML = "test:\n  evidence:\n    attach_media: false\n"
			}
			repo, err := config.LoadRepoFromBytes([]byte(repoYAML))
			if err != nil {
				t.Fatal(err)
			}
			sctx.Config = config.Merge(global, repo)
			var artifacts []types.TestArtifact
			for _, name := range []string{"one.png", "two.png", "three.png"} {
				path := writeEvidenceFile(t, sctx.EvidenceDir, name, []byte("\x89PNG binary"))
				artifacts = append(artifacts, types.TestArtifact{Kind: "screenshot", Label: name, Path: path})
			}
			payload, err := json.Marshal(map[string]any{"findings": []any{}, "testing_summary": "Three media files collected.", "artifacts": artifacts})
			if err != nil {
				t.Fatal(err)
			}
			insertCompletedStep(t, sctx, types.StepTest, string(payload), "")
			if err := os.WriteFile(calls, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			var logs []string
			sctx.Log = func(line string) { logs = append(logs, line) }
			_, _, markdown := (&PRStep{}).buildPipelineSection(sctx, scm.ProviderGitHub)
			recorded, err := os.ReadFile(calls)
			if err != nil {
				t.Fatal(err)
			}
			wantCalls := 0
			if tc.attach {
				wantCalls = 1
			}
			if got := strings.Count(string(recorded), "auth token"); got != wantCalls {
				t.Errorf("gh auth token calls = %d, want %d: %s", got, wantCalls, recorded)
			}
			if strings.Contains(markdown, "user-attachments") {
				t.Error("opt-out or credential refusal must not emit an attachment URL")
			}
			remoteState := "no evidence branch"
			if tc.store {
				tip := gitCmd(t, remote, "rev-parse", "refs/heads/no-mistakes/evidence")
				remoteState = gitCmd(t, remote, "ls-tree", "-r", "--name-only", tip)
				for _, artifact := range artifacts {
					name := filepath.Base(artifact.Path)
					want := "https://github.com/example/widgets/blob/" + tip + "/.no-mistakes/evidence/feature/add-login/" + name
					if !strings.Contains(markdown, want) {
						t.Errorf("missing commit-pinned link for %s: %s", name, markdown)
					}
					if got := gitCmd(t, remote, "show", tip+":.no-mistakes/evidence/feature/add-login/"+name); got != "\x89PNG binary" {
						t.Errorf("published artifact %s changed: %q", name, got)
					}
				}
			} else if !strings.Contains(markdown, "local file:") {
				t.Errorf("missing local-path fallback: %s", markdown)
			}
			if tc.attach && !strings.Contains(strings.Join(logs, "\n"), "skipping GitHub media attachments for 3 pending file(s)") {
				t.Errorf("enabled control did not encounter real credential refusal: %v", logs)
			}
			if got := gitCmd(t, sctx.WorkDir, "status", "--porcelain"); got != "" {
				t.Errorf("publication dirtied code branch: %s", got)
			}
			transcript := fmt.Sprintf("Scenario: %s\nGlobal config:\n%s\nRepo config:\n%s\ngh invocations (real binary, isolated empty credentials):\n%s\nProduct log:\n%s\nPublished evidence tree:\n%s\n\nGenerated PR testing markdown:\n%s\n", tc.name, globalYAML, repoYAML, recorded, strings.Join(logs, "\n"), remoteState, markdown)
			t.Log(transcript)
			if evidence != "" {
				if err := os.WriteFile(filepath.Join(evidence, tc.name+".txt"), []byte(transcript), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(evidence, tc.name+".md"), []byte(markdown), 0o644); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
	if evidence != "" {
		config.EnsureDefaultGlobalConfig(filepath.Join(evidence, "generated-default-config.yaml"))
	}
}
