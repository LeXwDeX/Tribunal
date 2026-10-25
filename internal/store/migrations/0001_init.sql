-- 0001_init.sql
-- M0 schema. Keep aligned with docs/design/02-data-model.md apart from the
-- two indexes/keys noted inline below.

CREATE TABLE projects (
  id                  INTEGER PRIMARY KEY,          -- GitLab project_id
  path_with_namespace TEXT    NOT NULL,
  web_url             TEXT    NOT NULL,
  default_branch      TEXT    NOT NULL,
  token_enc           BLOB    NOT NULL,             -- AES-GCM: nonce || ciphertext
  token_expires_at    TEXT,
  token_scopes        TEXT    NOT NULL DEFAULT '[]',-- JSON array
  webhook_secret_enc  BLOB    NOT NULL,
  hook_id             INTEGER,
  profile             TEXT    NOT NULL DEFAULT 'default',
  settings            TEXT    NOT NULL DEFAULT '{}',-- project settings JSON
  enabled             INTEGER NOT NULL DEFAULT 1,
  created_at          TEXT    NOT NULL,
  updated_at          TEXT    NOT NULL
);

CREATE TABLE workspaces (
  project_id      INTEGER PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
  state           TEXT    NOT NULL,   -- new | onboarding | ready | refreshing | error
  base_sha        TEXT,
  index_sha       TEXT,
  context_sha     TEXT,
  last_refresh_at TEXT,
  last_error      TEXT,
  updated_at      TEXT    NOT NULL
);

CREATE TABLE context_docs (
  id          TEXT    PRIMARY KEY,
  project_id  INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  base_sha    TEXT    NOT NULL,
  kind        TEXT    NOT NULL,       -- agents | claude
  source      TEXT    NOT NULL,       -- generated | manual
  content     TEXT    NOT NULL,
  locked      INTEGER NOT NULL DEFAULT 0,
  model       TEXT,
  workflow_id TEXT,
  created_at  TEXT    NOT NULL,
  updated_by  INTEGER,
  UNIQUE (project_id, base_sha, kind)
);

CREATE TABLE mcp_servers (
  id         TEXT    PRIMARY KEY,
  scope      TEXT    NOT NULL,        -- platform | project
  project_id INTEGER REFERENCES projects(id) ON DELETE CASCADE,
  name       TEXT    NOT NULL,
  config     TEXT    NOT NULL,
  enabled    INTEGER NOT NULL DEFAULT 1,
  created_at TEXT    NOT NULL,
  updated_at TEXT    NOT NULL,
  UNIQUE (scope, project_id, name)
);

CREATE TABLE skills (
  id          TEXT    PRIMARY KEY,
  scope       TEXT    NOT NULL,       -- platform | project
  project_id  INTEGER REFERENCES projects(id) ON DELETE CASCADE,
  name        TEXT    NOT NULL,
  description TEXT    NOT NULL,
  dir         TEXT    NOT NULL,
  enabled     INTEGER NOT NULL DEFAULT 1,
  created_at  TEXT    NOT NULL,
  UNIQUE (scope, project_id, name)
);

CREATE TABLE block_bindings (
  scope      TEXT    NOT NULL,        -- platform | project
  project_id INTEGER NOT NULL DEFAULT 0,
  block      TEXT    NOT NULL,        -- code-review | review | reasoner | verify | synthesize | onboarding
  mcp_tools  TEXT    NOT NULL DEFAULT '[]',
  skills     TEXT    NOT NULL DEFAULT '[]',
  PRIMARY KEY (scope, project_id, block)
);

-- Delivery UUIDs are unique per project; the same UUID string from another
-- project is a distinct delivery.
CREATE TABLE webhook_deliveries (
  project_id  INTEGER NOT NULL,
  uuid        TEXT    NOT NULL,       -- X-Gitlab-Event-UUID
  event       TEXT    NOT NULL,       -- Merge Request Hook | Note Hook | Push Hook
  outcome     TEXT    NOT NULL,       -- enqueued | ignored | rejected
  reason      TEXT,
  job_id      TEXT,
  received_at TEXT    NOT NULL,
  PRIMARY KEY (project_id, uuid)
);

