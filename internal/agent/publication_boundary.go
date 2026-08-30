package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

var ErrPublicationConfinementUnavailable = errors.New("confinement_unavailable")
var ErrPublicationConfinementCleanupUncertain = errors.New("publication confinement cleanup uncertain")

type PublicationExecutableRole string

const (
	PublicationExecutableLogicalEntry  PublicationExecutableRole = "logical-entry"
	PublicationExecutableInterpreter   PublicationExecutableRole = "interpreter"
	PublicationExecutableNativeCodex   PublicationExecutableRole = "native-codex"
	PublicationExecutableSandboxHelper PublicationExecutableRole = "linux-sandbox-helper"
	PublicationExecutableBubblewrap    PublicationExecutableRole = "bubblewrap"
	PublicationExecutableCanary        PublicationExecutableRole = "publication-canary"
	PublicationExecutableSentinel      PublicationExecutableRole = "lifecycle-sentinel"
)

type PublicationLaunchKind string

const (
	PublicationLaunchProbe   PublicationLaunchKind = "probe"
	PublicationLaunchExec    PublicationLaunchKind = "exec"
	PublicationLaunchCommand PublicationLaunchKind = "command"
)

type PublicationExecutableBinding struct {
	Role          PublicationExecutableRole
	LogicalPath   string
	RealPath      string
	RawSHA256     string
	Mode          os.FileMode
	FileIdentity  string
	LinkIdentity  string
	OwnerIdentity string

	info os.FileInfo
}

type PublicationCodexBoundaryManifest struct {
	ExecutableClosure    []PublicationExecutableBinding
	NativeVersion        string
	GOOS                 string
	GOARCH               string
	PolicyTemplateSHA256 string
	ProbeArgvSHA256      string
	ExecArgvSHA256       string
	CommandArgvSHA256    string
}

type PublicationCodexBoundaryOptions struct {
	LogicalEntryPath       string
	InterpreterPath        string
	NativeCodexPath        string
	SandboxHelperPath      string
	BubblewrapPath         string
	PermissionProfile      []byte
	ProbeFixedArgs         []string
	ExecFixedArgs          []string
	CommandFixedArgs       []string
	BootstrapDir           string
	ConfigHomeDir          string
	ManagedPackageRoot     string
	CanaryExecutablePath   string
	SentinelExecutablePath string
}

type PublicationCodexBoundaryV1 struct {
	manifest           PublicationCodexBoundaryManifest
	paths              map[PublicationExecutableRole]string
	profile            []byte
	fixed              map[PublicationLaunchKind][]string
	bootstrapPath      string
	bootstrapEntry     string
	configHome         string
	operatorHome       string
	managedPackageRoot string
}

type publicationDirectoryBinding struct {
	path  string
	mode  os.FileMode
	owner string
	info  os.FileInfo
}

// PublicationCodexView is an immutable, path-bound launch plan. It is the
// only public execution surface of PublicationCodexBoundaryV1: callers never
// append raw sandbox/config arguments.
type PublicationCodexView struct {
	boundary     *PublicationCodexBoundaryV1
	candidateDir string
	sourceDir    string
	scratchDir   string
	profileArgs  []string
	profileSHA   string
	candidate    publicationDirectoryBinding
	source       publicationDirectoryBinding
	scratch      publicationDirectoryBinding
	configHome   publicationDirectoryBinding
	allowCanary  bool
}

type PublicationCodexProbeOptions struct {
	CandidateDir   string
	SourceDir      string
	ScratchDir     string
	SiblingFile    string
	SourceFile     string
	TCPAddress     string
	UnixSocketPath string
}

type PublicationConfinementCanaryConfig struct {
	CanaryBinding        PublicationExecutableBinding
	CandidateDir         string
	SourceFile           string
	SiblingFile          string
	ScratchDir           string
	ReadyMarker          string
	LateMarker           string
	TCPAddress           string
	UnixSocketPath       string
	HomeDir              string
	ForbiddenExecutables []string
	Delay                time.Duration
}

type publicationLaunchWitness struct {
	namespaceIdentity  string
	namespaceInitPID   int
	namespaceInitStart string
	detachedPID        int
	detachedStart      string
	sentinelPID        int
	sentinelStart      string
	sentinelState      string
	sentinelCmd        *exec.Cmd
}

type publicationPreparedCommand struct {
	*exec.Cmd
	releaseReader *os.File
	releaseWriter *os.File
	infoReader    *os.File
	infoWriter    *os.File
}

