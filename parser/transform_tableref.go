// transform_tableref.go: FROM-clause table references — base tables,
// subqueries, table functions, joins, VALUES lists and PIVOT/UNPIVOT.
// Port of upstream's transform_tableref/*.cpp.
package parser

import (
	"strconv"
	"strings"
	"time"

	"github.com/sqlc-dev/darkwing/ast"
)

// FromClause <- 'FROM' List(TableRef): comma entries fold into implicit
// cross joins.
func (tc *transformContext) transformFromClause(n tnode) ast.TableRef {
	refs := n.child(1).listElems()
	result := tc.transformTableRef(refs[0])
	for _, r := range refs[1:] {
		right := tc.transformTableRef(r)
		join := &ast.JoinRef{
			Left: result, Right: right,
			JoinType: ast.JoinInner, RefType: ast.JoinRefCross,
			IsImplicit:       true,
			NearestCount:     1,
			NearestOrderType: ast.OrderAscending,
		}
		// only the outermost implicit join carries the FROM clause's
		// location; inner ones have none
		join.SetSpan(invalidSpan)
		result = join
	}
	if join, ok := result.(*ast.JoinRef); ok && join.IsImplicit {
		join.SetSpan(n.span())
	}
	return result
}

func newJoinRef(left, right ast.TableRef) *ast.JoinRef {
	j := &ast.JoinRef{
		Left: left, Right: right,
		JoinType: ast.JoinInner, RefType: ast.JoinRefRegular,
		NearestCount:     1,
		NearestOrderType: ast.OrderAscending,
	}
	if left != nil && right != nil {
		j.SetSpan(ast.Span{Start: left.Pos(), End: right.End()})
	}
	return j
}

// TableRef <- InnerTableRef JoinOrPivot*
func (tc *transformContext) transformTableRef(n tnode) ast.TableRef {
	ref := tc.transformInnerTableRef(n.child(0))
	for _, jp := range n.child(1).repeat() {
		ref = tc.transformJoinOrPivot(ref, jp)
	}
	return ref
}

// InnerTableRef <- ValuesRef / TableFunction / TableSubquery / BaseTableRef / ParensTableRef
func (tc *transformContext) transformInnerTableRef(n tnode) ast.TableRef {
	_, alt := n.sole().choice()
	switch alt.name() {
	case "ValuesRef":
		// ValuesClause TableAlias? — a VALUES ref in FROM position is
		// always wrapped in a subquery (SELECT * FROM valueslist), the
		// alias landing on the wrapper
		ref := tc.transformValuesClause(alt.child(0))
		ref.Alias = "valueslist"
		sub := &ast.SubqueryRef{Subquery: &ast.SelectStatement{Node: starSelect(ref, invalidSpan)}}
		sub.SetSpan(alt.span())
		if aliasNode, ok := alt.child(1).opt(); ok {
			sub.Alias, sub.ColumnNameAlias = tc.transformTableAlias(aliasNode)
		}
		return sub
	case "TableFunction":
		return tc.transformTableFunction(alt)
	case "TableSubquery":
		// Lateral? SubqueryReference TableAlias? — the location covers only
		// the parenthesized subquery, not the alias
		ref := &ast.SubqueryRef{Subquery: tc.transformSelectInternalStatement(alt.child(1).sole().parens())}
		ref.SetSpan(alt.child(1).span())
		if aliasNode, ok := alt.child(2).opt(); ok {
			ref.Alias, ref.ColumnNameAlias = tc.transformTableAlias(aliasNode)
		}
		return ref
	case "BaseTableRef":
		return tc.transformBaseTableRef(alt)
	case "ParensTableRef":
		// TableAliasColon? Parens(TableRef) TableAlias? SampleClause?
		inner := tc.transformTableRef(alt.child(1).parens())
		alias := ""
		var colAliases []string
		if colon, ok := alt.child(0).opt(); ok {
			alias = identifierText(colon.child(0))
		}
		if aliasNode, ok := alt.child(2).opt(); ok {
			alias, colAliases = tc.transformTableAlias(aliasNode)
		}
		if alias != "" || len(colAliases) > 0 {
			// aliasing a parenthesized join, VALUES list or subquery wraps
			// it in a fresh subquery carrying the alias
			switch inner.(type) {
			case *ast.ExpressionListRef, *ast.JoinRef, *ast.SubqueryRef:
				sub := &ast.SubqueryRef{Subquery: &ast.SelectStatement{Node: starSelect(inner, invalidSpan)}}
				sub.SetSpan(alt.span())
				inner = sub
			}
		}
		if alias != "" {
			setRefAlias(inner, alias)
			// an aliased parenthesized ref takes the whole ref's span
			if sp, ok := inner.(interface{ SetSpan(ast.Span) }); ok {
				sp.SetSpan(alt.span())
			}
		}
		if len(colAliases) > 0 {
			setRefColumnAliases(inner, colAliases)
		}
		if sc, ok := alt.child(3).opt(); ok {
			setRefSample(inner, tc.transformSampleClause(sc))
		}
		return inner
	}
	shapeError(alt, "unknown inner table ref")
	return nil
}

