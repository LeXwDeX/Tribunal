-- 0015_secret_migration.sql
-- A single row records that every stored project secret is in the sealed
-- format bound to its project and field. Once it exists, startup no longer
-- accepts legacy (unbound) ciphertexts.
CREATE TABLE secret_migration (
  id           INTEGER NOT NULL PRIMARY KEY CHECK (id = 1),
  completed_at TEXT NOT NULL
);
