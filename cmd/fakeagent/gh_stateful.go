package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ghStatefulPR is one pull request the stateful gh stub remembers, keyed by
// its head branch. The body is exactly what the pipeline last wrote, so a
// later `gh pr view --json body` reads it back the way a real forge would.
type ghStatefulPR struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	Base   string `json:"base"`
	Head   string `json:"head"`
	State  string `json:"state"`
	// ClosingIssues stands in for GitHub's own judgement of which issues the
	// body closes (owner/repository#number); the e2e test sets it alongside
	// an author edit.
	ClosingIssues []string `json:"closingIssues,omitempty"`
}

type ghStatefulState struct {
	PRs map[string]*ghStatefulPR `json:"prs"`
}

// runGhStatefulPRStub is a tiny in-file GitHub: PRs persist across gh calls
// in $FAKEAGENT_GH_STATE (a JSON file the e2e test may also edit, standing in
// for an author editing the PR description on the forge). It implements only
// the PR surface the pipeline drives: auth status, pr list/create/edit/view,
// and pr checks (always empty).
func runGhStatefulPRStub(args []string) int {
	var stdin string
	if hasArgValue(args, "--body-file", "-") {
		data, _ := io.ReadAll(os.Stdin)
		stdin = string(data)
	}
	recordGhStatefulInvocation(args, stdin)

	statePath := os.Getenv("FAKEAGENT_GH_STATE")
	if statePath == "" {
		fmt.Fprintln(os.Stderr, "fakeagent gh stateful-pr: FAKEAGENT_GH_STATE is required")
		return 1
	}
	state, err := loadGhStatefulState(statePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakeagent gh stateful-pr: %v\n", err)
		return 1
	}
	repo := os.Getenv("FAKEAGENT_GH_PARENT")
	if repo == "" {
		repo = "example/repo"
	}

	if len(args) >= 2 && args[0] == "auth" && args[1] == "status" {
		return 0
	}
	if len(args) >= 1 && args[0] == "api" && slices.Contains(args, "graphql") {
		// The forge has no checks, workflow runs or review threads: answer the commit
		// check rollup and review-thread queries with empty results.
		query := ""
		for _, arg := range args {
			if strings.HasPrefix(arg, "query=") {
				query = arg
			}
		}
		switch {
		case strings.Contains(query, "statusCheckRollup"):
			fmt.Println(`{"data":{"repository":{"object":{"statusCheckRollup":null}}}}`)
			return 0
		case strings.Contains(query, "reviewThreads"):
			fmt.Println(`{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}}`)
			return 0
		}
	}
	if len(args) >= 1 && args[0] == "api" && slices.ContainsFunc(args, func(arg string) bool { return strings.HasSuffix(arg, "/actions/runs") }) {
		fmt.Println(`[{"total_count":0,"workflow_runs":[]}]`)
		return 0
	}
	if len(args) < 2 || args[0] != "pr" {
		fmt.Fprintf(os.Stderr, "fakeagent gh stateful-pr: subcommand not implemented: %v\n", args)
		return 1
	}
	switch args[1] {
	case "list":
		head := argAfter(args, "--head")
		out := []map[string]any{}
		if pr := state.PRs[head]; pr != nil && pr.State == "OPEN" {
			out = append(out, map[string]any{
				"number":              pr.Number,
				"url":                 pr.URL,
				"baseRefName":         pr.Base,
				"headRefName":         pr.Head,
				"headRepositoryOwner": map[string]string{"login": strings.Split(repo, "/")[0]},
			})
		}
		data, _ := json.Marshal(out)
		fmt.Println(string(data))
		return 0
	case "create":
		head := argAfter(args, "--head")
		if i := strings.LastIndex(head, ":"); i >= 0 {
			head = head[i+1:]
		}
		number := 100 + len(state.PRs)
		pr := &ghStatefulPR{
			Number: number,
			URL:    fmt.Sprintf("https://github.com/%s/pull/%d", repo, number),
			Title:  argAfter(args, "--title"),
			Body:   stdin,
			Base:   argAfter(args, "--base"),
			Head:   head,
			State:  "OPEN",
		}
		state.PRs[head] = pr
		if err := saveGhStatefulState(statePath, state); err != nil {
			fmt.Fprintf(os.Stderr, "fakeagent gh stateful-pr: %v\n", err)
			return 1
		}
		fmt.Println(pr.URL)
		return 0
	case "edit", "view", "checks":
	default:
		fmt.Fprintf(os.Stderr, "fakeagent gh stateful-pr: subcommand not implemented: %v\n", args)
		return 1
	}

	pr := state.lookup(args)
	if pr == nil {
		fmt.Fprintf(os.Stderr, "no pull requests found for %v\n", args)
		return 1
	}
	switch args[1] {
	case "checks":
		fmt.Println("[]")
		return 0
	case "edit":
		if title := argAfter(args, "--title"); title != "" {
			pr.Title = title
		}
		if base := argAfter(args, "--base"); base != "" {
			pr.Base = base
		}
		if hasArgValue(args, "--body-file", "-") {
			pr.Body = stdin
		}
		if err := saveGhStatefulState(statePath, state); err != nil {
			fmt.Fprintf(os.Stderr, "fakeagent gh stateful-pr: %v\n", err)
			return 1
		}
		fmt.Println(pr.URL)
		return 0
	}
	// view
	fields := map[string]any{
		"number":                  pr.Number,
		"url":                     pr.URL,
		"title":                   pr.Title,
		"body":                    pr.Body,
		"state":                   pr.State,
		"baseRefName":             pr.Base,
		"headRefName":             pr.Head,
		"mergeable":               "MERGEABLE",
		"headRefOid":              ghStatefulHeadOID(repo, pr.Head),
		"closingIssuesReferences": ghStatefulClosingIssues(pr.ClosingIssues),
	}
	if jq := argAfter(args, "--jq"); strings.HasPrefix(jq, ".") {
		if value, ok := fields[strings.TrimPrefix(jq, ".")]; ok {
			fmt.Println(value)
			return 0
		}
		fmt.Fprintf(os.Stderr, "fakeagent gh stateful-pr: unsupported --jq %q\n", jq)
		return 1
	}
	out := map[string]any{}
	for _, name := range strings.Split(argAfter(args, "--json"), ",") {
		if value, ok := fields[name]; ok {
			out[name] = value
		}
	}
	data, _ := json.Marshal(out)
	fmt.Println(string(data))
	return 0
}

