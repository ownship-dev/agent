package executors

// Scripts run inside the service's container, so passwords stay in its env. Engine ids must
// match packages/core/src/services.ts.
type engineBackup struct {
	dumpScript    string
	restoreScript string
	// Redis has no online restore, so restores replace a file with the workload stopped.
	restoreVolumeFile string
}

var engineBackups = map[string]engineBackup{
	// Every database on the server, not one: users create their own from the studio. An older
	// single-database dump still restores into POSTGRES_DB, which only older services set.
	"postgres": {
		dumpScript:    `PGPASSWORD="$POSTGRES_PASSWORD" pg_dumpall --clean --if-exists -h 127.0.0.1 -U "${POSTGRES_USER:-postgres}"`,
		restoreScript: `PGPASSWORD="$POSTGRES_PASSWORD" psql -q -h 127.0.0.1 -U "${POSTGRES_USER:-postgres}" -d "${POSTGRES_DB:-postgres}" -f -`,
	},
	"mysql": {
		dumpScript: `export MYSQL_PWD="$MYSQL_ROOT_PASSWORD"; ` +
			`dbs=$(mysql -uroot -N -B -e "select schema_name from information_schema.schemata where schema_name not in ('mysql','information_schema','performance_schema','sys')") || exit 1; ` +
			`if [ -z "$dbs" ]; then echo '-- no user databases'; else mysqldump -uroot --add-drop-database --routines --events --databases $dbs; fi`,
		restoreScript: `MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql -uroot`,
	},
	"mongo": {
		dumpScript:    `mongodump --quiet --archive -u "$MONGO_INITDB_ROOT_USERNAME" -p "$MONGO_INITDB_ROOT_PASSWORD"`,
		restoreScript: `mongorestore --quiet --archive --drop -u "$MONGO_INITDB_ROOT_USERNAME" -p "$MONGO_INITDB_ROOT_PASSWORD"`,
	},
	"redis": {
		// SAVE is synchronous, so the rdb is a consistent snapshot.
		dumpScript:        `redis-cli -a "$REDIS_PASSWORD" --no-auth-warning SAVE >/dev/null && cat /data/dump.rdb`,
		restoreVolumeFile: "dump.rdb",
	},
}
