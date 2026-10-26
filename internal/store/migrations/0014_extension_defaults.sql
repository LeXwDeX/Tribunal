-- 0014_extension_defaults.sql
-- Built-in extensions carry the bundled name in builtin; rows created by an
-- administrator keep ''. Built-in rows can be disabled but not deleted.
ALTER TABLE skills ADD COLUMN builtin TEXT NOT NULL DEFAULT '';
ALTER TABLE mcp_servers ADD COLUMN builtin TEXT NOT NULL DEFAULT '';

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
