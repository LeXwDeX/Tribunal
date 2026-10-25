-- 0004_context_revisions.sql
-- Context documents become an append-only revision history.
--
-- The old UNIQUE (project_id, base_sha, kind) constraint made a manual edit
-- overwrite the only row for that base. Rebuild the table without it and add
-- an explicit active flag so one revision per (project_id, kind) is current,
-- even when several revisions share the same base SHA.
--
-- Existing rows keep their ids, content, lock flags and timestamps. The
-- revision matching workspaces.context_sha becomes active; otherwise the
-- newest revision per kind stays current, matching the old fallback.

CREATE TABLE context_docs_new (
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
  active      INTEGER NOT NULL DEFAULT 0
);

INSERT INTO context_docs_new
  (rowid, id, project_id, base_sha, kind, source, content, locked, model,
   workflow_id, created_at, updated_by, active)
SELECT rowid, id, project_id, base_sha, kind, source, content, locked, model,
       workflow_id, created_at, updated_by, 0
FROM context_docs
ORDER BY rowid;

WITH ranked AS (
  SELECT cd.id,
         ROW_NUMBER() OVER (
           PARTITION BY cd.project_id, cd.kind
           ORDER BY
             CASE WHEN cd.base_sha =
               COALESCE((SELECT w.context_sha FROM workspaces w
                         WHERE w.project_id = cd.project_id), '')
               THEN 0 ELSE 1 END,
             cd.created_at DESC,
             cd.rowid DESC
         ) AS rn
  FROM context_docs_new cd
)
UPDATE context_docs_new
SET active = 1
WHERE id IN (SELECT id FROM ranked WHERE rn = 1);

DROP TABLE context_docs;
ALTER TABLE context_docs_new RENAME TO context_docs;

CREATE INDEX idx_context_docs_base
  ON context_docs (project_id, base_sha, kind, created_at, id);
CREATE INDEX idx_context_docs_kind
  ON context_docs (project_id, kind, created_at, id);
CREATE INDEX idx_context_docs_workflow
  ON context_docs (project_id, base_sha, kind, workflow_id);
CREATE UNIQUE INDEX idx_context_docs_active
  ON context_docs (project_id, kind) WHERE active = 1;
