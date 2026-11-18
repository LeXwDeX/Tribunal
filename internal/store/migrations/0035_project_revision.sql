-- 0035_project_revision.sql
-- Add an optimistic-concurrency revision to project settings (#517). It changes
-- only when the management API writes profile, settings or enabled; token
-- rotation and webhook health scans keep refreshing updated_at without
-- invalidating a settings draft that was loaded earlier. Existing rows start at
-- 1 so a client can load and conditionally save them.
-- Additive migration with no down migration; a rollback restores the pre-upgrade
-- database snapshot and a matching binary.
ALTER TABLE projects ADD COLUMN revision INTEGER NOT NULL DEFAULT 1;