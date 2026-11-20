package daemon

import "context"

type operatorSkipReasonKey struct{}

// withOperatorSkipReason carries the reason an operator gave for a run's
// explicit skips (axi run --skip-reason) through admission to the executor,
// the way the base remote and caller restrictions travel. Only the request
// context carries it; the executor records it on every step the run skips.
func withOperatorSkipReason(ctx context.Context, reason string) context.Context {
	return context.WithValue(ctx, operatorSkipReasonKey{}, reason)
}

func operatorSkipReason(ctx context.Context) string {
	reason, _ := ctx.Value(operatorSkipReasonKey{}).(string)
	return reason
}
