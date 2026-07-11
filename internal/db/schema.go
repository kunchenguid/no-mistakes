package db

const schemaSQL = `
CREATE TABLE IF NOT EXISTS repos (
    id             TEXT PRIMARY KEY,
    working_path   TEXT NOT NULL UNIQUE,
    upstream_url   TEXT NOT NULL,
    fork_url       TEXT,
    default_branch TEXT NOT NULL DEFAULT 'main',
    created_at     INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS runs (
    id                   TEXT PRIMARY KEY,
    repo_id              TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    branch               TEXT NOT NULL,
    head_sha             TEXT NOT NULL,
    base_sha             TEXT NOT NULL,
    status               TEXT NOT NULL DEFAULT 'pending',
    pr_url               TEXT,
    error                TEXT,
    awaiting_agent_since INTEGER,
    parked_ms            INTEGER,
    created_at           INTEGER NOT NULL,
    updated_at           INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS run_completion_order (
    sequence INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id   TEXT NOT NULL UNIQUE REFERENCES runs(id) ON DELETE CASCADE
);

CREATE TRIGGER IF NOT EXISTS runs_completion_order_after_insert
AFTER INSERT ON runs
WHEN NEW.status = 'completed'
BEGIN
    INSERT OR IGNORE INTO run_completion_order (run_id) VALUES (NEW.id);
END;

CREATE TRIGGER IF NOT EXISTS runs_completion_order_after_update
AFTER UPDATE OF status ON runs
WHEN NEW.status = 'completed' AND OLD.status <> 'completed'
BEGIN
    INSERT OR IGNORE INTO run_completion_order (run_id) VALUES (NEW.id);
END;

CREATE TABLE IF NOT EXISTS step_results (
    id               TEXT PRIMARY KEY,
    run_id           TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    step_name        TEXT NOT NULL,
    step_order       INTEGER NOT NULL,
    status           TEXT NOT NULL DEFAULT 'pending',
    exit_code        INTEGER,
    duration_ms      INTEGER,
    log_path         TEXT,
    findings_json    TEXT,
    error            TEXT,
    started_at       INTEGER,
    completed_at     INTEGER,
    last_activity_at INTEGER,
    last_activity    TEXT,
    agent_pid        INTEGER,
    auto_fix_limit   INTEGER
);

CREATE TABLE IF NOT EXISTS step_rounds (
    id                   TEXT PRIMARY KEY,
    step_result_id       TEXT NOT NULL REFERENCES step_results(id) ON DELETE CASCADE,
    round                INTEGER NOT NULL,
    trigger_type         TEXT NOT NULL,
    findings_json        TEXT,
    user_findings_json   TEXT,
    selected_finding_ids TEXT,
    selection_source     TEXT,
    fix_summary          TEXT,
    state                TEXT NOT NULL DEFAULT 'completed',
    started_at           INTEGER,
    completed_at         INTEGER,
    duration_ms          INTEGER NOT NULL,
    created_at           INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS agent_invocations (
    id                    TEXT PRIMARY KEY,
    run_id                TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    step_name             TEXT NOT NULL,
    round                 INTEGER NOT NULL,
    purpose               TEXT NOT NULL,
    agent                 TEXT NOT NULL,
    model                 TEXT,
    session_mode          TEXT NOT NULL,
    session_key           TEXT,
    started_at            INTEGER NOT NULL,
    completed_at          INTEGER NOT NULL,
    duration_ms           INTEGER NOT NULL,
    exit_status           TEXT NOT NULL,
    failure_category      TEXT,
    input_tokens          INTEGER,
    output_tokens         INTEGER,
    cache_read_tokens     INTEGER,
    cache_creation_tokens INTEGER
);

CREATE INDEX IF NOT EXISTS idx_agent_invocations_run_started_id
    ON agent_invocations (run_id, started_at, id);

CREATE TABLE IF NOT EXISTS run_agent_sessions (
    run_id     TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    role       TEXT NOT NULL,
    agent      TEXT NOT NULL,
    session_id TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (run_id, role)
);

CREATE UNIQUE INDEX IF NOT EXISTS step_rounds_step_round_unique
    ON step_rounds(step_result_id, round);

CREATE TABLE IF NOT EXISTS utility_scopes (
    id         TEXT PRIMARY KEY,
    kind       TEXT NOT NULL,
    owner_pid  INTEGER NOT NULL,
    created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS invocation_attempt_starts (
    id               TEXT PRIMARY KEY,
    purpose          TEXT NOT NULL,
    role             TEXT NOT NULL,
    scope_kind       TEXT NOT NULL,
    run_id           TEXT,
    step_result_id   TEXT,
    step_round_id    TEXT,
    utility_scope_id TEXT,
    candidate_key    TEXT NOT NULL,
    profile          TEXT,
    tier             INTEGER,
    candidate_index  INTEGER,
    runner           TEXT,
    model            TEXT,
    effort           TEXT,
    started_at       INTEGER NOT NULL,
    CHECK (
        (scope_kind = 'pipeline' AND run_id IS NOT NULL AND step_result_id IS NOT NULL AND step_round_id IS NOT NULL AND utility_scope_id IS NULL)
        OR
        (scope_kind = 'utility' AND run_id IS NULL AND step_result_id IS NULL AND step_round_id IS NULL AND utility_scope_id IS NOT NULL)
    )
);

CREATE TABLE IF NOT EXISTS run_seals (
    id         TEXT PRIMARY KEY,
    run_id     TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    sha        TEXT NOT NULL,
    reason     TEXT NOT NULL,
    sealed_at  INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS run_seals_run_idx ON run_seals(run_id, sealed_at);

CREATE TABLE IF NOT EXISTS invocation_attempt_terminals (
    attempt_id            TEXT PRIMARY KEY REFERENCES invocation_attempt_starts(id) ON DELETE CASCADE,
    outcome               TEXT NOT NULL,
    failure_domain        TEXT,
    terminal_at           INTEGER NOT NULL,
    duration_ms           INTEGER NOT NULL,
    input_tokens          INTEGER NOT NULL DEFAULT 0,
    output_tokens         INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens     INTEGER NOT NULL DEFAULT 0,
    cache_creation_tokens INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS finding_lineages (
    id                TEXT PRIMARY KEY,
    run_id            TEXT NOT NULL,
    origin_attempt_id TEXT NOT NULL,
    display_id        TEXT,
    sequence          INTEGER NOT NULL,
    created_at        INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS finding_lineages_run ON finding_lineages(run_id);
CREATE INDEX IF NOT EXISTS finding_lineages_attempt ON finding_lineages(origin_attempt_id);

CREATE TABLE IF NOT EXISTS intent_cache (
    cache_key   TEXT PRIMARY KEY,
    summary     TEXT NOT NULL,
    agent_name  TEXT NOT NULL,
    session_id  TEXT NOT NULL,
    created_at  INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS finding_repairs (
    id               TEXT PRIMARY KEY,
    run_id           TEXT NOT NULL,
    lineage_id       TEXT NOT NULL,
    step_result_id   TEXT NOT NULL,
    step_round_id    TEXT NOT NULL,
    severity         TEXT NOT NULL,
    action           TEXT NOT NULL,
    description      TEXT NOT NULL,
    file             TEXT,
    line             INTEGER,
    tier             INTEGER NOT NULL,
    remaining_budget INTEGER NOT NULL,
    fixer_attempt_id    TEXT,
    verifier_attempt_id TEXT,
    verdict          TEXT,
    verdict_rationale TEXT,
    status           TEXT NOT NULL DEFAULT 'pending',
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS finding_repairs_run ON finding_repairs(run_id);
CREATE INDEX IF NOT EXISTS finding_repairs_lineage ON finding_repairs(lineage_id);

CREATE TABLE IF NOT EXISTS finding_repair_checks (
    id             TEXT PRIMARY KEY,
    repair_id      TEXT NOT NULL REFERENCES finding_repairs(id) ON DELETE CASCADE,
    command        TEXT NOT NULL,
    applicable     INTEGER NOT NULL,
    exit_code      INTEGER NOT NULL,
    output_excerpt TEXT,
    ran_at         INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS finding_repair_checks_repair ON finding_repair_checks(repair_id);

CREATE TABLE IF NOT EXISTS canary_activation (
    id               INTEGER PRIMARY KEY CHECK (id = 1),
    activated_at     INTEGER NOT NULL,
    fingerprint      TEXT NOT NULL DEFAULT '',
    completion_fence INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS canary_cohort_runs (
    cohort           TEXT NOT NULL,
    position         INTEGER NOT NULL,
    run_id           TEXT NOT NULL,
    completed_at     INTEGER NOT NULL,
    execution_ms     INTEGER NOT NULL,
    invocation_ms    INTEGER NOT NULL,
    escalations      INTEGER NOT NULL,
    failovers        INTEGER NOT NULL,
    changed_files    INTEGER NOT NULL,
    changed_lines    INTEGER NOT NULL,
    initial_findings INTEGER NOT NULL,
    created_at       INTEGER NOT NULL,
    PRIMARY KEY (cohort, position)
);

CREATE UNIQUE INDEX IF NOT EXISTS canary_cohort_run_unique
    ON canary_cohort_runs(cohort, run_id);
`

