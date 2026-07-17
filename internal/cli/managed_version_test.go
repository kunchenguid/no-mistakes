package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/authorization"
)

func TestVersionExposesBuildAndAuthorizationProtocol(t *testing.T) {
	version := newRootCmd().Version
	if !strings.Contains(version, "authorization-protocol="+authorization.ProtocolVersion) {
		t.Fatalf("version %q does not expose protocol %s", version, authorization.ProtocolVersion)
	}
}

func TestManagedUpdateRefusesSelfMutation(t *testing.T) {
	t.Setenv("NO_MISTAKES_AUTHORIZATION_MODE", "managed")
	cmd := newUpdateCmd()
	cmd.SetContext(context.Background())
	err := cmd.RunE(cmd, nil)
	if err == nil || !strings.Contains(err.Error(), "managed runtime") {
		t.Fatalf("managed update error = %v", err)
	}
}
