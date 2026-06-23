// Package intent extracts a short summary of the user's original intent
// for a code change by reading recent transcripts from local coding agents
// (Claude Code, Codex CLI, OpenCode, Rovo Dev, Pi, and GitHub Copilot CLI) on
// the developer's machine.
//
// Given the repo path, the diff filenames, and the commit time window, the
// package discovers candidate sessions, picks the one with the strongest
// file-overlap match, drops tool calls, summarizes the remaining user and
// assistant text via the configured agent, and returns the summary so it
// can be injected into pipeline step prompts.
package intent
