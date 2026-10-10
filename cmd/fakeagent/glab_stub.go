package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// runGlabStub shadows any system-installed glab during e2e, the same guard
// rail the gh and tea links are: BinDir is prepended to PATH, so a stray
// GitLab remote in a journey can never reach a real, authenticated glab on
// the developer's machine. Without FAKEAGENT_GLAB_MODE it fails closed
// (`glab auth status` is non-zero, so the GitLab host reports itself
// unauthenticated and the PR step skips); FAKEAGENT_GLAB_MODE=project-mr
// turns it into the stateless project stub the GitLab media journey drives.
func runGlabStub(args []string) int {
	switch os.Getenv("FAKEAGENT_GLAB_MODE") {
	case "project-mr":
		return runGlabProjectMRStub(args)
	}
	if len(args) >= 2 && args[0] == "auth" && args[1] == "status" {
		fmt.Fprintln(os.Stderr, "fakeagent glab: not authenticated (e2e stub)")
		return 1
	}
	fmt.Fprintf(os.Stderr, "fakeagent glab: subcommand not implemented in e2e stub: %v\n", args)
	return 1
}

// runGlabProjectMRStub models one GitLab project for the pipeline: `mr list`
// never finds an existing merge request (so the PR step exercises CreatePR),
// `mr create` answers with the merge request URL glab prints, `mr view`
// reports the merge request as already merged so the CI monitor exits on its
// first poll, and `api --method POST projects/<id>/uploads --form file=@<path>`
// answers with the JSON document GitLab's Markdown-uploads endpoint returns.
// Every invocation is appended to FAKEAGENT_GLAB_LOG, one JSON object per
// line, so a journey can read back the description it published and the
// exact upload calls it made.
//
// Two environment knobs model the server refusing or misbehaving, keyed on
// the uploaded file's basename (comma-separated lists):
// FAKEAGENT_GLAB_UPLOAD_REJECT answers with a 403 and a non-zero exit, and
// FAKEAGENT_GLAB_UPLOAD_ABSOLUTE_URL answers with an absolute URL on a foreign
// host, which the adapter must refuse to embed.
func runGlabProjectMRStub(args []string) int {
	recordGlabStubInvocation(args)

	host := os.Getenv("FAKEAGENT_GLAB_HOST")
	if host == "" {
		host = "gitlab.com"
	}
	project := os.Getenv("FAKEAGENT_GLAB_PROJECT")
	if project == "" {
		project = "example/widgets"
	}
	mrURL := fmt.Sprintf("https://%s/%s/-/merge_requests/7", host, project)

	if len(args) >= 2 && args[0] == "auth" && args[1] == "status" {
		return 0
	}
	if len(args) >= 2 && args[0] == "mr" && args[1] == "list" {
		fmt.Println("[]")
		return 0
	}
	if len(args) >= 2 && args[0] == "mr" && args[1] == "create" {
		fmt.Println(mrURL)
		return 0
	}
	if len(args) >= 2 && args[0] == "mr" && args[1] == "update" {
		fmt.Println(mrURL)
		return 0
	}
	if len(args) >= 2 && args[0] == "mr" && args[1] == "view" {
		payload := map[string]any{
			"iid":                   7,
			"title":                 "stub merge request",
			"web_url":               mrURL,
			"state":                 "merged",
			"target_branch":         "main",
			"detailed_merge_status": "mergeable",
			"sha":                   "",
			"head_pipeline":         nil,
		}
		_ = json.NewEncoder(os.Stdout).Encode(payload)
		return 0
	}
	if len(args) >= 1 && args[0] == "api" {
		return glabUploadResponse(args)
	}

	fmt.Fprintf(os.Stderr, "fakeagent glab project-mr: subcommand not implemented: %v\n", args)
	return 1
}

