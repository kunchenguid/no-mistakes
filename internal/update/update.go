package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/buildinfo"
	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

const (
	appName            = "no-mistakes"
	repoName           = "kunchenguid/no-mistakes"
	backgroundFlag     = "--update-check"
	noUpdateCheckEnv   = "NO_MISTAKES_NO_UPDATE_CHECK"
	checksumsAssetName = "checksums.txt"
	cacheTTL           = 24 * time.Hour
	maxAPIResponseSize = 5 << 20
	maxDownloadSize    = 100 << 20
	maxExtractedSize   = 100 << 20
)

var allowInsecureDownloads bool
var githubAPIBaseURL = "https://api.github.com"
var daemonIsRunning = daemon.IsRunning
var daemonStop = daemon.Stop
var daemonStart = daemon.Start

type daemonResetError struct {
	err           error
	daemonOffline bool
}

func (e *daemonResetError) Error() string {
	return e.err.Error()
}

func (e *daemonResetError) Unwrap() error {
	return e.err
}

type platformSpec struct {
	GOOS   string
	GOARCH string
}

type releaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type checkCache struct {
	CheckedAt     time.Time `json:"checked_at"`
	LatestVersion string    `json:"latest_version"`
}

type releaseResponse struct {
	TagName string         `json:"tag_name"`
	Assets  []releaseAsset `json:"assets"`
}

type releasePlan struct {
	CurrentVersion  string
	LatestVersion   string
	UpdateAvailable bool
	ArchiveName     string
	Archive         releaseAsset
	Checksums       releaseAsset
}

type updater struct {
	appName           string
	repo              string
	currentVersion    string
	platform          platformSpec
	apiBaseURL        string
	httpClient        *http.Client
	cachePath         string
	executablePath    string
	stdout            io.Writer
	stderr            io.Writer
	now               func() time.Time
	spawnBackground   func(currentVersion string) error
	resetDaemon       func() error
	disableBackground bool
	noColor           bool
}

type semVersion struct {
	major      int
	minor      int
	patch      int
	prerelease []string
}

func compareVersions(a, b string) (int, error) {
	va, err := parseVersion(a)
	if err != nil {
		return 0, err
	}
	vb, err := parseVersion(b)
	if err != nil {
		return 0, err
	}
	return va.compare(vb), nil
}

func Run(ctx context.Context, stdout, stderr io.Writer) error {
	u, err := defaultUpdater(stdout, stderr)
	if err != nil {
		return err
	}
	return u.run(ctx)
}

func MaybeHandleBackgroundCheck(args []string) (bool, error) {
	if len(args) != 2 || args[0] != backgroundFlag {
		return false, nil
	}
	u, err := defaultUpdater(io.Discard, io.Discard)
	if err != nil {
		return true, err
	}
	u.currentVersion = args[1]
	return true, u.refreshCache(context.Background())
}

func MaybeNotifyAndCheck(args []string, stderr io.Writer) {
	u, err := defaultUpdater(io.Discard, stderr)
	if err != nil {
		return
	}
	u.maybeNotifyAndCheck(args)
}

func defaultUpdater(stdout, stderr io.Writer) (*updater, error) {
	p, err := paths.New()
	if err != nil {
		return nil, fmt.Errorf("resolve paths: %w", err)
	}
	execPath, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve executable: %w", err)
	}
	return &updater{
		appName:         appName,
		repo:            repoName,
		currentVersion:  buildinfo.CurrentVersion(),
		platform:        platformSpec{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH},
		apiBaseURL:      githubAPIBaseURL,
		httpClient:      &http.Client{Timeout: 30 * time.Second},
		cachePath:       p.UpdateCheckFile(),
		executablePath:  execPath,
		stdout:          stdout,
		stderr:          stderr,
		now:             time.Now,
		spawnBackground: defaultSpawnBackground,
		resetDaemon: func() error {
			return defaultResetDaemon(p)
		},
	}, nil
}

