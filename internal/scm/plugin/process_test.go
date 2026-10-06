package plugin

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// These tests spawn a real process (this test binary re-executed as the
// plugin) to cover the parts the transport seam skips: argv delivery, empty
// stdin, exit status, and the per-invocation timeout. Keep them few: each
// spawn of a race-instrumented test binary costs most of a second.

func helperFactory(mode string) CmdFactory {
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, os.Args[0], append([]string{"-test.run=TestProviderPluginHelperProcess", "--", name}, args...)...)
		cmd.Env = append(os.Environ(), "NM_PROVIDER_PLUGIN_HELPER="+mode)
		return cmd
	}
}

func TestProviderPluginHelperProcess(t *testing.T) {
	mode := os.Getenv("NM_PROVIDER_PLUGIN_HELPER")
	if mode == "" {
		return
	}
	stdin, _ := io.ReadAll(os.Stdin)
	switch mode {
	case "echo":
		// Prove the configured args lead, the subcommand and context flags
		// follow, --json ends the argv, and stdin is empty.
		got := strings.Join(os.Args[3:], " ")
		want := "nm-ssm --profile work status --plugin=ssm --repo=team/repo --host=git.example.com --raw-host=alias --remote-url=https://git.example.com/team/repo.git --json"
		if got != want || len(stdin) != 0 {
			os.Stdout.WriteString(`{"error":{"message":"unexpected invocation"}}`)
			os.Stderr.WriteString("argv: " + got + "\nstdin: " + string(stdin))
			os.Exit(1)
		}
		os.Stdout.WriteString(`{"protocol_version":1,"max_pr_body_chars":42}`)
		os.Exit(0)
	case "stderr":
		os.Stderr.WriteString("token https://u:hunter2@example.com rejected")
		os.Exit(4)
	case "sleep":
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
	os.Exit(2)
}

func TestProcess_DeliversArgvWithEmptyStdin(t *testing.T) {
	host := New(Options{
		Name: "ssm", Executable: "nm-ssm", Args: []string{"--profile", "work"},
		Repository:     Repository{RemoteURL: "https://git.example.com/team/repo.git", Host: "git.example.com", RawHost: "alias", Path: "team/repo"},
		CommandFactory: helperFactory("echo"),
	})
	if err := host.Available(context.Background()); err != nil {
		t.Fatalf("Available: %v", err)
	}
	if host.MaxPRBodyChars() != 42 {
		t.Fatalf("MaxPRBodyChars = %d", host.MaxPRBodyChars())
	}
}

func TestProcess_ExitWithoutFailureDocumentReportsRedactedStderr(t *testing.T) {
	host := New(Options{Name: "ssm", Executable: "nm-ssm", CommandFactory: helperFactory("stderr")})
	err := host.Available(context.Background())
	if err == nil || !strings.Contains(err.Error(), "exit status 4") || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("Available error = %v", err)
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("error leaked a credential: %v", err)
	}
}

func TestProcess_TimeoutKillsThePluginAndFailsClosed(t *testing.T) {
	host := New(Options{Name: "ssm", Executable: "nm-ssm", Timeout: 500 * time.Millisecond, CommandFactory: helperFactory("sleep")})
	start := time.Now()
	err := host.Available(context.Background())
	if err == nil || !strings.Contains(err.Error(), "timed out after 500ms") {
		t.Fatalf("Available error = %v", err)
	}
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("a timeout must be a protocol violation: %v", err)
	}
	// The CI monitor's repeated reads retry exactly this case.
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("a timeout must be distinguishable from other violations: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("timeout took %s", elapsed)
	}
}
