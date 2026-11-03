package steps

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestPRTestingSummary_ExpandedAttachmentsKeepLinksAndVideoParagraph(t *testing.T) {
	steps, rounds := testStepWithArtifacts(`{"kind":"video","label":"Demo","path":"demo.mp4"},{"kind":"screenshot","label":"Screen","path":"screen.png"}`)
	attachments := map[string]string{
		"demo.mp4":   "https://github.com/user-attachments/assets/video",
		"screen.png": "https://github.com/user-attachments/assets/image",
	}
	md := buildPRTestingSummary(steps, rounds, evidenceTestUpstream, "abc123", "", "", nil, scm.ProviderGitHub, attachments, false)
	for _, want := range []string{
		"- Evidence: [Demo](https://github.com/example/widgets/blob/abc123/demo.mp4)",
		"- Evidence: [Screen](https://github.com/example/widgets/blob/abc123/screen.png)",
		"\n\n" + attachments["demo.mp4"] + "\n\n",
		"![Screen](" + attachments["screen.png"] + ")",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("missing attachment rendering %q:\n%s", want, md)
		}
	}
}

func TestPRTestingSummary_EvidenceCompact(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "run.txt"), []byte("checks passed"), 0o600); err != nil {
		t.Fatal(err)
	}
	links := &evidenceLinks{
		blobBase: "https://github.com/example/widgets/blob/abc123/",
		rawBase:  "https://raw.githubusercontent.com/example/widgets/abc123/",
		root:     root,
		files:    map[string]bool{"screen.png": true, "run.txt": true, "demo.mp4": true, "animation.gif": true},
	}
	steps, rounds := testStepWithArtifacts(fmt.Sprintf(`{"kind":"screenshot","label":"Screen","path":%q},{"kind":"log","label":"Log","path":%q},{"kind":"video","label":"Demo","path":%q},{"kind":"screenshot","label":"Animation","path":%q}`, filepath.Join(root, "screen.png"), filepath.Join(root, "run.txt"), filepath.Join(root, "demo.mp4"), filepath.Join(root, "animation.gif")))
	findings := strings.Replace(*steps[0].FindingsJSON, `"artifacts":`, `"tested":["go test ./internal/config"],"artifacts":`, 1)
	steps[0].FindingsJSON = &findings
	rounds["s1"][0].FindingsJSON = &findings
	for _, compact := range []bool{true, false} {
		t.Run(fmt.Sprint(compact), func(t *testing.T) {
			repo, err := config.LoadRepoFromBytes([]byte(fmt.Sprintf("test:\n  evidence:\n    store_in_repo: true\n    compact: %v\n", compact)))
			if err != nil {
				t.Fatal(err)
			}
			cfg := config.Merge(&config.GlobalConfig{}, repo)
			if !cfg.Test.Evidence.StoreInRepo {
				t.Fatal("store_in_repo not enabled")
			}
			md := buildPRTestingSummary(steps, rounds, evidenceTestUpstream, "head", t.TempDir(), root, links, scm.ProviderGitHub, nil, cfg.Test.Evidence.Compact)
			for _, artifact := range []struct{ label, path string }{{"Screen", "screen.png"}, {"Log", "run.txt"}, {"Demo", "demo.mp4"}, {"Animation", "animation.gif"}} {
				want := "[" + artifact.label + "](" + links.blobBase + artifact.path + ")"
				if !strings.Contains(md, want) {
					t.Errorf("missing durable link %q:\n%s", want, md)
				}
			}
			if compact {
				legacy := BuildTestingSummaryForPR(steps, rounds, evidenceTestUpstream, "head", t.TempDir(), root, links)
				if md != legacy {
					t.Errorf("default PR output changed:\n%s\nlegacy:\n%s", md, legacy)
				}
				if strings.Contains(md, "![Screen]") {
					t.Errorf("compact output embeds screenshot:\n%s", md)
				}
			} else {
				for _, want := range []string{"![Screen](" + links.rawBase + "screen.png)", "![Animation](" + links.rawBase + "animation.gif)", "<video src=\"" + links.rawBase + "demo.mp4\" controls></video>", "```text\nchecks passed\n```", "- Summary:", "- `go test ./internal/config`"} {
					if !strings.Contains(md, want) {
						t.Errorf("missing richer output %q:\n%s", want, md)
					}
				}
				if strings.Contains(md, "<details>") {
					t.Errorf("non-compact text is collapsed:\n%s", md)
				}
			}
		})
	}
}