func NewPublicationCodexBoundaryV1(ctx context.Context, opts PublicationCodexBoundaryOptions) (*PublicationCodexBoundaryV1, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	roles := []struct {
		role     PublicationExecutableRole
		path     string
		optional bool
	}{
		{PublicationExecutableLogicalEntry, opts.LogicalEntryPath, false},
		{PublicationExecutableInterpreter, opts.InterpreterPath, true},
		{PublicationExecutableNativeCodex, opts.NativeCodexPath, false},
		{PublicationExecutableSandboxHelper, opts.SandboxHelperPath, true},
		{PublicationExecutableBubblewrap, opts.BubblewrapPath, false},
		{PublicationExecutableCanary, opts.CanaryExecutablePath, true},
		{PublicationExecutableSentinel, opts.SentinelExecutablePath, false},
	}
	closure := make([]PublicationExecutableBinding, 0, len(roles))
	paths := make(map[PublicationExecutableRole]string, len(roles))
	for _, candidate := range roles {
		if strings.TrimSpace(candidate.path) == "" && candidate.optional {
			continue
		}
		binding, err := bindPublicationExecutable(candidate.role, candidate.path)
		if err != nil {
			return nil, fmt.Errorf("%w: bind %s: %v", ErrPublicationConfinementUnavailable, candidate.role, err)
		}
		closure = append(closure, binding)
		paths[candidate.role] = binding.RealPath
	}
	if len(opts.PermissionProfile) == 0 || len(opts.ProbeFixedArgs) == 0 || len(opts.ExecFixedArgs) == 0 || len(opts.CommandFixedArgs) == 0 ||
		strings.TrimSpace(opts.BootstrapDir) == "" || strings.TrimSpace(opts.ConfigHomeDir) == "" {
		return nil, fmt.Errorf("%w: publication policy and all fixed argv forms are required", ErrPublicationConfinementUnavailable)
	}
	operatorHome, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("%w: resolve operator home: %v", ErrPublicationConfinementUnavailable, err)
	}
	operatorHome, err = canonicalPublicationDirectory(operatorHome)
	if err != nil {
		return nil, fmt.Errorf("%w: canonicalize operator home: %v", ErrPublicationConfinementUnavailable, err)
	}
	native := paths[PublicationExecutableNativeCodex]
	versionCmd := exec.CommandContext(ctx, native, "--version")
	versionCmd.Env = []string{"PATH=" + filepath.Dir(paths[PublicationExecutableBubblewrap])}
	versionRaw, err := versionCmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%w: read native Codex version: %v", ErrPublicationConfinementUnavailable, err)
	}
	version := strings.TrimSpace(string(versionRaw))
	if version == "" || strings.ContainsAny(version, "\r\n") {
		return nil, fmt.Errorf("%w: native Codex version is empty or malformed", ErrPublicationConfinementUnavailable)
	}
	b := &PublicationCodexBoundaryV1{
		paths:        paths,
		profile:      append([]byte(nil), opts.PermissionProfile...),
		operatorHome: operatorHome,
		fixed: map[PublicationLaunchKind][]string{
			PublicationLaunchProbe:   append([]string(nil), opts.ProbeFixedArgs...),
			PublicationLaunchExec:    append([]string(nil), opts.ExecFixedArgs...),
			PublicationLaunchCommand: append([]string(nil), opts.CommandFixedArgs...),
		},
		managedPackageRoot: strings.TrimSpace(opts.ManagedPackageRoot),
	}
	bootstrapPath, bootstrapEntry, err := preparePublicationBootstrap(opts.BootstrapDir, paths[PublicationExecutableBubblewrap])
	if err != nil {
		return nil, fmt.Errorf("%w: prepare closed bubblewrap bootstrap: %v", ErrPublicationConfinementUnavailable, err)
	}
	b.bootstrapPath = bootstrapPath
	b.bootstrapEntry = bootstrapEntry
	configHome, err := preparePublicationConfigHome(opts.ConfigHomeDir)
	if err != nil {
		return nil, fmt.Errorf("%w: prepare private Codex config home: %v", ErrPublicationConfinementUnavailable, err)
	}
	b.configHome = configHome
	b.manifest = PublicationCodexBoundaryManifest{
		ExecutableClosure:    clonePublicationClosure(closure),
		NativeVersion:        version,
		GOOS:                 runtime.GOOS,
		GOARCH:               runtime.GOARCH,
		PolicyTemplateSHA256: sha256Hex(opts.PermissionProfile),
		ProbeArgvSHA256:      hashArgv(append(publicationLifecyclePrefix(native), opts.ProbeFixedArgs...)),
		ExecArgvSHA256:       hashArgv(append(publicationLifecyclePrefix(native), opts.ExecFixedArgs...)),
		CommandArgvSHA256:    hashArgv(append(publicationLifecyclePrefix(native), opts.CommandFixedArgs...)),
	}
	return b, nil
}

