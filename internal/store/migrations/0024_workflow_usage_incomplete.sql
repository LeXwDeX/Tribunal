-- 0024_workflow_usage_incomplete.sql
-- Persist the DAG UsageIncomplete projection for workflows and nodes.
--
-- Existing rows default to 0. That means no missing usage was detected for
-- them, not that their historical token counts are complete: old events did
-- not carry the marker. Values are not backfilled, and there is no down
-- migration because SQLite cannot drop a column safely in every supported
-- version.
ALTER TABLE workflows ADD COLUMN usage_incomplete INTEGER NOT NULL DEFAULT 0;
ALTER TABLE workflow_nodes ADD COLUMN usage_incomplete INTEGER NOT NULL DEFAULT 0;
