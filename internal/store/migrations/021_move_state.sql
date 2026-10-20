-- Progress of a database move (shared -> dedicated, or a major version upgrade). Written before the
-- target server exists and cleared at the end, so a restart of paasd can undo a move that never
-- reached the flip, or finish one that did. NULL = no move in progress.
ALTER TABLE databases ADD COLUMN move_state JSONB;
