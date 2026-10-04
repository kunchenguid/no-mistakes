// Package fakeplugin is a small, stateful provider plugin used by pipeline-step
// tests and the e2e harness. It keeps PRs in a JSON state file so a sequence
// of one-process-per-operation invocations behaves like one forge, and it is
// deliberately written against the public CLI contract only (argv in, one
// JSON document out), so it doubles as an executable reference for plugin
// authors. It is also strict about how it is called: an unknown flag, a
// missing context flag, a value flag without "=", a positional after a flag,
// non-empty stdin, a missing trailing --json, or a body file other users can
// read all fail the call, so an adapter regression fails the tests that drive
// it.
package fakeplugin

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// ExecutableName is the argv[0] basename the fake CLIs dispatch on.
const ExecutableName = "nm-fake-provider-plugin"

// Environment variables the fake reads.
const (
	EnvState = "FAKE_PROVIDER_PLUGIN_STATE"
	EnvLog   = "FAKE_PROVIDER_PLUGIN_LOG"
)

// State is the fake forge. Tests seed it and read it back.
type State struct {
	// Unauthenticated makes the status handshake exit non-zero like a
	// plugin with no credentials would.
	Unauthenticated bool `json:"unauthenticated,omitempty"`
	// ProtocolVersion overrides the handshake's reported version when
	// nonzero.
	ProtocolVersion int `json:"protocol_version,omitempty"`
	MaxPRBodyChars  int `json:"max_pr_body_chars,omitempty"`
	// HangOn makes the named subcommand (for example "pr view") sleep far
	// past any test timeout, so the adapter's per-call deadline fires.
	HangOn string `json:"hang_on,omitempty"`
	// Checks are reported for every PR.
	Checks []Check `json:"checks"`
	// MergeWhenChecksPass marks a PR merged at the head the CI step asked
	// about once a "pr checks" call sees every check passing.
	MergeWhenChecksPass bool   `json:"merge_when_checks_pass,omitempty"`
	FailedLogs          string `json:"failed_logs,omitempty"`
	NextNumber          int    `json:"next_number,omitempty"`
	PRs                 []PR   `json:"prs"`
}

// PR is one fake pull request.
type PR struct {
	Number     string `json:"number"`
	URL        string `json:"url"`
	HeadBranch string `json:"head_branch"`
	BaseBranch string `json:"base_branch"`
	HeadSHA    string `json:"head_sha,omitempty"`
	Title      string `json:"title"`
	Body       string `json:"body"`
	State      string `json:"state"`
	Draft      bool   `json:"draft,omitempty"`
}

// Check is one fake CI check.
type Check struct {
	Name   string `json:"name"`
	Bucket string `json:"bucket"`
	State  string `json:"state,omitempty"`
	Link   string `json:"link,omitempty"`
}

// Call is one parsed invocation, as recorded in the request log.
type Call struct {
	Command    string              `json:"command"`
	Positional []string            `json:"positional,omitempty"`
	Flags      map[string][]string `json:"flags,omitempty"`
	Switches   []string            `json:"switches,omitempty"`
}

// Flag returns the last value of a value flag.
func (c Call) Flag(name string) string {
	values := c.Flags[name]
	if len(values) == 0 {
		return ""
	}
	return values[len(values)-1]
}

// Switch reports whether a boolean flag was passed.
func (c Call) Switch(name string) bool {
	for _, s := range c.Switches {
		if s == name {
			return true
		}
	}
	return false
}

var contextFlags = []string{"plugin", "repo", "host", "raw-host", "remote-url"}

type spec struct {
	positional       int // number of positional args after the subcommand words
	required         []string
	optional         []string
	repeatable       []string
	switches         []string
	requiredSwitches []string
}

var specs = map[string]spec{
	"status":          {},
	"pr find":         {required: []string{"head"}, optional: []string{"base"}},
	"pr create":       {required: []string{"head", "base", "title", "body-file"}, switches: []string{"draft"}},
	"pr update":       {positional: 1, required: []string{"body-file"}, optional: []string{"title"}},
	"pr view":         {positional: 1},
	"pr checks":       {positional: 1, optional: []string{"head-sha"}},
	"pr mergeability": {positional: 1},
	"pr check-logs":   {positional: 1, optional: []string{"branch", "head-sha"}, repeatable: []string{"check"}, requiredSwitches: []string{"failed"}},
	"pr merged":       {positional: 1, required: []string{"expected-head"}},
	"pr retarget":     {positional: 1, required: []string{"base"}},
}

