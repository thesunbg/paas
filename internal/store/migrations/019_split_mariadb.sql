-- "mysql" used to run MariaDB. The engine name now means MySQL 8.4 and MariaDB is "mariadb".
-- Existing databases keep the server they are on: only names change, no restart is forced
-- (spec.language is a tuning hint that no engine reads, and the image is untouched).
-- paasd restarts as one binary on upgrade; this is not meant for rolling with the old binary.
UPDATE databases SET engine = 'mariadb' WHERE engine = 'mysql';

-- Shared server (system project) and dedicated database servers: slug, display name, language.
-- version is the service spec version: bump it so an open settings form does not overwrite this.
UPDATE services s
   SET slug = CASE WHEN p.slug = 'system' AND s.slug ~ '^shared-mysql(-|$)'
                   THEN 'shared-mariadb' || substr(s.slug, length('shared-mysql') + 1) ELSE s.slug END,
       name = CASE WHEN s.name LIKE 'Shared mysql %' THEN 'Shared mariadb ' || substr(s.name, length('Shared mysql ') + 1)
                   WHEN s.name LIKE '% (mysql)' THEN left(s.name, length(s.name) - length('(mysql)')) || '(mariadb)'
                   ELSE s.name END,
       spec = jsonb_set(s.spec, '{language}', '"mariadb"'),
       version = s.version + 1,
       updated_at = now()
  FROM projects p
 WHERE p.id = s.project_id AND s.type = 'database' AND s.spec->>'language' = 'mysql';

-- The shared root password follows the engine name.
INSERT INTO settings (key, value) SELECT 'shared_mariadb_root', value FROM settings WHERE key = 'shared_mysql_root'
ON CONFLICT (key) DO NOTHING;
DELETE FROM settings WHERE key = 'shared_mysql_root';