func (b *PublicationCodexBoundaryV1) Manifest() PublicationCodexBoundaryManifest {
	if b == nil {
		return PublicationCodexBoundaryManifest{}
	}
	manifest := b.manifest
	manifest.ExecutableClosure = clonePublicationClosure(manifest.ExecutableClosure)
	return manifest
}

func (b *PublicationCodexBoundaryV1) RevalidateForLaunch(kind PublicationLaunchKind) error {
	if b == nil {
		return fmt.Errorf("%w: publication boundary is nil", ErrPublicationConfinementUnavailable)
	}
	args, ok := b.fixed[kind]
	if !ok || len(args) == 0 {
		return fmt.Errorf("%w: unknown or empty launch kind %q", ErrPublicationConfinementUnavailable, kind)
	}
	for _, want := range b.manifest.ExecutableClosure {
		path := want.LogicalPath
		got, err := bindPublicationExecutable(want.Role, path)
		if err != nil || got.LogicalPath != want.LogicalPath || got.RealPath != want.RealPath || got.RawSHA256 != want.RawSHA256 ||
			got.Mode != want.Mode || got.FileIdentity != want.FileIdentity || got.LinkIdentity != want.LinkIdentity || got.OwnerIdentity != want.OwnerIdentity ||
			!os.SameFile(want.info, got.info) {
			return fmt.Errorf("%w: executable closure member %s changed", ErrPublicationConfinementUnavailable, want.Role)
		}
	}
	if err := verifyPublicationBootstrap(b.bootstrapPath, b.bootstrapEntry, b.paths[PublicationExecutableBubblewrap]); err != nil {
		return fmt.Errorf("%w: closed bubblewrap bootstrap changed: %v", ErrPublicationConfinementUnavailable, err)
	}
	if err := verifyPublicationConfigHome(b.configHome); err != nil {
		return fmt.Errorf("%w: private Codex config home changed: %v", ErrPublicationConfinementUnavailable, err)
	}
	if sha256Hex(b.profile) != b.manifest.PolicyTemplateSHA256 ||
		hashArgv(append(publicationLifecyclePrefix(b.paths[PublicationExecutableNativeCodex]), b.fixed[PublicationLaunchProbe]...)) != b.manifest.ProbeArgvSHA256 ||
		hashArgv(append(publicationLifecyclePrefix(b.paths[PublicationExecutableNativeCodex]), b.fixed[PublicationLaunchExec]...)) != b.manifest.ExecArgvSHA256 ||
		hashArgv(append(publicationLifecyclePrefix(b.paths[PublicationExecutableNativeCodex]), b.fixed[PublicationLaunchCommand]...)) != b.manifest.CommandArgvSHA256 {
		return fmt.Errorf("%w: publication policy or fixed argv changed", ErrPublicationConfinementUnavailable)
	}
	versionCmd := exec.Command(b.paths[PublicationExecutableNativeCodex], "--version")
	versionCmd.Env = b.hostEnv(false, b.configHome)
	versionRaw, err := versionCmd.Output()
	if err != nil || strings.TrimSpace(string(versionRaw)) != b.manifest.NativeVersion {
		return fmt.Errorf("%w: native Codex version changed", ErrPublicationConfinementUnavailable)
	}
	return nil
}

func (b *PublicationCodexBoundaryV1) commandForLaunch(ctx context.Context, kind PublicationLaunchKind, cwd string, args []string, hostCredentials bool, launchConfigHome string) (*publicationPreparedCommand, error) {
	if err := b.RevalidateForLaunch(kind); err != nil {
		return nil, err
	}
	if strings.TrimSpace(cwd) == "" {
		return nil, fmt.Errorf("%w: launch cwd is empty", ErrPublicationConfinementUnavailable)
	}
	native := b.paths[PublicationExecutableNativeCodex]
	bwrapArgs := publicationLifecyclePrefix(native)
	bwrapArgs = append(bwrapArgs, args...)
	cmd := exec.CommandContext(ctx, b.paths[PublicationExecutableBubblewrap], bwrapArgs...)
	cmd.Dir = cwd
	cmd.Env = b.hostEnv(hostCredentials, launchConfigHome)
	return &publicationPreparedCommand{Cmd: cmd}, nil
}

func publicationLifecyclePrefix(native string) []string {
	return []string{
		"--unshare-pid", "--unshare-ipc", "--unshare-uts", "--die-with-parent", "--new-session",
		"--bind", "/", "/", "--proc", "/proc", "--block-fd", "3", "--info-fd", "4", "--", native,
	}
}