func parseVersion(raw string) (semVersion, error) {
	trimmed := strings.TrimSpace(raw)
	trimmed = strings.TrimPrefix(trimmed, "v")
	if trimmed == "" {
		return semVersion{}, fmt.Errorf("parse version %q: empty", raw)
	}

	trimmed, _, _ = strings.Cut(trimmed, "+")
	core := trimmed
	pre := ""
	if before, after, ok := strings.Cut(trimmed, "-"); ok {
		core = before
		pre = after
	}

	parts := strings.Split(core, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return semVersion{}, fmt.Errorf("parse version %q: invalid core", raw)
	}

	v := semVersion{}
	for len(parts) < 3 {
		parts = append(parts, "0")
	}
	ints := []*int{&v.major, &v.minor, &v.patch}
	for i, part := range parts {
		if part == "" {
			return semVersion{}, fmt.Errorf("parse version %q: empty numeric segment", raw)
		}
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return semVersion{}, fmt.Errorf("parse version %q: invalid numeric segment %q", raw, part)
		}
		*ints[i] = n
	}

	if pre != "" {
		idents := strings.Split(pre, ".")
		for _, ident := range idents {
			if ident == "" {
				return semVersion{}, fmt.Errorf("parse version %q: empty prerelease segment", raw)
			}
			v.prerelease = append(v.prerelease, ident)
		}
	}

	return v, nil
}

func isDevVersion(version string) bool {
	if version == "" || version == "dev" {
		return true
	}
	_, err := parseVersion(version)
	return err != nil
}

func (v semVersion) compare(other semVersion) int {
	if diff := cmpInt(v.major, other.major); diff != 0 {
		return diff
	}
	if diff := cmpInt(v.minor, other.minor); diff != 0 {
		return diff
	}
	if diff := cmpInt(v.patch, other.patch); diff != 0 {
		return diff
	}

	if len(v.prerelease) == 0 && len(other.prerelease) == 0 {
		return 0
	}
	if len(v.prerelease) == 0 {
		return 1
	}
	if len(other.prerelease) == 0 {
		return -1
	}

	for i := 0; i < len(v.prerelease) && i < len(other.prerelease); i++ {
		if diff := comparePrereleaseIdentifier(v.prerelease[i], other.prerelease[i]); diff != 0 {
			return diff
		}
	}
	return cmpInt(len(v.prerelease), len(other.prerelease))
}

