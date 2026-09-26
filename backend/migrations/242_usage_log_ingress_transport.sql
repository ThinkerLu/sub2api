-- Additive and rollback-compatible: do not infer transport for historical rows.
ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS ingress_transport SMALLINT NOT NULL DEFAULT 0;
COMMENT ON COLUMN usage_logs.ingress_transport IS 'Actual client transport: 0=unknown, 1=HTTP, 2=WebSocket; independent of request_type and upstream_transport.';
