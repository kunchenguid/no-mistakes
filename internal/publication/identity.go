package publication

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/buildinfo"
)

const minAbbreviatedBuildSHABytes = 7

// CurrentPublisherBinding derives the identity shared by publication CLI and
// daemon processes from one executable and one exact source revision. A full
// ldflag commit is authoritative; an abbreviated or absent ldflag must be
// expanded from clean Go VCS build information.
func CurrentPublisherBinding(executablePath string) (PublisherBinding, error) {
	info, ok := debug.ReadBuildInfo()
	return publisherBindingWithBuildInfo(executablePath, buildinfo.Commit, info, ok)
}

func publisherBindingWithBuildInfo(executablePath, ldflagCommit string, info *debug.BuildInfo, buildInfoOK bool) (PublisherBinding, error) {
	if !filepath.IsAbs(executablePath) || !utf8.ValidString(executablePath) || len(executablePath) > MaxPublisherExecutablePathBytes {
		return PublisherBinding{}, fmt.Errorf("publisher executable path must be an absolute UTF-8 path of at most %d bytes", MaxPublisherExecutablePathBytes)
	}
	resolvedPath, err := filepath.EvalSymlinks(executablePath)
	if err != nil {
		return PublisherBinding{}, fmt.Errorf("resolve publisher executable: %w", err)
	}
	if !filepath.IsAbs(resolvedPath) || !utf8.ValidString(resolvedPath) || len(resolvedPath) > MaxPublisherExecutablePathBytes {
		return PublisherBinding{}, fmt.Errorf("resolved publisher executable path is not an absolute bounded UTF-8 path")
	}
	file, err := os.Open(resolvedPath)
	if err != nil {
		return PublisherBinding{}, fmt.Errorf("open publisher executable: %w", err)
	}
	defer file.Close()
	fileInfo, err := file.Stat()
	if err != nil {
		return PublisherBinding{}, fmt.Errorf("stat publisher executable: %w", err)
	}
	if !fileInfo.Mode().IsRegular() {
		return PublisherBinding{}, fmt.Errorf("publisher executable is not a regular file")
	}
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return PublisherBinding{}, fmt.Errorf("hash publisher executable: %w", err)
	}
	buildSHA, err := resolvePublisherBuildSHA(ldflagCommit, info, buildInfoOK)
	if err != nil {
		return PublisherBinding{}, err
	}
	return PublisherBinding{
		ExecutablePath:   resolvedPath,
		ExecutableSHA256: hex.EncodeToString(hasher.Sum(nil)),
		BuildSHA:         buildSHA,
		Protocol:         ProtocolV1,
	}, nil
}

func resolvePublisherBuildSHA(ldflagCommit string, info *debug.BuildInfo, buildInfoOK bool) (string, error) {
	if isLowerHex(ldflagCommit, 40) {
		return ldflagCommit, nil
	}

	shortCommit := ""
	switch {
	case ldflagCommit == "", ldflagCommit == "unknown", ldflagCommit == "dev":
		// A clean VCS revision below is the only fallback authority.
	case len(ldflagCommit) >= minAbbreviatedBuildSHABytes && len(ldflagCommit) < 40 && isLowerHex(ldflagCommit, len(ldflagCommit)):
		shortCommit = ldflagCommit
	default:
		return "", fmt.Errorf("publisher build SHA ldflag is invalid")
	}

	revision, modified, err := publisherVCSSettings(info, buildInfoOK)
	if err != nil {
		return "", err
	}
	if modified {
		return "", fmt.Errorf("publisher VCS build is dirty")
	}
	if !isLowerHex(revision, 40) {
		return "", fmt.Errorf("publisher VCS revision must be a full 40-character lowercase commit SHA")
	}
	if shortCommit != "" && !strings.HasPrefix(revision, shortCommit) {
		return "", fmt.Errorf("publisher build SHA ldflag is not a prefix of the full VCS revision")
	}
	return revision, nil
}

func publisherVCSSettings(info *debug.BuildInfo, buildInfoOK bool) (revision string, modified bool, err error) {
	if !buildInfoOK || info == nil {
		return "", false, fmt.Errorf("publisher VCS build information is unavailable")
	}
	modifiedValue := ""
	revisionSeen := false
	modifiedSeen := false
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			if revisionSeen {
				return "", false, fmt.Errorf("publisher VCS revision is ambiguous")
			}
			revisionSeen = true
			revision = setting.Value
		case "vcs.modified":
			if modifiedSeen {
				return "", false, fmt.Errorf("publisher VCS modified state is ambiguous")
			}
			modifiedSeen = true
			modifiedValue = setting.Value
		}
	}
	if revision == "" || (modifiedValue != "true" && modifiedValue != "false") {
		return "", false, fmt.Errorf("publisher VCS revision or clean-state evidence is missing")
	}
	return revision, modifiedValue == "true", nil
}
