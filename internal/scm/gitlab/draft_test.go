package gitlab

import (
	"context"
	"errors"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// Draft MR support is deliberately out of scope until the glab flag surface is
// verified against the pinned version; the capability must therefore stay off
// and MarkPRReady must fail as unsupported rather than shell out blindly.
func TestDraftIsUnsupported(t *testing.T) {
	t.Parallel()

	host := New(nil, nil, "gitlab.com", "group/project")
	if host.Capabilities().Draft {
		t.Fatal("GitLab must not declare draft support until the glab flags are verified")
	}
	if err := host.MarkPRReady(context.Background(), &scm.PR{Number: "42"}); !errors.Is(err, scm.ErrUnsupported) {
		t.Fatalf("MarkPRReady() error = %v, want ErrUnsupported", err)
	}
}