func comparePrereleaseIdentifier(a, b string) int {
	ai, aerr := strconv.Atoi(a)
	bi, berr := strconv.Atoi(b)
	switch {
	case aerr == nil && berr == nil:
		return cmpInt(ai, bi)
	case aerr == nil:
		return -1
	case berr == nil:
		return 1
	default:
		return cmpInt(strings.Compare(a, b), 0)
	}
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func releaseArchiveName(app, version string, platform platformSpec) string {
	ext := ".tar.gz"
	if platform.GOOS == "windows" {
		ext = ".zip"
	}
	return fmt.Sprintf("%s-%s-%s-%s%s", app, version, platform.GOOS, platform.GOARCH, ext)
}

func binaryName(app string, platform platformSpec) string {
	if platform.GOOS == "windows" {
		return app + ".exe"
	}
	return app
}

func pickReleaseAssets(app, tag string, assets []releaseAsset, platform platformSpec) (releaseAsset, releaseAsset, error) {
	archiveName := releaseArchiveName(app, tag, platform)
	var archive releaseAsset
	var checksums releaseAsset
	for _, asset := range assets {
		switch asset.Name {
		case archiveName:
			archive = asset
		case checksumsAssetName:
			checksums = asset
		}
	}
	if archive.Name == "" {
		return releaseAsset{}, releaseAsset{}, fmt.Errorf("release asset not found: %s", archiveName)
	}
	if checksums.Name == "" {
		return releaseAsset{}, releaseAsset{}, fmt.Errorf("release asset not found: %s", checksumsAssetName)
	}
	return archive, checksums, nil
}

func (u *updater) checkLatest(ctx context.Context) (*releasePlan, error) {
	release, err := u.fetchLatestRelease(ctx)
	if err != nil {
		return nil, err
	}
	plan := &releasePlan{
		CurrentVersion: u.currentVersion,
		LatestVersion:  release.TagName,
	}
	cmp, err := compareVersions(u.currentVersion, release.TagName)
	if err != nil {
		return nil, err
	}
	if cmp >= 0 {
		return plan, nil
	}
	archive, checksums, err := pickReleaseAssets(u.appName, release.TagName, release.Assets, u.platform)
	if err != nil {
		return nil, err
	}
	plan.UpdateAvailable = true
	plan.ArchiveName = releaseArchiveName(u.appName, release.TagName, u.platform)
	plan.Archive = archive
	plan.Checksums = checksums
	return plan, nil
}

func (u *updater) refreshCache(ctx context.Context) error {
	plan, err := u.checkLatest(ctx)
	if err != nil {
		return err
	}
	return writeCache(u.cachePath, &checkCache{
		CheckedAt:     u.now(),
		LatestVersion: plan.LatestVersion,
	})
}

func (u *updater) maybeNotifyAndCheck(args []string) {
	if u.disableBackground || isDevVersion(u.currentVersion) || os.Getenv(noUpdateCheckEnv) == "1" {
		return
	}
	if len(args) > 0 && (args[0] == "update" || args[0] == backgroundFlag) {
		return
	}
	cache := readCache(u.cachePath)
	if cache != nil {
		cmp, err := compareVersions(u.currentVersion, cache.LatestVersion)
		if err == nil && cmp < 0 {
			fmt.Fprintf(u.stderrWriter(), "%sA new version of %s is available: %s -> %s\nRun \"%s update\" to update%s\n", u.yellow(), u.appName, u.currentVersion, cache.LatestVersion, u.appName, u.reset())
		}
	}
	if cacheStale(cache, u.currentVersion, u.now()) && u.spawnBackground != nil {
		_ = u.spawnBackground(u.currentVersion)
	}
}

func (u *updater) run(ctx context.Context) error {
	if isDevVersion(u.currentVersion) {
		fmt.Fprintf(u.stdoutWriter(), "self-update unavailable for development builds (%s)\n", u.currentVersion)
		return nil
	}
	plan, err := u.checkLatest(ctx)
	if err != nil {
		return err
	}
	if err := writeCache(u.cachePath, &checkCache{CheckedAt: u.now(), LatestVersion: plan.LatestVersion}); err != nil {
		return err
	}
	if !plan.UpdateAvailable {
		fmt.Fprintf(u.stdoutWriter(), "%s is already up to date (%s)\n", u.appName, u.currentVersion)
		return nil
	}

	archiveData, err := u.downloadAsset(ctx, plan.Archive.BrowserDownloadURL, maxDownloadSize)
	if err != nil {
		return err
	}
	checksumsData, err := u.downloadAsset(ctx, plan.Checksums.BrowserDownloadURL, maxDownloadSize)
	if err != nil {
		return err
	}
	checksums, err := parseChecksums(checksumsData)
	if err != nil {
		return err
	}
	want, ok := checksums[plan.ArchiveName]
	if !ok {
		return fmt.Errorf("checksum not found for %s", plan.ArchiveName)
	}
	if err := verifyChecksum(archiveData, want); err != nil {
		return err
	}
	binaryData, err := u.extractBinary(archiveData)
	if err != nil {
		return err
	}
	if err := replaceExecutable(u.executablePath, binaryData); err != nil {
		return err
	}
	if u.resetDaemon != nil {
		if err := u.resetDaemon(); err != nil {
			var resetErr *daemonResetError
			if errors.As(err, &resetErr) && resetErr.daemonOffline {
				return fmt.Errorf("updated %s to %s, but daemon is offline: %w", u.appName, plan.LatestVersion, err)
			}
			return fmt.Errorf("updated %s to %s, but failed to reset daemon: %w", u.appName, plan.LatestVersion, err)
		}
	}
	fmt.Fprintf(u.stdoutWriter(), "updated %s from %s to %s\n", u.appName, u.currentVersion, plan.LatestVersion)
	return nil
}

func defaultResetDaemon(p *paths.Paths) error {
	if p == nil {
		return nil
	}
	alive, err := daemonIsRunning(p)
	if err == nil && !alive && !daemonArtifactsExist(p) {
		return nil
	}
	if err := daemonStop(p); err != nil {
		return fmt.Errorf("stop daemon: %w", err)
	}
	if err := daemonStart(p); err != nil {
		running, checkErr := daemonIsRunning(p)
		offline := checkErr == nil && !running
		return &daemonResetError{err: fmt.Errorf("start daemon: %w", err), daemonOffline: offline}
	}
	return nil
}

func daemonArtifactsExist(p *paths.Paths) bool {
	for _, path := range []string{p.Socket(), p.PIDFile()} {
		if _, err := os.Stat(path); err == nil {
			return true
		}
	}
	return false
}

func (u *updater) fetchLatestRelease(ctx context.Context) (*releaseResponse, error) {
	if u.httpClient == nil {
		u.httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(u.apiBaseURL, "/")+"/repos/"+u.repo+"/releases/latest", nil)
	if err != nil {
		return nil, fmt.Errorf("build release request: %w", err)
	}
	resp, err := u.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch latest release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch latest release: unexpected status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIResponseSize+1))
	if err != nil {
		return nil, fmt.Errorf("read latest release: %w", err)
	}
	if len(body) > maxAPIResponseSize {
		return nil, fmt.Errorf("latest release response exceeds %d bytes", maxAPIResponseSize)
	}
	var release releaseResponse
	if err := json.Unmarshal(body, &release); err != nil {
		return nil, fmt.Errorf("parse latest release: %w", err)
	}
	if release.TagName == "" {
		return nil, fmt.Errorf("latest release missing tag_name")
	}
	return &release, nil
}

