-- 0032_review_status_final_body_ready.sql
-- Mark whether a review_status row owns the merged review note (#451).
--
-- Rows written before #452 stored no merged body: their published summary is a
-- separate summary-marker note and their lifecycle status lives in a legacy
-- status-marker note. Setting final_body_ready=0 keeps the status worker on the
-- legacy note for those rows so it never overwrites a published summary. Rows
-- created after this migration default to 1; SetReviewStatusFinalBody also
-- sets 1 when a publication writes a body. The backfill only fills pre-existing
-- rows (final_body has the migration-0031 default of '' there), so replaying
-- the UPDATE cannot reset a merged row.
--
-- The migration runner applies each file once. Additive migration with no down
-- migration; a rollback restores the pre-upgrade database snapshot and a
-- matching binary.
ALTER TABLE review_status ADD COLUMN final_body_ready INTEGER NOT NULL DEFAULT 1;
UPDATE review_status SET final_body_ready = 0 WHERE final_body = '';