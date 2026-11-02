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
-- Reactions identify ambiguous notes, not accepted commands or command kinds.
-- Identifiable legacy commands and reactions are quarantined separately;
-- uncertainty is not evidence that a command was accepted or denied.
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
  completed_at TEXT    NOT NULL,
  reaction_quarantine INTEGER NOT NULL CHECK (reaction_quarantine = 1)
);

CREATE TABLE review_command_quarantine (
  project_id INTEGER NOT NULL,
  mr_iid     INTEGER NOT NULL,
  note_id    INTEGER NOT NULL,
  command    TEXT    NOT NULL, -- run | cancel | '' (kind unknown)
  reason     TEXT    NOT NULL,
  created_at TEXT    NOT NULL,
  PRIMARY KEY (project_id, mr_iid, note_id, command)
);
