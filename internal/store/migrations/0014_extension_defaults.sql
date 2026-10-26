-- 0014_extension_defaults.sql
-- One row per platform default extension decision. A key is processed once:
-- "<kind>:<name>" records whether the extension was registered, and
-- "<kind>:<name>@<block>" whether its default block grant was applied.
-- "block:<block>" keeps, in detail, the binding the defaults last wrote, so a
-- later default may extend a row only while an administrator has not changed it.
CREATE TABLE extension_defaults (
  key        TEXT NOT NULL PRIMARY KEY,
  outcome    TEXT NOT NULL CHECK (outcome IN ('applied', 'skipped')),
  detail     TEXT NOT NULL DEFAULT '',
  applied_at TEXT NOT NULL
);
