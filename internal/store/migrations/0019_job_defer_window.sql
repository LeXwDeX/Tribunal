-- 0019_job_defer_window.sql
-- Bound how long a contended job may defer. defer_since records the first
-- deferral; after MaxDeferWindow the job fails instead of extending
-- max_attempts forever. Existing rows start with NULL and keep the legacy
-- behavior until their next deferral.
ALTER TABLE jobs ADD COLUMN defer_since TEXT;
