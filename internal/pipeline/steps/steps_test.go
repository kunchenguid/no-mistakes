package steps

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestMain handles fake CLI dispatch when the test binary is invoked as gh/glab.
func TestMain(m *testing.M) {
	if mode := os.Getenv("FAKE_CLI_MODE"); mode != "" {
		handleFakeCLI(mode)
		return
	}
	os.Exit(m.Run())
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

	switch mode {
	case "gh":
		fakeGHHandler(args)
	case "glab":
		fakeGlabHandler(args)
	case "record-success":
		fakeRecordSuccessHandler()
	case "git-passthrough":
		fakeGitPassthroughHandler(args)
	case "git-status-error":
		fakeGitStatusErrorHandler(args)
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
	default:
		os.Exit(1)
	}
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
	if len(args) >= 2 && args[0] == "auth" && args[1] == "status" {
		os.Exit(0)
	}
	if len(args) >= 2 && args[0] == "pr" && args[1] == "list" {
		if prURL == "" {
			fmt.Println("[]")
			os.Exit(0)
		}
		number := extractTrailingNumber(prURL)
		fmt.Printf("[{\"number\":%d,\"url\":%q}]\n", number, prURL)
		os.Exit(0)
	}
	if len(args) >= 2 && args[0] == "pr" && args[1] == "view" {
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

func fakeCIGHHandler(args []string) {
	state := os.Getenv("FAKE_CLI_STATE")
	checksJSON := os.Getenv("FAKE_CLI_CHECKS")
	checksErr := os.Getenv("FAKE_CLI_CHECKS_ERR")
	mergeable := os.Getenv("FAKE_CLI_MERGEABLE")
	mergeableErr := os.Getenv("FAKE_CLI_MERGEABLE_ERR")
	joined := strings.Join(args, " ")

	if len(args) >= 2 && args[0] == "auth" && args[1] == "status" {
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
	if strings.Contains(joined, "run view") {
		fmt.Println("error log output")
		os.Exit(0)
	}
	os.Exit(1)
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
	if strings.Contains(joined, "pr checks") {
		fmt.Fprintln(os.Stderr, "no checks reported on the 'feature/e2e' branch")
		os.Exit(1)
	}
	if strings.Contains(joined, "pr view") && strings.Contains(joined, "--json state") {
		fmt.Println("OPEN")
		os.Exit(0)
	}
	os.Exit(1)
}
