package agent

import (
	"path"
	"strings"
)

// This file is the single authoritative definition of the local per-invocation
// performance metrics and their boundaries. Every count, category, and timing
// split recorded to agent_invocations is defined here so the semantics live in
// exactly one place; the codex adapter fills them from its event stream, the
// pipeline records them, and `no-mistakes stats` renders them, all against
// these definitions. Nothing here reads or stores prompts, outputs, diffs, or
// raw command arguments - only bounded counts, categories, and durations.

// ToolCategory is a bounded bucket for a single tool sub-command. The set is
// fixed and low-cardinality so the histogram stays bounded and privacy-safe:
// we categorize a command's intent by its leading verb and never store the
// command text itself.
type ToolCategory string

const (
	// ToolWait is a wait/poll call that produces no work: sleeping, waiting on
	// a background job, or polling a slow subprocess (e.g. codex write_stdin).
	ToolWait ToolCategory = "wait"
	// ToolTestLint runs a test suite or a linter/formatter.
	ToolTestLint ToolCategory = "test_lint"
	// ToolEdit mutates the working tree (patch/apply, file writes, moves).
	ToolEdit ToolCategory = "edit"
	// ToolRead inspects the working tree without mutating it (cat, grep, ls).
	ToolRead ToolCategory = "read"
	// ToolGit is any git invocation.
	ToolGit ToolCategory = "git"
	// ToolOther is anything not matched by the buckets above.
	ToolOther ToolCategory = "other"
)

// ToolCategoryCounts is the bounded histogram of classified tool sub-commands
// for one invocation. Because a compound command (`go test && git commit`)
// contributes one count per sub-command, the sum of these fields can exceed
// InvocationMetrics.ToolCalls, which counts whole tool invocations.
type ToolCategoryCounts struct {
	Wait     int
	TestLint int
	Edit     int
	Read     int
	Git      int
	Other    int
}

// Add increments the bucket for category.
func (c *ToolCategoryCounts) Add(category ToolCategory) {
	switch category {
	case ToolWait:
		c.Wait++
	case ToolTestLint:
		c.TestLint++
	case ToolEdit:
		c.Edit++
	case ToolRead:
		c.Read++
	case ToolGit:
		c.Git++
	default:
		c.Other++
	}
}

// Total returns the number of classified sub-commands.
func (c ToolCategoryCounts) Total() int {
	return c.Wait + c.TestLint + c.Edit + c.Read + c.Git + c.Other
}

// InvocationMetrics is the bounded activity evidence an adapter extracts from
// one invocation's event stream. A nil *InvocationMetrics means the adapter
// reported nothing (recorded as NULL, never a fabricated zero); a non-nil value
// means every field is meaningful, including a genuine zero.
type InvocationMetrics struct {
	// ModelRoundtrips counts the model-authored items in the turn (assistant
	// messages plus tool calls). It is a live-stream proxy for productive model
	// round-trips: because codex batches an exec into a single turn and does not
	// surface internal poll round-trips as items, every counted item is
	// productive work, not "are-we-there-yet" polling.
	ModelRoundtrips int
	// ToolCalls counts whole tool invocations (one command_execution item is one
	// tool call regardless of how many sub-commands it chains).
	ToolCalls int
	// ToolCategories is the per-sub-command histogram (see ToolCategoryCounts).
	ToolCategories ToolCategoryCounts
	// SubprocessWaitMS is the wall-clock spent inside tool subprocesses,
	// measured by the reader as the sum of each tool item's started->completed
	// interval. Combined with the invocation duration it separates subprocess
	// wait from model/reasoning time (see ModelTimeMS).
	SubprocessWaitMS int64
}

// ModelTimeMS is the authoritative split of invocation wall-clock into
// model/reasoning time: the invocation duration minus the time spent waiting on
// tool subprocesses. It never goes negative.
func ModelTimeMS(durationMS, subprocessWaitMS int64) int64 {
	if subprocessWaitMS <= 0 {
		return durationMS
	}
	if subprocessWaitMS >= durationMS {
		return 0
	}
	return durationMS - subprocessWaitMS
}

// FreshInputTokens is the non-cached portion of an invocation's reported input:
// the input tokens that were not served from the provider's prompt cache. It is
// the honest per-invocation cost signal, separated from cache reads. It never
// goes negative.
func FreshInputTokens(inputTokens, cacheReadTokens int) int {
	if cacheReadTokens <= 0 {
		return inputTokens
	}
	if cacheReadTokens >= inputTokens {
		return 0
	}
	return inputTokens - cacheReadTokens
}

// PerRoundTokens converts a token counter into the per-round amount for one
// invocation. Some adapters (codex) report usage cumulatively across a resumed
// durable session, so round N's raw counter includes rounds 1..N-1; there the
// per-round amount is current minus the same session's previous cumulative.
// Adapters that report per-invocation usage (cumulative == false), and the
// first invocation of any session (priorCumulative <= 0), report current as-is.
// A cumulative counter that appears to shrink is treated as non-cumulative for
// that row rather than fabricating a negative or oversized delta.
func PerRoundTokens(current, priorCumulative int, cumulative bool) int {
	if !cumulative || priorCumulative <= 0 || current < priorCumulative {
		return current
	}
	return current - priorCumulative
}

