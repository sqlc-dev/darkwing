// transform_ddl.go: CREATE / ALTER / DROP — the DDL transformers: tables
// (columns + constraints), views, schemas, indexes, sequences and types
// (CREATE MACRO / SECRET / TRIGGER live in transform_create_misc.go).
// Port of upstream's transformer/statement/transform_create_*.cpp,
// transform_alter.cpp and transform_drop.cpp.
package parser

import (
	"math"
	"strings"

	"github.com/sqlc-dev/darkwing/ast"
	"github.com/sqlc-dev/darkwing/token"
)

func init() {
	register("CreateStatement", func(tc *transformContext, n tnode) ast.Stmt {
		return tc.transformCreate(n)
	})
	register("AlterStatement", func(tc *transformContext, n tnode) ast.Stmt {
		return tc.transformAlter(n)
	})
	register("DropStatement", func(tc *transformContext, n tnode) ast.Stmt {
		return tc.transformDrop(n)
	})
}

// ---- CREATE ------------------------------------------------------------

// CreateStatement <- 'CREATE' OrReplace? Temporary? CreateStatementVariation
func (tc *transformContext) transformCreate(n tnode) ast.Stmt {
	stmt := &ast.CreateStatement{}
	stmt.SetSpan(n.span())
	onConflict := ast.CreateError
	if _, orReplace := n.child(1).opt(); orReplace {
		onConflict = ast.CreateReplace
	}
	temporary, persistent := false, false
	if temp, ok := n.child(2).opt(); ok {
		// Temporary <- Persistent / TempPersistent / TemporaryPersistent
		_, kind := temp.sole().choice()
		temporary = kind.name() != "Persistent"
		persistent = !temporary
	}
	_, variation := n.child(3).sole().choice()
	var info ast.CreateInfo
	switch variation.name() {
	case "CreateTableStmt":
		info = tc.transformCreateTable(variation)
	case "CreateSchemaStmt":
		info = tc.transformCreateSchema(variation)
	case "CreateViewStmt":
		info = tc.transformCreateView(variation)
	case "CreateIndexStmt":
		info = tc.transformCreateIndex(variation)
	case "CreateSequenceStmt":
		info = tc.transformCreateSequence(variation)
	case "CreateTypeStmt":
		info = tc.transformCreateType(variation)
	case "CreateMacroStmt":
		info = tc.transformCreateMacro(variation)
	case "CreateSecretStmt":
		info = tc.transformCreateSecret(variation)
	case "CreateTriggerStmt":
		info = tc.transformCreateTrigger(variation)
	default:
		shapeError(variation, "unknown create variation")
	}
	base := info.Base()
	base.Temporary = temporary
	if base.OnConflict == "" || onConflict == ast.CreateReplace {
		if base.OnConflict == ast.CreateIgnore && onConflict == ast.CreateReplace {
			raise("Cannot specify both OR REPLACE and IF NOT EXISTS within single create statement")
		}
		if onConflict != ast.CreateError || base.OnConflict == "" {
			if base.OnConflict == "" {
				base.OnConflict = onConflict
			} else if onConflict == ast.CreateReplace {
				base.OnConflict = onConflict
			}
		}
	}
	// TransformCreateStatement: PERSISTENT only applies to secrets, and
	// TEMPORARY is rejected for entries that derive it from their table
	if persistent && variation.name() != "CreateSecretStmt" {
		raise("PERSISTENT is only supported for CREATE SECRET")
	}
	if temporary {
		switch variation.name() {
		case "CreateIndexStmt":
			raise("Temporary indexes are not supported")
		case "CreateTriggerStmt":
			raise("Temporary triggers are not supported")
		}
	}
	stmt.Info = info
	return stmt
}

// applyIfNotExists sets IGNORE on the info when the IfNotExists optional
// matched.
func applyIfNotExists(base *ast.CreateInfoBase, n tnode) {
	if _, ok := n.opt(); ok {
		base.OnConflict = ast.CreateIgnore
	}
}

// fillQualifiedName splits a QualifiedName into the info's
// catalog/schema/name.
func (tc *transformContext) fillQualifiedName(base *ast.CreateInfoBase, n tnode) {
	base.Catalog, base.Schema, base.Name = tc.transformQualifiedNameParts(n)
}

// CreateTableStmt <- 'TABLE' IfNotExists? QualifiedName
// CreateTableDefinition CommitAction?
func (tc *transformContext) transformCreateTable(n tnode) ast.CreateInfo {
	info := &ast.CreateTableInfo{}
	info.SetSpan(n.span())
	applyIfNotExists(&info.CreateInfoBase, n.child(1))
	tc.fillQualifiedName(&info.CreateInfoBase, n.child(2))
	// CreateTableDefinition <- CreateTableAs / CreateColumnList
	_, def := n.child(3).sole().choice()
	switch def.name() {
	case "CreateTableAs":
		// IdentifierList? PartitionSortedOptions? WithList? 'AS' Statement WithData?
		if ids, ok := def.child(0).opt(); ok {
			info.QueryAliases = identifierList(ids.sole().parens())
		}
		if wl, ok := def.child(2).opt(); ok {
			info.Options = tc.transformWithList(wl)
		}
		stmt := tc.transformStatement(def.child(4))
		sel, ok := stmt.(*ast.SelectStatement)
		if !ok {
			raise("CREATE TABLE AS requires a SELECT clause")
		}
		info.Query = sel
	case "CreateColumnList":
		// Parens(CreateTableColumnList?) PartitionSortedOptions? WithList?
		if list, ok := def.child(0).parens().opt(); ok {
			for _, elem := range list.sole().listElems() {
				tc.transformCreateTableElement(info, elem)
			}
		}
		if wl, ok := def.child(2).opt(); ok {
			info.Options = tc.transformWithList(wl)
		}
		if len(info.Columns) == 0 {
			raise("Table must have at least one column!")
		}
	default:
		shapeError(def, "unknown create table definition")
	}
	if info.Name == "" {
		raise("Empty table name not supported")
	}
	return info
}

// transformWithList maps WithList <- 'WITH' RelOptionOrOids.
// RelOptionOrOids <- RelOptionList / Oids (OIDS raise NotImplemented
// upstream, post-parse; darkwing records no options for them).
func (tc *transformContext) transformWithList(n tnode) []ast.RelOption {
	_, alt := n.child(1).sole().choice()
	if alt.name() == "RelOptionList" {
		return tc.transformRelOptionList(alt)
	}
	return nil
}

// transformRelOptionList ports TransformRelOptionList:
// RelOptionList <- Parens(List(RelOption)); option names are
// case-insensitive and may not repeat.
func (tc *transformContext) transformRelOptionList(n tnode) []ast.RelOption {
	var out []ast.RelOption
	seen := map[string]bool{}
	for _, o := range n.sole().parens().listElems() {
		// RelOption <- RelOptionName RelOptionArgumentOpt?
		opt := ast.RelOption{Name: relOptionName(o.child(0))}
		if arg, ok := o.child(1).opt(); ok {
			// RelOptionArgumentOpt <- '=' DefArg
			opt.Value = tc.transformDefArg(arg.child(1))
		} else {
			opt.Value = constExpr(invalidSpan, nullValue())
		}
		key := strings.ToLower(opt.Name)
		if seen[key] {
			raise("Duplicate option \"%s\" in WITH clause", opt.Name)
		}
		seen[key] = true
		out = append(out, opt)
	}
	return out
}

