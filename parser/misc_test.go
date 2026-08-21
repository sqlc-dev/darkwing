// misc_test.go: hand-written shape tests for the milestone-5 statement
// transformers — the snapshot coverage for statements json_serialize_sql
// cannot see (it serializes SELECTs only).
package parser

import (
	"reflect"
	"strings"
	"testing"

	"github.com/sqlc-dev/darkwing/ast"
)

func parseAs[T ast.Stmt](t *testing.T, sql string) T {
	t.Helper()
	stmt := parseOne(t, sql)
	typed, ok := stmt.(T)
	if !ok {
		t.Fatalf("Parse(%q) = %T, want %T", sql, stmt, *new(T))
	}
	return typed
}

func TestSetResetShapes(t *testing.T) {
	set := parseAs[*ast.SetStatement](t, "SET memory_limit = '1GB'")
	if set.Name != "memory_limit" || set.Scope != ast.SetScopeAutomatic {
		t.Errorf("set = %+v", set)
	}
	if c, ok := set.Value.(*ast.ConstantExpression); !ok || c.Value.Str != "1GB" {
		t.Errorf("value = %#v", set.Value)
	}
	set = parseAs[*ast.SetStatement](t, "SET GLOBAL threads TO 4")
	if set.Scope != ast.SetScopeGlobal {
		t.Errorf("scope = %q", set.Scope)
	}
	// a bare identifier value folds to its name
	set = parseAs[*ast.SetStatement](t, "SET default_null_order = nulls_first")
	if c, ok := set.Value.(*ast.ConstantExpression); !ok || c.Value.Str != "nulls_first" {
		t.Errorf("identifier value = %#v", set.Value)
	}
	// SET SCHEMA and SET TIME ZONE special forms
	set = parseAs[*ast.SetStatement](t, "SET SCHEMA 'main'")
	if set.Name != "schema" {
		t.Errorf("schema set = %+v", set)
	}
	set = parseAs[*ast.SetStatement](t, "SET TIME ZONE 'UTC'")
	if set.Name != "timezone" {
		t.Errorf("timezone set = %+v", set)
	}
	// DEFAULT values fold into resets
	if _, ok := parseOne(t, "SET threads TO DEFAULT").(*ast.ResetStatement); !ok {
		t.Error("SET ... TO DEFAULT did not become a RESET")
	}
	if _, ok := parseOne(t, "SET TIME ZONE LOCAL").(*ast.ResetStatement); !ok {
		t.Error("SET TIME ZONE LOCAL did not become a RESET")
	}
	reset := parseAs[*ast.ResetStatement](t, "RESET SESSION threads")
	if reset.Name != "threads" || reset.Scope != ast.SetScopeSession {
		t.Errorf("reset = %+v", reset)
	}
	if msg := parseErr(t, "SET a = 1, 2"); !strings.Contains(msg, "single value") {
		t.Errorf("multi-value SET error = %q", msg)
	}
	// VARIABLE scope
	set = parseAs[*ast.SetStatement](t, "SET VARIABLE x = 42")
	if set.Scope != ast.SetScopeVariable {
		t.Errorf("variable scope = %q", set.Scope)
	}
}

func TestPragmaShapes(t *testing.T) {
	p := parseAs[*ast.PragmaStatement](t, "PRAGMA enable_progress_bar")
	if p.Name != "enable_progress_bar" || len(p.Parameters) != 0 {
		t.Errorf("pragma = %+v", p)
	}
	p = parseAs[*ast.PragmaStatement](t, "PRAGMA table_info('t')")
	if len(p.Parameters) != 1 {
		t.Errorf("call pragma = %+v", p)
	}
	// named parameters split off
	p = parseAs[*ast.PragmaStatement](t, "PRAGMA database_size(mode = 'fast')")
	if len(p.NamedParameters) != 1 || p.NamedParameters[0].Name != "mode" {
		t.Errorf("named parameters = %+v", p.NamedParameters)
	}
	// assignment pragmas become SET statements, except SQLite-compat ones
	if _, ok := parseOne(t, "PRAGMA memory_limit='1GB'").(*ast.SetStatement); !ok {
		t.Error("assignment pragma did not become SET")
	}
	p = parseAs[*ast.PragmaStatement](t, "PRAGMA table_info='t'")
	if p.Name != "table_info" || len(p.Parameters) != 1 {
		t.Errorf("sqlite-compat pragma = %+v", p)
	}
}

