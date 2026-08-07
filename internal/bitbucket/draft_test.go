package bitbucket

import (
	"context"
	"errors"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// Bitbucket Cloud has no draft pull requests at all, so the capability can
// never be turned on here.
func TestDraftIsUnsupported(t *testing.T) {
	t.Parallel()

	host := NewHost(nil, RepoRef{})
	if host.Capabilities().Draft {
		t.Fatal("Bitbucket Cloud has no draft pull requests")
	}
	if err := host.MarkPRReady(context.Background(), &scm.PR{Number: "42"}); !errors.Is(err, scm.ErrUnsupported) {
		t.Fatalf("MarkPRReady() error = %v, want ErrUnsupported", err)
	}
}
