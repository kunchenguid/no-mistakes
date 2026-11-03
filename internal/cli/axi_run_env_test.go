package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

const fakeRunEnvToken = "sk-ant-oat01-FAKE-run-secret-value"

func writeRunEnvFile(t *testing.T, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "run.env")
	if err := os.WriteFile(path, []byte("RUN_TOKEN="+fakeRunEnvToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAxiRunEnvFileRefusalsHappenBeforeTheDaemonAndNeverQuoteTheValue(t *testing.T) {
	owned := writeRunEnvFile(t, 0o600)
	unquotedLine := filepath.Join(t.TempDir(), "bad.env")
	if err := os.WriteFile(unquotedLine, []byte(fakeRunEnvToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name          string
		args          []string
		want          string
		posixModeOnly bool
	}{
		{"group readable", []string{"--run-env-file", writeRunEnvFile(t, 0o640)}, "chmod 600", true},
		{"world readable", []string{"--run-env-file", writeRunEnvFile(t, 0o604)}, "chmod 600", true},
		{"empty path", []string{"--run-env-file="}, "--run-env-file requires a file path", false},
		{"missing", []string{"--run-env-file", filepath.Join(t.TempDir(), "absent")}, "open run env file", false},
		{"line without a name", []string{"--run-env-file", unquotedLine}, "line 1 is not NAME=VALUE", false},
		{"with a Pi profile pin", []string{"--run-env-file", owned, "--model", "claude-bridge/claude-haiku-5-5"}, "cannot be combined with --model/--effort", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.posixModeOnly && runtime.GOOS == "windows" {
				t.Skip("POSIX mode bits")
			}
			home := filepath.Join(t.TempDir(), "not-created")
			t.Setenv("NM_HOME", home)
			cmd := newAxiRunCmd()
			cmd.SetArgs(append([]string{"--intent", "goal"}, tc.args...))
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			err := cmd.Execute()
			var ee *exitError
			if !errors.As(err, &ee) || ee.code != 2 || !strings.Contains(out.String(), tc.want) {
				t.Fatalf("error = %v, output = %s; want usage error %q", err, out.String(), tc.want)
			}
			if strings.Contains(out.String(), fakeRunEnvToken) {
				t.Fatalf("refusal quoted the secret: %s", out.String())
			}
			if _, err := os.Stat(home); !os.IsNotExist(err) {
				t.Fatalf("refused input opened app resources: stat = %v", err)
			}
		})
	}
}

func TestAxiRunEnvFileOlderDaemonIsRefusedBeforeTheGatePush(t *testing.T) {
	launched := olderDaemonFixture(t, func() (interface{}, error) { return &ipc.ProbeOmitIntentResult{OK: true}, nil })
	writeGlobalConfig(t, "")
	cmd := newAxiRunCmd()
	cmd.SetContext(context.Background())
	cmd.SetArgs([]string{"--intent", "goal", "--run-env-file", writeRunEnvFile(t, 0o600)})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := cmd.Execute(); err == nil || !strings.Contains(out.String(), "hand --run-env-file to the daemon") {
		t.Fatalf("old daemon accepted: %v\n%s", err, out.String())
	}
	if len(*launched) != 0 {
		t.Fatalf("old daemon took custody: %v", *launched)
	}
	if strings.Contains(out.String(), fakeRunEnvToken) {
		t.Fatalf("refusal quoted the secret: %s", out.String())
	}
}

func TestAxiRunEnvFileIsStagedForTheSubmittedHeadAndReleasedAfterTheLaunch(t *testing.T) {
	var staged ipc.StageRunEnvParams
	var released ipc.ReleaseRunEnvParams
	olderDaemonFixture(t, func() (interface{}, error) { return &ipc.ProbeOmitIntentResult{OK: true}, nil }, func(srv *ipc.Server) {
		srv.Handle(ipc.MethodStageRunEnv, func(_ context.Context, raw json.RawMessage) (interface{}, error) {
			return nil, json.Unmarshal(raw, &staged)
		})
		srv.Handle(ipc.MethodReleaseRunEnv, func(_ context.Context, raw json.RawMessage) (interface{}, error) {
			return nil, json.Unmarshal(raw, &released)
		})
	})
	writeGlobalConfig(t, "")
	cmd := newAxiRunCmd()
	cmd.SetContext(context.Background())
	cmd.SetArgs([]string{"--intent", "goal", "--run-env-file", writeRunEnvFile(t, 0o600)})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	_ = cmd.Execute()
	headOut, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	head := strings.TrimSpace(string(headOut))
	if staged.Vars["RUN_TOKEN"] != fakeRunEnvToken || staged.HeadSHA != head || staged.Branch == "" || staged.RepoID == "" {
		t.Fatalf("staged = repo %q branch %q head %q vars %d, want the token for head %s; output: %s", staged.RepoID, staged.Branch, staged.HeadSHA, len(staged.Vars), head, out.String())
	}
	if released.RepoID != staged.RepoID || released.Branch != staged.Branch || released.HeadSHA != staged.HeadSHA {
		t.Fatalf("launch did not release its staged variables: %+v", released)
	}
	if strings.Contains(out.String(), fakeRunEnvToken) {
		t.Fatalf("output quoted the secret: %s", out.String())
	}
}

func TestAxiRunEnvFileReattachIsRefusedRatherThanSilentlyIgnored(t *testing.T) {
	fx := newAxiTimeoutFixture(t, axiTimeoutOpts{})
	fx.setGetActive(func(context.Context) (*ipc.RunInfo, error) { return fx.running(), nil })
	cmd := newAxiRunCmd()
	cmd.SetArgs([]string{"--run-env-file", writeRunEnvFile(t, 0o600)})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := cmd.Execute()
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 2 || !strings.Contains(out.String(), "--run-env-file is accepted only when starting a new run") {
		t.Fatalf("reattach with --run-env-file = %v\n%s", err, out.String())
	}
}
