-- Bind an explicit grant to the exact filtered snapshot reviewed by an
-- administrator. Existing non-empty 0021 rows keep an empty snapshot and must
-- not execute until re-authorized. Empty grants keep both columns empty.
ALTER TABLE repository_skill_authorizations ADD COLUMN snapshot_sha256 TEXT NOT NULL DEFAULT '';
