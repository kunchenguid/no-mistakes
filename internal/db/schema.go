package db

const schemaSQL = `
CREATE TABLE IF NOT EXISTS repos (
    id             TEXT PRIMARY KEY,
    working_path   TEXT NOT NULL UNIQUE,
    upstream_url   TEXT NOT NULL,
    fork_url       TEXT,
    default_branch TEXT NOT NULL DEFAULT 'main',
    url_version    INTEGER NOT NULL DEFAULT 0,
    created_at     INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS receive_sessions (
    id                    TEXT PRIMARY KEY,
    repo_id               TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    gate_path             TEXT NOT NULL,
    capability_hash      TEXT NOT NULL,
    state                 TEXT NOT NULL DEFAULT 'active',
    phase                 TEXT NOT NULL DEFAULT 'issued',
    batch_hash            TEXT NOT NULL DEFAULT '',
    created_at            INTEGER NOT NULL,
    updated_at            INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS receive_sessions_active
    ON receive_sessions (repo_id, id, state);

CREATE TABLE IF NOT EXISTS receive_reservations (
    id          TEXT PRIMARY KEY,
    repo_id     TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    gate_path   TEXT NOT NULL,
    branch      TEXT NOT NULL,
    ref         TEXT NOT NULL,
    old_sha     TEXT NOT NULL,
    new_sha     TEXT NOT NULL,
    receive_session_id TEXT,
    receive_capability_hash TEXT,
    skip_steps  TEXT,
    intent      TEXT,
    state       TEXT NOT NULL DEFAULT 'reserved',
    run_id      TEXT,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS receive_reservations_pending_branch
    ON receive_reservations (repo_id, branch, state, created_at, id);

CREATE TABLE IF NOT EXISTS runs (
    id                   TEXT PRIMARY KEY,
    repo_id              TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    branch               TEXT NOT NULL,
    head_sha                TEXT NOT NULL,
    base_sha                TEXT NOT NULL,
    submitted_head_sha      TEXT,
    receive_reservation_id  TEXT,
    review_approved_head_sha TEXT,
    status                  TEXT NOT NULL DEFAULT 'pending',
    pr_url                  TEXT,
    pr_state                TEXT,
    pr_state_observed_at    INTEGER,
    ci_ready_at             INTEGER,
    ci_ready_no_ci          INTEGER NOT NULL DEFAULT 0,
    last_pushed_sha         TEXT,
    push_target_kind        TEXT,
    push_target_fingerprint TEXT,
    push_ref                TEXT,
    last_pushed_at          INTEGER,
    push_generation         INTEGER,
    push_active             INTEGER NOT NULL DEFAULT 0,
	terminal_head_verified_at INTEGER,
	publication_journal_state TEXT,
	publication_journal_target_kind TEXT,
	publication_journal_target_fingerprint TEXT,
	publication_journal_ref TEXT,
	publication_journal_target_version INTEGER,
	publication_attempt_head_sha TEXT,
	publication_attempt_target_kind TEXT,
	publication_attempt_target_fingerprint TEXT,
	publication_attempt_ref TEXT,
	custody_transition_phase TEXT,
    error                   TEXT,
    awaiting_agent_since INTEGER,
    parked_ms            INTEGER,
    created_at           INTEGER NOT NULL,
    updated_at           INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS run_publication_targets (
    run_id             TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    target_kind        TEXT NOT NULL,
    target_fingerprint TEXT NOT NULL,
    ref                TEXT NOT NULL,
    target_version     INTEGER NOT NULL,
    state              TEXT NOT NULL DEFAULT 'no_attempt' CHECK (state IN ('no_attempt', 'attempted', 'published', 'ambiguous')),
    request_identity   TEXT,
    attempt_head_sha   TEXT,
    generation         INTEGER NOT NULL DEFAULT 0,
    provenance         TEXT NOT NULL DEFAULT '',
    created_at         INTEGER NOT NULL,
    updated_at         INTEGER NOT NULL,
    PRIMARY KEY (run_id, target_fingerprint)
);

CREATE INDEX IF NOT EXISTS run_publication_targets_active
    ON run_publication_targets (run_id, state, target_fingerprint);

CREATE TABLE IF NOT EXISTS run_publication_target_sets (
    run_id          TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
    target_count    INTEGER NOT NULL,
    target_set_hash TEXT NOT NULL,
    state           TEXT NOT NULL CHECK (state IN ('complete', 'ambiguous')),
    generation      INTEGER NOT NULL DEFAULT 0,
    provenance      TEXT NOT NULL,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL
);

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
    reviewed_head_sha    TEXT,
    user_findings_json   TEXT,
    selected_finding_ids TEXT,
    selection_source     TEXT,
    fix_summary          TEXT,
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
    model_provider        TEXT,
    session_mode          TEXT NOT NULL,
    session_key           TEXT,
    fallback_reason       TEXT,
    started_at            INTEGER NOT NULL,
    completed_at          INTEGER NOT NULL,
    duration_ms           INTEGER NOT NULL,
    subprocess_wait_ms    INTEGER,
    exit_status           TEXT NOT NULL,
    failure_category      TEXT,
    input_tokens          INTEGER,
    output_tokens         INTEGER,
    cache_read_tokens     INTEGER,
    cache_creation_tokens INTEGER,
    fresh_input_tokens    INTEGER,
    reasoning_tokens      INTEGER,
    delta_input_tokens    INTEGER,
    delta_output_tokens   INTEGER,
    delta_cache_read_tokens INTEGER,
    model_roundtrips      INTEGER,
    tool_calls            INTEGER,
    tool_wait_calls       INTEGER,
    tool_test_lint_calls  INTEGER,
    tool_edit_calls       INTEGER,
    tool_read_calls       INTEGER,
    tool_git_calls        INTEGER,
    tool_other_calls      INTEGER,
    workload_files        INTEGER,
    workload_lines        INTEGER,
    finding_count         INTEGER
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

CREATE TABLE IF NOT EXISTS intent_cache (
    cache_key   TEXT PRIMARY KEY,
    summary     TEXT NOT NULL,
    agent_name  TEXT NOT NULL,
    session_id  TEXT NOT NULL,
    created_at  INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS internal_ref_mutations (
    id                 TEXT PRIMARY KEY,
    repo_id            TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    gate_path          TEXT NOT NULL,
    branch             TEXT NOT NULL,
    ref                TEXT NOT NULL,
    old_sha            TEXT NOT NULL,
    new_sha            TEXT NOT NULL,
    operation          TEXT NOT NULL,
    scope              TEXT NOT NULL,
    capability_hash    TEXT NOT NULL UNIQUE,
    authority_endpoint TEXT NOT NULL,
    state              TEXT NOT NULL DEFAULT 'issued',
    created_at         INTEGER NOT NULL,
    updated_at         INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS internal_ref_mutations_active
    ON internal_ref_mutations (repo_id, gate_path, state, created_at);

CREATE TABLE IF NOT EXISTS gate_ref_locks (
    run_id           TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
    repo_id          TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    gate_path        TEXT NOT NULL,
    branch           TEXT NOT NULL,
    ref              TEXT NOT NULL,
    lock_path        TEXT NOT NULL,
    owner_generation TEXT NOT NULL,
	authority_endpoint TEXT NOT NULL,
	expected_head    TEXT NOT NULL,
	new_head         TEXT NOT NULL DEFAULT '',
	file_identity    TEXT NOT NULL,
    state            TEXT NOT NULL DEFAULT 'prepared',
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS gate_ref_locks_active
    ON gate_ref_locks (repo_id, gate_path, state, updated_at);

CREATE TABLE IF NOT EXISTS gate_ref_quarantines (
    repo_id       TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    gate_path     TEXT NOT NULL,
    ref           TEXT NOT NULL,
    expected_head TEXT NOT NULL,
    observed_head TEXT NOT NULL,
    reason        TEXT NOT NULL,
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL,
    PRIMARY KEY (repo_id, gate_path, ref)
);

CREATE INDEX IF NOT EXISTS gate_ref_quarantines_active
    ON gate_ref_quarantines (repo_id, gate_path, updated_at);

CREATE TABLE IF NOT EXISTS managed_gate_refs (
    repo_id    TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    gate_path  TEXT NOT NULL,
    ref        TEXT NOT NULL,
    head       TEXT NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (repo_id, gate_path, ref)
);

CREATE INDEX IF NOT EXISTS managed_gate_refs_active
    ON managed_gate_refs (repo_id, gate_path, updated_at);

CREATE TABLE IF NOT EXISTS custody_ref_stages (
    run_id             TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
    repo_id            TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    gate_path          TEXT NOT NULL,
    branch             TEXT NOT NULL,
    ref                TEXT NOT NULL,
    old_sha            TEXT NOT NULL,
    new_sha            TEXT NOT NULL,
    owner_generation   TEXT NOT NULL,
    authority_endpoint TEXT NOT NULL,
    state              TEXT NOT NULL DEFAULT 'prepared',
    created_at         INTEGER NOT NULL,
    updated_at         INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS custody_ref_stages_active
    ON custody_ref_stages (repo_id, gate_path, state, updated_at);

CREATE TABLE IF NOT EXISTS recovery_anchor_stages (
    run_id             TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
    repo_id            TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    gate_path          TEXT NOT NULL,
    branch             TEXT NOT NULL,
    ref                TEXT NOT NULL,
    old_sha            TEXT NOT NULL,
    new_sha            TEXT NOT NULL,
    owner_generation   TEXT NOT NULL,
    authority_endpoint TEXT NOT NULL,
    state              TEXT NOT NULL DEFAULT 'prepared',
    created_at         INTEGER NOT NULL,
    updated_at         INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS recovery_anchor_stages_active
    ON recovery_anchor_stages (repo_id, gate_path, state, updated_at);
`

