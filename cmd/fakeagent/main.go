// fakeagent is a deterministic stand-in for the real Claude, Codex, Grok,
// OpenCode, and Antigravity CLIs used by no-mistakes' e2e tests. One binary is
// compiled and then symlinked under each agent's dispatch name; Antigravity
// is linked as both `antigravity` and its probed binary name `agy`.
// argv[0]'s basename selects which wire protocol to speak.
//
// All invocations are appended to $FAKEAGENT_LOG (one JSON object per line)
// so tests can assert on exactly which prompts the pipeline issued.
//
// Behaviour is driven by $FAKEAGENT_SCENARIO (a YAML file). When unset the
// agent returns an "all clean" canned response that satisfies every schema
// no-mistakes asks of it.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func main() {
	os.Exit(run(os.Args))
}

func run(argv []string) int {
	name := agentNameFromArgv0(argv[0])
	args := argv[1:]

	scenario, err := loadScenario(os.Getenv("FAKEAGENT_SCENARIO"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakeagent: scenario: %v\n", err)
		return 1
	}

	switch name {
	case "claude":
		return runClaude(args, os.Stdin, scenario)
	case "codex":
		return runCodex(args, os.Stdin, scenario)
	case "grok":
		return runGrok(args, scenario)
	case "antigravity", "agy":
		return runAgy(args, scenario)
	case "opencode":
		return runOpencode(args, scenario)
	case "gh":
		return runGhStub(args)
	case "tea":
		return runTeaStub(args)
	default:
		fmt.Fprintf(os.Stderr, "fakeagent: invoked under unknown name %q (argv[0]=%q)\n", name, argv[0])
		return 2
	}
}

// runGhStub shadows any system-installed gh during e2e so a stray PR/CI
// step can never reach github.com. It fails closed: `gh auth status`
// returns non-zero (so SCM detection treats GitHub as unauthenticated)
// and any other subcommand prints a clear error.
func runGhStub(args []string) int {
	switch os.Getenv("FAKEAGENT_GH_MODE") {
	case "fork-pr":
		return runGhForkPRStub(args)
	case "existing-pr":
		return runGhExistingPRStub(args)
	}
	if len(args) >= 2 && args[0] == "auth" && args[1] == "status" {
		fmt.Fprintln(os.Stderr, "fakeagent gh: not authenticated (e2e stub)")
		return 1
	}
	fmt.Fprintf(os.Stderr, "fakeagent gh: subcommand not implemented in e2e stub: %v\n", args)
	return 1
}

type ghStubInvocation struct {
	Time string   `json:"time"`
	Args []string `json:"args"`
	Repo string   `json:"repo,omitempty"`
	Head string   `json:"head,omitempty"`
	Base string   `json:"base,omitempty"`
	Body string   `json:"body,omitempty"`
}

func runGhForkPRStub(args []string) int {
	recordGhStubInvocation(args)

	if len(args) >= 2 && args[0] == "auth" && args[1] == "status" {
		return 0
	}
	if len(args) >= 2 && args[0] == "pr" && args[1] == "list" {
		head := argAfter(args, "--head")
		if strings.Contains(head, ":") {
			fmt.Fprintln(os.Stderr, `invalid argument: "--head" does not support "<owner>:<branch>"`)
			return 1
		}
		fmt.Println("[]")
		return 0
	}
	if len(args) >= 2 && args[0] == "pr" && args[1] == "create" {
		repo := argAfter(args, "--repo")
		if repo == "" {
			repo = os.Getenv("FAKEAGENT_GH_PARENT")
		}
		if repo == "" {
			repo = "parent/repo"
		}
		fmt.Printf("https://github.com/%s/pull/99\n", strings.TrimSuffix(repo, ".git"))
		return 0
	}
	if len(args) >= 2 && args[0] == "pr" && args[1] == "view" {
		if hasArgValue(args, "--json", "state") {
			fmt.Println("MERGED")
			return 0
		}
		if hasArgValue(args, "--json", "mergeable") {
			fmt.Println("MERGEABLE")
			return 0
		}
	}
	if len(args) >= 2 && args[0] == "pr" && args[1] == "checks" {
		fmt.Println("[]")
		return 0
	}

	fmt.Fprintf(os.Stderr, "fakeagent gh fork-pr: subcommand not implemented: %v\n", args)
	return 1
}

// recordGhStubInvocation appends the invocation to the stub log and returns the
// body read from stdin, so a caller that must also persist it does not have to
// consume stdin a second time (it is only readable once).
func recordGhStubInvocation(args []string) string {
	inv := ghStubInvocation{
		Time: time.Now().Format(time.RFC3339Nano),
		Args: append([]string(nil), args...),
		Repo: argAfter(args, "--repo"),
		Head: argAfter(args, "--head"),
		Base: argAfter(args, "--base"),
	}
	if hasArgValue(args, "--body-file", "-") {
		body, _ := io.ReadAll(os.Stdin)
		inv.Body = string(body)
	}
	logPath := os.Getenv("FAKEAGENT_GH_LOG")
	if logPath == "" {
		return inv.Body
	}
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return inv.Body
	}
	defer f.Close()
	_ = json.NewEncoder(f).Encode(inv)
	return inv.Body
}

