//go:build linux

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const productionPublicationPolicyTemplate = `publication-v1: extends=:read-only; container=deny; candidate=read; scratch=write; source=deny; home=deny; network=deny; env=closed`

func DiscoverProductionPublicationCodexBoundary(ctx context.Context, bootstrapRoot string) (*PublicationCodexBoundaryV1, error) {
	canaryExecutable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("%w: resolve publication canary executable", ErrPublicationConfinementUnavailable)
	}
	logical, err := exec.LookPath("codex")
	if err != nil {
		return nil, fmt.Errorf("%w: resolve logical Codex entry: %v", ErrPublicationConfinementUnavailable, err)
	}
	if !filepath.IsAbs(logical) {
		logical, err = filepath.Abs(logical)
		if err != nil {
			return nil, fmt.Errorf("%w: canonicalize logical Codex entry", ErrPublicationConfinementUnavailable)
		}
	}
	realLogical, err := filepath.EvalSymlinks(logical)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve logical Codex entry", ErrPublicationConfinementUnavailable)
	}
	packageRoot := ""
	native := realLogical
	if !isELFExecutable(realLogical) {
		if filepath.Base(realLogical) != "codex.js" || filepath.Base(filepath.Dir(realLogical)) != "bin" {
			return nil, fmt.Errorf("%w: logical Codex entry is neither native nor the supported package launcher", ErrPublicationConfinementUnavailable)
		}
		packageRoot = filepath.Dir(filepath.Dir(realLogical))
		triple, platformPackage, ok := publicationLinuxTarget()
		if !ok {
			return nil, fmt.Errorf("%w: unsupported Linux architecture %s", ErrPublicationConfinementUnavailable, runtime.GOARCH)
		}
		vendorRoots := []string{
			filepath.Join(packageRoot, "node_modules", "@openai", platformPackage, "vendor", triple),
			filepath.Join(packageRoot, "vendor", triple),
		}
		native = ""
		for _, vendor := range vendorRoots {
			candidate := filepath.Join(vendor, "bin", "codex")
			if isExecutableRegular(candidate) {
				native = candidate
				break
			}
		}
		if native == "" {
			return nil, fmt.Errorf("%w: installed package native Codex is not enumerable", ErrPublicationConfinementUnavailable)
		}
	}

	nativeDir := filepath.Dir(native)
	helper := filepath.Join(nativeDir, "codex-linux-sandbox")
	if !isExecutableRegular(helper) {
		return nil, fmt.Errorf("%w: installed Codex Linux sandbox helper is not enumerable", ErrPublicationConfinementUnavailable)
	}
	bwrap, err := selectPublicationBubblewrap(nativeDir)
	if err != nil {
		return nil, err
	}
	sentinel, err := exec.LookPath("sleep")
	if err != nil {
		return nil, fmt.Errorf("%w: resolve lifecycle sentinel: %v", ErrPublicationConfinementUnavailable, err)
	}
	if !filepath.IsAbs(sentinel) {
		sentinel, err = filepath.Abs(sentinel)
		if err != nil {
			return nil, fmt.Errorf("%w: canonicalize lifecycle sentinel", ErrPublicationConfinementUnavailable)
		}
	}
	return NewPublicationCodexBoundaryV1(ctx, PublicationCodexBoundaryOptions{
		LogicalEntryPath: logical, NativeCodexPath: native, SandboxHelperPath: helper, BubblewrapPath: bwrap,
		PermissionProfile:      []byte(productionPublicationPolicyTemplate),
		ProbeFixedArgs:         productionPublicationProbeFixedArgs(),
		ExecFixedArgs:          productionPublicationExecFixedArgs(),
		CommandFixedArgs:       productionPublicationCommandFixedArgs(),
		BootstrapDir:           bootstrapRoot,
		ConfigHomeDir:          bootstrapRoot + "-config",
		ManagedPackageRoot:     packageRoot,
		CanaryExecutablePath:   canaryExecutable,
		SentinelExecutablePath: sentinel,
	})
}

func publicationLinuxTarget() (string, string, bool) {
	switch runtime.GOARCH {
	case "amd64":
		return "x86_64-unknown-linux-musl", "codex-linux-x64", true
	case "arm64":
		return "aarch64-unknown-linux-musl", "codex-linux-arm64", true
	default:
		return "", "", false
	}
}

func isELFExecutable(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var magic [4]byte
	if _, err := f.Read(magic[:]); err != nil {
		return false
	}
	return magic == [4]byte{0x7f, 'E', 'L', 'F'} && isExecutableRegular(path)
}

func isExecutableRegular(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0
}

func selectPublicationBubblewrap(nativeDir string) (string, error) {
	// The packaged helper resolves its bundled bwrap relative to the native
	// payload when present, so bind that exact executable before considering a
	// system fallback. The closed bootstrap PATH points to the same bytes.
	bundled := filepath.Join(filepath.Dir(nativeDir), "codex-resources", "bwrap")
	if isExecutableRegular(bundled) {
		return bundled, nil
	}
	if system, err := exec.LookPath("bwrap"); err == nil {
		if !filepath.IsAbs(system) {
			system, err = filepath.Abs(system)
		}
		if err == nil && isExecutableRegular(system) {
			return system, nil
		}
	}
	return "", fmt.Errorf("%w: no exact system or bundled bubblewrap is available", ErrPublicationConfinementUnavailable)
}

