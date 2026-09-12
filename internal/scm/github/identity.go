package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// parseDiscoveredPRURL keeps the strict URL parser as the ordinary path and
// permits a changed repository slug only after authenticated identity proof.
// The proof shape is adapted from Bradley Waddington's closed, unmerged
// https://github.com/kunchenguid/no-mistakes/pull/1058 commit
// c0549c14610489c0d01eb5580ea16021685c8437.
func (h *Host) parseDiscoveredPRURL(ctx context.Context, raw string) (int, error) {
	number, err := parsePullRequestURL(raw, h.host, h.repoSlug())
	if err == nil {
		return number, nil
	}
	if h.host == "" {
		return 0, fmt.Errorf("cannot verify renamed GitHub repository without a known host: %w", err)
	}
	number, shapeErr := parsePullRequestURL(raw, h.host, "")
	if shapeErr != nil {
		return 0, shapeErr
	}
	currentSlug := RepoSlug(raw)
	if strings.EqualFold(currentSlug, h.repoSlug()) {
		return 0, err
	}
	if verifyErr := verifyRepositorySlugs(ctx, h.cmd, h.host, h.repoSlug(), currentSlug); verifyErr != nil {
		return 0, verifyErr
	}
	return number, nil
}

// VerifyRepositoryRename proves that previousTarget and currentTarget are two
// names for one GitHub repository. A redirect, matching text, or a successful
// Git operation is not identity evidence: both names must return the same
// positive repository ID from authenticated, same-host GitHub API reads, and
// both responses must name the current canonical owner/repository.
func VerifyRepositoryRename(ctx context.Context, cmd CmdFactory, previousTarget, currentTarget string) error {
	previous, err := parseRepositoryLocator(ctx, previousTarget)
	if err != nil {
		return fmt.Errorf("verify previous GitHub repository locator: %w", err)
	}
	current, err := parseRepositoryLocator(ctx, currentTarget)
	if err != nil {
		return fmt.Errorf("verify current GitHub repository locator: %w", err)
	}
	if !strings.EqualFold(previous.host, current.host) {
		return errors.New("repository rename crosses GitHub hosts")
	}
	if strings.EqualFold(previous.slug, current.slug) {
		return errors.New("repository locators do not name different owner/repository paths")
	}
	return verifyRepositorySlugs(ctx, cmd, current.host, previous.slug, current.slug)
}

type repositoryLocator struct {
	host string
	slug string
}

func parseRepositoryLocator(ctx context.Context, raw string) (repositoryLocator, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || strings.IndexFunc(trimmed, func(r rune) bool { return r < ' ' || r == 0x7f }) >= 0 {
		return repositoryLocator{}, errors.New("invalid repository locator")
	}
	if strings.Contains(trimmed, "://") {
		parsed, err := url.Parse(trimmed)
		if err != nil || parsed.Scheme == "" || parsed.Hostname() == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
			return repositoryLocator{}, errors.New("invalid repository locator")
		}
		switch strings.ToLower(parsed.Scheme) {
		case "http", "https":
			if parsed.User != nil {
				return repositoryLocator{}, errors.New("credential-bearing repository locator")
			}
		case "ssh", "git":
			if parsed.User != nil {
				if _, password := parsed.User.Password(); password {
					return repositoryLocator{}, errors.New("credential-bearing repository locator")
				}
			}
		default:
			return repositoryLocator{}, errors.New("unsupported repository locator scheme")
		}
	}
	host := strings.ToLower(strings.TrimSpace(scm.ResolveHost(ctx, trimmed)))
	path := strings.Trim(scm.RepoPath(trimmed), "/")
	parts := strings.Split(path, "/")
	if host == "" || len(parts) != 2 {
		return repositoryLocator{}, errors.New("expected an exact GitHub owner/repository locator")
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.TrimSpace(part) != part {
			return repositoryLocator{}, errors.New("expected an unambiguous GitHub owner/repository locator")
		}
	}
	return repositoryLocator{host: host, slug: parts[0] + "/" + parts[1]}, nil
}

type repositoryIdentity struct {
	ID       int64  `json:"id"`
	FullName string `json:"full_name"`
}

// verifyRepositorySlugs is the single authenticated repository-identity owner
// shared by PR discovery and explicit persisted-target migration.
func verifyRepositorySlugs(ctx context.Context, cmd CmdFactory, host, previousSlug, currentSlug string) error {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" || cmd == nil {
		return errors.New("GitHub repository identity verification is unavailable")
	}
	previous, err := readRepositoryIdentity(ctx, cmd, host, previousSlug)
	if err != nil {
		return err
	}
	current, err := readRepositoryIdentity(ctx, cmd, host, currentSlug)
	if err != nil {
		return err
	}
	if previous.ID != current.ID || !strings.EqualFold(previous.FullName, currentSlug) || !strings.EqualFold(current.FullName, currentSlug) {
		return errors.New("repository names do not match one verified GitHub repository identity")
	}
	return nil
}

func readRepositoryIdentity(ctx context.Context, cmd CmdFactory, host, slug string) (repositoryIdentity, error) {
	parts := strings.Split(strings.Trim(strings.TrimSpace(slug), "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || parts[0] == "." || parts[0] == ".." || parts[1] == "." || parts[1] == ".." {
		return repositoryIdentity{}, errors.New("invalid GitHub repository identity selector")
	}
	endpoint := "repos/" + url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1])
	out, err := cmd(ctx, "gh", "api", "--hostname", host, endpoint).Output()
	if err != nil {
		// Provider output is deliberately omitted: authentication errors can
		// contain private response data or credential-adjacent diagnostics.
		return repositoryIdentity{}, errors.New("authenticated GitHub repository identity read failed")
	}
	identity, err := decodeRepositoryIdentity(out)
	if err != nil {
		return repositoryIdentity{}, errors.New("authenticated GitHub repository identity response was malformed")
	}
	if identity.ID <= 0 || strings.TrimSpace(identity.FullName) == "" {
		return repositoryIdentity{}, errors.New("authenticated GitHub repository identity response was incomplete")
	}
	return identity, nil
}

func decodeRepositoryIdentity(data []byte) (repositoryIdentity, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return repositoryIdentity{}, errors.New("expected object")
	}
	var identity repositoryIdentity
	seenID, seenFullName := false, false
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return repositoryIdentity{}, err
		}
		name, ok := key.(string)
		if !ok {
			return repositoryIdentity{}, errors.New("expected object key")
		}
		switch name {
		case "id":
			if seenID {
				return repositoryIdentity{}, errors.New("duplicate id")
			}
			seenID = true
			if err := decoder.Decode(&identity.ID); err != nil {
				return repositoryIdentity{}, err
			}
		case "full_name":
			if seenFullName {
				return repositoryIdentity{}, errors.New("duplicate full_name")
			}
			seenFullName = true
			if err := decoder.Decode(&identity.FullName); err != nil {
				return repositoryIdentity{}, err
			}
		default:
			var ignored json.RawMessage
			if err := decoder.Decode(&ignored); err != nil {
				return repositoryIdentity{}, err
			}
		}
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') || !seenID || !seenFullName {
		return repositoryIdentity{}, errors.New("incomplete object")
	}
	if err := ensureJSONEnd(decoder); err != nil {
		return repositoryIdentity{}, err
	}
	return identity, nil
}

func ensureJSONEnd(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}