// transformDefArg maps DefArg <- DefArgNull / DefArgStringLiteral /
// NumberLiteral / NoneLiteral / Expression / DefArgKeyword.
func (tc *transformContext) transformDefArg(n tnode) ast.Expr {
	_, alt := n.sole().choice()
	switch alt.name() {
	case "DefArgNull", "NoneLiteral":
		return constExpr(alt.span(), nullValue())
	case "DefArgStringLiteral":
		return constExpr(alt.span(), varcharValue(identifierText(alt)))
	case "NumberLiteral":
		return numberConstant(alt.span(), alt.number())
	case "Expression":
		return tc.transformExpression(alt)
	case "DefArgKeyword":
		return constExpr(alt.span(), varcharValue(identifierText(alt)))
	}
	shapeError(alt, "unknown option argument")
	return nil
}

// CreateTableColumnElement <- CreateTableColumnDefinition / CreateTableConstraint
func (tc *transformContext) transformCreateTableElement(info *ast.CreateTableInfo, n tnode) {
	_, alt := n.sole().choice()
	switch alt.name() {
	case "CreateTableColumnDefinition":
		info.Columns = append(info.Columns, tc.transformColumnDefinition(alt.sole()))
	case "CreateTableConstraint":
		info.Constraints = append(info.Constraints, tc.transformTopLevelConstraint(alt.sole()))
	default:
		shapeError(alt, "unknown create table element")
	}
}

// ColumnDefinition <- Identifier Type? GeneratedColumn? ColumnConstraint*
// Port of TransformColumnDefinition. The children transform first
// (GeneratedColumn's checks, each constraint's own checks), then the
// column-level validation runs in upstream's order.
func (tc *transformContext) transformColumnDefinition(n tnode) ast.ColumnDef {
	col := ast.ColumnDef{}
	col.SetSpan(n.span())
	col.Names = []string{identifierText(n.child(0))}
	if typ, ok := n.child(1).opt(); ok {
		col.Type = tc.transformType(typ)
	}
	if gen, ok := n.child(2).opt(); ok {
		tc.transformGeneratedColumn(&col, gen)
	}
	entries := tc.transformColumnConstraints(n.child(3))
	if col.Type == nil && col.Generated == "" {
		raise("Column %s must have a type or be defined as a GENERATED column.", sqlIdentifier(col.Names[0]))
	}
	hasDefault, hasCollation := false, false
	for _, e := range entries {
		switch {
		case e.isDefault:
			if hasDefault {
				raise("Cannot define a default value twice")
			}
			hasDefault = true
		case e.collation != "":
			if hasCollation {
				raise("multiple COLLATE clauses not allowed")
			}
			hasCollation = true
			if col.Generated != "" {
				raise("Collations are not supported on generated columns")
			}
			applyColumnCollation(col.Type, e.collation)
			col.Collation = e.collation
			continue
		case e.compression != "":
			if strings.EqualFold(e.compression, "auto") {
				raise("Unrecognized option for column compression, expected none, uncompressed, " +
					"rle, dictionary, pfor, bitpacking, fsst, chimp, patas, zstd, alp, alprd or " +
					"roaring")
			}
			col.Compression = e.compression
			continue
		}
		if e.constraint != nil {
			col.Constraints = append(col.Constraints, e.constraint)
		}
	}
	if col.Generated != "" {
		if hasSubquery(col.Default) {
			raise("Expression of generated column %s contains a subquery, which isn't allowed", quotedIdent(col.Names[0]))
		}
		if hasDefault {
			raise("Not allowed to set default on a generated column")
		}
	}
	return col
}

// transformGeneratedColumn ports TransformGeneratedColumn:
// GeneratedColumn <- Generated? 'AS' Parens(Expression) GeneratedColumnType?
// Generated <- GeneratedAlways / GeneratedByDefault
func (tc *transformContext) transformGeneratedColumn(col *ast.ColumnDef, gen tnode) {
	if g, ok := gen.child(0).opt(); ok {
		if _, kind := g.sole().choice(); kind.name() == "GeneratedByDefault" {
			raise("for a generated column, GENERATED ALWAYS must be specified")
		}
	}
	col.Generated = ast.GeneratedVirtual
	col.Default = tc.transformExpression(gen.child(2).parens())
	verifyGeneratedColumnRefs(col.Default)
	if kind, ok := gen.child(3).opt(); ok {
		_, k := kind.sole().choice()
		if k.name() == "StoredGeneratedColumn" {
			col.Generated = ast.GeneratedStored
		}
	}
}

// columnConstraintEntry is upstream's ColumnConstraintEntry: one
// transformed ColumnConstraint, either a constraint or a column property.
type columnConstraintEntry struct {
	constraint  ast.Constraint
	isDefault   bool
	collation   string
	compression string
	// notNull / unique / primary mirror constraint_type_info
	notNull, unique, primary bool
	check, foreignKey        bool
}

// transformColumnConstraints maps ColumnConstraint* in order.
func (tc *transformContext) transformColumnConstraints(n tnode) []columnConstraintEntry {
	var out []columnConstraintEntry
	for _, c := range n.repeat() {
		out = append(out, tc.transformColumnConstraint(c))
	}
	return out
}

// quotedIdent renders an Identifier formatted into an upstream exception
// message (ExceptionFormatValue for Identifier: always double-quoted,
// embedded quotes doubled).
func quotedIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// sqlIdentifier renders an identifier the way upstream's
// SQLIdentifier::ToString does (RequiresQuotesForPrinting with
// allow_caps): double-quoted when empty, a keyword, `_` followed by a
// digit, or containing anything but letters, digits (not leading) and
// underscores.
func sqlIdentifier(s string) string {
	needs := s == "" || token.IsKeyword(s) || (len(s) > 1 && s[0] == '_' && s[1] >= '0' && s[1] <= '9')
	for i := 0; i < len(s) && !needs; i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_':
		case c >= '0' && c <= '9':
			needs = i == 0
		default:
			needs = true
		}
	}
	if !needs {
		return s
	}
	return quotedIdent(s)
}

// varcharTypeNames are the builtin type names DefaultTypeGenerator maps
// to VARCHAR.
var varcharTypeNames = map[string]bool{
	"varchar": true, "bpchar": true, "string": true, "char": true, "nvarchar": true, "text": true,
}

// applyColumnCollation ports PEGTransformerFactory::ApplyColumnCollation's
// parse-time checks: a collation needs a VARCHAR-typed column.
func applyColumnCollation(typ *ast.TypeExpression, collation string) {
	if typ == nil {
		raise("Specify the VARCHAR type to set a collation")
	}
	if !varcharTypeNames[strings.ToLower(typ.TypeName)] {
		raise("Only VARCHAR columns can have collations!")
	}
}

// dottedIdentifier flattens DottedIdentifier (Identifier DotColLabel*).
func dottedIdentifier(n tnode) []string {
	path := []string{identifierText(n.child(0))}
	for _, dot := range n.child(1).repeat() {
		path = append(path, identifierText(dot.child(1)))
	}
	return path
}

func setConstraintName(c ast.Constraint, name string) {
	switch t := c.(type) {
	case *ast.NotNullConstraint:
		t.Name = name
	case *ast.UniqueConstraint:
		t.Name = name
	case *ast.CheckConstraint:
		t.Name = name
	case *ast.DefaultConstraint:
		t.Name = name
	case *ast.ForeignKeyConstraint:
		t.Name = name
	}
}

