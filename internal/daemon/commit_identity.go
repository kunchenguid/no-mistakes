package daemon

import (
	"context"
	"fmt"

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
	// --worktree falls back to --local when worktreeConfig is disabled.
	var identity git.CommitIdentity
	for _, field := range []struct {
		key   string
		value *string
	}{{"user.name", &identity.AuthorName}, {"user.email", &identity.AuthorEmail}} {
		for _, scope := range []string{"--local", "--worktree"} {
			value, err := git.Run(ctx, checkout, "config", scope, "--includes", "--get", "--default", "", field.key)
			if err != nil {
				return identity, err
			}
			if value != "" {
				*field.value = value
			}
		}
	}
	identity.CommitterName, identity.CommitterEmail = identity.AuthorName, identity.AuthorEmail
	if err := identity.Validate(); err != nil {
		return identity, fmt.Errorf("cannot prove submitting checkout identity without a caller snapshot; submit using the current no-mistakes binary: %w", err)
	}
	return identity, nil
}

func pipelineEnvironment(forge *forgecontext.Context, identity git.CommitIdentity) runenv.Overlay {
	environment := forgeEnvironment(forge).Clone()
	if environment.Set == nil {
		environment.Set = make(map[string]string)
	}
	bound := identity.Environment()
	for key, value := range bound.Set {
		environment.Set[key] = value
	}
	environment.Unset = append(environment.Unset, bound.Unset...)
	return environment
}
