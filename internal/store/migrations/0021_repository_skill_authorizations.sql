-- Explicit repository skill grants are separate from legacy block bindings.
CREATE TABLE repository_skill_authorizations (
  project_id   INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  target_branch TEXT NOT NULL,
  block         TEXT NOT NULL,
  skills        TEXT NOT NULL,
  target_sha    TEXT NOT NULL,
  revision      TEXT NOT NULL,
  PRIMARY KEY (project_id, target_branch, block)
);