func TestCallUseCheckpointShapes(t *testing.T) {
	call := parseAs[*ast.CallStatement](t, "CALL pragma_table_info('t')")
	if call.Function == nil || call.Function.FunctionName != "pragma_table_info" ||
		len(call.Function.Arguments) != 1 {
		t.Errorf("call = %+v", call.Function)
	}
	use := parseAs[*ast.UseStatement](t, "USE db")
	if use.Name != "db" || use.Catalog != "" {
		t.Errorf("use = %+v", use)
	}
	use = parseAs[*ast.UseStatement](t, "USE db.main")
	if use.Catalog != "db" || use.Name != "main" {
		t.Errorf("qualified use = %+v", use)
	}
	if msg := parseErr(t, "USE a.b.c"); !strings.Contains(msg, "USE database") {
		t.Errorf("three-part USE error = %q", msg)
	}
	cp := parseAs[*ast.CheckpointStatement](t, "FORCE CHECKPOINT db")
	if !cp.Force || cp.Catalog != "db" {
		t.Errorf("checkpoint = %+v", cp)
	}
}

func TestTransactionShapes(t *testing.T) {
	tx := parseAs[*ast.TransactionStatement](t, "BEGIN TRANSACTION READ ONLY")
	if tx.Kind != ast.TransactionBegin || tx.Modifier != ast.TransactionReadOnly {
		t.Errorf("begin = %+v", tx)
	}
	if tx = parseAs[*ast.TransactionStatement](t, "COMMIT"); tx.Kind != ast.TransactionCommit {
		t.Errorf("commit = %+v", tx)
	}
	if tx = parseAs[*ast.TransactionStatement](t, "ABORT"); tx.Kind != ast.TransactionRollback {
		t.Errorf("abort = %+v", tx)
	}
}

func TestVacuumAnalyzeShapes(t *testing.T) {
	v := parseAs[*ast.VacuumStatement](t, "VACUUM ANALYZE t(a, b)")
	if !v.Vacuum || !v.Analyze {
		t.Errorf("vacuum flags = %+v", v)
	}
	if ref, ok := v.Table.(*ast.BaseTableRef); !ok || ref.TableName != "t" ||
		!reflect.DeepEqual(v.Columns, []string{"a", "b"}) {
		t.Errorf("vacuum target = %#v %v", v.Table, v.Columns)
	}
	// FULL parses (upstream errors post-parse), recorded as a raw option
	v = parseAs[*ast.VacuumStatement](t, "VACUUM FULL")
	if !reflect.DeepEqual(v.Options, []string{"full"}) {
		t.Errorf("legacy options = %v", v.Options)
	}
	a := parseAs[*ast.AnalyzeStatement](t, "ANALYZE t")
	if ref, ok := a.Table.(*ast.BaseTableRef); !ok || ref.TableName != "t" {
		t.Errorf("analyze target = %#v", a.Table)
	}
}

func TestExportImportShapes(t *testing.T) {
	e := parseAs[*ast.ExportStatement](t, "EXPORT DATABASE db TO 'dir' (FORMAT parquet)")
	if e.Database != "db" || e.Path != "dir" || e.Format != "parquet" || len(e.Options) != 0 {
		t.Errorf("export = %+v", e)
	}
	e = parseAs[*ast.ExportStatement](t, "EXPORT DATABASE 'dir'")
	if e.Format != "csv" {
		t.Errorf("default format = %q", e.Format)
	}
	i := parseAs[*ast.ImportStatement](t, "IMPORT DATABASE 'dir'")
	if i.Path != "dir" {
		t.Errorf("import = %+v", i)
	}
}