// transformColumnConstraint maps one ColumnConstraint.
// ColumnConstraint <- NameableColumnConstraint / ColumnCollation
// NameableColumnConstraint <- ConstraintNameClause? ColumnConstraintElement
// Column constraint names are parsed but not stored (upstream's
// TransformNameableColumnConstraint).
func (tc *transformContext) transformColumnConstraint(n tnode) columnConstraintEntry {
	_, alt := n.sole().choice()
	if alt.name() == "ColumnCollation" {
		// 'COLLATE' DottedIdentifier
		return columnConstraintEntry{collation: strings.Join(dottedIdentifier(alt.child(1)), ".")}
	}
	if alt.name() != "NameableColumnConstraint" {
		shapeError(alt, "unknown column constraint")
	}
	_, elem := alt.child(1).sole().choice()
	switch elem.name() {
	case "NotNullConstraint":
		// NullConstraint / NotNullColumnConstraint
		_, kind := elem.sole().choice()
		c := &ast.NotNullConstraint{Null: kind.name() == "NullConstraint"}
		c.SetSpan(elem.span())
		return columnConstraintEntry{constraint: c, notNull: !c.Null}
	case "UniqueConstraint":
		c := &ast.UniqueConstraint{}
		c.SetSpan(elem.span())
		return columnConstraintEntry{constraint: c, unique: true}
	case "PrimaryKeyConstraint":
		c := &ast.UniqueConstraint{IsPrimaryKey: true}
		c.SetSpan(elem.span())
		return columnConstraintEntry{constraint: c, unique: true, primary: true}
	case "DefaultValue":
		// 'DEFAULT' ColumnDefaultExpr
		c := &ast.DefaultConstraint{Expr: tc.transformColumnDefaultExpr(elem.child(1))}
		c.SetSpan(elem.span())
		return columnConstraintEntry{constraint: c, isDefault: true}
	case "CheckConstraint":
		c := &ast.CheckConstraint{Expr: checkConstraintExpr(tc.transformExpression(elem.child(1).parens()))}
		c.SetSpan(elem.span())
		return columnConstraintEntry{constraint: c, check: true}
	case "ForeignKeyConstraint":
		return columnConstraintEntry{constraint: tc.transformForeignKey(elem, nil), foreignKey: true}
	case "ColumnCompression":
		// 'USING' 'COMPRESSION' ColIdOrString
		return columnConstraintEntry{compression: identifierText(elem.child(2))}
	}
	shapeError(elem, "unknown column constraint")
	return columnConstraintEntry{}
}

// transformColumnDefaultExpr maps ColumnDefaultExpr (the restricted
// OR/AND ladder used by DEFAULT clauses).
func (tc *transformContext) transformColumnDefaultExpr(n tnode) ast.Expr {
	// ColumnDefaultExpr <- ColDefOrExpr; ColDefOrExpr and ColDefAndExpr
	// mirror the conjunction ladder shapes.
	colDefOr := n.sole()
	return tc.transformConjunction(colDefOr, ast.ConjunctionOr, func(and tnode) ast.Expr {
		return tc.transformConjunction(and, ast.ConjunctionAnd, tc.transformIsDistinctFrom)
	})
}

// transformForeignKey maps ForeignKeyConstraint
// ('REFERENCES' BaseTableName Parens(ColumnList)? KeyActions?), with the
// referencing columns of a table-level FOREIGN KEY.
func (tc *transformContext) transformForeignKey(n tnode, columns []string) *ast.ForeignKeyConstraint {
	c := &ast.ForeignKeyConstraint{Columns: columns}
	c.SetSpan(n.span())
	ref := &ast.BaseTableRef{}
	tc.fillBaseTableName(ref, n.child(1))
	c.Table = ast.QualifiedName{Catalog: ref.CatalogName, Schema: ref.SchemaName, Name: ref.TableName}
	if cols, ok := n.child(2).opt(); ok {
		c.ReferencedColumns = identifierList(cols.parens().sole())
	}
	// KeyActions <- UpdateFirstKeyActions / DeleteFirstKeyActions
	// UpdateFirstKeyActions <- UpdateAction DeleteAction?
	// DeleteFirstKeyActions <- DeleteAction UpdateAction?
	if actions, ok := n.child(3).opt(); ok {
		_, order := actions.sole().choice()
		first := keyAction(order.child(0).child(2))
		var second ast.KeyAction
		hasSecond := false
		if a, ok := order.child(1).opt(); ok {
			second, hasSecond = keyAction(a.child(2)), true
		}
		if order.name() == "UpdateFirstKeyActions" {
			c.OnUpdate = first
			if hasSecond {
				c.OnDelete = second
			}
		} else {
			c.OnDelete = first
			if hasSecond {
				c.OnUpdate = second
			}
		}
	}
	// the table-level form checks the column counts (upstream's
	// TransformTopForeignKeyConstraint, after the actions transform)
	if len(c.Columns) > 0 && len(c.ReferencedColumns) > 0 && len(c.Columns) != len(c.ReferencedColumns) {
		raise("The number of referencing and referenced columns for foreign keys must be the same")
	}
	return c
}

func keyAction(n tnode) ast.KeyAction {
	_, alt := n.sole().choice()
	switch alt.name() {
	case "NoKeyAction":
		return ast.KeyActionNone
	case "RestrictKeyAction":
		return ast.KeyActionRestrict
	case "CascadeKeyAction", "SetNullKeyAction", "SetDefaultKeyAction":
		// DuckDB does not support referential actions
		raise("FOREIGN KEY constraints cannot use CASCADE, SET NULL or SET DEFAULT")
	}
	shapeError(alt, "unknown key action")
	return ""
}

// verifyGeneratedColumnRefs ports VerifyColumnRefs: generated column
// expressions cannot contain qualified column references.
func verifyGeneratedColumnRefs(e ast.Expr) {
	if e == nil {
		return
	}
	if ref, ok := e.(*ast.ColumnRefExpression); ok && len(ref.ColumnNames) > 1 {
		raise("Qualified (tbl.name) column references are not allowed inside of generated column expressions")
	}
	for _, child := range e.Children() {
		if expr, ok := child.(ast.Expr); ok {
			verifyGeneratedColumnRefs(expr)
		}
	}
}

// hasSubquery reports whether the expression tree contains a subquery
// (upstream's ParsedExpression::HasSubquery).
func hasSubquery(e ast.Expr) bool {
	if e == nil {
		return false
	}
	if _, ok := e.(*ast.SubqueryExpression); ok {
		return true
	}
	for _, child := range e.Children() {
		if expr, ok := child.(ast.Expr); ok && hasSubquery(expr) {
			return true
		}
	}
	return false
}

// checkConstraintExpr guards a CHECK constraint body.
func checkConstraintExpr(e ast.Expr) ast.Expr {
	if hasSubquery(e) {
		raise("subqueries prohibited in CHECK constraints")
	}
	return e
}

// TopLevelConstraint <- ConstraintNameClause? TopLevelConstraintList
func (tc *transformContext) transformTopLevelConstraint(n tnode) ast.Constraint {
	name := ""
	if named, ok := n.child(0).opt(); ok {
		name = identifierText(named.child(1))
	}
	_, alt := n.child(1).sole().choice()
	var out ast.Constraint
	switch alt.name() {
	case "TopPrimaryKeyConstraint":
		// 'PRIMARY' 'KEY' ColumnIdList
		c := &ast.UniqueConstraint{IsPrimaryKey: true, Columns: identifierList(alt.child(2).sole().parens())}
		c.SetSpan(alt.span())
		out = c
	case "TopUniqueConstraint":
		c := &ast.UniqueConstraint{Columns: identifierList(alt.child(1).sole().parens())}
		c.SetSpan(alt.span())
		out = c
	case "TopCheckConstraint":
		inner := alt.sole()
		c := &ast.CheckConstraint{Expr: checkConstraintExpr(tc.transformExpression(inner.child(1).parens()))}
		c.SetSpan(alt.span())
		out = c
	case "TopForeignKeyConstraint":
		// 'FOREIGN' 'KEY' ColumnIdList ForeignKeyConstraint
		columns := identifierList(alt.child(2).sole().parens())
		out = tc.transformForeignKey(alt.child(3), columns)
	default:
		shapeError(alt, "unknown table constraint")
	}
	if name != "" {
		setConstraintName(out, name)
	}
	return out
}

