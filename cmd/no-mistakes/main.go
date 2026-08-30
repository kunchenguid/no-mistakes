package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/cli"
	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/telemetry"
	"github.com/kunchenguid/no-mistakes/internal/update"
)

var cleanupOldExecutable = update.CleanupOldExecutable
var maybeHandleBackgroundCheck = update.MaybeHandleBackgroundCheck
var maybeNotifyAndCheck = update.MaybeNotifyAndCheck

func main() {
	os.Exit(run())
}

func run() int {
	if invocation, handled, err := publicationConfinementDetachedChildFromArgs(os.Args[1:]); handled {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		if err := agent.RunPublicationConfinementDetachedChild(invocation.ReadyMarker, invocation.LateMarker, invocation.Delay); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}
	if invocation, handled, err := publicationConfinementCanaryFromArgs(os.Args[1:]); handled {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		if err := runPublicationConfinementCanary(invocation); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}
	_ = cleanupOldExecutable()

	if root, ok, err := daemonLogSinkRootFromArgs(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	} else if ok {
		if err := os.Setenv("NM_HOME", root); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if err := daemon.RunBootstrapLogSink(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}
	if root, ok, err := daemonRunRootFromArgs(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	} else if ok {
		if root != "" {
			if err := os.Setenv("NM_HOME", root); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
		}
		if err := daemon.Run(); err != nil {
			writeDaemonRunError(os.Stderr, err)
			return 1
		}
		return 0
	}

	if handled, err := maybeHandleBackgroundCheck(os.Args[1:]); handled {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}

	if !publicationMachineCommand(os.Args[1:]) {
		maybeNotifyAndCheck(os.Args[1:], os.Stderr)
	}

	// Redirect slog to a file for interactive CLI commands so logs never
	// leak into user-facing output. The daemon process sets up its own
	// file-based logger before reaching this point.
	slog.SetDefault(slog.New(slog.NewTextHandler(cliLogWriter(), nil)))
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
		defer cancel()
		_ = telemetry.Close(ctx)
	}()

	return cli.Execute()
}

type publicationConfinementCanaryInvocation struct {
	ScratchDir  string
	ReadyMarker string
	LateMarker  string
	Delay       time.Duration
}

type publicationConfinementDetachedChildInvocation struct {
	ReadyMarker string
	LateMarker  string
	Delay       time.Duration
}

func publicationConfinementDetachedChildFromArgs(args []string) (publicationConfinementDetachedChildInvocation, bool, error) {
	var invocation publicationConfinementDetachedChildInvocation
	if len(args) == 0 || args[0] != "__publication-confinement-detached-child" {
		return invocation, false, nil
	}
	if len(args) != 7 || args[1] != "--ready" || args[3] != "--late" || args[5] != "--delay-ms" {
		return invocation, true, fmt.Errorf("invalid private detached publication canary arguments")
	}
	for _, value := range []string{args[2], args[4]} {
		if !filepath.IsAbs(value) || filepath.Clean(value) != value {
			return invocation, true, fmt.Errorf("private detached publication canary paths must be clean and absolute")
		}
	}
	delayMS, err := strconv.Atoi(args[6])
	if err != nil || delayMS < 1 || delayMS > 10_000 {
		return invocation, true, fmt.Errorf("private detached publication canary delay is invalid")
	}
	invocation = publicationConfinementDetachedChildInvocation{
		ReadyMarker: args[2], LateMarker: args[4], Delay: time.Duration(delayMS) * time.Millisecond,
	}
	if invocation.ReadyMarker == invocation.LateMarker {
		return publicationConfinementDetachedChildInvocation{}, true, fmt.Errorf("private detached publication canary markers must be distinct")
	}
	return invocation, true, nil
}

