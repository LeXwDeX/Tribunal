CREATE TABLE llm_models (
    id TEXT PRIMARY KEY,
    display_name TEXT NOT NULL,
    protocol TEXT NOT NULL,
    base_url TEXT NOT NULL,
    model TEXT NOT NULL,
    params_json TEXT NOT NULL,
    api_key_sealed BLOB NULL,
    revision INTEGER NOT NULL,
    created_by INTEGER NOT NULL,
    created_at TEXT NOT NULL,
    updated_by INTEGER NOT NULL,
    updated_at TEXT NOT NULL,
    deleted_at TEXT NULL
);
