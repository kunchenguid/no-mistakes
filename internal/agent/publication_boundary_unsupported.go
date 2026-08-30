//go:build !linux

package agent

import (
	"context"
	"fmt"
	"time"
)

func probePublicationCodexBoundary(_ context.Context, _ *PublicationCodexBoundaryV1, _ PublicationCodexProbeOptions) error {
	return fmt.Errorf("%w: publication Codex confinement is supported only on Linux", ErrPublicationConfinementUnavailable)
}

func DetachPublicationConfinementCanary() error {
	return fmt.Errorf("%w: publication lifecycle canary is Linux-only", ErrPublicationConfinementUnavailable)
}

func RunPublicationConfinementCanary(PublicationConfinementCanaryConfig) error {
	return fmt.Errorf("%w: publication lifecycle canary is Linux-only", ErrPublicationConfinementUnavailable)
}

func RunPublicationConfinementDetachedChild(string, string, time.Duration) error {
	return fmt.Errorf("%w: publication lifecycle canary is Linux-only", ErrPublicationConfinementUnavailable)
}

func DiscoverProductionPublicationCodexBoundary(context.Context, string) (*PublicationCodexBoundaryV1, error) {
	return nil, fmt.Errorf("%w: production publication confinement is Linux-only", ErrPublicationConfinementUnavailable)
}

func (*publicationPreparedCommand) armLifecycleBarrier() error {
	return fmt.Errorf("%w: protected launch lifecycle is Linux-only", ErrPublicationConfinementUnavailable)
}

func (*publicationPreparedCommand) abortLifecycleBarrier() {}

func (*publicationPreparedCommand) bindAndReleaseLifecycle(string) (*publicationLaunchWitness, error) {
	return nil, fmt.Errorf("%w: protected launch lifecycle is Linux-only", ErrPublicationConfinementUnavailable)
}

func verifyPublicationLaunchTeardown(*publicationLaunchWitness) error {
	return fmt.Errorf("%w: protected launch lifecycle is Linux-only", ErrPublicationConfinementUnavailable)
}
