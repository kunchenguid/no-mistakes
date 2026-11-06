package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/paths"
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

	queryCommand := func(t *testing.T, dir, command string, args ...string) (string, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, append([]string{"axi", command}, args...)...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if ctx.Err() != nil {
			t.Fatalf("%s timed out: %v\n%s", command, ctx.Err(), out)
		}
		return string(out), err
	}
	query := func(t *testing.T, dir string, args ...string) (string, error) {
		t.Helper()
		return queryCommand(t, dir, "status", args...)
	}
	assertLogs := func(t *testing.T, dir, id string, args ...string) {
		t.Helper()
		out, err := queryCommand(t, dir, "logs", append([]string{"--step", "test"}, args...)...)
		if err != nil {
			t.Fatalf("logs %v failed while run %s exists: %v\n%s", args, id, err, out)
		}
		var doc struct {
			Run  string `toon:"run"`
			Step string `toon:"step"`
			Log  []struct {
				Line string `toon:"line"`
			} `toon:"log"`
		}
		if err := toon.UnmarshalString(out, &doc); err != nil {
			t.Fatalf("decode logs: %v\n%s", err, out)
		}
		if doc.Run != id || doc.Step != "test" || len(doc.Log) != 1 || doc.Log[0].Line != "live Test output" {
			t.Fatalf("logs lost live run %s or its output:\n%s", id, out)
		}
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
			BranchSync struct {
				Pipeline struct {
					Run string `toon:"run"`
				} `toon:"pipeline"`
			} `toon:"branch_sync"`
		}
		if err := toon.UnmarshalString(out, &doc); err != nil {
			t.Fatalf("decode status: %v\n%s", err, out)
		}
		if doc.Run.ID != id || doc.Run.Status != "running" {
			t.Fatalf("status lost live run %s:\n%s", id, out)
		}
		if doc.BranchSync.Pipeline.Run != id {
			t.Fatalf("status lost cached branch-sync annotation for run %s:\n%s", id, out)
		}
		for _, s := range doc.Run.Steps {
			if s.Step == step && s.Status == status {
				return
			}
		}
		t.Fatalf("status lost %s step in state %s:\n%s", step, status, out)
	}

	for _, preFork := range []bool{false, true} {
		name := "v1.106.1"
		if preFork {
			name += " without fork registration"
		}
		t.Run("released schema/"+name, func(t *testing.T) {
			dir, _, _, repo := setupAxiQueryRepo(t)
			run(t, dir, "git", "checkout", "-b", "feature/released-schema")
			t.Setenv("NM_HOME", t.TempDir())
			p, err := paths.New()
			if err != nil {
				t.Fatal(err)
			}
			writer, err := sql.Open("sqlite", p.DB()+"?_pragma=journal_mode(wal)")
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Close()
			var fixture struct {
				Release    string   `json:"release"`
				Commit     string   `json:"commit"`
				Schema     string   `json:"schema"`
				Migrations []string `json:"migrations"`
			}
			data, err := os.ReadFile(filepath.Join("testdata", "status-schema-v1.106.1.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(data, &fixture); err != nil {
				t.Fatal(err)
			}
			if fixture.Release != "v1.106.1" || fixture.Commit != "5732d1ed88a1572fa6115d6ede5d69bfec097946" {
				t.Fatal("fixture must retain its released-schema provenance")
			}
			if _, err := writer.Exec(fixture.Schema); err != nil {
				t.Fatal(err)
			}
			// Follow this release's opener, including its duplicate-column no-op.
			for _, statement := range fixture.Migrations {
				if _, err := writer.Exec(statement); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
					t.Fatal(err)
				}
			}
			// The last release already has fork_url. Also exercise a database
			// predating fork registration rather than mislabelling it as that release.
			if preFork {
				if _, err := writer.Exec("ALTER TABLE repos DROP COLUMN fork_url"); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := writer.Exec(`INSERT INTO repos (id, working_path, upstream_url, default_branch, created_at) VALUES (?, ?, ?, 'main', 1)`, repo.ID, dir, repo.UpstreamURL); err != nil {
				t.Fatal(err)
			}
			const id = "released-schema-run"
			if _, err := writer.Exec(`INSERT INTO runs (id, repo_id, branch, head_sha, base_sha, status, created_at, updated_at) VALUES (?, ?, 'feature/released-schema', 'head', 'base', 'running', 1, 2)`, id, repo.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := writer.Exec(`INSERT INTO step_results (id, run_id, step_name, step_order, status) VALUES ('released-schema-step', ?, 'test', ?, 'running')`, id, types.StepTest.Order()); err != nil {
				t.Fatal(err)
			}
			if _, err := writer.Exec(`INSERT INTO step_rounds (id, step_result_id, round, trigger_type, duration_ms, created_at) VALUES ('released-schema-round', 'released-schema-step', 1, 'initial', 1, 1)`); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(p.RunLogDir(id), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(p.RunLogDir(id), "test.log"), []byte("live Test output\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			var before int
			if err := writer.QueryRow("PRAGMA schema_version").Scan(&before); err != nil {
				t.Fatal(err)
			}
			tx, err := writer.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if _, err := tx.Exec("UPDATE runs SET updated_at = updated_at + 1 WHERE id = ?", id); err != nil {
				t.Fatal(err)
			}
			for range 3 {
				for _, args := range [][]string{nil, {"--run", id}} {
					assertRun(t, dir, id, "test", "running", args...)
					assertLogs(t, dir, id, args...)
				}
			}
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			if _, err := writer.Exec("UPDATE step_results SET status = 'completed' WHERE id = 'released-schema-step'"); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{nil, {"--run", id}} {
				assertRun(t, dir, id, "test", "completed", args...)
				assertLogs(t, dir, id, args...)
			}
			var after int
			if err := writer.QueryRow("PRAGMA schema_version").Scan(&after); err != nil || after != before {
				t.Fatalf("queries changed the released schema: before=%d after=%d err=%v", before, after, err)
			}
		})
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
			if err := os.MkdirAll(p.RunLogDir(live.ID), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(p.RunLogDir(live.ID), "test.log"), []byte("live Test output\n"), 0o600); err != nil {
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
				if _, err := writer.Exec(`DROP TABLE intent_cache;
					DROP TRIGGER runs_reviewer_profile_immutable;
					ALTER TABLE runs DROP COLUMN reviewer_profile;
					ALTER TABLE runs DROP COLUMN parked_ms;
					ALTER TABLE runs DROP COLUMN quota_routing`); err != nil {
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
				assertLogs(t, dir, live.ID)
				assertLogs(t, dir, live.ID, "--run", live.ID)
			}
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			if err := database.UpdateStepStatus(step.ID, types.StepStatusCompleted); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{nil, {"--run", live.ID}} {
				assertRun(t, dir, live.ID, "test", "completed", args...)
				assertLogs(t, dir, live.ID, args...)
			}
			if pendingSchema {
				var n int
				if err := writer.QueryRow("SELECT count(*) FROM pragma_table_info('runs') WHERE name IN ('reviewer_profile', 'parked_ms', 'quota_routing')").Scan(&n); err != nil {
					t.Fatal(err)
				}
				if n != 0 {
					t.Fatalf("queries recreated %d missing run columns", n)
				}
			}
		})
	}

	t.Run("does not repair schema", func(t *testing.T) {
		dir, p, database, repo := setupAxiQueryRepo(t)
		run(t, dir, "git", "checkout", "-b", "feature/schema-reader")
		live, err := database.InsertRun(repo.ID, "feature/schema-reader", "head", "base")
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
		for _, args := range [][]string{nil, {"--run", live.ID}} {
			assertRun(t, dir, live.ID, "test", "running", args...)
			var n int
			if err := conn.QueryRow("SELECT count(*) FROM sqlite_master WHERE name IN ('intent_cache', 'runs_verification_plan_immutable')").Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 0 {
				t.Fatalf("status recreated %d schema/migration objects", n)
			}
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
