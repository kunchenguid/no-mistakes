// Command fakecli is the tiny helper that pipeline-step tests place on PATH as
// gh/glab/git. It implements the same FAKE_CLI_MODE handlers that used to live
// in the race-instrumented test binary, so each fake invocation is a small
// process spawn instead of re-execing that binary (~0.8s under -race).
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

func main() {
	mode := os.Getenv("FAKE_CLI_MODE")
	if mode == "" {
		os.Exit(1)
	}
	handleFakeCLI(mode)
}

func handleFakeCLI(mode string) {
	args := os.Args[1:]
	logFile := os.Getenv("FAKE_CLI_LOG")

	if logFile != "" {
		f, _ := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if f != nil {
			fmt.Fprintln(f, strings.Join(args, " "))
			f.Close()
		}
	}
	logFakeCLIStdinBody(args, logFile)

	switch mode {
	case "gh":
		fakeGHHandler(args)
	case "glab":
		fakeGlabHandler(args)
	case "record-success":
		fakeRecordSuccessHandler()
	case "git-passthrough":
		fakeGitPassthroughHandler(args)
	case "git-move-head-passthrough":
		fakeGitMoveHeadPassthroughHandler(args)
	case "git-reset-after-commit-passthrough":
		fakeGitResetAfterCommitPassthroughHandler(args)
	case "git-require-noninteractive-env":
		fakeGitRequireNonInteractiveEnvHandler(args)
	case "git-status-error":
		fakeGitStatusErrorHandler(args)
	case "git-remote-error":
		fakeGitRemoteErrorHandler(args)
	case "ci-gh":
		fakeCIGHHandler(args)
	case "ci-gh-seq":
		fakeCIGHSequenceHandler(args)
	case "ci-gh-nochecks":
		fakeCIGHNoChecksHandler(args)
	case "ci-glab":
		fakeCIGlabHandler(args)
	case "ci-glab-seq":
		fakeCIGlabSequenceHandler(args)
	case "ci-gh-reconcile":
		fakeCIGHReconcileHandler(args)
	default:
		os.Exit(1)
	}
}

func logFakeCLIStdinBody(args []string, logFile string) {
	if logFile == "" || !argsUseStdinBodyFile(args) {
		return
	}
	body, err := io.ReadAll(os.Stdin)
	if err != nil {
		return
	}
	f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprint(f, "stdin --body ")
	fmt.Fprintln(f, string(body))
}

func argsUseStdinBodyFile(args []string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--body-file" && args[i+1] == "-" {
			return true
		}
	}
	return false
}

func fakeRecordSuccessHandler() {
	logFile := os.Getenv("FAKE_CLI_LOG")
	if logFile != "" {
		f, _ := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if f != nil {
			fmt.Fprintln(f, filepath.Base(os.Args[0]))
			f.Close()
		}
	}
	os.Exit(0)
}

func fakeGHHandler(args []string) {
	prURL := os.Getenv("FAKE_CLI_PR_URL")
	prBase := os.Getenv("FAKE_CLI_PR_BASE")
	prListJSON, hasPRListJSON := os.LookupEnv("FAKE_CLI_PR_LIST_JSON")
	if len(args) >= 2 && args[0] == "auth" && args[1] == "status" {
		os.Exit(0)
	}
	if len(args) >= 2 && args[0] == "pr" && args[1] == "list" {
		if hasPRListJSON {
			fmt.Print(prListJSON)
			os.Exit(0)
		}
		if prURL == "" {
			fmt.Println("[]")
			os.Exit(0)
		}
		// A real `gh pr list --base X` filters server-side by the PR's actual
		// base, so an existing PR opened against a different base than the one
		// requested here must not be returned.
		if requestedBase, ok := fakeCLIFlagValue(args, "--base"); ok && prBase != "" && requestedBase != prBase {
			fmt.Println("[]")
			os.Exit(0)
		}
		number := extractTrailingNumber(prURL)
		fmt.Printf("[{\"number\":%d,\"url\":%q,\"baseRefName\":%q}]\n", number, prURL, prBase)
		os.Exit(0)
	}
	if len(args) >= 2 && args[0] == "pr" && args[1] == "view" {
		if strings.Contains(strings.Join(args, " "), "--json state") {
			state := os.Getenv("FAKE_CLI_PR_STATE")
			if state == "" {
				state = "OPEN"
			}
			fmt.Println(state)
			os.Exit(0)
		}
		if prURL != "" {
			fmt.Println(prURL)
			os.Exit(0)
		}
		os.Exit(1)
	}
	if len(args) >= 2 && args[0] == "pr" && args[1] == "edit" {
		os.Exit(0)
	}
	if len(args) >= 2 && args[0] == "pr" && args[1] == "create" {
		fmt.Println("https://github.com/test/repo/pull/99")
		os.Exit(0)
	}
	os.Exit(1)
}

