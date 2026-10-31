package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/spf13/cobra"
)

func TestReviewerSelectionLaunchFlags(t *testing.T) {
	for _, cmd := range []*cobra.Command{newAxiRunCmd(), newRerunCmd()} {
		if err := cmd.ParseFlags([]string{"--reviewer", "codex", "--reviewer-model", "review-model", "--reviewer-effort", "high"}); err != nil {
			t.Fatalf("%s does not accept an independent reviewer: %v", cmd.Name(), err)
		}
	}
}

func TestReviewerSelectionFlagsPushOptionsAndStatus(t *testing.T) {
	cmd := newAxiRunCmd()
	if err := cmd.ParseFlags([]string{"--reviewer", "codex", "--reviewer-model", "reviewer", "--reviewer-effort", "high"}); err != nil {
		t.Fatal(err)
	}
	pin, err := reviewerFromFlags(cmd)
	if err != nil {
		t.Fatal(err)
	}
	options := formatReviewerPushOptions(pin)
	decoded, err := parseReviewerPushOptions(options)
	if err != nil || !reflect.DeepEqual(decoded, pin) {
		t.Fatalf("round trip: %+v %v", decoded, err)
	}
	if _, err := parseReviewerPushOptions(append(options, options...)); err == nil {
		t.Fatal("duplicate push option accepted")
	}
	database, err := db.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	repo, err := database.InsertRepo(t.TempDir(), "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRunWithIntentAndLaunchNonce(repo.ID, "feature", "h", "b", nil, "", "", "", "", false, nil, pin)
	if err != nil {
		t.Fatal(err)
	}
	for _, view := range []runView{runViewFromDB(run, nil, database), runViewFromIPC(&ipc.RunInfo{ID: run.ID, ReviewerProfile: pin})} {
		var out bytes.Buffer
		render := &cobra.Command{}
		render.SetOut(&out)
		emitDoc(render, runObjectField(view))
		for _, want := range []string{"reviewer_profile:", "agent: codex", "model: reviewer", "effort: high"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("status missing %q: %s", want, out.String())
			}
		}
	}
	var out bytes.Buffer
	if err := renderRunAgentPerf(&out, database, run.ID); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "pinned reviewer: agent=codex model=reviewer effort=high") {
		t.Fatalf("stats lost reviewer: %s", out.String())
	}
	out.Reset()
	render := &cobra.Command{}
	render.SetOut(&out)
	emitLaunchReceipt(render, ipc.LaunchReceipt{PiProfile: &agentcfg.PiProfile{Model: "openai/writer", Effort: agentcfg.EffortLow}, ReviewerProfile: pin})
	for _, want := range []string{"pi_profile:", "model: openai/writer", "reviewer_profile:", "model: reviewer"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("receipt missing %q: %s", want, out.String())
		}
	}
}

func TestReviewerSelectionBadFlagsRefuseWithoutLeakingInput(t *testing.T) {
	for _, args := range [][]string{
		{"--reviewer="}, {"--reviewer-model", "reviewer"},
		{"--reviewer", "auto"}, {"--reviewer", "codex", "--reviewer-effort="},
		{"--reviewer", "codex", "--reviewer-model", "https://secret@host/model"},
		{"--reviewer", "codex", "--reviewer-effort", "secret"},
	} {
		cmd := newAxiRunCmd()
		if err := cmd.ParseFlags(args); err != nil {
			t.Fatal(err)
		}
		if _, err := reviewerFromFlags(cmd); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("invalid flags error: %v", err)
		}
	}
	cmd := newAxiRunCmd()
	if err := cmd.ParseFlags([]string{"--reviewer", "codex", "--reviewer", "pi"}); err == nil {
		t.Fatal("repeated reviewer accepted")
	}
	if pin, err := reviewerFromFlags(newAxiRunCmd()); err != nil || pin != nil {
		t.Fatalf("omission invented reviewer: %+v %v", pin, err)
	}
	pin := &agentcfg.ReviewerProfile{Agent: "codex", Model: "reviewer", Effort: agentcfg.EffortHigh}
	if !pin.Matches(nil) || !pin.Matches(&agentcfg.ReviewerProfile{Agent: "codex"}) || pin.Matches(&agentcfg.ReviewerProfile{Agent: "pi"}) {
		t.Fatal("reattachment changed reviewer identity")
	}
}