// setRefAlias assigns the alias on any table ref type.
func setRefAlias(ref ast.TableRef, alias string) {
	switch r := ref.(type) {
	case *ast.BaseTableRef:
		r.Alias = alias
	case *ast.SubqueryRef:
		r.Alias = alias
	case *ast.TableFunctionRef:
		r.Alias = alias
	case *ast.ExpressionListRef:
		r.Alias = alias
	case *ast.JoinRef:
		r.Alias = alias
	case *ast.PivotRef:
		r.Alias = alias
	case *ast.ShowRef:
		r.Alias = alias
	case *ast.EmptyTableRef:
		r.Alias = alias
	}
}

// setRefColumnAliases assigns column aliases on any table ref type.
func setRefColumnAliases(ref ast.TableRef, cols []string) {
	switch r := ref.(type) {
	case *ast.BaseTableRef:
		r.ColumnNameAlias = cols
	case *ast.SubqueryRef:
		r.ColumnNameAlias = cols
	case *ast.TableFunctionRef:
		r.ColumnNameAlias = cols
	case *ast.ExpressionListRef:
		r.ExpectedNames = cols
	case *ast.JoinRef:
		r.ColumnNameAlias = cols
	case *ast.PivotRef:
		r.ColumnNameAlias = cols
	}
}

func setRefSample(ref ast.TableRef, sample *ast.SampleOptions) {
	switch r := ref.(type) {
	case *ast.BaseTableRef:
		r.Sample = sample
	case *ast.SubqueryRef:
		r.Sample = sample
	case *ast.TableFunctionRef:
		r.Sample = sample
	case *ast.ExpressionListRef:
		r.Sample = sample
	case *ast.JoinRef:
		r.Sample = sample
	case *ast.PivotRef:
		r.Sample = sample
	}
}

// BaseTableRef <- TableAliasColon? BaseTableName TableAlias? AtClause? SampleClause?
func (tc *transformContext) transformBaseTableRef(n tnode) ast.TableRef {
	ref := &ast.BaseTableRef{}
	// the ref's location spans the name plus alias/AT clauses
	ref.SetSpan(n.span())
	tc.fillBaseTableName(ref, n.child(1))
	if colon, ok := n.child(0).opt(); ok {
		ref.Alias = identifierText(colon.child(0))
	}
	if aliasNode, ok := n.child(2).opt(); ok {
		ref.Alias, ref.ColumnNameAlias = tc.transformTableAlias(aliasNode)
	}
	if at, ok := n.child(3).opt(); ok {
		ref.At = tc.transformAtClause(at)
	}
	if sc, ok := n.child(4).opt(); ok {
		ref.Sample = tc.transformSampleClause(sc)
	}
	return ref
}

// fillBaseTableName fills catalog/schema/table from a BaseTableName node.
// BaseTableName <- CatalogReservedSchemaTable / SchemaReservedTable /
// UnqualifiedBaseTableName
func (tc *transformContext) fillBaseTableName(ref *ast.BaseTableRef, n tnode) {
	_, alt := n.sole().choice()
	switch alt.name() {
	case "CatalogReservedSchemaTable":
		ref.CatalogName = identifierText(alt.child(0).child(0))
		ref.SchemaName = identifierText(alt.child(1).child(0))
		ref.TableName = identifierText(alt.child(2))
	case "SchemaReservedTable":
		ref.SchemaName = identifierText(alt.child(0).child(0))
		ref.TableName = identifierText(alt.child(1))
	case "UnqualifiedBaseTableName":
		ref.TableName = identifierText(alt.sole())
	default:
		shapeError(alt, "unknown base table name")
	}
}

// transformQualifiedNameParts splits a QualifiedName rule node.
// QualifiedName <- CatalogReservedSchemaIdentifier /
// SchemaReservedIdentifierOrStringLiteral / IdentifierOrStringLiteral
func (tc *transformContext) transformQualifiedNameParts(n tnode) (catalog, schema, name string) {
	_, alt := n.sole().choice()
	switch alt.name() {
	case "CatalogReservedSchemaIdentifier":
		catalog = identifierText(alt.child(0).child(0))
		schema = identifierText(alt.child(1).child(0))
		name = identifierText(alt.child(2))
	case "SchemaReservedIdentifierOrStringLiteral":
		schema = identifierText(alt.child(0).child(0))
		name = identifierText(alt.child(1))
	case "IdentifierOrStringLiteral":
		name = identifierText(alt)
	default:
		shapeError(alt, "unknown qualified name")
	}
	return catalog, schema, name
}