func productionPublicationProbeFixedArgs() []string {
	return []string{"sandbox"}
}

func productionPublicationExecFixedArgs() []string {
	return []string{
		"exec", "--ephemeral", "--ignore-user-config", "--ignore-rules", "--strict-config", "--json", "--color", "never",
		"-c", "project_doc_max_bytes=0", "-c", "mcp_servers={}", "-c", `approval_policy="never"`,
		"-c", `web_search="disabled"`, "-c", "features.apps=false", "-c", "features.auth_elicitation=false",
		"-c", "features.browser_use=false", "-c", "features.browser_use_external=false",
		"-c", "features.browser_use_full_cdp_access=false", "-c", "features.computer_use=false",
		"-c", "features.enable_mcp_apps=false", "-c", "features.in_app_browser=false",
		"-c", "features.plugins=false", "-c", "features.plugin_sharing=false", "-c", "features.remote_plugin=false",
	}
}

func productionPublicationCommandFixedArgs() []string {
	return []string{"sandbox"}
}

// BindView canonicalizes the only three repository paths which may influence
// a protected launch and renders one closed permission profile for them.
func (b *PublicationCodexBoundaryV1) BindView(candidateDir, sourceDir, scratchDir string) (*PublicationCodexView, error) {
	return b.bindView(candidateDir, sourceDir, scratchDir, false)
}

func (b *PublicationCodexBoundaryV1) bindProbeView(candidateDir, sourceDir, scratchDir string) (*PublicationCodexView, error) {
	return b.bindView(candidateDir, sourceDir, scratchDir, true)
}

func (b *PublicationCodexBoundaryV1) bindView(candidateDir, sourceDir, scratchDir string, allowCanary bool) (*PublicationCodexView, error) {
	if b == nil {
		return nil, fmt.Errorf("%w: publication boundary is nil", ErrPublicationConfinementUnavailable)
	}
	candidate, err := canonicalPublicationDirectory(candidateDir)
	if err != nil {
		return nil, fmt.Errorf("%w: candidate: %v", ErrPublicationConfinementUnavailable, err)
	}
	source, err := canonicalPublicationDirectory(sourceDir)
	if err != nil {
		return nil, fmt.Errorf("%w: source: %v", ErrPublicationConfinementUnavailable, err)
	}
	scratch, err := canonicalPublicationDirectory(scratchDir)
	if err != nil {
		return nil, fmt.Errorf("%w: scratch: %v", ErrPublicationConfinementUnavailable, err)
	}
	if candidate == source || candidate == scratch || source == scratch || filepath.Dir(candidate) != filepath.Dir(scratch) {
		return nil, fmt.Errorf("%w: candidate, source and scratch must be distinct and candidate/scratch must share one private container", ErrPublicationConfinementUnavailable)
	}
	candidateBinding, err := bindPublicationDirectory(candidate)
	if err != nil {
		return nil, fmt.Errorf("%w: bind candidate directory: %v", ErrPublicationConfinementUnavailable, err)
	}
	sourceBinding, err := bindPublicationDirectory(source)
	if err != nil {
		return nil, fmt.Errorf("%w: bind source directory: %v", ErrPublicationConfinementUnavailable, err)
	}
	scratchBinding, err := bindPublicationDirectory(scratch)
	if err != nil {
		return nil, fmt.Errorf("%w: bind scratch directory: %v", ErrPublicationConfinementUnavailable, err)
	}
	configHome, err := os.MkdirTemp(scratch, ".codex-host-")
	if err != nil {
		return nil, fmt.Errorf("%w: create launch-specific Codex config home: %v", ErrPublicationConfinementUnavailable, err)
	}
	if err := os.Chmod(configHome, 0o700); err != nil {
		return nil, fmt.Errorf("%w: protect launch-specific Codex config home: %v", ErrPublicationConfinementUnavailable, err)
	}
	configHomeBinding, err := bindPublicationDirectory(configHome)
	if err != nil {
		return nil, fmt.Errorf("%w: bind launch-specific Codex config home: %v", ErrPublicationConfinementUnavailable, err)
	}
	profileArgs := b.renderPublicationProfileArgs(candidate, source, scratch, allowCanary)
	return &PublicationCodexView{
		boundary: b, candidateDir: candidate, sourceDir: source, scratchDir: scratch,
		profileArgs: profileArgs, profileSHA: hashArgv(profileArgs), allowCanary: allowCanary,
		candidate: candidateBinding, source: sourceBinding, scratch: scratchBinding, configHome: configHomeBinding,
	}, nil
}