type PublicationLinuxLifecycleCanaryOptions struct {
	CandidateDir   string
	SourceDir      string
	ScratchDir     string
	SourceFile     string
	SiblingFile    string
	TCPAddress     string
	UnixSocketPath string
}

type PublicationLinuxLifecycleCanaryReport struct {
	ChildObservedAlive       bool
	WrapperExitedWithinBound bool
	NamespaceEmpty           bool
	ChildStillExists         bool
	LateMarkerExists         bool
	SentinelSignalled        bool
}

type publicationDetachedReadyV1 struct {
	NamespaceIdentity string `json:"namespace_identity"`
	NamespacePID      int    `json:"namespace_pid"`
	StartIdentity     string `json:"start_identity"`
}

func probePublicationCodexBoundary(ctx context.Context, boundary *PublicationCodexBoundaryV1, opts PublicationCodexProbeOptions) error {
	if boundary == nil {
		return fmt.Errorf("%w: Linux publication canary is unavailable", ErrPublicationConfinementUnavailable)
	}
	report, err := boundary.RunLinuxLifecycleCanary(ctx, PublicationLinuxLifecycleCanaryOptions{
		CandidateDir: opts.CandidateDir, SourceDir: opts.SourceDir, ScratchDir: opts.ScratchDir,
		SourceFile: opts.SourceFile, SiblingFile: opts.SiblingFile,
		TCPAddress: opts.TCPAddress, UnixSocketPath: opts.UnixSocketPath,
	})
	if err != nil {
		return err
	}
	if !report.ChildObservedAlive || !report.WrapperExitedWithinBound || !report.NamespaceEmpty ||
		report.ChildStillExists || report.LateMarkerExists || report.SentinelSignalled {
		return fmt.Errorf("%w: Linux lifecycle canary report is incomplete", ErrPublicationConfinementUnavailable)
	}
	if _, err := os.Stat(filepath.Join(opts.ScratchDir, "positive-control")); err != nil {
		return fmt.Errorf("%w: sandbox scratch-write positive control is absent", ErrPublicationConfinementUnavailable)
	}
	return nil
}

