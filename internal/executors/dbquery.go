package executors

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/sync/singleflight"

	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
)

const (
	dbDefaultMaxRows = 1000
	dbHardMaxRows    = 10000
	dbDefaultTimeout = 30 * time.Second
	dbHardMaxTimeout = 5 * time.Minute
	dbMaxCellBytes   = 64 * 1024
	dbMaxResultBytes = 8 << 20
	// A failed connect evicts the entry anyway, so a long life only saves cold lookups.
	dbCredentialMaxAge = 30 * time.Minute
)

// Credentials come from the service container's env, never the control-plane stream.
var dbCredentialScripts = map[string]string{
	"postgres": `printf '%s\n%s\n%s' "${POSTGRES_USER:-postgres}" "$POSTGRES_PASSWORD" "${POSTGRES_DB:-${POSTGRES_USER:-postgres}}"`,
	"mysql":    `printf '%s\n%s\n%s' root "$MYSQL_ROOT_PASSWORD" "$MYSQL_DATABASE"`,
	"redis":    `printf '%s\n%s\n%s' default "$REDIS_PASSWORD" 0`,
	"mongo":    `printf '%s\n%s\n%s' "$MONGO_INITDB_ROOT_USERNAME" "$MONGO_INITDB_ROOT_PASSWORD" admin`,
}

var dbRunners = map[string]func(context.Context, dbTarget, *agentv1.DbQuery, *agentv1.DbQueryResult) error{
	"postgres": runPostgres,
	"mysql":    runMySQL,
	"redis":    runRedis,
	"mongo":    runMongo,
}

type dbTarget struct {
	addr, user, password, database string
	at                             time.Time
}

type dbTargetCache struct {
	mu sync.Mutex
	m  map[string]dbTarget
	// The studio fires several requests at once; one cold lookup serves them all.
	resolving singleflight.Group
}

var dbTargets = &dbTargetCache{m: map[string]dbTarget{}}

// dbConnectError marks a failure before any statement ran, so a retry can't replay a write.
type dbConnectError struct{ err error }

func (e *dbConnectError) Error() string { return e.err.Error() }
func (e *dbConnectError) Unwrap() error { return e.err }

func connectFailed(err error) error {
	if err == nil {
		return nil
	}
	return &dbConnectError{err}
}

func (c *dbTargetCache) get(id string) (dbTarget, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.m[id]
	if !ok || time.Since(t.at) > dbCredentialMaxAge {
		return dbTarget{}, false
	}
	return t, true
}

func (c *dbTargetCache) put(id string, t dbTarget) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[id] = t
}

func (c *dbTargetCache) drop(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, id)
}