func (b *PublicationCodexBoundaryV1) renderPublicationProfileArgs(candidate, source, scratch string, allowCanary bool) []string {
	entries := map[string]string{
		filepath.Dir(candidate): "deny", candidate: "read", scratch: "write", source: "deny", b.operatorHome: "deny",
	}
	for _, binding := range b.manifest.ExecutableClosure {
		entries[binding.RealPath] = "deny"
	}
	if canary := b.paths[PublicationExecutableCanary]; allowCanary && canary != "" {
		entries[canary] = "read"
	}
	filesystem := canonicalPermissionFilesystem(entries)
	return []string{
		"-c", `default_permissions="publication"`,
		"-c", `permissions.publication.extends=":read-only"`,
		"-c", "permissions.publication.filesystem=" + filesystem,
		"-c", "permissions.publication.network.enabled=false",
		"-c", `shell_environment_policy.inherit="none"`,
		"-c", "shell_environment_policy.set=" + canonicalShellEnvironment(scratch),
		"-c", "allow_login_shell=false",
	}
}

func (v *PublicationCodexView) CandidateDir() string { return v.candidateDir }
func (v *PublicationCodexView) SourceDir() string    { return v.sourceDir }
func (v *PublicationCodexView) ScratchDir() string   { return v.scratchDir }
func (v *PublicationCodexView) PolicySHA256() string { return v.profileSHA }

func (v *PublicationCodexView) revalidate() error {
	if v == nil || v.boundary == nil {
		return fmt.Errorf("%w: publication view is nil", ErrPublicationConfinementUnavailable)
	}
	for name, want := range map[string]publicationDirectoryBinding{"candidate": v.candidate, "source": v.source, "scratch": v.scratch} {
		got, err := bindPublicationDirectory(want.path)
		if err != nil || got.path != want.path || got.mode != want.mode || got.owner != want.owner || !os.SameFile(want.info, got.info) {
			return fmt.Errorf("%w: bound %s path changed", ErrPublicationConfinementUnavailable, name)
		}
	}
	gotConfigHome, err := bindPublicationDirectory(v.configHome.path)
	if err != nil || gotConfigHome.path != v.configHome.path || gotConfigHome.mode != v.configHome.mode ||
		gotConfigHome.owner != v.configHome.owner || !os.SameFile(v.configHome.info, gotConfigHome.info) {
		return fmt.Errorf("%w: launch-specific Codex config home changed", ErrPublicationConfinementUnavailable)
	}
	if entries, err := os.ReadDir(v.configHome.path); err != nil || len(entries) != 0 {
		return fmt.Errorf("%w: launch-specific Codex config home is not empty", ErrPublicationConfinementUnavailable)
	}
	want := v.boundary.renderPublicationProfileArgs(v.candidateDir, v.sourceDir, v.scratchDir, v.allowCanary)
	if !equalPublicationArgv(want, v.profileArgs) || hashArgv(v.profileArgs) != v.profileSHA {
		return fmt.Errorf("%w: bound publication policy changed", ErrPublicationConfinementUnavailable)
	}
	return nil
}

func (b *PublicationCodexBoundaryV1) CanaryExecutable() string {
	if b == nil {
		return ""
	}
	return b.paths[PublicationExecutableCanary]
}

func (v *PublicationCodexView) AgentCommand(ctx context.Context, schemaPath string) (*publicationPreparedCommand, error) {
	if err := v.revalidate(); err != nil {
		return nil, err
	}
	args := append([]string(nil), v.boundary.fixed[PublicationLaunchExec]...)
	args = append(args, v.profileArgs...)
	args = append(args, "-C", v.candidateDir, "-")
	if schemaPath != "" {
		canonical, err := canonicalPublicationFile(schemaPath, v.scratchDir)
		if err != nil {
			return nil, fmt.Errorf("%w: output schema: %v", ErrPublicationConfinementUnavailable, err)
		}
		args = append(args, "--output-schema", canonical)
	}
	return v.boundary.commandForLaunch(ctx, PublicationLaunchExec, v.candidateDir, args, true, "")
}

func (v *PublicationCodexView) SandboxCommand(ctx context.Context, payload []string) (*publicationPreparedCommand, error) {
	if err := v.revalidate(); err != nil {
		return nil, err
	}
	if len(payload) == 0 {
		return nil, fmt.Errorf("%w: publication sandbox payload is empty", ErrPublicationConfinementUnavailable)
	}
	args := append([]string(nil), v.boundary.fixed[PublicationLaunchCommand]...)
	args = append(args, v.profileArgs...)
	args = append(args, "-P", "publication", "-C", v.candidateDir, "--")
	args = append(args, payload...)
	return v.boundary.commandForLaunch(ctx, PublicationLaunchCommand, v.candidateDir, args, false, v.configHome.path)
}

