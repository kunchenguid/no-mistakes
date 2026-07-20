package repoexec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/winproc"
)

const maxValidationOutputBytes = 4096

// ValidateRuntime verifies dependencies, selected login, repository access,
// push permission, and local Git override safety without asking gh to return a
// token or running Git's credential fill protocol.
func (c *GitHubContext) ValidateRuntime(ctx context.Context, workDir, upstreamURL, forkURL string) error {
	if c == nil {
		return nil
	}
	if err := c.ValidateStatic(upstreamURL, forkURL); err != nil {
		return err
	}
	if _, err := c.run(ctx, workDir, c.GitPath, "--version"); err != nil {
		return c.errorf("git_path could not be executed")
	}
	if err := c.ValidateLocalGitConfig(ctx, workDir); err != nil {
		return err
	}
	if _, err := c.run(ctx, workDir, c.GHPath, "auth", "status", "--hostname", GitHubHost); err != nil {
		return c.errorf("gh authentication is unavailable; authenticate the selected gh_config_dir for github.com")
	}
	if err := c.ValidateLogin(ctx, workDir); err != nil {
		return err
	}

	parent := githubSlug(upstreamURL)
	if parent == "" {
		return c.errorf("origin repository could not be resolved")
	}
	parentPermission, err := c.repositoryPermission(ctx, workDir, parent)
	if err != nil {
		return c.errorf("cannot read the parent repository with the selected login; verify access and organization SSO authorization")
	}
	if strings.TrimSpace(forkURL) == "" {
		if !permissionCanPush(parentPermission) {
			return c.errorf("selected login cannot push to the parent repository; configure a writable fork or grant write access")
		}
		return nil
	}
	if !permissionCanRead(parentPermission) {
		return c.errorf("selected login cannot read the parent repository; verify access and organization SSO authorization")
	}
	fork := githubSlug(forkURL)
	forkPermission, err := c.repositoryPermission(ctx, workDir, fork)
	if err != nil || !permissionCanPush(forkPermission) {
		return c.errorf("selected login cannot push to the configured fork; verify fork ownership, access, and organization SSO authorization")
	}
	return nil
}

// ValidateLogin verifies the account selected by GH_CONFIG_DIR without asking
// gh for token bytes. It is repeated before provider and network Git commands
// so an externally changed profile fails closed mid-run.
func (c *GitHubContext) ValidateLogin(ctx context.Context, workDir string) error {
	if c == nil {
		return nil
	}
	out, err := c.run(ctx, workDir, c.GHPath, "api", "--hostname", GitHubHost, "user", "--jq", ".login")
	if err != nil {
		return c.errorf("could not verify the selected GitHub login; re-authenticate the selected gh_config_dir")
	}
	login := strings.TrimSpace(out)
	if !validLogin(login) {
		return c.errorf("gh returned an invalid login while validating the selected account")
	}
	if !strings.EqualFold(login, strings.TrimSpace(c.ExpectedLogin)) {
		return c.errorf("authenticated login does not match expected_login %q; re-authenticate the selected gh_config_dir", c.ExpectedLogin)
	}
	return nil
}

// ValidateLocalGitConfig rejects repository-controlled keys that could reroute
// credentials or network destinations. Only key names are read; values (which
// could contain headers or helper output) are never requested.
func (c *GitHubContext) ValidateLocalGitConfig(ctx context.Context, workDir string) error {
	if c == nil || strings.TrimSpace(workDir) == "" {
		return nil
	}
	out, err := c.run(ctx, workDir, c.GitPath, "config", "--local", "--name-only", "--list")
	if err != nil {
		return c.errorf("could not inspect repository-local Git configuration")
	}
	for _, line := range strings.Split(out, "\n") {
		key := strings.ToLower(strings.TrimSpace(line))
		if unsafeLocalGitKey(key) {
			return c.errorf("repository-local Git configuration contains a credential, header, URL rewrite, push URL, askpass, or SSH override; remove it or leave the repository context disabled")
		}
	}
	return nil
}

func unsafeLocalGitKey(key string) bool {
	if strings.HasPrefix(key, "credential.") || key == "http.extraheader" || key == "core.sshcommand" || key == "core.askpass" {
		return true
	}
	if strings.HasPrefix(key, "http.") && strings.HasSuffix(key, ".extraheader") {
		return true
	}
	if strings.HasPrefix(key, "url.") && (strings.HasSuffix(key, ".insteadof") || strings.HasSuffix(key, ".pushinsteadof")) {
		return true
	}
	return strings.HasPrefix(key, "remote.") && strings.HasSuffix(key, ".pushurl")
}

// ValidateNetworkRemote resolves a named remote without applying URL rewrites,
// then enforces the initial HTTPS-only github.com surface. It also repeats the
// selected-login and local-config checks immediately before network access.
func (c *GitHubContext) ValidateNetworkRemote(ctx context.Context, workDir, remote string) error {
	if c == nil {
		return nil
	}
	if err := c.ValidateLogin(ctx, workDir); err != nil {
		return err
	}
	if err := c.ValidateLocalGitConfig(ctx, workDir); err != nil {
		return err
	}
	raw := strings.TrimSpace(remote)
	if !strings.Contains(raw, "://") {
		if !validRemoteName(raw) {
			return c.errorf("Git remote name is invalid")
		}
		resolved, err := c.run(ctx, workDir, c.GitPath, "config", "--get", "remote."+raw+".url")
		if err != nil {
			return c.errorf("could not resolve Git remote %q", raw)
		}
		raw = strings.TrimSpace(resolved)
	}
	if err := validateGitHubHTTPSURL(raw); err != nil {
		return c.errorf("network Git operation refused a non-HTTPS or non-github.com remote")
	}
	return nil
}

func validRemoteName(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("._-/", r) {
			continue
		}
		return false
	}
	return !strings.Contains(value, "..")
}

func (c *GitHubContext) repositoryPermission(ctx context.Context, workDir, slug string) (string, error) {
	if slug == "" {
		return "", errors.New("missing repository")
	}
	out, err := c.run(ctx, workDir, c.GHPath, "repo", "view", slug, "--json", "viewerPermission", "--jq", ".viewerPermission")
	if err != nil {
		return "", err
	}
	permission := strings.ToUpper(strings.TrimSpace(out))
	switch permission {
	case "READ", "TRIAGE", "WRITE", "MAINTAIN", "ADMIN":
		return permission, nil
	default:
		return "", errors.New("unknown permission")
	}
}

func permissionCanRead(permission string) bool {
	switch permission {
	case "READ", "TRIAGE", "WRITE", "MAINTAIN", "ADMIN":
		return true
	default:
		return false
	}
}

func permissionCanPush(permission string) bool {
	switch permission {
	case "WRITE", "MAINTAIN", "ADMIN":
		return true
	default:
		return false
	}
}

func githubSlug(raw string) string {
	trimmed := strings.Trim(strings.TrimSuffix(strings.TrimSpace(raw), ".git"), "/")
	const prefix = "https://github.com/"
	if !strings.HasPrefix(strings.ToLower(trimmed), prefix) {
		return ""
	}
	return trimmed[len(prefix):]
}

func (c *GitHubContext) run(ctx context.Context, workDir, path string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir = workDir
	cmd.Env = c.Environment(os.Environ(), workDir)
	winproc.Harden(cmd)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	if len(out) > maxValidationOutputBytes {
		return "", fmt.Errorf("validation output exceeded limit")
	}
	return strings.TrimSpace(string(out)), nil
}
