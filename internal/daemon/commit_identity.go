package daemon

import (
	"context"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/runenv"
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
		scopes = append(scopes, []string{"--file", path})
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

func pipelineEnvironment(forge *forgecontext.Context, identity git.CommitIdentity) (runenv.Overlay, error) {
	environment := forgeEnvironment(forge).Clone()
	bound, err := identity.SubprocessEnvironment(environment.Apply(nil))
	if err != nil {
		return runenv.Overlay{}, err
	}
	for _, key := range bound.Unset {
		delete(environment.Set, key)
	}
	environment.Unset = append(environment.Unset, bound.Unset...)
	if environment.Set == nil {
		environment.Set = make(map[string]string)
	}
	for key, value := range bound.Set {
		environment.Set[key] = value
	}
	return environment, nil
}
