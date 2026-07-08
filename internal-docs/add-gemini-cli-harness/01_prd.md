# Product Requirements Document (PRD): Add Gemini CLI Harness

## 1. Background & Context
`no-mistakes` is a Go-based application designed to orchestrate AI agents for fixing code and validating Pull Requests. It currently supports several CLI-based agent harnesses, including Claude, Codex, and Copilot. To ensure our users have access to state-of-the-art reasoning and code generation capabilities, we need to integrate Google's Gemini models. Specifically, we want to add a new agent harness that leverages the `gemini` CLI and sets the default model to `gemini 3.1 pro`.

## 2. Goals
- **Primary Goal:** Integrate the `gemini` CLI as a fully supported, first-class agent harness in `no-mistakes`.
- **Secondary Goal:** Configure the new harness to default to the `gemini 3.1 pro` model for optimal performance in code analysis and generation tasks.

## 3. Scope
**In Scope:**
- Define a new `AgentGemini` identifier in the internal types.
- Implement a `geminiAgent` struct conforming to the `Agent` interface in `internal/agent/agent.go` (and related files).
- Ensure the `Run` method for the Gemini agent correctly constructs the CLI invocation for `gemini`, mapping prompts, working directory, and structured JSON schemas appropriately.
- Configure the default model flag or parameter for this harness to `gemini 3.1 pro`.
- Update initialization logic in `agent.go` (e.g., `NewWithOptions`) to route to the new `geminiAgent`.

**Out of Scope:**
- Deprecating or removing existing agent harnesses (e.g., Claude, Codex).
- Developing brand new workflow capabilities in `no-mistakes` outside of enabling the Gemini harness.

## 4. Business Value
By integrating the `gemini 3.1 pro` model as a default option through the new `gemini` CLI harness, we unlock superior contextual understanding and faster code remediation for our end-users. This reduces the time spent on manual PR reviews and bug fixes, directly boosting developer velocity and code quality across the teams that rely on `no-mistakes`.

## 5. Acceptance Criteria
1. **Configuration:** Setting the agent backend to `gemini` in configuration or CLI flags successfully initializes the Gemini CLI harness.
2. **Execution:** When `no-mistakes` is executed with the Gemini agent, it correctly calls the `gemini` CLI underlying executable.
3. **Model Default:** The CLI invocation implicitly or explicitly targets the `gemini 3.1 pro` model by default.
4. **Interface Conformance:** The `geminiAgent` correctly implements the `Name()`, `Run()`, and `Close()` methods of the `Agent` interface, and is capable of returning correctly parsed structured output as defined by `json.RawMessage`.