// TableAlias <- TableAliasAs / TableAliasWithoutAs
func (tc *transformContext) transformTableAlias(n tnode) (string, []string) {
	_, alt := n.sole().choice()
	var alias string
	var colNode tnode
	var hasCols bool
	switch alt.name() {
	case "TableAliasAs":
		alias = identifierText(alt.child(1))
		colNode, hasCols = alt.child(2).opt()
	case "TableAliasWithoutAs":
		alias = identifierText(alt.child(0))
		colNode, hasCols = alt.child(1).opt()
	default:
		shapeError(alt, "unknown table alias")
	}
	var cols []string
	if hasCols {
		cols = identifierList(colNode.sole().parens())
	}
	return alias, cols
}

// AtClause <- 'AT' Parens(AtSpecifier)
func (tc *transformContext) transformAtClause(n tnode) *ast.AtClause {
	spec := n.child(1).parens()
	at := &ast.AtClause{}
	at.SetSpan(n.span())
	_, unit := spec.child(0).sole().choice()
	if unit.name() == "VersionAtUnit" {
		at.Unit = ast.AtVersion
	} else {
		at.Unit = ast.AtTimestamp
	}
	at.Expr = tc.transformExpression(spec.child(2))
	return at
}

// ValuesClause <- 'VALUES' List(ValuesExpressions)
func (tc *transformContext) transformValuesClause(n tnode) *ast.ExpressionListRef {
	ref := &ast.ExpressionListRef{}
	ref.SetSpan(invalidSpan)
	for _, row := range n.child(1).listElems() {
		ref.Values = append(ref.Values, tc.transformExpressionList(row.sole().parens()))
	}
	for _, row := range ref.Values[1:] {
		if len(row) != len(ref.Values[0]) {
			raise("VALUES lists must all be the same length, expected %d %s but found a list with %d %s",
				len(ref.Values[0]), plural(len(ref.Values[0]), "entry", "entries"),
				len(row), plural(len(row), "entry", "entries"))
		}
	}
	return ref
}

// TableFunction <- TableFunctionLateralOpt / TableFunctionAliasColon
func (tc *transformContext) transformTableFunction(n tnode) ast.TableRef {
	_, alt := n.sole().choice()
	ref := &ast.TableFunctionRef{}
	ref.SetSpan(alt.span())
	var qualified, args, ordinality tnode
	switch alt.name() {
	case "TableFunctionLateralOpt":
		// Lateral? QualifiedTableFunction TableFunctionArguments WithOrdinality? TableAlias?
		qualified, args, ordinality = alt.child(1), alt.child(2), alt.child(3)
		if aliasNode, ok := alt.child(4).opt(); ok {
			ref.Alias, ref.ColumnNameAlias = tc.transformTableAlias(aliasNode)
		}
	case "TableFunctionAliasColon":
		// TableAliasColon QualifiedTableFunction TableFunctionArguments WithOrdinality? SampleClause?
		ref.Alias = identifierText(alt.child(0).child(0))
		qualified, args, ordinality = alt.child(1), alt.child(2), alt.child(3)
		if sc, ok := alt.child(4).opt(); ok {
			ref.Sample = tc.transformSampleClause(sc)
		}
	default:
		shapeError(alt, "unknown table function")
	}
	if _, ok := ordinality.opt(); ok {
		ref.WithOrdinality = true
	}
	fn := tc.qualifiedFunctionExpr(qualified, args)
	// values(...) is a VALUES list, wrapped like any FROM-position VALUES
	if fn.FunctionName == "values" && fn.Schema == "" && fn.Catalog == "" {
		el := &ast.ExpressionListRef{}
		el.SetSpan(invalidSpan)
		el.Alias = "valueslist"
		var row []ast.Expr
		for _, a := range fn.Arguments {
			row = append(row, a.Expr)
		}
		el.Values = [][]ast.Expr{row}
		sub := &ast.SubqueryRef{Subquery: &ast.SelectStatement{Node: starSelect(el, invalidSpan)}}
		sub.SetSpan(alt.span())
		sub.Alias, sub.ColumnNameAlias = ref.Alias, ref.ColumnNameAlias
		return sub
	}
	ref.Function = fn
	return ref
}

