-- Additive: old releases ignore the column; old rows remain unknown.
ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS upstream_transport SMALLINT NOT NULL DEFAULT 0;
COMMENT ON COLUMN usage_logs.upstream_transport IS 'Actual upstream transport: 0=unknown, 1=HTTP, 2=WebSocket; independent of request_type.';