// CreateSchemaStmt <- 'SCHEMA' IfNotExists? QualifiedName WithOptionList?
// WithOptionList <- 'WITH' RelOptionList
func (tc *transformContext) transformCreateSchema(n tnode) ast.CreateInfo {
	info := &ast.CreateSchemaInfo{}
	info.SetSpan(n.span())
	applyIfNotExists(&info.CreateInfoBase, n.child(1))
	cat, schema, name := tc.transformQualifiedNameParts(n.child(2))
	// a schema's qualifier is its catalog
	if cat == "" {
		cat = schema
	}
	info.Catalog, info.Name = cat, name
	if wl, ok := n.child(3).opt(); ok {
		info.Options = tc.transformRelOptionList(wl.child(1))
	}
	return info
}

// CreateViewStmt <- CreateSecure? CreateRecursive? 'VIEW' IfNotExists?
// QualifiedName InsertColumnList? WithList? 'AS' SelectStatementInternal
func (tc *transformContext) transformCreateView(n tnode) ast.CreateInfo {
	info := &ast.CreateViewInfo{}
	info.SetSpan(n.span())
	info.Secure = optMatched(n.child(0))
	info.Recursive = optMatched(n.child(1))
	applyIfNotExists(&info.CreateInfoBase, n.child(3))
	tc.fillQualifiedName(&info.CreateInfoBase, n.child(4))
	cols, hasCols := n.child(5).opt()
	if wl, ok := n.child(6).opt(); ok {
		info.Options = tc.transformWithList(wl)
	}
	info.Query = tc.transformSelectInternalStatement(n.child(8))
	if info.Recursive && !hasCols {
		raise("CREATE RECURSIVE VIEW requires a column list")
	}
	if hasCols {
		info.Aliases = identifierList(cols.sole().parens())
	}
	// non-constant / non-boolean DEFER_BINDING values raise InvalidInput
	// upstream (post-parse)
	for _, o := range info.Options {
		if !strings.EqualFold(o.Name, "defer_binding") {
			raise("Only DEFER_BINDING is currently supported as option for CREATE VIEW")
		}
	}
	tc.pivotEntryCheck("view")
	return info
}

// CreateIndexStmt <- UniqueIndex? 'INDEX' IfNotExists? IndexName? 'ON'
// BaseTableName IndexType? Parens(List(IndexElement)) WithList?
// WhereClause?
// Without a USING clause, unqualified column elements are qualified with
// the table name (TransformCreateIndexStmt).
func (tc *transformContext) transformCreateIndex(n tnode) ast.CreateInfo {
	info := &ast.CreateIndexInfo{}
	info.SetSpan(n.span())
	if _, ok := n.child(0).opt(); ok {
		info.Unique = true
	}
	applyIfNotExists(&info.CreateInfoBase, n.child(2))
	if name, ok := n.child(3).opt(); ok {
		info.Name = identifierText(name)
	}
	ref := &ast.BaseTableRef{}
	tc.fillBaseTableName(ref, n.child(5))
	info.Table = ast.QualifiedName{Catalog: ref.CatalogName, Schema: ref.SchemaName, Name: ref.TableName}
	idxType, hasIndexType := n.child(6).opt()
	if hasIndexType {
		info.IndexType = identifierText(idxType.child(1))
	}
	for _, e := range n.child(7).parens().listElems() {
		// IndexElement <- Expression DescOrAsc? NullsFirstOrLast?
		expr := tc.transformExpression(e.child(0))
		if col, ok := expr.(*ast.ColumnRefExpression); ok && !hasIndexType && len(col.ColumnNames) == 1 {
			qualified := &ast.ColumnRefExpression{ColumnNames: []string{ref.TableName, col.ColumnNames[0]}}
			qualified.SetSpan(invalidSpan)
			expr = qualified
		}
		info.Elements = append(info.Elements, ast.IndexElement{
			Expr:      expr,
			OrderType: orderTypeOf(e.child(1)),
			NullOrder: nullOrderOf(e.child(2)),
		})
	}
	if wl, ok := n.child(8).opt(); ok {
		info.Options = tc.transformWithList(wl)
	}
	if wc, ok := n.child(9).opt(); ok {
		info.Where = tc.transformExpression(wc.child(1))
	}
	return info
}

// sequenceOptionKey names an option for duplicate detection (upstream's
// option pair keys).
func sequenceOptionKey(alt tnode) string {
	switch alt.name() {
	case "SeqSetCycle":
		return "cycle"
	case "SeqSetIncrement":
		return "increment"
	case "SeqSetMinMax":
		if seqIsMin(alt.child(0)) {
			return "minvalue"
		}
		return "maxvalue"
	case "SeqNoMinMax":
		if seqIsMin(alt.child(1)) {
			return "nominvalue"
		}
		return "nomaxvalue"
	case "SeqStartWith":
		return "start"
	case "SeqOwnedBy":
		return "owned"
	}
	shapeError(alt, "unknown sequence option")
	return ""
}

// seqConstant requires a sequence option value to fold to a constant
// (upstream's TransformSeqSet* transforms; negated literals are already
// folded by the expression transformer), and an integer one when set
// (CheckIntegerSequenceOption).
func (tc *transformContext) seqConstant(option string, e ast.Expr) ast.Value {
	c, ok := e.(*ast.ConstantExpression)
	if !ok {
		raise("Expected constant expression.")
	}
	if !c.Value.IsNull && !literalIsInteger(c.Value) {
		raise("Expected an integer argument for %s, got %s", option, tc.literalText(c))
	}
	return c.Value
}

// literalIsInteger reports whether a constant is an INTEGER literal
// (upstream's LiteralKind::INTEGER: digits only, no '.' or exponent).
func literalIsInteger(v ast.Value) bool {
	switch v.Type.ID {
	case "INTEGER", "BIGINT", "HUGEINT", "UHUGEINT", "BIGNUM", "VARINT":
		return true
	}
	return false
}

// literalText renders a constant the way upstream's Literal::ToString
// does: numbers as written, strings single-quoted, booleans as
// true/false.
func (tc *transformContext) literalText(c *ast.ConstantExpression) string {
	v := c.Value
	switch {
	case v.IsNull:
		return "NULL"
	case v.Kind == ast.ValueBool:
		if v.Bool {
			return "true"
		}
		return "false"
	case v.Kind == ast.ValueString:
		return "'" + strings.ReplaceAll(v.Str, "'", "''") + "'"
	}
	sp := c.Span
	if sp.Start >= 0 && sp.End <= len(tc.src) && sp.Start < sp.End {
		return strings.Join(strings.Fields(tc.src[sp.Start:sp.End]), "")
	}
	return v.Type.ID
}

// seqInt extracts an int64 option value. Values outside int64 fail
// upstream with a conversion error (post-parse), so ok=false skips the
// parse-time range checks.
func seqInt(v ast.Value) (int64, bool) {
	if v.Kind == ast.ValueInt64 {
		return v.Int64, true
	}
	return 0, false
}

