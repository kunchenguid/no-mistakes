package pipeline

import (
	"context"

	"github.com/kunchenguid/no-mistakes/internal/agent"
)

// fakeSessionAgent is the session-free test agent used by executor tests that
// only need a successful invocation. Durable sessions are not part of this
// build.
type fakeSessionAgent struct{ name string }

func newFakeSessionAgent() *fakeSessionAgent { return &fakeSessionAgent{name: "fake"} }

func (f *fakeSessionAgent) Name() string { return f.name }

func (f *fakeSessionAgent) Run(context.Context, agent.RunOpts) (*agent.Result, error) {
	return &agent.Result{Text: "ok"}, nil
}

func (f *fakeSessionAgent) Close() error { return nil }