// migrationStatements hold additive schema changes applied to databases that
// were created before the referenced columns existed. Each statement must be
// idempotent via its error being tolerated when the column already exists.
var migrationStatements = []string{
	`ALTER TABLE repos ADD COLUMN fork_url TEXT`,
	`ALTER TABLE step_rounds ADD COLUMN selected_finding_ids TEXT`,
	`ALTER TABLE step_rounds ADD COLUMN selection_source TEXT`,
	`ALTER TABLE step_rounds ADD COLUMN fix_summary TEXT`,
	`ALTER TABLE step_rounds ADD COLUMN user_findings_json TEXT`,
	`ALTER TABLE step_rounds ADD COLUMN state TEXT NOT NULL DEFAULT 'completed'`,
	`ALTER TABLE step_rounds ADD COLUMN started_at INTEGER`,
	`ALTER TABLE step_rounds ADD COLUMN completed_at INTEGER`,
	`ALTER TABLE runs ADD COLUMN intent TEXT`,
	`ALTER TABLE runs ADD COLUMN intent_source TEXT`,
	`ALTER TABLE runs ADD COLUMN intent_session_id TEXT`,
	`ALTER TABLE runs ADD COLUMN intent_score REAL`,
	`ALTER TABLE invocation_attempt_starts ADD COLUMN profile TEXT`,
	`ALTER TABLE invocation_attempt_starts ADD COLUMN tier INTEGER`,
	`ALTER TABLE invocation_attempt_starts ADD COLUMN candidate_index INTEGER`,
	`ALTER TABLE invocation_attempt_starts ADD COLUMN runner TEXT`,
	`ALTER TABLE invocation_attempt_starts ADD COLUMN model TEXT`,
	`ALTER TABLE invocation_attempt_starts ADD COLUMN effort TEXT`,
	`ALTER TABLE runs ADD COLUMN awaiting_agent_since INTEGER`,
	`ALTER TABLE runs ADD COLUMN parked_ms INTEGER`,
	`ALTER TABLE step_results ADD COLUMN last_activity_at INTEGER`,
	`ALTER TABLE step_results ADD COLUMN last_activity TEXT`,
	`ALTER TABLE step_results ADD COLUMN agent_pid INTEGER`,
	`ALTER TABLE step_results ADD COLUMN auto_fix_limit INTEGER`,
	`ALTER TABLE canary_activation ADD COLUMN completion_fence INTEGER NOT NULL DEFAULT -1`,
	`INSERT OR IGNORE INTO run_completion_order (run_id)
		SELECT r.id
		FROM runs r
		WHERE r.status = 'completed'
		  AND NOT EXISTS (SELECT 1 FROM run_completion_order c WHERE c.run_id = r.id)
		ORDER BY r.updated_at, r.id`,
	`UPDATE canary_activation
		SET completion_fence = COALESCE((SELECT MAX(sequence) FROM run_completion_order), 0)
		WHERE completion_fence = -1`,
}
