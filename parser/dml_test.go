// dml_test.go: hand-written shape tests for the milestone-4 DML
// transformers. The corpus and serialize goldens gate accept/reject and
// tree shape wholesale; these tests pin the Go-side AST surface a
// consumer (sqlc) programs against.
package parser

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/sqlc-dev/darkwing/ast"
)

func parseOne(t *testing.T, sql string) ast.Stmt {
	t.Helper()
	stmts, err := Parse(context.Background(), strings.NewReader(sql))
	if err != nil {
		t.Fatalf("Parse(%q): %v", sql, err)
	}
	if len(stmts) != 1 {
		t.Fatalf("Parse(%q): got %d statements, want 1", sql, len(stmts))
	}
	return stmts[0]
}

func parseErr(t *testing.T, sql string) string {
	t.Helper()
	_, err := Parse(context.Background(), strings.NewReader(sql))
	if err == nil {
		t.Fatalf("Parse(%q): expected error", sql)
	}
	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("Parse(%q): got %T (%v), want *parser.Error", sql, err, err)
	}
	return pe.Msg
}

func TestInsertShape(t *testing.T) {
	stmt := parseOne(t, "INSERT INTO db.s.t (a, b) VALUES (1, 2), (3, 4) RETURNING a + 1 AS x")
	ins, ok := stmt.(*ast.InsertStatement)
	if !ok {
		t.Fatalf("got %T, want *ast.InsertStatement", stmt)
	}
	if ins.Catalog != "db" || ins.Schema != "s" || ins.Table != "t" {
		t.Errorf("target = %q.%q.%q, want db.s.t", ins.Catalog, ins.Schema, ins.Table)
	}
	if !reflect.DeepEqual(ins.Columns, []string{"a", "b"}) {
		t.Errorf("columns = %v", ins.Columns)
	}
	if ins.Query == nil {
		t.Fatal("no select_statement for VALUES")
	}
	values, ok := ins.Query.Node.(*ast.SelectNode).FromTable.(*ast.ExpressionListRef)
	if !ok {
		t.Fatalf("VALUES did not become an expression list ref")
	}
	if len(values.Values) != 2 || len(values.Values[0]) != 2 {
		t.Errorf("values shape = %d rows", len(values.Values))
	}
	if len(ins.Returning) != 1 || ins.Returning[0].GetAlias() != "x" {
		t.Errorf("returning = %#v", ins.Returning)
	}
	if ins.OnConflict != nil || ins.DefaultValues {
		t.Errorf("unexpected on-conflict/default-values")
	}
}

func TestInsertOnConflict(t *testing.T) {
	stmt := parseOne(t, "INSERT INTO t VALUES (1) ON CONFLICT (a) WHERE a > 1 DO UPDATE SET b = 2 WHERE c > 3")
	ins := stmt.(*ast.InsertStatement)
	oc := ins.OnConflict
	if oc == nil {
		t.Fatal("no on-conflict info")
	}
	if oc.Action != ast.OnConflictUpdate {
		t.Errorf("action = %q", oc.Action)
	}
	if !reflect.DeepEqual(oc.IndexedColumns, []string{"a"}) {
		t.Errorf("indexed columns = %v", oc.IndexedColumns)
	}
	if oc.TargetWhere == nil {
		t.Error("no target where")
	}
	if oc.SetInfo == nil || len(oc.SetInfo.Columns) != 1 || oc.SetInfo.Columns[0][0] != "b" {
		t.Errorf("set info = %#v", oc.SetInfo)
	}
	if oc.SetInfo.Condition == nil {
		t.Error("no DO UPDATE WHERE condition")
	}

	// the OR REPLACE / OR IGNORE shorthands land in the same struct
	ins = parseOne(t, "INSERT OR REPLACE INTO t VALUES (1)").(*ast.InsertStatement)
	if ins.OnConflict == nil || ins.OnConflict.Action != ast.OnConflictReplace {
		t.Errorf("OR REPLACE on-conflict = %#v", ins.OnConflict)
	}
	if msg := parseErr(t, "INSERT OR REPLACE INTO t VALUES (1) ON CONFLICT DO NOTHING"); !strings.Contains(msg, "not compatible") {
		t.Errorf("shorthand+clause error = %q", msg)
	}
}

func TestInsertByNameDefaultValues(t *testing.T) {
	ins := parseOne(t, "INSERT INTO t BY NAME DEFAULT VALUES").(*ast.InsertStatement)
	if ins.ColumnOrder != ast.InsertByName {
		t.Errorf("column order = %q", ins.ColumnOrder)
	}
	if !ins.DefaultValues || ins.Query != nil {
		t.Errorf("default values = %v, query = %v", ins.DefaultValues, ins.Query)
	}
}

func TestUpdateShape(t *testing.T) {
	stmt := parseOne(t, "UPDATE t AS x SET a = 1, b = 2 FROM u WHERE x.i = u.i RETURNING a")
	up, ok := stmt.(*ast.UpdateStatement)
	if !ok {
		t.Fatalf("got %T, want *ast.UpdateStatement", stmt)
	}
	target := up.Table.(*ast.BaseTableRef)
	if target.TableName != "t" || target.Alias != "x" {
		t.Errorf("target = %q AS %q", target.TableName, target.Alias)
	}
	if len(up.SetInfo.Columns) != 2 || up.SetInfo.Columns[0][0] != "a" || up.SetInfo.Columns[1][0] != "b" {
		t.Errorf("set columns = %v", up.SetInfo.Columns)
	}
	if len(up.SetInfo.Expressions) != 2 {
		t.Errorf("set expressions = %d", len(up.SetInfo.Expressions))
	}
	if up.From == nil || up.Where == nil || len(up.Returning) != 1 {
		t.Errorf("from/where/returning missing")
	}
	// qualified SET targets are a transformer error, like upstream
	if msg := parseErr(t, "UPDATE t SET a.b = 1"); !strings.Contains(msg, "Qualified column names") {
		t.Errorf("dotted set target error = %q", msg)
	}
}

