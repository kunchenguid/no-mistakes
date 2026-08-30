package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/runenv"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

// piAgent spawns the pi CLI for each invocation. Pi reads its prompt from
// stdin and emits JSONL on stdout when --mode json is set. The lifecycle is
// codex-shaped: one process per Run, no managed server.
type piAgent struct {
	bin       string
	extraArgs []string
	// modelSource names the no-mistakes setting that supplied --model. Empty
	// means Pi's own settings.json default is authoritative when present.
	modelSource string
	// disableProjectSettings is the resolved, trusted-only opt-out. When true,
	// buildArgs suppresses pi's project-level AGENTS.md/CLAUDE.md discovery.
	disableProjectSettings bool
	subprocessContext
}

func (a *piAgent) Name() string { return "pi" }

// SupportsSessionResume reports Pi's durable-session capability: JSON mode
// emits a session header with its UUID, and `--session <uuid>` reopens it.
func (a *piAgent) SupportsSessionResume() bool { return true }

func (a *piAgent) ReportsAgentAttempts() bool { return true }

// ValidateConfiguration checks the configured Pi model inside the run worktree
// before pipeline steps start, using the resolution semantics Pi applies to
// each source. An explicit --model pattern is resolved by Pi's own CLI
// resolver (exact ids, case-insensitive substrings, display-name matches,
// aliases, and deterministic latest-version ambiguity) through a throwaway RPC
// session with no prompt. Pi is probed against its offline catalogue first so
// a cached answer costs no network access; an offline miss is re-probed
// through Pi's online resolution, and setup fails only when pi's model
// catalogue is reachable and pi still rejects the pattern there, because an
// unreachable catalogue means the probe cannot verify the model at all. A
// settings default instead follows Pi's provider-plus-model startup path,
// which silently falls back on anything it cannot use, so a default that is
// absent, inert, or missing from Pi's catalogue only produces a warning naming
// the settings file. Project packages and extensions that register models are
// part of every check because both run in the worktree.
func (a *piAgent) ValidateConfiguration(ctx context.Context, workDir string) error {
	model := piFlagValue(a.extraArgs, "--model")
	provider := piFlagValue(a.extraArgs, "--provider")
	source := a.modelSource
	explicit := model != ""
	if explicit && source == "" {
		source = "Pi --model option"
	}
	if !explicit {
		selection, trust := piSettingsModel(workDir, a.extraArgs, a.overlay())
		if selection.model == "" {
			return nil
		}
		if selection.project && trust == piTrustUndetermined {
			slog.Warn("pi project settings default may be inert: Pi has not recorded a trust decision for this worktree and may ignore the project copy and fall back to another model",
				"file", selection.source, "model", piQualifiedModel(selection.provider, selection.model))
		}
		if piCatalogueHasModel(selection.provider, selection.model, a.piModelCatalogue(ctx, workDir)) {
			return nil
		}
		slog.Warn("pi settings default model is not in pi's model catalogue; pi will fall back to another model at startup",
			"file", selection.source, "model", piQualifiedModel(selection.provider, selection.model))
		return nil
	}

	// Pi's offline catalogue is only a cache: an online run refreshes it from
	// the network and falls back silently when that is impossible. Probe
	// offline first so a cached answer costs no network access, and treat an
	// offline miss as stale rather than fatal until pi's online resolution
	// confirms it.
	offline := a.probeModelResolution(ctx, workDir, true)
	if offline.resolved {
		return nil
	}
	if !offline.rejected {
		return fmt.Errorf("validate pi model %q from %s: %s", piQualifiedModel(provider, model), source, offline.detail)
	}
	online := a.probeModelResolution(ctx, workDir, false)
	if online.resolved {
		return nil
	}
	if online.rejected && piCatalogReachable() {
		return fmt.Errorf("validate pi model %q from %s: %s", piQualifiedModel(provider, model), source, online.detail)
	}
	slog.Warn("pi model catalogue verification was not possible; continuing without rejecting the model",
		"model", piQualifiedModel(provider, model), "source", source, "detail", online.detail)
	return nil
}