CREATE TABLE jobs (
  id           TEXT    PRIMARY KEY,
  kind         TEXT    NOT NULL,
  key          TEXT,
  payload      TEXT    NOT NULL,
  state        TEXT    NOT NULL,      -- queued | running | done | failed | cancelled
  attempts     INTEGER NOT NULL DEFAULT 0,
  max_attempts INTEGER NOT NULL DEFAULT 3,
  run_after    TEXT    NOT NULL,
  lease_owner  TEXT,
  lease_until  TEXT,
  last_error   TEXT,
  created_at   TEXT    NOT NULL,
  updated_at   TEXT    NOT NULL
);
CREATE INDEX jobs_claim ON jobs (state, run_after);
-- One queued job and one running job may coexist per key. This permits a
-- pending successor while the current attempt is running.
CREATE UNIQUE INDEX jobs_active_key ON jobs (key, state)
  WHERE key IS NOT NULL AND state IN ('queued', 'running');

CREATE TABLE workflows (
  id            TEXT    PRIMARY KEY,
  kind          TEXT    NOT NULL,     -- review | onboarding
  project_id    INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  mr_iid        INTEGER,
  head_sha      TEXT,
  base_sha      TEXT,
  fingerprint   TEXT,
  profile       TEXT,
  trigger       TEXT    NOT NULL,     -- mr_open | mr_update | command | api | push | register
  triggered_by  INTEGER,
  status        TEXT    NOT NULL,     -- pending | running | completed | failed | cancelled
  graph_rev     INTEGER NOT NULL DEFAULT 0,
  extends_used  INTEGER NOT NULL DEFAULT 0,
  verdict       TEXT,
  cancel_reason TEXT,
  error         TEXT,
  tokens_in     INTEGER NOT NULL DEFAULT 0,
  tokens_out    INTEGER NOT NULL DEFAULT 0,
  cost_micros   INTEGER NOT NULL DEFAULT 0,
  created_at    TEXT    NOT NULL,
  started_at    TEXT,
  finished_at   TEXT
);
CREATE INDEX workflows_mr ON workflows (project_id, mr_iid, created_at DESC);
CREATE INDEX workflows_status ON workflows (status, created_at DESC);
CREATE UNIQUE INDEX workflows_active_review
  ON workflows (project_id, mr_iid)
  WHERE kind = 'review' AND status IN ('pending', 'running');

CREATE TABLE workflow_nodes (
  workflow_id TEXT    NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
  id          TEXT    NOT NULL,
  kind        TEXT    NOT NULL,
  status      TEXT    NOT NULL,
  required    INTEGER NOT NULL,
  depends_on  TEXT    NOT NULL DEFAULT '[]',
  params      TEXT    NOT NULL DEFAULT '{}',
  attempt     INTEGER NOT NULL DEFAULT 0,
  provider    TEXT,
  model       TEXT,
  output      TEXT,
  error       TEXT,
  tokens_in   INTEGER NOT NULL DEFAULT 0,
  tokens_out  INTEGER NOT NULL DEFAULT 0,
  cost_micros INTEGER NOT NULL DEFAULT 0,
  added_rev   INTEGER NOT NULL DEFAULT 0,
  queued_at   TEXT,
  started_at  TEXT,
  finished_at TEXT,
  PRIMARY KEY (workflow_id, id)
);

CREATE TABLE events (
  workflow_id TEXT    NOT NULL,
  seq         INTEGER NOT NULL,
  type        TEXT    NOT NULL,
  payload     TEXT    NOT NULL,
  at          TEXT    NOT NULL,
  PRIMARY KEY (workflow_id, seq)
);

