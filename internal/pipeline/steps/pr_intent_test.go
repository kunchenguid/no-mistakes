package steps

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestPRPublishIntentSuppressionCoversDefaultAgentAndFallback(t *testing.T) {
	t.Parallel()
	no, yes := false, true
	for _, tc := range []struct {
		name    string
		publish *bool
		want    bool
	}{
		{"default", nil, true},
		{"enabled", &yes, true},
		{"disabled", &no, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, fallback := range []bool{false, true} {
				dir, base, head := setupGitRepo(t)
				ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
					if fallback {
						return nil, errors.New("draft unavailable")
					}
					return &agent.Result{Output: json.RawMessage(`{"title":"feat: helper","body":"## What Changed\n\n- A helper."}`)}, nil
				}}
				sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
				sctx.Config.PR.PublishIntent = tc.publish
				sctx.UserIntent = "Entire original intent remains reviewer input."
				for _, limit := range []int{0, 4000} {
					got, err := (&PRStep{}).buildPRContent(sctx, "feature", "main", base, scm.ProviderGitHub, limit)
					if err != nil || strings.Contains(got.Body, "## Intent") != tc.want || !strings.Contains(got.Body, "## What Changed") {
						t.Fatalf("fallback=%v limit=%d: %+v, %v", fallback, limit, got, err)
					}
					if tc.want && !strings.Contains(got.Body, "## Intent\n\n"+sctx.UserIntent) {
						t.Fatalf("default/enabled publication lost intent: %s", got.Body)
					}
					if !strings.Contains(ag.calls[len(ag.calls)-1].Prompt, sctx.UserIntent) {
						t.Fatal("suppression removed full intent from model context")
					}
				}
			}
		})
	}
}
