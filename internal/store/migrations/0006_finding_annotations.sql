-- 0006_finding_annotations.sql
-- Human annotation fields are separate from producer-owned finding fields.
-- Existing rows migrate as unannotated. ReplaceFindings carries these columns
-- forward by finding ID; producer input never writes them.
ALTER TABLE findings ADD COLUMN annotation TEXT NOT NULL DEFAULT '';
ALTER TABLE findings ADD COLUMN annotated_by TEXT;
ALTER TABLE findings ADD COLUMN annotated_at TEXT;