// piProbeOutcome classifies one model-resolution probe. Only a clean non-zero
// exit from pi itself is its verdict; a probe that could not run is not
// evidence about the model.
type piProbeOutcome struct {
	resolved bool
	rejected bool
	detail   string
}

// probeModelResolution runs one throwaway pi session that resolves the
// configured model and exits once stdin closes.
func (a *piAgent) probeModelResolution(ctx context.Context, workDir string, offline bool) piProbeOutcome {
	probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	args := append([]string(nil), a.extraArgs...)
	if offline {
		args = append(args, "--offline")
	}
	args = append(args, "--mode", "rpc", "--no-session")
	cmd := exec.CommandContext(probeCtx, a.bin, args...)
	cmd.Dir = workDir
	cmd.Env = a.overlay().Apply(nil)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if detail == "" {
				detail = exitErr.String()
			}
			return piProbeOutcome{rejected: true, detail: detail}
		}
		if probeCtx.Err() != nil {
			return piProbeOutcome{detail: fmt.Sprintf("pi model probe did not finish: %v", probeCtx.Err())}
		}
		return piProbeOutcome{detail: err.Error()}
	}
	return piProbeOutcome{resolved: true}
}

// piCatalogEndpoint is pi's default model-catalog host. pi refreshes its
// catalogue from there at online startup and falls back to its cache without
// saying so, so only a reachable catalog makes an online rejection trustworthy.
var piCatalogEndpoint = "pi.dev:443"

func piCatalogReachable() bool {
	conn, err := net.DialTimeout("tcp", piCatalogEndpoint, 3*time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

type piModelSelection struct {
	provider string
	model    string
	source   string
	// project reports whether the model value itself came from the run
	// worktree's project settings, whose copy Pi honors only for a project it
	// trusts.
	project bool
}

// piTrustState classifies whether Pi would honor the worktree's project
// settings for a trusted project.
type piTrustState int

const (
	piTrustUndetermined piTrustState = iota
	piTrustTrusted
	piTrustUntrusted
)

// piSettingsModel reads Pi's configured default model the way Pi merges its
// settings: the project's .pi/settings.json overrides the global agent
// settings.json per key. Pi's startup path resolves that default by exact
// provider and model id and silently falls back whenever the file cannot be
// loaded, the project copy is untrusted, or either key is missing, so every
// such case is reported as a warning naming the file and yields no model to
// validate instead of failing setup. The returned trust state says whether Pi
// would honor the project copy, so the caller can explain a project default
// that may be inert.
func piSettingsModel(workDir string, extraArgs []string, overlay runenv.Overlay) (piModelSelection, piTrustState) {
	read := func(path string) (string, string, string, error) {
		var settings struct {
			DefaultProvider     string `json:"defaultProvider"`
			DefaultModel        string `json:"defaultModel"`
			DefaultProjectTrust string `json:"defaultProjectTrust"`
		}
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			return "", "", "", nil
		}
		if err != nil {
			return "", "", "", fmt.Errorf("read Pi model setting from %s: %w", path, err)
		}
		if err := json.Unmarshal(data, &settings); err != nil {
			return "", "", "", fmt.Errorf("parse Pi model setting from %s: %w", path, err)
		}
		return strings.TrimSpace(settings.DefaultProvider), strings.TrimSpace(settings.DefaultModel), strings.TrimSpace(settings.DefaultProjectTrust), nil
	}

	globalPath := piGlobalSettingsPath(overlay)
	globalProvider, globalModel, defaultProjectTrust, err := read(globalPath)
	if err != nil {
		slog.Warn("ignoring Pi global settings Pi cannot load; pi proceeds with its own model fallback", "file", globalPath, "error", err)
		globalProvider, globalModel, defaultProjectTrust = "", "", ""
	}
	trust := piProjectTrust(workDir, extraArgs, overlay, defaultProjectTrust)

	projectPath := filepath.Join(workDir, ".pi", "settings.json")
	projectProvider, projectModel, _, projectErr := read(projectPath)
	if projectErr != nil {
		slog.Warn("ignoring Pi project settings Pi cannot load; the file is inert", "file", projectPath, "error", projectErr)
		projectProvider, projectModel = "", ""
	} else if trust == piTrustUntrusted {
		slog.Warn("ignoring Pi project settings: Pi does not trust this project, so the file is inert", "file", projectPath)
		projectProvider, projectModel = "", ""
	}

	model, provider := globalModel, globalProvider
	source := globalPath + " (defaultProvider/defaultModel)"
	project := false
	if projectModel != "" {
		model = projectModel
		source = projectPath + " (defaultProvider/defaultModel)"
		project = true
	}
	if projectProvider != "" {
		provider = projectProvider
	}
	if model == "" {
		return piModelSelection{}, trust
	}
	if provider == "" {
		// Pi's startup path uses defaultProvider and defaultModel together and
		// falls back to its first available model when either is missing.
		slog.Warn("ignoring Pi defaultModel without defaultProvider; pi will fall back to its first available model", "file", source, "model", model)
		return piModelSelection{}, trust
	}
	return piModelSelection{provider: provider, model: model, source: source, project: project}, trust
}

// piModelCatalogue lists the provider and model id columns of pi --list-models
// from inside the run worktree, where project packages and extensions can
// register additional models. A catalogue that cannot be produced leaves the
// settings-default check undetermined.
func (a *piAgent) piModelCatalogue(ctx context.Context, workDir string) []string {
	catalogueCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	args := append([]string(nil), a.extraArgs...)
	args = append(args, "--offline", "--list-models")
	cmd := exec.CommandContext(catalogueCtx, a.bin, args...)
	cmd.Dir = workDir
	cmd.Env = a.overlay().Apply(nil)
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var models []string
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || strings.EqualFold(fields[0], "provider") {
			continue
		}
		models = append(models, fields[0]+"/"+fields[1])
	}
	return models
}