// RunLinuxLifecycleCanary is the non-skippable process-lifecycle falsifier. It
// deliberately runs only through the pinned model-free Codex sandbox command.
// The helper creates its own session inside Codex's PID namespace, changes cwd
// to / and closes inherited descriptors. Cancelling the outer native Codex
// process must destroy that namespace; no host CWD scan is involved.
func (b *PublicationCodexBoundaryV1) RunLinuxLifecycleCanary(ctx context.Context, opts PublicationLinuxLifecycleCanaryOptions) (PublicationLinuxLifecycleCanaryReport, error) {
	var report PublicationLinuxLifecycleCanaryReport
	if b == nil || !filepath.IsAbs(opts.CandidateDir) ||
		!filepath.IsAbs(opts.SourceDir) || !filepath.IsAbs(opts.ScratchDir) {
		return report, fmt.Errorf("%w: lifecycle canary paths must be absolute", ErrPublicationConfinementUnavailable)
	}
	var wantCanary PublicationExecutableBinding
	for _, binding := range b.manifest.ExecutableClosure {
		if binding.Role == PublicationExecutableCanary {
			wantCanary = binding
			break
		}
	}
	gotCanary, bindErr := bindPublicationExecutable(PublicationExecutableCanary, b.paths[PublicationExecutableCanary])
	if bindErr != nil || wantCanary.RealPath == "" || gotCanary.LogicalPath != wantCanary.LogicalPath ||
		gotCanary.RealPath != wantCanary.RealPath || gotCanary.RawSHA256 != wantCanary.RawSHA256 ||
		gotCanary.FileIdentity != wantCanary.FileIdentity || gotCanary.LinkIdentity != wantCanary.LinkIdentity ||
		gotCanary.OwnerIdentity != wantCanary.OwnerIdentity || !os.SameFile(gotCanary.info, wantCanary.info) {
		return report, fmt.Errorf("%w: lifecycle canary does not match the pinned executable", ErrPublicationConfinementUnavailable)
	}
	view, err := b.bindProbeView(opts.CandidateDir, opts.SourceDir, opts.ScratchDir)
	if err != nil {
		return report, err
	}
	ready := filepath.Join(opts.ScratchDir, "lifecycle.ready")
	late := filepath.Join(opts.ScratchDir, "lifecycle.late")
	_ = os.Remove(ready)
	_ = os.Remove(ready + ".tmp")
	_ = os.Remove(late)

	canaryCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	home, err := os.UserHomeDir()
	if err != nil {
		return report, fmt.Errorf("%w: resolve home for canary", ErrPublicationConfinementUnavailable)
	}
	config := PublicationConfinementCanaryConfig{
		CanaryBinding: wantCanary,
		CandidateDir:  opts.CandidateDir, SourceFile: opts.SourceFile, SiblingFile: opts.SiblingFile,
		ScratchDir: opts.ScratchDir, ReadyMarker: ready, LateMarker: late,
		TCPAddress: opts.TCPAddress, UnixSocketPath: opts.UnixSocketPath, HomeDir: home, Delay: 800 * time.Millisecond,
	}
	for _, binding := range b.manifest.ExecutableClosure {
		if binding.Role != PublicationExecutableCanary {
			config.ForbiddenExecutables = append(config.ForbiddenExecutables, binding.RealPath)
		}
	}
	rawConfig, err := json.Marshal(config)
	if err != nil {
		return report, err
	}
	if err := os.WriteFile(filepath.Join(opts.ScratchDir, "publication-canary.json"), rawConfig, 0o600); err != nil {
		return report, err
	}
	payload := []string{
		b.paths[PublicationExecutableCanary], "__publication-confinement-canary",
		"--scratch", opts.ScratchDir, "--ready", ready, "--late", late, "--delay-ms", "800",
	}
	cmd, err := view.probeCommand(canaryCtx, payload)
	if err != nil {
		return report, err
	}
	if err := cmd.armLifecycleBarrier(); err != nil {
		return report, err
	}
	if err := cmd.Start(); err != nil {
		cmd.abortLifecycleBarrier()
		return report, fmt.Errorf("%w: start lifecycle canary: %v", ErrPublicationConfinementUnavailable, err)
	}
	launchWitness, err := cmd.bindAndReleaseLifecycle(b.paths[PublicationExecutableSentinel])
	if err != nil {
		cancel()
		_ = cmd.Wait()
		cmd.abortLifecycleBarrier()
		return report, err
	}
	witnessVerified := false
	defer func() {
		if !witnessVerified {
			_ = launchWitness.sentinelCmd.Process.Kill()
			_ = launchWitness.sentinelCmd.Wait()
		}
	}()
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	consumedWait := false
	defer func() {
		if !consumedWait {
			cancel()
			select {
			case <-waitCh:
			case <-time.After(publicationBoundaryWait):
			}
		}
	}()

	deadline := time.Now().Add(5 * time.Second)
	var detachedReady publicationDetachedReadyV1
	for time.Now().Before(deadline) {
		raw, readErr := os.ReadFile(ready)
		if readErr == nil {
			if len(raw) > 4096 || json.Unmarshal(raw, &detachedReady) != nil ||
				detachedReady.NamespaceIdentity != launchWitness.namespaceIdentity || detachedReady.NamespacePID < 2 || detachedReady.StartIdentity == "" {
				cancel()
				return report, fmt.Errorf("%w: detached canary ready marker is malformed", ErrPublicationConfinementCleanupUncertain)
			}
			detachedPID, detachedStart, bindErr := bindPublicationDetachedChild(launchWitness, detachedReady)
			if bindErr != nil {
				cancel()
				return report, bindErr
			}
			launchWitness.detachedPID = detachedPID
			launchWitness.detachedStart = detachedStart
			report.ChildObservedAlive = true
			break
		}
		select {
		case <-waitCh:
			consumedWait = true
			cancel()
			return report, fmt.Errorf("%w: lifecycle wrapper exited before a provable namespace", ErrPublicationConfinementCleanupUncertain)
		case <-ctx.Done():
			cancel()
			return report, fmt.Errorf("%w: lifecycle probe cancelled before a provable namespace", ErrPublicationConfinementCleanupUncertain)
		case <-time.After(20 * time.Millisecond):
		}
	}
	if !report.ChildObservedAlive {
		cancel()
		if !consumedWait {
			select {
			case <-waitCh:
				consumedWait = true
			case <-time.After(publicationBoundaryWait):
			}
		}
		return report, fmt.Errorf("%w: detached lifecycle namespace was not positively observed", ErrPublicationConfinementCleanupUncertain)
	}
	detachedStart, detachedState, _, detachedExists, detachedErr := linuxProcessIdentityExact(launchWitness.detachedPID)
	if detachedErr != nil || !detachedExists || detachedStart != launchWitness.detachedStart || detachedState == "Z" || detachedState == "T" {
		cancel()
		return report, fmt.Errorf("%w: detached canary child was not live at cancellation boundary", ErrPublicationConfinementCleanupUncertain)
	}

	cancel()
	select {
	case <-waitCh:
		consumedWait = true
		report.WrapperExitedWithinBound = true
	case <-time.After(publicationBoundaryWait):
		return report, fmt.Errorf("%w: lifecycle wrapper did not exit within bound", ErrPublicationConfinementCleanupUncertain)
	}
	if err := verifyPublicationLaunchTeardown(launchWitness); err != nil {
		return report, err
	}
	witnessVerified = true

	initStart, initExists, identityErr := linuxProcessStartIdentityExact(launchWitness.namespaceInitPID)
	if identityErr != nil {
		return report, fmt.Errorf("%w: revalidate canary namespace init: %v", ErrPublicationConfinementCleanupUncertain, identityErr)
	}
	report.ChildStillExists = initExists && initStart == launchWitness.namespaceInitStart
	if detachedStart, detachedExists, detachedErr := linuxProcessStartIdentityExact(launchWitness.detachedPID); detachedErr != nil {
		return report, fmt.Errorf("%w: revalidate detached canary child: %v", ErrPublicationConfinementCleanupUncertain, detachedErr)
	} else if detachedExists && detachedStart == launchWitness.detachedStart {
		report.ChildStillExists = true
	}
	report.NamespaceEmpty = !report.ChildStillExists
	// verifyPublicationLaunchTeardown already proved that its separately
	// spawned, exact PID/start-identity sentinel remained alive and unchanged.
	report.SentinelSignalled = false
	_, lateErr := os.Lstat(late)
	report.LateMarkerExists = lateErr == nil
	if report.ChildStillExists || report.LateMarkerExists {
		return report, fmt.Errorf("%w: detached lifecycle child survived", ErrPublicationConfinementCleanupUncertain)
	}
	return report, nil
}

