// Package e2e holds end-to-end tests that drive the real no-mistakes
// binary against a temporary git repo and a fake agent. Tests live behind
// the `e2e` build tag so they are excluded from `go test ./...` and only
// run via `make e2e` (or `go test -tags=e2e ./internal/e2e/...`).
//
// The fake agent (cmd/fakeagent) is symlinked under each agent's binary
// name (claude, codex, opencode) into a temp PATH directory, and replies
// with deterministic canned responses defined by Scenario YAML or by the
// built-in "everything is clean" default. Every invocation is appended to
// $FAKEAGENT_LOG so tests can assert on which prompts the pipeline made.
//
// Why e2e at all: see the audit in the PR description. Unit tests for
// pipeline orchestration, executor branching, and CLI wiring give weak
// signal compared to actually running the binary against a real git
// repo. The e2e suite consolidates the "user pushes a branch and the
// pipeline runs to completion" journey into a single high-coverage test
// that is meant to grow rather than fan out into many small e2e files.
package e2e
