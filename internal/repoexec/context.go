// Package repoexec owns the optional, non-secret execution context attached to
// one registered repository. It deliberately models fields instead of accepting
// arbitrary environment variables or shell fragments.
package repoexec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"unicode"
)

const (
	GitHubContextVersion = 1
	GitHubHost           = "github.com"
	GitProtocolHTTPS     = "https"
	CredentialHelperGH   = "gh"
	maxContextFileBytes  = 64 * 1024
)

var ErrInvalidGitHubContextJSON = errors.New("invalid GitHub context JSON")

// CommitAuthor is the identity used for commits created by the pipeline.
type CommitAuthor struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// GitHubContext is a strict, non-secret GitHub and Git child-process context.
// Authentication remains in gh's configured operating-system credential store;
// this type must never gain a token, password, header, arbitrary environment,
// or shell-fragment field.
type GitHubContext struct {
	Version          int          `json:"version"`
	GHPath           string       `json:"gh_path"`
	GitPath          string       `json:"git_path"`
	GHConfigDir      string       `json:"gh_config_dir"`
	Host             string       `json:"host"`
	ExpectedLogin    string       `json:"expected_login"`
	GitProtocol      string       `json:"git_protocol"`
	CredentialHelper string       `json:"credential_helper"`
	CommitAuthor     CommitAuthor `json:"commit_author"`
	Label            string       `json:"label,omitempty"`
}

type contextKey struct{}
type localGitTransferKey struct{}

type localGitTransfer struct {
	source      string
	destination string
}

// WithGitHubContext binds selected to ctx without mutating process-global state.
func WithGitHubContext(ctx context.Context, selected *GitHubContext) context.Context {
	if selected == nil {
		return ctx
	}
	copy := *selected
	return context.WithValue(ctx, contextKey{}, &copy)
}

// GitHubContextFrom returns the repository context bound to ctx.
func GitHubContextFrom(ctx context.Context) (*GitHubContext, bool) {
	if ctx == nil {
		return nil, false
	}
	selected, ok := ctx.Value(contextKey{}).(*GitHubContext)
	return selected, ok && selected != nil
}

func WithTrustedLocalGitTransfer(ctx context.Context, source, destination string) context.Context {
	return context.WithValue(ctx, localGitTransferKey{}, localGitTransfer{source: source, destination: destination})
}

// LoadGitHubContext decodes a strict context document. Unknown fields are
// rejected so credential material can never be smuggled into a persisted
// catch-all map.
func LoadGitHubContext(path string) (*GitHubContext, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open GitHub context file: %w", err)
	}
	defer f.Close()
	if info, statErr := f.Stat(); statErr != nil {
		return nil, fmt.Errorf("stat GitHub context file: %w", statErr)
	} else if !info.Mode().IsRegular() {
		return nil, errors.New("GitHub context file must be a regular file")
	} else if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("GitHub context file must not be readable or writable by group or others (use mode 0600)")
	}

	data, err := io.ReadAll(io.LimitReader(f, maxContextFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read GitHub context file: %w", err)
	}
	if len(data) > maxContextFileBytes {
		return nil, fmt.Errorf("GitHub context file exceeds %d bytes", maxContextFileBytes)
	}
	selected, err := DecodeGitHubContext(data)
	if err != nil {
		return nil, fmt.Errorf("parse GitHub context file: %w", err)
	}
	return selected, nil
}

// DecodeGitHubContext decodes exactly one strict context JSON value.
func DecodeGitHubContext(data []byte) (*GitHubContext, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var selected GitHubContext
	if err := dec.Decode(&selected); err != nil {
		return nil, ErrInvalidGitHubContextJSON
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, ErrInvalidGitHubContextJSON
	}
	return &selected, nil
}

