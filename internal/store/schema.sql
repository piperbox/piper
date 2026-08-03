CREATE TABLE IF NOT EXISTS apps (
    name           TEXT PRIMARY KEY,
    port           INTEGER NOT NULL,
    repo           TEXT NOT NULL DEFAULT '',
    branch         TEXT NOT NULL DEFAULT '',
    root_dir       TEXT NOT NULL DEFAULT '',
    hostname       TEXT NOT NULL DEFAULT '',
    created_at     TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS deployments (
    id           TEXT PRIMARY KEY,
    app          TEXT NOT NULL REFERENCES apps(name),
    image_id     TEXT NOT NULL,
    container_id TEXT NOT NULL,
    host_port    INTEGER NOT NULL,
    status       TEXT NOT NULL,
    logs         TEXT NOT NULL DEFAULT '',
    pr           INTEGER NOT NULL DEFAULT 0,
    hostname     TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_deployments_app ON deployments(app, created_at);
CREATE TABLE IF NOT EXISTS github_app (
    id             INTEGER PRIMARY KEY CHECK (id = 1),
    app_id         INTEGER NOT NULL,
    slug           TEXT NOT NULL DEFAULT '',
    private_key    TEXT NOT NULL,
    webhook_secret TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS tokens (
    id         TEXT PRIMARY KEY,
    label      TEXT NOT NULL UNIQUE,
    token_hash TEXT NOT NULL UNIQUE,
    scope      TEXT NOT NULL DEFAULT 'admin',
    created_at TEXT NOT NULL,
    revoked_at TEXT
);
CREATE TABLE IF NOT EXISTS domain_config (
    id             INTEGER PRIMARY KEY CHECK (id = 1),
    domain         TEXT NOT NULL,
    dns_provider   TEXT NOT NULL,
    dns_token      TEXT NOT NULL,
    status         TEXT NOT NULL,
    error          TEXT NOT NULL DEFAULT '',
    cert_not_after TEXT NOT NULL DEFAULT '',
    updated_at     TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS app_domains (
    domain         TEXT PRIMARY KEY,
    app            TEXT NOT NULL REFERENCES apps(name),
    status         TEXT NOT NULL DEFAULT '',
    error          TEXT NOT NULL DEFAULT '',
    cert_not_after TEXT NOT NULL DEFAULT '',
    updated_at     TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS app_env (
    app        TEXT NOT NULL REFERENCES apps(name),
    key        TEXT NOT NULL,
    value      TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (app, key)
);
