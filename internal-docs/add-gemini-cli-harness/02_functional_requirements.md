# Functional Requirements: Gemini CLI Agent Harness

## Overview
The `no-mistakes` system requires a new agent harness to support the `gemini` CLI. This allows users to leverage the Gemini models to drive AI workflows, parallel processing, and automated reviews via the existing `Agent` interface.

## Flow Steps

1. **Initialization**:
   - Add a new agent identifier (e.g., `AgentGemini` mapping to `"gemini"`) in the supported agent types.
   - In the `agent.NewWithOptions` factory function, route the `"gemini"` identifier to initialize a new `geminiAgent` struct.
2. **Execution (`Run` method)**:
   - Construct the command line arguments for the `gemini` CLI.
   - Set the default model to `gemini-3.5-pro` (or `gemini 3.1 pro` if that's the exact identifier in the CLI).
   - Inject the user's prompt and optional structured output definitions (if `RunOpts.JSONSchema` is provided).
   - The process is spawned in the target working directory `RunOpts.CWD` with a safe environment using `gitSafeEnv(opts.CWD)` and `shellenv.ConfigureShellCommand(cmd)`.
   - Native agent lifecycle callbacks (`emitAgentStarted`, `emitAgentExited`) are triggered to track process status for pipeline visibility.
3. **Output Parsing**:
   - The harness reads the stdout (and stderr) from the `gemini` CLI process.
   - It captures token usage (input/output/cache) from the CLI's output (either parsed from JSON events or scraped).
   - It buffers the standard output and triggers `opts.OnChunk` for streaming text support.
   - If the CLI outputs raw text, it uses standard methods like `finalizeTextResult` to extract JSON fences if needed, or directly captures structured JSON if the CLI supports it natively.
4. **Finalization**:
   - Upon process exit, validate the structured output against `RunOpts.JSONSchema` (if provided).
   - Return a populated `*Result` containing `Output`, `Text`, and `Usage`.

## Data Mapping

- **Input Parameters**:
  - `RunOpts.Prompt` -> `gemini` CLI input prompt argument (`-p` or similar).
  - `RunOpts.CWD` -> Command execution directory.
  - `RunOpts.JSONSchema` -> Mapped to structured output formatting flags for the CLI if applicable, or used to validate the final output.
  - **Default Model**: Configured to use `gemini 3.1 pro` when constructing arguments unless overridden.
- **Output Parameters**:
  - `gemini` CLI stdout streaming events -> Dispatched via `opts.OnChunk`.
  - Final string representation -> `Result.Text`.
  - Parsed JSON output (if requested) -> `Result.Output`.
  - Token consumption fields -> `Result.Usage` (`InputTokens`, `OutputTokens`, etc.).

## Acceptance Criteria
1. The `geminiAgent` successfully implements the `Agent` interface (`Name()`, `Run()`, `Close()`).
2. Calling `agent.NewWithOptions` (or equivalent initialization function) with `"gemini"` successfully returns an instance of `geminiAgent`.
3. The harness sets the default execution model to `gemini 3.1 pro`.
4. The agent handles raw text execution and correctly populates `Result.Text`.
5. The agent handles structured JSON execution correctly, validating against `JSONSchema` and populating `Result.Output`.
6. Token usage is accurately tracked and populated in `Result.Usage`.
7. Errors from process execution or parsing are returned safely and cleanly to the pipeline without panicking.
8. Native agent lifecycle hooks (`emitAgentStarted`, `emitAgentExited`) are triggered with the correct PID for pipeline visibility.
