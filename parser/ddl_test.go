// ddl_test.go: hand-written shape tests for the milestone-4 DDL
// transformers (CREATE/ALTER/DROP families).
package parser

import (
	"reflect"
	"testing"

	"github.com/sqlc-dev/darkwing/ast"
)

func createInfo[T ast.CreateInfo](t *testing.T, sql string) T {
	t.Helper()
	stmt := parseOne(t, sql)
	cs, ok := stmt.(*ast.CreateStatement)
	if !ok {
		t.Fatalf("got %T, want *ast.CreateStatement", stmt)
	}
	info, ok := cs.Info.(T)
	if !ok {
		t.Fatalf("info = %T, want %T", cs.Info, *new(T))
	}
	return info
}

func TestCreateTableShape(t *testing.T) {
	info := createInfo[*ast.CreateTableInfo](t, `CREATE TABLE s.t (
		id INTEGER PRIMARY KEY,
		name VARCHAR NOT NULL DEFAULT 'x',
		ref_id INT REFERENCES other(id),
		CHECK (id > 0),
		UNIQUE (id, name)
	)`)
	if info.Schema != "s" || info.Name != "t" {
		t.Errorf("name = %q.%q", info.Schema, info.Name)
	}
	if info.OnConflict != ast.CreateError {
		t.Errorf("on conflict = %q", info.OnConflict)
	}
	if len(info.Columns) != 3 {
		t.Fatalf("columns = %d", len(info.Columns))
	}
	id := info.Columns[0]
	if id.Names[0] != "id" || id.Type.TypeName != "INTEGER" {
		t.Errorf("column 0 = %+v", id)
	}
	if pk, ok := id.Constraints[0].(*ast.UniqueConstraint); !ok || !pk.IsPrimaryKey {
		t.Errorf("column 0 constraint = %#v", id.Constraints)
	}
	name := info.Columns[1]
	if len(name.Constraints) != 2 {
		t.Fatalf("name constraints = %#v", name.Constraints)
	}
	if _, ok := name.Constraints[0].(*ast.NotNullConstraint); !ok {
		t.Errorf("constraint 0 = %T", name.Constraints[0])
	}
	if def, ok := name.Constraints[1].(*ast.DefaultConstraint); !ok || def.Expr == nil {
		t.Errorf("constraint 1 = %#v", name.Constraints[1])
	}
	if fk, ok := info.Columns[2].Constraints[0].(*ast.ForeignKeyConstraint); !ok ||
		fk.Table.Name != "other" || !reflect.DeepEqual(fk.ReferencedColumns, []string{"id"}) {
		t.Errorf("references = %#v", info.Columns[2].Constraints)
	}
	if len(info.Constraints) != 2 {
		t.Fatalf("table constraints = %d", len(info.Constraints))
	}
	if _, ok := info.Constraints[0].(*ast.CheckConstraint); !ok {
		t.Errorf("table constraint 0 = %T", info.Constraints[0])
	}
	if uq, ok := info.Constraints[1].(*ast.UniqueConstraint); !ok ||
		uq.IsPrimaryKey || !reflect.DeepEqual(uq.Columns, []string{"id", "name"}) {
		t.Errorf("table constraint 1 = %#v", info.Constraints[1])
	}
}

func TestCreateTableModifiers(t *testing.T) {
	info := createInfo[*ast.CreateTableInfo](t, "CREATE OR REPLACE TEMP TABLE t (i INT)")
	if info.OnConflict != ast.CreateReplace || !info.Temporary {
		t.Errorf("or-replace temp = %q temporary=%v", info.OnConflict, info.Temporary)
	}
	info = createInfo[*ast.CreateTableInfo](t, "CREATE TABLE IF NOT EXISTS t (i INT)")
	if info.OnConflict != ast.CreateIgnore {
		t.Errorf("if-not-exists = %q", info.OnConflict)
	}
	// both at once is upstream's transformer error
	if msg := parseErr(t, "CREATE OR REPLACE TABLE IF NOT EXISTS t (i INT)"); msg == "" {
		t.Error("OR REPLACE + IF NOT EXISTS accepted")
	}
	// CTAS
	info = createInfo[*ast.CreateTableInfo](t, "CREATE TABLE t AS SELECT 1 AS a")
	if info.Query == nil || len(info.Columns) != 0 {
		t.Errorf("CTAS query missing")
	}
}