// lookup resolves `gh pr <verb> <selector>` where the selector is a PR number
// or URL.
func (s *ghStatefulState) lookup(args []string) *ghStatefulPR {
	if len(args) < 3 {
		return nil
	}
	selector := args[2]
	for _, pr := range s.PRs {
		if selector == pr.URL || selector == strconv.Itoa(pr.Number) {
			return pr
		}
	}
	return nil
}

// ghStatefulClosingIssues renders refs in the closingIssuesReferences shape.
func ghStatefulClosingIssues(refs []string) []map[string]any {
	out := []map[string]any{}
	for _, ref := range refs {
		slug, number, _ := strings.Cut(ref, "#")
		owner, name, _ := strings.Cut(slug, "/")
		n, err := strconv.Atoi(number)
		if err != nil {
			continue
		}
		out = append(out, map[string]any{
			"number":     n,
			"repository": map[string]any{"name": name, "owner": map[string]string{"login": owner}},
		})
	}
	return out
}

// ghStatefulHeadOID reads the PR head from the forge's git remote (the e2e
// harness rewrites the github.com URL to its local upstream).
func ghStatefulHeadOID(repo, head string) string {
	out, err := exec.Command("git", "ls-remote", "https://github.com/"+repo+".git", "refs/heads/"+head).Output()
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

func loadGhStatefulState(path string) (*ghStatefulState, error) {
	state := &ghStatefulState{PRs: map[string]*ghStatefulPR{}}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return state, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, state); err != nil {
		return nil, fmt.Errorf("parse state: %w", err)
	}
	if state.PRs == nil {
		state.PRs = map[string]*ghStatefulPR{}
	}
	return state, nil
}

func saveGhStatefulState(path string, state *ghStatefulState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func recordGhStatefulInvocation(args []string, body string) {
	logPath := os.Getenv("FAKEAGENT_GH_LOG")
	if logPath == "" {
		return
	}
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_ = json.NewEncoder(f).Encode(ghStubInvocation{
		Time: time.Now().Format(time.RFC3339Nano),
		Args: append([]string(nil), args...),
		Head: argAfter(args, "--head"),
		Base: argAfter(args, "--base"),
		Body: body,
	})
}