// migrationStatements hold additive schema changes applied to databases that
// were created before the referenced columns existed. Each statement must be
// idempotent via its error being tolerated when the column already exists.
var migrationStatements = []string{
	`ALTER TABLE repos ADD COLUMN fork_url TEXT`,
	`ALTER TABLE repos ADD COLUMN url_version INTEGER NOT NULL DEFAULT 0`,
	`CREATE TABLE IF NOT EXISTS receive_sessions (id TEXT PRIMARY KEY, repo_id TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE, gate_path TEXT NOT NULL, capability_hash TEXT NOT NULL, state TEXT NOT NULL DEFAULT 'active', phase TEXT NOT NULL DEFAULT 'issued', batch_hash TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`,
	`CREATE INDEX IF NOT EXISTS receive_sessions_active ON receive_sessions (repo_id, id, state)`,
	`ALTER TABLE receive_sessions ADD COLUMN phase TEXT NOT NULL DEFAULT 'issued'`,
	`ALTER TABLE receive_sessions ADD COLUMN batch_hash TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE step_rounds ADD COLUMN selected_finding_ids TEXT`,
	`ALTER TABLE step_rounds ADD COLUMN selection_source TEXT`,
	`ALTER TABLE step_rounds ADD COLUMN fix_summary TEXT`,
	`ALTER TABLE step_rounds ADD COLUMN user_findings_json TEXT`,
	// A parked round may retain the reviewed commit as a non-authoritative
	// candidate. Only atomic review completion promotes it onto the run.
	`ALTER TABLE step_rounds ADD COLUMN reviewed_head_sha TEXT`,
	`ALTER TABLE runs ADD COLUMN intent TEXT`,
	`ALTER TABLE runs ADD COLUMN intent_source TEXT`,
	`ALTER TABLE runs ADD COLUMN intent_session_id TEXT`,
	`ALTER TABLE runs ADD COLUMN intent_score REAL`,
	`ALTER TABLE runs ADD COLUMN awaiting_agent_since INTEGER`,
	`ALTER TABLE runs ADD COLUMN parked_ms INTEGER`,
	// The CI step's per-check rerun budget. It is durable because a run
	// recovered after a daemon restart would otherwise get a fresh budget and
	// could issue reruns beyond the documented limit; the reservation is
	// written before the provider call, so a crash mid-request spends the
	// budget rather than silently granting a free retry.
	`ALTER TABLE runs ADD COLUMN ci_rerun_state TEXT`,
	// Branch synchronization provenance is intentionally nullable. Historical
	// rows stay unbound because mutable head_sha cannot prove a successful push.
	`ALTER TABLE runs ADD COLUMN submitted_head_sha TEXT`,
	`ALTER TABLE runs ADD COLUMN receive_reservation_id TEXT`,
	`CREATE UNIQUE INDEX IF NOT EXISTS runs_receive_reservation ON runs (receive_reservation_id) WHERE receive_reservation_id IS NOT NULL`,
	// Review authority is nullable and never backfilled. A historical mutable
	// head_sha cannot prove which exact commit a completed review approved.
	`ALTER TABLE runs ADD COLUMN review_approved_head_sha TEXT`,
	`ALTER TABLE runs ADD COLUMN last_pushed_sha TEXT`,
	`ALTER TABLE runs ADD COLUMN push_target_kind TEXT`,
	`ALTER TABLE runs ADD COLUMN push_target_fingerprint TEXT`,
	`ALTER TABLE runs ADD COLUMN push_ref TEXT`,
	`ALTER TABLE runs ADD COLUMN last_pushed_at INTEGER`,
	`ALTER TABLE runs ADD COLUMN push_generation INTEGER`,
	`ALTER TABLE runs ADD COLUMN push_active INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE runs ADD COLUMN terminal_head_verified_at INTEGER`,
	`ALTER TABLE runs ADD COLUMN publication_journal_state TEXT`,
	`ALTER TABLE runs ADD COLUMN publication_journal_target_kind TEXT`,
	`ALTER TABLE runs ADD COLUMN publication_journal_target_fingerprint TEXT`,
	`ALTER TABLE runs ADD COLUMN publication_journal_ref TEXT`,
	`ALTER TABLE runs ADD COLUMN publication_journal_target_version INTEGER`,
	`ALTER TABLE runs ADD COLUMN publication_attempt_head_sha TEXT`,
	`ALTER TABLE runs ADD COLUMN publication_attempt_target_kind TEXT`,
	`ALTER TABLE runs ADD COLUMN publication_attempt_target_fingerprint TEXT`,
	`ALTER TABLE runs ADD COLUMN publication_attempt_ref TEXT`,
	`ALTER TABLE runs ADD COLUMN pr_state TEXT`,
	`ALTER TABLE runs ADD COLUMN pr_state_observed_at INTEGER`,
	`ALTER TABLE runs ADD COLUMN ci_ready_at INTEGER`,
	`ALTER TABLE runs ADD COLUMN ci_ready_no_ci INTEGER NOT NULL DEFAULT 0`,
	// Custody return is nullable: NULL means the pipeline still owns any
	// unpublished head this run produced; a timestamp means an explicit
	// guarded recovery ended that ownership (internal/branchsync).
	`ALTER TABLE runs ADD COLUMN custody_returned_at INTEGER`,
	`ALTER TABLE runs ADD COLUMN custody_transition_token TEXT`,
	`ALTER TABLE runs ADD COLUMN custody_transition_phase TEXT`,
	`CREATE TABLE IF NOT EXISTS run_publication_targets (run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE, target_kind TEXT NOT NULL, target_fingerprint TEXT NOT NULL, ref TEXT NOT NULL, target_version INTEGER NOT NULL, state TEXT NOT NULL DEFAULT 'no_attempt' CHECK (state IN ('no_attempt', 'attempted', 'published', 'ambiguous')), request_identity TEXT, attempt_head_sha TEXT, generation INTEGER NOT NULL DEFAULT 0, provenance TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, PRIMARY KEY (run_id, target_fingerprint))`,
	`CREATE INDEX IF NOT EXISTS run_publication_targets_active ON run_publication_targets (run_id, state, target_fingerprint)`,
	`CREATE TABLE IF NOT EXISTS run_publication_target_sets (run_id TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE, target_count INTEGER NOT NULL, target_set_hash TEXT NOT NULL, state TEXT NOT NULL CHECK (state IN ('complete', 'ambiguous')), generation INTEGER NOT NULL DEFAULT 0, provenance TEXT NOT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`,
	`ALTER TABLE receive_reservations ADD COLUMN receive_session_id TEXT`,
	`ALTER TABLE receive_reservations ADD COLUMN receive_capability_hash TEXT`,
	`CREATE UNIQUE INDEX IF NOT EXISTS receive_reservations_session_transition ON receive_reservations (repo_id, receive_session_id, ref, old_sha, new_sha) WHERE receive_session_id IS NOT NULL`,
	`CREATE TABLE IF NOT EXISTS internal_ref_mutations (id TEXT PRIMARY KEY, repo_id TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE, gate_path TEXT NOT NULL, branch TEXT NOT NULL, ref TEXT NOT NULL, old_sha TEXT NOT NULL, new_sha TEXT NOT NULL, operation TEXT NOT NULL, scope TEXT NOT NULL, capability_hash TEXT NOT NULL UNIQUE, authority_endpoint TEXT NOT NULL, state TEXT NOT NULL DEFAULT 'issued', created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`,
	`CREATE INDEX IF NOT EXISTS internal_ref_mutations_active ON internal_ref_mutations (repo_id, gate_path, state, created_at)`,
	`ALTER TABLE internal_ref_mutations ADD COLUMN authority_endpoint TEXT NOT NULL DEFAULT ''`,
	`UPDATE internal_ref_mutations SET state = 'consumed', updated_at = created_at WHERE authority_endpoint = '' AND state IN ('issued', 'prepared')`,
	`CREATE TABLE IF NOT EXISTS gate_ref_locks (run_id TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE, repo_id TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE, gate_path TEXT NOT NULL, branch TEXT NOT NULL, ref TEXT NOT NULL, lock_path TEXT NOT NULL, owner_generation TEXT NOT NULL, authority_endpoint TEXT NOT NULL, expected_head TEXT NOT NULL, new_head TEXT NOT NULL DEFAULT '', file_identity TEXT NOT NULL, state TEXT NOT NULL DEFAULT 'prepared', created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`,
	`CREATE INDEX IF NOT EXISTS gate_ref_locks_active ON gate_ref_locks (repo_id, gate_path, state, updated_at)`,
	`ALTER TABLE gate_ref_locks ADD COLUMN new_head TEXT NOT NULL DEFAULT ''`,
	`CREATE TABLE IF NOT EXISTS gate_ref_quarantines (repo_id TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE, gate_path TEXT NOT NULL, ref TEXT NOT NULL, expected_head TEXT NOT NULL, observed_head TEXT NOT NULL, reason TEXT NOT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, PRIMARY KEY (repo_id, gate_path, ref))`,
	`CREATE INDEX IF NOT EXISTS gate_ref_quarantines_active ON gate_ref_quarantines (repo_id, gate_path, updated_at)`,
	`CREATE TABLE IF NOT EXISTS managed_gate_refs (repo_id TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE, gate_path TEXT NOT NULL, ref TEXT NOT NULL, head TEXT NOT NULL, updated_at INTEGER NOT NULL, PRIMARY KEY (repo_id, gate_path, ref))`,
	`CREATE INDEX IF NOT EXISTS managed_gate_refs_active ON managed_gate_refs (repo_id, gate_path, updated_at)`,
	`CREATE TABLE IF NOT EXISTS custody_ref_stages (run_id TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE, repo_id TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE, gate_path TEXT NOT NULL, branch TEXT NOT NULL, ref TEXT NOT NULL, old_sha TEXT NOT NULL, new_sha TEXT NOT NULL, owner_generation TEXT NOT NULL, authority_endpoint TEXT NOT NULL, state TEXT NOT NULL DEFAULT 'prepared', created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`,
	`CREATE INDEX IF NOT EXISTS custody_ref_stages_active ON custody_ref_stages (repo_id, gate_path, state, updated_at)`,
	`CREATE TABLE IF NOT EXISTS recovery_anchor_stages (run_id TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE, repo_id TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE, gate_path TEXT NOT NULL, branch TEXT NOT NULL, ref TEXT NOT NULL, old_sha TEXT NOT NULL, new_sha TEXT NOT NULL, owner_generation TEXT NOT NULL, authority_endpoint TEXT NOT NULL, state TEXT NOT NULL DEFAULT 'prepared', created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`,
	`CREATE INDEX IF NOT EXISTS recovery_anchor_stages_active ON recovery_anchor_stages (repo_id, gate_path, state, updated_at)`,
	`ALTER TABLE step_results ADD COLUMN last_activity_at INTEGER`,
	`ALTER TABLE step_results ADD COLUMN last_activity TEXT`,
	`ALTER TABLE step_results ADD COLUMN agent_pid INTEGER`,
	`ALTER TABLE step_results ADD COLUMN auto_fix_limit INTEGER`,
	// Session-fidelity telemetry columns (all nullable so pre-existing rows read
	// back as unknown, never a fabricated zero).
	`ALTER TABLE agent_invocations ADD COLUMN model_provider TEXT`,
	`ALTER TABLE agent_invocations ADD COLUMN fallback_reason TEXT`,
	`ALTER TABLE agent_invocations ADD COLUMN subprocess_wait_ms INTEGER`,
	`ALTER TABLE agent_invocations ADD COLUMN fresh_input_tokens INTEGER`,
	`ALTER TABLE agent_invocations ADD COLUMN reasoning_tokens INTEGER`,
	`ALTER TABLE agent_invocations ADD COLUMN delta_input_tokens INTEGER`,
	`ALTER TABLE agent_invocations ADD COLUMN delta_output_tokens INTEGER`,
	`ALTER TABLE agent_invocations ADD COLUMN delta_cache_read_tokens INTEGER`,
	`ALTER TABLE agent_invocations ADD COLUMN model_roundtrips INTEGER`,
	`ALTER TABLE agent_invocations ADD COLUMN tool_calls INTEGER`,
	`ALTER TABLE agent_invocations ADD COLUMN tool_wait_calls INTEGER`,
	`ALTER TABLE agent_invocations ADD COLUMN tool_test_lint_calls INTEGER`,
	`ALTER TABLE agent_invocations ADD COLUMN tool_edit_calls INTEGER`,
	`ALTER TABLE agent_invocations ADD COLUMN tool_read_calls INTEGER`,
	`ALTER TABLE agent_invocations ADD COLUMN tool_git_calls INTEGER`,
	`ALTER TABLE agent_invocations ADD COLUMN tool_other_calls INTEGER`,
	`ALTER TABLE agent_invocations ADD COLUMN workload_files INTEGER`,
	`ALTER TABLE agent_invocations ADD COLUMN workload_lines INTEGER`,
	`ALTER TABLE agent_invocations ADD COLUMN finding_count INTEGER`,
}
