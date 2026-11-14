-- 0034_webhook_delivery_mr.sql
-- Record the merge request of a webhook delivery so the read-only delivery view
-- can explain per MR why no review was created (#466).
--
-- The column is additive and defaults to 0 for historical rows. Enqueued
-- deliveries can recover the MR from their job payload; ignored and rejected
-- deliveries have no job, so their MR stays unknown until new deliveries record
-- it. The migration runner applies each file once and the backfill is a no-op
-- on replay.
-- Additive migration with no down migration; a rollback restores the pre-upgrade
-- database snapshot and a matching binary.
ALTER TABLE webhook_deliveries ADD COLUMN mr_iid INTEGER NOT NULL DEFAULT 0;

CREATE INDEX IF NOT EXISTS webhook_deliveries_mr
  ON webhook_deliveries (project_id, mr_iid, received_at);

UPDATE webhook_deliveries
SET mr_iid = COALESCE((
  SELECT CAST(json_extract(j.payload, '$.mr_iid') AS INTEGER)
  FROM jobs j
  WHERE j.id = webhook_deliveries.job_id AND json_valid(j.payload)
), 0)
WHERE mr_iid = 0 AND job_id IS NOT NULL AND job_id <> '';