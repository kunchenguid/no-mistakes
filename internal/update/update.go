package update

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
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
var currentGOOS = runtime.GOOS
var daemonIsRunning = daemon.IsRunning
var daemonExecutablePath = runningDaemonExecutablePath
var daemonStop = daemon.Stop
var daemonStart = daemon.Start
var windowsExecutablePathForPID = defaultWindowsExecutablePathForPID

type platformSpec struct {
	GOOS   string
	GOARCH string
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
	paths             *paths.Paths
	disableBackground bool
	noColor           bool
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

func CachedLatestVersion() string {
	u, err := defaultUpdater(io.Discard, io.Discard)
	if err != nil {
		return ""
	}
	return u.cachedLatestVersion()
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
		paths:           p,
		spawnBackground: defaultSpawnBackground,
		resetDaemon: func() error {
			return defaultResetDaemon(p)
		},
	}, nil
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

func (u *updater) cachedLatestVersion() string {
	if u == nil || u.disableBackground || isDevVersion(u.currentVersion) || os.Getenv(noUpdateCheckEnv) == "1" {
		return ""
	}
	cache := readCache(u.cachePath)
	if cache == nil {
		return ""
	}
	cmp, err := compareVersions(u.currentVersion, cache.LatestVersion)
	if err != nil || cmp >= 0 {
		return ""
	}
	return cache.LatestVersion
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
	if err := u.ensureDaemonUsesCurrentExecutable(); err != nil {
		return err
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