// Main serves one invocation and returns the process exit code. args
// excludes argv[0].
func Main(args []string, stdin io.Reader, stdout io.Writer, statePath, logPath string) int {
	call, err := parse(args)
	if logPath != "" {
		appendLog(logPath, call, args)
	}
	if err != nil {
		return fail(stdout, "usage", err.Error())
	}
	if extra, _ := io.ReadAll(io.LimitReader(stdin, 1)); len(extra) > 0 {
		return fail(stdout, "usage", "stdin must be empty")
	}
	var body string
	if path := call.Flag("body-file"); path != "" {
		if body, err = readBodyFile(path); err != nil {
			return fail(stdout, "usage", err.Error())
		}
	}
	if statePath == "" {
		return fail(stdout, "fake_error", EnvState+" is not set")
	}
	state, err := load(statePath)
	if err != nil {
		return fail(stdout, "fake_error", err.Error())
	}
	if state.HangOn != "" && state.HangOn == call.Command {
		time.Sleep(10 * time.Minute)
	}
	result, changed, err := serve(&state, call, body)
	if err != nil {
		return fail(stdout, "fake_error", err.Error())
	}
	if changed {
		if err := save(statePath, state); err != nil {
			return fail(stdout, "fake_error", err.Error())
		}
	}
	if err := json.NewEncoder(stdout).Encode(result); err != nil {
		return 2
	}
	return 0
}

func parse(args []string) (Call, error) {
	call := Call{Flags: map[string][]string{}}
	if len(args) == 0 || args[len(args)-1] != "--json" {
		return call, fmt.Errorf("the last argument must be --json")
	}
	args = args[:len(args)-1]
	var positional []string
	seenFlag := false
	for _, arg := range args {
		if !strings.HasPrefix(arg, "--") {
			if seenFlag {
				return call, fmt.Errorf("positional argument %q after a flag", arg)
			}
			positional = append(positional, arg)
			continue
		}
		seenFlag = true
		name, value, hasValue := strings.Cut(arg[2:], "=")
		if hasValue {
			call.Flags[name] = append(call.Flags[name], value)
		} else {
			call.Switches = append(call.Switches, name)
		}
	}
	if len(positional) > 0 && positional[0] == "pr" && len(positional) > 1 {
		call.Command = "pr " + positional[1]
		call.Positional = positional[2:]
	} else if len(positional) > 0 {
		call.Command = positional[0]
		call.Positional = positional[1:]
	}
	s, ok := specs[call.Command]
	if !ok {
		return call, fmt.Errorf("unknown subcommand %q", call.Command)
	}
	if len(call.Positional) != s.positional {
		return call, fmt.Errorf("%s takes %d positional argument(s), got %v", call.Command, s.positional, call.Positional)
	}
	allowed := map[string]bool{}
	for _, list := range [][]string{contextFlags, s.required, s.optional} {
		for _, name := range list {
			allowed[name] = true
		}
	}
	repeatable := map[string]bool{}
	for _, name := range s.repeatable {
		allowed[name], repeatable[name] = true, true
	}
	for name, values := range call.Flags {
		if !allowed[name] {
			return call, fmt.Errorf("%s does not accept --%s", call.Command, name)
		}
		if len(values) > 1 && !repeatable[name] {
			return call, fmt.Errorf("--%s passed %d times", name, len(values))
		}
	}
	for _, name := range append(append([]string(nil), contextFlags...), s.required...) {
		if _, ok := call.Flags[name]; !ok {
			return call, fmt.Errorf("%s requires --%s=<value>", call.Command, name)
		}
	}
	for _, name := range s.repeatable {
		if len(call.Flags[name]) == 0 {
			return call, fmt.Errorf("%s requires at least one --%s=<value>", call.Command, name)
		}
	}
	switches := map[string]bool{}
	for _, name := range append(append([]string(nil), s.switches...), s.requiredSwitches...) {
		switches[name] = true
	}
	for _, name := range call.Switches {
		if !switches[name] {
			return call, fmt.Errorf("%s does not accept switch --%s (value flags use --name=value)", call.Command, name)
		}
	}
	for _, name := range s.requiredSwitches {
		if !call.Switch(name) {
			return call, fmt.Errorf("%s requires --%s", call.Command, name)
		}
	}
	for _, p := range call.Positional {
		if n, err := strconv.Atoi(p); err != nil || n <= 0 {
			return call, fmt.Errorf("PR number %q is not a positive integer", p)
		}
	}
	return call, nil
}

