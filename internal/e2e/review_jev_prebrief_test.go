//go:build e2e

package e2e

import (
	"bufio"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// jevPrebriefHeading is the first line of the section the Jev pre-brief adds
// to a review prompt. Its absence means the reviewer got the prompt it gets
// with the assist off.
const jevPrebriefHeading = "Pre-brief (advisory output of a fast pre-screen model"

// jevEndpointHostPort is where the pinned TypeSafe endpoint lives. A CONNECT
// for it through HTTPS_PROXY is the daemon attempting a Jev evaluation.
const jevEndpointHostPort = "api.typesafe.ai:443"

// connectRecorder is an HTTPS proxy that never forwards anything: it records
// the CONNECT target of every tunnel the daemon asks for and refuses it. With
// HTTPS_PROXY pointing here, a journey can observe whether the daemon tried to
// reach TypeSafe, and no change content ever leaves the machine.
type connectRecorder struct {
	mu      sync.Mutex
	targets []string
}

// startConnectRecorder listens on loopback and routes the daemon's HTTPS
// traffic to it. It must run before `nm init`, which starts the daemon, so the
// daemon inherits the proxy setting.
func startConnectRecorder(t *testing.T) *connectRecorder {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	r := &connectRecorder{}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
				line, err := bufio.NewReader(conn).ReadString('\n')
				if err != nil {
					return
				}
				if fields := strings.Fields(line); len(fields) >= 2 && fields[0] == "CONNECT" {
					r.mu.Lock()
					r.targets = append(r.targets, fields[1])
					r.mu.Unlock()
				}
				_, _ = conn.Write([]byte("HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n"))
			}()
		}
	}()
	t.Setenv("HTTPS_PROXY", "http://"+ln.Addr().String())
	t.Setenv("https_proxy", "http://"+ln.Addr().String())
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	return r
}

// jevAttempts counts tunnels requested to the TypeSafe endpoint.
func (r *connectRecorder) jevAttempts() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, target := range r.targets {
		if target == jevEndpointHostPort {
			n++
		}
	}
	return n
}

const jevReviewAssistOn = "jev:\n  review_assist: true\n"

// seedWidgetPackage commits an unchanged use site of RenderWidget to main and
// publishes it, so a branch changing RenderWidget has a surrounding-context
// candidate and the pre-brief reaches the point of calling TypeSafe.
func seedWidgetPackage(t *testing.T, h *Harness) {
	t.Helper()
	h.CommitChange("main", "widget/render.go", "package widget\n\nfunc RenderWidget() string { return \"w\" }\n", "add widget renderer")
	h.CommitChange("main", "widget/user.go", "package widget\n\nfunc Header() string { return RenderWidget() + \"!\" }\n", "add widget user")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if out, err := h.runGit(ctx, h.WorkDir, "push", "origin", "main"); err != nil {
		t.Fatalf("push origin main: %v\n%s", err, out)
	}
}

const changedRenderWidget = "package widget\n\nfunc RenderWidget() string { return \"widget\" }\n"

// reviewStepLog returns the operator-visible review step log of a run.
func reviewStepLog(t *testing.T, h *Harness, runID string) string {
	t.Helper()
	logs, err := h.Run("axi", "logs", "--run", runID, "--step", "review", "--full")
	if err != nil {
		t.Fatalf("axi logs: %v\n%s", err, logs)
	}
	return logs
}

// mentionsJevAssist reports whether a step log carries any of the pre-brief's
// own log lines, which the review step writes only with the assist enabled.
func mentionsJevAssist(logs string) bool {
	return strings.Contains(logs, "jev review assist") || strings.Contains(logs, "jev pre-brief")
}

// runWidgetChange pushes a RenderWidget change through the gate and waits for
// the run to complete, returning it.
func runWidgetChange(t *testing.T, h *Harness, branch string) string {
	t.Helper()
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}
	h.CommitChange(branch, "widget/render.go", changedRenderWidget, "change widget renderer")
	h.PushToGate(branch)
	run := h.WaitForRun(branch, 120*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("run did not complete: status=%s error=%v", run.Status, deref(run.Error))
	}
	return run.ID
}