// CreateSequenceStmt <- 'SEQUENCE' IfNotExists? QualifiedName SequenceOption*
// Port of TransformCreateSequenceStmt, including its parse-time option
// validation. The option transforms run first (constant and integer
// checks), then duplicates, NULL values, zero increments and the
// min/max/start range checks over the evaluated defaults.
func (tc *transformContext) transformCreateSequence(n tnode) ast.CreateInfo {
	info := &ast.CreateSequenceInfo{}
	info.SetSpan(n.span())
	applyIfNotExists(&info.CreateInfoBase, n.child(1))
	tc.fillQualifiedName(&info.CreateInfoBase, n.child(2))

	type seqOpt struct {
		key  string
		alt  tnode
		expr ast.Expr
	}
	var opts []seqOpt
	for _, o := range n.child(3).repeat() {
		_, alt := o.sole().choice()
		key := sequenceOptionKey(alt)
		var scratch ast.CreateSequenceInfo
		if alt.name() != "SeqOwnedBy" {
			tc.applySequenceOption(&scratch, o)
		}
		var expr ast.Expr
		switch key {
		case "increment":
			expr = scratch.Increment
		case "minvalue":
			expr = scratch.MinValue
		case "maxvalue":
			expr = scratch.MaxValue
		case "start":
			expr = scratch.StartValue
		}
		opts = append(opts, seqOpt{key: key, alt: alt, expr: expr})
	}

	seen := map[string]bool{}
	for _, o := range opts {
		if seen[o.key] {
			raise("%s should be passed at most once", strings.ToUpper(o.key[:1])+o.key[1:])
		}
		seen[o.key] = true
	}
	if seen["nominvalue"] && seen["minvalue"] {
		raise("Minvalue should be passed at most once")
	}
	if seen["nomaxvalue"] && seen["maxvalue"] {
		raise("Maxvalue should be passed at most once")
	}

	inc, minV, maxV, start := int64(1), int64(1), int64(math.MaxInt64), int64(0)
	minSet, maxSet, startSet := false, false, false
	evalOK := true
	for _, o := range opts {
		switch o.key {
		case "cycle":
			_, kind := o.alt.sole().choice()
			info.Cycle = kind.name() == "SeqCycle"
		case "increment":
			info.Increment = o.expr
			v := o.expr.(*ast.ConstantExpression).Value
			if v.IsNull {
				raise("INCREMENT must not be NULL")
			}
			if iv, ok := seqInt(v); ok {
				inc = iv
				if inc == 0 {
					raise("Increment must not be zero")
				}
			} else {
				evalOK = false
			}
		case "minvalue", "maxvalue":
			v := o.expr.(*ast.ConstantExpression).Value
			if o.key == "minvalue" {
				info.MinValue = o.expr
				if v.IsNull {
					raise("MINVALUE must not be NULL")
				}
				if iv, ok := seqInt(v); ok {
					minV, minSet = iv, true
				} else {
					evalOK = false
				}
			} else {
				info.MaxValue = o.expr
				if v.IsNull {
					raise("MAXVALUE must not be NULL")
				}
				if iv, ok := seqInt(v); ok {
					maxV, maxSet = iv, true
				} else {
					evalOK = false
				}
			}
		case "nominvalue":
			info.NoMinValue = true
		case "nomaxvalue":
			info.NoMaxValue = true
		case "start":
			info.StartValue = o.expr
			v := o.expr.(*ast.ConstantExpression).Value
			if v.IsNull {
				raise("START value must not be NULL")
			}
			if iv, ok := seqInt(v); ok {
				start, startSet = iv, true
			} else {
				evalOK = false
			}
		case "owned":
			// OWNED BY is an ALTER SEQUENCE option upstream
			raise("Unrecognized option \"owned\" for CREATE SEQUENCE")
		}
	}
	if evalOK {
		if inc < 0 {
			if !minSet {
				minV = math.MinInt64
			}
			if !maxSet {
				maxV = -1
			}
		}
		if !startSet {
			if inc < 0 {
				start = maxV
			} else {
				start = minV
			}
		}
		if maxV <= minV {
			raise("MINVALUE (%d) must be less than MAXVALUE (%d)", minV, maxV)
		}
		if start < minV {
			raise("START value (%d) cannot be less than MINVALUE (%d)", start, minV)
		}
		if start > maxV {
			raise("START value (%d) cannot be greater than MAXVALUE (%d)", start, maxV)
		}
	}
	return info
}

// applySequenceOption maps one SequenceOption for ALTER SEQUENCE. The
// option transforms still require constant values (upstream raises in the
// per-option transforms); the CREATE-only NULL/range checks do not apply.
func (tc *transformContext) applySequenceOption(info *ast.CreateSequenceInfo, n tnode) {
	_, alt := n.sole().choice()
	switch alt.name() {
	case "SeqSetCycle":
		_, kind := alt.sole().choice()
		info.Cycle = kind.name() == "SeqCycle"
	case "SeqSetIncrement":
		// 'INCREMENT' 'BY'? Expression
		info.Increment = tc.transformExpression(alt.child(2))
		tc.seqConstant("INCREMENT", info.Increment)
	case "SeqSetMinMax":
		// SeqMinOrMax Expression
		expr := tc.transformExpression(alt.child(1))
		if seqIsMin(alt.child(0)) {
			tc.seqConstant("MINVALUE", expr)
		} else {
			tc.seqConstant("MAXVALUE", expr)
		}
		if seqIsMin(alt.child(0)) {
			info.MinValue = expr
		} else {
			info.MaxValue = expr
		}
	case "SeqNoMinMax":
		// 'NO' SeqMinOrMax
		if seqIsMin(alt.child(1)) {
			info.NoMinValue = true
		} else {
			info.NoMaxValue = true
		}
	case "SeqStartWith":
		// 'START' 'WITH'? Expression
		info.StartValue = tc.transformExpression(alt.child(2))
		tc.seqConstant("START", info.StartValue)
	case "SeqOwnedBy":
		cat, schema, name := tc.transformQualifiedNameParts(alt.child(2))
		info.OwnedBy = &ast.QualifiedName{Catalog: cat, Schema: schema, Name: name}
	default:
		shapeError(alt, "unknown sequence option")
	}
}

func seqIsMin(n tnode) bool {
	_, kind := n.sole().choice()
	return kind.name() == "MinValue"
}

// CreateTypeStmt <- 'TYPE' IfNotExists? QualifiedName 'AS' CreateType
func (tc *transformContext) transformCreateType(n tnode) ast.CreateInfo {
	info := &ast.CreateTypeInfo{}
	info.SetSpan(n.span())
	applyIfNotExists(&info.CreateInfoBase, n.child(1))
	tc.fillQualifiedName(&info.CreateInfoBase, n.child(2))
	_, kind := n.child(4).sole().choice()
	switch kind.name() {
	case "CreateTypeFromType":
		info.Type = tc.transformType(kind.sole())
	case "EnumSelectType":
		info.IsEnum = true
		info.EnumQuery = tc.transformSelectInternalStatement(kind.child(1).parens())
	case "EnumStringLiteralList":
		// 'ENUM' Parens(List(StringLiteral)?) ArrayBounds*: with bounds
		// the created type is a list/array of the enum
		info.IsEnum = true
		if list, ok := kind.child(1).parens().opt(); ok {
			for _, s := range list.listElems() {
				info.EnumValues = append(info.EnumValues, identifierText(s))
			}
		}
		if bounds := kind.child(2).repeat(); len(bounds) > 0 {
			var args []ast.Expr
			for _, v := range info.EnumValues {
				args = append(args, constExpr(invalidSpan, varcharValue(v)))
			}
			t := typeExpr(invalidSpan, "ENUM", args...)
			for _, b := range bounds {
				t = tc.applyArrayBound(t, b)
			}
			info.Type = t
		}
	default:
		shapeError(kind, "unknown create type")
	}
	return info
}

// ---- ALTER -------------------------------------------------------------

