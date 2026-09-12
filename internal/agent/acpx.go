package agent

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

const (
	acpxScannerMaxTokenSize = 256 * 1024 * 1024
	acpxSessionCloseTimeout = 5 * time.Second
)

var acpxProcessSalt = func() [32]byte {
	var salt [32]byte
	if _, err := rand.Read(salt[:]); err != nil {
		panic(fmt.Sprintf("initialize ACP process identity: %v", err))
	}
	return salt
}()

type acpxAgent struct {
	bin        string
	target     string
	rawCommand string
	// model is the harness-neutral model pin resolved by internal/agentcfg.
	// no-mistakes never speaks ACP itself, so acpx's own --model is the only
	// mechanism that reaches the target agent; empty leaves the target on its
	// configured default, exactly as before the common layer existed.
	model string
	subprocessContext
}

func (a *acpxAgent) Name() string { return "acp:" + a.target }

func (a *acpxAgent) ReportsAgentAttempts() bool { return true }

// SupportsSessionResume reports acpx's ACP session/load capability. The
// bridge-owned name is only used when the caller explicitly requests durable
// state, leaving every cold invocation unchanged.
func (a *acpxAgent) SupportsSessionResume() bool { return true }

func (a *acpxAgent) SupportsSessionProvider(provider string) bool {
	fingerprint, ok := strings.CutPrefix(provider, a.staticSessionProvider()+":")
	if !ok || len(fingerprint) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(fingerprint)
	return err == nil
}

func (a *acpxAgent) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	return runWithRetry(ctx, a.Name(), opts, claudeMaxRetries, classifyTransient, nil, func() (*Result, error) {
		return a.runOnce(ctx, opts)
	})
}

func (a *acpxAgent) runOnce(ctx context.Context, opts RunOpts) (*Result, error) {
	prompt := opts.Prompt
	if len(opts.JSONSchema) > 0 {
		prompt = buildACPStructuredPrompt(prompt, opts.JSONSchema)
	}
	if opts.Session == nil {
		return a.runPrompt(ctx, opts, prompt, a.buildArgs(opts))
	}

	provider, err := a.resolveSessionProvider(ctx, opts)
	if err != nil {
		return nil, SessionSetupFailed(fmt.Errorf("acpx serving configuration: %w", err))
	}
	if opts.Session.Agent != "" && opts.Session.Agent != provider {
		return nil, SessionSetupFailed(errors.New("acpx serving configuration changed before prompting"))
	}
	opts.Session.Agent = provider
	requestedID := opts.Session.ID
	if requestedID == "" {
		result, err := a.runPrompt(ctx, opts, prompt, a.buildArgs(opts))
		if err != nil {
			return nil, err
		}
		if result.SessionID == "" {
			return nil, PromptDelivered(errors.New("acpx exec did not report an ACP session identity"))
		}
		result.Provider = provider
		return result, nil
	}
	setupID, err := a.runSessionCommand(ctx, opts, a.buildSessionArgs(opts, "new", requestedID))
	if err != nil {
		return nil, SessionSetupFailed(fmt.Errorf("acpx session setup: %w", err))
	}
	if setupID != requestedID {
		return nil, SessionSetupFailed(fmt.Errorf("acpx session setup found ACP session %q, want %q", setupID, requestedID))
	}

	result, promptErr := a.runPrompt(ctx, opts, prompt, a.buildSessionPromptArgs(opts))
	closeCtx, cancelClose := context.WithTimeout(context.WithoutCancel(ctx), acpxSessionCloseTimeout)
	closeID, closeErr := a.runSessionCommand(closeCtx, opts, a.buildSessionArgs(opts, "close", ""))
	cancelClose()
	if promptErr != nil {
		return nil, promptErr
	}
	if closeErr != nil {
		return nil, PromptDelivered(fmt.Errorf("acpx session close: %w", closeErr))
	}
	if closeID != "" {
		result.SessionID = closeID
	} else if result.SessionID == "" {
		result.SessionID = setupID
	}
	result.Provider = provider
	result.Resumed = result.SessionID == requestedID
	return result, nil
}

