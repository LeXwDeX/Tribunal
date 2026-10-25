-- 0011_review_side_effects.sql
-- Durable desired state for the per-MR status note and per-note reactions.
-- Version/AppliedVersion fence a GitLab write against a newer desired state;
-- generation/head fencing is checked against review_controls at write time.
-- A failed or retried status job therefore never re-runs a model.
CREATE TABLE review_status (
  project_id      INTEGER NOT NULL,
  mr_iid          INTEGER NOT NULL,
  generation      INTEGER NOT NULL,
  head_sha        TEXT    NOT NULL DEFAULT '',
  attempt         INTEGER NOT NULL DEFAULT 0,
  profile         TEXT    NOT NULL DEFAULT '',
  state           TEXT    NOT NULL,
  detail          TEXT    NOT NULL DEFAULT '',
  note_id         INTEGER,
  body_hash       TEXT    NOT NULL DEFAULT '',
  version         INTEGER NOT NULL DEFAULT 1,
  applied_version INTEGER NOT NULL DEFAULT 0,
  last_error      TEXT    NOT NULL DEFAULT '',
  updated_at      TEXT    NOT NULL,
  PRIMARY KEY (project_id, mr_iid)
);

CREATE TABLE review_reactions (
  project_id      INTEGER NOT NULL,
  mr_iid          INTEGER NOT NULL,
  note_id         INTEGER NOT NULL,
  generation      INTEGER NOT NULL,
  desired         TEXT    NOT NULL, -- eyes | white_check_mark | x | none
  version         INTEGER NOT NULL DEFAULT 1,
  applied_version INTEGER NOT NULL DEFAULT 0,
  last_error      TEXT    NOT NULL DEFAULT '',
  updated_at      TEXT    NOT NULL,
  PRIMARY KEY (project_id, mr_iid, note_id)
);

CREATE INDEX review_status_pending
  ON review_status (applied_version, version, updated_at);

CREATE INDEX review_reactions_pending
  ON review_reactions (applied_version, version, updated_at);
