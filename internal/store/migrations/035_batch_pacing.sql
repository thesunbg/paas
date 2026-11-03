-- Batches start their copies a few at a time, so creating one takes longer and a
-- control plane may stop half way. The input (encrypted: it holds the template
-- inputs) and a heartbeat let another control plane resume it.
ALTER TABLE batches ADD COLUMN options_enc TEXT NOT NULL DEFAULT '';
ALTER TABLE batches ADD COLUMN heartbeat_at TIMESTAMPTZ NOT NULL DEFAULT now();