func TestPreparedStatementShapes(t *testing.T) {
	p := parseAs[*ast.PrepareStatement](t, "PREPARE q AS SELECT $1, $2")
	if p.Name != "q" {
		t.Errorf("prepare = %+v", p)
	}
	if _, ok := p.Statement.(*ast.SelectStatement); !ok {
		t.Errorf("prepared statement = %T", p.Statement)
	}
	if msg := parseErr(t, "PREPARE q AS CREATE TABLE t (i INT)"); !strings.Contains(msg, "not a preparable statement") {
		t.Errorf("non-preparable error = %q", msg)
	}
	ex := parseAs[*ast.ExecuteStatement](t, "EXECUTE q(1, x := 2)")
	if ex.Name != "q" || len(ex.Values) != 2 {
		t.Fatalf("execute = %+v", ex)
	}
	// positional arguments are named by position
	if ex.Values[0].Name != "1" || ex.Values[1].Name != "x" {
		t.Errorf("execute values = %+v", ex.Values)
	}
	d := parseAs[*ast.DeallocateStatement](t, "DEALLOCATE PREPARE q")
	if d.Name != "q" {
		t.Errorf("deallocate = %+v", d)
	}
}

func TestExplainShapes(t *testing.T) {
	e := parseAs[*ast.ExplainStatement](t, "EXPLAIN SELECT 1")
	if e.Type != ast.ExplainStandard {
		t.Errorf("explain type = %q", e.Type)
	}
	if _, ok := e.Statement.(*ast.SelectStatement); !ok {
		t.Errorf("explained statement = %T", e.Statement)
	}
	e = parseAs[*ast.ExplainStatement](t, "EXPLAIN ANALYZE UPDATE t SET a = 1")
	if e.Type != ast.ExplainAnalyze {
		t.Errorf("explain analyze type = %q", e.Type)
	}
	if _, ok := e.Statement.(*ast.UpdateStatement); !ok {
		t.Errorf("explained statement = %T", e.Statement)
	}
	e = parseAs[*ast.ExplainStatement](t, "EXPLAIN (FORMAT json) SELECT 1")
	if e.Format != "json" {
		t.Errorf("format = %q", e.Format)
	}
}

func TestAttachDetachConnectShapes(t *testing.T) {
	at := parseAs[*ast.AttachStatement](t, "ATTACH DATABASE 'file.db' AS db (READ_ONLY, TYPE sqlite)")
	if at.Alias != "db" || at.OnConflict != ast.CreateError || len(at.Options) != 2 {
		t.Fatalf("attach = %+v", at)
	}
	if c, ok := at.Path.(*ast.ConstantExpression); !ok || c.Value.Str != "file.db" {
		t.Errorf("path = %#v", at.Path)
	}
	if at.Options[0].Name != "read_only" || len(at.Options[0].Values) != 0 {
		t.Errorf("bare option = %+v", at.Options[0])
	}
	if at.Options[1].Name != "type" || at.Options[1].Values[0].Str != "sqlite" {
		t.Errorf("valued option = %+v", at.Options[1])
	}
	at = parseAs[*ast.AttachStatement](t, "ATTACH IF NOT EXISTS ':memory:' AS mem")
	if at.OnConflict != ast.CreateIgnore {
		t.Errorf("if-not-exists = %q", at.OnConflict)
	}
	if msg := parseErr(t, "ATTACH OR REPLACE IF NOT EXISTS 'x' AS y"); !strings.Contains(msg, "Cannot specify both") {
		t.Errorf("conflict error = %q", msg)
	}
	dt := parseAs[*ast.DetachStatement](t, "DETACH DATABASE IF EXISTS db")
	if dt.Name != "db" || !dt.IfExists {
		t.Errorf("detach = %+v", dt)
	}
	cn := parseAs[*ast.ConnectStatement](t, "CONNECT LOCAL")
	if !cn.Local {
		t.Errorf("connect local = %+v", cn)
	}
	cn = parseAs[*ast.ConnectStatement](t, "CONNECT 'remote' (TOKEN 'abc')")
	if cn.Name != "remote" || !cn.NameIsString || len(cn.Options) != 1 {
		t.Errorf("connect string = %+v", cn)
	}
	parseAs[*ast.DisconnectStatement](t, "DISCONNECT")
}