func (u *updater) downloadAsset(ctx context.Context, assetURL string, limit int64) ([]byte, error) {
	if err := ensureHTTPS(assetURL); err != nil {
		return nil, err
	}
	if u.httpClient == nil {
		u.httpClient = &http.Client{Timeout: 5 * time.Minute}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, assetURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build download request: %w", err)
	}
	resp, err := u.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download asset: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download asset: unexpected status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read asset: %w", err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("download exceeds %d bytes", limit)
	}
	return body, nil
}

func (u *updater) extractBinary(archive []byte) ([]byte, error) {
	name := binaryName(u.appName, u.platform)
	if u.platform.GOOS == "windows" {
		return extractBinaryFromZip(archive, name)
	}
	return extractBinaryFromTarGz(archive, name)
}

func replaceExecutable(target string, binaryData []byte) error {
	resolved, err := filepath.EvalSymlinks(target)
	if err == nil {
		target = resolved
	}
	info, err := os.Stat(target)
	if err != nil {
		return fmt.Errorf("stat executable: %w", err)
	}
	perm := info.Mode().Perm()
	if err := replaceExecutableAtomically(target, binaryData, perm); err == nil {
		removeQuarantine(target)
		return nil
	} else if runtime.GOOS == "darwin" {
		return fmt.Errorf("self-update requires an atomic replace on macOS; reinstall no-mistakes so the PATH entry points at a user-owned binary, then retry update: %w", err)
	}
	if err := overwriteExecutable(target, binaryData, perm); err != nil {
		return err
	}
	removeQuarantine(target)
	return nil
}

