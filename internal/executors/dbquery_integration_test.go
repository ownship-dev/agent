package executors

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"

	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
)

// Opt-in: DBQUERY_TEST_POSTGRES / DBQUERY_TEST_MYSQL hold a URL like
// postgres://user:pw@host:5432/db. Only temporary tables are touched.
func targetFromEnv(t *testing.T, key string) dbTarget {
	raw := os.Getenv(key)
	if raw == "" {
		t.Skipf("%s not set", key)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	pw, _ := u.User.Password()
	return dbTarget{addr: u.Host, user: u.User.Username(), password: pw, database: strings.TrimPrefix(u.Path, "/")}
}

func stmts(sqls ...string) []*agentv1.DbStatement {
	out := make([]*agentv1.DbStatement, len(sqls))
	for i, s := range sqls {
		out[i] = &agentv1.DbStatement{Sql: s}
	}
	return out
}

func TestDbQueryPostgresLive(t *testing.T) {
	target := targetFromEnv(t, "DBQUERY_TEST_POSTGRES")
	ctx := context.Background()
	cmd := &agentv1.DbQuery{Statements: append(stmts(
		"create temp table studio_t (id serial primary key, name text, tags text[], data jsonb, raw bytea)",
		"insert into studio_t (name, tags, data, raw) values ('a', '{x,y}', '{\"k\":1}', '\\xff00'), (null, null, null, null)",
	), &agentv1.DbStatement{
		Sql:          "update studio_t set name = $1 where id = $2",
		Params:       []*agentv1.DbValue{{Text: "renamed"}, {Text: "1"}},
		ExpectOneRow: true,
	}, &agentv1.DbStatement{Sql: "select * from studio_t order by id"})}
	res := &agentv1.DbQueryResult{}
	if err := runPostgres(ctx, target, cmd, res); err != nil || res.GetError() != "" {
		t.Fatalf("run: %v %s %s", err, res.GetError(), res.GetErrorDetail())
	}
	sel := res.GetResults()[3]
	if got := sel.GetColumns()[2].GetType(); got != "_text" && got != "text[]" {
		t.Errorf("array column type = %q", got)
	}
	row := sel.GetRows()[0].GetCells()
	if row[1].GetText() != "renamed" || row[2].GetText() != "{x,y}" || row[4].GetText() != `\xff00` {
		t.Errorf("unexpected row: %v", row)
	}
	if !sel.GetRows()[1].GetCells()[1].GetNull() {
		t.Error("NULL lost")
	}
	if res.GetResults()[1].GetRowsAffected() != 2 {
		t.Error("insert count lost")
	}

	bad := &agentv1.DbQueryResult{}
	_ = runPostgres(ctx, target, &agentv1.DbQuery{Transaction: true, Statements: stmts(
		"create temp table studio_r (id int)", "insert into studio_r values (1)", "select nope",
	)}, bad)
	if bad.GetError() == "" || bad.GetErrorStatement() != 2 || !strings.Contains(bad.GetErrorDetail(), "SQLSTATE") {
		t.Errorf("failure not reported: %+v", bad)
	}

	ro := &agentv1.DbQueryResult{}
	_ = runPostgres(ctx, target, &agentv1.DbQuery{ReadOnly: true, Statements: stmts("create temp table studio_ro (id int)")}, ro)
	if !strings.Contains(ro.GetError(), "read-only") {
		t.Errorf("read-only not enforced: %q", ro.GetError())
	}

	miss := &agentv1.DbQueryResult{}
	_ = runPostgres(ctx, target, &agentv1.DbQuery{Statements: append(stmts("create temp table studio_m (id int)"),
		&agentv1.DbStatement{Sql: "delete from studio_m where id = 1", ExpectOneRow: true})}, miss)
	if !strings.Contains(miss.GetError(), "exactly 1 row") {
		t.Errorf("expect_one_row not enforced: %q", miss.GetError())
	}
}

func TestDbQueryMySQLLive(t *testing.T) {
	target := targetFromEnv(t, "DBQUERY_TEST_MYSQL")
	ctx := context.Background()
	cmd := &agentv1.DbQuery{Statements: append(stmts(
		"create temporary table studio_t (id int auto_increment primary key, name varchar(20), raw varbinary(4))",
		"insert into studio_t (name, raw) values ('a', 0xff00), (null, null)",
	), &agentv1.DbStatement{
		Sql:          "update studio_t set name = ? where id = ?",
		Params:       []*agentv1.DbValue{{Text: "renamed"}, {Text: "1"}},
		ExpectOneRow: true,
	}, &agentv1.DbStatement{Sql: "select * from studio_t order by id"})}
	res := &agentv1.DbQueryResult{}
	if err := runMySQL(ctx, target, cmd, res); err != nil || res.GetError() != "" {
		t.Fatalf("run: %v %s", err, res.GetError())
	}
	sel := res.GetResults()[3]
	row := sel.GetRows()[0].GetCells()
	if row[1].GetText() != "renamed" || !row[2].GetBinary() || row[2].GetText() != "ff00" {
		t.Errorf("unexpected row: %v", row)
	}
	if !sel.GetRows()[1].GetCells()[1].GetNull() || res.GetResults()[1].GetRowsAffected() != 2 {
		t.Error("NULL or insert count lost")
	}
	ro := &agentv1.DbQueryResult{}
	_ = runMySQL(ctx, target, &agentv1.DbQuery{ReadOnly: true, Statements: stmts("create table studio_ro (id int)")}, ro)
	if ro.GetError() == "" {
		t.Error("read-only not enforced")
	}
}

func argvStmt(argv ...string) *agentv1.DbStatement {
	params := make([]*agentv1.DbValue, len(argv))
	for i, a := range argv {
		params[i] = &agentv1.DbValue{Text: a}
	}
	return &agentv1.DbStatement{Params: params}
}

func TestDbQueryRedisLive(t *testing.T) {
	target := targetFromEnv(t, "DBQUERY_TEST_REDIS")
	ctx := context.Background()
	res := &agentv1.DbQueryResult{}
	err := runRedis(ctx, target, &agentv1.DbQuery{Database: "3", Statements: []*agentv1.DbStatement{
		argvStmt("DEL", "studio:h"), argvStmt("HSET", "studio:h", "a", "1", "b", "x y"),
		argvStmt("HGETALL", "studio:h"), argvStmt("GET", "studio:missing"), argvStmt("LPUSH", "studio:h", "v"),
	}}, res)
	if err != nil || res.GetError() != "" {
		t.Fatalf("run: %v %s", err, res.GetError())
	}
	cell := func(i int) string { return res.GetResults()[i].GetRows()[0].GetCells()[0].GetText() }
	if cell(1) != "2" || cell(2) != `["a","1","b","x y"]` || cell(3) != "null" || !strings.Contains(cell(4), "$err") {
		t.Errorf("replies: %s %s %s %s", cell(1), cell(2), cell(3), cell(4))
	}
	ro := &agentv1.DbQueryResult{}
	_ = runRedis(ctx, target, &agentv1.DbQuery{ReadOnly: true, Statements: []*agentv1.DbStatement{argvStmt("FLUSHALL")}}, ro)
	if !strings.Contains(ro.GetError(), "read-only") {
		t.Errorf("read-only not enforced: %q", ro.GetError())
	}
}

func TestDbQueryMongoLive(t *testing.T) {
	target := targetFromEnv(t, "DBQUERY_TEST_MONGO")
	ctx := context.Background()
	res := &agentv1.DbQueryResult{}
	err := runMongo(ctx, target, &agentv1.DbQuery{Database: "studio_test", Statements: stmts(
		`{"drop": "c"}`,
		`{"insert": "c", "documents": [{"n": 1, "at": {"$date": "2024-01-02T03:04:05Z"}}]}`,
		`{"find": "c", "filter": {"n": 1}, "singleBatch": true}`,
	)}, res)
	if err != nil {
		t.Fatal(err)
	}
	// The drop fails on a fresh database (ns not found); only what follows matters.
	if res.GetError() != "" && res.GetErrorStatement() != 0 {
		t.Fatalf("run: %s", res.GetError())
	}
	if res.GetError() != "" {
		res = &agentv1.DbQueryResult{}
		_ = runMongo(ctx, target, &agentv1.DbQuery{Database: "studio_test", Statements: stmts(
			`{"insert": "c", "documents": [{"n": 1, "at": {"$date": "2024-01-02T03:04:05Z"}}]}`,
			`{"find": "c", "filter": {"n": 1}, "singleBatch": true}`,
		)}, res)
	}
	found := res.GetResults()[len(res.GetResults())-1].GetRows()[0].GetCells()[0].GetText()
	if !strings.Contains(found, `"$date"`) || !strings.Contains(found, `"$oid"`) {
		t.Errorf("find reply lost types: %s", found)
	}
	ro := &agentv1.DbQueryResult{}
	_ = runMongo(ctx, target, &agentv1.DbQuery{Database: "studio_test", ReadOnly: true, Statements: stmts(`{"dropDatabase": 1}`)}, ro)
	if !strings.Contains(ro.GetError(), "read-only") {
		t.Errorf("read-only not enforced: %q", ro.GetError())
	}
}
