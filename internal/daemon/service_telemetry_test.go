package daemon

import (
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/telemetry"
)

const serviceTelemetryKey = "NO_MISTAKES_TELEMETRY"

// Decode the generated definitions independently of the inheritance allowlist.
func serviceTelemetryEnvironment(t *testing.T, platform, definition string) map[string]string {
	t.Helper()
	env := make(map[string]string)
	if platform == "linux" {
		for _, line := range strings.Split(definition, "\n") {
			if !strings.HasPrefix(line, "Environment=") {
				continue
			}
			assignment, err := strconv.Unquote(strings.TrimPrefix(line, "Environment="))
			if err != nil {
				t.Fatal(err)
			}
			key, value, ok := strings.Cut(strings.ReplaceAll(assignment, "%%", "%"), "=")
			if !ok {
				t.Fatalf("invalid environment assignment %q", assignment)
			}
			env[key] = value
		}
		return env
	}
	var plist struct {
		Dict struct {
			Keys  []string `xml:"key"`
			Dicts []struct {
				Keys   []string `xml:"key"`
				Values []string `xml:"string"`
			} `xml:"dict"`
		} `xml:"dict"`
	}
	if err := xml.Unmarshal([]byte(definition), &plist); err != nil {
		t.Fatal(err)
	}
	if len(plist.Dict.Dicts) != 1 {
		t.Fatalf("expected one environment dictionary, got %d", len(plist.Dict.Dicts))
	}
	dict := plist.Dict.Dicts[0]
	if len(dict.Keys) != len(dict.Values) {
		t.Fatal("environment keys and values do not pair")
	}
	for i, key := range dict.Keys {
		env[key] = dict.Values[i]
	}
	return env
}

func TestManagedServiceTelemetry(t *testing.T) {
	for _, platform := range []string{"darwin", "linux"} {
		for _, value := range []string{"", "0", "false", "off", "1", "a\"b\\c&<%s"} {
			t.Run(platform+"/"+value, func(t *testing.T) {
				defer stubServiceRuntime(t)()
				runtimeGOOS = platform
				home := t.TempDir()
				p := paths.WithRoot(t.TempDir())
				serviceUserHomeDir = func() (string, error) { return home, nil }
				serviceExecutablePath = func() (string, error) { return "/bin/no-mistakes", nil }
				serviceCommandRunner = func(string, ...string) ([]byte, error) { return nil, nil }
				for _, key := range proxyEnvKeys {
					t.Setenv(key, "")
				}
				t.Setenv("HTTPS_PROXY", "http://user:p%40ss&word@proxy.example:8080")
				t.Setenv(serviceTelemetryKey, value)
				if value == "" {
					if err := os.Unsetenv(serviceTelemetryKey); err != nil {
						t.Fatal(err)
					}
				}

				install := installLaunchAgent
				path := launchAgentPath(p)
				if platform == "linux" {
					install = installSystemdUserService
					path = systemdUserServicePath(p)
				}
				if err := install(p, "/bin/no-mistakes"); err != nil {
					t.Fatal(err)
				}
				check := func() map[string]string {
					t.Helper()
					definition, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					env := serviceTelemetryEnvironment(t, platform, string(definition))
					got, present := env[serviceTelemetryKey]
					if got != value || present != (value != "") {
						t.Fatalf("service telemetry = %q, present %v, want %q", got, present, value)
					}
					if env["HTTPS_PROXY"] != "http://user:p%40ss&word@proxy.example:8080" {
						t.Fatalf("proxy changed: %v", env)
					}
					return env
				}
				env := check()
				if value == "0" || value == "false" || value == "off" || value == "" {
					assertServiceTelemetryTransport(t, env, value == "")
				}

				// An env-less restart must see no drift and issue no service commands.
				t.Setenv("HTTPS_PROXY", "")
				t.Setenv(serviceTelemetryKey, "")
				if err := os.Unsetenv(serviceTelemetryKey); err != nil {
					t.Fatal(err)
				}
				serviceCommandRunner = func(name string, args ...string) ([]byte, error) {
					t.Fatalf("unexpected service command: %s %v", name, args)
					return nil, nil
				}
				if changed, err := reinstallManagedServiceIfChanged(p, nil); err != nil || changed {
					t.Fatalf("env-less restart: changed=%v err=%v", changed, err)
				}
				// A real file refresh, such as an upgrade, must retain both values.
				serviceCommandRunner = func(string, ...string) ([]byte, error) { return nil, nil }
				if err := install(p, "/new/no-mistakes"); err != nil {
					t.Fatal(err)
				}
				check()
				// Changing only telemetry must not erase inherited proxies.
				t.Setenv(serviceTelemetryKey, "1")
				if err := install(p, "/new/no-mistakes"); err != nil {
					t.Fatal(err)
				}
				value = "1"
				check()
				// Changing only a proxy must not erase inherited telemetry.
				t.Setenv(serviceTelemetryKey, "")
				t.Setenv("HTTPS_PROXY", "http://user:p%40ss&word@proxy.example:8080")
				if err := install(p, "/new/no-mistakes"); err != nil {
					t.Fatal(err)
				}
				check()
			})
		}
	}
}