func fakeGitStatusErrorHandler(args []string) {
	realGit := os.Getenv("FAKE_CLI_REAL_GIT")
	if len(args) >= 2 && args[0] == "status" && args[1] == "--porcelain" {
		fmt.Fprintln(os.Stderr, "status failed")
		os.Exit(1)
	}
	fakeGitForward(args, realGit)
}

func fakeGitPassthroughHandler(args []string) {
	realGit := os.Getenv("FAKE_CLI_REAL_GIT")
	fakeGitForward(args, realGit)
}

func fakeGitMoveHeadPassthroughHandler(args []string) {
	realGit := os.Getenv("FAKE_CLI_REAL_GIT")
	if len(args) > 0 && args[0] == "push" {
		replacementHead := os.Getenv("FAKE_CLI_REPLACEMENT_HEAD")
		if replacementHead == "" {
			fmt.Fprintln(os.Stderr, "missing FAKE_CLI_REPLACEMENT_HEAD")
			os.Exit(1)
		}
		cmd := exec.Command(realGit, "reset", "--hard", replacementHead)
		cmd.Stdout = io.Discard
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	fakeGitForward(args, realGit)
}

// fakeGitSubcommand returns the git subcommand in args, skipping the global
// options (and their values) that may precede it.
func fakeGitSubcommand(args []string) string {
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-c" || args[i] == "-C":
			i++
		case strings.HasPrefix(args[i], "-"):
		default:
			return args[i]
		}
	}
	return ""
}

