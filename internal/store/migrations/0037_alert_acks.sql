-- 0037_alert_acks.sql
-- Per-user "read" marks for the console overview alerts (#656). The browser
-- computes alert_key and fingerprint; the server stores them opaquely and never
-- resolves a key to a project, so a key cannot probe project existence.
-- An alert counts as read while the alert's current fingerprint matches the
-- stored one (aggregate alerts store a finished_at watermark instead). Writes
-- keep at most 1000 rows per user and evict the oldest acked_at first.
-- Additive migration with no down migration; a rollback restores the pre-upgrade
-- database snapshot and a matching binary. Older binaries ignore the table.
CREATE TABLE alert_acks (
  user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  alert_key   TEXT    NOT NULL,
  fingerprint TEXT    NOT NULL,
  acked_at    TEXT    NOT NULL,
  PRIMARY KEY (user_id, alert_key)
) WITHOUT ROWID;