func (a *acpxAgent) runPrompt(ctx context.Context, opts RunOpts, prompt string, args []string) (*Result, error) {
	cmd := exec.CommandContext(ctx, a.bin, args...)
	cmd.Dir = opts.CWD
	cmd.Env = a.gitSafeEnv(opts.CWD, opts.Env)
	shellenv.ConfigureShellCommand(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("acpx stdin pipe: %w", err)
	}
	started, err := startNativeAgentCommand(cmd, nativeAgentActivityObserver(opts, a.Name()))
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("acpx start: %w", err)
	}
	defer started.closePipes()
	pid := started.pid()
	emitAgentStarted(opts, a.Name(), pid)
	stdinErrCh := writeNativeAgentStdin(stdin, prompt)

	var stderrBuf []byte
	var stderrWG sync.WaitGroup
	stderrWG.Add(1)
	go func() {
		defer stderrWG.Done()
		stderrBuf, _ = io.ReadAll(started.stderr)
	}()

	var usage TokenUsage
	text, stdoutErr, sessionID, err := parseAcpxJSONEventsWithSession(ctx, started.stdout, opts.OnChunk, &usage)
	if err != nil {
		err = started.waitAfterParseError(err)
		stderrWG.Wait()
		err = errors.Join(err, acpxStdinError(<-stdinErrCh))
		retErr := PromptDelivered(fmt.Errorf("acpx parse events: %w", err))
		emitAgentExited(opts, a.Name(), pid, retErr)
		return nil, retErr
	}
	waitErr := started.wait()
	stderrWG.Wait()
	stdinErr := acpxStdinError(<-stdinErrCh)
	if waitErr != nil {
		retErr := PromptDelivered(fmt.Errorf("acpx exited: %w: %s", errors.Join(waitErr, stdinErr), acpxProcessErrorOutput(stderrBuf, stdoutErr)))
		emitAgentExited(opts, a.Name(), pid, retErr)
		return nil, retErr
	}
	if stdinErr != nil {
		if out := acpxProcessErrorOutput(stderrBuf, stdoutErr); out != "" {
			stdinErr = fmt.Errorf("%w: %s", stdinErr, out)
		}
		stdinErr = PromptDelivered(stdinErr)
		emitAgentExited(opts, a.Name(), pid, stdinErr)
		return nil, stdinErr
	}
	if stdoutErr != "" {
		retErr := PromptDelivered(fmt.Errorf("acpx prompt: %s", stdoutErr))
		emitAgentExited(opts, a.Name(), pid, retErr)
		return nil, retErr
	}
	if usage.OutputTokens == 0 {
		usage.OutputTokens = estimateAcpxTokens(len(text))
	}
	res, err := finalizeTextResult(a.Name(), text, opts.JSONSchema, usage)
	if err != nil {
		err = PromptDelivered(err)
	}
	if res != nil {
		res.SessionID = sessionID
	}
	emitAgentExited(opts, a.Name(), pid, err)
	return res, err
}

func (a *acpxAgent) Close() error { return nil }

func (a *acpxAgent) buildArgs(opts RunOpts) []string {
	args := a.buildBaseArgs(opts)
	return append(args, "exec", "--file", "-")
}

func (a *acpxAgent) buildSessionPromptArgs(opts RunOpts) []string {
	args := a.buildBaseArgs(opts)
	return append(args, "prompt", "--session", a.sessionName(opts.Session.Scope, opts.Session.Agent), "--file", "-")
}

func (a *acpxAgent) buildSessionArgs(opts RunOpts, action, resumeID string) []string {
	args := a.buildBaseArgs(opts)
	args = append(args, "sessions", action)
	name := a.sessionName(opts.Session.Scope, opts.Session.Agent)
	switch action {
	case "new":
		args = append(args, "--name", name, "--resume-session", resumeID)
	case "close":
		args = append(args, name)
	}
	return args
}

