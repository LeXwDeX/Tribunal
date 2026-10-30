-- 0020_review_command_replay.sql
-- Idempotency identities for authenticated /review and /review cancel jobs.
-- One row per (project, MR, source note, command) is written in the same
-- transaction that bumps review_controls and enqueues the run/cancel job, so a
-- replayed job returns Duplicate without changing the control or the queue.
--
-- review_command_replay_backfill marks the one-time backfill of commands that
-- were accepted before this migration. Backfill candidates are queued,
-- running, or failed review.command jobs. A command is only backfilled when
-- its enqueue transaction left kind-specific review.run/review.cancel evidence
-- for the same note, or a reaction row at the current control generation can
-- prove the note's only known command kind with a matching control intent. An
-- older reaction may belong to a pruned command of the opposite kind and never
-- proves one. A review.command job alone may never have executed.
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