// ValidateDependencies validates the typed shape and local dependencies before
// an exact binary is used. It does not read credentials.
func (c *GitHubContext) ValidateDependencies() error {
	if c == nil {
		return nil
	}
	if c.Version != GitHubContextVersion {
		return c.errorf("unsupported version %d (want %d)", c.Version, GitHubContextVersion)
	}
	c.GHPath = filepath.Clean(strings.TrimSpace(c.GHPath))
	c.GitPath = filepath.Clean(strings.TrimSpace(c.GitPath))
	c.GHConfigDir = filepath.Clean(strings.TrimSpace(c.GHConfigDir))
	c.ExpectedLogin = strings.TrimSpace(c.ExpectedLogin)
	c.GitProtocol = strings.ToLower(strings.TrimSpace(c.GitProtocol))
	c.CredentialHelper = strings.ToLower(strings.TrimSpace(c.CredentialHelper))
	c.CommitAuthor.Name = strings.TrimSpace(c.CommitAuthor.Name)
	c.CommitAuthor.Email = strings.TrimSpace(c.CommitAuthor.Email)
	c.Label = strings.TrimSpace(c.Label)
	if !strings.EqualFold(strings.TrimSpace(c.Host), GitHubHost) {
		return c.errorf("host must be github.com")
	}
	c.Host = GitHubHost
	if strings.TrimSpace(c.GitProtocol) != GitProtocolHTTPS {
		return c.errorf("git_protocol must be https")
	}
	if strings.TrimSpace(c.CredentialHelper) != CredentialHelperGH {
		return c.errorf("credential_helper must be gh")
	}
	if !validLogin(c.ExpectedLogin) {
		return c.errorf("expected_login is invalid")
	}
	if c.Label != "" && !validLabel(c.Label) {
		return c.errorf("label must contain only letters, digits, spaces, '.', '_' or '-' and be at most 64 characters")
	}
	if !validIdentity(c.CommitAuthor) {
		return c.errorf("commit_author must contain a non-empty single-line name and email address")
	}
	for name, path := range map[string]string{"gh_path": c.GHPath, "git_path": c.GitPath} {
		if err := validateExecutable(path); err != nil {
			return c.errorf("%s must name an absolute executable file", name)
		}
	}
	if !canonicalExecutableName(c.GHPath, "gh") || !canonicalExecutableName(c.GitPath, "git") {
		return c.errorf("gh_path and git_path must use the canonical gh and git executable names")
	}
	if !filepath.IsAbs(c.GHConfigDir) {
		return c.errorf("gh_config_dir must be absolute")
	}
	if info, err := os.Stat(c.GHConfigDir); err != nil || !info.IsDir() {
		return c.errorf("gh_config_dir is missing or is not a directory")
	}
	if err := validateSelectedPATHPair(c.GHPath, c.GitPath); err != nil {
		return c.errorf("selected executable directories contain a conflicting gh or git executable")
	}
	if err := c.ValidateForPersistence(); err != nil {
		return c.errorf("context contains credential-like material; store credentials only through gh auth")
	}
	return nil
}

func canonicalExecutableName(path, name string) bool {
	return canonicalExecutableNameForOS(path, name, runtime.GOOS)
}

func canonicalExecutableNameForOS(path, name, goos string) bool {
	base := filepath.Base(path)
	if goos == "windows" {
		if separator := strings.LastIndexAny(path, `/\\`); separator >= 0 {
			base = path[separator+1:]
		}
		return strings.EqualFold(base, name) || strings.EqualFold(base, name+".exe")
	}
	return base == name
}

// ValidateStatic validates dependencies and the initial HTTPS-only github.com
// routing surface without reading credentials.
func (c *GitHubContext) ValidateStatic(upstreamURL, forkURL string) error {
	if c == nil {
		return nil
	}
	if err := c.ValidateDependencies(); err != nil {
		return err
	}
	if err := validateGitHubHTTPSURL(upstreamURL); err != nil {
		return c.errorf("origin must be an HTTPS github.com owner/repository URL without userinfo")
	}
	if strings.TrimSpace(forkURL) != "" {
		if err := validateGitHubHTTPSURL(forkURL); err != nil {
			return c.errorf("fork must be an HTTPS github.com owner/repository URL without userinfo")
		}
	}
	return nil
}

func validateExecutable(path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("not absolute")
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("missing")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		return errors.New("not executable")
	}
	return nil
}

func validateSelectedPATHPair(ghPath, gitPath string) error {
	selected := map[string]string{"gh": cleanExecutablePath(ghPath), "git": cleanExecutablePath(gitPath)}
	for _, dir := range uniqueStrings([]string{filepath.Dir(ghPath), filepath.Dir(gitPath)}) {
		for name, want := range selected {
			for _, candidate := range executableCandidates(dir, name) {
				if _, err := os.Stat(candidate); err != nil {
					continue
				}
				if cleanExecutablePath(candidate) != want {
					return errors.New("conflict")
				}
			}
		}
	}
	return nil
}

func executableCandidates(dir, name string) []string {
	candidates := []string{filepath.Join(dir, name)}
	if runtime.GOOS == "windows" {
		for _, ext := range []string{".exe", ".com", ".bat", ".cmd"} {
			candidates = append(candidates, filepath.Join(dir, name+ext))
		}
	}
	return candidates
}

func cleanExecutablePath(path string) string {
	cleaned := filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(cleaned); err == nil {
		cleaned = resolved
	}
	if runtime.GOOS == "windows" {
		cleaned = strings.ToLower(cleaned)
	}
	return cleaned
}