func (v *PublicationCodexView) probeCommand(ctx context.Context, payload []string) (*publicationPreparedCommand, error) {
	if err := v.revalidate(); err != nil {
		return nil, err
	}
	if len(payload) == 0 {
		return nil, fmt.Errorf("%w: publication probe payload is empty", ErrPublicationConfinementUnavailable)
	}
	args := append([]string(nil), v.boundary.fixed[PublicationLaunchProbe]...)
	args = append(args, v.profileArgs...)
	args = append(args, "-P", "publication", "-C", v.candidateDir, "--")
	args = append(args, payload...)
	return v.boundary.commandForLaunch(ctx, PublicationLaunchProbe, v.candidateDir, args, false, v.configHome.path)
}

func (v *PublicationCodexView) RunConfiguredCommand(ctx context.Context, command string) (string, int, error) {
	if strings.TrimSpace(command) == "" {
		return "", -1, fmt.Errorf("%w: configured command is empty", ErrPublicationConfinementUnavailable)
	}
	cmd, err := v.SandboxCommand(ctx, []string{"/bin/sh", "-c", command})
	if err != nil {
		return "", -1, err
	}
	output := &boundedOutputBuffer{limit: 4 << 20}
	cmd.Stdout = output
	cmd.Stderr = output
	shellenv.ConfigureShellCommand(cmd.Cmd)
	if err := cmd.armLifecycleBarrier(); err != nil {
		return "", -1, err
	}
	if err := cmd.Start(); err != nil {
		cmd.abortLifecycleBarrier()
		return "", -1, fmt.Errorf("start protected configured command: %w", err)
	}
	witness, observeErr := cmd.bindAndReleaseLifecycle(v.boundary.paths[PublicationExecutableSentinel])
	if observeErr != nil {
		shellenv.TerminateShellCommandGroup(cmd.Cmd)
		_ = cmd.Wait()
		cmd.abortLifecycleBarrier()
		return "", -1, observeErr
	}
	err = cmd.Wait()
	if teardownErr := verifyPublicationLaunchTeardown(witness); teardownErr != nil {
		return "", -1, teardownErr
	}
	if output.exceeded {
		return "", -1, fmt.Errorf("%w: configured command output exceeded bound", ErrPublicationConfinementUnavailable)
	}
	if err == nil {
		return output.String(), 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return output.String(), exitErr.ExitCode(), nil
	}
	return "", -1, fmt.Errorf("run protected configured command: %w", err)
}

func (b *PublicationCodexBoundaryV1) observeStartedLaunch(cmd *publicationPreparedCommand, pid int) (*publicationLaunchWitness, error) {
	if b == nil || cmd == nil || pid < 1 {
		return nil, fmt.Errorf("%w: protected launch PID is unavailable", ErrPublicationConfinementCleanupUncertain)
	}
	return cmd.bindAndReleaseLifecycle(b.paths[PublicationExecutableSentinel])
}

func (b *PublicationCodexBoundaryV1) verifyLaunchTeardown(witness *publicationLaunchWitness) error {
	if b == nil {
		return fmt.Errorf("%w: publication boundary is unavailable during teardown", ErrPublicationConfinementCleanupUncertain)
	}
	return verifyPublicationLaunchTeardown(witness)
}

func (b *PublicationCodexBoundaryV1) hostEnv(credentials bool, launchConfigHome string) []string {
	if !credentials && launchConfigHome == "" {
		launchConfigHome = b.configHome
	}
	env := publicationCodexHostEnv(b.bootstrapPath, launchConfigHome, credentials)
	if b.managedPackageRoot != "" {
		env = append(env, "CODEX_MANAGED_PACKAGE_ROOT="+b.managedPackageRoot)
	}
	sort.Strings(env[1:])
	return env
}

func (b *PublicationCodexBoundaryV1) Probe(ctx context.Context, opts PublicationCodexProbeOptions) error {
	return probePublicationCodexBoundary(ctx, b, opts)
}

