package intent

import (
	"context"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/winproc"
)

// resolveHome returns the home directory to use, preferring an explicit
// override (used in tests) over the OS-reported value.
func resolveHome(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	return os.UserHomeDir()
}

// canonicalPath returns the path with symlinks evaluated and cleaned. It
// silently falls back to filepath.Clean(abs) if EvalSymlinks fails (e.g.
// the path does not exist), since both sides of the comparison receive
// the same fallback treatment.
func canonicalPath(p string) string {
	if p == "" {
		return ""
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(abs)
}

// pathsEqual compares two paths after canonicalization.
func pathsEqual(a, b string) bool {
	return canonicalPath(a) == canonicalPath(b)
}

type repoMatcher struct {
	origin string
	ids    map[string]repoIdentity
}

type repoIdentity struct {
	canonical string
	commonDir string
	remote    string
}

func newRepoMatcher(ctx context.Context, originCWD string) *repoMatcher {
	m := &repoMatcher{
		origin: canonicalPath(originCWD),
		ids:    make(map[string]repoIdentity),
	}
	if m.origin != "" {
		m.ids[m.origin] = gitRepoIdentity(ctx, originCWD)
	}
	return m
}

func (m *repoMatcher) matches(ctx context.Context, cwd string) bool {
	if m == nil || m.origin == "" {
		return true
	}
	candidate := canonicalPath(cwd)
	if candidate == "" {
		return false
	}
	if candidate == m.origin {
		return true
	}
	origin := m.ids[m.origin]
	candidateID, ok := m.ids[candidate]
	if !ok {
		candidateID = gitRepoIdentity(ctx, cwd)
		m.ids[candidate] = candidateID
	}
	if origin.commonDir != "" && candidateID.commonDir != "" && origin.commonDir == candidateID.commonDir {
		return true
	}
	return origin.remote != "" && candidateID.remote != "" && origin.remote == candidateID.remote
}

func gitRepoIdentity(ctx context.Context, dir string) repoIdentity {
	id := repoIdentity{canonical: canonicalPath(dir)}
	if id.canonical == "" {
		return id
	}
	if commonDir := gitOutput(ctx, dir, "rev-parse", "--git-common-dir"); commonDir != "" {
		if !filepath.IsAbs(commonDir) {
			commonDir = filepath.Join(dir, commonDir)
		}
		id.commonDir = canonicalPath(commonDir)
	}
	remote := gitOutput(ctx, dir, "remote", "get-url", "origin")
	if remote == "" {
		if first := firstLine(gitOutput(ctx, dir, "remote")); first != "" {
			remote = gitOutput(ctx, dir, "remote", "get-url", first)
		}
	}
	id.remote = normalizeGitRemote(remote)
	return id
}

func gitOutput(ctx context.Context, dir string, args ...string) string {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	winproc.Harden(cmd)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func normalizeGitRemote(remote string) string {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return ""
	}
	if u, err := url.Parse(remote); err == nil && u.Host != "" {
		return cleanRemoteParts(u.Host, u.Path)
	}
	if at := strings.Index(remote, "@"); at >= 0 {
		rest := remote[at+1:]
		if colon := strings.Index(rest, ":"); colon >= 0 {
			return cleanRemoteParts(rest[:colon], rest[colon+1:])
		}
	}
	if filepath.IsAbs(remote) {
		return canonicalPath(remote)
	}
	return cleanRemoteParts("", remote)
}

func cleanRemoteParts(host, path string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	path = strings.Trim(strings.TrimSpace(path), "/")
	path = strings.TrimSuffix(path, ".git")
	path = strings.ToLower(path)
	if host == "" {
		return path
	}
	if path == "" {
		return host
	}
	return host + "/" + path
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}