func readBodyFile(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("body file: %w", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("body file %s is readable by other users (mode %v)", path, info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("body file: %w", err)
	}
	return string(data), nil
}

func serve(state *State, call Call, body string) (any, bool, error) {
	switch call.Command {
	case "status":
		if state.Unauthenticated {
			return nil, false, fmt.Errorf("not authenticated for %s", call.Flag("host"))
		}
		version := 1
		if state.ProtocolVersion != 0 {
			version = state.ProtocolVersion
		}
		return map[string]any{
			"protocol_version": version,
			"capabilities": map[string]bool{
				"mergeable_state":    true,
				"failed_check_logs":  true,
				"merged_proof":       true,
				"set_pr_base_branch": true,
			},
			"max_pr_body_chars": state.MaxPRBodyChars,
		}, false, nil
	case "pr find":
		head, base := call.Flag("head"), call.Flag("base")
		for _, pr := range state.PRs {
			if pr.State == "open" && pr.HeadBranch == head && (base == "" || pr.BaseBranch == base) {
				return map[string]any{"pr": wire(pr)}, false, nil
			}
		}
		return map[string]any{"pr": nil}, false, nil
	case "pr create":
		head := call.Flag("head")
		for _, pr := range state.PRs {
			if pr.State == "open" && pr.HeadBranch == head {
				return nil, false, fmt.Errorf("an open PR already exists for %s", head)
			}
		}
		if state.NextNumber <= 0 {
			state.NextNumber = 1
		}
		number := strconv.Itoa(state.NextNumber)
		state.NextNumber++
		base := "https://" + call.Flag("host") + "/" + strings.Trim(call.Flag("repo"), "/")
		pr := PR{Number: number, URL: base + "/pulls/" + number, HeadBranch: head, BaseBranch: call.Flag("base"), Title: call.Flag("title"), Body: body, State: "open", Draft: call.Switch("draft")}
		state.PRs = append(state.PRs, pr)
		return map[string]any{"pr": wire(pr)}, true, nil
	}
	number := call.Positional[0]
	index := -1
	for i := range state.PRs {
		if state.PRs[i].Number == number {
			index = i
		}
	}
	if index < 0 {
		return nil, false, fmt.Errorf("PR %s not found", number)
	}
	return servePR(state, &state.PRs[index], call, body)
}

func servePR(state *State, pr *PR, call Call, body string) (any, bool, error) {
	switch call.Command {
	case "pr update":
		if title, ok := call.Flags["title"]; ok {
			pr.Title = title[0]
		}
		pr.Body = body
		return map[string]any{"pr": wire(*pr)}, true, nil
	case "pr view":
		return map[string]any{"pr": wire(*pr), "state": pr.State, "title": pr.Title, "body": pr.Body}, false, nil
	case "pr checks":
		checks := state.Checks
		if checks == nil {
			checks = []Check{}
		}
		changed := false
		if head := call.Flag("head-sha"); head != "" && pr.HeadSHA != head {
			pr.HeadSHA = head
			changed = true
		}
		if state.MergeWhenChecksPass && pr.State == "open" && allPass(checks) {
			pr.State = "merged"
			changed = true
		}
		return map[string]any{"checks": checks}, changed, nil
	case "pr mergeability":
		return map[string]any{"state": "mergeable"}, false, nil
	case "pr check-logs":
		return map[string]any{"logs": state.FailedLogs}, false, nil
	case "pr merged":
		proof := map[string]any{"merged": pr.State == "merged", "number": pr.Number, "url": pr.URL, "head_sha": pr.HeadSHA}
		if pr.State == "merged" {
			proof["merge_commit_sha"] = "fakemergecommit"
			proof["merged_at"] = time.Now().UTC().Format(time.RFC3339)
			proof["merged_by"] = "fake-maintainer"
		}
		return proof, false, nil
	case "pr retarget":
		pr.BaseBranch = call.Flag("base")
		return map[string]any{"pr": wire(*pr)}, true, nil
	}
	return nil, false, fmt.Errorf("unsupported subcommand %q", call.Command)
}

func allPass(checks []Check) bool {
	if len(checks) == 0 {
		return false
	}
	for _, check := range checks {
		if check.Bucket != "pass" && check.Bucket != "skipping" {
			return false
		}
	}
	return true
}

func wire(pr PR) map[string]any {
	return map[string]any{
		"number":      pr.Number,
		"url":         pr.URL,
		"head_branch": pr.HeadBranch,
		"base_branch": pr.BaseBranch,
		"head_sha":    pr.HeadSHA,
	}
}

func load(path string) (State, error) {
	var state State
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return state, nil
	}
	if err != nil {
		return state, fmt.Errorf("read state: %w", err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return state, nil
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return state, fmt.Errorf("decode state: %w", err)
	}
	return state, nil
}

// Load reads a state file for test assertions.
func Load(path string) (State, error) { return load(path) }

// Save writes a state file, for tests seeding the fake forge.
func Save(path string, state State) error { return save(path, state) }

func save(path string, state State) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".fakeplugin-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// loggedCall is one request-log line: the parsed call plus the raw argv.
type loggedCall struct {
	Call
	Argv []string `json:"argv"`
}

func appendLog(path string, call Call, argv []string) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	line, err := json.Marshal(loggedCall{Call: call, Argv: argv})
	if err != nil {
		return
	}
	fmt.Fprintln(f, string(line))
}

// ReadLog returns every call recorded in a request log, oldest first.
func ReadLog(path string) ([]Call, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var calls []Call
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var logged loggedCall
		if err := json.Unmarshal([]byte(line), &logged); err != nil {
			return nil, fmt.Errorf("parse request log line %q: %w", line, err)
		}
		calls = append(calls, logged.Call)
	}
	return calls, nil
}

// fail prints a failure document and exits non-zero, as the contract
// requires for every failure.
func fail(stdout io.Writer, code, message string) int {
	_ = json.NewEncoder(stdout).Encode(map[string]any{"error": map[string]string{"code": code, "message": message}})
	return 1
}
