# Technical Design: Add Gemini CLI Harness

## 1. Architecture Overview
The `no-mistakes` orchestrator will support a new agent backend using the `gemini` CLI. The integration will follow the existing `Agent` interface defined in `internal/agent/agent.go`. The new `geminiAgent` will securely invoke the `gemini` executable via `exec.CommandContext`, handle streaming output, parse structured JSON, and extract token usage.

## 2. System Boundaries & Interfaces

### 2.1 Types Configuration
- **`internal/types/types.go`**: Add `AgentGemini AgentName = "gemini"` to the `AgentName` constants list.
- **`internal/config/config.go`**: Register `"gemini"` in the mapping arrays to allow users to select `agent: gemini` in their `.no-mistakes.yaml`.

### 2.2 Agent Wiring
- **`internal/agent/agent.go`**: Update the `NewWithOptions` factory function:
  ```go
  case types.AgentGemini:
      return &geminiAgent{bin: bin, extraArgs: extraArgs}, nil
  ```

### 2.3 Agent Implementation (`internal/agent/gemini.go`)
- Create a `geminiAgent` struct:
  ```go
  type geminiAgent struct {
      bin       string
      extraArgs []string
  }
  ```
- Implement the `Agent` interface methods: `Name()`, `Run()`, and `Close()`.

## 3. Primary Technical Path

### 3.1 Execution and Arguments
The `Run()` method will invoke a `runOnce` function (wrapped in `runWithRetry` for fault tolerance).
The arguments to the `gemini` CLI will be constructed as follows:
- Prepend base flags: `--model`, `"gemini-3.1-pro"` (the default model).
- Inject `opts.extraArgs` to allow user overrides (e.g., custom model or temperature).
- Append the prompt: `-p`, `opts.Prompt`.
- Append structured output flags: `--output-format`, `"stream-json"`.
- If `opts.JSONSchema` is non-empty, pass it via `--json-schema` (or equivalent supported flag).

### 3.2 Parsing Output
The `gemini` CLI is expected to emit JSONL (JSON Lines) to `stdout` to support real-time chunk streaming and metadata extraction.
We will implement `parseGeminiEvents(ctx context.Context, r io.Reader, onChunk func(string), usage *TokenUsage, result **geminiResult) error`:
- **`chunk` events**: Extract partial text and stream it via the `opts.OnChunk` callback.
- **`result` events**: Extract the final `structured_output` (if schema was provided), accumulated `usage` (input/output tokens), and exit status.

The final parsed data will be validated and returned in the standard `*Result` struct.

## 4. Potential Fallback Routes

### 4.1 Fallback 1: No JSONL Streaming Native Support
If the `gemini` CLI lacks mature JSONL `stream-json` support, the `geminiAgent` will fallback to streaming raw text from `stdout`.
- Text chunks will be dispatched directly to `opts.OnChunk`.
- Upon process completion, the agent will call the existing `finalizeTextResult("gemini", textBuf, opts.JSONSchema, usage)` helper defined in `agent.go`. This robust fallback utilizes `fencedJSONCandidates` and `lastBareJSONObject` to extract the JSON payload from the raw text stream, guaranteeing compatibility even if the CLI outputs raw markdown.

### 4.2 Fallback 2: Model Availability / Degradation
If `gemini-3.1-pro` is unavailable or experiencing rate limits (HTTP 429), the orchestrator will:
1. Use the `runWithRetry` wrapper to perform automatic backoff retries.
2. Allow users to seamlessly fallback to `gemini-2.5-pro` (or similar) by adding `--model=gemini-2.5-pro` to the `agent_args_override` configuration. The code will ensure user-defined model flags take precedence over the default `gemini-3.1-pro`.
