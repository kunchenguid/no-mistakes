package main

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

func readFlake(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("flake.nix")
	if err != nil {
		t.Fatalf("read flake.nix: %v", err)
	}
	return string(data)
}

// The `# x-release-please-version` annotation in flake.nix is inert unless the
// file is registered under extra-files. Without the registration the flake
// keeps building the last hand-written version forever, so `nix run` misreports
// `--version` and the CLI advertises an update it can never install from the
// read-only Nix store.
func TestReleasePleaseConfigBumpsFlakeVersion(t *testing.T) {
	data, err := os.ReadFile("release-please-config.json")
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var cfg struct {
		Packages map[string]struct {
			ExtraFiles []string `json:"extra-files"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	pkg, ok := cfg.Packages["."]
	if !ok {
		t.Fatalf("release-please config missing '.' package")
	}
	for _, f := range pkg.ExtraFiles {
		if f == "flake.nix" {
			return
		}
	}
	t.Fatalf("release-please config must list flake.nix under extra-files; the x-release-please-version annotation is otherwise never applied, got %v", pkg.ExtraFiles)
}

func TestFlakeVersionMatchesReleaseManifest(t *testing.T) {
	data, err := os.ReadFile(".release-please-manifest.json")
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var manifest map[string]string
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	want, ok := manifest["."]
	if !ok {
		t.Fatalf("manifest missing '.' entry")
	}

	match := regexp.MustCompile(`version = "([^"]+)"; # x-release-please-version`).FindStringSubmatch(readFlake(t))
	if match == nil {
		t.Fatalf("flake.nix must carry an annotated `version = \"...\"; # x-release-please-version` line")
	}
	if match[1] != want {
		t.Fatalf("flake version %q does not match release manifest %q", match[1], want)
	}
}

// The flake is a real install path, so it must stamp the same buildinfo
// variables the Makefile and release workflow stamp; a missing one renders as
// a placeholder in `no-mistakes --version`.
func TestFlakeStampsBuildinfoLikeReleaseBuilds(t *testing.T) {
	content := readFlake(t)
	for _, name := range []string{"Version", "Commit", "Date", "TelemetryWebsiteID"} {
		if !strings.Contains(content, "internal/buildinfo."+name+"=") {
			t.Errorf("flake ldflags must set buildinfo.%s", name)
		}
	}
}
