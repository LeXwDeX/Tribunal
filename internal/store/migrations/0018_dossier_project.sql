-- Only an existing workflow is authoritative for legacy dossier ownership.
ALTER TABLE workflow_dossiers ADD COLUMN project_id INTEGER NOT NULL DEFAULT 0;
UPDATE workflow_dossiers SET project_id = COALESCE(
  (SELECT project_id FROM workflows WHERE workflows.id = workflow_dossiers.workflow_id), 0
);
CREATE INDEX workflow_dossiers_project ON workflow_dossiers(project_id);