// TestJevReviewAssistJourney drives the real binary to prove the opt-in Jev
// review pre-brief (jev.review_assist, issue #1055) keeps every guarantee the
// review has without it: the unconfigured default never contacts TypeSafe and
// reviews exactly as before, a pushed branch cannot opt its own review in, and
// an opted-in review whose TypeSafe call cannot succeed still runs the same
// complete review with its prompt untouched.
//
// TypeSafe is never reached: HTTPS_PROXY routes the daemon to a local recorder
// that refuses every tunnel, so the journey observes whether a Jev evaluation
// was attempted without sending anything off the machine.
func TestJevReviewAssistJourney(t *testing.T) {
	t.Run("unconfigured_default_never_contacts_typesafe_even_with_a_key", func(t *testing.T) {
		h := NewHarness(t, SetupOpts{Agent: "claude"})
		t.Setenv("TYPESAFE_API_KEY", "nm-e2e-unused-key")
		proxy := startConnectRecorder(t)
		seedWidgetPackage(t, h)

		runID := runWidgetChange(t, h, "assist-unconfigured")

		if n := proxy.jevAttempts(); n != 0 {
			t.Errorf("unconfigured daemon attempted %d TypeSafe evaluation(s); the assist must be opt-in", n)
		}
		prompt := reviewPrompt(t, h)
		if strings.Contains(prompt, jevPrebriefHeading) {
			t.Errorf("unconfigured review prompt carries a pre-brief:\n%s", promptTail(prompt))
		}
		if logs := reviewStepLog(t, h, runID); mentionsJevAssist(logs) {
			t.Errorf("unconfigured review step log records a jev pre-brief:\n%s", logs)
		}
	})

	t.Run("pushed_repo_config_cannot_opt_its_own_review_in", func(t *testing.T) {
		h := NewHarness(t, SetupOpts{Agent: "claude"})
		t.Setenv("TYPESAFE_API_KEY", "nm-e2e-unused-key")
		proxy := startConnectRecorder(t)
		seedWidgetPackage(t, h)
		// Both the trusted default-branch copy and the pushed branch ask for
		// the assist. jev is global-only, so neither may turn it on.
		repoOptIn := "ignore_patterns:\n  - '*.generated.go'\n  - 'vendor/**'\nallow_repo_commands: true\n" + jevReviewAssistOn
		pushMainRepoConfig(t, h, repoOptIn)
		h.CommitChange("assist-repo-opt-in", ".no-mistakes.yaml", "# contributor copy\n"+repoOptIn, "contributor: opt into the assist")

		runID := runWidgetChange(t, h, "assist-repo-opt-in")

		if n := proxy.jevAttempts(); n != 0 {
			t.Errorf("SECURITY REGRESSION: repository config opted the review into %d TypeSafe evaluation(s)", n)
		}
		if prompt := reviewPrompt(t, h); strings.Contains(prompt, jevPrebriefHeading) {
			t.Errorf("repository-opted review prompt carries a pre-brief:\n%s", promptTail(prompt))
		}
		if logs := reviewStepLog(t, h, runID); mentionsJevAssist(logs) {
			t.Errorf("repository-opted review step log records a jev pre-brief:\n%s", logs)
		}
	})

	t.Run("opt_in_without_a_key_reviews_without_a_prebrief", func(t *testing.T) {
		h := NewHarness(t, SetupOpts{Agent: "claude", GlobalConfigExtra: jevReviewAssistOn})
		t.Setenv("TYPESAFE_API_KEY", "")
		proxy := startConnectRecorder(t)
		seedWidgetPackage(t, h)

		runID := runWidgetChange(t, h, "jev-no-key")

		if n := proxy.jevAttempts(); n != 0 {
			t.Errorf("keyless daemon attempted %d TypeSafe evaluation(s)", n)
		}
		logs := reviewStepLog(t, h, runID)
		const want = "jev review assist is enabled but TYPESAFE_API_KEY is not set; reviewing without a pre-brief"
		if !strings.Contains(logs, want) {
			t.Errorf("review step log is missing %q\n%s", want, logs)
		}
		if prompt := reviewPrompt(t, h); strings.Contains(prompt, jevPrebriefHeading) {
			t.Errorf("keyless review prompt carries a pre-brief:\n%s", promptTail(prompt))
		}
		t.Logf("review step log:\n%s", logs)
	})

	t.Run("opt_in_with_unreachable_typesafe_reviews_without_a_prebrief", func(t *testing.T) {
		h := NewHarness(t, SetupOpts{Agent: "claude", GlobalConfigExtra: jevReviewAssistOn})
		t.Setenv("TYPESAFE_API_KEY", "nm-e2e-unreachable-key")
		proxy := startConnectRecorder(t)
		seedWidgetPackage(t, h)

		runID := runWidgetChange(t, h, "jev-unreachable")

		if n := proxy.jevAttempts(); n != 1 {
			t.Errorf("opted-in review attempted %d TypeSafe evaluation(s), want exactly 1", n)
		}
		logs := reviewStepLog(t, h, runID)
		for _, want := range []string{"jev pre-brief unavailable (", "reviewing without a pre-brief"} {
			if !strings.Contains(logs, want) {
				t.Errorf("review step log is missing %q\n%s", want, logs)
			}
		}
		if strings.Contains(logs, "nm-e2e-unreachable-key") {
			t.Errorf("review step log leaks the TypeSafe key:\n%s", logs)
		}
		if prompt := reviewPrompt(t, h); strings.Contains(prompt, jevPrebriefHeading) {
			t.Errorf("failed-assist review prompt carries a pre-brief:\n%s", promptTail(prompt))
		}
		t.Logf("review step log:\n%s", logs)
	})

	t.Run("opt_in_rereview_consults_jev_again_and_stays_session_free", func(t *testing.T) {
		h := NewHarness(t, SetupOpts{
			Agent:             "claude",
			Scenario:          writeJevRereviewScenario(t),
			GlobalConfigExtra: jevReviewAssistOn,
		})
		t.Setenv("TYPESAFE_API_KEY", "nm-e2e-unreachable-key")
		proxy := startConnectRecorder(t)
		seedWidgetPackage(t, h)
		if out, err := h.Run("init"); err != nil {
			t.Fatalf("nm init: %v\n%s", err, out)
		}

		const branch = "jev-rereview"
		h.CommitChange(branch, "widget/render.go", changedRenderWidget, "change widget renderer")
		h.PushToGate(branch)
		gated := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusAwaitingApproval, 90*time.Second)
		if gated == nil {
			t.Fatal("run did not park at the review gate")
		}
		h.RespondWithFindings(gated.ID, types.StepReview, types.ActionFix, []string{"jev-widget-check"})
		run := h.WaitForRun(branch, 120*time.Second)
		if run.Status != types.RunCompleted {
			t.Fatalf("run did not complete: status=%s error=%v", run.Status, deref(run.Error))
		}

		invs := h.AgentInvocations()
		initial := firstInvocationMatching(invs, func(prompt string) bool {
			return strings.Contains(prompt, reviewTurnMarker) &&
				!strings.Contains(prompt, rereviewTurnMarker) &&
				!strings.Contains(prompt, fixTurnMarker)
		})
		rereview := firstInvocationMatching(invs, func(prompt string) bool {
			return strings.Contains(prompt, reviewTurnMarker) &&
				strings.Contains(prompt, rereviewTurnMarker) &&
				!strings.Contains(prompt, fixTurnMarker)
		})
		if initial == nil || rereview == nil {
			t.Fatalf("missing a review turn: initial=%v rereview=%v", initial != nil, rereview != nil)
		}
		// Each review turn, the rereview included, runs its own evaluation.
		if n := proxy.jevAttempts(); n != 2 {
			t.Errorf("review loop attempted %d TypeSafe evaluation(s), want 2 (initial review and rereview)", n)
		}
		// The rereview is a fresh, session-free turn that still reviews the
		// complete change: the changed file is in its prompt's scope.
		assertNoResume(t, "initial review", initial.Args)
		assertNoResume(t, "post-fix rereview", rereview.Args)
		if !strings.Contains(rereview.Prompt, "widget/render.go") {
			t.Errorf("rereview prompt does not name the changed file:\n%s", promptTail(rereview.Prompt))
		}
		logs := reviewStepLog(t, h, run.ID)
		if got := strings.Count(logs, "jev pre-brief unavailable ("); got != 2 {
			t.Errorf("review step log records %d failed pre-brief(s), want 2\n%s", got, logs)
		}
		t.Logf("review step log:\n%s", logs)
	})
}

