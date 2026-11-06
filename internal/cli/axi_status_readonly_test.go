package cli

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
	toon "github.com/toon-format/toon-go"
)

// Exercise the built CLI against a separate SQLite writer, rather than only
// checking which opener the query selects. No daemon or agent is needed.
func TestAxiStatusReadOnly(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "no-mistakes")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	build := exec.Command("go", "build", "-o", bin, "github.com/kunchenguid/no-mistakes/cmd/no-mistakes")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, out)
	}

	query := func(t *testing.T, dir string, args ...string) (string, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, append([]string{"axi", "status"}, args...)...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if ctx.Err() != nil {
			t.Fatalf("status timed out: %v\n%s", ctx.Err(), out)
		}
		return string(out), err
	}
	assertRun := func(t *testing.T, dir, id, step, status string, args ...string) {
		t.Helper()
		out, err := query(t, dir, args...)
		if err != nil {
			t.Fatalf("status %v failed while run %s exists: %v\n%s", args, id, err, out)
		}
		var doc struct {
			Run struct {
				ID     string `toon:"id"`
				Status string `toon:"status"`
				Steps  []struct {
					Step   string `toon:"step"`
					Status string `toon:"status"`
				} `toon:"steps"`
			} `toon:"run"`
		}
		if err := toon.UnmarshalString(out, &doc); err != nil {
			t.Fatalf("decode status: %v\n%s", err, out)
		}
		if doc.Run.ID != id || doc.Run.Status != "running" {
			t.Fatalf("status lost live run %s:\n%s", id, out)
		}
		for _, s := range doc.Run.Steps {
			if s.Step == step && s.Status == status {
				return
			}
		}
		t.Fatalf("status lost %s step in state %s:\n%s", step, status, out)
	}

	for _, pendingSchema := range []bool{false, true} {
		name := "current schema"
		if pendingSchema {
			name = "pending schema"
		}
		t.Run("live Test remains visible during a write transaction/"+name, func(t *testing.T) {
			dir, p, database, repo := setupAxiQueryRepo(t)
			run(t, dir, "git", "checkout", "-b", "feature/live-test")
			live, err := database.InsertRun(repo.ID, "feature/live-test", "head", "base")
			if err != nil {
				t.Fatal(err)
			}
			if err := database.UpdateRunStatus(live.ID, types.RunRunning); err != nil {
				t.Fatal(err)
			}
			step, err := database.InsertStepResult(live.ID, types.StepTest)
			if err != nil {
				t.Fatal(err)
			}
			if err := database.UpdateStepStatus(step.ID, types.StepStatusRunning); err != nil {
				t.Fatal(err)
			}
			writer, err := sql.Open("sqlite", p.DB())
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Close()
			if pendingSchema {
				// Model an existing database awaiting an additive schema change.
				// Status does not need this table, so a pure reader can still
				// observe the committed run while another connection writes.
				if _, err := writer.Exec("DROP TABLE intent_cache"); err != nil {
					t.Fatal(err)
				}
			}
			var mode string
			if err := writer.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
				t.Fatalf("fixture must use WAL: mode=%q, err=%v", mode, err)
			}
			tx, err := writer.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			// Returning from UPDATE is the barrier: this real second connection
			// owns the write lock until the explicit rollback below.
			if _, err := tx.Exec("UPDATE runs SET updated_at = updated_at + 1 WHERE id = ?", live.ID); err != nil {
				t.Fatal(err)
			}
			for range 3 {
				assertRun(t, dir, live.ID, "test", "running")
				assertRun(t, dir, live.ID, "test", "running", "--run", live.ID)
			}
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			if err := database.UpdateStepStatus(step.ID, types.StepStatusCompleted); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{nil, {"--run", live.ID}} {
				assertRun(t, dir, live.ID, "test", "completed", args...)
			}
		})
	}

	t.Run("does not repair schema", func(t *testing.T) {
		dir, p, _, _ := setupAxiQueryRepo(t)
		conn, err := sql.Open("sqlite", p.DB())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		// These unused objects distinguish schemaSQL and migrationStatements
		// from reads without making the status query itself invalid.
		if _, err := conn.Exec("DROP TABLE intent_cache; DROP TRIGGER runs_verification_plan_immutable"); err != nil {
			t.Fatal(err)
		}
		if out, err := query(t, dir); err != nil {
			t.Fatalf("status: %v\n%s", err, out)
		}
		var n int
		if err := conn.QueryRow("SELECT count(*) FROM sqlite_master WHERE name IN ('intent_cache', 'runs_verification_plan_immutable')").Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("status recreated %d schema/migration objects", n)
		}
	})

	t.Run("missing run is not a live run", func(t *testing.T) {
		dir, _, _, _ := setupAxiQueryRepo(t)
		out, err := query(t, dir)
		if err != nil || !strings.Contains(out, "runs_on_current_branch: 0") {
			t.Fatalf("genuine no-run status: %v\n%s", err, out)
		}
		out, err = query(t, dir, "--run", "missing-run")
		if err == nil || !strings.Contains(out, "not found") {
			t.Fatalf("missing explicit run must report not found: %v\n%s", err, out)
		}
	})

	t.Run("missing database is not created", func(t *testing.T) {
		dir, _, _, _ := setupAxiQueryRepo(t)
		root := filepath.Join(t.TempDir(), "absent")
		t.Setenv("NM_HOME", root)
		for _, args := range [][]string{nil, {"--run", "missing-run"}} {
			out, err := query(t, dir, args...)
			if err == nil || !strings.Contains(out, "error:") || strings.Contains(out, "runs_on_current_branch:") {
				t.Fatalf("missing database must report a read error: %v\n%s", err, out)
			}
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Fatalf("status created state directories: %v", err)
			}
		}
	})

	for _, corrupt := range []bool{false, true} {
		name := "empty database"
		if corrupt {
			name = "corrupt database"
		}
		t.Run(name+" reports a read error", func(t *testing.T) {
			dir, _, _, _ := setupAxiQueryRepo(t)
			root := t.TempDir()
			t.Setenv("NM_HOME", root)
			path := filepath.Join(root, "state.sqlite")
			var contents []byte
			if corrupt {
				contents = []byte("this is not a SQLite database")
			}
			if err := os.WriteFile(path, contents, 0o600); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{nil, {"--run", "missing-run"}} {
				out, err := query(t, dir, args...)
				if err == nil || !strings.Contains(out, "error:") || strings.Contains(out, "runs_on_current_branch:") || strings.Contains(out, "not found") {
					t.Fatalf("invalid database must report a read error, not no-run: %v\n%s", err, out)
				}
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(after, contents) {
				t.Fatalf("status changed invalid database: %v", err)
			}
		})
	}
}