// DbQuery uses a native driver, so values keep their types and NULLs.
func (d *Deployer) DbQuery(ctx context.Context, cmd *agentv1.DbQuery, emit Emit) error {
	res := &agentv1.DbQueryResult{RequestId: cmd.GetRequestId()}
	reply := func() error {
		return emit(&agentv1.Event{Payload: &agentv1.Event_DbQueryResult{DbQueryResult: res}})
	}
	if _, ok := dbCredentialScripts[cmd.GetEngine()]; !ok {
		res.Error = fmt.Sprintf("the %q engine has no studio", cmd.GetEngine())
		return reply()
	}
	if len(cmd.GetStatements()) == 0 {
		res.Error = "nothing to run"
		return reply()
	}

	timeout := dbDefaultTimeout
	if s := cmd.GetTimeoutSeconds(); s > 0 {
		timeout = min(time.Duration(s)*time.Second, dbHardMaxTimeout)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	target, cached, err := d.dbTarget(ctx, cmd)
	if err != nil {
		res.Error = err.Error()
		return reply()
	}
	err = dbRunners[cmd.GetEngine()](ctx, target, cmd, res)
	var connErr *dbConnectError
	if err != nil && cached && errors.As(err, &connErr) && ctx.Err() == nil {
		// A restarted container moves its address or password; look both up again once.
		dbTargets.drop(cmd.GetServiceId())
		if target, _, err = d.dbTarget(ctx, cmd); err != nil {
			res.Error = err.Error()
			return reply()
		}
		res.Results, res.Error, res.ErrorDetail, res.ErrorStatement = nil, "", "", 0
		err = dbRunners[cmd.GetEngine()](ctx, target, cmd, res)
	}
	if err != nil {
		// Stale address or rotated password: re-read both next time.
		dbTargets.drop(cmd.GetServiceId())
		if res.Error == "" {
			res.Error = err.Error()
		}
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) && res.Error != "" {
		res.Error = fmt.Sprintf("timed out after %s: %s", timeout, res.Error)
	}
	return reply()
}

// dbTarget reports whether the target came from the cache, so only a stale one is retried.
func (d *Deployer) dbTarget(ctx context.Context, cmd *agentv1.DbQuery) (dbTarget, bool, error) {
	if t, ok := dbTargets.get(cmd.GetServiceId()); ok {
		return t, true, nil
	}
	v, err, _ := dbTargets.resolving.Do(cmd.GetServiceId(), func() (any, error) {
		return d.resolveDbTarget(ctx, cmd)
	})
	if err != nil {
		return dbTarget{}, false, err
	}
	return v.(dbTarget), false, nil
}

func (d *Deployer) resolveDbTarget(ctx context.Context, cmd *agentv1.DbQuery) (dbTarget, error) {
	addr, err := d.orch.DialAddr(ctx, cmd.GetServiceId(), cmd.GetPort())
	if err != nil {
		return dbTarget{}, fmt.Errorf("database is not reachable: %w", err)
	}
	creds, err := d.orch.AppExecCmd(ctx, cmd.GetServiceId(), dbCredentialScripts[cmd.GetEngine()])
	if err != nil {
		return dbTarget{}, err
	}
	out, err := creds.Output()
	if err != nil {
		return dbTarget{}, fmt.Errorf("read database credentials: %s", firstLine(string(out), err))
	}
	parts := strings.SplitN(string(out), "\n", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" {
		return dbTarget{}, fmt.Errorf("database container has no credentials in its environment")
	}
	t := dbTarget{addr: addr, user: parts[0], password: parts[1], database: parts[2], at: time.Now()}
	dbTargets.put(cmd.GetServiceId(), t)
	return t, nil
}

// resultBudget keeps a reply well under the stream's frame limit.
type resultBudget struct {
	maxRows int
	bytes   int
}

func newBudget(cmd *agentv1.DbQuery) *resultBudget {
	rows := int(cmd.GetMaxRows())
	if rows <= 0 {
		rows = dbDefaultMaxRows
	}
	return &resultBudget{maxRows: min(rows, dbHardMaxRows)}
}

// cell returns ok=false once the byte budget is spent.
func (b *resultBudget) cell(raw []byte, isNull bool) (*agentv1.DbCell, bool) {
	if isNull {
		return &agentv1.DbCell{Null: true}, true
	}
	c := &agentv1.DbCell{}
	if !utf8.Valid(raw) {
		c.Binary = true
		// Hex doubles the size, so binary is cut at half the cap.
		if len(raw) > dbMaxCellBytes/2 {
			raw, c.Truncated = raw[:dbMaxCellBytes/2], true
		}
		c.Text = hex.EncodeToString(raw)
	} else {
		text := string(raw)
		if len(text) > dbMaxCellBytes {
			cut := dbMaxCellBytes
			// Cut on a rune boundary so the text stays valid UTF-8.
			for cut > 0 && !utf8.RuneStart(text[cut]) {
				cut--
			}
			text, c.Truncated = text[:cut], true
		}
		c.Text = text
	}
	b.bytes += len(c.Text) + 8
	return c, b.bytes <= dbMaxResultBytes
}

func runPostgres(ctx context.Context, t dbTarget, cmd *agentv1.DbQuery, res *agentv1.DbQueryResult) error {
	database := t.database
	if cmd.GetDatabase() != "" {
		database = cmd.GetDatabase()
	}
	dsn := (&url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(t.user, t.password),
		Host:     t.addr,
		Path:     "/" + database,
		RawQuery: "sslmode=disable&connect_timeout=10&application_name=ownship-studio",
	}).String()
	conn, err := pgconn.Connect(ctx, dsn)
	if err != nil {
		return connectFailed(err)
	}
	// A fresh ctx still sends a clean Terminate if the request expired.
	defer conn.Close(context.Background())

	budget := newBudget(cmd)
	types := pgtype.NewMap()
	exec := func(i int, st *agentv1.DbStatement) error {
		start := time.Now()
		params := make([][]byte, len(st.GetParams()))
		for j, p := range st.GetParams() {
			if !p.GetNull() {
				params[j] = []byte(p.GetText())
			}
		}
		rr := conn.ExecParams(ctx, st.GetSql(), params, nil, nil, nil)
		set := &agentv1.DbResultSet{}
		fields := rr.FieldDescriptions()
		var unknown []uint32
		for _, f := range fields {
			col := &agentv1.DbColumn{Name: f.Name}
			if typ, ok := types.TypeForOID(f.DataTypeOID); ok {
				col.Type = typ.Name
			} else {
				unknown = append(unknown, f.DataTypeOID)
			}
			set.Columns = append(set.Columns, col)
		}
		full := false
		for rr.NextRow() {
			// Rows past the cap are still drained: cancelling would abort a RETURNING write.
			if full || len(set.Rows) >= budget.maxRows {
				set.Truncated = true
				continue
			}
			row := &agentv1.DbRow{}
			for _, v := range rr.Values() {
				c, ok := budget.cell(v, v == nil)
				row.Cells = append(row.Cells, c)
				full = full || !ok
			}
			set.Rows = append(set.Rows, row)
		}
		tag, err := rr.Close()
		if err != nil {
			res.ErrorStatement = uint32(i)
			res.Error, res.ErrorDetail = pgErrorText(err)
			return err
		}
		set.Command = tag.String()
		set.RowsAffected = tag.RowsAffected()
		set.DurationMs = uint32(time.Since(start).Milliseconds())
		if len(unknown) > 0 {
			resolvePgTypes(ctx, conn, fields, set.Columns)
		}
		if st.GetExpectOneRow() && set.RowsAffected != 1 {
			res.ErrorStatement = uint32(i)
			res.Error = fmt.Sprintf("expected to change exactly 1 row, but %d matched: the row changed or is gone; reload and retry", set.RowsAffected)
			return errors.New(res.Error)
		}
		res.Results = append(res.Results, set)
		return nil
	}
	return runStatements(ctx, cmd, "BEGIN READ ONLY", func(sqlText string) error {
		return conn.Exec(ctx, sqlText).Close()
	}, exec)
}