// qualifiedFunctionExpr builds the call expression for a qualified table
// function plus its argument list (shared by table functions and CALL).
// QualifiedTableFunction <- CatalogQualification? SchemaQualification? TableFunctionName
// TableFunctionArguments <- Parens(List(FunctionArgument)?)
func (tc *transformContext) qualifiedFunctionExpr(qualified, args tnode) *ast.FunctionExpression {
	fn := &ast.FunctionExpression{}
	fn.SetSpan(invalidSpan)
	if cq, ok := qualified.child(0).opt(); ok {
		fn.Catalog = identifierText(cq.child(0))
	}
	if sq, ok := qualified.child(1).opt(); ok {
		fn.Schema = identifierText(sq.child(0))
	}
	if fn.Catalog != "" && fn.Schema == "" {
		fn.Catalog, fn.Schema = "", fn.Catalog
	}
	fn.FunctionName = strings.ToLower(identifierText(qualified.child(2)))
	if list, ok := args.sole().parens().opt(); ok {
		for _, a := range list.listElems() {
			fn.Arguments = append(fn.Arguments, tc.transformFunctionArgument(a))
		}
	}
	return fn
}

// ---- joins -------------------------------------------------------------

// JoinOrPivot <- JoinClause / TablePivotClause / TableUnpivotClause
func (tc *transformContext) transformJoinOrPivot(left ast.TableRef, n tnode) ast.TableRef {
	_, alt := n.sole().choice()
	switch alt.name() {
	case "JoinClause":
		out := tc.transformJoinClause(left, alt)
		if j, ok := out.(*ast.JoinRef); ok {
			// the join's location is the join clause itself
			j.SetSpan(alt.span())
		}
		return out
	case "TablePivotClause":
		return tc.transformTablePivot(left, alt)
	case "TableUnpivotClause":
		return tc.transformTableUnpivot(left, alt)
	}
	shapeError(alt, "unknown join-or-pivot")
	return nil
}

// JoinClause <- JoinByClause / RegularJoinClause / JoinWithoutOnClause / NearestJoinClause
func (tc *transformContext) transformJoinClause(left ast.TableRef, n tnode) ast.TableRef {
	_, alt := n.sole().choice()
	switch alt.name() {
	case "RegularJoinClause":
		// Asof? JoinType? 'JOIN' TableRef JoinQualifier
		join := newJoinRef(left, tc.transformTableRef(alt.child(3)))
		if _, asof := alt.child(0).opt(); asof {
			join.RefType = ast.JoinRefAsof
		}
		if jt, ok := alt.child(1).opt(); ok {
			join.JoinType = joinTypeOf(jt)
		}
		tc.applyJoinQualifier(join, alt.child(4))
		return join
	case "JoinByClause":
		// 'JOIN' 'BY' Parens('TYPE' ColLabel) TableRef JoinQualifier
		join := newJoinRef(left, tc.transformTableRef(alt.child(3)))
		label := identifierText(alt.child(2).parens().child(1))
		join.JoinType = joinTypeFromLabel(label)
		tc.applyJoinQualifier(join, alt.child(4))
		return join
	case "JoinWithoutOnClause":
		// JoinPrefix 'JOIN' InnerTableRef
		join := newJoinRef(left, tc.transformInnerTableRef(alt.child(2)))
		_, prefix := alt.child(0).sole().choice()
		switch prefix.name() {
		case "CrossJoinPrefix":
			join.RefType = ast.JoinRefCross
		case "NaturalJoinPrefix":
			join.RefType = ast.JoinRefNatural
			if jt, ok := prefix.child(1).opt(); ok {
				join.JoinType = joinTypeOf(jt)
			}
		case "PositionalJoinPrefix":
			join.RefType = ast.JoinRefPositional
		}
		return join
	case "NearestJoinClause":
		return tc.transformNearestJoin(left, alt)
	}
	shapeError(alt, "unknown join clause")
	return nil
}

// joinTypeOf maps a JoinType rule node.
func joinTypeOf(n tnode) ast.JoinType {
	_, alt := n.sole().choice()
	switch alt.name() {
	case "FullJoin":
		return ast.JoinFull
	case "LeftJoin":
		return ast.JoinLeft
	case "RightJoin":
		return ast.JoinRight
	case "SemiJoin":
		return ast.JoinSemi
	case "AntiJoin":
		return ast.JoinAnti
	}
	return ast.JoinInner
}

// joinTypeFromLabel maps JOIN BY (TYPE label): upstream strips an
// optional _JOIN suffix and matches the enum spelling (mark_join → MARK).
// Unknown labels pass through verbatim — upstream only rejects them
// post-parse, so they are not parser errors.
func joinTypeFromLabel(label string) ast.JoinType {
	name := strings.TrimSuffix(normalizeKeyword(label), "_JOIN")
	switch name {
	case "LEFT", "RIGHT", "INNER", "FULL", "SEMI", "ANTI",
		"RIGHT_SEMI", "RIGHT_ANTI", "SINGLE", "MARK":
		return ast.JoinType(name)
	}
	return ast.JoinType(normalizeKeyword(label))
}

