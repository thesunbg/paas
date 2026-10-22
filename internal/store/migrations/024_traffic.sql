-- Network traffic: counters the nodes report per container, and per-minute
-- deltas in the metrics (bytes received / sent during that minute).
ALTER TABLE instances ADD COLUMN net_rx_bytes BIGINT NOT NULL DEFAULT 0;
ALTER TABLE instances ADD COLUMN net_tx_bytes BIGINT NOT NULL DEFAULT 0;
ALTER TABLE service_metrics ADD COLUMN net_rx_bytes BIGINT NOT NULL DEFAULT 0;
ALTER TABLE service_metrics ADD COLUMN net_tx_bytes BIGINT NOT NULL DEFAULT 0;
ALTER TABLE node_metrics ADD COLUMN net_rx_bytes BIGINT NOT NULL DEFAULT 0;
ALTER TABLE node_metrics ADD COLUMN net_tx_bytes BIGINT NOT NULL DEFAULT 0;

-- Daily totals, kept 90 days (the per-minute metrics only 48 hours).
CREATE TABLE service_traffic_daily (
    service_id UUID NOT NULL REFERENCES services(id) ON DELETE CASCADE,
    day        DATE NOT NULL,
    rx_bytes   BIGINT NOT NULL DEFAULT 0,
    tx_bytes   BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (service_id, day)
);
CREATE TABLE node_traffic_daily (
    node_id  UUID NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    day      DATE NOT NULL,
    rx_bytes BIGINT NOT NULL DEFAULT 0,
    tx_bytes BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (node_id, day)
);