func replaceExecutableAtomically(target string, binaryData []byte, perm os.FileMode) error {
	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, filepath.Base(target)+"-new-*")
	if err != nil {
		return fmt.Errorf("create temp executable: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()
	if _, err := tmp.Write(binaryData); err != nil {
		return fmt.Errorf("write temp executable: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp executable: %w", err)
	}
	if err := os.Chmod(tmpPath, perm); err != nil {
		return fmt.Errorf("chmod temp executable: %w", err)
	}
	if err := os.Rename(tmpPath, target); err != nil {
		return fmt.Errorf("rename temp executable: %w", err)
	}
	return nil
}

func overwriteExecutable(path string, binaryData []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return fmt.Errorf("overwrite executable: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(binaryData); err != nil {
		return fmt.Errorf("overwrite executable: %w", err)
	}
	if err := f.Chmod(perm); err != nil {
		return fmt.Errorf("chmod executable: %w", err)
	}
	return nil
}

func defaultSpawnBackground(currentVersion string) error {
	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("open null device: %w", err)
	}
	defer devNull.Close()

	cmd := exec.Command(execPath, backgroundFlag, currentVersion)
	cmd.Env = append(os.Environ(), noUpdateCheckEnv+"=1")
	cmd.Stdout = devNull
	cmd.Stderr = devNull
	cmd.Stdin = nil
	cmd.SysProcAttr = detachedProcAttr()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start background update check: %w", err)
	}
	return nil
}

func (u *updater) stdoutWriter() io.Writer {
	if u.stdout == nil {
		return io.Discard
	}
	return u.stdout
}

func (u *updater) stderrWriter() io.Writer {
	if u.stderr == nil {
		return io.Discard
	}
	return u.stderr
}

func (u *updater) yellow() string {
	if u.noColor {
		return ""
	}
	return "\033[33m"
}

func (u *updater) reset() string {
	if u.noColor {
		return ""
	}
	return "\033[0m"
}

func writeCache(path string, entry *checkCache) error {
	if entry == nil {
		return fmt.Errorf("write cache: nil entry")
	}
	if path == "" {
		return nil
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("write cache: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("write cache dir: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write cache file: %w", err)
	}
	return nil
}

func readCache(path string) *checkCache {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var entry checkCache
	if err := json.Unmarshal(data, &entry); err != nil {
		return nil
	}
	if entry.LatestVersion == "" {
		return nil
	}
	return &entry
}

func cacheStale(entry *checkCache, currentVersion string, now time.Time) bool {
	if entry == nil {
		return true
	}
	if now.Sub(entry.CheckedAt) > cacheTTL {
		return true
	}
	cmp, err := compareVersions(currentVersion, entry.LatestVersion)
	if err != nil {
		return true
	}
	return cmp >= 0
}

func parseChecksums(data []byte) (map[string]string, error) {
	checksums := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) != 2 {
			return nil, fmt.Errorf("parse checksums: malformed line %q", line)
		}
		checksums[parts[1]] = parts[0]
	}
	return checksums, nil
}

func verifyChecksum(data []byte, want string) error {
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	if got != want {
		return fmt.Errorf("checksum mismatch: got %s want %s", got, want)
	}
	return nil
}

func ensureHTTPS(rawURL string) error {
	if allowInsecureDownloads {
		return nil
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("parse url: %w", err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("reject non-https url: %s", rawURL)
	}
	return nil
}

func extractBinaryFromTarGz(archive []byte, binaryName string) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("open tar.gz: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read tar.gz: %w", err)
		}
		if filepath.Base(hdr.Name) != binaryName {
			continue
		}
		return readExtractedBinary(tr)
	}
	return nil, fmt.Errorf("binary not found in tar.gz: %s", binaryName)
}

func extractBinaryFromZip(archive []byte, binaryName string) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return nil, fmt.Errorf("open zip: %w", err)
	}
	for _, file := range zr.File {
		if filepath.Base(file.Name) != binaryName {
			continue
		}
		rc, err := file.Open()
		if err != nil {
			return nil, fmt.Errorf("open zip entry: %w", err)
		}
		defer rc.Close()
		return readExtractedBinary(rc)
	}
	return nil, fmt.Errorf("binary not found in zip: %s", binaryName)
}

func readExtractedBinary(r io.Reader) ([]byte, error) {
	limited := io.LimitReader(r, maxExtractedSize+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read extracted binary: %w", err)
	}
	if len(data) > maxExtractedSize {
		return nil, fmt.Errorf("extracted binary exceeds %d bytes", maxExtractedSize)
	}
	return data, nil
}