// applyJoinQualifier fills ON/USING.
// JoinQualifier <- OnClause / UsingClause
func (tc *transformContext) applyJoinQualifier(join *ast.JoinRef, n tnode) {
	_, alt := n.sole().choice()
	switch alt.name() {
	case "OnClause":
		join.Condition = tc.transformExpression(alt.child(1))
	case "UsingClause":
		join.UsingColumns = identifierList(alt.child(1).parens())
	default:
		shapeError(alt, "unknown join qualifier")
	}
}

// NearestJoinClause <- NearestJoinBare / NearestJoinAliased
func (tc *transformContext) transformNearestJoin(left ast.TableRef, n tnode) ast.TableRef {
	_, alt := n.sole().choice()
	var right ast.TableRef
	switch alt.name() {
	case "NearestJoinAliased":
		right = tc.transformTableRef(alt.child(2))
	case "NearestJoinBare":
		right = tc.transformNearestBareRef(alt.child(2))
	default:
		shapeError(alt, "unknown nearest join")
	}
	join := newJoinRef(left, right)
	join.RefType = ast.JoinRefNearest
	if jt, ok := alt.child(0).opt(); ok {
		join.JoinType = joinTypeOf(jt)
	}
	if join.JoinType != ast.JoinInner && join.JoinType != ast.JoinLeft {
		raise("NEAREST BY only supports INNER and LEFT OUTER joins, not %s", string(join.JoinType))
	}
	if ae, ok := alt.child(3).opt(); ok {
		_, kind := ae.sole().choice()
		join.NearestApprox = kind.name() == "NearestApprox"
	}
	if num, ok := alt.child(5).opt(); ok {
		v, err := strconv.ParseInt(num.number(), 10, 64)
		if err != nil {
			raise("invalid NEAREST count")
		}
		join.NearestCount = v
	}
	_, ds := alt.child(7).sole().choice()
	if ds.name() == "NearestSimilarity" {
		join.NearestOrderType = ast.OrderDescending
	} else {
		join.NearestOrderType = ast.OrderAscending
	}
	join.RankingExpression = tc.transformExpression(alt.child(8))
	return join
}

// NearestBareTableRef <- NearestValuesRef / NearestTableFunction /
// NearestTableSubquery / NearestBaseTableRef / NearestParensTableRef
func (tc *transformContext) transformNearestBareRef(n tnode) ast.TableRef {
	_, alt := n.sole().choice()
	switch alt.name() {
	case "NearestValuesRef":
		return tc.transformValuesClause(alt.sole())
	case "NearestTableFunction":
		// Lateral? QualifiedTableFunction TableFunctionArguments WithOrdinality?
		ref := &ast.TableFunctionRef{}
		ref.SetSpan(alt.span())
		qualified := alt.child(1)
		fn := &ast.FunctionExpression{FunctionName: strings.ToLower(identifierText(qualified.child(2)))}
		if cq, ok := qualified.child(0).opt(); ok {
			fn.Schema = identifierText(cq.child(0))
		}
		if sq, ok := qualified.child(1).opt(); ok {
			if fn.Schema != "" {
				fn.Catalog, fn.Schema = fn.Schema, identifierText(sq.child(0))
			} else {
				fn.Schema = identifierText(sq.child(0))
			}
		}
		if list, ok := alt.child(2).sole().parens().opt(); ok {
			for _, a := range list.listElems() {
				fn.Arguments = append(fn.Arguments, tc.transformFunctionArgument(a))
			}
		}
		if _, ok := alt.child(3).opt(); ok {
			ref.WithOrdinality = true
		}
		ref.Function = fn
		return ref
	case "NearestTableSubquery":
		ref := &ast.SubqueryRef{Subquery: tc.transformSelectInternalStatement(alt.child(1).sole().parens())}
		ref.SetSpan(alt.span())
		return ref
	case "NearestBaseTableRef":
		ref := &ast.BaseTableRef{}
		ref.SetSpan(alt.span())
		tc.fillBaseTableName(ref, alt.child(0))
		if at, ok := alt.child(1).opt(); ok {
			ref.At = tc.transformAtClause(at)
		}
		if sc, ok := alt.child(2).opt(); ok {
			ref.Sample = tc.transformSampleClause(sc)
		}
		return ref
	case "NearestParensTableRef":
		inner := tc.transformTableRef(alt.child(0).parens())
		if sc, ok := alt.child(1).opt(); ok {
			setRefSample(inner, tc.transformSampleClause(sc))
		}
		return inner
	}
	shapeError(alt, "unknown nearest bare table ref")
	return nil
}

// ---- PIVOT / UNPIVOT ---------------------------------------------------

