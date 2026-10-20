-- Databases created before versions were selectable have version = ''. Label them with the
-- engine default so the column is always a catalog name. The label is a guess for old databases
-- (the image tag froze per node); the real server version is read from the server.
UPDATE databases SET version = CASE engine
    WHEN 'postgres' THEN '17' WHEN 'mysql' THEN '11.8' WHEN 'redis' THEN '7.4' WHEN 'mongo' THEN '7.0'
  END
 WHERE version = '';

-- Last lifecycle status ("ending"/"eol") a database.eol notification was sent for. Its own column:
-- options_version guards user saves of options, and a loop writing there would race them.
ALTER TABLE databases ADD COLUMN eol_notified TEXT NOT NULL DEFAULT '';
