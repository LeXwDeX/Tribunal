-- 0007_project_access.sql
-- Per-project authorization. Existing users start enabled with no grants.
--
-- A grant is the only source of project business-data access, including for
-- platform admins. Sessions and API tokens stay user-wide; project filtering
-- happens in each read query. Dropping a user or project cascades its grants.

ALTER TABLE users ADD COLUMN disabled INTEGER NOT NULL DEFAULT 0;

CREATE TABLE project_grants (
  project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  role       TEXT    NOT NULL CHECK (role IN ('read', 'manage')),
  PRIMARY KEY (project_id, user_id)
);

-- Listing one user's projects and deleting users stay indexed.
CREATE INDEX project_grants_user ON project_grants (user_id, project_id);