// TablePivotClause <- 'PIVOT' Parens(TablePivotClauseBody) TableAlias?
func (tc *transformContext) transformTablePivot(left ast.TableRef, n tnode) ast.TableRef {
	body := n.child(1).parens()
	ref := &ast.PivotRef{Source: left}
	// the ref's location is the parenthesized pivot body
	ref.SetSpan(body.span())
	// TablePivotClauseBody <- TargetList 'FOR' PivotValueList+ PivotGroupByList?
	for _, e := range body.child(0).listElems() {
		ref.Aggregates = append(ref.Aggregates, tc.transformAliasedExpression(e))
	}
	for _, pv := range body.child(2).repeat() {
		ref.Pivots = append(ref.Pivots, tc.transformPivotValueList(pv, true))
	}
	if gb, ok := body.child(3).opt(); ok {
		ref.GroupBy = identifierList(gb.child(2))
	}
	if aliasNode, ok := n.child(2).opt(); ok {
		ref.Alias, ref.ColumnNameAlias = tc.transformTableAlias(aliasNode)
	}
	return ref
}

// PivotValueList <- PivotHeader 'IN' PivotValueTarget
// expandHeader: the table-ref FOR form expands a parenthesized header
// into its parts; the statement ON form keeps it whole.
func (tc *transformContext) transformPivotValueList(n tnode, expandHeader bool) ast.PivotColumn {
	var col ast.PivotColumn
	header := tc.transformBaseExprNode(n.child(0).sole())
	if expandHeader {
		col.PivotExpressions = rowParts(header)
	} else {
		col.PivotExpressions = []ast.Expr{header}
	}
	_, target := n.child(2).sole().choice()
	switch target.name() {
	case "PivotEnumTarget":
		col.PivotEnum = identifierText(target.sole())
	case "PivotListTarget":
		// PivotTargetList <- Parens(TargetList)
		for _, e := range target.sole().sole().parens().listElems() {
			col.Entries = append(col.Entries, tc.pivotEntry(e))
		}
	default:
		shapeError(target, "unknown pivot value target")
	}
	return col
}

// pivotEntry converts one aliased expression into a pivot IN-list entry:
// constants keep their value, a bare column name is stringified (PIVOT
// ... IN (c) pivots on the value "c"), anything else stays an expression.
func (tc *transformContext) pivotEntry(n tnode) ast.PivotColumnEntry {
	expr := tc.transformAliasedExpression(n)
	entry := ast.PivotColumnEntry{Alias: expr.GetAlias()}
	expr.SetAlias("")
	parts := rowParts(expr)
	values := make([]ast.Value, 0, len(parts))
	for _, part := range parts {
		switch e := part.(type) {
		case *ast.ConstantExpression:
			values = append(values, e.Value)
		case *ast.ColumnRefExpression:
			if len(e.ColumnNames) == 1 {
				values = append(values, ast.Value{Type: ast.LogicalType{ID: "VARCHAR"}, Kind: ast.ValueString, Str: e.ColumnNames[0]})
				continue
			}
		case *ast.CastExpression:
			// fold constant casts: integer casts of integer constants
			// (`1::INT`) and ISO dates (`'2022-01-01'::DATE`)
			if c, isConst := e.Child.(*ast.ConstantExpression); isConst && e.CastType != nil &&
				e.CastType.Schema == "" && len(e.CastType.Args) == 0 {
				if id, isInt := integerTypeNames[e.CastType.TypeName]; isInt && c.Value.Kind == ast.ValueInt64 {
					v := c.Value
					v.Type = ast.LogicalType{ID: id}
					values = append(values, v)
					continue
				}
				if e.CastType.TypeName == "DATE" && c.Value.Kind == ast.ValueString {
					if days, ok := isoDateToDays(c.Value.Str); ok {
						values = append(values, ast.Value{Type: ast.LogicalType{ID: "DATE"}, Kind: ast.ValueInt64, Int64: days})
						continue
					}
				}
			}
		}
	}
	if len(values) == len(parts) {
		entry.Values = values
	} else {
		// any unfoldable part keeps the whole entry as an expression
		entry.StarExpr = expr
	}
	return entry
}

// isoDateToDays parses a YYYY-MM-DD date into days since 1970-01-01.
func isoDateToDays(s string) (int64, bool) {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return 0, false
	}
	return int64(t.Unix() / 86400), true
}

// integerTypeNames maps unbound integer type spellings to logical type
// ids (used to fold constant casts in pivot IN entries).
var integerTypeNames = map[string]string{
	"INTEGER":  "INTEGER",
	"BIGINT":   "BIGINT",
	"SMALLINT": "SMALLINT",
	"TINYINT":  "TINYINT",
	"HUGEINT":  "HUGEINT",
}

// rowParts flattens a row(...) constructor into its parts (multi-column
// pivot IN entries), leaving other expressions intact.
func rowParts(e ast.Expr) []ast.Expr {
	if f, ok := e.(*ast.FunctionExpression); ok && f.FunctionName == "row" && f.Schema == "" && !f.IsOperator {
		parts := make([]ast.Expr, 0, len(f.Arguments))
		for _, a := range f.Arguments {
			parts = append(parts, a.Expr)
		}
		return parts
	}
	return []ast.Expr{e}
}

