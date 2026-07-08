# Validation Analysis: Add Gemini CLI Harness

This document outlines the findings from the Phase 3 validation of the `gemini-cli` integration into the `no-mistakes` orchestrator.

## 1. Systemic Risks & Edge Cases in Implementation

### 1.1 Inappropriate Reuse of Claude Parsing Logic
The `gemini.go` implementation directly reuses `parseClaudeEvents`, `claudeResult`, and `finalizeClaudeResult` from `claude.go`. This introduces severe brittleness:
- **Event Type Mismatches:** If the `gemini` CLI emits different JSONL event types (e.g., `type: "model_response"` instead of `type: "assistant"`, or `type: "done"` instead of `type: "result"`), `parseClaudeEvents` will silently ignore them, resulting in a `gemini returned no result event` error.
- **Usage Metrics Discrepancy:** The `claudeMessage` struct expects Anthropic-specific token usage fields (like `input_tokens` and `output_tokens`). If the `gemini` CLI outputs usage metadata using different keys (e.g., `promptTokenCount`, `candidatesTokenCount`), the token usage metrics will silently remain zero.
- **Error Format Differences:** The Claude CLI error format (`is_error: true`, `subtype: ...`) may not match the Gemini CLI error format. Real Gemini API errors could be swallowed or ignored, leaving the user with generic "no result event" or "exit status 1" errors rather than actionable API feedback.

### 1.2 Inappropriate Reuse of Claude Retry Logic
The `geminiAgent.Run` method uses Claude's specific retry constants and logic:
```go
runWithRetry(ctx, "gemini", opts, claudeMaxRetries, claudeRetryClassifier, nil, ...)
```
Google API error strings (e.g., for HTTP 429 Quota Exceeded or HTTP 503 Service Unavailable) often differ from Anthropic's error strings. The `claudeRetryClassifier` will likely fail to identify Gemini's transient errors, rendering the retry mechanism useless for `geminiAgent` during actual rate limits.

## 2. Architecture Gaps & Deviations from Technical Design

### 2.1 Missing `parseGeminiEvents`
The Technical Design (Section 3.2) explicitly mandated a standalone `parseGeminiEvents` function and a `geminiResult` struct to properly parse the `gemini` CLI's `stream-json` output. The current implementation cuts corners and violates this system boundary by tightly coupling `geminiAgent` to `claudeAgent` types.

### 2.2 Missing Fallback 1 (Text Stream Extraction)
The Technical Design (Section 4.1) specified a fallback to raw text parsing (`finalizeTextResult`) if the `gemini` CLI lacks mature JSONL `stream-json` support. The current implementation strictly assumes a Claude-compatible JSONL format and completely lacks the required fallback to extract `fencedJSONCandidates` or `lastBareJSONObject` if the CLI outputs raw markdown text instead.

## 3. Recommendations
- **Immediate Fast Pivot:** Refactor `gemini.go` to implement its own `parseGeminiEvents`, `geminiEvent`, and `geminiResult` structures that accurately match the actual JSON schema emitted by the `gemini --output-format stream-json` command.
- **Implement Gemini Retry Classifier:** Define a `geminiRetryClassifier` that targets Google API / Gemini CLI specific error strings for robust retries.
- **Implement Text Fallback:** Ensure that if parsing the JSONL stream fails (or isn't supported), the agent safely falls back to raw text extraction as defined in the architectural design.