func TestCommentOnShapes(t *testing.T) {
	c := parseAs[*ast.CommentOnStatement](t, "COMMENT ON TABLE s.t IS 'hi'")
	if c.OnType != ast.CommentOnTable || c.Schema != "s" || c.Name != "t" {
		t.Errorf("comment = %+v", c)
	}
	if v, ok := c.Value.(*ast.ConstantExpression); !ok || v.Value.Str != "hi" {
		t.Errorf("comment value = %#v", c.Value)
	}
	c = parseAs[*ast.CommentOnStatement](t, "COMMENT ON COLUMN t.col IS NULL")
	if c.OnType != ast.CommentOnColumn || c.Name != "t" || c.Column != "col" {
		t.Errorf("column comment = %+v", c)
	}
	if v, ok := c.Value.(*ast.ConstantExpression); !ok || !v.Value.IsNull {
		t.Errorf("null comment = %#v", c.Value)
	}
	if msg := parseErr(t, "COMMENT ON COLUMN c IS 'x'"); !strings.Contains(msg, "Invalid column reference") {
		t.Errorf("bare column error = %q", msg)
	}
}

func TestExtensionStatementShapes(t *testing.T) {
	l := parseAs[*ast.LoadStatement](t, "LOAD httpfs")
	if l.Kind != ast.LoadTypeLoad || l.Name != "httpfs" {
		t.Errorf("load = %+v", l)
	}
	l = parseAs[*ast.LoadStatement](t, "LOAD 'ext.duckdb_extension' AS my_ext")
	if l.Kind != ast.LoadTypeLoadAs || l.Alias != "my_ext" {
		t.Errorf("load as = %+v", l)
	}
	l = parseAs[*ast.LoadStatement](t, "FORCE INSTALL spatial FROM core_nightly VERSION 'v1.2'")
	if l.Kind != ast.LoadTypeForceInstall || l.Name != "spatial" ||
		l.Repository != "core_nightly" || !l.RepoIsAlias || l.Version != "v1.2" {
		t.Errorf("install = %+v", l)
	}
	l = parseAs[*ast.LoadStatement](t, "INSTALL x FROM 'https://repo'")
	if l.Kind != ast.LoadTypeInstall || l.RepoIsAlias || l.Repository != "https://repo" {
		t.Errorf("install from url = %+v", l)
	}
	u := parseAs[*ast.UpdateExtensionsStatement](t, "UPDATE EXTENSIONS (a, b)")
	if !reflect.DeepEqual(u.Extensions, []string{"a", "b"}) {
		t.Errorf("update extensions = %+v", u)
	}
	er := parseAs[*ast.ExternalResourceStatement](t, "CREATE EXTERNAL RESOURCE 'gpu' AS g (device 1)")
	if er.Operation != ast.ExternalResourceCreate || er.Type != "gpu" || er.Name != "g" || len(er.Options) != 1 {
		t.Errorf("create external resource = %+v", er)
	}
	er = parseAs[*ast.ExternalResourceStatement](t, "SHOW ALL EXTERNAL RESOURCES")
	if er.Operation != ast.ExternalResourceShow || !er.All {
		t.Errorf("show external resources = %+v", er)
	}
}

func TestCopyShapes(t *testing.T) {
	c := parseAs[*ast.CopyStatement](t, "COPY s.t (a, b) FROM 'data.csv.gz' (HEADER, DELIMITER '|')")
	info := c.Info
	if info.Schema != "s" || info.Table != "t" || !reflect.DeepEqual(info.Columns, []string{"a", "b"}) {
		t.Errorf("copy target = %+v", info)
	}
	if !info.IsFrom || info.FilePath != "data.csv.gz" || info.Format != "csv" {
		t.Errorf("copy source = %+v", info)
	}
	if len(info.Options) != 2 || info.Options[0].Name != "header" || info.Options[1].Name != "delimiter" {
		t.Errorf("options = %+v", info.Options)
	}
	// PostgreSQL-style specialized options without parentheses
	c = parseAs[*ast.CopyStatement](t, "COPY t TO stdout CSV HEADER")
	if c.Info.IsFrom || c.Info.FilePath != "/dev/stdout" || !c.Info.FormatExplicit || c.Info.Format != "csv" {
		t.Errorf("specialized copy = %+v", c.Info)
	}
	// FORMAT option overrides the extension
	c = parseAs[*ast.CopyStatement](t, "COPY t TO 'out.bin' (FORMAT parquet)")
	if c.Info.Format != "parquet" || !c.Info.FormatExplicit {
		t.Errorf("format option = %+v", c.Info)
	}
	if msg := parseErr(t, "COPY t TO 'f' (HEADER, HEADER)"); !strings.Contains(msg, "duplicate option") {
		t.Errorf("duplicate option error = %q", msg)
	}
	// COPY (SELECT ...) TO
	c = parseAs[*ast.CopyStatement](t, "COPY (SELECT 1) TO 'out.parquet'")
	if c.Info.Query == nil || c.Info.Table != "" {
		t.Errorf("copy select = %+v", c.Info)
	}
	// COPY FROM DATABASE
	cd := parseAs[*ast.CopyDatabaseStatement](t, "COPY FROM DATABASE a TO b (SCHEMA)")
	if cd.FromDatabase != "a" || cd.ToDatabase != "b" || cd.CopyType != ast.CopyDatabaseSchema {
		t.Errorf("copy database = %+v", cd)
	}
	// the flagless form desugars into PRAGMA copy_database
	p := parseAs[*ast.PragmaStatement](t, "COPY FROM DATABASE a TO b")
	if p.Name != "copy_database" || len(p.Parameters) != 2 {
		t.Errorf("copy database pragma = %+v", p)
	}
}

