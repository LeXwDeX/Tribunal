-- MCP configuration is migrated separately when the master key is available.
ALTER TABLE mcp_servers ADD COLUMN config_enc BLOB;

CREATE TABLE mcp_config_seal (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  verifier BLOB NOT NULL
);