func bindPublicationExecutable(role PublicationExecutableRole, path string) (PublicationExecutableBinding, error) {
	if strings.TrimSpace(path) == "" || !filepath.IsAbs(path) {
		return PublicationExecutableBinding{}, fmt.Errorf("path must be absolute")
	}
	logicalPath := filepath.Clean(path)
	linkInfo, err := os.Lstat(logicalPath)
	if err != nil {
		return PublicationExecutableBinding{}, err
	}
	realPath, err := filepath.EvalSymlinks(logicalPath)
	if err != nil {
		return PublicationExecutableBinding{}, err
	}
	info, err := os.Stat(realPath)
	if err != nil {
		return PublicationExecutableBinding{}, err
	}
	if !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return PublicationExecutableBinding{}, fmt.Errorf("path is not an executable regular file")
	}
	raw, err := os.ReadFile(realPath)
	if err != nil {
		return PublicationExecutableBinding{}, err
	}
	identity := fmt.Sprintf("%d:%d:%d:%d", info.Size(), info.Mode(), info.ModTime().UnixNano(), len(raw))
	return PublicationExecutableBinding{
		Role: role, LogicalPath: logicalPath, RealPath: realPath, RawSHA256: sha256Hex(raw), Mode: info.Mode(),
		FileIdentity: identity, LinkIdentity: fileInfoIdentity(linkInfo), OwnerIdentity: publicationExecutableOwner(info), info: info,
	}, nil
}

func publicationExecutableOwner(info os.FileInfo) string {
	if info == nil || info.Sys() == nil {
		return ""
	}
	value := reflect.ValueOf(info.Sys())
	if value.Kind() == reflect.Pointer {
		value = value.Elem()
	}
	if !value.IsValid() || value.Kind() != reflect.Struct {
		return ""
	}
	uid := value.FieldByName("Uid")
	gid := value.FieldByName("Gid")
	if !uid.IsValid() || !gid.IsValid() || !uid.CanUint() || !gid.CanUint() {
		return ""
	}
	return fmt.Sprintf("%d:%d", uid.Uint(), gid.Uint())
}

func clonePublicationClosure(in []PublicationExecutableBinding) []PublicationExecutableBinding {
	out := make([]PublicationExecutableBinding, len(in))
	copy(out, in)
	return out
}

func sha256Hex(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func hashArgv(args []string) string {
	var buf bytes.Buffer
	for _, arg := range args {
		buf.WriteString(fmt.Sprintf("%d:", len(arg)))
		buf.WriteString(arg)
		buf.WriteByte(0)
	}
	return sha256Hex(buf.Bytes())
}

func equalPublicationArgv(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func publicationCodexHostEnv(bootstrapPath, configHome string, credentials bool) []string {
	keep := map[string]bool{
		"USER": true, "LOGNAME": true, "SSL_CERT_FILE": true, "SSL_CERT_DIR": true,
	}
	if credentials {
		keep["HOME"] = true
		for _, key := range []string{"CODEX_HOME", "OPENAI_API_KEY", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY"} {
			keep[key] = true
		}
	}
	env := []string{"PATH=" + bootstrapPath}
	if !credentials {
		env = append(env, "HOME="+configHome, "CODEX_HOME="+configHome)
	}
	for _, entry := range os.Environ() {
		key, _, ok := strings.Cut(entry, "=")
		if ok && keep[key] {
			env = append(env, entry)
		}
	}
	sort.Strings(env[1:])
	return env
}

func canonicalPublicationDirectory(path string) (string, error) {
	if strings.TrimSpace(path) == "" || !filepath.IsAbs(path) {
		return "", fmt.Errorf("path must be absolute")
	}
	clean := filepath.Clean(path)
	real, err := filepath.EvalSymlinks(clean)
	if err != nil || !filepath.IsAbs(real) {
		return "", fmt.Errorf("path must be an existing canonical directory")
	}
	info, err := os.Lstat(clean)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("path must be a real directory")
	}
	return filepath.Clean(real), nil
}

func bindPublicationDirectory(path string) (publicationDirectoryBinding, error) {
	canonical, err := canonicalPublicationDirectory(path)
	if err != nil {
		return publicationDirectoryBinding{}, err
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return publicationDirectoryBinding{}, err
	}
	owner, err := publicationDirectoryOwner(info)
	if err != nil {
		return publicationDirectoryBinding{}, err
	}
	return publicationDirectoryBinding{path: canonical, mode: info.Mode(), owner: owner, info: info}, nil
}

func publicationDirectoryOwner(info os.FileInfo) (string, error) {
	if info == nil {
		return "", fmt.Errorf("file identity is unavailable")
	}
	value := reflect.ValueOf(info.Sys())
	if value.IsValid() && value.Kind() == reflect.Pointer && !value.IsNil() {
		value = value.Elem()
	}
	if value.IsValid() && value.Kind() == reflect.Struct {
		uid := value.FieldByName("Uid")
		if uid.IsValid() && uid.CanUint() {
			return strconv.FormatUint(uid.Uint(), 10), nil
		}
	}
	return publicationCurrentOwner(), nil
}

func publicationCurrentOwner() string {
	current, err := user.Current()
	if err != nil {
		return ""
	}
	return current.Uid
}

func canonicalPublicationFile(path, root string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("path must be absolute")
	}
	clean := filepath.Clean(path)
	real, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return "", fmt.Errorf("path must exist")
	}
	rel, err := filepath.Rel(root, real)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path must be a child of bound scratch")
	}
	info, err := os.Lstat(clean)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("path must be an existing regular non-symlink file")
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(clean))
	if err != nil || parent != filepath.Dir(real) {
		return "", fmt.Errorf("file parent must resolve inside bound scratch")
	}
	return real, nil
}

