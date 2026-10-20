package databases

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// What a move checks before it switches a database to its new server: the same tables (collections)
// and the same row counts, and that the extra users can log in. Row counts are taken for the
// InventoryMax biggest tables; with more tables than that only the list of tables is compared in full.

// InventoryMax bounds how many tables are counted.
const InventoryMax = 200

// Inventory returns a command printing "T|<table>" for every table and "C|<table>|<rows>" for the
// InventoryMax biggest ones (collections for Mongo).
func Inventory(engine string, shared bool, c Credentials) (Command, error) {
	switch engine {
	case Postgres:
		user := c.Username
		if shared {
			user = "postgres"
		}
		sql := fmt.Sprintf(`SELECT 'T|' || schemaname || '.' || relname FROM pg_stat_user_tables ORDER BY 1;
SELECT format('SELECT %%L, count(*) FROM %%I.%%I', 'C|' || s || '.' || t, s, t)
  FROM (SELECT schemaname s, relname t FROM pg_stat_user_tables ORDER BY pg_total_relation_size(relid) DESC, 1, 2 LIMIT %d) x \gexec
`, InventoryMax)
		return Command{Cmd: []string{"psql", "-v", "ON_ERROR_STOP=1", "-At", "-F", "|", "-U", user, "-d", c.DBName}, Stdin: sql}, nil
	case MySQL, MariaDB:
		f := myFamilies[engine]
		const from = " FROM information_schema.tables WHERE table_schema=%s AND table_type='BASE TABLE'"
		list := "SELECT CONCAT('T|', table_name)" + fmt.Sprintf(from, quoteLit(c.DBName)) + " ORDER BY 1"
		// Generates one "SELECT ... COUNT(*)" per table, which the second client then runs.
		gen := "SELECT CONCAT('SELECT CONCAT(''C|'',', QUOTE(table_name), ',''|'',COUNT(*)) FROM `', REPLACE(table_name, '`', '``'), '`;')" +
			fmt.Sprintf(from, quoteLit(c.DBName)) + fmt.Sprintf(" ORDER BY data_length+index_length DESC, table_name LIMIT %d", InventoryMax)
		script := fmt.Sprintf(`C="%[1]s -uroot -N -B"
$C -e %[2]s || exit 1
S=$($C -e %[3]s) || exit 1
printf '%%s\n' "$S" | $C %[4]s`, f.client, shQuote(list), shQuote(gen), shQuote(c.DBName))
		return Command{Cmd: []string{"sh", "-c", script}, Env: []string{"MYSQL_PWD=" + firstNonEmpty(c.RootPass, c.Password)}}, nil
	case Mongo:
		js := fmt.Sprintf(`const rows = [];
for (const d of db.adminCommand({listDatabases: 1, nameOnly: true}).databases) {
  if (['admin', 'config', 'local'].includes(d.name)) continue;
  const x = db.getSiblingDB(d.name);
  for (const c of x.getCollectionInfos({type: 'collection'})) {
    if (c.name.startsWith('system.')) continue;
    rows.push({ns: d.name + '.' + c.name, coll: x.getCollection(c.name), size: x.runCommand({collStats: c.name}).size || 0});
  }
}
rows.sort((a, b) => a.ns < b.ns ? -1 : 1);
for (const r of rows) print('T|' + r.ns);
rows.sort((a, b) => b.size - a.size || (a.ns < b.ns ? -1 : 1));
for (const r of rows.slice(0, %d)) print('C|' + r.ns + '|' + r.coll.countDocuments({}));
`, InventoryMax)
		return Command{Cmd: []string{"mongosh", "--quiet", "-u", c.Username, "-p", c.Password, "--authenticationDatabase", "admin", "--eval", js}}, nil
	}
	return Command{}, fmt.Errorf("%s has no inventory", engine)
}

type inventory struct {
	tables map[string]bool
	counts map[string]int64
}

func parseInventory(out string) inventory {
	inv := inventory{map[string]bool{}, map[string]int64{}}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "T|"):
			inv.tables[line[2:]] = true
		case strings.HasPrefix(line, "C|"):
			if i := strings.LastIndex(line, "|"); i > 2 {
				if n, err := strconv.ParseInt(line[i+1:], 10, 64); err == nil {
					inv.counts[line[2:i]] = n
				}
			}
		}
	}
	return inv
}

// CompareInventory checks that two Inventory outputs describe the same data.
func CompareInventory(src, dst string) error {
	a, b := parseInventory(src), parseInventory(dst)
	var lost, extra []string
	for t := range a.tables {
		if !b.tables[t] {
			lost = append(lost, t)
		}
	}
	for t := range b.tables {
		if !a.tables[t] {
			extra = append(extra, t)
		}
	}
	if len(lost)+len(extra) > 0 {
		slices.Sort(lost)
		slices.Sort(extra)
		return fmt.Errorf("tables differ: missing on the new server %v, unexpected on it %v", lost, extra)
	}
	for t, n := range a.counts {
		m, ok := b.counts[t]
		switch {
		case ok && m != n:
			return fmt.Errorf("table %s has %d rows on the old server and %d on the new one", t, n, m)
		case !ok && len(a.tables) <= InventoryMax:
			return fmt.Errorf("table %s was not counted on the new server", t)
		}
	}
	return nil
}

// CheckLogin returns a command that logs in as an extra user over the network (so the password is checked)
// and runs a trivial query on the database.
func CheckLogin(engine string, c Credentials, user, password string) (Command, error) {
	switch engine {
	case Postgres:
		// Through the container's own address: pg_hba trusts the loopback, not that one.
		return Command{Cmd: []string{"sh", "-c", `exec psql -v ON_ERROR_STOP=1 -At -h "$(hostname)" -U "$U" -d "$DB" -c 'SELECT 1'`},
			Env: []string{"PGPASSWORD=" + password, "U=" + user, "DB=" + c.DBName}}, nil
	case MySQL, MariaDB:
		f := myFamilies[engine]
		return Command{Cmd: f.cmd("-h127.0.0.1", "-u"+user, "-N", "-B", "-e", "SELECT 1", c.DBName), Env: []string{"MYSQL_PWD=" + password}}, nil
	}
	return Command{}, fmt.Errorf("extra users are supported for PostgreSQL and MySQL")
}

// Extensions returns a command listing the extensions a PostgreSQL database uses (available=false)
// or the ones a server could install (available=true); ok is false for the other engines.
func Extensions(engine string, shared bool, c Credentials, available bool) (cmd Command, ok bool) {
	if engine != Postgres {
		return Command{}, false
	}
	user := c.Username
	if shared {
		user = "postgres"
	}
	if available {
		return Command{Cmd: []string{"psql", "-At", "-U", user, "-d", "postgres", "-c", "SELECT name FROM pg_available_extensions ORDER BY 1"}}, true
	}
	return Command{Cmd: []string{"psql", "-At", "-U", user, "-d", c.DBName, "-c", "SELECT extname FROM pg_extension WHERE extname <> 'plpgsql' ORDER BY 1"}}, true
}

// MissingExtensions lists the installed extensions (one per line) that the other list lacks.
func MissingExtensions(installed, available string) []string {
	have := map[string]bool{}
	for _, l := range strings.Fields(available) {
		have[l] = true
	}
	var out []string
	for _, l := range strings.Fields(installed) {
		if !have[l] {
			out = append(out, l)
		}
	}
	return out
}