// piCatalogueHasModel reports whether Pi's catalogue contains the exact
// provider and model id pair its startup default lookup requires.
func piCatalogueHasModel(provider, model string, catalogue []string) bool {
	for _, entry := range catalogue {
		if entry == provider+"/"+model {
			return true
		}
	}
	return false
}

// piProjectTrust classifies whether Pi would honor the worktree's project
// settings for a trusted project, mirroring Pi's own resolution order for a
// non-interactive run: the --approve/--no-approve override (last occurrence
// wins), the recorded decision for the worktree or its nearest ancestor in the
// agent trust store, and finally the global defaultProjectTrust setting. With
// none of those recorded, Pi's outcome depends on its trust prompt or project
// extensions, so trust is undetermined.
func piProjectTrust(workDir string, extraArgs []string, overlay runenv.Overlay, defaultProjectTrust string) piTrustState {
	override := piTrustUndetermined
	for i := 0; i < len(extraArgs); i++ {
		switch extraArgs[i] {
		case "--approve", "-a":
			override = piTrustTrusted
		case "--no-approve", "-na":
			override = piTrustUntrusted
		}
		if piArgTakesValue(extraArgs[i]) {
			i++
		}
	}
	if override != piTrustUndetermined {
		return override
	}
	if decision, ok := piTrustStoreDecision(piAgentDir(overlay), workDir); ok {
		if decision {
			return piTrustTrusted
		}
		return piTrustUntrusted
	}
	switch defaultProjectTrust {
	case "always":
		return piTrustTrusted
	case "never":
		return piTrustUntrusted
	}
	return piTrustUndetermined
}

// piTrustStoreDecision reads Pi's agent trust store (trust.json mapping
// canonical project paths to trust decisions) and reports the recorded
// decision for the worktree or its nearest ancestor. A store that cannot be
// read or parsed leaves the outcome undetermined.
func piTrustStoreDecision(agentDir, workDir string) (bool, bool) {
	data, err := os.ReadFile(filepath.Join(agentDir, "trust.json"))
	if err != nil {
		return false, false
	}
	var entries map[string]*bool
	if err := json.Unmarshal(data, &entries); err != nil {
		return false, false
	}
	cwd, err := filepath.Abs(workDir)
	if err != nil {
		return false, false
	}
	if resolved, linkErr := filepath.EvalSymlinks(cwd); linkErr == nil {
		cwd = resolved
	}
	for {
		if decision, ok := entries[cwd]; ok && decision != nil {
			return *decision, true
		}
		parent := filepath.Dir(cwd)
		if parent == cwd {
			return false, false
		}
		cwd = parent
	}
}

