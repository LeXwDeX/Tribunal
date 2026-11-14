-- 0033_background_maintenance.sql
-- Background maintenance for long-running deployments (#459).
--
-- 1. review_status.last_verified_at separates "the desired version is applied"
--    from "we recently confirmed the remote note still matches". The five-minute
--    reconciliation only enqueues rows whose desired version is not applied;
--    successfully applied rows are revisited by a low-frequency self-heal scan
--    and short-circuit locally when they were verified recently.
-- 2. llm_call_daily and its latency/error companions keep provider/model usage
--    long after llm_calls detail rows are deleted by storage.llm_call_retention.
--    The backfill is replay-safe: INSERT OR IGNORE only fills missing buckets and
--    SELECT ... GROUP BY is idempotent. The status backfill is also replay-safe.
-- 3. feedback_collection_state exposes the last successful collection pass and
--    the most recent sanitized error class of the feedback collector.
-- 4. workspace_capacity stores the latest measured workspace root usage so the
--    API can report the cap and whether it is exceeded.
--
-- Additive migration with no down migration; a rollback restores the pre-upgrade
-- database snapshot and a matching binary. The migration runner applies each
-- file once; every data statement below is idempotent if it were replayed.
ALTER TABLE review_status ADD COLUMN last_verified_at TEXT NOT NULL DEFAULT '';
UPDATE review_status SET last_verified_at = updated_at WHERE applied_version >= version;

CREATE TABLE IF NOT EXISTS llm_call_daily (
  day             TEXT    NOT NULL,        -- UTC day of the workflow created_at
  project_id      INTEGER NOT NULL,
  provider        TEXT    NOT NULL,
  model           TEXT    NOT NULL,
  calls           INTEGER NOT NULL DEFAULT 0,
  errors          INTEGER NOT NULL DEFAULT 0,
  input_tokens    INTEGER NOT NULL DEFAULT 0,
  output_tokens   INTEGER NOT NULL DEFAULT 0,
  cache_read      INTEGER NOT NULL DEFAULT 0,
  cache_write     INTEGER NOT NULL DEFAULT 0,
  latency_ms_sum  INTEGER NOT NULL DEFAULT 0,
  latency_samples INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (day, project_id, provider, model)
);

-- One row per distinct latency value keeps the percentile exact. Integer
-- millisecond values are bounded by the audit contract.
CREATE TABLE IF NOT EXISTS llm_call_latency_daily (
  day         TEXT    NOT NULL,
  project_id  INTEGER NOT NULL,
  provider    TEXT    NOT NULL,
  model       TEXT    NOT NULL,
  latency_ms  INTEGER NOT NULL,
  samples     INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (day, project_id, provider, model, latency_ms)
);

CREATE TABLE IF NOT EXISTS llm_call_error_daily (
  day        TEXT    NOT NULL,
  project_id INTEGER NOT NULL,
  provider   TEXT    NOT NULL,
  model      TEXT    NOT NULL,
  error_kind TEXT    NOT NULL,
  calls      INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (day, project_id, provider, model, error_kind)
);

INSERT OR IGNORE INTO llm_call_daily
  (day, project_id, provider, model, calls, errors, input_tokens, output_tokens,
   cache_read, cache_write, latency_ms_sum, latency_samples)
SELECT substr(w.created_at, 1, 10), w.project_id, c.provider, c.model,
       COUNT(*),
       SUM(CASE WHEN c.error_kind IS NULL OR c.error_kind = '' THEN 0 ELSE 1 END),
       SUM(c.input_tokens), SUM(c.output_tokens),
       SUM(c.cache_read), SUM(c.cache_write),
       SUM(c.latency_ms), COUNT(*)
FROM llm_calls c JOIN workflows w ON w.id = c.workflow_id
GROUP BY 1, 2, 3, 4;

INSERT OR IGNORE INTO llm_call_latency_daily
  (day, project_id, provider, model, latency_ms, samples)
SELECT substr(w.created_at, 1, 10), w.project_id, c.provider, c.model, c.latency_ms, COUNT(*)
FROM llm_calls c JOIN workflows w ON w.id = c.workflow_id
GROUP BY 1, 2, 3, 4, 5;

INSERT OR IGNORE INTO llm_call_error_daily
  (day, project_id, provider, model, error_kind, calls)
SELECT substr(w.created_at, 1, 10), w.project_id, c.provider, c.model, c.error_kind, COUNT(*)
FROM llm_calls c JOIN workflows w ON w.id = c.workflow_id
WHERE c.error_kind IS NOT NULL AND c.error_kind <> ''
GROUP BY 1, 2, 3, 4, 5;

CREATE TABLE IF NOT EXISTS feedback_collection_state (
  id               INTEGER PRIMARY KEY CHECK (id = 1),
  last_collected_at TEXT,
  last_error_class TEXT NOT NULL DEFAULT '',
  last_error_at    TEXT
);
INSERT OR IGNORE INTO feedback_collection_state (id, last_collected_at, last_error_class, last_error_at)
VALUES (1, NULL, '', NULL);

CREATE TABLE IF NOT EXISTS workspace_capacity (
  id          INTEGER PRIMARY KEY CHECK (id = 1),
  measured_at TEXT,
  total_bytes INTEGER NOT NULL DEFAULT 0,
  limit_bytes INTEGER NOT NULL DEFAULT 0,
  over_limit  INTEGER NOT NULL DEFAULT 0
);
INSERT OR IGNORE INTO workspace_capacity (id, measured_at, total_bytes, limit_bytes, over_limit)
VALUES (1, NULL, 0, 0, 0);