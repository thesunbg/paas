CREATE UNIQUE INDEX databases_shared_name_idx ON databases (engine, db_name) WHERE mode = 'shared';