func (a *acpxAgent) buildBaseArgs(opts RunOpts) []string {
	args := make([]string, 0, 14)
	if a.rawCommand != "" {
		args = append(args, "--agent", a.rawCommand)
	}
	if opts.CWD != "" {
		args = append(args, "--cwd", opts.CWD)
	}
	args = append(args,
		"--format", "json",
		"--json-strict",
		"--approve-all",
		"--non-interactive-permissions", "deny",
		"--suppress-reads",
	)
	if a.model != "" {
		args = append(args, "--model", a.model)
	}
	if a.rawCommand == "" {
		args = append(args, a.target)
	}
	return args
}

func (a *acpxAgent) staticSessionProvider() string {
	sum := sha256.Sum256([]byte(a.target + "\x00" + a.rawCommand + "\x00" + a.model))
	return a.Name() + ":" + hex.EncodeToString(sum[:])
}

func (a *acpxAgent) resolveSessionProvider(ctx context.Context, opts RunOpts) (string, error) {
	return a.resolveSessionProviderWithSalt(ctx, opts, acpxProcessSalt)
}

func (a *acpxAgent) resolveSessionProviderWithSalt(ctx context.Context, opts RunOpts, processSalt [32]byte) (string, error) {
	env := a.gitSafeEnv(opts.CWD, opts.Env)
	acpxPath := a.bin
	if resolved, err := exec.LookPath(acpxPath); err == nil {
		acpxPath = resolved
	}
	acpxIdentity, err := fingerprintACPExecutable(acpxPath)
	if err != nil {
		return "", fmt.Errorf("fingerprint acpx executable: %w", err)
	}

	identity := sha256.New()
	identity.Write(acpxIdentity[:])
	identity.Write([]byte{0})
	sortedEnv := append([]string(nil), env...)
	sort.Strings(sortedEnv)
	for _, entry := range sortedEnv {
		identity.Write([]byte(entry))
		identity.Write([]byte{0})
	}

	if a.rawCommand != "" {
		if rawPath, ok := resolveRawACPExecutable(a.rawCommand, opts.CWD, env); ok {
			rawIdentity, fingerprintErr := fingerprintACPExecutable(rawPath)
			if fingerprintErr == nil {
				identity.Write(rawIdentity[:])
			} else {
				identity.Write(processSalt[:])
			}
		} else {
			// A process-local binding allows safe reuse while this daemon still
			// owns the observation, but never guesses across reconstruction.
			identity.Write(processSalt[:])
		}
		return a.staticSessionProvider() + ":" + hex.EncodeToString(identity.Sum(nil)), nil
	}

	cmd := exec.CommandContext(ctx, a.bin, "--cwd", opts.CWD, "--format", "json", "config", "show")
	cmd.Dir = opts.CWD
	cmd.Env = env
	shellenv.ConfigureShellCommand(cmd)
	output, err := shellenv.CombinedOutputShellCommand(cmd)
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}
	var config any
	if err := json.Unmarshal(output, &config); err != nil {
		return "", fmt.Errorf("invalid config JSON: %w", err)
	}
	canonical, err := json.Marshal(config)
	if err != nil {
		return "", err
	}
	identity.Write(canonical)
	return a.staticSessionProvider() + ":" + hex.EncodeToString(identity.Sum(nil)), nil
}

func fingerprintACPExecutable(path string) ([32]byte, error) {
	var zero [32]byte
	absolute, err := filepath.Abs(path)
	if err != nil {
		return zero, err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return zero, err
	}
	// File metadata cannot prove that executable contents are unchanged:
	// deployment tools can preserve the inode, size, mode, and timestamps.
	// Rehash before every resume so a foreign implementation never receives
	// a persisted ACP session identity.
	file, err := os.Open(resolved)
	if err != nil {
		return zero, err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return zero, err
	}
	var sum [32]byte
	copy(sum[:], hash.Sum(nil))
	return sum, nil
}

func resolveRawACPExecutable(command, cwd string, env []string) (string, bool) {
	fields := strings.Fields(command)
	if len(fields) == 0 || strings.ContainsAny(fields[0], `"'\\`) {
		return "", false
	}
	name := fields[0]
	if filepath.IsAbs(name) {
		return name, true
	}
	if filepath.Base(name) != name {
		return "", false
	}
	pathValue := ""
	for _, entry := range env {
		if key, value, ok := strings.Cut(entry, "="); ok && strings.EqualFold(key, "PATH") {
			pathValue = value
		}
	}
	for _, dir := range filepath.SplitList(pathValue) {
		if dir == "" {
			dir = cwd
		}
		candidate := filepath.Join(dir, name)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate, true
		}
	}
	return "", false
}