// fakeGitResetAfterCommitPassthroughHandler forwards every git invocation to the
// real binary and, for a commit, resets HEAD afterwards. It models an
// out-of-band reset landing between the pipeline's own commit and the head
// continuity check that follows it, without relying on a repository hook.
func fakeGitResetAfterCommitPassthroughHandler(args []string) {
	realGit := os.Getenv("FAKE_CLI_REAL_GIT")
	if fakeGitSubcommand(args) != "commit" {
		fakeGitForward(args, realGit)
		return
	}
	replacementHead := os.Getenv("FAKE_CLI_REPLACEMENT_HEAD")
	if replacementHead == "" {
		fmt.Fprintln(os.Stderr, "missing FAKE_CLI_REPLACEMENT_HEAD")
		os.Exit(1)
	}
	commit := exec.Command(realGit, args...)
	commit.Stdout = os.Stdout
	commit.Stderr = os.Stderr
	if err := commit.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if status := exitErr.ExitCode(); status >= 0 {
				os.Exit(status)
			}
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	reset := exec.Command(realGit, "reset", "--hard", replacementHead)
	reset.Stdout = io.Discard
	reset.Stderr = os.Stderr
	if err := reset.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func fakeGitRequireNonInteractiveEnvHandler(args []string) {
	realGit := os.Getenv("FAKE_CLI_REAL_GIT")
	required := map[string]string{
		"GIT_EDITOR":          "true",
		"GIT_SEQUENCE_EDITOR": "true",
		"GIT_TERMINAL_PROMPT": "0",
		"GIT_OPTIONAL_LOCKS":  "0",
	}
	for key, want := range required {
		if got := os.Getenv(key); got != want {
			fmt.Fprintf(os.Stderr, "%s=%q, want %q\n", key, got, want)
			os.Exit(1)
		}
	}
	helperCmd := exec.Command(realGit, "config", "--global", "--get-all", "credential.https://github.com.helper")
	helperCmd.Env = os.Environ()
	helperOut, err := helperCmd.Output()
	if err != nil || !strings.Contains(string(helperOut), "gh auth git-credential") {
		fmt.Fprintf(os.Stderr, "github credential helper = %q, err=%v; want inherited gh auth git-credential helper\n", strings.TrimSpace(string(helperOut)), err)
		os.Exit(1)
	}
	fakeGitForward(args, realGit)
}

func fakeGitRemoteErrorHandler(args []string) {
	realGit := os.Getenv("FAKE_CLI_REAL_GIT")
	if len(args) > 0 && (args[0] == "ls-remote" || args[0] == "push") {
		fmt.Fprintf(os.Stderr, "remote failed: %s\n", strings.Join(args, " "))
		os.Exit(1)
	}
	fakeGitForward(args, realGit)
}

func fakeGitForward(args []string, realGit string) {
	if realGit == "" {
		fmt.Fprintln(os.Stderr, "missing FAKE_CLI_REAL_GIT")
		os.Exit(1)
	}
	cmd := exec.Command(realGit, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if status := exitErr.ExitCode(); status >= 0 {
				os.Exit(status)
			}
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func fakeGlabHandler(args []string) {
	mrViewJSON := os.Getenv("FAKE_CLI_MR_VIEW_JSON")
	if len(args) >= 2 && args[0] == "auth" && args[1] == "status" {
		os.Exit(0)
	}
	if len(args) >= 2 && args[0] == "mr" && args[1] == "list" {
		if mrViewJSON == "" {
			fmt.Println("[]")
			os.Exit(0)
		}
		fmt.Println("[" + mrViewJSON + "]")
		os.Exit(0)
	}
	if len(args) >= 2 && args[0] == "mr" && args[1] == "view" {
		if mrViewJSON != "" {
			fmt.Println(mrViewJSON)
			os.Exit(0)
		}
		os.Exit(1)
	}
	if len(args) >= 2 && args[0] == "mr" && args[1] == "update" {
		os.Exit(0)
	}
	if len(args) >= 2 && args[0] == "mr" && args[1] == "create" {
		fmt.Println("https://gitlab.com/test/repo/-/merge_requests/99")
		os.Exit(0)
	}
	os.Exit(1)
}

func fakeCLIFlagValue(args []string, flag string) (string, bool) {
	for i, arg := range args {
		if arg == flag && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

func extractTrailingNumber(rawURL string) int {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return 0
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) == 0 {
		return 0
	}
	number, err := strconv.Atoi(parts[len(parts)-1])
	if err != nil {
		return 0
	}
	return number
}

func fakeCIGHReconcileHandler(args []string) {
	joined := strings.Join(args, " ")
	if len(args) >= 2 && args[0] == "auth" && args[1] == "status" {
		os.Exit(0)
	}
	if strings.Contains(joined, "pr list") {
		fmt.Println("[]")
		os.Exit(0)
	}
	if strings.Contains(joined, "pr create") {
		fmt.Println("https://github.com/test/repo/pull/42")
		os.Exit(0)
	}
	if strings.Contains(joined, "pr view") && strings.Contains(joined, "--json state") {
		state, err := os.ReadFile(os.Getenv("FAKE_CLI_STATE_PATH"))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		switch state := strings.TrimSpace(string(state)); state {
		case "ERROR":
			fmt.Fprintln(os.Stderr, "provider unavailable")
			os.Exit(1)
		default:
			fmt.Println(state)
			os.Exit(0)
		}
	}
	if strings.Contains(joined, "pr view") && strings.Contains(joined, "--json headRefOid") {
		fmt.Println(os.Getenv("FAKE_CLI_PR_HEAD_SHA"))
		os.Exit(0)
	}
	if strings.Contains(joined, "pr view") && strings.Contains(joined, "--json mergeable") {
		fmt.Println("MERGEABLE")
		os.Exit(0)
	}
	if strings.Contains(joined, "pr checks") {
		fmt.Println(`[{"name":"build","state":"SUCCESS","bucket":"pass"}]`)
		os.Exit(0)
	}
	if strings.Contains(joined, "api") && strings.Contains(joined, "graphql") {
		printFakeCommitChecks(`[{"name":"build","state":"SUCCESS","bucket":"pass"}]`, args)
		os.Exit(0)
	}
	fmt.Fprintln(os.Stderr, "unsupported reconcile gh argv:", joined)
	os.Exit(1)
}

func fakeGHHandlePRContentCommands(args []string, joined string) {
	if strings.Contains(joined, "pr view") && strings.Contains(joined, "--json title,body") {
		title := os.Getenv("FAKE_CLI_PR_TITLE")
		if title == "" {
			title = "test pr"
		}
		body := os.Getenv("FAKE_CLI_PR_BODY")
		if path := os.Getenv("FAKE_CLI_PR_BODY_FILE"); path != "" {
			data, err := os.ReadFile(path)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			body = string(data)
		}
		payload, err := json.Marshal(map[string]string{"title": title, "body": body})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(string(payload))
		os.Exit(0)
	}
	if strings.Contains(joined, "pr edit") {
		if editErr := os.Getenv("FAKE_CLI_PR_EDIT_ERR"); editErr != "" {
			fmt.Fprintln(os.Stderr, editErr)
			os.Exit(1)
		}
		if path := os.Getenv("FAKE_CLI_PR_BODY_FILE"); path != "" {
			body, err := io.ReadAll(os.Stdin)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			if err := os.WriteFile(path, body, 0o644); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
		}
		os.Exit(0)
	}
}

func fakeCIGHHandler(args []string) {
	state := os.Getenv("FAKE_CLI_STATE")
	stateErr := os.Getenv("FAKE_CLI_STATE_ERR")
	checksJSON := os.Getenv("FAKE_CLI_CHECKS")
	checksErr := os.Getenv("FAKE_CLI_CHECKS_ERR")
	mergeable := os.Getenv("FAKE_CLI_MERGEABLE")
	mergeableErr := os.Getenv("FAKE_CLI_MERGEABLE_ERR")
	joined := strings.Join(args, " ")

	if len(args) >= 2 && args[0] == "auth" && args[1] == "status" {
		os.Exit(0)
	}
	fakeGHHandlePRContentCommands(args, joined)
	if strings.Contains(joined, "pr view") && strings.Contains(joined, "--json headRefOid") {
		fmt.Println(os.Getenv("FAKE_CLI_PR_HEAD_SHA"))
		os.Exit(0)
	}
	if strings.Contains(joined, "pr view") && strings.Contains(joined, "--json mergeable") {
		if mergeableErr != "" {
			fmt.Fprintln(os.Stderr, mergeableErr)
			os.Exit(1)
		}
		if mergeable == "" {
			mergeable = "MERGEABLE"
		}
		fmt.Println(mergeable)
		os.Exit(0)
	}
	if strings.Contains(joined, "pr view") && strings.Contains(joined, "--json state") {
		if stateErr != "" {
			fmt.Fprintln(os.Stderr, stateErr)
			os.Exit(1)
		}
		fmt.Println(state)
		os.Exit(0)
	}
	if strings.Contains(joined, "pr checks") {
		if checksErr != "" {
			fmt.Fprintln(os.Stderr, checksErr)
			os.Exit(1)
		}
		fmt.Println(checksJSON)
		os.Exit(0)
	}
	if strings.Contains(joined, "api") && strings.Contains(joined, "graphql") {
		if checksErr != "" {
			fmt.Fprintln(os.Stderr, checksErr)
			os.Exit(1)
		}
		printFakeCommitChecks(checksJSON, args)
		os.Exit(0)
	}
	if strings.Contains(joined, "api") && strings.Contains(joined, "actions/runs") {
		printFakeWorkflowRuns()
		os.Exit(0)
	}
	if strings.Contains(joined, "run rerun") {
		fakeCIGHRerun()
	}
	if strings.Contains(joined, "run view") {
		fmt.Println("error log output")
		os.Exit(0)
	}
	os.Exit(1)
}

// fakeCIGHRerun answers `gh run rerun`, failing when FAKE_CLI_RERUN_ERR asks it
// to so tests can exercise a provider that refuses the request.
func fakeCIGHRerun() {
	if rerunErr := os.Getenv("FAKE_CLI_RERUN_ERR"); rerunErr != "" {
		fmt.Fprintln(os.Stderr, rerunErr)
		os.Exit(1)
	}
	os.Exit(0)
}

func fakeCIGHSequenceHandler(args []string) {
	state := os.Getenv("FAKE_CLI_STATE")
	checksPath := os.Getenv("FAKE_CLI_CHECKS_PATH")
	indexPath := os.Getenv("FAKE_CLI_CHECKS_INDEX_PATH")
	mergeable := os.Getenv("FAKE_CLI_MERGEABLE")
	mergeableErr := os.Getenv("FAKE_CLI_MERGEABLE_ERR")
	joined := strings.Join(args, " ")

	if len(args) >= 2 && args[0] == "auth" && args[1] == "status" {
		os.Exit(0)
	}
	fakeGHHandlePRContentCommands(args, joined)
	if strings.Contains(joined, "pr view") && strings.Contains(joined, "--json mergeable") {
		if mergeableErr != "" {
			fmt.Fprintln(os.Stderr, mergeableErr)
			os.Exit(1)
		}
		if mergeable == "" {
			mergeable = "MERGEABLE"
		}
		fmt.Println(mergeable)
		os.Exit(0)
	}
	if strings.Contains(joined, "pr view") && strings.Contains(joined, "--json state") {
		fmt.Println(state)
		os.Exit(0)
	}
	if strings.Contains(joined, "pr view") && strings.Contains(joined, "--json headRefOid") {
		fmt.Println(os.Getenv("FAKE_CLI_PR_HEAD_SHA"))
		os.Exit(0)
	}
	if strings.Contains(joined, "pr checks") {
		data, err := os.ReadFile(checksPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		entries := strings.Split(strings.TrimSpace(string(data)), "\n")
		if len(entries) == 0 || entries[0] == "" {
			fmt.Println("[]")
			os.Exit(0)
		}

		index := 0
		if rawIndex, err := os.ReadFile(indexPath); err == nil {
			if parsed, err := strconv.Atoi(strings.TrimSpace(string(rawIndex))); err == nil {
				index = parsed
			}
		}
		if index >= len(entries) {
			index = len(entries) - 1
		}
		if err := os.WriteFile(indexPath, []byte(strconv.Itoa(index+1)), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(entries[index])
		os.Exit(0)
	}
	if strings.Contains(joined, "api") && strings.Contains(joined, "graphql") {
		data, err := os.ReadFile(checksPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		entries := strings.Split(strings.TrimSpace(string(data)), "\n")
		if len(entries) == 0 || entries[0] == "" {
			printFakeCommitChecks("[]", args)
			os.Exit(0)
		}
		index := 0
		if rawIndex, err := os.ReadFile(indexPath); err == nil {
			if parsed, err := strconv.Atoi(strings.TrimSpace(string(rawIndex))); err == nil {
				index = parsed
			}
		}
		if index >= len(entries) {
			index = len(entries) - 1
		}
		if err := os.WriteFile(indexPath, []byte(strconv.Itoa(index+1)), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		printFakeCommitChecks(entries[index], args)
		os.Exit(0)
	}
	if strings.Contains(joined, "api") && strings.Contains(joined, "actions/runs") {
		printFakeWorkflowRuns()
		os.Exit(0)
	}
	if strings.Contains(joined, "run rerun") {
		fakeCIGHRerun()
	}
	if strings.Contains(joined, "run view") {
		fmt.Println("error log output")
		os.Exit(0)
	}
	os.Exit(1)
}

func fakeCIGlabHandler(args []string) {
	state := os.Getenv("FAKE_CLI_STATE")
	if state == "" {
		state = "opened"
	}
	checksJSON := os.Getenv("FAKE_CLI_CHECKS")
	if checksJSON == "" {
		checksJSON = "[]"
	}
	conflicts := "false"
	if os.Getenv("FAKE_CLI_MR_CONFLICTS") == "true" {
		conflicts = "true"
	}
	mergeStatus := os.Getenv("FAKE_CLI_MERGE_STATUS")
	if mergeStatus == "" {
		mergeStatus = "mergeable"
	}
	traceOutput := os.Getenv("FAKE_CLI_TRACE")
	if traceOutput == "" {
		traceOutput = "gitlab job trace output"
	}
	joined := strings.Join(args, " ")

	if len(args) >= 2 && args[0] == "auth" && args[1] == "status" {
		os.Exit(0)
	}
	if strings.Contains(joined, "mr view") {
		fmt.Printf(`{"iid":42,"web_url":"https://gitlab.com/test/repo/-/merge_requests/42","state":%q,"has_conflicts":%s,"detailed_merge_status":%q,"head_pipeline":{"id":7}}`,
			state, conflicts, mergeStatus)
		fmt.Println()
		os.Exit(0)
	}
	if strings.Contains(joined, "mr create") {
		fmt.Println("https://gitlab.com/test/repo/-/merge_requests/99")
		os.Exit(0)
	}
	if strings.Contains(joined, "mr update") {
		os.Exit(0)
	}
	if strings.Contains(joined, "ci status") {
		fmt.Println(checksJSON)
		os.Exit(0)
	}
	if strings.Contains(joined, "ci get") {
		fmt.Println(checksJSON)
		os.Exit(0)
	}
	if strings.Contains(joined, "ci trace") {
		fmt.Println(traceOutput)
		os.Exit(0)
	}
	os.Exit(1)
}

func fakeCIGlabSequenceHandler(args []string) {
	state := os.Getenv("FAKE_CLI_STATE")
	if state == "" {
		state = "opened"
	}
	conflicts := "false"
	if os.Getenv("FAKE_CLI_MR_CONFLICTS") == "true" {
		conflicts = "true"
	}
	mergeStatus := os.Getenv("FAKE_CLI_MERGE_STATUS")
	if mergeStatus == "" {
		mergeStatus = "mergeable"
	}
	checksPath := os.Getenv("FAKE_CLI_CHECKS_PATH")
	indexPath := os.Getenv("FAKE_CLI_CHECKS_INDEX_PATH")
	joined := strings.Join(args, " ")

	if len(args) >= 2 && args[0] == "auth" && args[1] == "status" {
		os.Exit(0)
	}
	if strings.Contains(joined, "mr view") {
		fmt.Printf(`{"iid":42,"web_url":"https://gitlab.com/test/repo/-/merge_requests/42","state":%q,"has_conflicts":%s,"detailed_merge_status":%q,"head_pipeline":{"id":7}}`,
			state, conflicts, mergeStatus)
		fmt.Println()
		os.Exit(0)
	}
	if strings.Contains(joined, "ci status") || strings.Contains(joined, "ci get") {
		data, err := os.ReadFile(checksPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		entries := strings.Split(strings.TrimSpace(string(data)), "\n")
		if len(entries) == 0 || entries[0] == "" {
			fmt.Println("[]")
			os.Exit(0)
		}
		index := 0
		if rawIndex, err := os.ReadFile(indexPath); err == nil {
			if parsed, err := strconv.Atoi(strings.TrimSpace(string(rawIndex))); err == nil {
				index = parsed
			}
		}
		if index >= len(entries) {
			index = len(entries) - 1
		}
		if err := os.WriteFile(indexPath, []byte(strconv.Itoa(index+1)), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(entries[index])
		os.Exit(0)
	}
	if strings.Contains(joined, "ci trace") {
		fmt.Println("gitlab job trace output")
		os.Exit(0)
	}
	os.Exit(1)
}

func fakeCIGHNoChecksHandler(args []string) {
	joined := strings.Join(args, " ")

	if len(args) >= 2 && args[0] == "auth" && args[1] == "status" {
		os.Exit(0)
	}
	fakeGHHandlePRContentCommands(args, joined)
	if strings.Contains(joined, "pr checks") {
		fmt.Fprintln(os.Stderr, "no checks reported on the 'feature/e2e' branch")
		os.Exit(1)
	}
	if strings.Contains(joined, "api") && strings.Contains(joined, "actions/runs") {
		printFakeWorkflowRuns()
		os.Exit(0)
	}
	if strings.Contains(joined, "api") && strings.Contains(joined, "graphql") {
		printFakeCommitChecks("[]", args)
		os.Exit(0)
	}
	if strings.Contains(joined, "pr view") && strings.Contains(joined, "--json state") {
		fmt.Println("OPEN")
		os.Exit(0)
	}
	if strings.Contains(joined, "pr view") && strings.Contains(joined, "--json headRefOid") {
		fmt.Println(os.Getenv("FAKE_CLI_PR_HEAD_SHA"))
		os.Exit(0)
	}
	os.Exit(1)
}

func printFakeWorkflowRuns() {
	raw := os.Getenv("FAKE_CLI_WORKFLOW_RUNS")
	if raw == "" {
		raw = "[]"
	}
	var runs []json.RawMessage
	if err := json.Unmarshal([]byte(raw), &runs); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	page := struct {
		TotalCount   int               `json:"total_count"`
		WorkflowRuns []json.RawMessage `json:"workflow_runs"`
	}{
		TotalCount:   len(runs),
		WorkflowRuns: runs,
	}
	encoded, err := json.Marshal([]any{page})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(string(encoded))
}

func printFakeCommitChecks(raw string, args []string) {
	var checks []struct {
		Name        string `json:"name"`
		State       string `json:"state"`
		Status      string `json:"status"`
		Conclusion  string `json:"conclusion"`
		Bucket      string `json:"bucket"`
		CompletedAt string `json:"completedAt"`
		Link        string `json:"link"`
	}
	if err := json.Unmarshal([]byte(raw), &checks); err != nil {
		fmt.Println(raw)
		return
	}
	nodes := make([]map[string]string, 0, len(checks))
	for _, check := range checks {
		status := check.Status
		if status == "" {
			status = "COMPLETED"
		}
		conclusion := check.State
		if conclusion == "" {
			conclusion = check.Conclusion
		}
		if conclusion == "" {
			switch check.Bucket {
			case "pass":
				conclusion = "SUCCESS"
			case "fail":
				conclusion = "FAILURE"
			case "cancel":
				conclusion = "CANCELLED"
			case "skip":
				conclusion = "SKIPPED"
			}
		}
		if check.Bucket == "pending" {
			status = "IN_PROGRESS"
			conclusion = ""
		}
		link := check.Link
		if repo := fakeGraphQLRepo(args); repo != "" {
			link = strings.Replace(link, "github.com/test/repo/", "github.com/"+repo+"/", 1)
		}
		nodes = append(nodes, map[string]string{
			"__typename": "CheckRun", "name": check.Name, "status": status,
			"conclusion": conclusion, "completedAt": check.CompletedAt, "detailsUrl": link,
		})
	}
	response := map[string]any{
		"data": map[string]any{
			"repository": map[string]any{
				"object": map[string]any{
					"statusCheckRollup": map[string]any{
						"contexts": map[string]any{
							"nodes":    nodes,
							"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""},
						},
					},
				},
			},
		},
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(string(encoded))
}

func fakeGraphQLRepo(args []string) string {
	var owner, name string
	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, "owner="):
			owner = strings.TrimPrefix(arg, "owner=")
		case strings.HasPrefix(arg, "name="):
			name = strings.TrimPrefix(arg, "name=")
		}
	}
	if owner == "" || name == "" {
		return ""
	}
	return owner + "/" + name
}
