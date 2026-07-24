package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

const acpxScannerMaxTokenSize = 256 * 1024 * 1024

type acpxAgent struct {
	bin        string
	target     string
	rawCommand string
}

func (a *acpxAgent) Name() string { return "acp:" + a.target }

func (a *acpxAgent) ReportsAgentAttempts() bool { return true }

func (a *acpxAgent) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	return runWithRetry(ctx, a.Name(), opts, claudeMaxRetries, classifyTransient, nil, func() (*Result, error) {
		return a.runOnce(ctx, opts)
	})
}

func (a *acpxAgent) runOnce(ctx context.Context, opts RunOpts) (*Result, error) {
	// Write the prompt to a temp file so that large prompts don't exceed the
	// OS command-line length limit (notably 8191 chars on Windows). acpx
	// accepts "exec -f <file>" as an alternative to passing the text inline.
	prompt := opts.Prompt
	if len(opts.JSONSchema) > 0 {
		prompt = buildACPStructuredPrompt(prompt, opts.JSONSchema)
	}
	promptFile, err := os.CreateTemp("", "nm-acpx-prompt-*.txt")
	if err != nil {
		return nil, fmt.Errorf("acpx prompt file: %w", err)
	}
	promptPath := promptFile.Name()
	defer os.Remove(promptPath)
	if _, err := promptFile.WriteString(prompt); err != nil {
		promptFile.Close()
		return nil, fmt.Errorf("acpx prompt file write: %w", err)
	}
	if err := promptFile.Close(); err != nil {
		return nil, fmt.Errorf("acpx prompt file close: %w", err)
	}

	args := a.buildArgs(opts, promptPath)
	cmd := exec.CommandContext(ctx, a.bin, args...)
	cmd.Dir = opts.CWD
	cmd.Stdin = nil
	cmd.Env = gitSafeEnv(opts.CWD)
	shellenv.ConfigureShellCommand(cmd)

	started, err := startNativeAgentCommand(cmd)
	if err != nil {
		return nil, fmt.Errorf("acpx start: %w", err)
	}
	defer started.closePipes()
	pid := started.pid()
	emitAgentStarted(opts, a.Name(), pid)

	var stderrBuf []byte
	var stderrWG sync.WaitGroup
	stderrWG.Add(1)
	go func() {
		defer stderrWG.Done()
		stderrBuf, _ = io.ReadAll(started.stderr)
	}()

	var usage TokenUsage
	text, stdoutErr, err := parseAcpxJSONEvents(ctx, started.stdout, opts.OnChunk, &usage)
	if err != nil {
		err = started.waitAfterParseError(err)
		stderrWG.Wait()
		retErr := fmt.Errorf("acpx parse events: %w", err)
		emitAgentExited(opts, a.Name(), pid, retErr)
		return nil, retErr
	}
	waitErr := started.wait()
	stderrWG.Wait()
	if waitErr != nil {
		retErr := fmt.Errorf("acpx exited: %w: %s", waitErr, acpxProcessErrorOutput(stderrBuf, stdoutErr))
		emitAgentExited(opts, a.Name(), pid, retErr)
		return nil, retErr
	}
	if usage.OutputTokens == 0 {
		usage.OutputTokens = estimateAcpxTokens(len(text))
	}
	res, err := finalizeTextResult(a.Name(), text, opts.JSONSchema, usage)
	emitAgentExited(opts, a.Name(), pid, err)
	return res, err
}

func (a *acpxAgent) Close() error { return nil }

func (a *acpxAgent) buildArgs(opts RunOpts, promptFile string) []string {
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
	if a.rawCommand == "" {
		args = append(args, a.target)
	}
	args = append(args, "exec", "-f", promptFile)
	return args
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
		Update acpxSessionUpdate `json:"update"`
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
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), acpxScannerMaxTokenSize)
	var output strings.Builder
	var stdoutErr string

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return "", stdoutErr, ctx.Err()
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
		return "", stdoutErr, err
	}
	return output.String(), stdoutErr, nil
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
