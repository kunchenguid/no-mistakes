package cli

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// Exercise the shipped process, including command registration and flags, with
// private storage and no daemon, forge, provider, or live repository.
func TestExportCLI(t *testing.T) {
	_, source, _, _ := runtime.Caller(0)
	binary := filepath.Join(t.TempDir(), "no-mistakes")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	build := exec.Command("go", "build", "-ldflags", "-X github.com/kunchenguid/no-mistakes/internal/buildinfo.Version=v1.0.0", "-o", binary, "./cmd/no-mistakes")
	build.Dir = filepath.Clean(filepath.Join(filepath.Dir(source), "../.."))
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build export CLI: %v\n%s", err, out)
	}
	invoke := func(home string, args ...string) (string, string, error) {
		t.Helper()
		cmd := exec.Command(binary, args...)
		// A release build with update checks enabled must still stay local.
		for _, entry := range os.Environ() {
			if !strings.HasPrefix(entry, "NM_HOME=") && !strings.HasPrefix(entry, "NO_MISTAKES_NO_UPDATE_CHECK=") {
				cmd.Env = append(cmd.Env, entry)
			}
		}
		cmd.Env = append(cmd.Env, "NM_HOME="+home, "NO_MISTAKES_NO_UPDATE_CHECK=0")
		cmd.Dir = t.TempDir()
		var out, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &stderr
		err := cmd.Run()
		return out.String(), stderr.String(), err
	}

	t.Run("stored facts and privacy", func(t *testing.T) {
		home := t.TempDir()
		path := paths.WithRoot(home).DB()
		d, err := db.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := d.Close(); err != nil {
			t.Fatal(err)
		}
		fixture, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		// Constants are stored facts, deliberately inconsistent with derived
		// elapsed times so an exporter cannot silently recompute durations.
		_, err = fixture.Exec(`
INSERT INTO repos(id, working_path, upstream_url, default_branch, created_at) VALUES
 ('repo-a', '/private/unrelated-repo-path', 'https://user:synthetic-credential@example.com/acme/project.git?token=secret', 'main', 100),
 ('repo-b', '/private/other-repo', 'git@example.com:other/private.git', 'main', 101);
INSERT INTO runs(id, repo_id, branch, head_sha, base_sha, submitted_head_sha, status, pr_url, created_at, updated_at, parked_ms, awaiting_agent_since, intent, error, evidence_dir, ci_ready_at) VALUES
 ('run-a', 'repo-a', 'feature/export', 'head-a', 'base-a', 'submitted-a', 'running', 'https://example.com/acme/project/pull/7', 200, 900, 12345, 880, 'SENTINEL_PROMPT', 'SENTINEL_TERMINAL', '/private/evidence-path', 870),
 ('run-b', 'repo-b', 'unrelated-branch', 'unrelated-head', 'base-b', NULL, 'completed', NULL, 201, 901, 0, NULL, 'OTHER_RUN_SECRET', NULL, NULL, NULL);
INSERT INTO step_results(id, run_id, step_name, step_order, status, exit_code, duration_ms, started_at, completed_at, response_origin, log_path, findings_json, error) VALUES
 ('review-a', 'run-a', 'review', 1, 'completed', 0, 111, 300, 500, 'interactive', '/private/raw-log', 'SENTINEL_FINDINGS', 'SENTINEL_TERMINAL'),
 ('ci-a', 'run-a', 'ci', 8, 'awaiting_approval', NULL, 222, 600, NULL, 'auto_yes', NULL, NULL, NULL),
 ('review-b', 'run-b', 'review', 1, 'completed', 0, 1, 301, 501, 'unknown', NULL, NULL, NULL);
INSERT INTO step_rounds(id, step_result_id, round, trigger_type, duration_ms, created_at, selection_source, selected_finding_ids, response_origin, reviewed_head_sha, repair_published, findings_json, fix_summary, global_config_yaml) VALUES
 ('round-a1', 'review-a', 1, 'initial', 17, 310, 'user_declined', '[]', 'auto_yes', 'reviewed-a', 0, 'SENTINEL_FINDINGS', 'SENTINEL_PROMPT', 'SYNTHETIC_API_KEY'),
 ('round-a2', 'review-a', 2, 'answer', 19, 410, 'user', '["finding-a"]', 'interactive', NULL, 0, NULL, NULL, NULL),
 ('round-a3', 'review-a', 3, 'initial', 21, 460, NULL, '', 'unknown', NULL, 0, NULL, NULL, NULL),
 ('round-ci', 'ci-a', 2, 'auto_fix', 23, 650, 'auto_fix', '["ci-finding"]', 'unknown', NULL, 1, NULL, NULL, NULL),
 ('round-b', 'review-b', 1, 'initial', 29, 311, NULL, NULL, 'unknown', NULL, 0, NULL, NULL, NULL);
INSERT INTO agent_invocations(id, run_id, step_name, round, purpose, agent, model, session_mode, started_at, completed_at, duration_ms, exit_status, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, fresh_input_tokens, reasoning_tokens, delta_input_tokens, delta_output_tokens, delta_cache_read_tokens, subprocess_wait_ms, session_key) VALUES
 ('inv-ok', 'run-a', 'review', 1, 'review', 'codex', 'served-model', 'cold', 320, 350, 1234, 'ok', 100, 20, 30, 40, 70, 10, 100, 20, 30, 555, 'PRIVATE_SESSION'),
 ('inv-error', 'run-a', 'review', 2, 'review', 'codex', NULL, 'cold', 420, 450, 2345, 'error', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 'PRIVATE_SESSION'),
 ('inv-cancel', 'run-a', 'ci', 2, 'ci-fix', 'codex', '', 'cold', 660, 680, 3456, 'cancelled', 0, 0, 0, NULL, 0, NULL, 0, 0, 0, 0, NULL),
 ('inv-other', 'run-b', 'review', 1, 'review', 'codex', 'OTHER_RUN_SECRET', 'cold', 321, 351, 9876, 'ok', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL);
UPDATE agent_invocations SET model_provider = 'openai' WHERE id = 'inv-ok';
UPDATE agent_invocations SET failure_category = 'exit' WHERE id = 'inv-error';
UPDATE agent_invocations SET failure_category = 'cancelled' WHERE id = 'inv-cancel';
`)
		if err != nil {
			fixture.Close()
			t.Fatal(err)
		}
		if err := fixture.Close(); err != nil {
			t.Fatal(err)
		}
		records, err := db.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		sourceStep, err := records.GetStepResult("review-a")
		if err != nil || sourceStep == nil {
			records.Close()
			t.Fatalf("source step: %v", err)
		}
		var retryRuns []*db.Run
		var carriedSteps []*db.StepResult
		// A second retry must still name the original work, whose rounds and
		// invocations remain on run-a, rather than an intermediate carried row.
		for _, branch := range []string{"feature/retry", "feature/retry-again"} {
			retry, err := records.InsertRun("repo-a", branch, "retry-head", "base-a")
			if err != nil {
				records.Close()
				t.Fatal(err)
			}
			carried, err := records.InsertCarriedStepResult(retry.ID, sourceStep)
			if err != nil {
				records.Close()
				t.Fatal(err)
			}
			retryRuns = append(retryRuns, retry)
			carriedSteps = append(carriedSteps, carried)
			sourceStep = carried
		}
		if err := records.Close(); err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		out, stderr, err := invoke(home, "export", "--run", "run-a", "--json")
		if err != nil {
			t.Fatalf("export: %v\n%s", err, stderr)
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("export JSON: %v\n%s", err, out)
		}
		if got["schema_version"] != float64(1) {
			t.Fatalf("schema_version = %v", got["schema_version"])
		}
		check := func(label string, value any, expected string) {
			t.Helper()
			var want any
			if err := json.Unmarshal([]byte(expected), &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(value, want) {
				t.Fatalf("%s = %#v, want %#v", label, value, want)
			}
		}
		check("run", got["run"], `{"id":"run-a","repo_id":"repo-a","branch":"feature/export","head_sha":"head-a","base_sha":"base-a","submitted_head_sha":"submitted-a","pr_url":"https://example.com/acme/project/pull/7","status":"running","created_at":200,"updated_at":900,"parked_ms":12345,"awaiting_agent_since":880,"ci_ready_at":870}`)
		steps := got["steps"].([]any)
		if len(steps) != 2 {
			t.Fatalf("steps = %v", steps)
		}
		review, ci := steps[0].(map[string]any), steps[1].(map[string]any)
		if review["carried_from"] != nil || ci["carried_from"] != nil {
			t.Fatal("executed steps must not claim a carried source")
		}
		check("review timing", []any{review["id"], review["name"], review["status"], review["started_at"], review["completed_at"], review["duration_ms"], review["response_origin"]}, `["review-a","review","completed",300,500,111,"interactive"]`)
		check("CI timing", []any{ci["id"], ci["name"], ci["status"], ci["started_at"], ci["completed_at"], ci["duration_ms"], ci["response_origin"], ci["ci_attempt"]}, `["ci-a","ci","awaiting_approval",600,null,222,"auto_yes",null]`)
		rounds := review["rounds"].([]any)
		if len(rounds) != 3 {
			t.Fatalf("rounds = %v", rounds)
		}
		check("first round", rounds[0], `{"id":"round-a1","round":1,"trigger":"initial","created_at":310,"duration_ms":17,"reviewed_head_sha":"reviewed-a","selection_source":"user_declined","selected_finding_ids":[],"response_origin":"auto_yes","repair_published":false,"gate":{"first_parked_at":null,"reason":null,"owner":null,"notified_at":null,"escalated_at":null,"responded_at":null,"responder_identity":null,"action":null,"resolution_evidence":null}}`)
		second := rounds[1].(map[string]any)
		check("answer round", []any{second["id"], second["round"], second["trigger"], second["created_at"], second["duration_ms"], second["response_origin"], second["selection_source"], second["selected_finding_ids"]}, `["round-a2",2,"answer",410,19,"interactive","user",["finding-a"]]`)
		third := rounds[2].(map[string]any)
		check("unrecorded selection", []any{third["selected_finding_ids"], third["selection_source"], third["response_origin"]}, `[null,null,"unknown"]`)
		ciRound := ci["rounds"].([]any)[0].(map[string]any)
		check("local CI repair binding", []any{ciRound["id"], ciRound["round"], ciRound["trigger"], ciRound["created_at"], ciRound["duration_ms"], ciRound["repair_published"]}, `["round-ci",2,"auto_fix",650,23,true]`)
		invs := got["invocations"].([]any)
		if len(invs) != 3 {
			t.Fatalf("invocations = %v", invs)
		}
		for i, expected := range []string{
			`["inv-ok","review",1,320,350,1234,"ok",100,20,30,40,70,10,100,20,30,555]`,
			`["inv-error","review",2,420,450,2345,"error",null,null,null,null,null,null,null,null,null,null]`,
			`["inv-cancel","ci",2,660,680,3456,"cancelled",0,0,0,null,0,null,0,0,0,0]`,
		} {
			inv := invs[i].(map[string]any)
			check("invocation facts", []any{inv["id"], inv["step_name"], inv["round"], inv["started_at"], inv["completed_at"], inv["duration_ms"], inv["exit_status"], inv["input_tokens"], inv["output_tokens"], inv["cache_read_tokens"], inv["cache_creation_tokens"], inv["fresh_input_tokens"], inv["reasoning_tokens"], inv["delta_input_tokens"], inv["delta_output_tokens"], inv["delta_cache_read_tokens"], inv["subprocess_wait_ms"]}, expected)
			metadata := []string{`["review","codex","served-model","openai","cold",null]`, `["review","codex",null,null,"cold","exit"]`, `["ci-fix","codex",null,null,"cold","cancelled"]`}
			check("invocation identity", []any{inv["purpose"], inv["agent"], inv["model"], inv["model_provider"], inv["session_mode"], inv["failure_category"]}, metadata[i])
		}
		for _, secret := range []string{"SENTINEL", "SYNTHETIC_API_KEY", "synthetic-credential", "secret", "/private/", "PRIVATE_SESSION", "OTHER_RUN", "unrelated-", "run-b", "repo-b"} {
			if strings.Contains(out, secret) {
				t.Fatalf("export leaked %q", secret)
			}
		}
		again, stderr, err := invoke(home, "export", "--run", "run-a", "--json")
		if err != nil || again != out {
			t.Fatalf("repeatability: %v\n%s\n%s", err, stderr, again)
		}
		if stdout, _, err := invoke(home, "export", "--run", "missing", "--json"); err == nil || stdout != "" {
			t.Fatalf("missing run returned %q, %v", stdout, err)
		}
		for i, retry := range retryRuns {
			retryOut, stderr, err := invoke(home, "export", "--run", retry.ID, "--json")
			if err != nil {
				t.Fatalf("export retry: %v\n%s", err, stderr)
			}
			var retryDoc map[string]any
			if err := json.Unmarshal([]byte(retryOut), &retryDoc); err != nil {
				t.Fatal(err)
			}
			if retryDoc["run"].(map[string]any)["id"] != retry.ID {
				t.Fatal("retry export changed the requested run")
			}
			retrySteps := retryDoc["steps"].([]any)
			if len(retrySteps) != 1 {
				t.Fatalf("retry steps = %v", retrySteps)
			}
			step := retrySteps[0].(map[string]any)
			if step["id"] != carriedSteps[i].ID {
				t.Fatalf("carried step ID = %v, want %s", step["id"], carriedSteps[i].ID)
			}
			check("carried source", step["carried_from"], `{"run_id":"run-a","step_id":"review-a"}`)
			check("carried timing", []any{step["status"], step["started_at"], step["completed_at"], step["duration_ms"]}, `["completed",300,500,111]`)
			check("carried rounds remain on source", step["rounds"], `[]`)
			check("carried invocations remain on source", retryDoc["invocations"], `[]`)
			for _, secret := range []string{"SENTINEL", "PRIVATE_SESSION", "OTHER_RUN", "inv-ok", "round-a1", "/private/"} {
				if strings.Contains(retryOut, secret) {
					t.Fatalf("retry export leaked source payload %q", secret)
				}
			}
		}
		// A read-only WAL reader can create -wal/-shm sidecars. Checkpoint
		// after closing the exporter so any record writes hidden in WAL would
		// reach the main file and fail the byte comparison as well.
		checkpoint, err := sql.Open("sqlite", "file:"+path+"?mode=rw")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := checkpoint.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
			checkpoint.Close()
			t.Fatal(err)
		}
		if err := checkpoint.Close(); err != nil {
			t.Fatal(err)
		}
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatalf("export mutated DB: %v", err)
		}
		entries, err := os.ReadDir(home)
		if err != nil || len(entries) != 1 || entries[0].Name() != "state.sqlite" {
			t.Fatalf("export created runtime storage: %v, %v", entries, err)
		}
	})

	for _, kind := range []string{"missing database", "damaged database", "old schema"} {
		t.Run(kind, func(t *testing.T) {
			home := filepath.Join(t.TempDir(), "nm")
			path := paths.WithRoot(home).DB()
			if kind != "missing database" {
				if err := os.Mkdir(home, 0700); err != nil {
					t.Fatal(err)
				}
				if kind == "damaged database" {
					if err := os.WriteFile(path, []byte("damaged sqlite"), 0600); err != nil {
						t.Fatal(err)
					}
				} else {
					s, err := sql.Open("sqlite", path)
					if err != nil {
						t.Fatal(err)
					}
					_, err = s.Exec("CREATE TABLE sentinel(id TEXT)")
					s.Close()
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			before, _ := os.ReadFile(path)
			out, _, err := invoke(home, "export", "--run", "run-a", "--json")
			if err == nil || out != "" {
				t.Fatalf("expected failure without document: %q, %v", out, err)
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(before, after) {
				t.Fatal("export created or migrated storage")
			}
			if kind == "missing database" {
				if _, err := os.Stat(home); !os.IsNotExist(err) {
					t.Fatalf("export created NM_HOME: %v", err)
				}
			}
		})
	}
	for _, args := range [][]string{{"export"}, {"export", "--run", "run-a"}, {"export", "--json"}, {"export", "--run", "run-a", "--json", "extra"}} {
		if out, _, err := invoke(filepath.Join(t.TempDir(), "missing"), args...); err == nil || out != "" {
			t.Fatalf("invalid args %v: %q, %v", args, out, err)
		}
	}
}