func canonicalPermissionFilesystem(entries map[string]string) string {
	keys := make([]string, 0, len(entries))
	for key := range entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, strconv.Quote(key)+"="+strconv.Quote(entries[key]))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func canonicalShellEnvironment(scratch string) string {
	values := map[string]string{
		"HOME": scratch, "PATH": "/usr/bin:/bin", "TMP": scratch, "TMPDIR": scratch, "TEMP": scratch,
	}
	return canonicalPermissionFilesystem(values)
}

func fileInfoIdentity(info os.FileInfo) string {
	if info == nil {
		return ""
	}
	return fmt.Sprintf("%d:%d:%d", info.Size(), info.Mode(), info.ModTime().UnixNano())
}

func preparePublicationBootstrap(root, bwrap string) (string, string, error) {
	if !filepath.IsAbs(root) {
		return "", "", fmt.Errorf("bootstrap path must be absolute")
	}
	if err := os.Mkdir(root, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", "", err
	}
	info, err := os.Lstat(root)
	owner, ownerErr := publicationDirectoryOwner(info)
	if err != nil || ownerErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 || owner != publicationCurrentOwner() {
		return "", "", fmt.Errorf("bootstrap root must be a private real 0700 directory")
	}
	entry := filepath.Join(root, "bwrap")
	if _, err := os.Lstat(entry); errors.Is(err, os.ErrNotExist) {
		if err := os.Symlink(bwrap, entry); err != nil {
			return "", "", err
		}
	} else if err != nil {
		return "", "", err
	}
	if err := verifyPublicationBootstrap(root, entry, bwrap); err != nil {
		return "", "", err
	}
	return root, entry, nil
}

func verifyPublicationBootstrap(root, entry, bwrap string) error {
	if entry == "" {
		return nil
	}
	info, err := os.Lstat(root)
	owner, ownerErr := publicationDirectoryOwner(info)
	if err != nil || ownerErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 || owner != publicationCurrentOwner() {
		return fmt.Errorf("bootstrap root changed")
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || entries[0].Name() != "bwrap" {
		return fmt.Errorf("bootstrap root is not one-entry closed")
	}
	target, err := filepath.EvalSymlinks(entry)
	if err != nil {
		return err
	}
	want, err := filepath.EvalSymlinks(bwrap)
	if err != nil || target != want {
		return fmt.Errorf("bootstrap bwrap target changed")
	}
	return nil
}

func preparePublicationConfigHome(root string) (string, error) {
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("config home must be absolute")
	}
	if err := os.Mkdir(root, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	if err := verifyPublicationConfigHome(root); err != nil {
		return "", err
	}
	return root, nil
}

func verifyPublicationConfigHome(root string) error {
	info, err := os.Lstat(root)
	if err != nil {
		return err
	}
	owner, ownerErr := publicationDirectoryOwner(info)
	if ownerErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 || owner != publicationCurrentOwner() {
		return fmt.Errorf("config home must be a private real owner-bound 0700 directory")
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		return fmt.Errorf("config home must remain empty")
	}
	return nil
}

func boundedReadAll(reader io.Reader, limit int64) ([]byte, error) {
	limited := io.LimitReader(reader, limit+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("output exceeds %d-byte bound", limit)
	}
	return raw, nil
}

type boundedOutputBuffer struct {
	buf      bytes.Buffer
	limit    int
	exceeded bool
}

func (b *boundedOutputBuffer) Write(p []byte) (int, error) {
	if b.exceeded {
		return len(p), nil
	}
	remaining := b.limit - b.buf.Len()
	if remaining <= 0 {
		b.exceeded = true
		return len(p), nil
	}
	if len(p) > remaining {
		_, _ = b.buf.Write(p[:remaining])
		b.exceeded = true
		return len(p), nil
	}
	_, _ = b.buf.Write(p)
	return len(p), nil
}

func (b *boundedOutputBuffer) String() string { return b.buf.String() }

const publicationBoundaryWait = 10 * time.Second
