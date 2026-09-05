package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestPipeline_CIRepairRefreshesPublishedAttestationBeforeReadiness exercises
// the complete late-repair transition through the step and executor interfaces:
// publish the initial PR content, commit a CI repair, revalidate it, push the
// repaired head, republish the PR, then let CI inspect the raw body.
//
// The initiating transition and the masking condition are deliberately
// separate. The successful case proves the current repair-publication route.
// The failed-refresh case reproduces the remaining mask before CI starts:
// PRStep used to downgrade an UpdatePR error to a warning, so CI ran against
// the pushed head while the body still carried the prior attestation. The
// simulated required check then reports the same user-visible head mismatch as
// the live incident.
func TestPipeline_CIRepairRefreshesPublishedAttestationBeforeReadiness(t *testing.T) {
	for _, tc := range []struct {
		name       string
		failUpdate bool
	}{
		{name: "current head is republished"},
		{name: "failed refresh cannot reach CI", failUpdate: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, baseSHA, initialHead := setupGitRepo(t)
			fork := t.TempDir()
			gitCmd(t, fork, "init", "--bare", ".")
			gitCmd(t, dir, "remote", "add", "origin", fork)

			ag := &mockAgent{
				name: "test",
				runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
					return &agent.Result{Output: json.RawMessage(`{"title":"fix(pipeline): refresh repaired attestations","body":"## What Changed\n\n- refresh the attestation after CI repair"}`)}, nil
				},
			}
			sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, initialHead, config.Commands{})

			prURL := "https://github.com/test/repo/pull/42"
			env, _ := fakeGH(t, prURL)
			bodyFile := filepath.Join(t.TempDir(), "pr-body.md")
			if err := os.WriteFile(bodyFile, []byte(compliantPipelineBody(t, baseSHA)), 0o644); err != nil {
				t.Fatal(err)
			}
			env = append(env,
				"FAKE_CLI_PR_BODY_FILE="+bodyFile,
				"FAKE_CLI_PR_TITLE=fix: stale title",
			)
			if tc.failUpdate {
				env = append(env, "FAKE_CLI_PR_EDIT_ERR=simulated PR update failure")
			}
			for key, value := range environmentEntries(t, env) {
				t.Setenv(key, value)
			}

			ci := &attestationTransitionCI{
				t:           t,
				bodyFile:    bodyFile,
				remote:      fork,
				initialHead: initialHead,
			}
			steps := []pipeline.Step{
				&transitionStep{name: types.StepReview, execute: func(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
					return &pipeline.StepOutcome{
						Findings:              `{"findings":[],"summary":"clean","risk_level":"low","risk_rationale":"attestation transition only"}`,
						ReviewApprovedHeadSHA: sctx.Run.HeadSHA,
					}, nil
				}},
				&transitionStep{name: types.StepTest, execute: func(*pipeline.StepContext) (*pipeline.StepOutcome, error) {
					return &pipeline.StepOutcome{Findings: `{"findings":[],"summary":"clean","testing_summary":"transition exercised","tested":["pipeline interfaces"]}`}, nil
				}},
				&PushStep{},
				&PRStep{},
				ci,
			}

			appPaths := paths.WithRoot(t.TempDir())
			if err := appPaths.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			exec := pipeline.NewExecutor(sctx.DB, appPaths, sctx.Config, ag, steps, nil)
			exec.SetForgeContext(&forgecontext.Context{
				Provider: scm.ProviderGitHub,
				Host:     "github.com",
			})

			err := exec.Execute(context.Background(), sctx.Run, sctx.Repo, dir)
			if tc.failUpdate {
				if err == nil || !strings.Contains(err.Error(), "step pr failed") || !strings.Contains(err.Error(), "simulated PR update failure") {
					t.Fatalf("pipeline error = %v, want the PR refresh failure", err)
				}
				if ci.calls != 0 {
					t.Fatalf("CI executions = %d, want 0; stale content reached the visible required-check boundary", ci.calls)
				}
				body, readErr := os.ReadFile(bodyFile)
				if readErr != nil {
					t.Fatal(readErr)
				}
				if got, parseErr := attestedHead(string(body)); parseErr != nil || got != baseSHA {
					t.Fatalf("body after refused refresh attests %q (%v), want unchanged prior head %s", got, parseErr, baseSHA)
				}
				return
			}

			if err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			if ci.calls != 1 {
				t.Fatalf("CI executions = %d, want one repair and final readiness observation", ci.calls)
			}
			body, err := os.ReadFile(bodyFile)
			if err != nil {
				t.Fatal(err)
			}
			assertFirstAttestationBindsHead(t, string(body), ci.finalHead)
			if remoteHead := gitCmd(t, fork, "rev-parse", "refs/heads/feature"); remoteHead != ci.finalHead {
				t.Fatalf("remote head = %s, want repaired head %s", remoteHead, ci.finalHead)
			}
		})
	}
}

