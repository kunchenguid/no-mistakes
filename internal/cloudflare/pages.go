package cloudflare

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// WranglerPackage is the wrangler release whose `pages deploy` output
// ParseDeployOutput reads, used whenever no wrangler is on PATH.
const WranglerPackage = "wrangler@4.156.0"

const (
	APITokenEnv   = "CLOUDFLARE_API_TOKEN"
	AccountIDEnv  = "CLOUDFLARE_ACCOUNT_ID"
	OutputFileEnv = "WRANGLER_OUTPUT_FILE_PATH"
)

// CredentialEnv lists the variables a deploy authenticates with. Both must be
// present: without them wrangler falls back to a stored OAuth login, which
// names whichever account last ran `wrangler login` on this machine.
var CredentialEnv = []string{APITokenEnv, AccountIDEnv}

// Pages project names are a single DNS label, the subdomain they are served on.
var projectNamePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

func ValidateProjectName(name string) error {
	if !projectNamePattern.MatchString(name) {
		return fmt.Errorf("%q is not a Cloudflare Pages project name (lowercase letters, digits and inner dashes, at most 63 characters)", name)
	}
	return nil
}

type DeployRequest struct {
	Project       string
	Branch        string
	CommitHash    string
	CommitMessage string
	Dir           string
}

// Without every --commit-* flag wrangler describes the deployment from
// whatever git checkout it runs in, and without --branch it may delegate a new
// project's deploy to Workers.
func (r DeployRequest) Args() []string {
	return []string{
		"pages", "deploy", r.Dir,
		"--project-name=" + r.Project,
		"--branch=" + r.Branch,
		"--commit-hash=" + r.CommitHash,
		"--commit-message=" + r.CommitMessage,
		"--commit-dirty=true",
	}
}

type deployOutputEntry struct {
	Type    string `json:"type"`
	Version int    `json:"version"`
	URL     string `json:"url"`
}

// ParseDeployOutput returns the deployment URL from the ndjson wrangler writes
// to WRANGLER_OUTPUT_FILE_PATH. The deployment URL, unlike the branch alias,
// keeps serving these bytes after a later deploy to the same branch.
func ParseDeployOutput(data []byte) (string, error) {
	var deployed *deployOutputEntry
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var entry deployOutputEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			return "", fmt.Errorf("wrangler output is not ndjson: %w", err)
		}
		if entry.Type == "pages-deploy-detailed" {
			deployed = &entry
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read wrangler output: %w", err)
	}
	if deployed == nil {
		return "", errors.New("wrangler reported no pages deployment")
	}
	if deployed.Version != 1 {
		return "", fmt.Errorf("wrangler reported pages deployment output version %d, want 1", deployed.Version)
	}
	return deploymentURL(deployed.URL)
}

func deploymentURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.Trim(u.Path, "/") != "" {
		return "", fmt.Errorf("wrangler reported deployment URL %q, want an https origin", raw)
	}
	host := strings.ToLower(u.Hostname())
	if u.Port() != "" || !strings.HasSuffix(host, ".pages.dev") {
		return "", fmt.Errorf("wrangler reported deployment URL %q, want a pages.dev host", raw)
	}
	return "https://" + host, nil
}