func TestCopyInCTE(t *testing.T) {
	sel := parseAs[*ast.SelectStatement](t,
		"WITH c AS (COPY t TO 'out.csv') SELECT * FROM c")
	ctes := sel.Node.CTEMapRef()
	if len(ctes.Entries) != 1 {
		t.Fatalf("cte entries = %d", len(ctes.Entries))
	}
	if _, ok := ctes.Entries[0].CTE.Query.(*ast.CopyQueryNode); !ok {
		t.Errorf("cte body = %T, want *ast.CopyQueryNode", ctes.Entries[0].CTE.Query)
	}
	if msg := parseErr(t, "WITH c AS (COPY t FROM 'in.csv') SELECT 1"); !strings.Contains(msg, "COPY FROM cannot") {
		t.Errorf("copy-from CTE error = %q", msg)
	}
	if msg := parseErr(t, "WITH RECURSIVE c AS (COPY t TO 'o.csv') SELECT 1"); !strings.Contains(msg, "Recursive CTEs with COPY") {
		t.Errorf("recursive copy CTE error = %q", msg)
	}
}

func TestCreateMacroShapes(t *testing.T) {
	info := createInfo[*ast.CreateMacroInfo](t, "CREATE MACRO add2(a, b := 5) AS a + b")
	if info.Name != "add2" || info.IsTable || len(info.Macros) != 1 {
		t.Fatalf("macro = %+v", info)
	}
	m := info.Macros[0]
	if len(m.Parameters) != 2 || m.Parameters[0].Name != "a" || m.Parameters[1].Name != "b" {
		t.Fatalf("parameters = %+v", m.Parameters)
	}
	if m.Parameters[0].Default != nil || m.Parameters[1].Default == nil {
		t.Errorf("defaults = %+v", m.Parameters)
	}
	if m.Expr == nil || m.Query != nil {
		t.Errorf("body = %+v", m)
	}
	// scalar overloads
	info = createInfo[*ast.CreateMacroInfo](t, "CREATE MACRO m1(a) AS a, (a, b) AS a + b")
	if info.IsTable || len(info.Macros) != 2 {
		t.Errorf("overloads = %+v", info)
	}
	// table macros
	info = createInfo[*ast.CreateMacroInfo](t, "CREATE MACRO t1(x) AS TABLE SELECT x")
	if !info.IsTable || len(info.Macros) != 1 || info.Macros[0].Query == nil {
		t.Errorf("table macro = %+v", info)
	}
	if msg := parseErr(t, "CREATE MACRO m(a, a) AS a"); !strings.Contains(msg, "Duplicate parameter") {
		t.Errorf("dup param error = %q", msg)
	}
	if msg := parseErr(t, "CREATE MACRO m(a := 1, b) AS a + b"); !strings.Contains(msg, "without a default") {
		t.Errorf("default order error = %q", msg)
	}
	if msg := parseErr(t, "CREATE MACRO m() AS 1, () AS TABLE SELECT 1"); !strings.Contains(msg, "Cannot mix") {
		t.Errorf("mixed macro error = %q", msg)
	}
}