CREATE TABLE findings (
  id                 TEXT    PRIMARY KEY,
  workflow_id        TEXT    NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
  origin_node        TEXT    NOT NULL,
  origin_index       INTEGER NOT NULL,
  origin_provider    TEXT,
  severity           TEXT    NOT NULL, -- P0 | P1 | P2 | info
  category           TEXT    NOT NULL,
  path               TEXT,
  line               INTEGER,
  side               TEXT,             -- new | old
  title              TEXT    NOT NULL,
  body               TEXT    NOT NULL,
  evidence           TEXT,
  confidence         TEXT,             -- high | medium | low
  dedupe_group       TEXT,
  votes              REAL    NOT NULL DEFAULT 1,
  challenge_status   TEXT,
  challenge_node     TEXT,
  challenge_evidence TEXT,
  decision           TEXT,             -- publish | drop | mark | downgrade
  decision_reason    TEXT,
  final_severity     TEXT,
  position_ok        INTEGER,
  publish_key        TEXT
);
CREATE INDEX findings_wf ON findings (workflow_id);

CREATE TABLE publications (
  id          TEXT    PRIMARY KEY,
  project_id  INTEGER NOT NULL,
  mr_iid      INTEGER NOT NULL,
  kind        TEXT    NOT NULL,       -- summary | discussion | commit_status | failure_note
  publish_key TEXT    NOT NULL,
  gitlab_id   TEXT,
  workflow_id TEXT    NOT NULL,
  fingerprint TEXT    NOT NULL,
  body_hash   TEXT,
  created_at  TEXT    NOT NULL,
  updated_at  TEXT    NOT NULL,
  UNIQUE (project_id, mr_iid, kind, publish_key)
);

CREATE TABLE llm_calls (
  id            TEXT    PRIMARY KEY,
  workflow_id   TEXT    NOT NULL,
  node_id       TEXT    NOT NULL,
  turn          INTEGER NOT NULL,
  provider      TEXT    NOT NULL,
  model         TEXT    NOT NULL,
  request       BLOB,
  response      BLOB,
  input_tokens  INTEGER NOT NULL DEFAULT 0,
  output_tokens INTEGER NOT NULL DEFAULT 0,
  cache_read    INTEGER NOT NULL DEFAULT 0,
  cache_write   INTEGER NOT NULL DEFAULT 0,
  latency_ms    INTEGER NOT NULL,
  stop_reason   TEXT,
  error_kind    TEXT,
  request_id    TEXT,
  created_at    TEXT    NOT NULL
);
CREATE INDEX llm_calls_node ON llm_calls (workflow_id, node_id, turn);

CREATE TABLE feedback (
  id             TEXT    PRIMARY KEY,
  project_id     INTEGER NOT NULL,
  mr_iid         INTEGER NOT NULL,
  publication_id TEXT    NOT NULL,
  finding_id     TEXT,
  signal         TEXT    NOT NULL,    -- thumbsup | thumbsdown | resolved_changed | resolved_unchanged
  gitlab_user_id INTEGER,
  observed_at    TEXT    NOT NULL,
  UNIQUE (publication_id, signal, gitlab_user_id)
);

CREATE TABLE users (
  id            INTEGER PRIMARY KEY,  -- GitLab user id
  username      TEXT    NOT NULL,
  name          TEXT    NOT NULL,
  role          TEXT    NOT NULL,     -- admin | viewer
  last_login_at TEXT
);

CREATE TABLE sessions (
  id         TEXT    PRIMARY KEY,
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  expires_at TEXT    NOT NULL,
  created_at TEXT    NOT NULL
);

CREATE TABLE api_tokens (
  id           TEXT    PRIMARY KEY,
  user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name         TEXT    NOT NULL,
  token_hash   BLOB    NOT NULL,
  last_used_at TEXT,
  expires_at   TEXT,
  created_at   TEXT    NOT NULL
);