func (a *acpxAgent) sessionName(scope, provider string) string {
	sum := sha256.Sum256([]byte(scope + "\x00" + provider))
	return "nm-" + hex.EncodeToString(sum[:16])
}

func (a *acpxAgent) runSessionCommand(ctx context.Context, opts RunOpts, args []string) (string, error) {
	cmd := exec.CommandContext(ctx, a.bin, args...)
	cmd.Dir = opts.CWD
	cmd.Env = a.gitSafeEnv(opts.CWD, opts.Env)
	shellenv.ConfigureShellCommand(cmd)
	output, runErr := shellenv.CombinedOutputShellCommand(cmd)
	sessionID, jsonErr := parseAcpxSessionCommand(output)
	if jsonErr != nil {
		return "", jsonErr
	}
	if runErr != nil {
		return "", fmt.Errorf("%w: %s", runErr, strings.TrimSpace(string(output)))
	}
	return sessionID, nil
}

func parseAcpxSessionCommand(output []byte) (string, error) {
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	var sessionID string
	var parsed bool
	for scanner.Scan() {
		var event struct {
			Error         *acpxJSONError `json:"error"`
			AcpxSessionID string         `json:"acpxSessionId"`
		}
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			continue
		}
		parsed = true
		if event.Error != nil && event.Error.Message != "" {
			return "", errors.New(event.Error.Message)
		}
		if event.AcpxSessionID != "" {
			sessionID = event.AcpxSessionID
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	if !parsed {
		return "", fmt.Errorf("acpx returned no JSON session result: %s", strings.TrimSpace(string(output)))
	}
	return sessionID, nil
}

func acpxStdinError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("acpx stdin: %w", err)
}

func acpxProcessErrorOutput(stderr []byte, stdoutErr string) string {
	parts := make([]string, 0, 2)
	if stderrText := strings.TrimSpace(string(stderr)); stderrText != "" {
		parts = append(parts, stderrText)
	}
	if stdoutErr != "" {
		parts = append(parts, stdoutErr)
	}
	return strings.Join(parts, "\n")
}

func buildACPStructuredPrompt(prompt string, schema json.RawMessage) string {
	return prompt + "\n\n## no-mistakes final output contract\n\n" +
		"When the task is complete, your final assistant message must be a single JSON object that matches this JSON Schema. " +
		"Return only the JSON object. Do not wrap it in Markdown fences. Do not include prose before or after the JSON.\n\n" +
		string(schema)
}

type acpxJSONMessage struct {
	Method string         `json:"method"`
	Error  *acpxJSONError `json:"error"`
	Result struct {
		Usage acpxUsageFields `json:"usage"`
	} `json:"result"`
	Params struct {
		SessionID string            `json:"sessionId"`
		Update    acpxSessionUpdate `json:"update"`
	} `json:"params"`
}

type acpxJSONError struct {
	Message string `json:"message"`
}

type acpxSessionUpdate struct {
	SessionUpdate string          `json:"sessionUpdate"`
	Content       json.RawMessage `json:"content"`
	Text          string          `json:"text"`
	Used          int             `json:"used"`
	usedReported  bool
	acpxUsageFields
	Meta struct {
		Usage acpxUsageFields `json:"usage"`
	} `json:"_meta"`
}

