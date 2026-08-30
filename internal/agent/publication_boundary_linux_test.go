//go:build linux && publication_confinement

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "__publication-confinement-detached-child" {
		if len(os.Args) != 8 || os.Args[2] != "--ready" || os.Args[4] != "--late" || os.Args[6] != "--delay-ms" || os.Args[7] != "800" {
			fmt.Fprintln(os.Stderr, "invalid private detached publication canary invocation")
			os.Exit(2)
		}
		for _, path := range []string{os.Args[3], os.Args[5]} {
			if !filepath.IsAbs(path) || filepath.Clean(path) != path {
				fmt.Fprintln(os.Stderr, "invalid private detached publication canary path")
				os.Exit(2)
			}
		}
		if err := RunPublicationConfinementDetachedChild(os.Args[3], os.Args[5], 800*time.Millisecond); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Println("codex-cli 0.150.1-test")
		os.Exit(0)
	}
	if len(os.Args) > 1 && os.Args[1] == "exec" {
		transientFailure := false
		for index := 2; index+1 < len(os.Args); index++ {
			if os.Args[index] == "--test-marker" || os.Args[index] == "--test-transient-marker" {
				file, err := os.OpenFile(os.Args[index+1], os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
				if err != nil {
					fmt.Fprintln(os.Stderr, err)
					os.Exit(1)
				}
				_, _ = file.WriteString("x")
				_ = file.Close()
				transientFailure = os.Args[index] == "--test-transient-marker"
				break
			}
		}
		if transientFailure {
			fmt.Fprintln(os.Stderr, "transient upstream failure")
			os.Exit(1)
		}
		fmt.Println(`{"type":"item.completed","item":{"type":"agent_message","text":"ok"}}`)
		fmt.Println(`{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}`)
		os.Exit(0)
	}
	if len(os.Args) > 1 && os.Args[1] == "__publication-confinement-canary" {
		if len(os.Args) != 10 || os.Args[2] != "--scratch" || os.Args[4] != "--ready" ||
			os.Args[6] != "--late" || os.Args[8] != "--delay-ms" || os.Args[9] != "800" {
			fmt.Fprintln(os.Stderr, "invalid private publication canary invocation")
			os.Exit(2)
		}
		for _, path := range []string{os.Args[3], os.Args[5], os.Args[7]} {
			if !filepath.IsAbs(path) || filepath.Clean(path) != path {
				fmt.Fprintln(os.Stderr, "invalid private publication canary path")
				os.Exit(2)
			}
		}
		raw, err := os.ReadFile(filepath.Join(os.Args[3], "publication-canary.json"))
		if err == nil {
			var config PublicationConfinementCanaryConfig
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.DisallowUnknownFields()
			err = decoder.Decode(&config)
			if err == nil {
				var extra any
				if trailing := decoder.Decode(&extra); trailing != io.EOF {
					err = fmt.Errorf("trailing private publication canary config")
				}
			}
			if err == nil && (config.ScratchDir != os.Args[3] || config.ReadyMarker != os.Args[5] ||
				config.LateMarker != os.Args[7] || config.Delay != 800*time.Millisecond) {
				err = fmt.Errorf("private publication canary binding mismatch")
			}
			if err == nil {
				err = RunPublicationConfinementCanary(config)
			}
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestPublicationCodexBoundaryLinuxNamespaceOwnsDetachedDescendantLifetime(t *testing.T) {
	boundary, err := DiscoverProductionPublicationCodexBoundary(context.Background(), filepath.Join(t.TempDir(), "bootstrap"))
	if err != nil {
		t.Fatalf("discover exact installed Linux boundary: %v", err)
	}
	container := t.TempDir()
	candidate := filepath.Join(container, "candidate")
	scratch := filepath.Join(container, "scratch")
	sibling := filepath.Join(container, "sibling")
	source := t.TempDir()
	for _, dir := range []string{candidate, scratch, sibling} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(candidate, "readable.txt"), []byte("ok\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	sourceFile := filepath.Join(source, "secret")
	siblingFile := filepath.Join(sibling, "secret")
	for _, path := range []string{sourceFile, siblingFile} {
		if err := os.WriteFile(path, []byte("secret\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tcpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcpListener.Close()
	// Keep the socket inside the granted scratch tree so this is an
	// independent AF_UNIX negative, not a consequence of source denial.
	unixPath := filepath.Join(scratch, "probe.sock")
	unixListener, err := net.Listen("unix", unixPath)
	if err != nil {
		t.Fatal(err)
	}
	defer unixListener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	report, err := boundary.RunLinuxLifecycleCanary(ctx, PublicationLinuxLifecycleCanaryOptions{
		CandidateDir: candidate, SourceDir: source, ScratchDir: scratch,
		SourceFile: sourceFile, SiblingFile: siblingFile,
		TCPAddress: tcpListener.Addr().String(), UnixSocketPath: unixPath,
	})
	if err != nil {
		t.Fatalf("non-skipped Linux namespace canary: %v", err)
	}
	if !report.ChildObservedAlive || !report.WrapperExitedWithinBound || !report.NamespaceEmpty ||
		report.ChildStillExists || report.LateMarkerExists || report.SentinelSignalled {
		t.Fatalf("Linux namespace teardown report=%#v", report)
	}
	for name, command := range map[string]string{"fast-zero": "exit 0", "fast-nonzero": "exit 17"} {
		t.Run(name, func(t *testing.T) {
			fastScratch := filepath.Join(container, name)
			if err := os.Mkdir(fastScratch, 0o700); err != nil {
				t.Fatal(err)
			}
			view, err := boundary.BindView(candidate, source, fastScratch)
			if err != nil {
				t.Fatal(err)
			}
			_, exitCode, err := view.RunConfiguredCommand(context.Background(), command)
			if err != nil {
				t.Fatalf("fast configured command: %v", err)
			}
			want := 0
			if name == "fast-nonzero" {
				want = 17
			}
			if exitCode != want {
				t.Fatalf("exit=%d, want %d", exitCode, want)
			}
		})
	}
}

func TestPublicationNamespacePIDChainParserFailsClosed(t *testing.T) {
	chain, err := parseLinuxNamespacePIDChain([]byte("Name:\tbwrap\nNSpid:\t901 44 1\n"))
	if err != nil || len(chain) != 3 || chain[0] != 901 || chain[2] != 1 {
		t.Fatalf("positive NSpid chain=%v err=%v", chain, err)
	}
	for name, raw := range map[string]string{
		"missing":   "Name:\tbwrap\n",
		"empty":     "NSpid:\t\n",
		"malformed": "NSpid:\t901 nope 1\n",
		"zero":      "NSpid:\t901 0 1\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseLinuxNamespacePIDChain([]byte(raw)); err == nil {
				t.Fatalf("malformed NSpid bytes %q were accepted", raw)
			}
		})
	}
	for name, chain := range map[string][]int{
		"single-level":   {901},
		"first-mismatch": {900, 1},
		"tail-not-init":  {901, 44},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validatePublicationNamespaceInit(901, chain); err == nil {
				t.Fatalf("non-init NSpid chain %v was accepted", chain)
			}
		})
	}
}

func TestPublicationDetachedChildSetsidClosesHighFDAndSignalsExactReady(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	late := filepath.Join(dir, "late")
	cmd := exec.Command(self,
		"__publication-confinement-detached-child",
		"--ready", ready,
		"--late", late,
		"--delay-ms", "800",
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("private detached child: %v: %s", err, output)
	}
	raw, err := os.ReadFile(ready)
	if err != nil {
		t.Fatal(err)
	}
	var got publicationDetachedReadyV1
	if err := json.Unmarshal(raw, &got); err != nil || got.NamespacePID < 1 || got.StartIdentity == "" || got.NamespaceIdentity == "" {
		t.Fatalf("detached ready=%#v err=%v raw=%q", got, err, raw)
	}
	if _, err := os.Stat(late); err != nil {
		t.Fatalf("detached child did not reach bounded late marker: %v", err)
	}
}

func TestPublicationChildEnumerationIncludesNonLeaderThreadChildren(t *testing.T) {
	type started struct {
		cmd *exec.Cmd
		tid int
		err error
	}
	start := func() started {
		cmd := exec.Command("/bin/sleep", "30")
		return started{cmd: cmd, tid: syscall.Gettid(), err: cmd.Start()}
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	var child started
	var release chan struct{}
	if syscall.Gettid() != os.Getpid() {
		child = start()
	} else {
		result := make(chan started, 1)
		release = make(chan struct{})
		go func() {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			result <- start()
			<-release
		}()
		child = <-result
	}
	if release != nil {
		defer close(release)
	}
	if child.err != nil {
		t.Fatal(child.err)
	}
	defer func() {
		_ = child.cmd.Process.Kill()
		_ = child.cmd.Wait()
	}()
	if child.tid == os.Getpid() {
		t.Fatal("fixture did not start from a non-leader task")
	}
	contains := func(path string) bool {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, field := range strings.Fields(string(raw)) {
			pid, err := strconv.Atoi(field)
			if err != nil {
				t.Fatal(err)
			}
			if pid == child.cmd.Process.Pid {
				return true
			}
		}
		return false
	}
	root := filepath.Join("/proc", strconv.Itoa(os.Getpid()), "task")
	if contains(filepath.Join(root, strconv.Itoa(os.Getpid()), "children")) {
		t.Fatal("non-leader child unexpectedly appeared in leader-only children list")
	}
	if !contains(filepath.Join(root, strconv.Itoa(child.tid), "children")) {
		t.Fatal("non-leader child missing from spawning task children list")
	}
	children, err := linuxProcessChildrenExact(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, pid := range children {
		found = found || pid == child.cmd.Process.Pid
	}
	if !found {
		t.Fatalf("all-task enumeration missed non-leader child %d: %v", child.cmd.Process.Pid, children)
	}
}

func TestPublicationLifecycleSentinelMustRemainRunningAndUnsignalled(t *testing.T) {
	newWitness := func(t *testing.T) *publicationLaunchWitness {
		t.Helper()
		cmd := exec.Command("/bin/sleep", "300")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		start, state, _, exists, err := linuxProcessIdentityExact(cmd.Process.Pid)
		if err != nil || !exists || state == "Z" || state == "T" {
			t.Fatalf("bind sentinel: state=%q exists=%v err=%v", state, exists, err)
		}
		return &publicationLaunchWitness{
			namespaceIdentity: "pid:[18446744073709551615]", namespaceInitPID: 1 << 30, namespaceInitStart: "absent",
			sentinelPID: cmd.Process.Pid, sentinelStart: start, sentinelState: state, sentinelCmd: cmd,
		}
	}

	t.Run("live", func(t *testing.T) {
		if err := verifyPublicationLaunchTeardown(newWitness(t)); err != nil {
			t.Fatalf("live unrelated sentinel: %v", err)
		}
	})
	for name, signal := range map[string]syscall.Signal{"stopped": syscall.SIGSTOP, "killed-zombie": syscall.SIGKILL} {
		t.Run(name, func(t *testing.T) {
			witness := newWitness(t)
			if err := witness.sentinelCmd.Process.Signal(signal); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				_, state, _, exists, err := linuxProcessIdentityExact(witness.sentinelPID)
				if err != nil {
					t.Fatal(err)
				}
				if exists && (state == "T" || state == "Z") {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if err := verifyPublicationLaunchTeardown(witness); err == nil {
				t.Fatalf("%s sentinel was accepted as unchanged", name)
			}
		})
	}
}

func TestPublicationCodexAgentFastDeterministicLaunchIsWitnessedExactlyOnce(t *testing.T) {
	discovered, err := DiscoverProductionPublicationCodexBoundary(context.Background(), filepath.Join(t.TempDir(), "discovery-bootstrap"))
	if err != nil {
		t.Fatalf("discover exact installed Linux boundary: %v", err)
	}
	pathFor := func(role PublicationExecutableRole) string {
		for _, binding := range discovered.Manifest().ExecutableClosure {
			if binding.Role == role {
				return binding.RealPath
			}
		}
		return ""
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	container := t.TempDir()
	marker := filepath.Join(container, "agent-invocations")
	boundary, err := NewPublicationCodexBoundaryV1(context.Background(), PublicationCodexBoundaryOptions{
		LogicalEntryPath: self, NativeCodexPath: self, SandboxHelperPath: pathFor(PublicationExecutableSandboxHelper),
		BubblewrapPath: pathFor(PublicationExecutableBubblewrap), CanaryExecutablePath: self,
		SentinelExecutablePath: pathFor(PublicationExecutableSentinel), PermissionProfile: []byte("publication-v1-test"),
		ProbeFixedArgs: []string{"sandbox"}, ExecFixedArgs: []string{"exec", "--test-marker", marker},
		CommandFixedArgs: []string{"sandbox"}, BootstrapDir: filepath.Join(container, "bootstrap"), ConfigHomeDir: filepath.Join(container, "config-home"),
	})
	if err != nil {
		t.Fatal(err)
	}
	protected, err := NewPublicationCodexAgent(boundary, nil)
	if err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(container, "candidate")
	scratch := filepath.Join(container, "scratch")
	source := filepath.Join(container, "source")
	for _, dir := range []string{candidate, scratch, source} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	result, err := protected.Run(context.Background(), RunOpts{
		Prompt: "deterministic", CWD: candidate, PublicationSourceDir: source, PublicationScratchDir: scratch,
	})
	if err != nil {
		t.Fatalf("protected deterministic agent: %v", err)
	}
	if result == nil || result.Text != "ok" {
		t.Fatalf("protected deterministic result=%#v", result)
	}
	raw, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "x" {
		t.Fatalf("protected agent invocation bytes=%q, want exactly one", raw)
	}

	transientMarker := filepath.Join(container, "transient-agent-invocations")
	transientBoundary, err := NewPublicationCodexBoundaryV1(context.Background(), PublicationCodexBoundaryOptions{
		LogicalEntryPath: self, NativeCodexPath: self, SandboxHelperPath: pathFor(PublicationExecutableSandboxHelper),
		BubblewrapPath: pathFor(PublicationExecutableBubblewrap), CanaryExecutablePath: self,
		SentinelExecutablePath: pathFor(PublicationExecutableSentinel), PermissionProfile: []byte("publication-v1-test"),
		ProbeFixedArgs: []string{"sandbox"}, ExecFixedArgs: []string{"exec", "--test-transient-marker", transientMarker},
		CommandFixedArgs: []string{"sandbox"}, BootstrapDir: filepath.Join(container, "transient-bootstrap"), ConfigHomeDir: filepath.Join(container, "transient-config-home"),
	})
	if err != nil {
		t.Fatal(err)
	}
	transientAgent, err := NewPublicationCodexAgent(transientBoundary, nil)
	if err != nil {
		t.Fatal(err)
	}
	transientScratch := filepath.Join(container, "transient-scratch")
	if err := os.Mkdir(transientScratch, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := transientAgent.Run(context.Background(), RunOpts{
		Prompt: "deterministic transient failure", CWD: candidate, PublicationSourceDir: source, PublicationScratchDir: transientScratch,
	}); err == nil {
		t.Fatal("transient protected agent failure unexpectedly succeeded")
	}
	raw, err = os.ReadFile(transientMarker)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "x" {
		t.Fatalf("transient protected agent invocation bytes=%q, want exactly one and no retry", raw)
	}
}
