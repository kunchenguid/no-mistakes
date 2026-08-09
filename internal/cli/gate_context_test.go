package cli

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/gatecontext"
	gitpkg "github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/spf13/cobra"
)

func TestGateControlPolicyCoversEveryMutationEntrypoint(t *testing.T) {
	root := newRootCmd()
	cases := []struct {
		args    []string
		mutates bool
	}{
		{args: nil, mutates: true},
		{args: []string{"init"}, mutates: true},
		{args: []string{"eject"}, mutates: true},
		{args: []string{"rerun"}, mutates: true},
		{args: []string{"sync"}, mutates: true},
		{args: []string{"sync", "--recover"}, mutates: true},
		{args: []string{"sync", "--check"}, mutates: false},
		{args: []string{"axi", "run"}, mutates: true},
		{args: []string{"axi", "respond"}, mutates: true},
		{args: []string{"axi", "sync"}, mutates: true},
		{args: []string{"axi", "sync", "--recover"}, mutates: true},
		{args: []string{"axi", "sync", "--check"}, mutates: false},
		{args: []string{"axi", "abort"}, mutates: true},
		{args: []string{"axi", "status"}, mutates: false},
		{args: []string{"axi", "logs"}, mutates: false},
		{args: []string{"status"}, mutates: false},
		{args: []string{"doctor"}, mutates: false},
		{args: []string{"daemon", "stop", "--force"}, mutates: true},
	}
	for _, tc := range cases {
		cmd, _, err := root.Find(tc.args)
		if err != nil {
			t.Fatalf("find %v: %v", tc.args, err)
		}
		if contains(tc.args, "--check") {
			_ = cmd.Flags().Set("check", "true")
		} else if cmd.Flags().Lookup("check") != nil {
			_ = cmd.Flags().Set("check", "false")
		}
		if got := mutatesPipelineControl(cmd); got != tc.mutates {
			t.Errorf("%v mutates = %v, want %v (path=%s)", tc.args, got, tc.mutates, cmd.CommandPath())
		}
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestGateContextRefusalIsStructuredActionableAndPrivacySafe(t *testing.T) {
	cmd := newAxiRunCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	err := emitGateContextRefusal(cmd, gatecontext.Result{Nested: true, ManagedGit: true, AgentDescendant: true, RunID: "run-safe", Phase: types.StepDocument})
	if err == nil {
		t.Fatal("expected refusal exit error")
	}
	text := out.String()
	for _, want := range []string{
		"code: nested_gate_context",
		"run: run-safe",
		"phase: document",
		"enclosing executor owns validation, push, PR, and CI",
		"no-mistakes axi status",
		"Return control to the outer executor",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("refusal missing %q:\n%s", want, text)
		}
	}
	for _, forbidden := range []string{"agent_pid", "peer_pid", "/worktrees/", "/repos/"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("refusal leaked %q:\n%s", forbidden, text)
		}
	}
}

func TestGateControlGuardClassifiesLegacySchemaBeforeMigration(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)
	p := paths.WithRoot(nmHome)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	run(t, root, "git", "init", "--bare", "--initial-branch=main", origin)
	work := filepath.Join(root, "work")
	run(t, root, "git", "init", "--initial-branch=main", work)
	run(t, work, "git", "config", "user.email", "test@example.com")
	run(t, work, "git", "config", "user.name", "Test")
	run(t, work, "git", "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("legacy\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, work, "git", "add", "README.md")
	run(t, work, "git", "commit", "-m", "base")
	run(t, work, "git", "remote", "add", "origin", origin)
	run(t, work, "git", "push", "-u", "origin", "main")

	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	repo, _, err := gate.Init(context.Background(), database, p, work)
	if err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	runRecord, err := database.InsertRun(repo.ID, "main", "head", "base")
	if err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(runRecord.ID, types.RunRunning); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	step, err := database.InsertStepResult(runRecord.ID, types.StepReview)
	if err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.StartStep(step.ID); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	gateDir := p.RepoDir(repo.ID)
	if _, err := gitpkg.Run(context.Background(), gateDir, "fetch", work, "HEAD:refs/heads/main"); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	managed := p.WorktreeDir(repo.ID, runRecord.ID)
	if err := os.MkdirAll(filepath.Dir(managed), 0o755); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if _, err := gitpkg.Run(context.Background(), gateDir, "worktree", "add", "--detach", managed, "refs/heads/main"); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	legacy, err := sql.Open("sqlite", p.DB())
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"ALTER TABLE runs DROP COLUMN custody_return_reason",
		"ALTER TABLE repos DROP COLUMN metadata_generation",
	} {
		if _, err := legacy.Exec(statement); err != nil {
			_ = legacy.Close()
			t.Fatalf("prepare legacy schema with %q: %v", statement, err)
		}
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	command := func(t *testing.T) *cobra.Command {
		t.Helper()
		root := newRootCmd()
		root.SetContext(context.Background())
		cmd, _, err := root.Find([]string{"daemon", "start"})
		if err != nil {
			t.Fatal(err)
		}
		cmd.SetContext(context.Background())
		return cmd
	}
	withWorkingDirectory := func(t *testing.T, dir string) {
		t.Helper()
		before, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chdir(dir); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chdir(before) })
	}

	t.Run("stopped daemon allows an ordinary caller", func(t *testing.T) {
		withWorkingDirectory(t, work)
		if err := guardGateControl(command(t)); err != nil {
			t.Fatalf("legacy read-only guard rejected ordinary caller: %v", err)
		}
	})

	t.Run("managed worktree remains contained", func(t *testing.T) {
		withWorkingDirectory(t, managed)
		cmd := command(t)
		var out bytes.Buffer
		cmd.SetOut(&out)
		err := guardGateControl(cmd)
		if _, ok := err.(*exitError); !ok || !strings.Contains(out.String(), "code: nested_gate_context") {
			t.Fatalf("legacy read-only guard did not preserve containment: err=%v output=%q", err, out.String())
		}
	})

	readonly, err := sql.Open("sqlite", "file:"+p.DB()+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer readonly.Close()
	for table, column := range map[string]string{"runs": "custody_return_reason", "repos": "metadata_generation"} {
		var count int
		if err := readonly.QueryRow("SELECT count(*) FROM pragma_table_info(?) WHERE name = ?", table, column).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("read-only guard migrated %s.%s", table, column)
		}
	}
}