func TestReviewerSelectionRejectedBeforeGateSubmission(t *testing.T) {
	bin := buildNoMistakesBinary(t)
	for _, mode := range []string{"ordinary", "proof", "direct"} {
		t.Run(mode, func(t *testing.T) {
			calls := make(chan *agentcfg.ReviewerProfile, 4)
			var available atomic.Bool
			olderDaemonFixture(t, nil, func(srv *ipc.Server) {
				srv.Handle(ipc.MethodClaimLaunchReceipt, func(context.Context, json.RawMessage) (interface{}, error) {
					return &ipc.ClaimLaunchReceiptResult{}, nil
				})
				srv.Handle(ipc.MethodResolveReviewerProfile, func(_ context.Context, raw json.RawMessage) (interface{}, error) {
					var params ipc.ResolveReviewerProfileParams
					if err := json.Unmarshal(raw, &params); err != nil {
						return nil, err
					}
					if params.RepoID == "" {
						return nil, fmt.Errorf("missing reviewer repository")
					}
					calls <- params.ReviewerProfile
					return nil, fmt.Errorf("requested reviewer unavailable")
				})
				srv.Handle(ipc.MethodAdmitPush, func(_ context.Context, raw json.RawMessage) (interface{}, error) {
					var params ipc.AdmitPushParams
					if err := json.Unmarshal(raw, &params); err != nil {
						return nil, err
					}
					calls <- params.ReviewerProfile
					if !available.Load() {
						return nil, fmt.Errorf("requested reviewer unavailable")
					}
					return &ipc.AdmitPushResult{}, nil
				})
			})
			writeGlobalConfig(t, "")
			p, err := paths.New()
			if err != nil {
				t.Fatal(err)
			}
			d, err := db.Open(p.DB())
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			repo, err := findRepo(d)
			if err != nil {
				t.Fatal(err)
			}
			local, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			gateDir := p.RepoDir(repo.ID)
			cliGit(t, local, "clone", "--bare", local, gateDir)
			cliGit(t, gateDir, "config", "receive.advertisePushOptions", "true")
			cliGit(t, local, "remote", "add", gate.RemoteName, gateDir)
			cliGit(t, local, "commit", "--allow-empty", "-m", "new submission")
			before := cliGit(t, gateDir, "show-ref")
			pin := &agentcfg.ReviewerProfile{Agent: "codex", Model: "reviewer", Effort: agentcfg.EffortHigh}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			var cmd *exec.Cmd
			if mode == "direct" {
				exe, err := os.Executable()
				if err != nil {
					t.Fatal(err)
				}
				script := strings.Replace(git.PreReceiveHookScript(), "NM_BIN='"+exe+"'", "NM_BIN='"+bin+"'", 1)
				if err := os.WriteFile(filepath.Join(gateDir, "hooks", "pre-receive"), []byte(script), 0755); err != nil {
					t.Fatal(err)
				}
				args := []string{"push", "-o", formatReviewerPushOptions(pin)[0], gate.RemoteName, "HEAD:refs/heads/feature/omit"}
				cmd = exec.CommandContext(ctx, "git", args...)
			} else {
				args := []string{"axi", "run", "--intent", "validate independent reviewer", "--reviewer", "codex", "--reviewer-model", pin.Model, "--reviewer-effort", string(pin.Effort)}
				if mode == "proof" {
					args = append(args, "--launch-nonce", "reviewer-request", "--validation-generation", "reviewer-generation")
				}
				cmd = exec.CommandContext(ctx, bin, args...)
			}
			out, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(out), "reviewer unavailable") {
				t.Fatalf("unavailable reviewer accepted: %v\n%s", err, out)
			}
			select {
			case got := <-calls:
				if got == nil || *got != *pin {
					t.Fatalf("reviewer lost at admission: %+v", got)
				}
			default:
				t.Fatal("reviewer availability boundary was not called")
			}
			if after := cliGit(t, gateDir, "show-ref"); after != before {
				t.Fatalf("rejected reviewer changed refs: before=%s after=%s", before, after)
			}
			if mode == "direct" {
				available.Store(true)
				cmd = exec.CommandContext(ctx, "git", "push", "-o", formatReviewerPushOptions(pin)[0], gate.RemoteName, "HEAD:refs/heads/feature/omit")
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("available reviewer rejected: %v\n%s", err, out)
				}
				if got := cliGit(t, gateDir, "rev-parse", "refs/heads/feature/omit"); got != cliGit(t, local, "rev-parse", "HEAD") {
					t.Fatal("successful admission did not update gate ref")
				}
			}
		})
	}
}