// ClassifyToolCommand classifies one tool invocation's command into one bucket
// per chained sub-command. It unwraps a shell wrapper (`bash -lc '<script>'`),
// splits the script on command separators, and classifies each sub-command by
// its leading verb. It is a deliberately simple, allocation-light heuristic -
// the same leading-verb approach the efficiency audits used - and it never
// retains the command text.
func ClassifyToolCommand(command string) []ToolCategory {
	script := unwrapShellCommand(command)
	subs := splitCompoundCommand(script)
	if len(subs) == 0 {
		return nil
	}
	categories := make([]ToolCategory, 0, len(subs))
	for _, sub := range subs {
		categories = append(categories, classifySubcommand(sub))
	}
	return categories
}

// shellNames are the shells codex/agents wrap tool commands in.
var shellNames = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "fish": true,
}

// commandWrappers are prefixes that precede the real verb without changing its
// classification (env assignments are handled separately).
var commandWrappers = map[string]bool{
	"sudo": true, "env": true, "time": true, "nice": true, "nohup": true,
	"xargs": true, "command": true, "builtin": true, "exec": true, "stdbuf": true,
}

// unwrapShellCommand extracts the inner script from a `<shell> -lc '<script>'`
// wrapper. Fields inside the quoted script are re-joined with single spaces,
// which is lossless for leading-verb classification. A command with no
// recognized shell wrapper is returned unchanged.
func unwrapShellCommand(command string) string {
	fields := strings.Fields(command)
	if len(fields) < 3 {
		return command
	}
	if !shellNames[path.Base(fields[0])] {
		return command
	}
	for i := 1; i < len(fields)-1; i++ {
		if isShellCommandFlag(fields[i]) {
			return trimMatchingQuotes(strings.Join(fields[i+1:], " "))
		}
	}
	return command
}

// isShellCommandFlag reports whether flag is a `-c`-family shell flag
// (`-c`, `-lc`, `-lic`, `-ic`, ...) that introduces an inline script.
func isShellCommandFlag(flag string) bool {
	if len(flag) < 2 || flag[0] != '-' || len(flag) > 5 {
		return false
	}
	body := flag[1:]
	if !strings.HasSuffix(body, "c") {
		return false
	}
	for _, r := range body {
		switch r {
		case 'c', 'l', 'i', 'e', 'x':
		default:
			return false
		}
	}
	return true
}

func trimMatchingQuotes(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		if (s[0] == '\'' && s[len(s)-1] == '\'') || (s[0] == '"' && s[len(s)-1] == '"') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// splitCompoundCommand splits a script into sub-commands on the shell operators
// that sequence separate commands: &&, ||, ;, |, and newlines.
func splitCompoundCommand(script string) []string {
	var subs []string
	start := 0
	var quote rune
	escaped := false
	runes := []rune(script)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if escaped {
			escaped = false
			continue
		}
		if r == '\\' && quote != '\'' {
			escaped = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			}
			continue
		}
		if r == '\'' || r == '"' {
			quote = r
			continue
		}
		separatorWidth := 0
		switch r {
		case '\n', ';':
			separatorWidth = 1
		case '|':
			separatorWidth = 1
			if i+1 < len(runes) && runes[i+1] == '|' {
				separatorWidth = 2
			}
		case '&':
			if i+1 < len(runes) && runes[i+1] == '&' {
				separatorWidth = 2
			}
		}
		if separatorWidth == 0 {
			continue
		}
		if sub := strings.TrimSpace(string(runes[start:i])); sub != "" {
			subs = append(subs, sub)
		}
		start = i + separatorWidth
		i += separatorWidth - 1
	}
	if sub := strings.TrimSpace(string(runes[start:])); sub != "" {
		subs = append(subs, sub)
	}
	return subs
}

// testLintVerbs are standalone linters, formatters, and test runners.
var testLintVerbs = map[string]bool{
	"pytest": true, "vitest": true, "jest": true, "mocha": true, "rspec": true,
	"phpunit": true, "ctest": true, "shellcheck": true, "eslint": true,
	"prettier": true, "ruff": true, "flake8": true, "pylint": true, "mypy": true,
	"black": true, "isort": true, "golangci-lint": true, "gofmt": true,
	"goimports": true, "staticcheck": true, "revive": true, "vet": true,
	"tsc": true, "clippy": true, "rubocop": true, "standardrb": true,
	"checkstyle": true, "detekt": true, "ktlint": true, "swiftlint": true,
	"stylelint": true, "biome": true,
}

