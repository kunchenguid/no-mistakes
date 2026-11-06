package git

import (
	"context"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/runenv"
)

// CommitIdentity is the submitting checkout's effective identity, resolved by
// Git before the daemon can substitute its own environment or configuration.
// Dates are deliberately not captured: each new commit gets its creation time.
type CommitIdentity struct {
	AuthorName     string `json:"author_name"`
	AuthorEmail    string `json:"author_email"`
	CommitterName  string `json:"committer_name"`
	CommitterEmail string `json:"committer_email"`
}

func (i CommitIdentity) Validate() error {
	for _, value := range []string{i.AuthorName, i.AuthorEmail, i.CommitterName, i.CommitterEmail} {
		if strings.TrimSpace(value) == "" || strings.ContainsAny(value, "\x00\r\n<>") {
			return fmt.Errorf("submitting checkout has an incomplete or invalid commit identity")
		}
	}
	return nil
}

// CaptureCommitIdentity honors config includes, worktree config, and explicit
// author/committer environment overrides, but refuses Git's guessed identity.
func CaptureCommitIdentity(ctx context.Context, dir string) (CommitIdentity, error) {
	var identity CommitIdentity
	for _, role := range []struct {
		variable    string
		name, email *string
	}{
		{"GIT_AUTHOR_IDENT", &identity.AuthorName, &identity.AuthorEmail},
		{"GIT_COMMITTER_IDENT", &identity.CommitterName, &identity.CommitterEmail},
	} {
		out, err := Run(ctx, dir, "-c", "user.useConfigOnly=true", "var", role.variable)
		if err != nil {
			return identity, fmt.Errorf("resolve submitting checkout %s: %w", role.variable, err)
		}
		start, end := strings.LastIndex(out, " <"), strings.LastIndex(out, "> ")
		if start <= 0 || end <= start+2 {
			return identity, fmt.Errorf("cannot parse submitting checkout %s", role.variable)
		}
		*role.name, *role.email = out[:start], out[start+2:end]
	}
	return identity, identity.Validate()
}

func (i CommitIdentity) Environment() runenv.Overlay {
	return runenv.Overlay{Set: map[string]string{
		"GIT_AUTHOR_NAME": i.AuthorName, "GIT_AUTHOR_EMAIL": i.AuthorEmail,
		"GIT_COMMITTER_NAME": i.CommitterName, "GIT_COMMITTER_EMAIL": i.CommitterEmail,
	}, Unset: []string{"GIT_AUTHOR_DATE", "GIT_COMMITTER_DATE"}}
}

type commitIdentityContextKey struct{}

// WithCommitIdentity freezes a value, never a pointer owned by an IPC caller.
func WithCommitIdentity(ctx context.Context, identity CommitIdentity) context.Context {
	return context.WithValue(ctx, commitIdentityContextKey{}, identity)
}

func CommitIdentityFromContext(ctx context.Context) (CommitIdentity, bool) {
	if ctx == nil {
		return CommitIdentity{}, false
	}
	identity, ok := ctx.Value(commitIdentityContextKey{}).(CommitIdentity)
	return identity, ok
}

func ApplyCommitIdentity(ctx context.Context, base []string) []string {
	if identity, ok := CommitIdentityFromContext(ctx); ok {
		return identity.Environment().Apply(base)
	}
	return base
}
