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
	"postgres": {
		dumpScript:    `PGPASSWORD="$POSTGRES_PASSWORD" pg_dump --clean --if-exists -h 127.0.0.1 -U "$POSTGRES_USER" "$POSTGRES_DB"`,
		restoreScript: `PGPASSWORD="$POSTGRES_PASSWORD" psql -q -h 127.0.0.1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -f -`,
	},
	"mysql": {
		dumpScript:    `MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysqldump -uroot --databases "$MYSQL_DATABASE" --add-drop-database`,
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