func TestCreateViewIndexSequenceTypeSchema(t *testing.T) {
	view := createInfo[*ast.CreateViewInfo](t, "CREATE VIEW v (a, b) AS SELECT 1, 2")
	if !reflect.DeepEqual(view.Aliases, []string{"a", "b"}) || view.Query == nil {
		t.Errorf("view = %+v", view)
	}

	idx := createInfo[*ast.CreateIndexInfo](t, "CREATE UNIQUE INDEX i ON t (a, b DESC)")
	if !idx.Unique || idx.Table.Name != "t" || len(idx.Elements) != 2 {
		t.Errorf("index = %+v", idx)
	}
	if idx.Elements[1].OrderType != ast.OrderDescending {
		t.Errorf("index order = %+v", idx.Elements[1])
	}

	seq := createInfo[*ast.CreateSequenceInfo](t, "CREATE SEQUENCE q INCREMENT BY 2 MINVALUE 1 MAXVALUE 10 START WITH 3 CYCLE")
	if seq.Increment == nil || seq.MinValue == nil || seq.MaxValue == nil || seq.StartValue == nil || !seq.Cycle {
		t.Errorf("sequence = %+v", seq)
	}

	typ := createInfo[*ast.CreateTypeInfo](t, "CREATE TYPE mood AS ENUM ('happy', 'sad')")
	if !typ.IsEnum || !reflect.DeepEqual(typ.EnumValues, []string{"happy", "sad"}) {
		t.Errorf("enum = %+v", typ)
	}
	typ = createInfo[*ast.CreateTypeInfo](t, "CREATE TYPE pair AS STRUCT(a INT, b INT)")
	if typ.IsEnum || typ.Type == nil || typ.Type.TypeName != "STRUCT" {
		t.Errorf("alias type = %+v", typ)
	}

	schema := createInfo[*ast.CreateSchemaInfo](t, "CREATE SCHEMA IF NOT EXISTS sch")
	if schema.Name != "sch" || schema.OnConflict != ast.CreateIgnore {
		t.Errorf("schema = %+v", schema)
	}
}

func TestAlterTableShape(t *testing.T) {
	stmt := parseOne(t, "ALTER TABLE IF EXISTS s.t ADD COLUMN IF NOT EXISTS c INTEGER DEFAULT 3")
	al, ok := stmt.(*ast.AlterStatement)
	if !ok {
		t.Fatalf("got %T, want *ast.AlterStatement", stmt)
	}
	if al.Entity != ast.AlterEntityTable || !al.IfExists || al.Name.Schema != "s" || al.Name.Name != "t" {
		t.Errorf("alter header = %+v", al)
	}
	add, ok := al.Actions[0].(*ast.AddColumnInfo)
	if !ok || !add.IfNotExists || add.Column.Names[0] != "c" || add.Column.Type.TypeName != "INTEGER" {
		t.Errorf("add column = %#v", al.Actions[0])
	}

	al = parseOne(t, "ALTER TABLE t DROP COLUMN c CASCADE").(*ast.AlterStatement)
	if drop, ok := al.Actions[0].(*ast.DropColumnInfo); !ok || drop.Column[0] != "c" || !drop.Cascade {
		t.Errorf("drop column = %#v", al.Actions[0])
	}

	al = parseOne(t, "ALTER TABLE t ALTER COLUMN c SET DATA TYPE VARCHAR USING c::VARCHAR").(*ast.AlterStatement)
	if at, ok := al.Actions[0].(*ast.AlterColumnTypeInfo); !ok || at.Type.TypeName != "VARCHAR" || at.Using == nil {
		t.Errorf("alter type = %#v", al.Actions[0])
	}

	al = parseOne(t, "ALTER TABLE t RENAME COLUMN a TO b").(*ast.AlterStatement)
	if rc, ok := al.Actions[0].(*ast.RenameColumnInfo); !ok || rc.Column[0] != "a" || rc.NewName != "b" {
		t.Errorf("rename column = %#v", al.Actions[0])
	}

	al = parseOne(t, "ALTER VIEW v RENAME TO w").(*ast.AlterStatement)
	if al.Entity != ast.AlterEntityView {
		t.Errorf("entity = %q", al.Entity)
	}
	if rn, ok := al.Actions[0].(*ast.RenameInfo); !ok || rn.NewName != "w" {
		t.Errorf("rename = %#v", al.Actions[0])
	}
}

func TestDropShape(t *testing.T) {
	stmt := parseOne(t, "DROP TABLE IF EXISTS s.a, b CASCADE")
	drop, ok := stmt.(*ast.DropStatement)
	if !ok {
		t.Fatalf("got %T, want *ast.DropStatement", stmt)
	}
	if drop.DropType != ast.DropTable || !drop.IfExists || !drop.Cascade {
		t.Errorf("drop = %+v", drop)
	}
	want := []ast.QualifiedName{{Schema: "s", Name: "a"}, {Name: "b"}}
	if !reflect.DeepEqual(drop.Names, want) {
		t.Errorf("names = %+v", drop.Names)
	}

	if dt := parseOne(t, "DROP VIEW v").(*ast.DropStatement).DropType; dt != ast.DropView {
		t.Errorf("view drop type = %q", dt)
	}
	if dt := parseOne(t, "DROP SEQUENCE q").(*ast.DropStatement).DropType; dt != ast.DropSequence {
		t.Errorf("sequence drop type = %q", dt)
	}
}
