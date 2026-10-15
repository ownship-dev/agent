package executors

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
)

func TestFirstKeyword(t *testing.T) {
	cases := map[string]string{
		"select 1":                             "SELECT",
		"  \n-- note\nshow tables":             "SHOW",
		"/* a */ /* b */ update t set a=1":     "UPDATE",
		"# mysql comment\nDESC t":              "DESC",
		"(select 1) union (select 2)":          "(",
		"with x as (select 1) select * from x": "WITH",
		"-- only a comment":                    "",
	}
	for in, want := range cases {
		if got := firstKeyword(in); got != want {
			t.Errorf("firstKeyword(%q) = %q, want %q", in, got, want)
		}
	}
	if returnsRows("insert into t values (1)") || !returnsRows("explain select 1") {
		t.Fatal("returnsRows misclassified a statement")
	}
}

func TestBudgetCell(t *testing.T) {
	b := &resultBudget{maxRows: 10}
	if c, _ := b.cell(nil, true); !c.GetNull() {
		t.Fatal("null lost")
	}
	if c, _ := b.cell([]byte{}, false); c.GetNull() || c.GetText() != "" {
		t.Fatal("empty string must stay distinct from NULL")
	}
	if c, _ := b.cell([]byte{0xff, 0x00}, false); !c.GetBinary() || c.GetText() != "ff00" {
		t.Fatalf("binary value not hex encoded: %+v", c)
	}
	long := strings.Repeat("é", dbMaxCellBytes)
	c, _ := b.cell([]byte(long), false)
	if !c.GetTruncated() || len(c.GetText()) > dbMaxCellBytes || !strings.HasSuffix(c.GetText(), "é") {
		t.Fatal("long cell must be cut on a rune boundary")
	}
}

func TestRunStatementsRollsBackOnFailure(t *testing.T) {
	var log []string
	raw := func(s string) error { log = append(log, s); return nil }
	exec := func(i int, st *agentv1.DbStatement) error {
		log = append(log, st.GetSql())
		if st.GetSql() == "bad" {
			return errors.New("boom")
		}
		return nil
	}
	cmd := &agentv1.DbQuery{Transaction: true, Statements: []*agentv1.DbStatement{{Sql: "a"}, {Sql: "bad"}, {Sql: "c"}}}
	if err := runStatements(context.Background(), cmd, "BEGIN READ ONLY", raw, exec); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(log, ","); got != "BEGIN,a,bad,ROLLBACK" {
		t.Fatalf("got %s", got)
	}

	log = nil
	cmd.Transaction = false
	_ = runStatements(context.Background(), cmd, "BEGIN READ ONLY", raw, exec)
	if got := strings.Join(log, ","); got != "a,bad" {
		t.Fatalf("autocommit must stop at the failure, got %s", got)
	}

	log = nil
	cmd.ReadOnly, cmd.Statements = true, cmd.Statements[:1]
	_ = runStatements(context.Background(), cmd, "BEGIN READ ONLY", raw, exec)
	if got := strings.Join(log, ","); got != "BEGIN READ ONLY,a,COMMIT" {
		t.Fatalf("read-only must open a read-only transaction, got %s", got)
	}
}

type stubDbOrch struct {
	Orchestrator
	dials int
}

func (o *stubDbOrch) DialAddr(context.Context, string, uint32) (string, error) {
	o.dials++
	return fmt.Sprintf("10.0.0.%d:5432", o.dials), nil
}

func (o *stubDbOrch) AppExecCmd(ctx context.Context, _, _ string) (*exec.Cmd, error) {
	return exec.CommandContext(ctx, "printf", `u\npw\ndb`), nil
}

func TestDbQueryRetriesAStaleCachedTargetOnce(t *testing.T) {
	orch := &stubDbOrch{}
	d := &Deployer{orch: orch}
	var addrs []string
	prev := dbRunners["postgres"]
	t.Cleanup(func() { dbRunners["postgres"] = prev; dbTargets.drop("svc-retry") })
	dbRunners["postgres"] = func(_ context.Context, tg dbTarget, _ *agentv1.DbQuery, res *agentv1.DbQueryResult) error {
		addrs = append(addrs, tg.addr)
		if tg.addr == "10.0.0.1:5432" {
			return connectFailed(errors.New("connection refused"))
		}
		res.Results = append(res.Results, &agentv1.DbResultSet{Command: "SELECT 1"})
		return nil
	}
	var got *agentv1.DbQueryResult
	emit := func(e *agentv1.Event) error { got = e.GetDbQueryResult(); return nil }
	cmd := &agentv1.DbQuery{ServiceId: "svc-retry", Engine: "postgres", Statements: []*agentv1.DbStatement{{Sql: "select 1"}}}

	// Cold: a fresh lookup that fails is reported, not retried.
	if err := d.DbQuery(context.Background(), cmd, emit); err != nil {
		t.Fatal(err)
	}
	if got.GetError() == "" || len(addrs) != 1 {
		t.Fatalf("a fresh target must not be retried: %v %q", addrs, got.GetError())
	}

	// Warm but stale: the cached address fails to connect, so it is looked up again once.
	dbTargets.put("svc-retry", dbTarget{addr: "10.0.0.1:5432", user: "u", password: "pw", at: time.Now()})
	orch.dials = 1
	addrs = nil
	if err := d.DbQuery(context.Background(), cmd, emit); err != nil {
		t.Fatal(err)
	}
	if got.GetError() != "" || len(got.GetResults()) != 1 || strings.Join(addrs, ",") != "10.0.0.1:5432,10.0.0.2:5432" {
		t.Fatalf("stale target not retried: %v %q", addrs, got.GetError())
	}
}
