package executors

import (
	"context"
	"errors"
	"strings"
	"testing"

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