type acpxUsageFields struct {
	InputTokens                   int `json:"input_tokens"`
	OutputTokens                  int `json:"output_tokens"`
	CacheReadInputTokens          int `json:"cache_read_input_tokens"`
	CacheReadTokens               int `json:"cache_read_tokens"`
	CacheCreationInputTokens      int `json:"cache_creation_input_tokens"`
	CacheWriteInputTokens         int `json:"cache_write_input_tokens"`
	CacheWriteTokens              int `json:"cache_write_tokens"`
	CachedInputTokens             int `json:"cached_input_tokens"`
	InputTokensCamel              int `json:"inputTokens"`
	OutputTokensCamel             int `json:"outputTokens"`
	CacheReadInputTokensCamel     int `json:"cacheReadInputTokens"`
	CacheCreationInputTokensCamel int `json:"cacheCreationInputTokens"`
	CachedInputTokensCamel        int `json:"cachedInputTokens"`
	CacheReadTokensCamel          int `json:"cacheReadTokens"`
	CachedReadTokensCamel         int `json:"cachedReadTokens"`
	CacheCreationTokensCamel      int `json:"cacheCreationTokens"`
	CacheWriteTokensCamel         int `json:"cacheWriteTokens"`
	CachedWriteTokensCamel        int `json:"cachedWriteTokens"`
	reported                      bool
	cacheCreationReported         bool
}

func parseAcpxJSONEvents(ctx context.Context, r io.Reader, onChunk func(string), usage *TokenUsage) (string, string, error) {
	text, stdoutErr, _, err := parseAcpxJSONEventsWithSession(ctx, r, onChunk, usage)
	return text, stdoutErr, err
}

func parseAcpxJSONEventsWithSession(ctx context.Context, r io.Reader, onChunk func(string), usage *TokenUsage) (string, string, string, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), acpxScannerMaxTokenSize)
	var output strings.Builder
	var stdoutErr string
	var sessionID string

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return "", stdoutErr, sessionID, ctx.Err()
		default:
		}

		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var msg acpxJSONMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}
		if msg.Params.SessionID != "" {
			sessionID = msg.Params.SessionID
		}
		markAcpxUsagePresence(line, &msg)
		if msg.Error != nil && msg.Error.Message != "" && stdoutErr == "" {
			stdoutErr = msg.Error.Message
		}
		*usage = acpxMaxUsage(*usage, acpxUsageFieldsToTokenUsage(msg.Result.Usage))
		if msg.Method != "session/update" {
			continue
		}

		update := msg.Params.Update
		switch update.SessionUpdate {
		case "usage_update":
			*usage = acpxMaxUsage(*usage, acpxUpdateUsage(update))
		case "agent_message_chunk":
			text := acpxUpdateText(update)
			if text == "" {
				continue
			}
			output.WriteString(text)
			if onChunk != nil {
				onChunk(text)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", stdoutErr, sessionID, err
	}
	return output.String(), stdoutErr, sessionID, nil
}

func acpxUpdateUsage(update acpxSessionUpdate) TokenUsage {
	usage := acpxUsageFieldsToTokenUsage(update.acpxUsageFields)
	metaUsage := acpxUsageFieldsToTokenUsage(update.Meta.Usage)
	usage = acpxMaxUsage(usage, metaUsage)
	if update.Used > usage.InputTokens {
		usage.InputTokens = update.Used
	}
	usage.Reported = usage.Reported || update.usedReported || update.Used != 0
	return usage
}

func acpxUsageFieldsToTokenUsage(fields acpxUsageFields) TokenUsage {
	return TokenUsage{
		Reported: fields.reported || acpxUsageFieldsHaveValues(fields),
		CacheCreationReported: fields.cacheCreationReported || acpxFirstPositive(
			fields.CacheCreationInputTokens,
			fields.CacheWriteInputTokens,
			fields.CacheWriteTokens,
			fields.CacheCreationInputTokensCamel,
			fields.CacheCreationTokensCamel,
			fields.CacheWriteTokensCamel,
			fields.CachedWriteTokensCamel,
		) > 0,
		InputTokens: acpxFirstPositive(
			fields.InputTokens,
			fields.InputTokensCamel,
		),
		OutputTokens: acpxFirstPositive(
			fields.OutputTokens,
			fields.OutputTokensCamel,
		),
		CacheReadTokens: acpxFirstPositive(
			fields.CacheReadInputTokens,
			fields.CacheReadTokens,
			fields.CachedInputTokens,
			fields.CacheReadInputTokensCamel,
			fields.CachedInputTokensCamel,
			fields.CacheReadTokensCamel,
			fields.CachedReadTokensCamel,
		),
		CacheCreationTokens: acpxFirstPositive(
			fields.CacheCreationInputTokens,
			fields.CacheWriteInputTokens,
			fields.CacheWriteTokens,
			fields.CacheCreationInputTokensCamel,
			fields.CacheCreationTokensCamel,
			fields.CacheWriteTokensCamel,
			fields.CachedWriteTokensCamel,
		),
	}
}