// AlterStatement <- 'ALTER' AlterOptions
func (tc *transformContext) transformAlter(n tnode) ast.Stmt {
	stmt := &ast.AlterStatement{}
	stmt.SetSpan(n.span())
	_, alt := n.child(1).sole().choice()
	switch alt.name() {
	case "AlterTableStmt":
		// 'TABLE' IfExists? BaseTableName List(AlterTableOptions)
		stmt.Entity = ast.AlterEntityTable
		stmt.IfExists = optMatched(alt.child(1))
		ref := &ast.BaseTableRef{}
		tc.fillBaseTableName(ref, alt.child(2))
		stmt.Name = ast.QualifiedName{Catalog: ref.CatalogName, Schema: ref.SchemaName, Name: ref.TableName}
		for _, opt := range alt.child(3).listElems() {
			stmt.Actions = append(stmt.Actions, tc.transformAlterTableOption(opt))
		}
		if len(stmt.Actions) > 1 {
			raise("Only one ALTER command per statement is supported")
		}
	case "AlterViewStmt":
		// 'VIEW' IfExists? BaseTableName RenameAlter
		stmt.Entity = ast.AlterEntityView
		stmt.IfExists = optMatched(alt.child(1))
		ref := &ast.BaseTableRef{}
		tc.fillBaseTableName(ref, alt.child(2))
		stmt.Name = ast.QualifiedName{Catalog: ref.CatalogName, Schema: ref.SchemaName, Name: ref.TableName}
		stmt.Actions = append(stmt.Actions, tc.transformRenameAlter(alt.child(3)))
	case "AlterSequenceStmt":
		// 'SEQUENCE' IfExists? QualifiedSequenceName AlterSequenceOptions
		stmt.Entity = ast.AlterEntitySequence
		stmt.IfExists = optMatched(alt.child(1))
		stmt.Name = tc.qualifiedSequenceName(alt.child(2))
		_, opts := alt.child(3).sole().choice()
		switch opts.name() {
		case "RenameAlterSequenceOptions":
			stmt.Actions = append(stmt.Actions, tc.transformRenameAlter(opts.sole()))
		case "SetSequenceOption":
			seq := &ast.SetSequenceOptionInfo{}
			seq.SetSpan(opts.span())
			seenOwned := false
			for _, o := range opts.sole().repeat() {
				_, optAlt := o.sole().choice()
				if optAlt.name() == "SeqOwnedBy" {
					if seenOwned {
						raise("Owned by value should be passed at most once")
					}
					seenOwned = true
				}
				tc.applySequenceOption(&seq.Options, o)
			}
			stmt.Actions = append(stmt.Actions, seq)
		}
	case "AlterDatabaseStmt":
		// 'DATABASE' IfExists? Identifier 'SET' 'ALIAS' 'TO' Identifier
		stmt.Entity = ast.AlterEntityDatabase
		stmt.IfExists = optMatched(alt.child(1))
		stmt.Name = ast.QualifiedName{Name: identifierText(alt.child(2))}
		info := &ast.SetDatabaseAliasInfo{NewName: identifierText(alt.child(6))}
		info.SetSpan(alt.span())
		stmt.Actions = append(stmt.Actions, info)
	case "AlterSchemaStmt":
		// 'SCHEMA' IfExists? QualifiedName AlterSchemaOptions
		// AlterSchemaOptions <- RenameAlter / SetOptions / ResetOptions
		// (RENAME raises NotImplemented upstream: post-parse)
		stmt.Entity = ast.AlterEntitySchema
		stmt.IfExists = optMatched(alt.child(1))
		cat, schema, name := tc.transformQualifiedNameParts(alt.child(2))
		if cat == "" {
			cat = schema
		}
		stmt.Name = ast.QualifiedName{Catalog: cat, Name: name}
		_, opt := alt.child(3).sole().choice()
		switch opt.name() {
		case "RenameAlter":
			stmt.Actions = append(stmt.Actions, tc.transformRenameAlter(opt))
		case "SetOptions":
			stmt.Actions = append(stmt.Actions, tc.transformSetOptions(opt))
		case "ResetOptions":
			stmt.Actions = append(stmt.Actions, tc.transformResetOptions(opt))
		default:
			shapeError(opt, "unknown alter schema option")
		}
	default:
		shapeError(alt, "unknown alter statement")
	}
	return stmt
}

func optMatched(n tnode) bool {
	_, ok := n.opt()
	return ok
}

func (tc *transformContext) qualifiedSequenceName(n tnode) ast.QualifiedName {
	// QualifiedSequenceName <- CatalogQualification? SchemaQualification? SequenceName
	q := ast.QualifiedName{}
	if cq, ok := n.child(0).opt(); ok {
		q.Catalog = identifierText(cq.child(0))
	}
	if sq, ok := n.child(1).opt(); ok {
		q.Schema = identifierText(sq.child(0))
	}
	if q.Catalog != "" && q.Schema == "" {
		q.Catalog, q.Schema = "", q.Catalog
	}
	q.Name = identifierText(n.child(2))
	return q
}

// transformRenameAlter maps RenameAlter ('RENAME' 'TO' ColIdOrString).
func (tc *transformContext) transformRenameAlter(n tnode) ast.AlterInfo {
	info := &ast.RenameInfo{NewName: identifierText(n.child(2))}
	info.SetSpan(n.span())
	return info
}

// nestedColumnName flattens NestedColumnName (IdentifierDot* ColumnName).
func nestedColumnName(n tnode) []string {
	var path []string
	for _, id := range n.child(0).repeat() {
		path = append(path, identifierText(id.child(0)))
	}
	return append(path, identifierText(n.child(1)))
}

// transformAlterTableOption maps one AlterTableOptions entry.
func (tc *transformContext) transformAlterTableOption(n tnode) ast.AlterInfo {
	_, alt := n.sole().choice()
	switch alt.name() {
	case "AddColumn":
		// 'ADD' 'COLUMN'? IfNotExists? AddColumnEntry
		info := &ast.AddColumnInfo{IfNotExists: optMatched(alt.child(2))}
		info.SetSpan(alt.span())
		info.Column = tc.transformAddColumnEntry(alt.child(3))
		return info
	case "DropConstraint":
		// 'DROP' 'CONSTRAINT' IfExists? ConstraintName DropBehavior?
		// (TransformDropConstraint raises NotImplemented: post-parse)
		info := &ast.DropConstraintInfo{
			IfExists: optMatched(alt.child(2)),
			Name:     identifierText(alt.child(3)),
		}
		if behavior, ok := alt.child(4).opt(); ok {
			_, b := behavior.sole().choice()
			info.Cascade = b.name() == "CascadeDropBehavior"
		}
		info.SetSpan(alt.span())
		return info
	case "DropColumn":
		// 'DROP' 'COLUMN'? IfExists? NestedColumnName DropBehavior?
		info := &ast.DropColumnInfo{
			IfExists: optMatched(alt.child(2)),
			Column:   nestedColumnName(alt.child(3)),
		}
		if behavior, ok := alt.child(4).opt(); ok {
			_, b := behavior.sole().choice()
			info.Cascade = b.name() == "CascadeDropBehavior"
		}
		info.SetSpan(alt.span())
		return info
	case "AlterColumn":
		// 'ALTER' 'COLUMN'? NestedColumnName AlterColumnEntry
		column := nestedColumnName(alt.child(2))
		return tc.transformAlterColumnEntry(column, alt.child(3), alt.span())
	case "AddConstraint":
		info := &ast.AddConstraintInfo{Constraint: tc.transformTopLevelConstraint(alt.child(1))}
		info.SetSpan(alt.span())
		return info
	case "RenameColumn":
		// 'RENAME' 'COLUMN'? NestedColumnName 'TO' ColIdOrString
		info := &ast.RenameColumnInfo{
			Column:  nestedColumnName(alt.child(2)),
			NewName: identifierText(alt.child(4)),
		}
		info.SetSpan(alt.span())
		return info
	case "RenameAlter":
		return tc.transformRenameAlter(alt)
	case "SetPartitionedBy":
		info := &ast.GenericAlterInfo{Kind: "SET_PARTITIONED_BY", Expressions: tc.transformExpressionList(alt.child(3).parens())}
		info.SetSpan(alt.span())
		return info
	case "ResetPartitionedBy":
		info := &ast.GenericAlterInfo{Kind: "RESET_PARTITIONED_BY"}
		info.SetSpan(alt.span())
		return info
	case "SetSortedBy":
		info := &ast.GenericAlterInfo{Kind: "SET_SORTED_BY"}
		for _, o := range tc.transformOrderByExpressionsNode(alt.child(3).parens()) {
			info.Expressions = append(info.Expressions, o.Expression)
		}
		info.SetSpan(alt.span())
		return info
	case "ResetSortedBy":
		info := &ast.GenericAlterInfo{Kind: "RESET_SORTED_BY"}
		info.SetSpan(alt.span())
		return info
	case "SetOptions":
		return tc.transformSetOptions(alt)
	case "ResetOptions":
		return tc.transformResetOptions(alt)
	}
	shapeError(alt, "unknown alter table option")
	return nil
}