// runTeaStub shadows any system-installed tea during the Gitea provider e2e
// journey. It is a stateless canned-response stub, mirroring
// runGhForkPRStub: `pulls list` always reports no existing PR (so the PR
// step exercises CreatePR), `pulls create` fabricates a plausible PR URL
// from its own human-readable output (real tea has no --output json on
// create; the pipeline's CreatePR falls back to scanning that output because
// this stub's stateless `pulls list` can never re-find the PR it just
// created), and `pulls <idx>` (view) reports the PR as already merged so the
// CI step's GetPRState short-circuits on the first poll without needing to
// model Gitea Actions runs at all.
func runTeaStub(args []string) int {
	recordTeaStubInvocation(args)

	if len(args) >= 1 && args[0] == "api" && args[len(args)-1] == "/user" {
		fmt.Println(`{"login":"e2e-tea-user"}`)
		return 0
	}
	if len(args) >= 2 && args[0] == "pulls" && args[1] == "list" {
		fmt.Println("[]")
		return 0
	}
	if len(args) >= 2 && args[0] == "pulls" && args[1] == "create" {
		repo := argAfter(args, "--repo")
		if repo == "" {
			repo = "owner/repo"
		}
		host := os.Getenv("FAKEAGENT_TEA_HOST")
		if host == "" {
			host = "gitea.example.com"
		}
		fmt.Printf("  # #99 stub PR (open)\n\n  http://%s/%s/pulls/99\n", host, strings.TrimSuffix(repo, ".git"))
		return 0
	}
	if len(args) >= 2 && args[0] == "pulls" && args[1] == "edit" {
		fmt.Println("updated")
		return 0
	}
	if len(args) >= 2 && args[0] == "pulls" {
		// `tea pulls <idx> --output json` (view a single PR by index). Merged
		// on the first poll so the CI step's GetPRState exits without needing
		// to model Gitea Actions runs.
		if _, err := strconv.Atoi(args[1]); err == nil {
			fmt.Println(`{"index":99,"state":"closed","hasMerged":true,"head":"","base":"main"}`)
			return 0
		}
	}

	fmt.Fprintf(os.Stderr, "fakeagent tea: subcommand not implemented in e2e stub: %v\n", args)
	return 1
}

type teaStubInvocation struct {
	Time  string   `json:"time"`
	Args  []string `json:"args"`
	Repo  string   `json:"repo,omitempty"`
	Login string   `json:"login,omitempty"`
	Head  string   `json:"head,omitempty"`
	Base  string   `json:"base,omitempty"`
}

func recordTeaStubInvocation(args []string) {
	logPath := os.Getenv("FAKEAGENT_TEA_LOG")
	if logPath == "" {
		return
	}
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()

	inv := teaStubInvocation{
		Time:  time.Now().Format(time.RFC3339Nano),
		Args:  append([]string(nil), args...),
		Repo:  argAfter(args, "--repo"),
		Login: argAfter(args, "--login"),
		Head:  argAfter(args, "--head"),
		Base:  argAfter(args, "--base"),
	}
	_ = json.NewEncoder(f).Encode(inv)
}

func argAfter(args []string, flag string) string {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}