func markAcpxUsagePresence(line []byte, msg *acpxJSONMessage) {
	var raw struct {
		Result struct {
			Usage json.RawMessage `json:"usage"`
		} `json:"result"`
		Params struct {
			Update json.RawMessage `json:"update"`
		} `json:"params"`
	}
	if json.Unmarshal(line, &raw) != nil {
		return
	}
	markAcpxUsageFields(raw.Result.Usage, &msg.Result.Usage)
	if len(raw.Params.Update) == 0 {
		return
	}
	var update map[string]json.RawMessage
	if json.Unmarshal(raw.Params.Update, &update) != nil {
		return
	}
	markAcpxUsageFields(raw.Params.Update, &msg.Params.Update.acpxUsageFields)
	if _, ok := update["used"]; ok {
		msg.Params.Update.usedReported = true
	}
	if meta, ok := update["_meta"]; ok {
		var metaFields struct {
			Usage json.RawMessage `json:"usage"`
		}
		if json.Unmarshal(meta, &metaFields) == nil {
			markAcpxUsageFields(metaFields.Usage, &msg.Params.Update.Meta.Usage)
		}
	}
}

func markAcpxUsageFields(raw json.RawMessage, fields *acpxUsageFields) {
	if len(raw) == 0 || fields == nil {
		return
	}
	var values map[string]json.RawMessage
	if json.Unmarshal(raw, &values) != nil {
		return
	}
	usageKeys := []string{"input_tokens", "output_tokens", "cache_read_input_tokens", "cache_read_tokens", "cached_input_tokens", "inputTokens", "outputTokens", "cacheReadInputTokens", "cachedInputTokens", "cacheReadTokens", "cachedReadTokens"}
	cacheKeys := []string{"cache_creation_input_tokens", "cache_write_input_tokens", "cache_write_tokens", "cacheCreationInputTokens", "cacheCreationTokens", "cacheWriteTokens", "cachedWriteTokens"}
	for _, key := range usageKeys {
		if _, ok := values[key]; ok {
			fields.reported = true
		}
	}
	for _, key := range cacheKeys {
		if _, ok := values[key]; ok {
			fields.reported = true
			fields.cacheCreationReported = true
		}
	}
}

func acpxUsageFieldsHaveValues(fields acpxUsageFields) bool {
	fields.reported = false
	fields.cacheCreationReported = false
	return fields != (acpxUsageFields{})
}

func acpxMaxUsage(a, b TokenUsage) TokenUsage {
	return TokenUsage{
		Reported:              a.Reported || b.Reported,
		CacheCreationReported: a.CacheCreationReported || b.CacheCreationReported,
		InputTokens:           max(a.InputTokens, b.InputTokens),
		OutputTokens:          max(a.OutputTokens, b.OutputTokens),
		CacheReadTokens:       max(a.CacheReadTokens, b.CacheReadTokens),
		CacheCreationTokens:   max(a.CacheCreationTokens, b.CacheCreationTokens),
	}
}

func acpxFirstPositive(values ...int) int {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}

func acpxUpdateText(update acpxSessionUpdate) string {
	if update.Text != "" {
		return update.Text
	}
	if len(update.Content) == 0 {
		return ""
	}
	var content struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(update.Content, &content); err == nil && content.Text != "" {
		return content.Text
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(update.Content, &parts); err != nil {
		return ""
	}
	var b strings.Builder
	for _, part := range parts {
		if part.Text != "" {
			b.WriteString(part.Text)
		}
	}
	return b.String()
}

func estimateAcpxTokens(charCount int) int {
	if charCount <= 0 {
		return 0
	}
	return (charCount + 3) / 4
}