func (cmd *publicationPreparedCommand) armLifecycleBarrier() error {
	if cmd == nil || cmd.Cmd == nil || len(cmd.ExtraFiles) != 0 || cmd.releaseReader != nil || cmd.infoReader != nil {
		return fmt.Errorf("%w: lifecycle barrier command is invalid", ErrPublicationConfinementCleanupUncertain)
	}
	releaseReader, releaseWriter, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("%w: create lifecycle release pipe: %v", ErrPublicationConfinementCleanupUncertain, err)
	}
	infoReader, infoWriter, err := os.Pipe()
	if err != nil {
		_ = releaseReader.Close()
		_ = releaseWriter.Close()
		return fmt.Errorf("%w: create lifecycle info pipe: %v", ErrPublicationConfinementCleanupUncertain, err)
	}
	cmd.releaseReader = releaseReader
	cmd.releaseWriter = releaseWriter
	cmd.infoReader = infoReader
	cmd.infoWriter = infoWriter
	// Go assigns ExtraFiles consecutively from fd 3. The immutable bwrap
	// prefix binds --block-fd 3 and --info-fd 4 to this exact ordering.
	cmd.ExtraFiles = []*os.File{releaseReader, infoWriter}
	return nil
}

func (cmd *publicationPreparedCommand) abortLifecycleBarrier() {
	if cmd == nil {
		return
	}
	for _, file := range []*os.File{cmd.releaseReader, cmd.releaseWriter, cmd.infoReader, cmd.infoWriter} {
		if file != nil {
			_ = file.Close()
		}
	}
	cmd.releaseReader = nil
	cmd.releaseWriter = nil
	cmd.infoReader = nil
	cmd.infoWriter = nil
}

func (cmd *publicationPreparedCommand) bindAndReleaseLifecycle(sentinelPath string) (*publicationLaunchWitness, error) {
	if cmd == nil || cmd.Process == nil || cmd.releaseReader == nil || cmd.releaseWriter == nil || cmd.infoReader == nil || cmd.infoWriter == nil {
		return nil, fmt.Errorf("%w: lifecycle barrier was not armed", ErrPublicationConfinementCleanupUncertain)
	}
	_ = cmd.releaseReader.Close()
	cmd.releaseReader = nil
	_ = cmd.infoWriter.Close()
	cmd.infoWriter = nil
	type infoReadResult struct {
		raw []byte
		err error
	}
	readCh := make(chan infoReadResult, 1)
	go func(reader *os.File) {
		raw, readErr := io.ReadAll(io.LimitReader(reader, (64<<10)+1))
		readCh <- infoReadResult{raw: raw, err: readErr}
	}(cmd.infoReader)
	var result infoReadResult
	select {
	case result = <-readCh:
	case <-time.After(3 * time.Second):
		_ = cmd.infoReader.Close()
		cmd.infoReader = nil
		return nil, fmt.Errorf("%w: bwrap lifecycle identity timed out", ErrPublicationConfinementCleanupUncertain)
	}
	raw, err := result.raw, result.err
	_ = cmd.infoReader.Close()
	cmd.infoReader = nil
	if err != nil || len(raw) == 0 || len(raw) > 64<<10 {
		return nil, fmt.Errorf("%w: read bounded bwrap lifecycle identity", ErrPublicationConfinementCleanupUncertain)
	}
	var info struct {
		ChildPID int `json:"child-pid"`
	}
	if err := json.Unmarshal(raw, &info); err != nil || info.ChildPID < 1 {
		return nil, fmt.Errorf("%w: decode bwrap lifecycle identity", ErrPublicationConfinementCleanupUncertain)
	}
	start, state, ppid, exists, err := linuxProcessIdentityExact(info.ChildPID)
	if err != nil || !exists || ppid != cmd.Process.Pid || state == "Z" || state == "T" {
		return nil, fmt.Errorf("%w: bind exact blocked bwrap child", ErrPublicationConfinementCleanupUncertain)
	}
	namespace, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(info.ChildPID), "ns", "pid"))
	if err != nil || !strings.HasPrefix(namespace, "pid:[") {
		return nil, fmt.Errorf("%w: bind exact bwrap PID namespace", ErrPublicationConfinementCleanupUncertain)
	}
	nspids, err := linuxNamespacePIDChain(info.ChildPID)
	if err != nil || validatePublicationNamespaceInit(info.ChildPID, nspids) != nil {
		return nil, fmt.Errorf("%w: bwrap child is not the exact PID-namespace init", ErrPublicationConfinementCleanupUncertain)
	}
	sentinel := exec.Command(sentinelPath, "2147483647")
	if err := sentinel.Start(); err != nil {
		return nil, fmt.Errorf("%w: start bound lifecycle sentinel: %v", ErrPublicationConfinementCleanupUncertain, err)
	}
	sentinelStart, sentinelState, _, sentinelExists, err := linuxProcessIdentityExact(sentinel.Process.Pid)
	if err != nil || !sentinelExists || sentinelState == "Z" || sentinelState == "T" {
		_ = sentinel.Process.Kill()
		_ = sentinel.Wait()
		return nil, fmt.Errorf("%w: bind running lifecycle sentinel", ErrPublicationConfinementCleanupUncertain)
	}
	witness := &publicationLaunchWitness{
		namespaceIdentity:  namespace,
		namespaceInitPID:   info.ChildPID,
		namespaceInitStart: start,
		sentinelPID:        sentinel.Process.Pid,
		sentinelStart:      sentinelStart,
		sentinelState:      sentinelState,
		sentinelCmd:        sentinel,
	}
	if _, err := cmd.releaseWriter.Write([]byte{1}); err != nil {
		_ = sentinel.Process.Kill()
		_ = sentinel.Wait()
		return nil, fmt.Errorf("%w: release exact bwrap lifecycle barrier", ErrPublicationConfinementCleanupUncertain)
	}
	_ = cmd.releaseWriter.Close()
	cmd.releaseWriter = nil
	return witness, nil
}

