-- 0036_stream_revisions.sql
-- Give review streams a cheap change marker (#531). Each SSE poll otherwise
-- re-reads every node, finding and publication row. workflow_stream_revisions
-- holds one counter per workflow; triggers on the tables the stream projection
-- depends on bump it inside the same write transaction, so a poll reads one
-- row and skips the projection queries while the counter is unchanged.
-- The guards skip rows whose workflow is already gone, and project deletion
-- removes the counter rows explicitly, so cascade deletes do not resurrect a
-- counter.
-- Additive migration with no down migration; a rollback restores the pre-upgrade
-- database snapshot and a matching binary.
CREATE TABLE workflow_stream_revisions (
  workflow_id TEXT    PRIMARY KEY,
  revision    INTEGER NOT NULL DEFAULT 0
);

CREATE TRIGGER workflow_stream_rev_findings_insert AFTER INSERT ON findings
WHEN EXISTS (SELECT 1 FROM workflows WHERE id = NEW.workflow_id)
BEGIN
  INSERT INTO workflow_stream_revisions (workflow_id, revision)
  VALUES (NEW.workflow_id, 1)
  ON CONFLICT (workflow_id) DO UPDATE SET revision = revision + 1;
END;

CREATE TRIGGER workflow_stream_rev_findings_update AFTER UPDATE ON findings
WHEN EXISTS (SELECT 1 FROM workflows WHERE id = NEW.workflow_id)
BEGIN
  INSERT INTO workflow_stream_revisions (workflow_id, revision)
  VALUES (NEW.workflow_id, 1)
  ON CONFLICT (workflow_id) DO UPDATE SET revision = revision + 1;
END;

CREATE TRIGGER workflow_stream_rev_nodes_insert AFTER INSERT ON workflow_nodes
WHEN EXISTS (SELECT 1 FROM workflows WHERE id = NEW.workflow_id)
BEGIN
  INSERT INTO workflow_stream_revisions (workflow_id, revision)
  VALUES (NEW.workflow_id, 1)
  ON CONFLICT (workflow_id) DO UPDATE SET revision = revision + 1;
END;

CREATE TRIGGER workflow_stream_rev_nodes_update AFTER UPDATE ON workflow_nodes
WHEN EXISTS (SELECT 1 FROM workflows WHERE id = NEW.workflow_id)
BEGIN
  INSERT INTO workflow_stream_revisions (workflow_id, revision)
  VALUES (NEW.workflow_id, 1)
  ON CONFLICT (workflow_id) DO UPDATE SET revision = revision + 1;
END;

CREATE TRIGGER workflow_stream_rev_publications_insert AFTER INSERT ON publications
WHEN EXISTS (SELECT 1 FROM workflows WHERE id = NEW.workflow_id)
BEGIN
  INSERT INTO workflow_stream_revisions (workflow_id, revision)
  VALUES (NEW.workflow_id, 1)
  ON CONFLICT (workflow_id) DO UPDATE SET revision = revision + 1;
END;

CREATE TRIGGER workflow_stream_rev_publications_update AFTER UPDATE ON publications
WHEN EXISTS (SELECT 1 FROM workflows WHERE id = NEW.workflow_id)
BEGIN
  INSERT INTO workflow_stream_revisions (workflow_id, revision)
  VALUES (NEW.workflow_id, 1)
  ON CONFLICT (workflow_id) DO UPDATE SET revision = revision + 1;
END;

-- Stream polls also list publications by workflow on every revision change;
-- the table currently has no index for that access path.
CREATE INDEX publications_wf ON publications (workflow_id);