// resolvePgTypes names user types (enums, domains, extensions) the driver can't decode.
func resolvePgTypes(ctx context.Context, conn *pgconn.PgConn, fields []pgconn.FieldDescription, cols []*agentv1.DbColumn) {
	for i, f := range fields {
		if cols[i].Type != "" {
			continue
		}
		rr := conn.ExecParams(ctx, "select format_type($1::oid, null)", [][]byte{[]byte(strconv.FormatUint(uint64(f.DataTypeOID), 10))}, nil, nil, nil)
		for rr.NextRow() {
			cols[i].Type = string(rr.Values()[0])
		}
		if _, err := rr.Close(); err != nil || cols[i].Type == "" {
			cols[i].Type = "oid " + strconv.FormatUint(uint64(f.DataTypeOID), 10)
		}
	}
}

func pgErrorText(err error) (string, string) {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err.Error(), ""
	}
	var detail []string
	if pgErr.Detail != "" {
		detail = append(detail, pgErr.Detail)
	}
	if pgErr.Hint != "" {
		detail = append(detail, "Hint: "+pgErr.Hint)
	}
	if pgErr.Position > 0 {
		detail = append(detail, fmt.Sprintf("at character %d", pgErr.Position))
	}
	detail = append(detail, "SQLSTATE "+pgErr.Code)
	return pgErr.Severity + ": " + pgErr.Message, strings.Join(detail, " · ")
}