func piGlobalSettingsPath(overlay runenv.Overlay) string {
	return filepath.Join(piAgentDir(overlay), "settings.json")
}

func piAgentDir(overlay runenv.Overlay) string {
	agentDir := strings.TrimSpace(piEnvironmentValue(overlay, "PI_CODING_AGENT_DIR"))
	if agentDir == "" {
		home := strings.TrimSpace(piEnvironmentValue(overlay, "HOME"))
		if home == "" {
			home, _ = os.UserHomeDir()
		}
		agentDir = filepath.Join(home, ".pi", "agent")
	} else if strings.HasPrefix(agentDir, "~"+string(filepath.Separator)) {
		home := strings.TrimSpace(piEnvironmentValue(overlay, "HOME"))
		if home == "" {
			home, _ = os.UserHomeDir()
		}
		agentDir = filepath.Join(home, strings.TrimPrefix(agentDir, "~"+string(filepath.Separator)))
	}
	return agentDir
}

func piEnvironmentValue(overlay runenv.Overlay, key string) string {
	for configuredKey, value := range overlay.Set {
		if strings.EqualFold(configuredKey, key) {
			return value
		}
	}
	for _, unset := range overlay.Unset {
		if strings.EqualFold(unset, key) {
			return ""
		}
	}
	return os.Getenv(key)
}

func piFlagValue(args []string, flag string) string {
	value := ""
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == flag && i+1 < len(args) {
			value = strings.TrimSpace(args[i+1])
			i++
			continue
		}
		if strings.HasPrefix(arg, flag+"=") {
			value = strings.TrimSpace(strings.TrimPrefix(arg, flag+"="))
		}
	}
	return value
}

func piQualifiedModel(provider, model string) string {
	model = strings.TrimSpace(model)
	provider = strings.TrimSpace(provider)
	if provider == "" || strings.HasPrefix(model, provider+"/") {
		return model
	}
	return provider + "/" + model
}

// piClassifyTransient keeps a clean Pi payload that failed the
// structured-output boundary terminal: that payload is the evidence to
// diagnose, and its own text (for example a quoted "503") must not be mistaken
// for a transport failure and replayed. Pi is the only adapter with this
// suppression; every other adapter keeps the shared prose-retry behavior.
func piClassifyTransient(err error) (string, bool) {
	var malformed *malformedAgentOutputError
	if errors.As(err, &malformed) {
		return "", false
	}
	return classifyTransient(err)
}

// NeutralizesGateInstructions reports whether pi is currently launched with the
// target repo's project agent-instruction files suppressed. It is meaningful
// only under the opt-out (disableProjectSettings): the gate only consults it
// when the repo opted out. Pi's --no-context-files (-nc) disables AGENTS.md and
// CLAUDE.md discovery for the session. buildArgs places the suppression flag
// before user arguments so it cannot be consumed as another flag's value.
// Verified against current pi --help: "--no-context-files, -nc Disable AGENTS.md
// and CLAUDE.md discovery and loading".
func (a *piAgent) NeutralizesGateInstructions() bool {
	return a.disableProjectSettings
}

func (a *piAgent) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	return runWithRetry(ctx, "pi", opts, claudeMaxRetries, piClassifyTransient, nil, func() (*Result, error) {
		return a.runOnce(ctx, opts)
	})
}

func (a *piAgent) Close() error { return nil }