func TestCreateSecretShapes(t *testing.T) {
	info := createInfo[*ast.CreateSecretInfo](t, "CREATE SECRET (TYPE s3, KEY_ID 'k')")
	if info.Name != "__default_s3" {
		t.Errorf("default name = %q", info.Name)
	}
	if info.Type == nil || len(info.Options) != 1 || info.Options[0].Name != "key_id" {
		t.Errorf("secret = %+v", info)
	}
	info = createInfo[*ast.CreateSecretInfo](t, "CREATE SECRET my_secret IN motherduck (TYPE s3, SCOPE 's3://b')")
	if info.Name != "my_secret" || info.Storage != "motherduck" || info.Scope == nil {
		t.Errorf("named secret = %+v", info)
	}
	if msg := parseErr(t, "CREATE SECRET (KEY_ID 'k')"); !strings.Contains(msg, "must have a type") {
		t.Errorf("missing type error = %q", msg)
	}
}

func TestCreateTriggerShapes(t *testing.T) {
	info := createInfo[*ast.CreateTriggerInfo](t, `CREATE TRIGGER trg AFTER UPDATE OF a, b ON s.t
		REFERENCING NEW TABLE AS n OLD TABLE AS o
		FOR EACH ROW
		INSERT INTO log VALUES (1)`)
	if info.Name != "trg" || info.Timing != ast.TriggerAfter || info.Event != ast.TriggerEventUpdate {
		t.Errorf("trigger = %+v", info)
	}
	if !reflect.DeepEqual(info.Columns, []string{"a", "b"}) {
		t.Errorf("columns = %v", info.Columns)
	}
	if info.Table == nil || info.Table.SchemaName != "s" || info.Table.TableName != "t" {
		t.Errorf("table = %+v", info.Table)
	}
	if info.ReferencingNew != "n" || info.ReferencingOld != "o" || info.ForEach != ast.TriggerForEachRow {
		t.Errorf("referencing = %+v", info)
	}
	if _, ok := info.Body.(*ast.InsertStatement); !ok {
		t.Errorf("body = %T", info.Body)
	}
	if msg := parseErr(t, `CREATE TRIGGER trg AFTER INSERT ON t
		REFERENCING NEW TABLE AS x NEW TABLE AS y
		INSERT INTO l VALUES (1)`); !strings.Contains(msg, "cannot be specified multiple times") {
		t.Errorf("dup referencing error = %q", msg)
	}
}

func TestPivotEntryChecks(t *testing.T) {
	// data-extracted pivots cannot appear in views or macros
	if msg := parseErr(t, "CREATE VIEW v AS PIVOT t ON x USING sum(y)"); !strings.Contains(msg, "cannot be used in views") {
		t.Errorf("pivot view error = %q", msg)
	}
	// with an explicit IN list they can
	parseOne(t, "CREATE VIEW v AS PIVOT t ON x IN (a, b) USING sum(y)")
	// parameters cannot mix with data extraction
	if msg := parseErr(t, "PIVOT (SELECT a + ? AS a FROM t) ON a USING sum(a)"); !strings.Contains(msg, "cannot have parameters") {
		t.Errorf("pivot params error = %q", msg)
	}
}

func TestSequenceOptionChecks(t *testing.T) {
	if msg := parseErr(t, "CREATE SEQUENCE s START 13 START WITH 3"); !strings.Contains(msg, "Start should be passed at most once") {
		t.Errorf("dup start = %q", msg)
	}
	if msg := parseErr(t, "CREATE SEQUENCE s START NULL"); !strings.Contains(msg, "START value must not be NULL") {
		t.Errorf("null start = %q", msg)
	}
	if msg := parseErr(t, "CREATE SEQUENCE s MINVALUE 7 MAXVALUE 5"); !strings.Contains(msg, "must be less than MAXVALUE") {
		t.Errorf("min/max = %q", msg)
	}
	if msg := parseErr(t, "CREATE SEQUENCE s INCREMENT 0"); !strings.Contains(msg, "Increment must not be zero") {
		t.Errorf("zero increment = %q", msg)
	}
	// a negative increment flips the defaults
	if msg := parseErr(t, "CREATE SEQUENCE s INCREMENT -1 START 1 CYCLE"); !strings.Contains(msg, "cannot be greater than MAXVALUE (-1)") {
		t.Errorf("negative increment default = %q", msg)
	}
	parseOne(t, "CREATE SEQUENCE s INCREMENT 2 MINVALUE 0 MAXVALUE 100 START 4 CYCLE")
}
