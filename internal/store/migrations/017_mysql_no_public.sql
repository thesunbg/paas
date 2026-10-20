-- MySQL/MariaDB cannot be offered on a public port: the edge cannot terminate TLS for the MySQL
-- protocol, and public database ports require TLS. The edge already ignores these routes; drop the
-- stale public_port/allowed_cidrs so the API, dashboard and port allocator agree with it.
-- options_version moves too, so a writer holding an older read cannot put the port back.
UPDATE databases
   SET options = options - 'public_port' - 'allowed_cidrs',
       options_version = options_version + 1
 WHERE engine = 'mysql' AND options ? 'public_port';
