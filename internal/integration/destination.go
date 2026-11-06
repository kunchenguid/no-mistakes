// Package integration binds an explicit integration remote to one run.
package integration

import (
	"context"
	"crypto/rand"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/evidence"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
)

// Destination is immutable run input. URL never contains embedded credentials.
// Key owns a private ref namespace so concurrent runs never overwrite origin.
type Destination struct {
	Remote        string `json:"remote"`
	URL           string `json:"url"`
	TransportURL  string `json:"transport_url"`
	DefaultBranch string `json:"default_branch"`
	Key           string `json:"key"`
}

type requestKey struct{}
type request struct {
	remote string
	pinned *Destination
}

// WithRemote carries an operator's selection through admission helpers. Only
// the request context carries it; execution and recovery use the durable pin.
func WithRemote(ctx context.Context, remote string) context.Context {
	return context.WithValue(ctx, requestKey{}, request{remote: remote})
}

func WithPinned(ctx context.Context, pinned *Destination) context.Context {
	return context.WithValue(ctx, requestKey{}, request{pinned: pinned})
}

func RequestedRemote(ctx context.Context) string {
	r, _ := ctx.Value(requestKey{}).(request)
	if r.pinned != nil {
		return r.pinned.Remote
	}
	return r.remote
}

func RemoteName(destination *Destination) string {
	if destination != nil {
		return destination.Remote
	}
	return "origin"
}

func MatchesRemote(selected, requested string) bool {
	if selected == "" {
		selected = "origin"
	}
	return requested == "" || selected == requested
}

func ValidateRemote(name string) error {
	if name == "" || strings.HasPrefix(name, "-") || strings.ContainsAny(name, " \t\r\n\x00") {
		return fmt.Errorf("base remote must be a configured remote name")
	}
	if _, err := evidence.NormalizeBranch(name); err != nil {
		return fmt.Errorf("invalid base remote name")
	}
	return nil
}

func remoteURL(ctx context.Context, workDir, name string) (string, error) {
	if err := ValidateRemote(name); err != nil {
		return "", err
	}
	names, err := git.Run(ctx, workDir, "remote")
	if err != nil {
		return "", fmt.Errorf("list integration remotes: %w", err)
	}
	found := false
	for _, candidate := range strings.Split(names, "\n") {
		found = found || candidate == name
	}
	if !found {
		return "", fmt.Errorf("base remote %q is not configured", name)
	}
	remote, err := git.GetRemoteURL(ctx, workDir, name)
	if err != nil || strings.TrimSpace(remote) == "" {
		return "", fmt.Errorf("base remote %q has no readable URL", name)
	}
	// A local relative URL is relative to the invoking clone, not the gate.
	if !filepath.IsAbs(remote) && !strings.Contains(remote, ":") {
		remote = filepath.Join(workDir, remote)
		remote, err = filepath.Abs(remote)
		if err != nil {
			return "", fmt.Errorf("resolve base remote path: %w", err)
		}
	}
	return remote, nil
}

func configuredURL(ctx context.Context, workDir, name string) (string, error) {
	urls, err := git.GetConfiguredRemoteURLs(ctx, workDir, name)
	if err != nil || len(urls) != 1 || strings.TrimSpace(urls[0]) == "" {
		return "", fmt.Errorf("base remote %q must have exactly one URL", name)
	}
	remote := urls[0]
	if !filepath.IsAbs(remote) && !strings.Contains(remote, ":") {
		return filepath.Abs(filepath.Join(workDir, remote))
	}
	return remote, nil
}

// ResolveRequest refuses unknown names and unreadable remote HEADs. An absent
// selector retains the existing origin behavior, including legacy rows.
func ResolveRequest(ctx context.Context, workDir string) (*Destination, error) {
	r, _ := ctx.Value(requestKey{}).(request)
	if r.pinned != nil {
		if _, err := r.pinned.RuntimeURL(ctx, workDir); err != nil {
			return nil, err
		}
		copy := *r.pinned
		copy.Key = rand.Text()
		return &copy, nil
	}
	if r.remote == "" {
		return nil, nil
	}
	remote, err := remoteURL(ctx, workDir, r.remote)
	if err != nil {
		return nil, err
	}
	identity, err := configuredURL(ctx, workDir, r.remote)
	if err != nil {
		return nil, err
	}
	out, err := git.Run(ctx, workDir, "ls-remote", "--symref", remote, "HEAD")
	if err != nil {
		return nil, fmt.Errorf("resolve base remote %q: %w", r.remote, err)
	}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == "ref:" && fields[2] == "HEAD" && strings.HasPrefix(fields[1], "refs/heads/") {
			branch := strings.TrimPrefix(fields[1], "refs/heads/")
			if _, err := evidence.NormalizeBranch(branch); err != nil {
				break
			}
			return &Destination{Remote: r.remote, URL: safeurl.Redact(identity), TransportURL: safeurl.Redact(remote), DefaultBranch: branch, Key: rand.Text()}, nil
		}
	}
	return nil, fmt.Errorf("base remote %q has no resolvable default branch", r.remote)
}

// RuntimeURL recovers credentials only from the same named destination. Moving
// or removing that remote refuses rather than falling back to origin.
func (d *Destination) RuntimeURL(ctx context.Context, workDir string) (string, error) {
	if err := d.Validate(); err != nil {
		return "", err
	}
	remote, err := remoteURL(ctx, workDir, d.Remote)
	if err != nil {
		return "", err
	}
	identity, err := configuredURL(ctx, workDir, d.Remote)
	if err != nil {
		return "", err
	}
	if safeurl.Redact(identity) != d.URL || safeurl.Redact(remote) != d.TransportURL {
		return "", fmt.Errorf("base remote %q changed since this run was admitted", d.Remote)
	}
	return remote, nil
}

func (d *Destination) Ref(branch string) string {
	return "refs/no-mistakes/integration/" + d.Key + "/" + branch
}

func (d *Destination) Validate() error {
	if d == nil || ValidateRemote(d.Remote) != nil || d.URL == "" || d.TransportURL == "" || safeurl.Redact(d.URL) != d.URL || safeurl.Redact(d.TransportURL) != d.TransportURL || len(d.Key) < 16 {
		return fmt.Errorf("invalid integration destination pin")
	}
	for _, c := range d.Key {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return fmt.Errorf("invalid integration destination namespace")
		}
	}
	if _, err := evidence.NormalizeBranch(d.DefaultBranch); err != nil {
		return fmt.Errorf("invalid integration default branch")
	}
	return nil
}

func (d Destination) Value() (driver.Value, error) {
	if err := d.Validate(); err != nil {
		return nil, err
	}
	b, err := json.Marshal(d)
	return string(b), err
}

func (d *Destination) Scan(src any) error {
	var b []byte
	switch v := src.(type) {
	case string:
		b = []byte(v)
	case []byte:
		b = v
	default:
		return fmt.Errorf("invalid persisted integration destination")
	}
	if err := json.Unmarshal(b, d); err != nil {
		return fmt.Errorf("invalid persisted integration destination")
	}
	return d.Validate()
}