// unpivotEntry keeps the expression as star_expr (upstream's UNPIVOT
// entries are expressions, not values; the alias stays on both).
func (tc *transformContext) unpivotEntry(n tnode) ast.PivotColumnEntry {
	expr := tc.transformAliasedExpression(n)
	entry := ast.PivotColumnEntry{Alias: expr.GetAlias()}
	entry.StarExpr = expr
	return entry
}

// transformBaseExprNode transforms a BaseExpression rule node.
func (tc *transformContext) transformBaseExprNode(n tnode) ast.Expr {
	return tc.transformBase(n)
}

// TableUnpivotClause <- 'UNPIVOT' IncludeOrExcludeNulls? Parens(TableUnpivotClauseBody) TableAlias?
func (tc *transformContext) transformTableUnpivot(left ast.TableRef, n tnode) ast.TableRef {
	ref := &ast.PivotRef{Source: left}
	if ien, ok := n.child(1).opt(); ok {
		_, kind := ien.sole().choice()
		ref.IncludeNulls = kind.name() == "IncludeNulls"
	}
	body := n.child(2).parens()
	// upstream's unpivot ref location covers the clause body inside the
	// parentheses
	ref.SetSpan(body.span())
	// TableUnpivotClauseBody <- UnpivotHeader 'FOR' UnpivotValueList+
	ref.UnpivotNames = tc.transformUnpivotHeader(body.child(0))
	valueLists := body.child(2).repeat()
	if len(valueLists) > 1 {
		raise("UNPIVOT requires a single pivot element")
	}
	for _, uv := range valueLists {
		// UnpivotValueList <- UnpivotHeader 'IN' UnpivotTargetList
		var col ast.PivotColumn
		col.UnpivotNames = tc.transformUnpivotHeader(uv.child(0))
		if len(col.UnpivotNames) != 1 {
			raise("UNPIVOT requires a single column name for the PIVOT IN clause")
		}
		// UnpivotTargetList <- Parens(TargetList)
		for _, e := range uv.child(2).parens().listElems() {
			col.Entries = append(col.Entries, tc.unpivotEntry(e))
		}
		ref.Pivots = append(ref.Pivots, col)
	}
	if aliasNode, ok := n.child(3).opt(); ok {
		ref.Alias, ref.ColumnNameAlias = tc.transformTableAlias(aliasNode)
	}
	return ref
}

// UnpivotHeader <- UnpivotHeaderSingle / UnpivotHeaderList
func (tc *transformContext) transformUnpivotHeader(n tnode) []string {
	_, alt := n.sole().choice()
	switch alt.name() {
	case "UnpivotHeaderSingle":
		return []string{identifierText(alt.sole())}
	case "UnpivotHeaderList":
		return identifierList(alt.sole().parens())
	}
	shapeError(alt, "unknown unpivot header")
	return nil
}

// ---- PIVOT / UNPIVOT statement forms ----------------------------------

// PivotStatement <- PivotKeyword TableRef PivotOn? PivotUsing? PivotGroupByList?
func (tc *transformContext) transformPivotStatement(n tnode) ast.QueryNode {
	ref := &ast.PivotRef{Source: tc.transformTableRef(n.child(1))}
	ref.SetSpan(invalidSpan)
	if on, ok := n.child(2).opt(); ok {
		// PivotOn <- 'ON' PivotColumnList
		for _, e := range on.child(1).sole().listElems() {
			ref.Pivots = append(ref.Pivots, tc.transformPivotColumnEntry(e))
		}
		// port of TransformPivotColumnList's checks on the ON expressions
		for _, col := range ref.Pivots {
			for _, e := range col.PivotExpressions {
				if isScalarExpr(e) {
					raise("Cannot pivot on constant value \"%s\"", constantText(e))
				}
				if hasSubquery(e) {
					raise("Cannot pivot on subquery \"%s\"", constantText(e))
				}
			}
		}
		// pivot columns without an IN list or enum extract their values
		// from the data; upstream records them as pivot entries (enum
		// expansion), darkwing counts them for the CREATE VIEW/MACRO
		// check. Parameters seen before this point (the pivot source) mix
		// with data extraction and are rejected at the statement level;
		// parameters later in the statement (USING aggregates) are fine,
		// matching upstream's transform order.
		for _, col := range ref.Pivots {
			if col.PivotEnum == "" && len(col.Entries) == 0 && len(col.PivotExpressions) > 0 {
				tc.pivotEntries++
				if tc.paramCount > 0 || tc.hasNamed || tc.hasPosition {
					tc.pivotEntryHasParams = true
				}
			}
		}
	}
	if using, ok := n.child(3).opt(); ok {
		for _, e := range using.child(1).listElems() {
			ref.Aggregates = append(ref.Aggregates, tc.transformAliasedExpression(e))
		}
	}
	if gb, ok := n.child(4).opt(); ok {
		ref.GroupBy = identifierList(gb.child(2))
	}
	if len(ref.Pivots) > 0 && len(ref.Aggregates) == 0 {
		// pivots without USING default to count_star()
		ref.Aggregates = []ast.Expr{fnCall(invalidSpan, "count_star")}
	}
	if len(ref.Pivots) == 0 {
		// PIVOT without ON is a plain grouped aggregate:
		// SELECT <groups>, <aggregates> FROM source GROUP BY <groups>
		node := &ast.SelectNode{AggregateHandling: ast.StandardHandling}
		node.SetSpan(invalidSpan)
		node.FromTable = ref.Source
		for i, g := range ref.GroupBy {
			col := &ast.ColumnRefExpression{ColumnNames: []string{g}}
			col.SetSpan(invalidSpan)
			node.SelectList = append(node.SelectList, col)
			groupCol := &ast.ColumnRefExpression{ColumnNames: []string{g}}
			groupCol.SetSpan(invalidSpan)
			node.GroupExpressions = append(node.GroupExpressions, groupCol)
			_ = i
		}
		if len(node.GroupExpressions) > 0 {
			set := ast.GroupingSet{}
			for i := range node.GroupExpressions {
				set = append(set, i)
			}
			node.GroupSets = []ast.GroupingSet{set}
		}
		node.SelectList = append(node.SelectList, ref.Aggregates...)
		return node
	}
	return starSelect(ref, n.span())
}

