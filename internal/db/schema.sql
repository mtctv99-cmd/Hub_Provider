PRAGMA journal_mode = WAL;
PRAGMA busy_timeout = 5000;
PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS identities (
    id TEXT PRIMARY KEY,
    provider_group TEXT NOT NULL,
    email TEXT UNIQUE NOT NULL,
    normalized_email TEXT,
    priority INTEGER DEFAULT 1,
    global_status TEXT NOT NULL DEFAULT 'active',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS credentials (
    id TEXT PRIMARY KEY,
    identity_id TEXT NOT NULL REFERENCES identities(id) ON DELETE CASCADE,
    domain TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'HEALTHY',
    max_concurrency INTEGER NOT NULL DEFAULT 1,
    last_used_at DATETIME,
    cooldown_until DATETIME,
    consecutive_errors INTEGER DEFAULT 0,
    source_ref TEXT,
    fingerprint TEXT,
    pool_role TEXT NOT NULL DEFAULT 'AUTO',
    last_error_kind TEXT,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(identity_id, domain)
);

CREATE TABLE IF NOT EXISTS credential_secrets (
    credential_id TEXT PRIMARY KEY REFERENCES credentials(id) ON DELETE CASCADE,
    encrypted_payload BLOB NOT NULL,
    nonce BLOB NOT NULL,
    key_version INTEGER NOT NULL DEFAULT 1,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS runtime_leases (
    lease_id TEXT PRIMARY KEY,
    credential_id TEXT NOT NULL REFERENCES credentials(id),
    client_id TEXT,
    request_id TEXT NOT NULL,
    acquired_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    expires_at DATETIME NOT NULL
);

CREATE TABLE IF NOT EXISTS provider_breakers (
    provider_name TEXT PRIMARY KEY,
    state TEXT NOT NULL DEFAULT 'CLOSED',
    failure_count INTEGER DEFAULT 0,
    last_failure_at DATETIME,
    open_until DATETIME,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS request_traffic_hourly (
    hour_start INTEGER PRIMARY KEY,
    request_count INTEGER NOT NULL DEFAULT 0,
    duration_ms INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS request_traffic_minutely (
    minute_start INTEGER PRIMARY KEY,
    request_count INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS observability_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    request_id TEXT,
    event_type TEXT NOT NULL,
    provider TEXT,
    model TEXT,
    account_id TEXT,
    latency_ms REAL,
    error_kind TEXT,
    detail TEXT,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Persistent async work shared by LLM, TTS, image, and video APIs.
CREATE TABLE IF NOT EXISTS jobs (
    id TEXT PRIMARY KEY,
    type TEXT NOT NULL DEFAULT 'generic',
    payload BLOB NOT NULL DEFAULT X'',
    kind TEXT NOT NULL DEFAULT 'generic',
    status TEXT NOT NULL DEFAULT 'PENDING',
    attempts INTEGER NOT NULL DEFAULT 0,
    available_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    locked_at DATETIME,
    locked_by TEXT,
    model TEXT,
    engine TEXT,
    input_json TEXT NOT NULL DEFAULT '{}',
    result_json TEXT,
    output_path TEXT,
    output_mime TEXT,
    error TEXT,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    started_at DATETIME,
    completed_at DATETIME,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_jobs_status_created ON jobs(status, created_at);
CREATE INDEX IF NOT EXISTS idx_jobs_kind_status ON jobs(kind, status);
CREATE INDEX IF NOT EXISTS idx_jobs_claim ON jobs(status, available_at, created_at, id);

CREATE TABLE IF NOT EXISTS media_files (
    id TEXT PRIMARY KEY,
    job_id TEXT REFERENCES jobs(id) ON DELETE SET NULL,
    path TEXT NOT NULL UNIQUE,
    mime TEXT NOT NULL,
    size_bytes INTEGER NOT NULL DEFAULT 0,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    expires_at DATETIME
);
CREATE INDEX IF NOT EXISTS idx_media_files_expires ON media_files(expires_at);

CREATE TABLE IF NOT EXISTS worker_sessions (
    worker_id TEXT PRIMARY KEY,
    model TEXT NOT NULL,
    endpoint TEXT,
    max_concurrency INTEGER NOT NULL DEFAULT 8,
    active_requests INTEGER NOT NULL DEFAULT 0,
    last_seen_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    connected_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    status TEXT NOT NULL DEFAULT 'CONNECTED'
);

CREATE TABLE IF NOT EXISTS account_cooldowns (
    identity_id TEXT PRIMARY KEY REFERENCES identities(id) ON DELETE CASCADE,
    state TEXT NOT NULL DEFAULT 'READY',
    cooldown_until DATETIME,
    last_used_at DATETIME,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS api_keys (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    key_hash TEXT NOT NULL UNIQUE,
    active INTEGER NOT NULL DEFAULT 1,
    scopes TEXT NOT NULL DEFAULT 'all',
    expires_at DATETIME,
    rate_limit_rpm INTEGER NOT NULL DEFAULT 0,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_used_at DATETIME
);

CREATE TABLE IF NOT EXISTS api_key_rate_windows (
    key_id TEXT NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
    window_start INTEGER NOT NULL,
    request_count INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (key_id, window_start)
);

CREATE TABLE IF NOT EXISTS admin_sessions (
    token_hash TEXT PRIMARY KEY,
    expires_at INTEGER NOT NULL,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS runtime_settings (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS admin_credentials (
    id TEXT PRIMARY KEY,
    password_hash TEXT NOT NULL,
    salt TEXT NOT NULL,
    must_change_password INTEGER NOT NULL DEFAULT 1,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    password_changed_at DATETIME
);