// transformSetOptions maps SetOptions <- 'SET' RelOptionList.
func (tc *transformContext) transformSetOptions(n tnode) ast.AlterInfo {
	info := &ast.GenericAlterInfo{Kind: "SET_OPTIONS", Options: tc.transformRelOptionList(n.child(1))}
	info.SetSpan(n.span())
	return info
}

// transformResetOptions maps ResetOptions <- 'RESET' RelOptionList.
// RESET (...) options cannot carry values (a NULL/NONE value passes,
// mirroring upstream's null-constant default); upstream checks the whole
// list and names the last offending option.
func (tc *transformContext) transformResetOptions(n tnode) ast.AlterInfo {
	info := &ast.GenericAlterInfo{Kind: "RESET_OPTIONS", Options: tc.transformRelOptionList(n.child(1))}
	info.SetSpan(n.span())
	offender := ""
	for _, o := range info.Options {
		if c, ok := o.Value.(*ast.ConstantExpression); !ok || !c.Value.IsNull {
			offender = o.Name
		}
	}
	if offender != "" {
		raise("Reset option \"%s\" cannot set any value. Did you mean to use SET?", offender)
	}
	for i := range info.Options {
		info.Options[i].Value = nil
	}
	return info
}

// transformAddColumnEntry ports TransformAddColumnEntry + TransformAddColumn:
// AddColumnEntry <- DottedIdentifier Type? GeneratedColumn? ColumnConstraint*
func (tc *transformContext) transformAddColumnEntry(entry tnode) ast.ColumnDef {
	col := ast.ColumnDef{Names: dottedIdentifier(entry.child(0))}
	col.SetSpan(entry.span())
	if typ, ok := entry.child(1).opt(); ok {
		col.Type = tc.transformType(typ)
	}
	if gen, ok := entry.child(2).opt(); ok {
		tc.transformGeneratedColumn(&col, gen)
	}
	entries := tc.transformColumnConstraints(entry.child(3))
	if col.Type == nil && col.Generated == "" {
		raise("Column definition requires a type or generated expression")
	}
	if col.Generated != "" {
		raise("Adding generated columns after table creation is not supported yet")
	}
	hasDefault := false
	for _, e := range entries {
		switch {
		case e.isDefault:
			if hasDefault {
				raise("Cannot define a default value twice")
			}
			hasDefault = true
		case e.notNull, e.unique && !e.primary:
			// NOT NULL / UNIQUE become follow-up ALTERs upstream
		case e.primary:
			raise("Adding columns with PRIMARY KEY constraints is not supported yet")
		case e.check:
			raise("Adding columns with CHECK constraints is not supported yet")
		case e.foreignKey:
			raise("Adding columns with FOREIGN KEY constraints is not supported yet")
		case e.compression != "":
			if strings.EqualFold(e.compression, "auto") {
				raise("Unrecognized option for column compression")
			}
			col.Compression = e.compression
			continue
		case e.collation != "":
			applyColumnCollation(col.Type, e.collation)
			col.Collation = e.collation
			continue
		}
		if e.constraint != nil {
			col.Constraints = append(col.Constraints, e.constraint)
		}
	}
	return col
}

// relOptionName reads a RelOptionName (DottedColLabel / StringLiteral).
// DottedColLabel <- ColLabel DotColLabel* joins with dots
// (TransformDottedColLabel).
func relOptionName(n tnode) string {
	_, alt := n.sole().choice()
	if alt.name() == "DottedColLabel" {
		return strings.Join(dottedIdentifier(alt), ".")
	}
	return identifierText(alt)
}

// transformOrderByExpressionsNode maps a bare OrderByExpressions node
// (used by SET SORTED BY).
func (tc *transformContext) transformOrderByExpressionsNode(n tnode) []ast.OrderByNode {
	_, alt := n.sole().choice()
	if alt.name() != "OrderByExpressionList" {
		raise("SET SORTED BY requires an expression list")
	}
	var out []ast.OrderByNode
	for _, e := range alt.sole().listElems() {
		out = append(out, ast.OrderByNode{
			Type:       orderTypeOf(e.child(1)),
			NullOrder:  nullOrderOf(e.child(2)),
			Expression: tc.transformExpression(e.child(0)),
		})
	}
	return out
}

// transformChangeNullability maps ChangeNullability (DropOrSet 'NOT' 'NULL').
func (tc *transformContext) transformChangeNullability(column []string, n tnode) ast.AlterInfo {
	_, kind := n.child(0).sole().choice()
	info := &ast.SetNotNullInfo{Column: column, Drop: kind.name() == "DropNullability"}
	info.SetSpan(n.span())
	return info
}

// transformAlterColumnEntry maps AlterColumnEntry.
func (tc *transformContext) transformAlterColumnEntry(column []string, n tnode, sp ast.Span) ast.AlterInfo {
	_, alt := n.sole().choice()
	switch alt.name() {
	case "AddOrDropDefault":
		_, kind := alt.sole().choice()
		info := &ast.SetDefaultInfo{Column: column}
		if kind.name() == "AddDefault" {
			// 'SET' 'DEFAULT' Expression
			info.Expr = tc.transformExpression(kind.child(2))
		}
		info.SetSpan(sp)
		return info
	case "ChangeNullability":
		return tc.transformChangeNullability(column, alt)
	case "AlterType":
		// SetData? 'TYPE' Type? ColumnCollation? UsingExpression?
		info := &ast.AlterColumnTypeInfo{Column: column}
		info.SetSpan(sp)
		if typ, ok := alt.child(2).opt(); ok {
			info.Type = tc.transformType(typ)
		}
		collation := ""
		if coll, ok := alt.child(3).opt(); ok {
			collation = strings.Join(dottedIdentifier(coll.child(1)), ".")
		}
		if using, ok := alt.child(4).opt(); ok {
			info.Using = tc.transformExpression(using.child(1))
		}
		if info.Type == nil && info.Using == nil {
			raise("Omitting the type is only possible in combination with USING")
		}
		if collation != "" {
			applyColumnCollation(info.Type, collation)
			info.Collation = collation
		}
		return info
	}
	shapeError(alt, "unknown alter column entry")
	return nil
}

// ---- DROP --------------------------------------------------------------

