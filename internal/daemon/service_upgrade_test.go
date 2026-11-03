package daemon

import (
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func TestStopThenStartRefreshesManagedExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX service definitions")
	}
	for _, platform := range []string{"darwin", "linux"} {
		t.Run(platform, func(t *testing.T) {
			cleanup := stubServiceRuntime(t)
			defer cleanup()
			runtimeGOOS = platform
			p := paths.WithRoot(filepath.Join(t.TempDir(), "nm-home"))
			if err := p.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			home := t.TempDir()
			serviceUserHomeDir = func() (string, error) { return home, nil }
			serviceCurrentUser = func() (*user.User, error) { return &user.User{Uid: "501"}, nil }
			const current = "/opt/new/no-mistakes"
			const previous = "/opt/old/no-mistakes"
			serviceExecutablePath = func() (string, error) { return current, nil }
			file, content := systemdUserServicePath(p), renderSystemdUnit(previous, p, home)
			if platform == "darwin" {
				file, content = launchAgentPath(p), renderLaunchAgent(previous, p, home)
			}
			if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			running, stopped, started := true, false, false
			daemonHealthCheck = func(*paths.Paths) (bool, error) { return running, nil }
			serviceCommandRunner = func(name string, args ...string) ([]byte, error) {
				command := name + " " + strings.Join(args, " ")
				if strings.Contains(command, "launchctl bootout ") || command == "systemctl --user stop "+systemdServiceName(p) {
					running, stopped = false, true
				}
				if strings.Contains(command, "launchctl kickstart ") || command == "systemctl --user start "+systemdServiceName(p) {
					if !stopped {
						t.Fatal("new daemon started before stopping old daemon")
					}
					data, err := os.ReadFile(file)
					if err != nil {
						t.Fatal(err)
					}
					if !strings.Contains(string(data), current) || strings.Contains(string(data), previous) {
						t.Fatalf("service started with stale executable:\n%s", data)
					}
					running, started = true, true
				}
				return nil, nil
			}
			if err := Stop(p); err != nil {
				t.Fatal(err)
			}
			if !stopped || running {
				t.Fatal("old managed daemon was not stopped")
			}
			if err := Start(p); err != nil {
				t.Fatal(err)
			}
			if !started || !running {
				t.Fatal("new managed daemon was not started")
			}
		})
	}
}
