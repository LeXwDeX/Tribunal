-- 0005_retention_indexes.sql
-- Align the retention DELETE scans with their keyset order. Each index starts
-- at the cutoff column, so one bounded batch can stop after LIMIT rows.
--
-- webhook_deliveries uses an implicit rowid tie-breaker: SQLite appends the
-- rowid to every index entry, which matches ORDER BY received_at, rowid.
-- jobs only scans terminal states, so a partial index avoids indexing the
-- queued and running rows that retention never deletes.

CREATE INDEX llm_calls_created
  ON llm_calls (created_at, id);

CREATE INDEX webhook_deliveries_received
  ON webhook_deliveries (received_at);

CREATE INDEX jobs_terminal_updated
  ON jobs (updated_at, id)
  WHERE state IN ('done', 'failed', 'cancelled');