// DropStatement <- 'DROP' DropEntries DropBehavior?
func (tc *transformContext) transformDrop(n tnode) ast.Stmt {
	stmt := &ast.DropStatement{}
	stmt.SetSpan(n.span())
	if behavior, ok := n.child(2).opt(); ok {
		_, b := behavior.sole().choice()
		stmt.Cascade = b.name() == "CascadeDropBehavior"
	}
	_, alt := n.child(1).sole().choice()
	switch alt.name() {
	case "DropTable":
		// TableOrView IfExists? List(BaseTableName)
		_, kind := alt.child(0).sole().choice()
		switch kind.name() {
		case "CommentTable":
			stmt.DropType = ast.DropTable
		case "CommentView":
			stmt.DropType = ast.DropView
		case "MaterializedViewEntry":
			stmt.DropType = ast.DropMaterializedView
		}
		stmt.IfExists = optMatched(alt.child(1))
		for _, name := range alt.child(2).listElems() {
			ref := &ast.BaseTableRef{}
			tc.fillBaseTableName(ref, name)
			stmt.Names = append(stmt.Names, ast.QualifiedName{Catalog: ref.CatalogName, Schema: ref.SchemaName, Name: ref.TableName})
		}
	case "DropTableFunction":
		// CommentMacroTable IfExists? List(QualifiedTableFunction)
		stmt.DropType = ast.DropMacroTable
		stmt.IfExists = optMatched(alt.child(1))
		for _, name := range alt.child(2).listElems() {
			stmt.Names = append(stmt.Names, qualifiedTableFunctionName(name))
		}
	case "DropFunction":
		// FunctionTypeMacro IfExists? List(FunctionIdentifier)
		_, kind := alt.child(0).sole().choice()
		if kind.name() == "FunctionTypeMacroKeyword" {
			stmt.DropType = ast.DropMacro
		} else {
			stmt.DropType = ast.DropFunction
		}
		stmt.IfExists = optMatched(alt.child(1))
		for _, name := range alt.child(2).listElems() {
			catalog, schema, fname := tc.transformFunctionIdentifierRaw(name)
			stmt.Names = append(stmt.Names, ast.QualifiedName{Catalog: catalog, Schema: schema, Name: fname})
		}
	case "DropSchema":
		stmt.DropType = ast.DropSchema
		stmt.IfExists = optMatched(alt.child(1))
		for _, name := range alt.child(2).listElems() {
			cat, schema, nm := tc.transformQualifiedNameParts(name)
			if cat == "" {
				cat = schema
			}
			stmt.Names = append(stmt.Names, ast.QualifiedName{Catalog: cat, Name: nm})
		}
	case "DropIndex":
		stmt.DropType = ast.DropIndex
		stmt.IfExists = optMatched(alt.child(1))
		for _, name := range alt.child(2).listElems() {
			stmt.Names = append(stmt.Names, tc.qualifiedIndexName(name))
		}
	case "DropSequence":
		stmt.DropType = ast.DropSequence
		stmt.IfExists = optMatched(alt.child(1))
		for _, name := range alt.child(2).listElems() {
			stmt.Names = append(stmt.Names, tc.qualifiedSequenceName(name))
		}
	case "DropCollation":
		stmt.DropType = ast.DropCollation
		stmt.IfExists = optMatched(alt.child(1))
		for _, name := range alt.child(2).listElems() {
			stmt.Names = append(stmt.Names, ast.QualifiedName{Name: identifierText(name)})
		}
	case "DropType":
		stmt.DropType = ast.DropTypeType
		stmt.IfExists = optMatched(alt.child(1))
		for _, name := range alt.child(2).listElems() {
			catalog, schema, tname := tc.transformQualifiedTypeName(name)
			stmt.Names = append(stmt.Names, ast.QualifiedName{Catalog: catalog, Schema: schema, Name: tname})
		}
	case "DropSecret":
		// Temporary? 'SECRET' IfExists? SecretName DropSecretStorage?
		stmt.DropType = ast.DropSecret
		if temp, ok := alt.child(0).opt(); ok {
			_, kind := temp.sole().choice()
			stmt.Temporary = kind.name() != "Persistent"
		}
		stmt.IfExists = optMatched(alt.child(2))
		stmt.Names = append(stmt.Names, ast.QualifiedName{Name: identifierText(alt.child(3))})
		if storage, ok := alt.child(4).opt(); ok {
			if stmt.Temporary {
				raise("Can not combine TEMPORARY with specifying a storage for drop secret")
			}
			stmt.Storage = identifierText(storage.child(1))
		}
	case "DropTrigger":
		// 'TRIGGER' IfExists? TriggerName 'ON' BaseTableName
		stmt.DropType = ast.DropTrigger
		stmt.IfExists = optMatched(alt.child(1))
		stmt.Names = append(stmt.Names, ast.QualifiedName{Name: identifierText(alt.child(2))})
	default:
		shapeError(alt, "unknown drop entry")
	}
	if _, ok := n.child(2).opt(); ok && stmt.DropType == ast.DropSecret {
		raise("CASCADE and RESTRICT are not supported for DROP SECRET")
	}
	return stmt
}

// qualifiedTableFunctionName maps QualifiedTableFunction
// (CatalogQualification? SchemaQualification* TableFunctionName) to a
// three-part name: like QualifiedName, the schema is the component
// before the name and the catalog leads a path of three or more.
func qualifiedTableFunctionName(n tnode) ast.QualifiedName {
	var path []string
	if cq, ok := n.child(0).opt(); ok {
		path = append(path, identifierText(cq.child(0)))
	}
	for _, sq := range n.child(1).repeat() {
		path = append(path, identifierText(sq.child(0)))
	}
	q := ast.QualifiedName{Name: identifierText(n.child(2))}
	if len(path) >= 1 {
		q.Schema = path[len(path)-1]
	}
	if len(path) >= 2 {
		q.Catalog = path[0]
	}
	return q
}

// qualifiedIndexName maps QualifiedIndexName.
func (tc *transformContext) qualifiedIndexName(n tnode) ast.QualifiedName {
	_, alt := n.sole().choice()
	switch alt.name() {
	case "CatalogReservedSchemaIndex":
		return ast.QualifiedName{
			Catalog: identifierText(alt.child(0).child(0)),
			Schema:  identifierText(alt.child(1).child(0)),
			Name:    identifierText(alt.child(2)),
		}
	case "SchemaReservedIndex":
		return ast.QualifiedName{
			Schema: identifierText(alt.child(0).child(0)),
			Name:   identifierText(alt.child(1)),
		}
	case "QualifiedIndexNameString":
		return ast.QualifiedName{Name: identifierText(alt.sole())}
	}
	shapeError(alt, "unknown qualified index name")
	return ast.QualifiedName{}
}

// transformFunctionIdentifierRaw is transformFunctionIdentifier without
// the lower-casing (DROP keeps the spelled name).
func (tc *transformContext) transformFunctionIdentifierRaw(n tnode) (catalog, schema, name string) {
	_, alt := n.sole().choice()
	switch alt.name() {
	case "CatalogReservedSchemaFunctionName":
		// CatalogQualification ReservedSchemaQualification* ReservedFunctionName
		catalog = identifierText(alt.child(0).child(0))
		if schemas := alt.child(1).repeat(); len(schemas) > 0 {
			schema = identifierText(schemas[len(schemas)-1].child(0))
		}
		name = identifierText(alt.child(2))
	case "SchemaReservedFunctionName":
		schema = identifierText(alt.child(0).child(0))
		name = identifierText(alt.child(1))
	case "FunctionNameAsQualifiedName":
		name = identifierText(alt.sole())
	default:
		shapeError(alt, "unknown function identifier")
	}
	if catalog != "" && schema == "" {
		catalog, schema = "", catalog
	}
	return catalog, schema, name
}