func linuxNamespacePIDChain(pid int) ([]int, error) {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status"))
	if err != nil {
		return nil, err
	}
	return parseLinuxNamespacePIDChain(raw)
}

func parseLinuxNamespacePIDChain(raw []byte) ([]int, error) {
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "NSpid:") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, "NSpid:"))
		if len(fields) == 0 {
			return nil, fmt.Errorf("empty NSpid chain")
		}
		chain := make([]int, 0, len(fields))
		for _, field := range fields {
			value, parseErr := strconv.Atoi(field)
			if parseErr != nil || value < 1 {
				return nil, fmt.Errorf("malformed NSpid chain")
			}
			chain = append(chain, value)
		}
		return chain, nil
	}
	return nil, fmt.Errorf("missing NSpid chain")
}

func validatePublicationNamespaceInit(hostPID int, chain []int) error {
	if hostPID < 1 || len(chain) < 2 || chain[0] != hostPID || chain[len(chain)-1] != 1 {
		return fmt.Errorf("NSpid chain does not bind the exact namespace init")
	}
	return nil
}

func bindPublicationDetachedChild(witness *publicationLaunchWitness, ready publicationDetachedReadyV1) (int, string, error) {
	if witness == nil || witness.namespaceInitPID < 1 || witness.namespaceInitStart == "" {
		return 0, "", fmt.Errorf("%w: namespace-init witness is incomplete", ErrPublicationConfinementCleanupUncertain)
	}
	type exactNode struct {
		pid   int
		start string
	}
	queue := []exactNode{{pid: witness.namespaceInitPID, start: witness.namespaceInitStart}}
	seen := map[int]struct{}{witness.namespaceInitPID: {}}
	matchedPID := 0
	matchedStart := ""
	for len(queue) > 0 {
		if len(seen) > 4096 {
			return 0, "", fmt.Errorf("%w: protected descendant tree exceeded bound", ErrPublicationConfinementCleanupUncertain)
		}
		parent := queue[0]
		queue = queue[1:]
		parentStart, _, _, parentExists, parentErr := linuxProcessIdentityExact(parent.pid)
		if parentErr != nil || !parentExists || parentStart != parent.start {
			return 0, "", fmt.Errorf("%w: exact protected ancestor changed during traversal", ErrPublicationConfinementCleanupUncertain)
		}
		children, childErr := linuxProcessChildrenExact(parent.pid)
		if childErr != nil {
			return 0, "", fmt.Errorf("%w: enumerate exact protected children: %v", ErrPublicationConfinementCleanupUncertain, childErr)
		}
		for _, pid := range children {
			if _, duplicate := seen[pid]; duplicate {
				return 0, "", fmt.Errorf("%w: protected descendant tree is cyclic", ErrPublicationConfinementCleanupUncertain)
			}
			start, state, ppid, exists, identityErr := linuxProcessIdentityExact(pid)
			if identityErr != nil || !exists || ppid != parent.pid {
				return 0, "", fmt.Errorf("%w: inspect exact protected descendant", ErrPublicationConfinementCleanupUncertain)
			}
			chain, chainErr := linuxNamespacePIDChain(pid)
			if chainErr != nil {
				return 0, "", fmt.Errorf("%w: inspect protected descendant NSpid: %v", ErrPublicationConfinementCleanupUncertain, chainErr)
			}
			seen[pid] = struct{}{}
			if chain[0] != pid || chain[len(chain)-1] != ready.NamespacePID || start != ready.StartIdentity {
				queue = append(queue, exactNode{pid: pid, start: start})
				continue
			}
			namespace, namespaceErr := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "ns", "pid"))
			if namespaceErr != nil || namespace != ready.NamespaceIdentity || namespace != witness.namespaceIdentity {
				return 0, "", fmt.Errorf("%w: detached child namespace does not match exact witness", ErrPublicationConfinementCleanupUncertain)
			}
			if state == "Z" || state == "T" {
				return 0, "", fmt.Errorf("%w: detached canary child is not live", ErrPublicationConfinementCleanupUncertain)
			}
			if matchedPID != 0 {
				return 0, "", fmt.Errorf("%w: detached canary child is ambiguous", ErrPublicationConfinementCleanupUncertain)
			}
			matchedPID, matchedStart = pid, start
		}
	}
	if matchedPID == 0 {
		return 0, "", fmt.Errorf("%w: detached canary child was not found live", ErrPublicationConfinementCleanupUncertain)
	}
	return matchedPID, matchedStart, nil
}