func hasArgValue(args []string, flag, value string) bool {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

func agentNameFromArgv0(arg0 string) string {
	base := filepath.Base(arg0)
	base = strings.TrimSuffix(base, ".exe")
	return base
}

// existingPRStubConfig describes the single upstream pull request the
// existing-pr gh stub models. It is read from the JSON file named by
// $FAKEAGENT_GH_PR_CONFIG on EVERY invocation rather than from the
// environment, because the daemon is long-lived: a test that changes the
// forge's answer mid-run must be able to change it after the daemon started.
type existingPRStubConfig struct {
	PRRepo     string `json:"pr_repo"`     // upstream owner/repo the PR lives in
	PRNumber   string `json:"pr_number"`   // its number
	BaseRef    string `json:"base_ref"`    // the branch it targets
	SourceRepo string `json:"source_repo"` // owner/repo of the source (fork)
	SourceRef  string `json:"source_ref"`  // the source branch
	SourceDir  string `json:"source_dir"`  // the fork's git dir, read for the live head
	HeadSHA    string `json:"head_sha"`    // pins the head instead of reading the fork
	State      string `json:"state"`       // "open" (default) or "closed", for the identity lookup
	// ViewState is what `gh pr view --json state` answers, separately from the
	// identity lookup above. It defaults to State; a case only sets it when it
	// needs the two to disagree.
	ViewState string `json:"view_state"`
	Merged    bool   `json:"merged"`
	BodyFile  string `json:"body_file"`  // the PR body; `gh pr edit` rewrites it
	TitleFile string `json:"title_file"` // the PR title
	CreatedPR string `json:"created_pr"` // URL `gh pr create` would answer with
}

func loadExistingPRStubConfig() existingPRStubConfig {
	cfg := existingPRStubConfig{}
	if path := os.Getenv("FAKEAGENT_GH_PR_CONFIG"); path != "" {
		if data, err := os.ReadFile(path); err == nil {
			_ = json.Unmarshal(data, &cfg)
		}
	}
	if cfg.State == "" {
		cfg.State = "open"
	}
	if cfg.BaseRef == "" {
		cfg.BaseRef = "main"
	}
	if cfg.ViewState == "" {
		cfg.ViewState = cfg.State
	}
	return cfg
}

func (c existingPRStubConfig) url() string {
	return "https://github.com/" + c.PRRepo + "/pull/" + c.PRNumber
}

// head reports the pull request's source head. Unless a test pins one, it is
// the fork repository's real branch tip, so the stub tracks every push the
// pipeline makes the way the forge would.
func (c existingPRStubConfig) head() string {
	if c.HeadSHA != "" {
		return c.HeadSHA
	}
	if c.SourceDir == "" {
		return ""
	}
	out, err := exec.Command("git", "--git-dir", c.SourceDir, "rev-parse", "refs/heads/"+c.SourceRef).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// runGhExistingPRStub models one upstream github.com pull request whose source
// branch lives in another repository (the contributor's fork), which is the
// shape `--existing-pr` exists for. Nothing about the head is canned: it is
// read live from the fork repository the pipeline actually pushes to.
func runGhExistingPRStub(args []string) int {
	body := recordGhStubInvocation(args)
	if len(args) == 0 {
		return 1
	}
	cfg := loadExistingPRStubConfig()

	switch {
	case args[0] == "auth" && len(args) >= 2 && args[1] == "status":
		return 0

	case args[0] == "api":
		return ghExistingPRAPI(args, cfg)

	case args[0] == "pr" && len(args) >= 2 && args[1] == "view":
		return ghExistingPRView(args, cfg)

	case args[0] == "pr" && len(args) >= 2 && args[1] == "edit":
		if title := argAfter(args, "--title"); title != "" {
			writeStubFile(cfg.TitleFile, title)
		}
		if hasArgValue(args, "--body-file", "-") {
			writeStubFile(cfg.BodyFile, body)
		}
		fmt.Println(cfg.url())
		return 0

	case args[0] == "pr" && len(args) >= 2 && args[1] == "checks":
		fmt.Println("[]")
		return 0

	case args[0] == "pr" && len(args) >= 2 && args[1] == "list":
		// Branch discovery must never be how an associated run finds its
		// target: answering "no pull request" here means a run that fell back
		// to discovery would go on to create one, which the test can see.
		fmt.Println("[]")
		return 0

	case args[0] == "pr" && len(args) >= 2 && args[1] == "create":
		// Recorded above. A run that reaches this has opened the accidental
		// fork pull request `--existing-pr` exists to prevent, so the URL is
		// deliberately distinguishable from the associated one.
		created := cfg.CreatedPR
		if created == "" {
			created = "https://github.com/" + cfg.SourceRepo + "/pull/1"
		}
		fmt.Println(created)
		return 0
	}

	fmt.Fprintf(os.Stderr, "fakeagent gh existing-pr: subcommand not implemented: %v\n", args)
	return 1
}

func ghExistingPRAPI(args []string, cfg existingPRStubConfig) int {
	joined := strings.Join(args, " ")
	switch {
	case strings.Contains(joined, "graphql"):
		// One completed, successful check run on the head commit, so the CI
		// step reaches a green verdict instead of waiting for registration.
		fmt.Println(`{"data":{"repository":{"object":{"statusCheckRollup":{"contexts":{"nodes":[` +
			`{"__typename":"CheckRun","databaseId":1,"id":"CR_1","name":"build","status":"COMPLETED","conclusion":"SUCCESS",` +
			`"completedAt":"2026-01-01T00:00:00Z","startedAt":"2026-01-01T00:00:00Z",` +
			`"detailsUrl":"https://github.com/` + cfg.PRRepo + `/actions/runs/1",` +
			`"checkSuite":{"app":{"slug":"github-actions"}}}` +
			`],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}}}`)
		return 0
	case strings.Contains(joined, "/actions/runs"):
		// This read is `--paginate --slurp`, so its result is an ARRAY OF
		// PAGES. A bare `[]` is zero pages, which the reader rejects outright
		// ("workflow run discovery returned no pages") and the CI step then
		// escalates as a persistent check-read failure. One empty page is the
		// honest "this commit triggered no workflow runs".
		fmt.Println(`[{"total_count":0,"workflow_runs":[]}]`)
		return 0
	case args[len(args)-1] == "repos/"+cfg.PRRepo+"/pulls/"+cfg.PRNumber:
		payload := map[string]any{
			"number":   atoiOrZero(cfg.PRNumber),
			"html_url": cfg.url(),
			"state":    strings.ToLower(cfg.State),
			"merged":   cfg.Merged,
			"base": map[string]any{
				"ref":  cfg.BaseRef,
				"repo": map[string]any{"full_name": cfg.PRRepo, "html_url": "https://github.com/" + cfg.PRRepo},
			},
			"head": map[string]any{
				"ref":  cfg.SourceRef,
				"sha":  cfg.head(),
				"repo": map[string]any{"full_name": cfg.SourceRepo, "html_url": "https://github.com/" + cfg.SourceRepo},
			},
		}
		encoded, _ := json.Marshal(payload)
		fmt.Println(string(encoded))
		return 0
	}
	// Every other API read (workflow runs for a head commit, and so on)
	// reports nothing rather than failing the step.
	fmt.Println("[]")
	return 0
}

// ghExistingPRView answers `gh pr view`. The caller asks for named JSON fields
// and optionally a --jq expression selecting exactly one of them, so the stub
// renders the selected scalar alone and the object otherwise.
func ghExistingPRView(args []string, cfg existingPRStubConfig) int {
	values := map[string]any{
		"title":            stubFileOr(cfg.TitleFile, "Upstream account-context change"),
		"body":             stubFileOr(cfg.BodyFile, ""),
		"state":            strings.ToUpper(cfg.ViewState),
		"baseRefName":      cfg.BaseRef,
		"headRefName":      cfg.SourceRef,
		"headRefOid":       cfg.head(),
		"mergeable":        "MERGEABLE",
		"mergeStateStatus": "CLEAN",
	}
	if jq := argAfter(args, "--jq"); jq != "" {
		key := strings.TrimPrefix(strings.TrimSpace(jq), ".")
		value, ok := values[key]
		if !ok {
			fmt.Fprintf(os.Stderr, "fakeagent gh existing-pr: unmodelled --jq %q\n", jq)
			return 1
		}
		fmt.Println(value)
		return 0
	}
	selected := map[string]any{}
	for _, field := range strings.Split(argAfter(args, "--json"), ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		value, ok := values[field]
		if !ok {
			fmt.Fprintf(os.Stderr, "fakeagent gh existing-pr: unmodelled --json field %q\n", field)
			return 1
		}
		selected[field] = value
	}
	encoded, err := json.Marshal(selected)
	if err != nil {
		return 1
	}
	fmt.Println(string(encoded))
	return 0
}

func atoiOrZero(value string) int {
	n, err := strconv.Atoi(value)
	if err != nil {
		return 0
	}
	return n
}

func stubFileOr(path, fallback string) string {
	if path == "" {
		return fallback
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fallback
	}
	return string(data)
}

func writeStubFile(path, content string) {
	if path == "" {
		return
	}
	_ = os.WriteFile(path, []byte(content), 0o644)
}