func publicationConfinementCanaryFromArgs(args []string) (publicationConfinementCanaryInvocation, bool, error) {
	var invocation publicationConfinementCanaryInvocation
	if len(args) == 0 || args[0] != "__publication-confinement-canary" {
		return invocation, false, nil
	}
	if len(args) != 9 || args[1] != "--scratch" || args[3] != "--ready" || args[5] != "--late" || args[7] != "--delay-ms" {
		return invocation, true, fmt.Errorf("invalid private publication confinement canary arguments")
	}
	for _, value := range []string{args[2], args[4], args[6]} {
		if !filepath.IsAbs(value) || filepath.Clean(value) != value {
			return invocation, true, fmt.Errorf("private publication confinement canary paths must be clean and absolute")
		}
	}
	delayMS, err := strconv.Atoi(args[8])
	if err != nil || delayMS < 1 || delayMS > 10_000 {
		return invocation, true, fmt.Errorf("private publication confinement canary delay is invalid")
	}
	invocation = publicationConfinementCanaryInvocation{
		ScratchDir: args[2], ReadyMarker: args[4], LateMarker: args[6], Delay: time.Duration(delayMS) * time.Millisecond,
	}
	if invocation.ReadyMarker == invocation.LateMarker {
		return publicationConfinementCanaryInvocation{}, true, fmt.Errorf("private publication confinement canary markers must be distinct")
	}
	return invocation, true, nil
}

func runPublicationConfinementCanary(invocation publicationConfinementCanaryInvocation) error {
	raw, err := os.ReadFile(filepath.Join(invocation.ScratchDir, "publication-canary.json"))
	if err != nil {
		return err
	}
	var config agent.PublicationConfinementCanaryConfig
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return fmt.Errorf("decode publication canary config: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("decode publication canary config: trailing data")
	}
	if config.ScratchDir != invocation.ScratchDir || config.ReadyMarker != invocation.ReadyMarker ||
		config.LateMarker != invocation.LateMarker || config.Delay != invocation.Delay {
		return fmt.Errorf("publication canary config does not match bound argv")
	}
	return agent.RunPublicationConfinementCanary(config)
}

func publicationMachineCommand(args []string) bool {
	if len(args) < 2 || args[0] != "publication" {
		return false
	}
	switch args[1] {
	case "start", "authorize", "status":
		return true
	default:
		return false
	}
}

func writeDaemonRunError(stderr *os.File, err error) {
	if errors.Is(err, daemon.ErrSingletonLockHeld) {
		p, pathErr := paths.New()
		if pathErr == nil {
			stderrInfo, stderrErr := stderr.Stat()
			bootstrapInfo, bootstrapErr := os.Stat(p.DaemonBootstrapLog())
			if stderrErr == nil && bootstrapErr == nil && os.SameFile(stderrInfo, bootstrapInfo) {
				return
			}
		}
	}
	fmt.Fprintln(stderr, err)
}

func daemonLogSinkRootFromArgs(args []string) (string, bool, error) {
	if len(args) != 4 || args[0] != "daemon" || args[1] != "log-sink" || args[2] != "--root" {
		return "", false, nil
	}
	if args[3] == "" {
		return "", false, fmt.Errorf("empty value for --root")
	}
	return args[3], true, nil
}

func daemonRunRootFromArgs(args []string) (string, bool, error) {
	if len(args) < 2 || args[0] != "daemon" || args[1] != "run" {
		return "", false, nil
	}
	if len(args) == 2 {
		return "", true, nil
	}
	if len(args) == 3 {
		arg := args[2]
		if arg == "--help" || arg == "-h" {
			return "", false, nil
		}
		if arg == "--root" {
			return "", false, fmt.Errorf("missing value for --root")
		}
		if value, ok := strings.CutPrefix(arg, "--root="); ok {
			return value, true, nil
		}
		return "", false, nil
	}
	if len(args) == 4 && args[2] == "--root" {
		return args[3], true, nil
	}
	return "", false, nil
}

// cliLogWriter returns a writer for CLI logs. Falls back to io.Discard
// if the log file cannot be opened (e.g. before first init).
func cliLogWriter() io.Writer {
	p, err := paths.New()
	if err != nil {
		return io.Discard
	}
	f, err := os.OpenFile(p.CLILog(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return io.Discard
	}
	return f
}
