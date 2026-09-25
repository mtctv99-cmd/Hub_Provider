CREATE TABLE IF NOT EXISTS jobs (
    id TEXT PRIMARY KEY,
    type TEXT NOT NULL,
    payload BLOB NOT NULL,
    kind TEXT NOT NULL DEFAULT 'generic',
    model TEXT,
    engine TEXT,
    input_json TEXT NOT NULL DEFAULT '{}',
    result_json TEXT,
    output_path TEXT,
    output_mime TEXT,
    status TEXT NOT NULL CHECK (status IN ('PENDING', 'PROCESSING', 'COMPLETED', 'FAILED')),
    attempts INTEGER NOT NULL DEFAULT 0,
    available_at DATETIME NOT NULL,
    locked_at DATETIME,
    locked_by TEXT,
    completed_at DATETIME,
    error TEXT,
    created_at DATETIME NOT NULL,
    updated_at DATETIME NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_jobs_claim
    ON jobs(status, available_at, created_at, id);
CREATE INDEX IF NOT EXISTS idx_jobs_cleanup
    ON jobs(status, completed_at, updated_at);
