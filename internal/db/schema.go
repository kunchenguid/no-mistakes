package db

const schemaSQL = `
CREATE TABLE IF NOT EXISTS repos (
    id             TEXT PRIMARY KEY,
    working_path   TEXT NOT NULL UNIQUE,
    upstream_url   TEXT NOT NULL,
    default_branch TEXT NOT NULL DEFAULT 'main',
    created_at     INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS runs (
    id         TEXT PRIMARY KEY,
    repo_id    TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    branch     TEXT NOT NULL,
    head_sha   TEXT NOT NULL,
    base_sha   TEXT NOT NULL,
    status     TEXT NOT NULL DEFAULT 'pending',
    pr_url     TEXT,
    error      TEXT,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS step_results (
    id            TEXT PRIMARY KEY,
    run_id        TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    step_name     TEXT NOT NULL,
    step_order    INTEGER NOT NULL,
    status        TEXT NOT NULL DEFAULT 'pending',
    exit_code     INTEGER,
    duration_ms   INTEGER,
    log_path      TEXT,
    findings_json TEXT,
    error         TEXT,
    started_at    INTEGER,
    completed_at  INTEGER
);
`
