-- 0008_publication_state.sql
-- Publication outcome is separate from workflow execution. publication_state
-- gains the terminal "failed" value; publication_error carries a bounded,
-- caller-sanitized reason and never an upstream response body or credential.
ALTER TABLE workflow_dossiers ADD COLUMN publication_error TEXT NOT NULL DEFAULT '';
