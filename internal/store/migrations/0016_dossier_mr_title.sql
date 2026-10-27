-- 0016_dossier_mr_title.sql
-- The MR title is copied out of the gzip dossier when it is saved, so the
-- review list can read titles for a whole page without decompressing each
-- dossier or calling GitLab. Dossiers saved before this migration keep an
-- empty title; the list then omits mr_title and the console shows !iid.
ALTER TABLE workflow_dossiers ADD COLUMN mr_title TEXT NOT NULL DEFAULT '';
