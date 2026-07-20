package steps

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
	"testing"
)

// TestMain handles fake CLI dispatch when the test binary is invoked as gh/glab.
func TestMain(m *testing.M) {
	if mode := os.Getenv("FAKE_CLI_MODE"); mode != "" {
		handleFakeCLI(mode)
		return
	}
	// Agent harnesses inject git config (e.g. safe.bareRepository=explicit)
	// via GIT_CONFIG_COUNT/KEY_n/VALUE_n; tests that need it re-set it with
	// t.Setenv (issue #362).
	os.Unsetenv("GIT_CONFIG_COUNT")
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
	case "ci-gh-provenance":
		fakeCIGHProvenanceHandler(args)
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
	if strings.Contains(joined, "pr view") && strings.Contains(joined, "--json mergeable") {
		fmt.Println("MERGEABLE")
		os.Exit(0)
	}
	if strings.Contains(joined, "pr checks") {
		fmt.Println(`[{"name":"build","state":"SUCCESS","bucket":"pass"}]`)
		os.Exit(0)
	}
	fmt.Fprintln(os.Stderr, "unsupported reconcile gh argv:", joined)
	os.Exit(1)
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
	if serveFakeGitHubProvenanceAPI(args, checksJSON) {
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
		if headsPath := os.Getenv("FAKE_CLI_HEADS_PATH"); headsPath != "" {
			head := currentFakeCIGHHead(headsPath, indexPath)
			fmt.Printf("{\"state\":%q,\"headRefOid\":%q}\n", state, head)
		} else {
			fmt.Println(state)
		}
		os.Exit(0)
	}
	if strings.Contains(joined, "/check-runs?per_page=100") {
		if serveFakeGitHubProvenanceAPI(args, nextFakeCIGHChecks(checksPath, indexPath)) {
			os.Exit(0)
		}
	}
	if serveFakeGitHubProvenanceAPI(args, "") {
		os.Exit(0)
	}
	if strings.Contains(joined, "pr checks") {
		fmt.Println(nextFakeCIGHChecks(checksPath, indexPath))
		os.Exit(0)
	}
	if strings.Contains(joined, "run view") {
		fmt.Println("error log output")
		os.Exit(0)
	}
	os.Exit(1)
}

func currentFakeCIGHHead(headsPath, indexPath string) string {
	data, err := os.ReadFile(headsPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	entries := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(entries) == 0 || entries[0] == "" {
		return ""
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
	return entries[index]
}

func nextFakeCIGHChecks(checksPath, indexPath string) string {
	data, err := os.ReadFile(checksPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	entries := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(entries) == 0 || entries[0] == "" {
		return "[]"
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
	return entries[index]
}

func serveFakeGitHubProvenanceAPI(args []string, checksJSON string) bool {
	joined := strings.Join(args, " ")
	switch {
	case strings.Contains(joined, "/protection/required_status_checks"):
		fmt.Fprintln(os.Stderr, "Branch not protected")
		os.Exit(1)
		return false
	case strings.Contains(joined, "/rules/branches/"):
		fmt.Println("[]")
		return true
	case strings.Contains(joined, "/statuses?per_page=100"):
		legacyStatuses := os.Getenv("FAKE_CLI_LEGACY_STATUSES")
		if legacyStatuses == "" {
			legacyStatuses = "[]"
		}
		fmt.Println(legacyStatuses)
		return true
	case strings.Contains(joined, "/check-runs?per_page=100"):
		fmt.Println(fakeGitHubCheckRunsJSON(checksJSON))
		return true
	default:
		return false
	}
}

func fakeGitHubCheckRunsJSON(checksJSON string) string {
	var checks []struct {
		Name        string `json:"name"`
		State       string `json:"state"`
		Status      string `json:"status"`
		Conclusion  string `json:"conclusion"`
		Bucket      string `json:"bucket"`
		CompletedAt string `json:"completedAt"`
	}
	if err := json.Unmarshal([]byte(checksJSON), &checks); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	runs := make([]struct {
		Name        string `json:"name"`
		Status      string `json:"status"`
		Conclusion  string `json:"conclusion,omitempty"`
		CompletedAt string `json:"completed_at,omitempty"`
	}, 0, len(checks))
	for _, check := range checks {
		status, conclusion := fakeGitHubCheckRunState(check)
		runs = append(runs, struct {
			Name        string `json:"name"`
			Status      string `json:"status"`
			Conclusion  string `json:"conclusion,omitempty"`
			CompletedAt string `json:"completed_at,omitempty"`
		}{
			Name: check.Name, Status: status, Conclusion: conclusion, CompletedAt: check.CompletedAt,
		})
	}

	encoded, err := json.Marshal(struct {
		CheckRuns any `json:"check_runs"`
	}{CheckRuns: runs})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	return string(encoded)
}

func fakeGitHubCheckRunState(check struct {
	Name        string `json:"name"`
	State       string `json:"state"`
	Status      string `json:"status"`
	Conclusion  string `json:"conclusion"`
	Bucket      string `json:"bucket"`
	CompletedAt string `json:"completedAt"`
}) (string, string) {
	if check.Status != "" {
		return strings.ToLower(check.Status), strings.ToLower(check.Conclusion)
	}
	switch strings.ToLower(check.Bucket) {
	case "pending":
		return "in_progress", ""
	case "pass":
		return "completed", "success"
	case "fail":
		return "completed", "failure"
	case "cancel":
		return "completed", "cancelled"
	case "skipping":
		return "completed", "skipped"
	}
	switch strings.ToUpper(check.State) {
	case "PENDING", "QUEUED", "IN_PROGRESS", "WAITING", "REQUESTED", "EXPECTED":
		return "in_progress", ""
	default:
		return "completed", strings.ToLower(check.State)
	}
}

func fakeCIGHProvenanceHandler(args []string) {
	joined := strings.Join(args, " ")
	if len(args) >= 2 && args[0] == "auth" && args[1] == "status" {
		os.Exit(0)
	}
	if strings.Contains(joined, "pr view") && strings.Contains(joined, "--json mergeable") {
		fmt.Println("MERGEABLE")
		os.Exit(0)
	}
	if strings.Contains(joined, "pr view") && strings.Contains(joined, "--json state") {
		fmt.Println("OPEN")
		os.Exit(0)
	}
	if strings.Contains(joined, "branches/main/protection/required_status_checks") {
		fmt.Fprintln(os.Stderr, "Branch not protected")
		os.Exit(1)
	}
	if strings.Contains(joined, "rules/branches/main") || strings.Contains(joined, "/statuses?per_page=100") || strings.Contains(joined, "run list") {
		fmt.Println("[]")
		os.Exit(0)
	}
	if strings.Contains(joined, "/check-runs?per_page=100") {
		if logPath := os.Getenv("FAKE_CLI_HEAD_LOG"); logPath != "" {
			if f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
				fmt.Fprintln(f, joined)
				f.Close()
			}
		}
		if strings.Contains(joined, "/commits/"+os.Getenv("FAKE_CLI_INITIAL_HEAD")+"/") {
			fmt.Println(`{"check_runs":[{"name":"build","status":"completed","conclusion":"failure"}]}`)
		} else {
			fmt.Println(`{"check_runs":[{"name":"build","status":"in_progress"}]}`)
		}
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
	if serveFakeGitHubProvenanceAPI(args, "[]") {
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
