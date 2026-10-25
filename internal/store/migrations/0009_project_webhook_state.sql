-- 0009_project_webhook_state.sql
-- Explicit webhook provisioning state. Existing rows that already hold a hook
-- ID become "unknown": the hook may still exist, but its state was never
-- verified. All other rows keep the default "disabled".
ALTER TABLE projects ADD COLUMN webhook_state TEXT NOT NULL DEFAULT 'disabled';
ALTER TABLE projects ADD COLUMN webhook_error TEXT NOT NULL DEFAULT '';
ALTER TABLE projects ADD COLUMN webhook_events TEXT NOT NULL DEFAULT '[]';
UPDATE projects SET webhook_state = 'unknown' WHERE hook_id IS NOT NULL;