// glabUploadResponse answers `glab api --method POST projects/<id>/uploads
// --form file=@<path>` the way GitLab does, after checking the request has the
// shape the real endpoint needs: a POST, an uploads path, and a readable,
// non-empty file behind the form field.
func glabUploadResponse(args []string) int {
	if argAfter(args, "--method") != "POST" {
		fmt.Fprintf(os.Stderr, "fakeagent glab: api call is not a POST: %v\n", args)
		return 1
	}
	endpoint := ""
	for _, a := range args {
		if strings.HasPrefix(a, "projects/") && strings.HasSuffix(a, "/uploads") {
			endpoint = a
		}
	}
	if endpoint == "" {
		fmt.Fprintf(os.Stderr, "fakeagent glab: api call is not a project upload: %v\n", args)
		return 1
	}
	form := argAfter(args, "--form")
	if !strings.HasPrefix(form, "file=@") {
		fmt.Fprintf(os.Stderr, "fakeagent glab: upload carries no file=@<path> form field: %v\n", args)
		return 1
	}
	path := strings.TrimPrefix(form, "file=@")
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || info.Size() == 0 {
		fmt.Fprintf(os.Stderr, "fakeagent glab: upload file is not a readable non-empty file: %s (%v)\n", path, err)
		return 1
	}
	name := filepath.Base(path)
	if listsBasename(os.Getenv("FAKEAGENT_GLAB_UPLOAD_REJECT"), name) {
		fmt.Fprintln(os.Stderr, "POST https://gitlab.com/api/v4/projects/1234/uploads: 403 {message: 403 Forbidden}")
		return 1
	}
	secret := uploadSecret(name)
	if listsBasename(os.Getenv("FAKEAGENT_GLAB_UPLOAD_ABSOLUTE_URL"), name) {
		fmt.Printf(`{"id":9,"alt":%q,"url":"https://evil.example/uploads/%s/%s","full_path":"/-/project/1234/uploads/%s/%s","markdown":"![%s](https://evil.example/uploads/%s/%s)"}`+"\n",
			name, secret, name, secret, name, name, secret, name)
		return 0
	}
	// Real glab prints its update banner on stderr; the adapter must read
	// stdout on its own for the JSON to stay parseable.
	fmt.Fprintln(os.Stderr, "A new version of glab is available: 1.99.0")
	stem := strings.TrimSuffix(name, filepath.Ext(name))
	url := fmt.Sprintf("/uploads/%s/%s", secret, name)
	fmt.Printf(`{"id":9,"alt":%q,"url":%q,"full_path":"/-/project/1234%s","markdown":"![%s](%s)"}`+"\n", stem, url, url, stem, url)
	return 0
}

// uploadSecret derives GitLab's 32-hex upload secret from the file name so a
// journey can predict the URL it expects to see embedded.
func uploadSecret(name string) string {
	sum := sha256.Sum256([]byte(name))
	return hex.EncodeToString(sum[:])[:32]
}

func listsBasename(list, name string) bool {
	for _, candidate := range strings.Split(list, ",") {
		if candidate != "" && strings.TrimSpace(candidate) == name {
			return true
		}
	}
	return false
}

type glabStubInvocation struct {
	Time         string   `json:"time"`
	Args         []string `json:"args"`
	Hostname     string   `json:"hostname,omitempty"`
	SourceBranch string   `json:"source_branch,omitempty"`
	TargetBranch string   `json:"target_branch,omitempty"`
	Title        string   `json:"title,omitempty"`
	Description  string   `json:"description,omitempty"`
	UploadFile   string   `json:"upload_file,omitempty"`
}

func recordGlabStubInvocation(args []string) {
	logPath := os.Getenv("FAKEAGENT_GLAB_LOG")
	if logPath == "" {
		return
	}
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()

	inv := glabStubInvocation{
		Time:         time.Now().Format(time.RFC3339Nano),
		Args:         append([]string(nil), args...),
		Hostname:     argAfter(args, "--hostname"),
		SourceBranch: argAfter(args, "--source-branch"),
		TargetBranch: argAfter(args, "--target-branch"),
		Title:        argAfter(args, "--title"),
		Description:  argAfter(args, "--description"),
		UploadFile:   strings.TrimPrefix(argAfter(args, "--form"), "file=@"),
	}
	_ = json.NewEncoder(f).Encode(inv)
}
