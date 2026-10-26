-- 0013_admin_bindings.sql
-- Bind each username-form bootstrap admin to the GitLab user ID that first
-- used it. GitLab usernames can be renamed and reused, so a later account
-- with the same username must not inherit admin.
CREATE TABLE admin_bindings (
  username TEXT    PRIMARY KEY,  -- lower-case GitLab username
  user_id  INTEGER NOT NULL,     -- GitLab user id
  bound_at TEXT    NOT NULL
);

-- Session creation now counts and evicts per user.
CREATE INDEX sessions_user ON sessions (user_id, expires_at);
