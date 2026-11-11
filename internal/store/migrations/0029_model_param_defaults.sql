-- 0029_model_param_defaults.sql
-- Platform-level default parameters for user models. The table holds at most
-- one row (id = 1). params_json uses the same shape as llm_models.params_json;
-- the zero value of a field means "unset", so a user model inherits the
-- default unless it sets that field itself.
--
-- Config models are deliberately not covered: their defaults come from the
-- base/overlay configuration. There is no down migration; a rollback restores
-- the pre-upgrade database snapshot and a matching binary.
CREATE TABLE llm_model_defaults (
    id          INTEGER PRIMARY KEY CHECK (id = 1),
    params_json TEXT    NOT NULL,
    revision    INTEGER NOT NULL CHECK (revision > 0),
    updated_by  INTEGER NOT NULL,
    updated_at  TEXT    NOT NULL
);
