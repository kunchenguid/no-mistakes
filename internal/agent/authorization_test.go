package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type authorizationProbeAgent struct {
	attempts int
}

func (a *authorizationProbeAgent) Name() string { return "probe" }
func (a *authorizationProbeAgent) Close() error { return nil }
func (a *authorizationProbeAgent) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	for i := 0; i < 2; i++ {
		if err := authorizeLaunch(ctx, opts); err != nil {
			return nil, err
		}
		a.attempts++
	}
	return &Result{}, nil
}

func TestWithLaunchAuthorizationReauthorizesEveryConcreteAttempt(t *testing.T) {
	probe := &authorizationProbeAgent{}
	authorizations := 0
	agent := WithLaunchAuthorization(probe, func(context.Context) error {
		authorizations++
		return nil
	})
	if _, err := agent.Run(context.Background(), RunOpts{}); err != nil {
		t.Fatal(err)
	}
	if authorizations != 2 || probe.attempts != 2 {
		t.Fatalf("authorizations=%d attempts=%d, want 2 each", authorizations, probe.attempts)
	}
}

func TestLaunchAuthorizationDenialPreventsAttempt(t *testing.T) {
	probe := &authorizationProbeAgent{}
	agent := WithLaunchAuthorization(probe, func(context.Context) error {
		return errors.New("managed authorization denied")
	})
	if _, err := agent.Run(context.Background(), RunOpts{}); err == nil {
		t.Fatal("expected denial")
	}
	if probe.attempts != 0 {
		t.Fatalf("agent attempted %d launches after denial", probe.attempts)
	}
}

func TestGitSafeEnvScrubsManagedCredentialsAndScope(t *testing.T) {
	t.Setenv("PERCH_HOOK_TOKEN", "secret-perch-token")
	t.Setenv("PERCH_TASK_ID", "task-1")
	t.Setenv("NO_MISTAKES_AUTHORIZATION_TOKEN", "secret-generic-token")
	env := gitSafeEnv(t.TempDir())
	joined := strings.Join(env, "\n")
	for _, forbidden := range []string{"secret-perch-token", "secret-generic-token", "PERCH_TASK_ID", "PERCH_HOOK_TOKEN", "NO_MISTAKES_AUTHORIZATION_TOKEN"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("child environment leaked %q: %s", forbidden, joined)
		}
	}
	if !strings.Contains(joined, GateRoleEnvVar+"=1") {
		t.Fatal("gate role marker missing")
	}
}
