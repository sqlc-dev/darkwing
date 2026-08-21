// transform_ddl.go: CREATE / ALTER / DROP — the milestone-4 DDL
// transformers: tables (columns + constraints), views, schemas, indexes,
// sequences and types. CREATE MACRO / SECRET / TRIGGER belong to
// milestone 5 and report ErrUnsupported. Port of upstream's
// transformer/statement/transform_create_*.cpp, transform_alter.cpp and
// transform_drop.cpp.
package parser

import (
	"strings"

	"github.com/sqlc-dev/darkwing/ast"
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
	temporary := false
	if temp, ok := n.child(2).opt(); ok {
		// Temporary <- Persistent / TempPersistent / TemporaryPersistent
		_, kind := temp.sole().choice()
		temporary = kind.name() != "Persistent"
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
	case "CreateMacroStmt", "CreateSecretStmt", "CreateTriggerStmt":
		panic(&internalErrorUnsupported{rule: variation.name()})
	default:
		shapeError(variation, "unknown create variation")
	}
	base := info.Base()
	base.Temporary = temporary
	if base.OnConflict == "" || onConflict == ast.CreateReplace {
		if base.OnConflict == ast.CreateIgnore && onConflict == ast.CreateReplace {
			raise("cannot combine OR REPLACE with IF NOT EXISTS")
		}
		if onConflict != ast.CreateError || base.OnConflict == "" {
			if base.OnConflict == "" {
				base.OnConflict = onConflict
			} else if onConflict == ast.CreateReplace {
				base.OnConflict = onConflict
			}
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
		stmt := tc.transformStatement(def.child(4))
		sel, ok := stmt.(*ast.SelectStatement)
		if !ok {
			raise("CREATE TABLE AS requires a SELECT statement")
		}
		info.Query = sel
	case "CreateColumnList":
		// Parens(CreateTableColumnList?) PartitionSortedOptions? WithList?
		if list, ok := def.child(0).parens().opt(); ok {
			for _, elem := range list.sole().listElems() {
				tc.transformCreateTableElement(info, elem)
			}
		}
	default:
		shapeError(def, "unknown create table definition")
	}
	return info
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

// ColumnDefinition <- DottedIdentifier Type? GeneratedColumn?
// ConstraintNameClause? ColumnConstraint*
func (tc *transformContext) transformColumnDefinition(n tnode) ast.ColumnDef {
	col := ast.ColumnDef{}
	col.SetSpan(n.span())
	col.Names = dottedIdentifier(n.child(0))
	if typ, ok := n.child(1).opt(); ok {
		col.Type = tc.transformType(typ)
	}
	if gen, ok := n.child(2).opt(); ok {
		// GeneratedColumn <- Generated? 'AS' Parens(Expression) GeneratedColumnType?
		col.Generated = ast.GeneratedVirtual
		col.Default = tc.transformExpression(gen.child(2).parens())
		if kind, ok := gen.child(3).opt(); ok {
			_, k := kind.sole().choice()
			if k.name() == "StoredGeneratedColumn" {
				col.Generated = ast.GeneratedStored
			}
		}
	}
	constraintName := ""
	if named, ok := n.child(3).opt(); ok {
		constraintName = identifierText(named.child(1))
	}
	for _, c := range n.child(4).repeat() {
		constraint := tc.transformColumnConstraint(&col, c)
		if constraint == nil {
			continue
		}
		if constraintName != "" {
			setConstraintName(constraint, constraintName)
			constraintName = ""
		}
		col.Constraints = append(col.Constraints, constraint)
	}
	return col
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

// transformColumnConstraint maps one ColumnConstraint; collation and
// compression fold into the column itself and return nil.
func (tc *transformContext) transformColumnConstraint(col *ast.ColumnDef, n tnode) ast.Constraint {
	_, alt := n.sole().choice()
	switch alt.name() {
	case "NotNullConstraint":
		// NullConstraint / NotNullColumnConstraint
		_, kind := alt.sole().choice()
		c := &ast.NotNullConstraint{Null: kind.name() == "NullConstraint"}
		c.SetSpan(alt.span())
		return c
	case "UniqueConstraint":
		c := &ast.UniqueConstraint{}
		c.SetSpan(alt.span())
		return c
	case "PrimaryKeyConstraint":
		c := &ast.UniqueConstraint{IsPrimaryKey: true}
		c.SetSpan(alt.span())
		return c
	case "DefaultValue":
		// 'DEFAULT' ColumnDefaultExpr
		c := &ast.DefaultConstraint{Expr: tc.transformColumnDefaultExpr(alt.child(1))}
		c.SetSpan(alt.span())
		return c
	case "CheckConstraint":
		c := &ast.CheckConstraint{Expr: tc.transformExpression(alt.child(1).parens())}
		c.SetSpan(alt.span())
		return c
	case "ForeignKeyConstraint":
		return tc.transformForeignKey(alt, nil)
	case "ColumnCollation":
		col.Collation = strings.Join(dottedIdentifier(alt.child(1)), ".")
		return nil
	case "ColumnCompression":
		col.Compression = identifierText(alt.child(2))
		return nil
	}
	shapeError(alt, "unknown column constraint")
	return nil
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
// ('REFERENCES' BaseTableName Parens(ColumnList)? KeyActions), with the
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
	// KeyActions <- UpdateAction? DeleteAction?
	actions := n.child(3)
	if ua, ok := actions.child(0).opt(); ok {
		c.OnUpdate = keyAction(ua.child(2))
	}
	if da, ok := actions.child(1).opt(); ok {
		c.OnDelete = keyAction(da.child(2))
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
	case "CascadeKeyAction":
		return ast.KeyActionCascade
	case "SetNullKeyAction":
		return ast.KeyActionSetNull
	case "SetDefaultKeyAction":
		return ast.KeyActionSetDefault
	}
	shapeError(alt, "unknown key action")
	return ""
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
		c := &ast.CheckConstraint{Expr: tc.transformExpression(inner.child(1).parens())}
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

// CreateSchemaStmt <- 'SCHEMA' IfNotExists? QualifiedName
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
	return info
}

// CreateViewStmt <- CreateRecursive? 'VIEW' IfNotExists? QualifiedName
// InsertColumnList? WithList? 'AS' SelectStatementInternal
func (tc *transformContext) transformCreateView(n tnode) ast.CreateInfo {
	info := &ast.CreateViewInfo{}
	info.SetSpan(n.span())
	if _, ok := n.child(0).opt(); ok {
		info.Recursive = true
	}
	applyIfNotExists(&info.CreateInfoBase, n.child(2))
	tc.fillQualifiedName(&info.CreateInfoBase, n.child(3))
	if cols, ok := n.child(4).opt(); ok {
		info.Aliases = identifierList(cols.sole().parens())
	}
	info.Query = tc.transformSelectInternalStatement(n.child(7))
	return info
}

// CreateIndexStmt <- UniqueIndex? 'INDEX' IfNotExists? IndexName? 'ON'
// BaseTableName InsertColumnList? IndexType? Parens(List(IndexElement))?
// WithList? WhereClause?
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
	if cols, ok := n.child(6).opt(); ok {
		// old-style column list: plain column references
		for _, c := range cols.sole().parens().listElems() {
			col := &ast.ColumnRefExpression{ColumnNames: []string{identifierText(c)}}
			col.SetSpan(c.span())
			info.Elements = append(info.Elements, ast.IndexElement{Expr: col})
		}
	}
	if idxType, ok := n.child(7).opt(); ok {
		info.IndexType = identifierText(idxType.child(1))
	}
	if elems, ok := n.child(8).opt(); ok {
		for _, e := range elems.parens().listElems() {
			// IndexElement <- Expression DescOrAsc? NullsFirstOrLast?
			info.Elements = append(info.Elements, ast.IndexElement{
				Expr:      tc.transformExpression(e.child(0)),
				OrderType: orderTypeOf(e.child(1)),
				NullOrder: nullOrderOf(e.child(2)),
			})
		}
	}
	if wc, ok := n.child(10).opt(); ok {
		info.Where = tc.transformExpression(wc.child(1))
	}
	return info
}

// CreateSequenceStmt <- 'SEQUENCE' IfNotExists? QualifiedName SequenceOption*
func (tc *transformContext) transformCreateSequence(n tnode) ast.CreateInfo {
	info := &ast.CreateSequenceInfo{}
	info.SetSpan(n.span())
	applyIfNotExists(&info.CreateInfoBase, n.child(1))
	tc.fillQualifiedName(&info.CreateInfoBase, n.child(2))
	for _, opt := range n.child(3).repeat() {
		tc.applySequenceOption(info, opt)
	}
	return info
}

// applySequenceOption maps one SequenceOption.
func (tc *transformContext) applySequenceOption(info *ast.CreateSequenceInfo, n tnode) {
	_, alt := n.sole().choice()
	switch alt.name() {
	case "SeqSetCycle":
		_, kind := alt.sole().choice()
		info.Cycle = kind.name() == "SeqCycle"
	case "SeqSetIncrement":
		// 'INCREMENT' 'BY'? Expression
		info.Increment = tc.transformExpression(alt.child(2))
	case "SeqSetMinMax":
		// SeqMinOrMax Expression
		expr := tc.transformExpression(alt.child(1))
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
		info.IsEnum = true
		if list, ok := kind.child(1).parens().opt(); ok {
			for _, s := range list.listElems() {
				info.EnumValues = append(info.EnumValues, identifierText(s))
			}
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
			for _, o := range opts.sole().repeat() {
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
		// 'SCHEMA' IfExists? QualifiedName RenameAlter
		stmt.Entity = ast.AlterEntitySchema
		stmt.IfExists = optMatched(alt.child(1))
		cat, schema, name := tc.transformQualifiedNameParts(alt.child(2))
		if cat == "" {
			cat = schema
		}
		stmt.Name = ast.QualifiedName{Catalog: cat, Name: name}
		stmt.Actions = append(stmt.Actions, tc.transformRenameAlter(alt.child(3)))
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

// transformRenameAlter maps RenameAlter ('RENAME' 'TO' Identifier).
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
		entry := alt.child(3)
		// AddColumnEntry <- DottedIdentifier Type? GeneratedColumn? ColumnConstraint*
		col := ast.ColumnDef{Names: dottedIdentifier(entry.child(0))}
		col.SetSpan(entry.span())
		if typ, ok := entry.child(1).opt(); ok {
			col.Type = tc.transformType(typ)
		}
		if gen, ok := entry.child(2).opt(); ok {
			col.Generated = ast.GeneratedVirtual
			col.Default = tc.transformExpression(gen.child(2).parens())
			if kind, ok := gen.child(3).opt(); ok {
				_, k := kind.sole().choice()
				if k.name() == "StoredGeneratedColumn" {
					col.Generated = ast.GeneratedStored
				}
			}
		}
		for _, c := range entry.child(3).repeat() {
			if constraint := tc.transformColumnConstraint(&col, c); constraint != nil {
				col.Constraints = append(col.Constraints, constraint)
			}
		}
		info.Column = col
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
	case "ChangeNullability":
		return tc.transformChangeNullability(nil, alt)
	case "RenameColumn":
		// 'RENAME' 'COLUMN'? NestedColumnName 'TO' Identifier
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
		info := &ast.GenericAlterInfo{Kind: "SET_OPTIONS"}
		info.SetSpan(alt.span())
		return info
	case "ResetOptions":
		info := &ast.GenericAlterInfo{Kind: "RESET_OPTIONS"}
		info.SetSpan(alt.span())
		return info
	}
	shapeError(alt, "unknown alter table option")
	return nil
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
		// SetData? 'TYPE' Type? UsingExpression?
		info := &ast.AlterColumnTypeInfo{Column: column}
		info.SetSpan(sp)
		if typ, ok := alt.child(2).opt(); ok {
			info.Type = tc.transformType(typ)
		}
		if using, ok := alt.child(3).opt(); ok {
			info.Using = tc.transformExpression(using.child(1))
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
		// CommentMacroTable IfExists? List(TableFunctionName)
		stmt.DropType = ast.DropMacroTable
		stmt.IfExists = optMatched(alt.child(1))
		for _, name := range alt.child(2).listElems() {
			stmt.Names = append(stmt.Names, ast.QualifiedName{Name: identifierText(name)})
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
	return stmt
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
		catalog = identifierText(alt.child(0).child(0))
		if sq, ok := alt.child(1).opt(); ok {
			schema = identifierText(sq.child(0))
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
