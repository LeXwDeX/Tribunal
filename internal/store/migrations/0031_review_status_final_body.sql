-- 0031_review_status_final_body.sql
-- Keep the rendered review summary in the review_status row so the single
-- status note can carry the final result (#451). The note itself is still
-- created and edited by the status worker; this column only stores its text.
--
-- Additive migration: old rows read an empty body and keep the pre-upgrade
-- status-only rendering. There is no down migration; a rollback restores the
-- pre-upgrade database snapshot and a matching binary.
ALTER TABLE review_status ADD COLUMN final_body TEXT NOT NULL DEFAULT '';