func validLogin(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 39 || strings.HasPrefix(value, "-") || strings.HasSuffix(value, "-") {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' {
			continue
		}
		return false
	}
	return true
}

func validLabel(value string) bool {
	if value == "" || len([]rune(value)) > 64 || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == ' ' || r == '.' || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

func validIdentity(identity CommitAuthor) bool {
	name := strings.TrimSpace(identity.Name)
	email := strings.TrimSpace(identity.Email)
	if name == "" || email == "" || hasControl(name) || hasControl(email) || strings.ContainsAny(name+email, "\r\n") {
		return false
	}
	at := strings.LastIndexByte(email, '@')
	return at > 0 && at < len(email)-1 && !strings.ContainsAny(email, " <>\t")
}

func hasControl(value string) bool {
	return strings.IndexFunc(value, unicode.IsControl) >= 0
}

func looksLikeCredential(value string) bool {
	lower := strings.ToLower(value)
	for _, marker := range []string{"github_pat_", "ghp_", "gho_", "ghu_", "ghs_", "ghr_", "bearer ", "authorization:"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func validateGitHubHTTPSURL(raw string) error {
	if looksLikeCredential(raw) {
		return errors.New("credential-like URL")
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !strings.EqualFold(u.Scheme, "https") || !strings.EqualFold(u.Hostname(), GitHubHost) || u.User != nil || u.Port() != "" || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("unsupported URL")
	}
	parts := strings.Split(strings.Trim(strings.TrimSuffix(u.Path, ".git"), "/"), "/")
	if len(parts) != 2 || !validGitHubPathSegment(parts[0]) || !validGitHubPathSegment(parts[1]) {
		return errors.New("missing owner/repository")
	}
	return nil
}

func validGitHubPathSegment(value string) bool {
	if value == "" || value == "." || value == ".." {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

// Environment returns an exact child environment for this repository. Higher
// precedence ambient token, config-injection, askpass, SSH, and author variables
// are removed before the typed values are installed.
func (c *GitHubContext) Environment(base []string, dir string) []string {
	if c == nil {
		return base
	}
	if base == nil {
		base = os.Environ()
	}
	blockedExact := map[string]bool{
		"GH_TOKEN": true, "GITHUB_TOKEN": true, "GH_ENTERPRISE_TOKEN": true, "GITHUB_ENTERPRISE_TOKEN": true,
		"GH_CONFIG_DIR": true, "GH_HOST": true, "GH_REPO": true, "GH_PROMPT_DISABLED": true, "GH_NO_UPDATE_NOTIFIER": true,
		"GIT_CONFIG_COUNT": true, "GIT_CONFIG_PARAMETERS": true, "GIT_CONFIG_GLOBAL": true, "GIT_CONFIG_SYSTEM": true, "GIT_CONFIG_NOSYSTEM": true,
		"GIT_ASKPASS": true, "SSH_ASKPASS": true, "SSH_ASKPASS_REQUIRE": true, "GIT_SSH": true, "GIT_SSH_COMMAND": true, "GIT_SSH_VARIANT": true, "SSH_AUTH_SOCK": true,
		"GIT_AUTHOR_NAME": true, "GIT_AUTHOR_EMAIL": true, "GIT_COMMITTER_NAME": true, "GIT_COMMITTER_EMAIL": true, "EMAIL": true,
		"GIT_DIR": true, "GIT_WORK_TREE": true, "GIT_COMMON_DIR": true, "GIT_INDEX_FILE": true, "GIT_OBJECT_DIRECTORY": true, "GIT_ALTERNATE_OBJECT_DIRECTORIES": true, "GIT_NAMESPACE": true,
		"GIT_EXEC_PATH": true, "GIT_TEMPLATE_DIR": true, "GIT_CEILING_DIRECTORIES": true, "GIT_DISCOVERY_ACROSS_FILESYSTEM": true, "GIT_PROXY_COMMAND": true, "GIT_CURL_VERBOSE": true,
		"GIT_SSL_NO_VERIFY": true, "GIT_SSL_CAINFO": true, "GIT_SSL_CAPATH": true, "GIT_SSL_CERT": true, "GIT_SSL_KEY": true, "GIT_SSL_CERT_PASSWORD_PROTECTED": true,
		"GIT_SSL_VERSION": true, "GIT_SSL_CIPHER_LIST": true, "GIT_HTTP_PROXY": true, "GIT_HTTPS_PROXY": true,
		"HTTP_PROXY": true, "HTTPS_PROXY": true, "ALL_PROXY": true, "NO_PROXY": true,
		"http_proxy": true, "https_proxy": true, "all_proxy": true, "no_proxy": true,
		"CURL_CA_BUNDLE": true, "CURL_SSL_BACKEND": true, "SSL_CERT_FILE": true, "SSL_CERT_DIR": true, "OPENSSL_CONF": true, "OPENSSL_MODULES": true,
		"GIT_TERMINAL_PROMPT": true, "GCM_INTERACTIVE": true, "GIT_EDITOR": true, "GIT_SEQUENCE_EDITOR": true, "GIT_MERGE_AUTOEDIT": true,
		"EDITOR": true, "VISUAL": true, "GIT_PAGER": true, "PAGER": true, "PATH": true,
	}
	out := make([]string, 0, len(base)+32)
	ambientPath := ""
	for _, entry := range base {
		key, _, found := strings.Cut(entry, "=")
		canonical := key
		if runtime.GOOS == "windows" {
			canonical = strings.ToUpper(canonical)
		}
		if strings.EqualFold(canonical, "PATH") {
			_, ambientPath, _ = strings.Cut(entry, "=")
			continue
		}
		if blockedExact[canonical] || strings.HasPrefix(canonical, "GIT_CONFIG_KEY_") || strings.HasPrefix(canonical, "GIT_CONFIG_VALUE_") || strings.HasPrefix(canonical, "GIT_TRACE") {
			continue
		}
		if found {
			out = append(out, entry)
		}
	}

	selectedDirs := uniqueStrings([]string{filepath.Dir(c.GHPath), filepath.Dir(c.GitPath)})
	pathEntries := append([]string(nil), selectedDirs...)
	for _, entry := range filepath.SplitList(ambientPath) {
		if entry != "" && !containsPath(pathEntries, entry) {
			pathEntries = append(pathEntries, entry)
		}
	}
	out = append(out,
		"PATH="+strings.Join(pathEntries, string(os.PathListSeparator)),
		"GH_CONFIG_DIR="+c.GHConfigDir,
		"GH_HOST="+GitHubHost,
		"GH_PROMPT_DISABLED=1",
		"GH_NO_UPDATE_NOTIFIER=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GCM_INTERACTIVE=Never",
		"GIT_EDITOR=true",
		"GIT_SEQUENCE_EDITOR=true",
		"GIT_MERGE_AUTOEDIT=no",
		"EDITOR=true",
		"VISUAL=true",
		"GIT_PAGER=cat",
		"PAGER=cat",
	)

	config := [][2]string{
		{"credential.helper", ""},
		{"credential.helper", credentialHelperCommand(c.GHPath)},
		{"credential.useHttpPath", "false"},
		{"http.extraHeader", ""},
		{"core.askPass", ""},
		{"core.sshCommand", ""},
		{"user.name", c.CommitAuthor.Name},
		{"user.email", c.CommitAuthor.Email},
		{"user.useConfigOnly", "true"},
	}
	out = append(out, "GIT_CONFIG_COUNT="+strconv.Itoa(len(config)))
	for i, pair := range config {
		out = append(out,
			fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, pair[0]),
			fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, pair[1]),
		)
	}
	return out
}

func credentialHelperCommand(ghPath string) string {
	return "!" + shellQuote(ghPath) + " auth git-credential"
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func uniqueStrings(values []string) []string {
	var out []string
	for _, value := range values {
		if value == "" || containsPath(out, value) {
			continue
		}
		out = append(out, value)
	}
	return out
}

func containsPath(values []string, candidate string) bool {
	candidate = filepath.Clean(candidate)
	for _, value := range values {
		value = filepath.Clean(value)
		if runtime.GOOS == "windows" {
			if strings.EqualFold(value, candidate) {
				return true
			}
		} else if value == candidate {
			return true
		}
	}
	return false
}

// ValidateForPersistence rejects credential-like material before a context can
// enter durable state. Runtime/static validation applies the fuller contract.
func (c *GitHubContext) ValidateForPersistence() error {
	if c == nil {
		return nil
	}
	for _, value := range []string{c.GHPath, c.GitPath, c.GHConfigDir, c.ExpectedLogin, c.CommitAuthor.Name, c.CommitAuthor.Email, c.Label} {
		if looksLikeCredential(value) {
			return errors.New("repository GitHub context contains credential-like material")
		}
	}
	return nil
}

// LabelForDiagnostics returns a sanitized, non-secret context description.
func (c *GitHubContext) LabelForDiagnostics() string {
	if c != nil && validLabel(c.Label) && !looksLikeCredential(c.Label) {
		return fmt.Sprintf("GitHub context %q", c.Label)
	}
	return "repository GitHub context"
}

func (c *GitHubContext) errorf(format string, args ...any) error {
	return fmt.Errorf("%s: %s", c.LabelForDiagnostics(), fmt.Sprintf(format, args...))
}
