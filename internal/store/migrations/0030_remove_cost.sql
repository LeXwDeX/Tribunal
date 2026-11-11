-- 0030_remove_cost.sql
-- Remove the money cost projection from workflows, workflow nodes and stored
-- model parameters. Token usage (tokens_in/tokens_out) is unchanged.
--
-- SQLite cannot drop a column safely once it may be part of constraints or
-- indexes, so workflows and workflow_nodes are rebuilt with the same columns,
-- defaults, foreign keys and indexes minus cost_micros. The migration runner
-- disables foreign key enforcement for the duration of migrations; otherwise
-- DROP TABLE would perform an implicit DELETE that cascades into findings and
-- workflow_nodes. There is no down migration: a rollback restores the
-- pre-upgrade database snapshot and a matching binary.
CREATE TABLE workflows_new (
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
  usage_incomplete INTEGER NOT NULL DEFAULT 0,
  created_at    TEXT    NOT NULL,
  started_at    TEXT,
  finished_at   TEXT
);

INSERT INTO workflows_new
  (id, kind, project_id, mr_iid, head_sha, base_sha, fingerprint, profile,
   trigger, triggered_by, status, graph_rev, extends_used, verdict,
   cancel_reason, error, tokens_in, tokens_out, usage_incomplete,
   created_at, started_at, finished_at)
SELECT
   id, kind, project_id, mr_iid, head_sha, base_sha, fingerprint, profile,
   trigger, triggered_by, status, graph_rev, extends_used, verdict,
   cancel_reason, error, tokens_in, tokens_out, usage_incomplete,
   created_at, started_at, finished_at
FROM workflows;

DROP TABLE workflows;
ALTER TABLE workflows_new RENAME TO workflows;

CREATE INDEX workflows_mr ON workflows (project_id, mr_iid, created_at DESC);
CREATE INDEX workflows_status ON workflows (status, created_at DESC);
CREATE UNIQUE INDEX workflows_active_review
  ON workflows (project_id, mr_iid)
  WHERE kind = 'review' AND status IN ('pending', 'running');

CREATE TABLE workflow_nodes_new (
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
  usage_incomplete INTEGER NOT NULL DEFAULT 0,
  added_rev   INTEGER NOT NULL DEFAULT 0,
  queued_at   TEXT,
  started_at  TEXT,
  finished_at TEXT,
  PRIMARY KEY (workflow_id, id)
);

INSERT INTO workflow_nodes_new
  (workflow_id, id, kind, status, required, depends_on, params, attempt,
   provider, model, output, error, tokens_in, tokens_out, usage_incomplete,
   added_rev, queued_at, started_at, finished_at)
SELECT
   workflow_id, id, kind, status, required, depends_on, params, attempt,
   provider, model, output, error, tokens_in, tokens_out, usage_incomplete,
   added_rev, queued_at, started_at, finished_at
FROM workflow_nodes;

DROP TABLE workflow_nodes;
ALTER TABLE workflow_nodes_new RENAME TO workflow_nodes;

-- Stored model parameters may still carry the legacy price object. Removing it
-- keeps the record valid for the narrower parameter model. Invalid JSON is
-- left untouched so a corrupt record stays diagnosable instead of failing the
-- whole upgrade.
UPDATE llm_models
SET params_json = json_remove(params_json, '$.price')
WHERE json_valid(params_json);