func (a *piAgent) runOnce(ctx context.Context, opts RunOpts) (*Result, error) {
	if opts.Session != nil && opts.Session.ID != "" && !isPiSessionID(opts.Session.ID) {
		// Pi accepts a path or partial UUID for --session. no-mistakes persists
		// only the full UUID that Pi minted, so corrupt local metadata cannot
		// turn a recovery attempt into an arbitrary session-file selection.
		return nil, fmt.Errorf("invalid pi session identity")
	}
	args := a.buildArgs(opts.Session)
	cmd := exec.CommandContext(ctx, a.bin, args...)
	cmd.Dir = opts.CWD
	cmd.Env = a.gitSafeEnv(opts.CWD, opts.Env)
	shellenv.ConfigureShellCommand(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("pi stdin pipe: %w", err)
	}

	started, err := startNativeAgentCommand(cmd, nativeAgentActivityObserver(opts, "pi"))
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("pi start: %w", err)
	}
	defer started.closePipes()
	pid := started.pid()
	emitAgentStarted(opts, "pi", pid)

	prompt := buildPiPrompt(opts.Prompt, opts.JSONSchema)
	stdinErrCh := writeNativeAgentStdin(stdin, prompt)

	var stderrBuf []byte
	var stderrWG sync.WaitGroup
	stderrWG.Add(1)
	go func() {
		defer stderrWG.Done()
		stderrBuf, _ = io.ReadAll(started.stderr)
	}()

	pp := &piParser{onChunk: opts.OnChunk}
	if err := pp.parse(ctx, started.stdout); err != nil {
		err = started.waitAfterParseError(err)
		stderrWG.Wait()
		err = errors.Join(err, piStdinError(<-stdinErrCh))
		retErr := fmt.Errorf("pi parse events: %w", err)
		emitAgentExited(opts, "pi", pid, retErr)
		return nil, retErr
	}

	waitErr := started.wait()
	stderrWG.Wait()
	stdinErr := piStdinError(<-stdinErrCh)
	stderr := strings.TrimSpace(string(stderrBuf))
	if waitErr != nil {
		if stderr != "" {
			retErr := fmt.Errorf("pi exited: %w: %s", errors.Join(waitErr, stdinErr), stderr)
			emitAgentExited(opts, "pi", pid, retErr)
			return nil, retErr
		}
		retErr := fmt.Errorf("pi exited: %w", errors.Join(waitErr, stdinErr))
		emitAgentExited(opts, "pi", pid, retErr)
		return nil, retErr
	}
	if stdinErr != nil {
		if stderr != "" {
			stdinErr = fmt.Errorf("%w: %s", stdinErr, stderr)
		}
		emitAgentExited(opts, "pi", pid, stdinErr)
		return nil, stdinErr
	}

	if pp.assistantError != "" {
		retErr := fmt.Errorf("pi reported error: %s", pp.assistantError)
		emitAgentExited(opts, "pi", pid, retErr)
		return nil, retErr
	}

	text := pp.finalText()
	res, err := finalizeTextResult("pi", text, opts.JSONSchema, pp.usage)
	if err == nil {
		res.Model = pp.model
		res.ModelProvider = pp.provider
	}
	if err == nil && opts.Session != nil {
		if pp.sessionID == "" {
			// A durable invocation without Pi's required JSON-mode session header
			// cannot be resumed safely on a later fixer turn.
			err = fmt.Errorf("pi did not report a session identity")
		} else if opts.Session.ID != "" && pp.sessionID != opts.Session.ID {
			err = fmt.Errorf("pi did not confirm the requested session")
		} else {
			res.SessionID = pp.sessionID
			res.Resumed = opts.Session.ID != ""
			// Pi's agent_end event contains only the messages generated by this
			// invocation, including after --session, so usage is not cumulative.
			res.SessionUsageCumulative = false
		}
	}
	emitAgentExited(opts, "pi", pid, err)
	return res, err
}

func piStdinError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("pi stdin: %w", err)
}

