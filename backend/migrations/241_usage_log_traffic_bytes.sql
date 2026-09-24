-- Per-request application-layer body traffic. Values are logical request/response
-- body bytes observed inside sub2api, not TCP/TLS wire bytes.
ALTER TABLE usage_logs
    ADD COLUMN IF NOT EXISTS ingress_body_bytes BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS upstream_request_body_bytes BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS upstream_response_body_bytes BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS egress_body_bytes BIGINT NOT NULL DEFAULT 0;

COMMENT ON COLUMN usage_logs.ingress_body_bytes IS 'Client request body bytes read by sub2api';
COMMENT ON COLUMN usage_logs.upstream_request_body_bytes IS 'Request body bytes sent to the upstream provider';
COMMENT ON COLUMN usage_logs.upstream_response_body_bytes IS 'Response body bytes received from the upstream provider';
COMMENT ON COLUMN usage_logs.egress_body_bytes IS 'Response body bytes written back to the client';

CREATE INDEX IF NOT EXISTS idx_usage_logs_session_id_exact
    ON usage_logs (session_id)
    WHERE session_id IS NOT NULL;

ALTER TABLE usage_dashboard_hourly
    ADD COLUMN IF NOT EXISTS ingress_body_bytes BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS upstream_request_body_bytes BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS upstream_response_body_bytes BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS egress_body_bytes BIGINT NOT NULL DEFAULT 0;

ALTER TABLE usage_dashboard_daily
    ADD COLUMN IF NOT EXISTS ingress_body_bytes BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS upstream_request_body_bytes BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS upstream_response_body_bytes BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS egress_body_bytes BIGINT NOT NULL DEFAULT 0;
