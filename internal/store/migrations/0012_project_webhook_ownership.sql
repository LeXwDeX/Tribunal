-- 0012_project_webhook_ownership.sql
-- Record which GitLab hooks this service owns and when provisioning changed.
-- A pre-upgrade row with a hook ID could only have been written by the service
-- (manual registration stores no hook ID), so it becomes a service-owned
-- legacy hook. Rows without a hook ID stay unowned.
ALTER TABLE projects ADD COLUMN webhook_managed_by TEXT NOT NULL DEFAULT '';
ALTER TABLE projects ADD COLUMN webhook_created_at TEXT;
ALTER TABLE projects ADD COLUMN webhook_updated_at TEXT;
UPDATE projects
SET webhook_managed_by = 'crc',
    webhook_created_at = updated_at,
    webhook_updated_at = updated_at
WHERE hook_id IS NOT NULL;
