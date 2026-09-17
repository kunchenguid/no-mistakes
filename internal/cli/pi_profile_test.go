package cli

import (
	"bytes"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/spf13/cobra"
)

func TestPiProfileFlagsAndPushOptions(t *testing.T) {
	for _, args := range [][]string{{}, {"--model", "openai-codex/gpt-5.4", "--effort", "high"}, {"--effort", "low"}} {
		cmd := &cobra.Command{}
		var model, effort string
		bindPiProfileFlags(cmd, &model, &effort)
		if err := cmd.ParseFlags(args); err != nil {
			t.Fatal(err)
		}
		profile, err := piProfileFromFlags(cmd, model, effort)
		if err != nil {
			t.Fatal(err)
		}
		encoded := formatPiProfilePushOptions(profile)
		decoded, err := parsePiProfilePushOptions(encoded)
		if err != nil || !reflect.DeepEqual(decoded, profile) {
			t.Fatalf("roundtrip %+v -> %+v: %v", profile, decoded, err)
		}
		if len(encoded) > 0 {
			if _, err := parsePiProfilePushOptions(append(encoded, encoded...)); err == nil {
				t.Fatal("duplicate accepted")
			}
		}
	}
	for _, args := range [][]string{{"--model="}, {"--effort="}, {"--model", "https://secret@host/model"}, {"--effort", "wrong"}} {
		cmd := &cobra.Command{}
		var model, effort string
		bindPiProfileFlags(cmd, &model, &effort)
		if err := cmd.ParseFlags(args); err != nil {
			t.Fatal(err)
		}
		_, err := piProfileFromFlags(cmd, model, effort)
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("bad flags error: %v", err)
		}
	}
}

func TestPiProfileDuplicateFlagsAreRefused(t *testing.T) {
	cmd := newAxiRunCmd()
	if err := cmd.ParseFlags([]string{"--model", "openai/model-a", "--model", "openai/model-b"}); err == nil {
		t.Fatal("conflicting model flags silently used last value")
	}
}

func TestPiProfileHelpStatusAndUsageEvidence(t *testing.T) {
	var help bytes.Buffer
	cmd := newAxiRunCmd()
	cmd.SetOut(&help)
	if err := cmd.Help(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--model", "--effort", "immutable Pi profile", "recovery"} {
		if !strings.Contains(help.String(), want) {
			t.Errorf("help missing %s", want)
		}
	}
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	repo, err := d.InsertRepo(t.TempDir(), "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	pin := &agentcfg.PiProfile{Model: "openai-codex/gpt-5.4", Effort: agentcfg.EffortHigh}
	run, err := d.InsertRunWithIntentAndLaunchNonce(repo.ID, "feature", "head", "base", nil, "", "", "", "", pin)
	if err != nil {
		t.Fatal(err)
	}
	for _, view := range []runView{runViewFromDB(run, nil, d), runViewFromIPC(&ipc.RunInfo{ID: run.ID, PiProfile: pin})} {
		var out bytes.Buffer
		render := &cobra.Command{}
		render.SetOut(&out)
		emitDoc(render, runObjectField(view))
		for _, want := range []string{"pi_profile:", "model: openai-codex/gpt-5.4", "effort: high"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("status missing %s: %s", want, out.String())
			}
		}
	}
	var out bytes.Buffer
	if err := renderRunAgentPerf(&out, d, run.ID); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "model=openai-codex/gpt-5.4 effort=high") {
		t.Fatalf("usage missing pin: %s", out.String())
	}
}
