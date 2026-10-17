-- Project network indexes are allocated as the lowest free slot (store.CreateProject)
-- instead of a sequence, so deleted projects give theirs back. A released index
-- cools down first: a node that was offline when the project was deleted still
-- runs its containers on that bridge until it reconnects and stops them.
ALTER TABLE projects ALTER COLUMN net_index DROP DEFAULT;
DROP SEQUENCE IF EXISTS projects_net_index_seq;
CREATE TABLE net_index_released (
    net_index   INT PRIMARY KEY,
    released_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
