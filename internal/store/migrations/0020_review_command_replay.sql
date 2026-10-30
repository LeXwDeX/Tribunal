-- 0020_review_command_replay.sql
-- Idempotency identities for authenticated /review and /review cancel jobs.
-- One row per (project, MR, source note, command) is written in the same
-- transaction that bumps review_controls and enqueues the run/cancel job, so a
-- replayed job returns Duplicate without changing the control or the queue.
--
-- review_command_replay_backfill marks the one-time backfill of commands that
-- were accepted before this migration. A valid kind-specific review.run or
-- review.cancel job proves acceptance even if the command job was pruned.
-- A fingerprint-bound review workflow and matching command-triggered dossier
-- can also prove a run when both jobs were pruned; it cannot prove a cancel.
-- All proven note IDs must be below 2^53: older payloads converted JSON
-- numbers through float64 and could round downstream jobs and reactions.
-- Ambiguous JSON objects with repeated keys are not identity evidence.
-- Queued, running, or failed command jobs may also be proven by a reaction at
-- the current control generation, if their kind is the only known kind and
-- matches the control intent. Done or cancelled commands require kind-specific
-- proof: they may have been denied or stopped before enqueue.
-- A reaction alone or an older reaction does not prove a command identity.
--
-- Rows have no foreign key to projects; DeleteProject removes them explicitly
-- so a re-registered project cannot reuse a stale identity.
CREATE TABLE review_command_replays (
  project_id INTEGER NOT NULL,
  mr_iid     INTEGER NOT NULL,
  note_id    INTEGER NOT NULL,
  command    TEXT    NOT NULL, -- run | cancel
  job_id     TEXT    NOT NULL,
  created_at TEXT    NOT NULL,
  PRIMARY KEY (project_id, mr_iid, note_id, command)
);

CREATE TABLE review_command_replay_backfill (
  id           INTEGER PRIMARY KEY CHECK (id = 1),
  completed_at TEXT    NOT NULL
);
