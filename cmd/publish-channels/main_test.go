package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/update"
)

func TestRunPublishesHighestSemverBetaAndGitHubLatestStable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("publish-channels is invoked from Ubuntu GitHub Actions")
	}

	fakeBin := t.TempDir()
	uploadDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "gh.log")
	latestPath := filepath.Join(t.TempDir(), "latest.json")
	allPath := filepath.Join(t.TempDir(), "all.json")
	if err := os.WriteFile(latestPath, []byte(`{"tag_name":"v1.2.3","draft":false,"prerelease":false,"assets":[{"name":"checksums.txt","browser_download_url":"https://github.com/example/stable"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(allPath, []byte(`[
		{"tag_name":"v1.3.0-beta.2","draft":false,"prerelease":true,"assets":[{"name":"checksums.txt","browser_download_url":"https://github.com/example/beta"}]},
		{"tag_name":"v1.2.3","draft":false,"prerelease":false,"assets":[{"name":"checksums.txt","browser_download_url":"https://github.com/example/stable"}]},
		{"tag_name":"v1.3.0-beta.1","draft":false,"prerelease":true,"assets":[]}
	]`), 0o644); err != nil {
		t.Fatal(err)
	}

	script := `#!/bin/sh
printf '%s\n' "$*" >> "$GH_LOG"
case "$1" in
  api)
    if [ "$2" = "--paginate" ]; then
      cat "$ALL_RELEASES"
      exit 0
    fi
    case "$2" in
      */releases/latest)
        cat "$LATEST_RELEASE"
        exit 0
        ;;
    esac
    echo "unexpected api args: $*" >&2
    exit 1
    ;;
  release)
    if [ "$2" = "view" ]; then
      echo "release not found" >&2
      exit 1
    fi
    if [ "$2" = "create" ]; then
      exit 0
    fi
    if [ "$2" = "upload" ]; then
      cp "$4" "$UPLOAD_DIR/channels.json"
      exit 0
    fi
    echo "unexpected release args: $*" >&2
    exit 1
    ;;
esac
echo "unexpected gh args: $*" >&2
exit 1
`
	ghPath := filepath.Join(fakeBin, "gh")
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GH_REPO", "kunchenguid/no-mistakes")
	t.Setenv("GH_LOG", logPath)
	t.Setenv("LATEST_RELEASE", latestPath)
	t.Setenv("ALL_RELEASES", allPath)
	t.Setenv("UPLOAD_DIR", uploadDir)

	if err := run(); err != nil {
		t.Fatalf("run error = %v", err)
	}

	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	logText := string(logged)
	if !strings.Contains(logText, "release create channels") {
		t.Fatalf("expected to create the channels release, gh log:\n%s", logText)
	}
	if !strings.Contains(logText, "--prerelease") || !strings.Contains(logText, "--latest=false") {
		t.Fatalf("channels release must be a non-latest prerelease so it cannot replace GitHub latest, gh log:\n%s", logText)
	}
	if !strings.Contains(logText, "release upload channels") || !strings.Contains(logText, "--clobber") {
		t.Fatalf("expected clobbering upload of channels.json, gh log:\n%s", logText)
	}

	payload, err := os.ReadFile(filepath.Join(uploadDir, "channels.json"))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := update.EncodeChannelsManifest(
		[]byte(`{"tag_name":"v1.2.3","draft":false,"prerelease":false,"assets":[{"name":"checksums.txt","browser_download_url":"https://github.com/example/stable"}]}`),
		[]byte(`[
			{"tag_name":"v1.3.0-beta.2","draft":false,"prerelease":true,"assets":[{"name":"checksums.txt","browser_download_url":"https://github.com/example/beta"}]},
			{"tag_name":"v1.2.3","draft":false,"prerelease":false,"assets":[{"name":"checksums.txt","browser_download_url":"https://github.com/example/stable"}]},
			{"tag_name":"v1.3.0-beta.1","draft":false,"prerelease":true,"assets":[]}
		]`),
	)
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != string(decoded) {
		t.Fatalf("uploaded manifest = %s, want %s", payload, decoded)
	}

	var manifest struct {
		Stable struct {
			TagName string `json:"tag_name"`
		} `json:"stable"`
		Beta struct {
			TagName string `json:"tag_name"`
		} `json:"beta"`
	}
	if err := json.Unmarshal(payload, &manifest); err != nil {
		t.Fatalf("uploaded manifest is not JSON: %v", err)
	}
	if manifest.Stable.TagName != "v1.2.3" {
		t.Fatalf("stable tag = %q, want v1.2.3", manifest.Stable.TagName)
	}
	if manifest.Beta.TagName != "v1.3.0-beta.2" {
		t.Fatalf("beta tag = %q, want v1.3.0-beta.2", manifest.Beta.TagName)
	}
}