// buildArgs returns the Pi argv for one invocation. A nil session is an
// intentionally cold step; an empty SessionRef starts a durable session; a
// populated SessionRef resumes the UUID that no-mistakes previously recorded.
// Under the project-settings opt-out, the context-file suppression flag comes
// first. User extras otherwise precede managed JSON/session flags.
func (a *piAgent) buildArgs(session *SessionRef) []string {
	args := make([]string, 0, len(a.extraArgs)+5)
	// Project-settings opt-out (trusted-only; see config.DisableProjectSettings):
	// disable AGENTS.md/CLAUDE.md discovery so an agent-orchestration target
	// (firstmate) cannot install a fleet-captain identity on the gate agent.
	// Preserve an operator-pinned -nc/--no-context-files spelling, but place it
	// first. When the repo did not opt out, nothing is added and pi loads project
	// instruction files exactly as before (backward-compat).
	pinIndex := -1
	if a.disableProjectSettings {
		pinIndex = piNoContextFilesArgIndex(a.extraArgs)
		contextFlag := "--no-context-files"
		if pinIndex >= 0 {
			contextFlag = a.extraArgs[pinIndex]
		}
		args = append(args, contextFlag)
	}
	for i, arg := range a.extraArgs {
		if i != pinIndex {
			args = append(args, arg)
		}
	}
	args = append(args, "--mode", "json")
	switch {
	case session == nil:
		args = append(args, "--no-session")
	case session.ID != "":
		args = append(args, "--session", session.ID)
	}
	return args
}

// isPiSessionID accepts only the full canonical UUID Pi emits in its JSON
// session header. It deliberately rejects paths and partial UUIDs accepted by
// Pi's CLI because no-mistakes must never resume an ambiguous global session.
func isPiSessionID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i := 0; i < len(id); i++ {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if id[i] != '-' {
				return false
			}
			continue
		}
		if !(id[i] >= '0' && id[i] <= '9') && !(id[i] >= 'a' && id[i] <= 'f') && !(id[i] >= 'A' && id[i] <= 'F') {
			return false
		}
	}
	return true
}

func piNoContextFilesArgIndex(args []string) int {
	for i := 0; i < len(args); i++ {
		if piNoContextFilesArg(args[i]) {
			return i
		}
		if piArgTakesValue(args[i]) {
			i++
		}
	}
	return -1
}

func piNoContextFilesArg(arg string) bool {
	return arg == "--no-context-files" || arg == "-nc"
}

func piArgTakesValue(arg string) bool {
	switch arg {
	case "--mode", "--provider", "--model", "--api-key", "--system-prompt",
		"--append-system-prompt", "--name", "-n", "--session", "--session-id",
		"--fork", "--session-dir", "--models", "--tools", "-t", "--exclude-tools",
		"-xt", "--thinking", "--export", "--extension", "-e", "--skill",
		"--prompt-template", "--theme":
		return true
	default:
		return false
	}
}

// buildPiPrompt appends a JSON-output contract to the user prompt when a
// schema is provided. Pi has no equivalent of codex's --output-schema flag,
// so we inline the schema in the prompt the same way gnhf does.
func buildPiPrompt(prompt string, schema json.RawMessage) string {
	if len(schema) == 0 {
		return prompt
	}
	pretty, err := json.MarshalIndent(json.RawMessage(schema), "", "  ")
	if err != nil {
		pretty = []byte(schema)
	}
	return prompt + "\n\n## no-mistakes final output contract\n\n" +
		"When the iteration is complete, your final assistant response must be only valid JSON matching this JSON Schema. " +
		"Do not wrap it in Markdown fences. Do not include prose before or after the JSON object.\n\n" +
		string(pretty)
}

// piParser tracks the streaming state of one Pi run. It accumulates text
// deltas, captures the final assistant text and usage, and surfaces any
// reported assistant error.
type piParser struct {
	onChunk func(string)

	streamText     map[int]string
	completeText   map[int]string
	finalAssistant map[string]any
	sessionID      string
	model          string
	provider       string
	usage          TokenUsage
	seenUsage      map[string]struct{}
	assistantError string
}

func (p *piParser) parse(ctx context.Context, r io.Reader) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 256*1024*1024)

	p.streamText = make(map[int]string)
	p.completeText = make(map[int]string)
	p.seenUsage = make(map[string]struct{})

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var event map[string]any
		if err := json.Unmarshal(line, &event); err != nil {
			continue
		}
		p.handleEvent(event)
	}

	return scanner.Err()
}