func TestDeleteShape(t *testing.T) {
	stmt := parseOne(t, "DELETE FROM t USING u, v WHERE t.i = u.i RETURNING t.i")
	del, ok := stmt.(*ast.DeleteStatement)
	if !ok {
		t.Fatalf("got %T, want *ast.DeleteStatement", stmt)
	}
	if del.Table.(*ast.BaseTableRef).TableName != "t" {
		t.Errorf("table = %#v", del.Table)
	}
	if len(del.Using) != 2 {
		t.Errorf("using = %d refs", len(del.Using))
	}
	if del.Where == nil || len(del.Returning) != 1 {
		t.Error("where/returning missing")
	}

	tr := parseOne(t, "TRUNCATE TABLE s.t").(*ast.TruncateStatement)
	ref := tr.Table.(*ast.BaseTableRef)
	if ref.SchemaName != "s" || ref.TableName != "t" {
		t.Errorf("truncate target = %#v", ref)
	}
}

func TestMergeIntoShape(t *testing.T) {
	stmt := parseOne(t, `MERGE INTO t USING s ON t.i = s.i
		WHEN MATCHED AND t.j > 0 THEN UPDATE SET j = s.j
		WHEN NOT MATCHED THEN INSERT (i, j) VALUES (s.i, s.j)
		WHEN NOT MATCHED BY SOURCE THEN DELETE
		RETURNING merge_action, *`)
	mg, ok := stmt.(*ast.MergeIntoStatement)
	if !ok {
		t.Fatalf("got %T, want *ast.MergeIntoStatement", stmt)
	}
	if mg.JoinCondition == nil {
		t.Error("no join condition")
	}
	if len(mg.Actions) != 3 {
		t.Fatalf("actions = %d", len(mg.Actions))
	}
	upd, insAct, del := mg.Actions[0], mg.Actions[1], mg.Actions[2]
	if upd.Kind != ast.MergeWhenMatched || upd.Action != ast.MergeUpdate || upd.Condition == nil {
		t.Errorf("action[0] = %+v", upd)
	}
	if insAct.Kind != ast.MergeWhenNotMatchedByTarget || insAct.Action != ast.MergeInsert ||
		!reflect.DeepEqual(insAct.Columns, []string{"i", "j"}) || len(insAct.Expressions) != 2 {
		t.Errorf("action[1] = %+v", insAct)
	}
	if del.Kind != ast.MergeWhenNotMatchedBySource || del.Action != ast.MergeDelete {
		t.Errorf("action[2] = %+v", del)
	}
	if len(mg.Returning) != 2 {
		t.Errorf("returning = %d", len(mg.Returning))
	}
}

func TestDMLParameters(t *testing.T) {
	ins := parseOne(t, "INSERT INTO t VALUES ($a, $b, $a)").(*ast.InsertStatement)
	want := []ast.NamedParam{{Name: "a", Index: 1}, {Name: "b", Index: 2}}
	if !reflect.DeepEqual(ins.NamedParams, want) {
		t.Errorf("params = %+v, want %+v", ins.NamedParams, want)
	}
	if msg := parseErr(t, "UPDATE t SET a = $x WHERE b = ?"); !strings.Contains(msg, "Mixing named and positional") {
		t.Errorf("mixed params error = %q", msg)
	}
}

func TestDMLInCTE(t *testing.T) {
	sel := parseOne(t, "WITH ins AS (INSERT INTO t VALUES (1) RETURNING i) SELECT i FROM ins").(*ast.SelectStatement)
	ctes := sel.Node.(*ast.SelectNode).CTEs
	if len(ctes.Entries) != 1 {
		t.Fatalf("cte entries = %d", len(ctes.Entries))
	}
	iq, ok := ctes.Entries[0].CTE.Query.(*ast.InsertQueryNode)
	if !ok {
		t.Fatalf("cte body = %T, want *ast.InsertQueryNode", ctes.Entries[0].CTE.Query)
	}
	if iq.Insert.Table != "t" || len(iq.Insert.Returning) != 1 {
		t.Errorf("wrapped insert = %+v", iq.Insert)
	}

	del := parseOne(t, "WITH d AS (DELETE FROM t RETURNING i) SELECT * FROM d").(*ast.SelectStatement)
	if _, ok := del.Node.(*ast.SelectNode).CTEs.Entries[0].CTE.Query.(*ast.DeleteQueryNode); !ok {
		t.Error("DELETE CTE body did not become a DeleteQueryNode")
	}

	up := parseOne(t, "WITH u AS (UPDATE t SET a = 1 RETURNING i) SELECT * FROM u").(*ast.SelectStatement)
	if _, ok := up.Node.(*ast.SelectNode).CTEs.Entries[0].CTE.Query.(*ast.UpdateQueryNode); !ok {
		t.Error("UPDATE CTE body did not become an UpdateQueryNode")
	}

	// non-DML statements stay rejected, like upstream
	if msg := parseErr(t, "WITH x AS (DROP TABLE t) SELECT 1"); !strings.Contains(msg, "CTE body must be") {
		t.Errorf("DDL-in-CTE error = %q", msg)
	}
}
