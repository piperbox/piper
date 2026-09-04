-- Postgres DDL, applied on every start with IF NOT EXISTS. This file is
-- always the complete current shape: a schema change edits it directly
-- (pre-1.x policy, no migrations). Timestamps are TEXT on purpose — RFC3339Nano
-- (or the fixed-width pendingTimeLayout) compared as strings, as the Go code
-- has always done.
--
-- Most created_at values use time.RFC3339Nano; trimmed fractional zeros make
-- lexical ordering unsafe, so creation-order queries must use the id column.
-- The pending_events exception is written with fixed-width pendingTimeLayout,
-- so its documented text comparisons remain chronological.
--
-- id BIGSERIAL columns stand in for SQLite's rowid where the code orders or
-- dedupes by insertion order; they are never exposed.

CREATE TABLE IF NOT EXISTS agents (
    id             BIGSERIAL,
    name           TEXT PRIMARY KEY,
    token_hash     TEXT NOT NULL UNIQUE,
    base_domain    TEXT NOT NULL,
    account_id     TEXT,
    -- box_id is the durable identity piperd mints on first enroll
    -- (<dataDir>/box-id). Non-empty box_id makes enroll an upsert keyed on
    -- (account_id, box_id): token rotates, base domain and quota slot are
    -- reused. Empty box_id is an operator/legacy enroll: fresh row per call.
    box_id         TEXT,
    control_token  TEXT,
    webhook_secret TEXT,
    created_at     TEXT NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS agents_base_domain_unique
    ON agents(base_domain);

CREATE UNIQUE INDEX IF NOT EXISTS agents_account_box_unique
    ON agents(account_id, box_id)
    WHERE box_id IS NOT NULL AND box_id <> '';

-- username is unique per type, not globally: users and orgs hold separate
-- namespaces, so an org can never take a GitHub login out from under the user
-- who owns it (#411). Hostnames stay distinct regardless — appHostname keys its
-- hash on the account id, and the slug is only the human-readable half.
CREATE TABLE IF NOT EXISTS accounts (
    id           TEXT PRIMARY KEY,
    github_id    TEXT UNIQUE,
    github_login TEXT,
    username     TEXT NOT NULL,
    type         TEXT NOT NULL DEFAULT 'user',
    disabled     BOOLEAN NOT NULL DEFAULT false,
    created_at   TEXT NOT NULL,
    UNIQUE(username, type)
);

CREATE TABLE IF NOT EXISTS account_creds (
    token_hash  TEXT PRIMARY KEY,
    account_id  TEXT NOT NULL REFERENCES accounts(id),
    created_at  TEXT NOT NULL
);

-- agent_name attributes each hostname to the box that claimed it, so removing a
-- box reclaims its app slots and two boxes on one account can hold the same app
-- name without colliding (#405). account_id stays because the app cap is still
-- per account, not per box.
CREATE TABLE IF NOT EXISTS hostnames (
    hostname    TEXT PRIMARY KEY,
    agent_name  TEXT NOT NULL REFERENCES agents(name),
    account_id  TEXT NOT NULL REFERENCES accounts(id),
    app         TEXT NOT NULL,
    pr          INTEGER NOT NULL DEFAULT 0,
    created_at  TEXT NOT NULL,
    UNIQUE(agent_name, app, pr)
);

CREATE TABLE IF NOT EXISTS org_members (
    id         BIGSERIAL,
    org_id     TEXT NOT NULL REFERENCES accounts(id),
    account_id TEXT NOT NULL REFERENCES accounts(id),
    role       TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (org_id, account_id)
);

CREATE TABLE IF NOT EXISTS org_invites (
    id           BIGSERIAL,
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
    id              BIGSERIAL,
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
    id          BIGSERIAL,
    agent_name  TEXT NOT NULL REFERENCES agents(name),
    app         TEXT NOT NULL,
    ref         TEXT NOT NULL,
    event       TEXT NOT NULL,
    payload     BYTEA NOT NULL,
    created_at  TEXT NOT NULL,
    attempts    INTEGER NOT NULL,
    next_try_at TEXT NOT NULL,
    PRIMARY KEY (agent_name, app, ref)
);

-- relay_instances is the pool of live relay processes an edge can dial. A
-- row is live while last_seen is within instanceTTL (heartbeat every 5 s);
-- liveness is a read-side predicate, so a crashed relay drops out of routing
-- without anyone deleting anything. Whoever reads a dead row deletes it. The
-- four addrs are what an edge dials for each of the relay's listeners.
-- TIMESTAMPTZ here (unlike the TEXT stamps above) because the liveness
-- predicate compares against the server's now(), never a relay's clock.
CREATE TABLE IF NOT EXISTS relay_instances (
    id          TEXT PRIMARY KEY,
    started_at  TIMESTAMPTZ NOT NULL,
    last_seen   TIMESTAMPTZ NOT NULL,
    sessions    INTEGER NOT NULL DEFAULT 0,
    tls_addr    TEXT NOT NULL,
    http_addr   TEXT NOT NULL,
    tunnel_addr TEXT NOT NULL,
    api_addr    TEXT NOT NULL
);

-- agent_owners says which instance terminates an agent's tunnel. The
-- instance cascade takes ownership down with a deleted instance row; the
-- agents cascade lets DeleteAgent stay unchanged.
CREATE TABLE IF NOT EXISTS agent_owners (
    agent_name  TEXT PRIMARY KEY REFERENCES agents(name) ON DELETE CASCADE,
    instance_id TEXT NOT NULL REFERENCES relay_instances(id) ON DELETE CASCADE,
    since       TIMESTAMPTZ NOT NULL
);