func (p *piParser) handleEvent(event map[string]any) {
	typ, _ := event["type"].(string)
	switch typ {
	case "session":
		// The JSON-mode header is emitted before agent_start. Preserve only its
		// first valid full UUID so later arbitrary events cannot replace it.
		if p.sessionID == "" {
			if id, _ := event["id"].(string); isPiSessionID(id) {
				p.sessionID = id
			}
		}
	case "message_update":
		p.rememberAssistant(event["message"])
		p.handleAssistantEvent(event["assistantMessageEvent"])
	case "message_end", "turn_end":
		p.rememberAssistant(event["message"])
		p.recordAssistantUsage(event["message"])
	case "agent_end":
		p.rememberAgentEnd(event["messages"])
	}
}

func (p *piParser) rememberAssistant(raw any) {
	msg, ok := raw.(map[string]any)
	if !ok {
		return
	}
	if role, _ := msg["role"].(string); role != "assistant" {
		return
	}
	p.finalAssistant = msg
	// Pi reports the serving model and provider on every assistant message;
	// keep the latest so local invocation telemetry matches Claude/Codex/Grok.
	if v, _ := msg["model"].(string); v != "" {
		p.model = v
	}
	if v, _ := msg["provider"].(string); v != "" {
		p.provider = v
	}

	if reason, _ := msg["stopReason"].(string); reason == "error" || reason == "aborted" {
		p.assistantError = piFirstString(msg, "errorMessage", "error", "message")
		if p.assistantError == "" {
			p.assistantError = fmt.Sprintf("stopReason=%s", reason)
		}
	} else {
		p.assistantError = ""
	}
}