func linuxProcessChildrenExact(tgid int) ([]int, error) {
	if tgid < 1 {
		return nil, fmt.Errorf("invalid thread-group id")
	}
	taskRoot := filepath.Join("/proc", strconv.Itoa(tgid), "task")
	tasks, err := os.ReadDir(taskRoot)
	if err != nil || len(tasks) > 4096 {
		return nil, fmt.Errorf("read bounded task list: %w", err)
	}
	children := make(map[int]struct{})
	totalBytes := 0
	for _, task := range tasks {
		tid, parseErr := strconv.Atoi(task.Name())
		if parseErr != nil || tid < 1 || !task.IsDir() {
			return nil, fmt.Errorf("malformed task entry")
		}
		raw, readErr := os.ReadFile(filepath.Join(taskRoot, task.Name(), "children"))
		if readErr != nil {
			return nil, readErr
		}
		totalBytes += len(raw)
		if totalBytes > 64<<10 {
			return nil, fmt.Errorf("task child lists exceeded bound")
		}
		for _, field := range strings.Fields(string(raw)) {
			pid, childParseErr := strconv.Atoi(field)
			if childParseErr != nil || pid < 1 {
				return nil, fmt.Errorf("malformed task child list")
			}
			children[pid] = struct{}{}
		}
	}
	result := make([]int, 0, len(children))
	for pid := range children {
		result = append(result, pid)
	}
	sort.Ints(result)
	return result, nil
}