// readVerbs inspect without mutating.
var readVerbs = map[string]bool{
	"cat": true, "head": true, "tail": true, "less": true, "more": true,
	"grep": true, "egrep": true, "fgrep": true, "rg": true, "ag": true,
	"ls": true, "find": true, "fd": true, "wc": true, "cut": true, "sort": true,
	"uniq": true, "tr": true, "awk": true, "jq": true, "yq": true, "stat": true,
	"file": true, "tree": true, "nl": true, "tac": true, "column": true,
	"diff": true, "cmp": true, "xxd": true, "od": true, "bat": true,
	"readlink": true, "dirname": true, "basename": true, "pwd": true,
	"which": true, "whereis": true, "realpath": true, "hexdump": true,
}

// editVerbs mutate the working tree.
var editVerbs = map[string]bool{
	"apply_patch": true, "patch": true, "tee": true, "cp": true, "mv": true,
	"rm": true, "mkdir": true, "rmdir": true, "touch": true, "chmod": true,
	"chown": true, "ln": true, "install": true, "dd": true, "truncate": true,
}

// waitVerbs poll or idle.
var waitVerbs = map[string]bool{
	"sleep": true, "wait": true, "write_stdin": true, "watch": true,
	"until": true,
}

func classifySubcommand(sub string) ToolCategory {
	verb, rest := leadingVerb(sub)
	if verb == "" {
		return ToolOther
	}
	switch {
	case verb == "git":
		return ToolGit
	case waitVerbs[verb]:
		return ToolWait
	case verb == "sed":
		if hasInPlaceFlag(rest) {
			return ToolEdit
		}
		return ToolRead
	case editVerbs[verb]:
		return ToolEdit
	case testLintVerbs[verb]:
		return ToolTestLint
	case verb == "go":
		if firstArg(rest) == "test" || firstArg(rest) == "vet" {
			return ToolTestLint
		}
		return ToolOther
	case verb == "cargo":
		if a := firstArg(rest); a == "test" || a == "clippy" || a == "fmt" {
			return ToolTestLint
		}
		return ToolOther
	case verb == "npm" || verb == "pnpm" || verb == "yarn" || verb == "bun":
		if scriptRunnerIsTestLint(rest) {
			return ToolTestLint
		}
		return ToolOther
	case verb == "make":
		if makeTargetIsTestLint(rest) {
			return ToolTestLint
		}
		return ToolOther
	case verb == "gradle" || verb == "./gradlew" || verb == "gradlew":
		if a := firstArg(rest); a == "test" || a == "check" || a == "lint" {
			return ToolTestLint
		}
		return ToolOther
	case readVerbs[verb]:
		return ToolRead
	default:
		return ToolOther
	}
}

// leadingVerb returns the base command verb and the remaining tokens, skipping
// leading `VAR=value` env assignments and command wrappers (sudo, env, time,
// ...). The verb is reduced to its basename so `/usr/bin/git` classifies as git.
func leadingVerb(sub string) (verb string, rest []string) {
	tokens := strings.Fields(sub)
	i := 0
	for i < len(tokens) {
		tok := tokens[i]
		if isEnvAssignment(tok) {
			i++
			continue
		}
		base := path.Base(tok)
		if commandWrappers[base] {
			i++
			// `env`/`xargs` may be followed by their own flags; skip a following
			// flag token so the real verb is found.
			for i < len(tokens) && strings.HasPrefix(tokens[i], "-") {
				i++
			}
			continue
		}
		return base, tokens[i+1:]
	}
	return "", nil
}

func isEnvAssignment(tok string) bool {
	eq := strings.IndexByte(tok, '=')
	if eq <= 0 {
		return false
	}
	for _, r := range tok[:eq] {
		if !(r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

func firstArg(rest []string) string {
	for _, tok := range rest {
		if strings.HasPrefix(tok, "-") {
			continue
		}
		return tok
	}
	return ""
}

func hasInPlaceFlag(rest []string) bool {
	for _, tok := range rest {
		if tok == "-i" || strings.HasPrefix(tok, "-i.") || strings.HasPrefix(tok, "--in-place") {
			return true
		}
	}
	return false
}

// scriptRunnerIsTestLint reports whether an npm/pnpm/yarn/bun invocation runs a
// test or lint script (`npm test`, `pnpm run lint`, `yarn lint`, ...).
func scriptRunnerIsTestLint(rest []string) bool {
	for _, tok := range rest {
		if strings.HasPrefix(tok, "-") || tok == "run" || tok == "exec" {
			continue
		}
		return isTestLintWord(tok)
	}
	return false
}

func makeTargetIsTestLint(rest []string) bool {
	for _, tok := range rest {
		if strings.HasPrefix(tok, "-") {
			continue
		}
		if isTestLintWord(tok) {
			return true
		}
	}
	return false
}

func isTestLintWord(word string) bool {
	switch word {
	case "test", "tests", "lint", "lints", "check", "checks", "vet", "e2e",
		"typecheck", "format", "fmt", "verify":
		return true
	}
	return strings.HasPrefix(word, "test") || strings.HasPrefix(word, "lint")
}
