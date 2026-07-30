package azuredevops

import (
	"context"
	"errors"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// Draft PR support is deliberately out of scope until the az flag surface is
// verified; the capability stays off and MarkPRReady fails as unsupported.
func TestDraftIsUnsupported(t *testing.T) {
	t.Parallel()

	host := New(nil, nil, "org", "project", "repo")
	if host.Capabilities().Draft {
		t.Fatal("Azure DevOps must not declare draft support until the az flags are verified")
	}
	if err := host.MarkPRReady(context.Background(), &scm.PR{Number: "42"}); !errors.Is(err, scm.ErrUnsupported) {
		t.Fatalf("MarkPRReady() error = %v, want ErrUnsupported", err)
	}
}
