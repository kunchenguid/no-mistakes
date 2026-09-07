// Command publish-channels writes the no-mistakes update channel manifest
// (channels.json) and uploads it to a dedicated GitHub prerelease tagged
// `channels`. The updater reads that asset over the un-rate-limited release
// download CDN instead of the REST API.
//
// Usage (CI):
//
//	GH_REPO=kunchenguid/no-mistakes GH_TOKEN=... go run ./cmd/publish-channels
package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/update"
)

const (
	channelsTag   = "channels"
	channelsAsset = "channels.json"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "publish-channels: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	repo := strings.TrimSpace(os.Getenv("GH_REPO"))
	if repo == "" {
		return fmt.Errorf("GH_REPO is required")
	}

	latest, err := ghAPIOptional("repos/" + repo + "/releases/latest")
	if err != nil {
		return err
	}
	all, err := ghOutput("api", "--paginate", "repos/"+repo+"/releases")
	if err != nil {
		return fmt.Errorf("list releases: %w", err)
	}
	manifest, err := update.EncodeChannelsManifest(latest, all)
	if err != nil {
		return err
	}

	dir, err := os.MkdirTemp("", "nm-channels-")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, channelsAsset)
	if err := os.WriteFile(path, manifest, 0o600); err != nil {
		return fmt.Errorf("write temp manifest: %w", err)
	}

	if err := ensureChannelsRelease(repo); err != nil {
		return err
	}
	if err := ghRun("release", "upload", channelsTag, path, "--clobber", "--repo", repo); err != nil {
		return fmt.Errorf("upload %s: %w", channelsAsset, err)
	}
	fmt.Printf("published %s to %s@%s\n", channelsAsset, repo, channelsTag)
	return nil
}

func ensureChannelsRelease(repo string) error {
	if err := ghRun("release", "view", channelsTag, "--repo", repo); err == nil {
		return nil
	}
	notes := "Channel index for `no-mistakes update`. Not a product release; the updater reads " + channelsAsset + " from this tag over the GitHub release-asset CDN."
	return ghRun(
		"release", "create", channelsTag,
		"--repo", repo,
		"--prerelease",
		"--latest=false",
		"--title", "Update channels",
		"--notes", notes,
	)
}

func ghAPIOptional(endpoint string) ([]byte, error) {
	out, err := ghOutput("api", endpoint)
	if err == nil {
		return out, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && (bytes.Contains(exitErr.Stderr, []byte("404")) || bytes.Contains(out, []byte("\"status\":\"404\""))) {
		return nil, nil
	}
	return nil, fmt.Errorf("GET %s: %w", endpoint, err)
}

func ghOutput(args ...string) ([]byte, error) {
	cmd := exec.Command("gh", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitErr.Stderr = stderr.Bytes()
		}
		return out, err
	}
	return out, nil
}

func ghRun(args ...string) error {
	cmd := exec.Command("gh", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
