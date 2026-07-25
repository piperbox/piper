CREATE TABLE IF NOT EXISTS agents (
    name           TEXT PRIMARY KEY,
    token_hash     TEXT NOT NULL UNIQUE,
    base_domain    TEXT NOT NULL,
    account_id     TEXT,
    control_token  TEXT,
    webhook_secret TEXT,
    created_at     TEXT NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS agents_base_domain_unique
    ON agents(base_domain);

CREATE TABLE IF NOT EXISTS accounts (
    id           TEXT PRIMARY KEY,
    github_id    TEXT UNIQUE,
    github_login TEXT,
    username     TEXT NOT NULL UNIQUE,
    type         TEXT NOT NULL DEFAULT 'user',
    disabled     INTEGER NOT NULL DEFAULT 0,
    created_at   TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS account_creds (
    token_hash  TEXT PRIMARY KEY,
    account_id  TEXT NOT NULL REFERENCES accounts(id),
    created_at  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS hostnames (
    hostname    TEXT PRIMARY KEY,
    account_id  TEXT NOT NULL REFERENCES accounts(id),
    app         TEXT NOT NULL,
    pr          INTEGER NOT NULL DEFAULT 0,
    created_at  TEXT NOT NULL,
    UNIQUE(account_id, app, pr)
);

CREATE TABLE IF NOT EXISTS org_members (
    org_id     TEXT NOT NULL REFERENCES accounts(id),
    account_id TEXT NOT NULL REFERENCES accounts(id),
    role       TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (org_id, account_id)
);

CREATE TABLE IF NOT EXISTS org_invites (
    org_id       TEXT NOT NULL REFERENCES accounts(id),
    github_login TEXT NOT NULL,
    invited_by   TEXT NOT NULL REFERENCES accounts(id),
    created_at   TEXT NOT NULL,
    PRIMARY KEY (org_id, github_login)
);

CREATE TABLE IF NOT EXISTS custom_domains (
    domain      TEXT PRIMARY KEY,
    agent_base  TEXT NOT NULL,
    status      TEXT NOT NULL,
    created_at  TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS custom_domains_agent_base ON custom_domains(agent_base);

CREATE TABLE IF NOT EXISTS github_installations (
    installation_id TEXT PRIMARY KEY,
    account_id      TEXT NOT NULL REFERENCES accounts(id),
    target_type     TEXT NOT NULL,
    target_login    TEXT NOT NULL,
    created_at      TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS github_installations_account
    ON github_installations(account_id);

CREATE TABLE IF NOT EXISTS repo_bindings (
    agent_name TEXT NOT NULL REFERENCES agents(name),
    app        TEXT NOT NULL,
    repo       TEXT NOT NULL,
    branch     TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (agent_name, app)
);

CREATE INDEX IF NOT EXISTS repo_bindings_repo ON repo_bindings(repo);

-- attempts counts failed delivery attempts for this slot (a park only happens
-- after one has failed, so it starts at 1); next_try_at is the earliest time
-- the retry sweep may pick the row up, so a permanently-failing box backs off
-- instead of being retried every sweep forever. Both use the fixed-width
-- pendingTimeLayout, so string comparison is chronological.
CREATE TABLE IF NOT EXISTS pending_events (
    agent_name  TEXT NOT NULL REFERENCES agents(name),
    app         TEXT NOT NULL,
    ref         TEXT NOT NULL,
    event       TEXT NOT NULL,
    payload     BLOB NOT NULL,
    created_at  TEXT NOT NULL,
    attempts    INTEGER NOT NULL,
    next_try_at TEXT NOT NULL,
    PRIMARY KEY (agent_name, app, ref)
);