func assertServiceTelemetryTransport(t *testing.T, env map[string]string, enabled bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestManagedServiceTelemetryTransport$")
	// A fresh process resets telemetry.Default's cache and models the service
	// manager supplying the decoded environment, without launching a daemon.
	cmd.Env = []string{
		"NM_SERVICE_TELEMETRY_HELPER=1",
		"NM_SERVICE_TELEMETRY_ENABLED=" + strconv.FormatBool(enabled),
		"NO_MISTAKES_UMAMI_HOST=https://telemetry.invalid",
		"NO_MISTAKES_UMAMI_WEBSITE_ID=synthetic-test",
		"NM_HOME=" + t.TempDir(),
		"HOME=" + t.TempDir(),
	}
	for key, value := range env {
		if key != "HOME" {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	cmd.Dir = t.TempDir()
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fake daemon telemetry: %v\n%s", err, output)
	}
}

func TestManagedServiceTelemetryStartup(t *testing.T) {
	for _, platform := range []string{"darwin", "linux"} {
		for _, value := range []string{"0", "false", "off"} {
			for _, direction := range []string{"saved_opt_out", "shell_opt_out"} {
				t.Run(platform+"/"+value+"/"+direction, func(t *testing.T) {
					defer stubServiceRuntime(t)()
					runtimeGOOS = platform
					home := t.TempDir()
					serviceUserHomeDir = func() (string, error) { return home, nil }
					serviceCommandRunner = func(string, ...string) ([]byte, error) { return nil, nil }
					for _, key := range proxyEnvKeys {
						t.Setenv(key, "")
					}
					saved, shell := value, "1"
					if direction == "shell_opt_out" {
						saved, shell = "1", value
					}
					t.Setenv(serviceTelemetryKey, saved)
					p := paths.WithRoot(t.TempDir())
					install := installLaunchAgent
					path := launchAgentPath(p)
					if platform == "linux" {
						install = installSystemdUserService
						path = systemdUserServicePath(p)
					}
					if err := install(p, "/bin/no-mistakes"); err != nil {
						t.Fatal(err)
					}
					// Refresh from the saved definition without an installer env value.
					t.Setenv(serviceTelemetryKey, "")
					if err := install(p, "/new/no-mistakes"); err != nil {
						t.Fatal(err)
					}
					definition, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					env := serviceTelemetryEnvironment(t, platform, string(definition))
					if env[serviceTelemetryKey] != saved {
						t.Fatalf("refreshed telemetry = %q, want %q", env[serviceTelemetryKey], saved)
					}
					env["NM_SERVICE_TELEMETRY_PREPARE"] = "1"
					env["NM_SERVICE_TELEMETRY_SHELL"] = shell
					assertServiceTelemetryTransport(t, env, false)
				})
			}
		}
	}
	for _, value := range []string{"", "1"} {
		t.Run("enabled_control/"+value, func(t *testing.T) {
			assertServiceTelemetryTransport(t, map[string]string{
				serviceTelemetryKey:            value,
				"NM_SERVICE_TELEMETRY_PREPARE": "1",
				"NM_SERVICE_TELEMETRY_SHELL":   "1",
			}, true)
		})
	}
}

type serviceTelemetryTransport struct{ events atomic.Int32 }

func (r *serviceTelemetryTransport) RoundTrip(*http.Request) (*http.Response, error) {
	r.events.Add(1)
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header)}, nil
}

func TestManagedServiceTelemetryTransport(t *testing.T) {
	if os.Getenv("NM_SERVICE_TELEMETRY_HELPER") != "1" {
		t.Skip("invoked in a fresh process with a decoded service environment")
	}
	transport := &serviceTelemetryTransport{}
	http.DefaultTransport = transport
	if os.Getenv("NM_SERVICE_TELEMETRY_PREPARE") == "1" {
		oldApply := applyShellEnvToProcess
		defer func() { applyShellEnvToProcess = oldApply }()
		applied := false
		applyShellEnvToProcess = func(excluded ...string) error {
			applied = true
			for _, key := range excluded {
				if key == serviceTelemetryKey {
					return nil
				}
			}
			return os.Setenv(serviceTelemetryKey, os.Getenv("NM_SERVICE_TELEMETRY_SHELL"))
		}
		if err := prepareDaemonEnvironment(); err != nil {
			t.Fatal(err)
		}
		if !applied {
			t.Fatal("startup did not apply the shell environment")
		}
	}
	for _, event := range []string{"run started", "step_completed", "approval", "fix", "run finished"} {
		telemetry.Track(event, nil)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := telemetry.Close(ctx); err != nil {
		t.Fatal(err)
	}
	want := int32(0)
	if os.Getenv("NM_SERVICE_TELEMETRY_ENABLED") == "true" {
		want = 5
	}
	if got := transport.events.Load(); got != want {
		t.Fatalf("transport recorded %d events, want %d", got, want)
	}
}