type transitionStep struct {
	name    types.StepName
	execute func(*pipeline.StepContext) (*pipeline.StepOutcome, error)
}

func (s *transitionStep) Name() types.StepName { return s.name }

func (s *transitionStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	return s.execute(sctx)
}

type attestationTransitionCI struct {
	t           *testing.T
	bodyFile    string
	remote      string
	initialHead string
	finalHead   string
	calls       int
}

func (s *attestationTransitionCI) Name() types.StepName { return types.StepCI }

func (s *attestationTransitionCI) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	s.calls++
	body, err := os.ReadFile(s.bodyFile)
	if err != nil {
		return nil, fmt.Errorf("read published PR body: %w", err)
	}
	attested, err := attestedHead(string(body))
	if err != nil {
		return nil, err
	}
	remoteHead := gitCmd(s.t, s.remote, "rev-parse", "refs/heads/feature")
	if attested != remoteHead {
		return nil, fmt.Errorf("required check failure: PR body attests %s while the PR head is %s", attested, remoteHead)
	}

	if attested != s.initialHead {
		return nil, fmt.Errorf("initial PR body attests %s, want %s", attested, s.initialHead)
	}
	if err := os.WriteFile(filepath.Join(sctx.WorkDir, "ci-repair.txt"), []byte("repaired\n"), 0o644); err != nil {
		return nil, err
	}
	repair, err := (&CIStep{}).commitRepair(sctx, "repair required check")
	if err != nil {
		return nil, err
	}
	if !repair.HeadAdvanced || repair.Revalidate {
		return nil, fmt.Errorf("CI repair result = %+v, want an immediately published repair", repair)
	}
	s.finalHead = sctx.Run.HeadSHA
	body, err = os.ReadFile(s.bodyFile)
	if err != nil {
		return nil, fmt.Errorf("read repaired PR body: %w", err)
	}
	attested, err = attestedHead(string(body))
	if err != nil {
		return nil, err
	}
	remoteHead = gitCmd(s.t, s.remote, "rev-parse", "refs/heads/feature")
	if attested != remoteHead || remoteHead != s.finalHead {
		return nil, fmt.Errorf("required check failure after repair: body=%s remote=%s run=%s", attested, remoteHead, s.finalHead)
	}
	return &pipeline.StepOutcome{}, nil
}

func attestedHead(body string) (string, error) {
	if count := strings.Count(body, pipelineAttestationCommentPrefix); count != 1 {
		return "", fmt.Errorf("PR body has %d live attestation markers, want 1", count)
	}
	start := strings.Index(body, pipelineAttestationCommentPrefix) + len(pipelineAttestationCommentPrefix)
	end := strings.Index(body[start:], pipelineAttestationCommentClosingToken)
	if end < 0 {
		return "", fmt.Errorf("PR body attestation is not closed")
	}
	var attestation pipelineAttestation
	if err := json.Unmarshal([]byte(body[start:start+end]), &attestation); err != nil {
		return "", fmt.Errorf("parse PR body attestation: %w", err)
	}
	return attestation.HeadSHA, nil
}

func environmentEntries(t *testing.T, entries []string) map[string]string {
	t.Helper()
	result := make(map[string]string, len(entries))
	for _, entry := range entries {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			t.Fatalf("invalid environment entry %q", entry)
		}
		result[key] = value
	}
	return result
}
