CREATE TABLE review_profile_overrides (
    profile TEXT PRIMARY KEY,
    overrides_json TEXT NOT NULL,
    revision INTEGER NOT NULL CHECK (revision > 0),
    updated_by INTEGER NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE review_profile_model_refs (
    profile TEXT NOT NULL REFERENCES review_profile_overrides(profile) ON DELETE CASCADE,
    node TEXT NOT NULL,
    model_id TEXT NOT NULL,
    PRIMARY KEY (profile, node)
);

CREATE INDEX review_profile_model_refs_model_id ON review_profile_model_refs(model_id);
