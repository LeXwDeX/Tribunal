ALTER TABLE users ADD COLUMN global_read INTEGER NOT NULL DEFAULT 0 CHECK (global_read IN (0, 1));
ALTER TABLE users ADD COLUMN global_read_by INTEGER NULL;
ALTER TABLE users ADD COLUMN global_read_at TEXT NULL;
CREATE INDEX users_global_read_idx ON users(id) WHERE global_read = 1;