func (p *piParser) rememberAgentEnd(raw any) {
	messages, ok := raw.([]any)
	if !ok {
		return
	}

	total := TokenUsage{}
	seen := make(map[string]struct{})
	hasUsage := false
	for i, rawMsg := range messages {
		msg, ok := rawMsg.(map[string]any)
		if !ok || msg["role"] != "assistant" {
			continue
		}
		usageMap, ok := msg["usage"].(map[string]any)
		if !ok {
			continue
		}
		usage := piUsageFrom(usageMap)
		if piUsageIsZero(usage) {
			continue
		}
		key := piUsageKey(msg)
		if key == "" {
			key = fmt.Sprintf("agent_end:%d", i)
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		total = piUsageAdd(total, usage)
		hasUsage = true
	}
	if hasUsage {
		p.usage = total
		p.seenUsage = make(map[string]struct{}, len(seen))
		for key := range seen {
			p.seenUsage[key] = struct{}{}
		}
	}

	for i := len(messages) - 1; i >= 0; i-- {
		if msg, ok := messages[i].(map[string]any); ok && msg["role"] == "assistant" {
			p.rememberAssistant(msg)
			return
		}
	}
}

func (p *piParser) recordAssistantUsage(raw any) {
	msg, ok := raw.(map[string]any)
	if !ok || msg["role"] != "assistant" {
		return
	}
	usageMap, ok := msg["usage"].(map[string]any)
	if !ok {
		return
	}
	usage := piUsageFrom(usageMap)
	if piUsageIsZero(usage) {
		return
	}
	key := piUsageKey(msg)
	if key == "" {
		encoded, err := json.Marshal([]any{msg["role"], msg["stopReason"], msg["content"], msg["usage"]})
		if err != nil {
			return
		}
		key = string(encoded)
	}
	if p.seenUsage == nil {
		p.seenUsage = make(map[string]struct{})
	}
	if _, ok := p.seenUsage[key]; ok {
		return
	}
	p.seenUsage[key] = struct{}{}
	p.usage = piUsageAdd(p.usage, usage)
}

func (p *piParser) handleAssistantEvent(raw any) {
	evt, ok := raw.(map[string]any)
	if !ok {
		return
	}
	idx := piIntField(evt, "contentIndex", "content_index")
	switch evt["type"] {
	case "text_delta":
		// Emit just the incremental delta. no-mistakes' OnChunk consumers
		// (TUI log line buffer, file logger) expect appended text, not
		// cumulative state.
		delta := piFirstString(evt, "delta", "text", "content")
		if delta == "" {
			return
		}
		p.streamText[idx] += delta
		if p.onChunk != nil {
			p.onChunk(delta)
		}
	case "text_end":
		// Capture the complete text for final-result resolution. Don't
		// re-emit to OnChunk: the deltas already covered it. If the event
		// carries the full text (Pi's normal shape), prefer that over the
		// delta accumulator since it's authoritative.
		text := piFirstString(evt, "text", "content")
		if text == "" {
			text = p.streamText[idx]
		}
		p.completeText[idx] = text
	}
}

// finalText returns the final assistant text, preferring (in order) the
// content of the last assistant message, the text_end-completed deltas, and
// finally the in-flight stream buffer.
func (p *piParser) finalText() string {
	if text := strings.TrimSpace(textFromAssistantMessage(p.finalAssistant)); text != "" {
		return text
	}
	if text := strings.TrimSpace(joinByIndex(p.completeText)); text != "" {
		return text
	}
	return strings.TrimSpace(joinByIndex(p.streamText))
}

func textFromAssistantMessage(msg map[string]any) string {
	if msg == nil {
		return ""
	}
	switch v := msg["content"].(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, block := range v {
			switch t := block.(type) {
			case string:
				b.WriteString(t)
			case map[string]any:
				if s, ok := t["text"].(string); ok {
					b.WriteString(s)
					continue
				}
				if s, ok := t["content"].(string); ok {
					b.WriteString(s)
				}
			}
		}
		return b.String()
	}
	if s, ok := msg["text"].(string); ok {
		return s
	}
	return ""
}

func joinByIndex(parts map[int]string) string {
	if len(parts) == 0 {
		return ""
	}
	max := -1
	for k := range parts {
		if k > max {
			max = k
		}
	}
	var b strings.Builder
	for i := 0; i <= max; i++ {
		b.WriteString(parts[i])
	}
	return b.String()
}

func piFirstString(m map[string]any, names ...string) string {
	for _, n := range names {
		if v, ok := m[n].(string); ok {
			return v
		}
	}
	return ""
}

func piIntField(m map[string]any, names ...string) int {
	for _, n := range names {
		switch v := m[n].(type) {
		case float64:
			return int(v)
		case int:
			return v
		case json.Number:
			if i, err := v.Int64(); err == nil {
				return int(i)
			}
		}
	}
	return 0
}

func piUsageFrom(usage map[string]any) TokenUsage {
	_, cacheCreationReported := usage["cacheWrite"]
	return TokenUsage{
		Reported:              len(usage) > 0,
		CacheCreationReported: cacheCreationReported,
		InputTokens:           piIntField(usage, "input"),
		OutputTokens:          piIntField(usage, "output"),
		CacheReadTokens:       piIntField(usage, "cacheRead"),
		CacheCreationTokens:   piIntField(usage, "cacheWrite"),
	}
}

func piUsageAdd(a, b TokenUsage) TokenUsage {
	return TokenUsage{
		Reported:              a.Reported || b.Reported,
		CacheCreationReported: a.CacheCreationReported || b.CacheCreationReported,
		InputTokens:           a.InputTokens + b.InputTokens,
		OutputTokens:          a.OutputTokens + b.OutputTokens,
		CacheReadTokens:       a.CacheReadTokens + b.CacheReadTokens,
		CacheCreationTokens:   a.CacheCreationTokens + b.CacheCreationTokens,
	}
}

func piUsageIsZero(usage TokenUsage) bool {
	return usage.InputTokens == 0 && usage.OutputTokens == 0 &&
		usage.CacheReadTokens == 0 && usage.CacheCreationTokens == 0
}

func piUsageKey(msg map[string]any) string {
	for _, name := range []string{"responseId", "id"} {
		if value, ok := msg[name].(string); ok && value != "" {
			return name + ":" + value
		}
	}
	return ""
}