// isScalarExpr ports ParsedExpression::IsScalar: true when the tree
// contains no column/positional references, defaults or subqueries
// (parameters count as scalar upstream).
func isScalarExpr(e ast.Expr) bool {
	switch e.(type) {
	case nil:
		return true
	case *ast.ColumnRefExpression, *ast.DefaultExpression,
		*ast.PositionalReferenceExpression, *ast.SubqueryExpression:
		return false
	}
	for _, child := range e.Children() {
		if expr, ok := child.(ast.Expr); ok && !isScalarExpr(expr) {
			return false
		}
	}
	return true
}

// constantText renders an expression for an error message, best-effort
// (upstream calls the full ToString renderer, which darkwing does not
// have; only constants are spelled out).
func constantText(e ast.Expr) string {
	c, ok := e.(*ast.ConstantExpression)
	if !ok {
		return "?"
	}
	switch {
	case c.Value.IsNull:
		return "NULL"
	case c.Value.Kind == ast.ValueString:
		return "'" + c.Value.Str + "'"
	case c.Value.Kind == ast.ValueInt64:
		return strconv.FormatInt(c.Value.Int64, 10)
	case c.Value.Kind == ast.ValueBool:
		if c.Value.Bool {
			return "true"
		}
		return "false"
	}
	return "?"
}

// PivotColumnEntry <- PivotColumnSubquery / PivotValueList / PivotColumnExpression
func (tc *transformContext) transformPivotColumnEntry(n tnode) ast.PivotColumn {
	_, alt := n.sole().choice()
	switch alt.name() {
	case "PivotColumnSubquery":
		// BaseExpression 'IN' Parens(SelectStatementInternal)
		return ast.PivotColumn{
			PivotExpressions: []ast.Expr{tc.transformBaseExprNode(alt.child(0))},
			Subquery:         tc.transformSelectInternalStatement(alt.child(2).parens()),
		}
	case "PivotValueList":
		return tc.transformPivotValueList(alt, false)
	case "PivotColumnExpression":
		return ast.PivotColumn{PivotExpressions: []ast.Expr{tc.transformExpression(alt.sole())}}
	}
	shapeError(alt, "unknown pivot column entry")
	return ast.PivotColumn{}
}

// UnpivotStatement <- UnpivotKeyword TableRef 'ON' TargetList IntoNameValues?
func (tc *transformContext) transformUnpivotStatement(n tnode) ast.QueryNode {
	ref := &ast.PivotRef{Source: tc.transformTableRef(n.child(1))}
	ref.SetSpan(invalidSpan)
	var col ast.PivotColumn
	for _, e := range n.child(3).listElems() {
		col.Entries = append(col.Entries, tc.unpivotEntry(e))
	}
	if into, ok := n.child(4).opt(); ok {
		// IntoNameValues <- 'INTO' 'NAME' ColIdOrString ValueOrValues List(Identifier)
		col.UnpivotNames = []string{identifierText(into.child(2))}
		ref.UnpivotNames = identifierList(into.child(4))
	} else {
		col.UnpivotNames = []string{"name"}
		ref.UnpivotNames = []string{"value"}
	}
	ref.Pivots = []ast.PivotColumn{col}
	return starSelect(ref, n.span())
}