func runMySQL(ctx context.Context, t dbTarget, cmd *agentv1.DbQuery, res *agentv1.DbQueryResult) error {
	cfg := mysql.NewConfig()
	cfg.User, cfg.Passwd, cfg.Net, cfg.Addr = t.user, t.password, "tcp", t.addr
	cfg.DBName = t.database
	if cmd.GetDatabase() != "" {
		cfg.DBName = cmd.GetDatabase()
	}
	cfg.Timeout = 10 * time.Second
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		return err
	}
	db := sql.OpenDB(connector)
	defer db.Close()
	// One connection, so a transaction spans every statement.
	conn, err := db.Conn(ctx)
	if err == nil {
		// db.Conn can hand back a connection that only fails on first use.
		err = conn.PingContext(ctx)
	}
	if err != nil {
		return connectFailed(err)
	}
	defer conn.Close()

	budget := newBudget(cmd)
	exec := func(i int, st *agentv1.DbStatement) error {
		start := time.Now()
		args := make([]any, len(st.GetParams()))
		for j, p := range st.GetParams() {
			if !p.GetNull() {
				args[j] = p.GetText()
			}
		}
		set := &agentv1.DbResultSet{}
		fail := func(err error) error {
			res.ErrorStatement = uint32(i)
			res.Error = err.Error()
			return err
		}
		if returnsRows(st.GetSql()) {
			rows, err := conn.QueryContext(ctx, st.GetSql(), args...)
			if err != nil {
				return fail(err)
			}
			defer rows.Close()
			types, err := rows.ColumnTypes()
			if err != nil {
				return fail(err)
			}
			for _, ct := range types {
				set.Columns = append(set.Columns, &agentv1.DbColumn{Name: ct.Name(), Type: strings.ToLower(ct.DatabaseTypeName())})
			}
			raw := make([]sql.RawBytes, len(types))
			dest := make([]any, len(types))
			for j := range raw {
				dest[j] = &raw[j]
			}
			full := false
			for rows.Next() {
				if full || len(set.Rows) >= budget.maxRows {
					set.Truncated = true
					continue
				}
				if err := rows.Scan(dest...); err != nil {
					return fail(err)
				}
				row := &agentv1.DbRow{}
				for _, v := range raw {
					c, ok := budget.cell(v, v == nil)
					row.Cells = append(row.Cells, c)
					full = full || !ok
				}
				set.Rows = append(set.Rows, row)
			}
			if err := rows.Err(); err != nil {
				return fail(err)
			}
			set.Command = fmt.Sprintf("%s %d", firstKeyword(st.GetSql()), len(set.Rows))
		} else {
			r, err := conn.ExecContext(ctx, st.GetSql(), args...)
			if err != nil {
				return fail(err)
			}
			set.RowsAffected, _ = r.RowsAffected()
			set.Command = fmt.Sprintf("%s %d", firstKeyword(st.GetSql()), set.RowsAffected)
		}
		set.DurationMs = uint32(time.Since(start).Milliseconds())
		if st.GetExpectOneRow() && set.RowsAffected != 1 {
			return fail(fmt.Errorf("expected to change exactly 1 row, but %d matched: the row changed or is gone; reload and retry", set.RowsAffected))
		}
		res.Results = append(res.Results, set)
		return nil
	}
	// DDL commits implicitly, so the session itself is made read-only.
	if cmd.GetReadOnly() {
		if _, err := conn.ExecContext(ctx, "SET SESSION TRANSACTION READ ONLY"); err != nil {
			return err
		}
	}
	return runStatements(ctx, cmd, "START TRANSACTION READ ONLY", func(sqlText string) error {
		_, err := conn.ExecContext(ctx, sqlText)
		return err
	}, exec)
}

// runStatements stops at the first failure and rolls back a transactional request.
func runStatements(ctx context.Context, cmd *agentv1.DbQuery, beginReadOnly string, raw func(string) error, exec func(int, *agentv1.DbStatement) error) error {
	if !cmd.GetTransaction() && !cmd.GetReadOnly() {
		for i, st := range cmd.GetStatements() {
			// nil so a bad query doesn't evict cached credentials.
			if err := exec(i, st); err != nil {
				return nil
			}
		}
		return nil
	}
	begin := "BEGIN"
	if cmd.GetReadOnly() {
		begin = beginReadOnly
	}
	if err := raw(begin); err != nil {
		return err
	}
	for i, st := range cmd.GetStatements() {
		if err := exec(i, st); err != nil {
			_ = raw("ROLLBACK")
			return nil
		}
	}
	return raw("COMMIT")
}

// MySQL can't tell ahead of time whether a statement returns rows, so decide by keyword.
var rowKeywords = map[string]bool{
	"SELECT": true, "SHOW": true, "DESCRIBE": true, "DESC": true, "EXPLAIN": true, "WITH": true,
	"VALUES": true, "TABLE": true, "CALL": true, "HELP": true, "ANALYZE": true, "CHECK": true,
	"CHECKSUM": true, "OPTIMIZE": true, "REPAIR": true, "(": true,
}

func returnsRows(sqlText string) bool {
	return rowKeywords[firstKeyword(sqlText)]
}

func firstKeyword(s string) string {
	for {
		s = strings.TrimLeft(s, " \t\r\n")
		switch {
		case strings.HasPrefix(s, "--"), strings.HasPrefix(s, "#"):
			nl := strings.IndexByte(s, '\n')
			if nl < 0 {
				return ""
			}
			s = s[nl+1:]
		case strings.HasPrefix(s, "/*"):
			end := strings.Index(s, "*/")
			if end < 0 {
				return ""
			}
			s = s[end+2:]
		case strings.HasPrefix(s, "("):
			return "("
		default:
			end := strings.IndexFunc(s, func(r rune) bool {
				return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '_')
			})
			if end < 0 {
				end = len(s)
			}
			return strings.ToUpper(s[:end])
		}
	}
}