func verifyPublicationLaunchTeardown(witness *publicationLaunchWitness) error {
	if witness == nil || witness.namespaceIdentity == "" || witness.namespaceInitPID < 1 || witness.namespaceInitStart == "" || witness.sentinelCmd == nil {
		return fmt.Errorf("%w: protected launch witness is incomplete", ErrPublicationConfinementCleanupUncertain)
	}
	defer func() {
		_ = witness.sentinelCmd.Process.Kill()
		_ = witness.sentinelCmd.Wait()
	}()
	deadline := time.Now().Add(publicationBoundaryWait)
	for time.Now().Before(deadline) {
		got, exists, err := linuxProcessStartIdentityExact(witness.namespaceInitPID)
		if err != nil {
			return fmt.Errorf("%w: revalidate protected namespace init %d: %v", ErrPublicationConfinementCleanupUncertain, witness.namespaceInitPID, err)
		}
		// The blocked child was proved above to be PID 1 in the newly-created
		// namespace. Linux kills every remaining member when that namespace init
		// exits, so exact absence of this PID/start identity is the lifecycle
		// authority and cannot be hidden by PR_SET_DUMPABLE ownership changes.
		if !exists || got != witness.namespaceInitStart {
			sentinelStart, sentinelState, _, exists, err := linuxProcessIdentityExact(witness.sentinelPID)
			if err != nil || !exists || sentinelStart != witness.sentinelStart || sentinelState == "Z" || sentinelState == "T" {
				return fmt.Errorf("%w: unrelated lifecycle sentinel changed", ErrPublicationConfinementCleanupUncertain)
			}
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("%w: protected namespace retained members", ErrPublicationConfinementCleanupUncertain)
}

func RunPublicationConfinementCanary(config PublicationConfinementCanaryConfig) error {
	if config.Delay <= 0 || config.CanaryBinding.Role != PublicationExecutableCanary || !filepath.IsAbs(config.CanaryBinding.LogicalPath) ||
		!filepath.IsAbs(config.CanaryBinding.RealPath) || !filepath.IsAbs(config.CandidateDir) || !filepath.IsAbs(config.ScratchDir) ||
		!filepath.IsAbs(config.ReadyMarker) || !filepath.IsAbs(config.LateMarker) {
		return fmt.Errorf("invalid publication canary configuration")
	}
	selfInfo, err := os.Stat("/proc/self/exe")
	if err != nil {
		return fmt.Errorf("stat running publication canary: %w", err)
	}
	gotCanary, err := bindPublicationExecutable(PublicationExecutableCanary, config.CanaryBinding.LogicalPath)
	if err != nil || gotCanary.LogicalPath != config.CanaryBinding.LogicalPath || gotCanary.RealPath != config.CanaryBinding.RealPath ||
		gotCanary.RawSHA256 != config.CanaryBinding.RawSHA256 || gotCanary.Mode != config.CanaryBinding.Mode ||
		gotCanary.FileIdentity != config.CanaryBinding.FileIdentity || gotCanary.LinkIdentity != config.CanaryBinding.LinkIdentity ||
		gotCanary.OwnerIdentity != config.CanaryBinding.OwnerIdentity || !os.SameFile(selfInfo, gotCanary.info) {
		return fmt.Errorf("running publication canary does not match bound executable")
	}
	if _, err := os.ReadFile(filepath.Join(config.CandidateDir, "readable.txt")); err != nil {
		return fmt.Errorf("candidate read positive control: %w", err)
	}
	if err := os.WriteFile(filepath.Join(config.ScratchDir, "positive-control"), []byte("ok\n"), 0o600); err != nil {
		return fmt.Errorf("scratch write positive control: %w", err)
	}
	if err := os.WriteFile(filepath.Join(config.CandidateDir, "forbidden-write"), []byte("bad\n"), 0o600); err == nil {
		return fmt.Errorf("candidate write unexpectedly succeeded")
	}
	if err := os.Chmod(filepath.Join(config.CandidateDir, "readable.txt"), 0o600); err == nil {
		return fmt.Errorf("candidate chmod unexpectedly succeeded")
	}
	linkToCandidate := filepath.Join(config.ScratchDir, "candidate-link")
	if err := os.Symlink(filepath.Join(config.CandidateDir, "readable.txt"), linkToCandidate); err != nil {
		return fmt.Errorf("scratch symlink positive control: %w", err)
	}
	if _, err := os.ReadFile(linkToCandidate); err != nil {
		return fmt.Errorf("scratch-to-candidate read positive control: %w", err)
	}
	if err := os.WriteFile(linkToCandidate, []byte("bad\n"), 0o600); err == nil {
		return fmt.Errorf("scratch symlink bypassed candidate write denial")
	}
	if err := os.Link(filepath.Join(config.CandidateDir, "readable.txt"), filepath.Join(config.ScratchDir, "candidate-hardlink")); err == nil {
		return fmt.Errorf("hardlink bypassed candidate write boundary")
	}
	if err := os.Rename(filepath.Join(config.CandidateDir, "readable.txt"), filepath.Join(config.ScratchDir, "renamed-candidate")); err == nil {
		return fmt.Errorf("rename bypassed candidate write boundary")
	}
	for name, path := range map[string]string{"source": config.SourceFile, "sibling": config.SiblingFile} {
		if path == "" || !filepath.IsAbs(path) {
			return fmt.Errorf("%s canary path is invalid", name)
		}
		if _, err := os.ReadFile(path); err == nil {
			return fmt.Errorf("%s read unexpectedly succeeded", name)
		}
	}
	sourceLink := filepath.Join(config.ScratchDir, "source-link")
	if err := os.Symlink(config.SourceFile, sourceLink); err != nil {
		return fmt.Errorf("scratch source symlink positive control: %w", err)
	}
	if _, err := os.ReadFile(sourceLink); err == nil {
		return fmt.Errorf("scratch symlink bypassed source read denial")
	}
	if entries, err := os.ReadDir(config.HomeDir); err == nil || len(entries) != 0 {
		return fmt.Errorf("operator home read unexpectedly succeeded")
	}
	for _, key := range []string{"OPENAI_API_KEY", "GH_TOKEN", "GITHUB_TOKEN", "SSH_AUTH_SOCK", "GIT_ASKPASS", "CODEX_HOME"} {
		if os.Getenv(key) != "" {
			return fmt.Errorf("sensitive environment %s reached sandbox", key)
		}
	}
	if _, err := exec.LookPath("codex"); err == nil {
		return fmt.Errorf("nested Codex executable is reachable")
	}
	for index, executable := range config.ForbiddenExecutables {
		if executable == "" || !filepath.IsAbs(executable) {
			return fmt.Errorf("forbidden executable binding is invalid")
		}
		if err := requirePublicationExecutionDenied(executable); err != nil {
			return fmt.Errorf("bound executable %s: %w", filepath.Base(executable), err)
		}
		alias := filepath.Join(config.ScratchDir, "forbidden-exec-"+strconv.Itoa(index))
		if err := os.Symlink(executable, alias); err != nil {
			return fmt.Errorf("create executable alias: %w", err)
		}
		if err := requirePublicationExecutionDenied(alias); err != nil {
			return fmt.Errorf("bound executable alias %s: %w", filepath.Base(executable), err)
		}
	}
	if err := requirePublicationExecutionDenied("/proc/1/exe"); err != nil {
		return fmt.Errorf("pid-namespace executable alias: %w", err)
	}
	for network, address := range map[string]string{"tcp": config.TCPAddress, "unix": config.UnixSocketPath} {
		if address == "" {
			return fmt.Errorf("%s canary address is empty", network)
		}
		conn, err := net.DialTimeout(network, address, 300*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return fmt.Errorf("%s connection unexpectedly succeeded", network)
		}
	}
	gotCanary, err = bindPublicationExecutable(PublicationExecutableCanary, config.CanaryBinding.LogicalPath)
	if err != nil || gotCanary.RawSHA256 != config.CanaryBinding.RawSHA256 || gotCanary.Mode != config.CanaryBinding.Mode ||
		gotCanary.FileIdentity != config.CanaryBinding.FileIdentity || gotCanary.LinkIdentity != config.CanaryBinding.LinkIdentity ||
		gotCanary.OwnerIdentity != config.CanaryBinding.OwnerIdentity || !os.SameFile(selfInfo, gotCanary.info) {
		return fmt.Errorf("bound publication canary changed before detached child launch")
	}
	child := exec.Command(config.CanaryBinding.RealPath,
		"__publication-confinement-detached-child",
		"--ready", config.ReadyMarker,
		"--late", config.LateMarker,
		"--delay-ms", strconv.FormatInt(config.Delay.Milliseconds(), 10),
	)
	if err := child.Start(); err != nil {
		return fmt.Errorf("start detached publication canary child: %w", err)
	}
	return child.Wait()
}

// RunPublicationConfinementDetachedChild is reachable only through the exact
// private canary executable. Unlike the bwrap payload/session leader, this
// child can create a new session and falsify process-group based cleanup.
func RunPublicationConfinementDetachedChild(readyMarker, lateMarker string, delay time.Duration) error {
	if delay <= 0 || !filepath.IsAbs(readyMarker) || !filepath.IsAbs(lateMarker) || readyMarker == lateMarker {
		return fmt.Errorf("invalid detached publication canary child configuration")
	}
	if err := DetachPublicationConfinementCanary(); err != nil {
		return err
	}
	if _, _, errno := syscall.Syscall6(syscall.SYS_PRCTL, 4, 0, 0, 0, 0, 0); errno != 0 {
		return fmt.Errorf("make lifecycle child non-dumpable: %w", errno)
	}
	if err := os.Chdir("/"); err != nil {
		return err
	}
	probeFD, err := syscall.Open("/dev/null", syscall.O_RDONLY, 0)
	if err != nil {
		return fmt.Errorf("open high-fd probe: %w", err)
	}
	if err := syscall.Dup3(probeFD, 300, 0); err != nil {
		_ = syscall.Close(probeFD)
		return fmt.Errorf("install high-fd probe: %w", err)
	}
	if probeFD != 300 {
		_ = syscall.Close(probeFD)
	}
	if err := closePublicationNonStdioFDs(); err != nil {
		return err
	}
	var fdStat syscall.Stat_t
	if err := syscall.Fstat(300, &fdStat); !errors.Is(err, syscall.EBADF) {
		return fmt.Errorf("high inherited descriptor remained open")
	}
	namespaceIdentity, err := os.Readlink("/proc/self/ns/pid")
	if err != nil || !strings.HasPrefix(namespaceIdentity, "pid:[") {
		return fmt.Errorf("read lifecycle PID namespace: %w", err)
	}
	start, state, _, exists, err := linuxProcessIdentityExact(os.Getpid())
	if err != nil || !exists || state == "Z" || state == "T" {
		return fmt.Errorf("bind detached canary identity")
	}
	readyRaw, err := json.Marshal(publicationDetachedReadyV1{
		NamespaceIdentity: namespaceIdentity,
		NamespacePID:      os.Getpid(),
		StartIdentity:     start,
	})
	if err != nil {
		return err
	}
	readyTemp := readyMarker + ".tmp"
	_ = os.Remove(readyTemp)
	if err := os.WriteFile(readyTemp, readyRaw, 0o600); err != nil {
		return err
	}
	if err := os.Rename(readyTemp, readyMarker); err != nil {
		return fmt.Errorf("publish detached ready marker atomically: %w", err)
	}
	time.Sleep(delay)
	if err := os.WriteFile(lateMarker, []byte("survived\n"), 0o600); err != nil {
		return err
	}
	return nil
}

func closePublicationNonStdioFDs() error {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return fmt.Errorf("enumerate inherited descriptors: %w", err)
	}
	for _, entry := range entries {
		fd, parseErr := strconv.Atoi(entry.Name())
		if parseErr != nil || fd <= 2 {
			continue
		}
		if closeErr := syscall.Close(fd); closeErr != nil && !errors.Is(closeErr, syscall.EBADF) {
			return fmt.Errorf("close inherited descriptor %d: %w", fd, closeErr)
		}
	}
	return nil
}

func requirePublicationExecutionDenied(path string) error {
	err := exec.Command(path, "--version").Run()
	if err == nil {
		return fmt.Errorf("execution unexpectedly succeeded")
	}
	var pathErr *os.PathError
	if !errors.As(err, &pathErr) || !errors.Is(pathErr, os.ErrPermission) {
		return fmt.Errorf("process started instead of being denied before exec: %w", err)
	}
	return nil
}

func linuxProcessStartIdentity(pid int) string {
	start, exists, _ := linuxProcessStartIdentityExact(pid)
	if !exists {
		return ""
	}
	return start
}

func linuxProcessStartIdentityExact(pid int) (string, bool, error) {
	start, _, _, exists, err := linuxProcessIdentityExact(pid)
	return start, exists, err
}

func linuxProcessIdentityExact(pid int) (start, state string, ppid int, exists bool, err error) {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		if os.IsNotExist(err) {
			return "", "", 0, false, nil
		}
		return "", "", 0, false, err
	}
	text := string(raw)
	closeParen := strings.LastIndex(text, ")")
	if closeParen < 0 {
		return "", "", 0, false, fmt.Errorf("malformed process stat")
	}
	fields := strings.Fields(text[closeParen+1:])
	if len(fields) <= 19 {
		return "", "", 0, false, fmt.Errorf("short process stat")
	}
	parent, parseErr := strconv.Atoi(fields[1])
	if parseErr != nil {
		return "", "", 0, false, parseErr
	}
	return fields[19], fields[0], parent, true, nil
}

func DetachPublicationConfinementCanary() error {
	_, err := syscall.Setsid()
	return err
}