// writeJevRereviewScenario scripts one review finding, a fix, and a clean
// rereview on widget/render.go, so the review loop runs a rereview.
func writeJevRereviewScenario(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "jev-rereview-scenario.yaml")
	content := `actions:
  - match: "` + fixTurnMarker + `"
    text: "addressed the review finding"
    edits:
      - path: "widget/render.go"
        old: "return \"widget\""
        new: "return \"widget-fixed\""
    stage: ["widget/render.go"]
    structured:
      summary: "resolve review finding"
  - match: "` + rereviewTurnMarker + `"
    text: "clean after fix"
    structured:
      findings: []
      summary: "clean after fix"
      risk_level: low
      risk_rationale: "issue resolved by the fixer"
      risk_scope: source-or-external
      reviewed_paths:
        - "widget/render.go"
  - match: "` + reviewTurnMarker + `"
    text: "found one blocking issue"
    structured:
      findings:
        - id: "jev-widget-check"
          severity: warning
          description: "mechanical issue routed through the fixer"
          file: "widget/render.go"
          action: auto-fix
          review_scope: source
      summary: "one blocking issue"
      risk_level: low
      risk_rationale: "single mechanical issue"
      risk_scope: source-or-external
  - text: "no issues found"
    structured:
      findings: []
      summary: "no issues found"
      risk_level: low
      risk_rationale: "no risks detected in the diff"
      risk_scope: source-or-external
      tested:
        - "fakeagent: simulated test run"
      testing_summary: "simulated tests passed"
      artifacts: []
      scenarios:
        - name: "fakeagent: simulated end-to-end scenario"
          result: pass
          live: true
          evidence: "fakeagent: simulated test run"
          reason: ""
      verdict: go
      title: "feat: jev rereview"
      body: "## Summary\njev rereview e2e"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write scenario: %v", err)
	}
	return path
}
