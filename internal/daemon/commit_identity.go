package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/git"
)

func submittedIdentityContext(ctx context.Context, identity *git.CommitIdentity) (context.Context, error) {
	if identity == nil {
		return ctx, nil
	}
	if err := identity.Validate(); err != nil {
		return ctx, err
	}
	return git.WithCommitIdentity(ctx, *identity), nil
}

func submissionIdentity(ctx context.Context, checkout string) (git.CommitIdentity, error) {
	if identity, ok := git.CommitIdentityFromContext(ctx); ok {
		return identity, identity.Validate()
	}
	// An older caller cannot prove which global config or environment it used.
	// Only a complete checkout-local identity is safe without its snapshot.
	var identity git.CommitIdentity
	scopes := [][]string{{"--local"}}
	isolated, err := git.Run(ctx, checkout, "config", "--local", "--type=bool", "--get", "--default", "false", "extensions.worktreeConfig")
	if err != nil {
		return identity, err
	}
	if isolated == "true" {
		path, err := git.Run(ctx, checkout, "rev-parse", "--git-path", "config.worktree")
		if err != nil {
			return identity, err
		}
		absolute := path
		if !filepath.IsAbs(absolute) {
			absolute = filepath.Join(checkout, absolute)
		}
		if _, err := os.Stat(absolute); err == nil {
			scopes = append(scopes, []string{"--file", path})
		} else if !os.IsNotExist(err) {
			return identity, err
		}
	}
	values := make(map[string]string)
	for _, scope := range scopes {
		args := append([]string{"config"}, scope...)
		out, err := git.RunRaw(ctx, checkout, append(args, "--includes", "--null", "--list")...)
		if err != nil {
			return identity, err
		}
		for _, entry := range strings.Split(string(out), "\x00") {
			key, value, _ := strings.Cut(entry, "\n")
			values[key] = value
		}
	}
	for _, field := range []struct {
		key, fallback string
		value         *string
	}{
		{"author.name", "user.name", &identity.AuthorName},
		{"author.email", "user.email", &identity.AuthorEmail},
		{"committer.name", "user.name", &identity.CommitterName},
		{"committer.email", "user.email", &identity.CommitterEmail},
	} {
		value, exists := values[field.key]
		if !exists {
			value = values[field.fallback]
		}
		*field.value = value
	}
	if err := identity.Validate(); err != nil {
		return identity, fmt.Errorf("cannot prove submitting checkout identity without a caller snapshot; submit using the current no-mistakes binary: %w", err)
	}
	return identity, nil
}
