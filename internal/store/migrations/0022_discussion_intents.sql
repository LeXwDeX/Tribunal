-- Keep discussion receipts across project deletion: the remote thread remains.
CREATE TABLE discussion_intents (
  project_id INTEGER NOT NULL,
  mr_iid INTEGER NOT NULL,
  kind TEXT NOT NULL CHECK (kind = 'discussion'),
  publish_key TEXT NOT NULL,
  gitlab_id TEXT NOT NULL DEFAULT '',
  verification_required INTEGER NOT NULL DEFAULT 0 CHECK (verification_required IN (0, 1)),
  PRIMARY KEY (project_id, mr_iid, kind, publish_key)
);

-- Old publication records are evidence only when they contain a remote ID.
INSERT OR IGNORE INTO discussion_intents (project_id, mr_iid, kind, publish_key, gitlab_id)
SELECT project_id, mr_iid, kind, publish_key, COALESCE(gitlab_id, '')
FROM publications
WHERE kind = 'discussion' AND project_id > 0 AND mr_iid > 0 AND publish_key <> '';

-- Older releases stored findings before sending but did not store send intent.
-- A missing key fences the entire MR, because the old attempt cannot be identified.
INSERT OR IGNORE INTO discussion_intents (project_id, mr_iid, kind, publish_key)
SELECT w.project_id, w.mr_iid, 'discussion',
       COALESCE(NULLIF(f.publish_key, ''), '*')
FROM findings f JOIN workflows w ON w.id = f.workflow_id
WHERE w.kind = 'review' AND w.project_id > 0 AND w.mr_iid > 0
  AND (f.position_ok = 1 OR f.position_ok IS NULL)
  AND f.decision IN ('publish', 'mark', 'downgrade');
