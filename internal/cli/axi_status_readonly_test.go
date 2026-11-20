package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
	toon "github.com/toon-format/toon-go"
)

// Exercise the built CLI against a separate SQLite writer, rather than only
// checking which opener the query selects. No daemon or agent is needed.
func TestAxiStatusReadOnly(t *testing.T) {
	t.Parallel()
	bin := buildNoMistakesBinary(t)
	// All commands only read this checkout. Give each schema variant its own
	// state root explicitly, so independent cases need no process-wide Setenv.
	dir := t.TempDir()
	run(t, dir, "git", "init", "-b", "feature/status-reader")
	run(t, dir, "git", "-c", "user.email=test@test.com", "-c", "user.name=Test", "commit", "--allow-empty", "-m", "initial")
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	openFixture := func(t *testing.T) (*paths.Paths, *db.DB, *db.Repo) {
		t.Helper()
		p := paths.WithRoot(t.TempDir())
		if err := p.EnsureDirs(); err != nil {
			t.Fatal(err)
		}
		database, err := db.Open(p.DB())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = database.Close() })
		repo, err := database.InsertRepoWithID("repo-1", dir, "origin", "main")
		if err != nil {
			t.Fatal(err)
		}
		return p, database, repo
	}

	queryCommand := func(t *testing.T, root, command string, args ...string) (string, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, append([]string{"axi", command}, args...)...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "NM_HOME="+root, "NM_TEST_START_DAEMON=")
		out, err := cmd.CombinedOutput()
		if ctx.Err() != nil {
			t.Fatalf("%s timed out: %v\n%s", command, ctx.Err(), out)
		}
		return string(out), err
	}
	query := func(t *testing.T, root string, args ...string) (string, error) {
		t.Helper()
		return queryCommand(t, root, "status", args...)
	}
	assertLogs := func(t *testing.T, root, id string, args ...string) {
		t.Helper()
		out, err := queryCommand(t, root, "logs", append([]string{"--step", "test"}, args...)...)
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
	assertRun := func(t *testing.T, root, id, step, status string, args ...string) string {
		t.Helper()
		out, err := query(t, root, args...)
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
				return out
			}
		}
		t.Fatalf("status lost %s step in state %s:\n%s", step, status, out)
		return ""
	}

	for _, preFork := range []bool{false, true} {
		name := "v1.106.1"
		if preFork {
			name += " without additive metadata"
		}
		t.Run("released schema/"+name, func(t *testing.T) {
			t.Parallel()
			p := paths.WithRoot(t.TempDir())
			if err := p.EnsureDirs(); err != nil {
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
				for _, column := range []string{"reviewed_head_sha", "starting_head_sha", "trusted_config_sha", "global_config_yaml", "repo_config_yaml", "user_findings_json", "selected_finding_ids", "selection_source", "fix_summary", "repair_published", "response_origin"} {
					if _, err := writer.Exec("ALTER TABLE step_rounds DROP COLUMN " + column); err != nil {
						t.Fatal(err)
					}
				}
				for _, column := range []string{"last_activity_at", "last_activity", "agent_pid", "auto_fix_limit", "round_started_at", "override_reason", "skip_reason", "approval_reason", "failure_cause", "carried_from", "response_origin"} {
					if _, err := writer.Exec("ALTER TABLE step_results DROP COLUMN " + column); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := writer.Exec(`INSERT INTO repos (id, working_path, upstream_url, default_branch, created_at) VALUES (?, ?, ?, 'main', 1)`, "repo-1", dir, "origin"); err != nil {
				t.Fatal(err)
			}
			const id = "released-schema-run"
			head, base := strings.Repeat("a", 40), strings.Repeat("b", 40)
			if _, err := writer.Exec(`INSERT INTO runs (id, repo_id, branch, head_sha, base_sha, submitted_head_sha, status, created_at, updated_at) VALUES (?, ?, 'feature/status-reader', ?, ?, ?, 'running', 1, 2)`, id, "repo-1", head, base, head); err != nil {
				t.Fatal(err)
			}
			if _, err := writer.Exec(`INSERT INTO step_results (id, run_id, step_name, step_order, status) VALUES ('released-schema-step', ?, 'test', ?, 'running')`, id, types.StepTest.Order()); err != nil {
				t.Fatal(err)
			}
			if _, err := writer.Exec(`INSERT INTO step_rounds (id, step_result_id, round, trigger_type, duration_ms, created_at) VALUES ('released-schema-round', 'released-schema-step', 1, 'initial', 1, 1)`); err != nil {
				t.Fatal(err)
			}
			if _, err := writer.Exec(`INSERT INTO step_results (id, run_id, step_name, step_order, status) VALUES ('released-schema-document', ?, 'document', ?, 'completed')`, id, types.StepDocument.Order()); err != nil {
				t.Fatal(err)
			}
			if _, err := writer.Exec(`INSERT INTO agent_invocations (id, run_id, step_name, round, purpose, agent, session_mode, started_at, completed_at, duration_ms, exit_status) VALUES ('released-schema-invocation', ?, 'document', 1, 'housekeeping', 'fake', 'cold', 1, 2, 1, 'success')`, id); err != nil {
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
			assertAnnotations := func(out string) {
				// Check the same successful status observation, rather than
				// starting an identical second CLI process for its annotations.
				t.Helper()
				var doc struct {
					Run struct {
						Shared []struct {
							Step  string `toon:"attributed_to"`
							Scope string `toon:"scope"`
						} `toon:"shared_work"`
						Active []struct {
							Step  string `toon:"step"`
							Round string `toon:"round"`
						} `toon:"active_steps"`
					} `toon:"run"`
				}
				if err := toon.UnmarshalString(out, &doc); err != nil {
					t.Fatal(err)
				}
				var housekeeping bool
				for _, work := range doc.Run.Shared {
					housekeeping = housekeeping || (work.Step == "document" && work.Scope == "document+lint housekeeping")
				}
				if !housekeeping || len(doc.Run.Active) != 1 || doc.Run.Active[0].Step != "test" || doc.Run.Active[0].Round != "round 1" {
					t.Fatalf("legacy reads lost round or invocation evidence:\n%s", out)
				}
			}
			assertBranch := func(status string) {
				t.Helper()
				out, err := query(t, p.Root(), "--branch", "feature/status-reader")
				if err != nil {
					t.Fatalf("legacy branch snapshot: %v\n%s", err, out)
				}
				var doc struct {
					Schema string `toon:"schema"`
					Run    struct {
						ID    string `toon:"id"`
						Steps []struct {
							Step     string `toon:"step"`
							Recorded bool   `toon:"recorded"`
							Status   any    `toon:"status"`
						} `toon:"steps"`
					} `toon:"run"`
				}
				if err := toon.UnmarshalString(out, &doc); err != nil {
					t.Fatal(err)
				}
				for _, step := range doc.Run.Steps {
					if doc.Schema == "branch-publication.v1" && doc.Run.ID == id && step.Step == "test" && step.Recorded && step.Status == status {
						return
					}
				}
				t.Fatalf("legacy snapshot lost Test state:\n%s", out)
			}
			for range 3 {
				for _, args := range [][]string{nil, {"--run", id}} {
					out := assertRun(t, p.Root(), id, "test", "running", args...)
					assertLogs(t, p.Root(), id, args...)
					assertAnnotations(out)
				}
				assertBranch("running")
			}
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			if _, err := writer.Exec("UPDATE step_results SET status = 'completed' WHERE id = 'released-schema-step'"); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{nil, {"--run", id}} {
				assertRun(t, p.Root(), id, "test", "completed", args...)
				assertLogs(t, p.Root(), id, args...)
			}
			assertBranch("completed")
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
			t.Parallel()
			p, database, repo := openFixture(t)
			if !pendingSchema {
				t.Run("missing run is not a live run", func(t *testing.T) {
					out, err := query(t, p.Root())
					if err != nil || !strings.Contains(out, "runs_on_current_branch: 0") {
						t.Fatalf("genuine no-run status: %v\n%s", err, out)
					}
					out, err = query(t, p.Root(), "--run", "missing-run")
					if err == nil || !strings.Contains(out, "not found") {
						t.Fatalf("missing explicit run must report not found: %v\n%s", err, out)
					}
				})
			}
			live, err := database.InsertRun(repo.ID, "feature/status-reader", "head", "base")
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
				assertRun(t, p.Root(), live.ID, "test", "running")
				assertRun(t, p.Root(), live.ID, "test", "running", "--run", live.ID)
				assertLogs(t, p.Root(), live.ID)
				assertLogs(t, p.Root(), live.ID, "--run", live.ID)
			}
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			if !pendingSchema {
				t.Run("does not repair schema", func(t *testing.T) {
					// Reuse the current-schema fixture after the lock test, before
					// completing Test, so the same running-state assertions apply.
					if _, err := writer.Exec("DROP TABLE intent_cache; DROP TRIGGER runs_verification_plan_immutable"); err != nil {
						t.Fatal(err)
					}
					for _, args := range [][]string{nil, {"--run", live.ID}} {
						assertRun(t, p.Root(), live.ID, "test", "running", args...)
						var n int
						if err := writer.QueryRow("SELECT count(*) FROM sqlite_master WHERE name IN ('intent_cache', 'runs_verification_plan_immutable')").Scan(&n); err != nil {
							t.Fatal(err)
						}
						if n != 0 {
							t.Fatalf("status recreated %d schema/migration objects", n)
						}
					}
				})
			}
			if err := database.UpdateStepStatus(step.ID, types.StepStatusCompleted); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{nil, {"--run", live.ID}} {
				assertRun(t, p.Root(), live.ID, "test", "completed", args...)
				assertLogs(t, p.Root(), live.ID, args...)
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

	for _, tc := range []struct {
		name     string
		missing  bool
		contents []byte
	}{
		{name: "missing database is not created", missing: true},
		{name: "empty database reports a read error"},
		{name: "corrupt database reports a read error", contents: []byte("this is not a SQLite database")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := filepath.Join(t.TempDir(), "state")
			path := filepath.Join(root, "state.sqlite")
			if !tc.missing {
				if err := os.Mkdir(root, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, tc.contents, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			for _, args := range [][]string{nil, {"--run", "missing-run"}} {
				out, err := query(t, root, args...)
				if err == nil || !strings.Contains(out, "error:") || strings.Contains(out, "runs_on_current_branch:") || (!tc.missing && strings.Contains(out, "not found")) {
					t.Fatalf("invalid database must report a read error, not no-run: %v\n%s", err, out)
				}
				if tc.missing {
					if _, err := os.Stat(root); !os.IsNotExist(err) {
						t.Fatalf("status created state directories: %v", err)
					}
				}
			}
			if !tc.missing {
				after, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(after, tc.contents) {
					t.Fatalf("status changed invalid database: %v", err)
				}
			}
		})
	}
}
