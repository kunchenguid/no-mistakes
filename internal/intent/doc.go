// Package intent extracts a short summary of the user's original intent
// for a code change by reading recent transcripts from local coding agents
// (Claude Code, Codex CLI, OpenCode, Rovo Dev, Pi, and GitHub Copilot CLI) on
// the developer's machine.
//
// Given the repo path, Git change context, diff filenames, and commit time
// window, the package discovers candidate sessions, picks the one with the
// strongest file-overlap match, drops tool calls, and summarizes the remaining
// user and assistant text against that change via the configured agent. The
// returned summary can be injected into pipeline step prompts.
package intent
