// transform_select.go: the SELECT query tree — query nodes, set
// operations, CTEs, result modifiers, GROUP BY and samples. Port of
// upstream's transform_select_node.cpp / transform_setop.cpp /
// transform_cte.cpp / transform_groupby.cpp / transform_sample.cpp.
package parser

import (
	"sort"
	"strconv"
	"strings"

	"github.com/sqlc-dev/darkwing/ast"
	"github.com/sqlc-dev/darkwing/internal/serialize"
)

func init() {
	register("SelectStatement", func(tc *transformContext, n tnode) ast.Stmt {
		return tc.transformSelectInternalStatement(n.sole())
	})
}

// invalidSpan marks synthesized nodes with no source location; the
// serializer renders it as DuckDB's INVALID_INDEX.
var invalidSpan = ast.Span{Start: -1, End: -1}

// transformSelectInternalStatement wraps a SelectStatementInternal node
// into a SelectStatement.
func (tc *transformContext) transformSelectInternalStatement(n tnode) *ast.SelectStatement {
	stmt := &ast.SelectStatement{Node: tc.transformSelectInternal(n)}
	stmt.SetSpan(n.span())
	return stmt
}

// SelectStatementInternal <- WithClause? SelectSetOpChain ResultModifiers?
func (tc *transformContext) transformSelectInternal(n tnode) ast.QueryNode {
	var ctes ast.CTEMap
	if wc, ok := n.child(0).opt(); ok {
		ctes = tc.transformWithClause(wc)
	}
	node := tc.transformSetOpChain(n.child(1))
	if rm, ok := n.child(2).opt(); ok {
		tc.applyResultModifiers(node, rm)
	}
	if len(ctes.Entries) > 0 {
		cteRef := node.CTEMapRef()
		cteRef.Entries = append(ctes.Entries, cteRef.Entries...)
	}
	return node
}

// ---- WITH clauses ------------------------------------------------------

// WithClause <- 'WITH' Recursive? List(WithStatement)
func (tc *transformContext) transformWithClause(n tnode) ast.CTEMap {
	_, recursive := n.child(1).opt()
	var out ast.CTEMap
	for _, ws := range n.child(2).listElems() {
		name, cte := tc.transformWithStatement(ws, recursive)
		out.Entries = append(out.Entries, ast.CTEMapEntry{Name: name, CTE: cte})
	}
	return out
}

// WithStatement <- ColIdOrString InsertColumnList? UsingKey? 'AS' Materialized? CTEBody
func (tc *transformContext) transformWithStatement(n tnode, recursive bool) (string, *ast.CommonTableExpr) {
	name := identifierText(n.child(0))
	cte := &ast.CommonTableExpr{Materialized: ast.CTEMaterializeDefault}
	cte.SetSpan(n.span())
	if cols, ok := n.child(1).opt(); ok {
		cte.Aliases = identifierList(cols.sole().parens())
	}
	if uk, ok := n.child(2).opt(); ok {
		// UsingKey <- 'USING' 'KEY' Parens(TargetList)
		for _, e := range uk.child(2).parens().listElems() {
			cte.KeyTargets = append(cte.KeyTargets, tc.transformAliasedExpression(e))
		}
	}
	if mat, ok := n.child(4).opt(); ok {
		// Materialized <- 'NOT'? 'MATERIALIZED'
		if _, not := mat.child(0).opt(); not {
			cte.Materialized = ast.CTEMaterializeNever
		} else {
			cte.Materialized = ast.CTEMaterializeAlways
		}
	}
	// CTEBody <- CTESelectBody / CTEDMLBody
	_, body := n.child(5).sole().choice()
	switch body.name() {
	case "CTESelectBody":
		cte.Query = tc.transformSelectInternal(body.sole().parens())
	case "CTEDMLBody":
		stmt := tc.transformStatement(body.sole().parens())
		sel, ok := stmt.(*ast.SelectStatement)
		if !ok {
			raise("can only use SELECT statements (or DML in milestone 5) inside a CTE")
		}
		cte.Query = sel.Node
	default:
		shapeError(body, "unknown CTE body")
	}
	if recursive {
		if setop, ok := cte.Query.(*ast.SetOperationNode); ok && setop.SetOpType == ast.SetOpUnion && len(setop.Inputs) == 2 {
			rec := &ast.RecursiveCTENode{
				CTEName:    name,
				UnionAll:   setop.SetOpAll,
				Left:       setop.Inputs[0],
				Right:      setop.Inputs[1],
				Aliases:    cte.Aliases,
				KeyTargets: cte.KeyTargets,
			}
			rec.Modifiers = setop.Modifiers
			rec.CTEs = setop.CTEs
			rec.SetSpan(ast.Span{Start: setop.Pos(), End: setop.End()})
			cte.Query = rec
		}
	}
	return name, cte
}

// ---- set operation chains ---------------------------------------------

// SelectSetOpChain <- IntersectChain SelectSetOpChainTail*
func (tc *transformContext) transformSetOpChain(n tnode) ast.QueryNode {
	node := tc.transformIntersectChain(n.child(0))
	for _, tail := range n.child(1).repeat() {
		// SelectSetOpChainTail <- SetopClause IntersectChain
		clause := tail.child(0)
		right := tc.transformIntersectChain(tail.child(1))
		node = tc.buildSetOp(node, right, clause)
	}
	return node
}

// SetopClause <- SetopType DistinctOrAll? ByName?
func (tc *transformContext) buildSetOp(left, right ast.QueryNode, clause tnode) ast.QueryNode {
	_, kind := clause.child(0).sole().choice()
	setopType := ast.SetOpUnion
	if kind.name() == "SetopExcept" {
		setopType = ast.SetOpExcept
	}
	all := false
	if da, ok := clause.child(1).opt(); ok {
		_, d := da.sole().choice()
		all = d.name() == "AllKeyword"
	}
	if _, byName := clause.child(2).opt(); byName {
		if setopType != ast.SetOpUnion {
			raise("BY NAME is only supported for UNION")
		}
		setopType = ast.SetOpUnionByName
	}
	// same-kind UNION chains flatten into one n-ary node (never EXCEPT
	// or INTERSECT, and never across modifiers)
	if setopType == ast.SetOpUnion || setopType == ast.SetOpUnionByName {
		if ls, ok := left.(*ast.SetOperationNode); ok && ls.SetOpType == setopType &&
			ls.SetOpAll == all && len(ls.Modifiers) == 0 && len(ls.CTEs.Entries) == 0 {
			ls.Inputs = append(ls.Inputs, right)
			ls.SetSpan(ast.Span{Start: ls.Pos(), End: right.End()})
			return ls
		}
	}
	node := &ast.SetOperationNode{SetOpType: setopType, SetOpAll: all, Inputs: []ast.QueryNode{left, right}}
	node.SetSpan(ast.Span{Start: left.Pos(), End: right.End()})
	return node
}

// IntersectChain <- SelectAtom IntersectChainTail*
func (tc *transformContext) transformIntersectChain(n tnode) ast.QueryNode {
	node := tc.transformSelectAtom(n.child(0))
	for _, tail := range n.child(1).repeat() {
		// IntersectChainTail <- SetIntersectClause SelectAtom
		clause := tail.child(0) // 'INTERSECT' DistinctOrAll?
		right := tc.transformSelectAtom(tail.child(1))
		all := false
		if da, ok := clause.child(1).opt(); ok {
			_, d := da.sole().choice()
			all = d.name() == "AllKeyword"
		}
		setop := &ast.SetOperationNode{SetOpType: ast.SetOpIntersect, SetOpAll: all, Inputs: []ast.QueryNode{node, right}}
		setop.SetSpan(ast.Span{Start: node.Pos(), End: right.End()})
		node = setop
	}
	return node
}

// SelectAtom <- SelectParens / SelectStatementType
func (tc *transformContext) transformSelectAtom(n tnode) ast.QueryNode {
	_, alt := n.sole().choice()
	switch alt.name() {
	case "SelectParens":
		return tc.transformSelectInternal(alt.sole().parens())
	case "SelectStatementType":
		return tc.transformSelectStatementType(alt)
	}
	shapeError(alt, "unknown select atom")
	return nil
}

// SelectStatementType <- OptionalParensSimpleSelect / ValuesClause /
// DescribeStatement / TableStatement / PivotStatement / UnpivotStatement
func (tc *transformContext) transformSelectStatementType(n tnode) ast.QueryNode {
	_, alt := n.sole().choice()
	switch alt.name() {
	case "OptionalParensSimpleSelect":
		_, inner := alt.sole().choice()
		if inner.name() == "SimpleSelectParens" {
			return tc.transformSimpleSelect(inner.sole().parens())
		}
		return tc.transformSimpleSelect(inner)
	case "ValuesClause":
		ref := tc.transformValuesClause(alt)
		ref.Alias = "valueslist"
		return starSelect(ref, alt.span())
	case "DescribeStatement":
		return tc.transformDescribe(alt)
	case "TableStatement":
		// 'TABLE' BaseTableName — synthesized SELECT * FROM t, no locations
		ref := &ast.BaseTableRef{}
		tc.fillBaseTableName(ref, alt.child(1))
		ref.SetSpan(invalidSpan)
		return starSelect(ref, invalidSpan)
	case "PivotStatement":
		return tc.transformPivotStatement(alt)
	case "UnpivotStatement":
		return tc.transformUnpivotStatement(alt)
	}
	shapeError(alt, "unknown select statement type")
	return nil
}

// starSelect builds SELECT * FROM ref — the desugaring for VALUES,
// TABLE, PIVOT and SHOW forms.
func starSelect(ref ast.TableRef, sp ast.Span) *ast.SelectNode {
	star := &ast.StarExpression{}
	star.SetSpan(invalidSpan)
	node := &ast.SelectNode{
		SelectList:        []ast.Expr{star},
		FromTable:         ref,
		AggregateHandling: ast.StandardHandling,
	}
	node.SetSpan(sp)
	return node
}

// SimpleSelect <- SelectFrom WhereClause? GroupByClause? HavingClause?
// WindowClause? QualifyClause? SampleClause?
func (tc *transformContext) transformSimpleSelect(n tnode) ast.QueryNode {
	node := &ast.SelectNode{AggregateHandling: ast.StandardHandling}
	node.SetSpan(n.span())
	tc.pushWindowScope()
	defer tc.popWindowScope()

	// named windows resolve while transforming the select list, so the
	// WINDOW clause (child 4) is processed first
	if wc, ok := n.child(4).opt(); ok {
		// WindowClause <- 'WINDOW' List(WindowDefinition)
		for _, def := range wc.child(1).listElems() {
			// WindowDefinition <- Identifier 'AS' WindowFrameDefinition
			w := &ast.WindowExpression{
				FrameStart: ast.WindowUnboundedPreceding, FrameEnd: ast.WindowCurrentRowRange,
				ExcludeClause: ast.WindowExcludeNoOther,
			}
			tc.transformWindowFrameDefinition(w, def.child(2))
			tc.defineWindow(identifierText(def.child(0)), w)
		}
	}

	// SelectFrom <- SelectFromClause / FromSelectClause
	_, sf := n.child(0).sole().choice()
	var selectClause, fromClause tnode
	var hasSelect, hasFrom bool
	switch sf.name() {
	case "SelectFromClause":
		selectClause, hasSelect = sf.child(0), true
		fromClause, hasFrom = sf.child(1).opt()
	case "FromSelectClause":
		fromClause, hasFrom = sf.child(0), true
		selectClause, hasSelect = sf.child(1).opt()
	default:
		shapeError(sf, "unknown select-from")
	}

	if hasFrom {
		node.FromTable = tc.transformFromClause(fromClause)
	} else {
		empty := &ast.EmptyTableRef{}
		empty.SetSpan(invalidSpan)
		node.FromTable = empty
	}

	if hasSelect {
		// SelectClause <- 'SELECT' DistinctClause? TargetList?
		if dc, ok := selectClause.child(1).opt(); ok {
			tc.applyDistinct(node, dc)
		}
		if tl, ok := selectClause.child(2).opt(); ok {
			for _, e := range tl.listElems() {
				node.SelectList = append(node.SelectList, tc.transformAliasedExpression(e))
			}
		}
	} else {
		star := &ast.StarExpression{}
		star.SetSpan(invalidSpan)
		node.SelectList = []ast.Expr{star}
	}

	if wc, ok := n.child(1).opt(); ok {
		node.Where = tc.transformExpression(wc.child(1))
	}
	if gb, ok := n.child(2).opt(); ok {
		tc.transformGroupBy(node, gb)
	}
	if hv, ok := n.child(3).opt(); ok {
		node.Having = tc.transformExpression(hv.child(1))
	}
	if qc, ok := n.child(5).opt(); ok {
		node.Qualify = tc.transformExpression(qc.child(1))
	}
	if sc, ok := n.child(6).opt(); ok {
		node.Sample = tc.transformSampleClause(sc)
	}
	return node
}

// applyDistinct appends the DISTINCT modifier.
// DistinctClause <- DistinctOn / DistinctAll
func (tc *transformContext) applyDistinct(node *ast.SelectNode, n tnode) {
	_, alt := n.sole().choice()
	if alt.name() != "DistinctOn" {
		return // ALL is the default
	}
	mod := &ast.DistinctModifier{}
	mod.SetSpan(alt.span())
	if targets, ok := alt.child(1).opt(); ok {
		mod.DistinctOnTargets = tc.transformExpressionList(targets.child(1).parens())
	}
	node.Modifiers = append(node.Modifiers, mod)
}

// AliasedExpression <- ColIdExpression / ExpressionAsCollabel / ExpressionOptIdentifier
func (tc *transformContext) transformAliasedExpression(n tnode) ast.Expr {
	_, alt := n.sole().choice()
	switch alt.name() {
	case "ColIdExpression":
		// prefix alias: `alias: expr`
		expr := tc.transformExpression(alt.child(2))
		expr.SetAlias(identifierText(alt.child(0)))
		return expr
	case "ExpressionAsCollabel":
		expr := tc.transformExpression(alt.child(0))
		expr.SetAlias(identifierText(alt.child(2)))
		return expr
	case "ExpressionOptIdentifier":
		expr := tc.transformExpression(alt.child(0))
		if id, ok := alt.child(1).opt(); ok {
			expr.SetAlias(identifierText(id))
		}
		return expr
	case "Expression":
		// ExpressionAlias (expression statements) reuses this dispatch
		return tc.transformExpression(alt)
	}
	shapeError(alt, "unknown aliased expression")
	return nil
}

// ---- GROUP BY ----------------------------------------------------------

// GroupByClause <- 'GROUP' 'BY' GroupByExpressions
func (tc *transformContext) transformGroupBy(node *ast.SelectNode, n tnode) {
	_, alt := n.child(2).sole().choice()
	if alt.name() == "GroupByAll" {
		node.AggregateHandling = ast.ForceAggregates
		return
	}
	// GROUP BY * is another spelling of GROUP BY ALL
	if elems := alt.sole().listElems(); len(elems) == 1 {
		if star, ok := tc.peekBareStar(elems[0]); ok && star {
			node.AggregateHandling = ast.ForceAggregates
			return
		}
	}
	// GroupByList <- List(GroupByExpression); each entry contributes a
	// list of grouping sets, combined as a cross product (upstream's
	// transform_group_by.cpp).
	sets := []ast.GroupingSet{{}}
	for _, entry := range alt.sole().listElems() {
		entrySets := tc.transformGroupByExpressionEntry(node, entry)
		var combined []ast.GroupingSet
		for _, base := range sets {
			for _, es := range entrySets {
				combined = append(combined, mergeSet(base, es))
			}
		}
		sets = combined
	}
	node.GroupSets = sets
}

func mergeSet(a, b ast.GroupingSet) ast.GroupingSet {
	return normalizeSet(append(append(ast.GroupingSet{}, a...), b...))
}

// normalizeSet sorts and deduplicates a grouping set (upstream stores
// std::set<idx_t>).
func normalizeSet(s ast.GroupingSet) ast.GroupingSet {
	sort.Ints(s)
	out := ast.GroupingSet{}
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}

// peekBareStar reports whether a GroupByExpression entry is a bare `*`.
func (tc *transformContext) peekBareStar(n tnode) (bool, bool) {
	_, alt := n.sole().choice()
	if alt.name() != "GroupByBaseExpression" {
		return false, false
	}
	expr := tc.transformExpression(alt.sole())
	star, ok := expr.(*ast.StarExpression)
	if !ok || star.Columns || star.Expr != nil || star.RelationName != "" ||
		len(star.ExcludeList) != 0 || len(star.ReplaceList) != 0 ||
		len(star.QualifiedExcludeList) != 0 || len(star.RenameList) != 0 {
		return false, false
	}
	return true, true
}

// groupByUnit registers one GROUP BY element: a parenthesized list
// (parsed as a row constructor) expands into its parts, all in one
// grouping set.
func (tc *transformContext) groupByUnit(node *ast.SelectNode, expr ast.Expr) ast.GroupingSet {
	if row, ok := expr.(*ast.FunctionExpression); ok && row.FunctionName == "row" &&
		row.Schema == "" && !row.IsOperator && len(row.Arguments) > 0 {
		set := ast.GroupingSet{}
		for _, a := range row.Arguments {
			set = append(set, addGroupExpression(node, a.Expr))
		}
		return normalizeSet(set)
	}
	return ast.GroupingSet{addGroupExpression(node, expr)}
}

// addGroupExpression appends expr to the group list, returning its
// index; a structurally identical expression reuses its earlier index
// (upstream deduplicates group expressions).
func addGroupExpression(node *ast.SelectNode, expr ast.Expr) int {
	fp := serialize.Fingerprint(expr)
	for i, e := range node.GroupExpressions {
		if serialize.Fingerprint(e) == fp {
			return i
		}
	}
	node.GroupExpressions = append(node.GroupExpressions, expr)
	return len(node.GroupExpressions) - 1
}

// GroupByExpression <- EmptyGroupingItem / CubeOrRollupClause /
// GroupingSetsClause / GroupByBaseExpression
func (tc *transformContext) transformGroupByExpressionEntry(node *ast.SelectNode, n tnode) []ast.GroupingSet {
	_, alt := n.sole().choice()
	switch alt.name() {
	case "EmptyGroupingItem":
		return []ast.GroupingSet{{}}
	case "GroupByBaseExpression":
		return []ast.GroupingSet{tc.groupByUnit(node, tc.transformExpression(alt.sole()))}
	case "CubeOrRollupClause":
		// CubeOrRollup Parens(List(Expression)?): each element is one
		// unit (a parenthesized list counts as a single unit)
		_, kind := alt.child(0).sole().choice()
		var units []ast.GroupingSet
		if list, ok := alt.child(1).parens().opt(); ok {
			for _, e := range list.listElems() {
				units = append(units, tc.groupByUnit(node, tc.transformExpression(e)))
			}
		}
		if kind.name() == "CubeKeyword" {
			return normalizeSets(cubeSets(units))
		}
		return normalizeSets(rollupSets(units))
	case "GroupingSetsClause":
		// 'GROUPING' 'SETS' Parens(List(GroupByExpression))
		var out []ast.GroupingSet
		for _, e := range alt.child(2).parens().listElems() {
			out = append(out, tc.transformGroupByExpressionEntry(node, e)...)
		}
		return out
	}
	shapeError(alt, "unknown group-by expression")
	return nil
}

func normalizeSets(sets []ast.GroupingSet) []ast.GroupingSet {
	for i := range sets {
		sets[i] = normalizeSet(sets[i])
	}
	return sets
}

// rollupSets returns increasing prefixes — (), (0), (0,1) — matching
// upstream's serialization order. Units are unioned in order.
func rollupSets(units []ast.GroupingSet) []ast.GroupingSet {
	out := []ast.GroupingSet{{}}
	prefix := ast.GroupingSet{}
	for _, u := range units {
		prefix = append(prefix, u...)
		out = append(out, append(ast.GroupingSet{}, prefix...))
	}
	return out
}

// cubeSets returns all unit subsets in upstream's DFS pre-order: (),
// (0), (0,1), (1) for two units.
func cubeSets(units []ast.GroupingSet) []ast.GroupingSet {
	var out []ast.GroupingSet
	var walk func(prefix ast.GroupingSet, from int)
	walk = func(prefix ast.GroupingSet, from int) {
		out = append(out, append(ast.GroupingSet{}, prefix...))
		for i := from; i < len(units); i++ {
			walk(append(prefix, units[i]...), i+1)
		}
	}
	walk(ast.GroupingSet{}, 0)
	return out
}

// ---- result modifiers --------------------------------------------------

// ResultModifiers <- OrderByClause? LimitOffset?
func (tc *transformContext) applyResultModifiers(node ast.QueryNode, n tnode) {
	mods := node.ModifiersRef()
	if ob, ok := n.child(0).opt(); ok {
		om := &ast.OrderModifier{Orders: tc.transformOrderByClause(ob)}
		om.SetSpan(ob.span())
		*mods = append(*mods, om)
	}
	if lo, ok := n.child(1).opt(); ok {
		*mods = append(*mods, tc.transformLimitOffset(lo)...)
	}
}

// transformOrderByClause handles OrderByClause ('ORDER' 'BY'
// OrderByExpressions).
func (tc *transformContext) transformOrderByClause(n tnode) []ast.OrderByNode {
	_, alt := n.child(2).sole().choice()
	switch alt.name() {
	case "OrderByAll":
		// ORDER BY ALL DescOrAsc? NullsFirstOrLast? — upstream represents
		// ALL as a synthesized COLUMNS(*) star
		star := &ast.StarExpression{Columns: true}
		star.SetSpan(invalidSpan)
		return []ast.OrderByNode{{
			Type:       orderTypeOf(alt.child(1)),
			NullOrder:  nullOrderOf(alt.child(2)),
			Expression: star,
		}}
	case "OrderByExpressionList":
		var out []ast.OrderByNode
		for _, e := range alt.sole().listElems() {
			// OrderByExpression <- Expression DescOrAsc? NullsFirstOrLast?
			out = append(out, ast.OrderByNode{
				Type:       orderTypeOf(e.child(1)),
				NullOrder:  nullOrderOf(e.child(2)),
				Expression: tc.transformExpression(e.child(0)),
			})
		}
		return out
	}
	shapeError(alt, "unknown order-by expressions")
	return nil
}

func orderTypeOf(n tnode) ast.OrderType {
	da, ok := n.opt()
	if !ok {
		return ast.OrderDefault
	}
	_, alt := da.sole().choice()
	if alt.name() == "DescendingOrder" {
		return ast.OrderDescending
	}
	return ast.OrderAscending
}

func nullOrderOf(n tnode) ast.NullOrder {
	nf, ok := n.opt()
	if !ok {
		return ast.NullOrderDefault
	}
	_, alt := nf.sole().choice()
	if alt.name() == "NullsFirst" {
		return ast.NullsFirst
	}
	return ast.NullsLast
}

// LimitOffset <- LimitOffsetClause / OffsetFetchClause / OffsetLimitClause / FetchOnlyClause
func (tc *transformContext) transformLimitOffset(n tnode) []ast.ResultModifier {
	_, alt := n.sole().choice()
	mod := &ast.LimitModifier{LimitType: ast.LimitRowCount}
	mod.SetSpan(alt.span())
	switch alt.name() {
	case "LimitOffsetClause":
		tc.applyLimitClause(mod, alt.child(0))
		if oc, ok := alt.child(1).opt(); ok {
			mod.Offset = tc.transformOffsetValue(oc)
		}
	case "OffsetLimitClause":
		mod.Offset = tc.transformOffsetValue(alt.child(0))
		if lc, ok := alt.child(1).opt(); ok {
			tc.applyLimitClause(mod, lc)
		}
	case "OffsetFetchClause":
		mod.Offset = tc.transformOffsetValue(alt.child(0))
		mod.Limit = tc.transformFetchValue(alt.child(1))
	case "FetchOnlyClause":
		mod.Limit = tc.transformFetchValue(alt.sole())
	default:
		shapeError(alt, "unknown limit/offset")
	}
	return []ast.ResultModifier{mod}
}

// applyLimitClause fills limit/limit-type from a LimitClause
// ('LIMIT' LimitValue).
func (tc *transformContext) applyLimitClause(mod *ast.LimitModifier, n tnode) {
	_, alt := n.child(1).sole().choice()
	switch alt.name() {
	case "LimitAll":
		// LIMIT ALL is an explicit NULL limit
		mod.Limit = constExpr(invalidSpan, ast.Value{Type: ast.LogicalType{ID: "NULL"}, IsNull: true, Kind: ast.ValueNull})
	case "LimitLiteralPercent":
		mod.Limit = numberConstant(alt.child(0).span(), alt.child(0).number())
		mod.LimitType = ast.LimitPercentage
	case "LimitExpression":
		// Expression '%'?
		mod.Limit = tc.transformExpression(alt.child(0))
		if _, pct := alt.child(1).opt(); pct {
			mod.LimitType = ast.LimitPercentage
		}
	default:
		shapeError(alt, "unknown limit value")
	}
}

// OffsetValue <- Expression RowOrRows?
func (tc *transformContext) transformOffsetValue(n tnode) ast.Expr {
	return tc.transformExpression(n.child(1).child(0))
}

// FetchClause <- 'FETCH' FirstOrNext FetchValue RowOrRows 'ONLY'
func (tc *transformContext) transformFetchValue(n tnode) ast.Expr {
	return tc.transformExpression(n.child(2).sole())
}

// ---- SHOW / DESCRIBE / SUMMARIZE --------------------------------------

// specialShowNames are the unqualified SHOW/DESCRIBE targets that stay
// symbolic (SHOW TABLES, SHOW DATABASES, ...) instead of describing a
// relation.
var specialShowNames = map[string]bool{
	"tables":    true,
	"databases": true,
	"schemas":   true,
	"variables": true,
}

// DescribeStatement <- ShowTables / ShowSelect / ShowAllTables / ShowQualifiedName
// All SHOW/DESCRIBE desugaring uses synthesized (location-free) nodes,
// mirroring upstream.
func (tc *transformContext) transformDescribe(n tnode) ast.QueryNode {
	_, alt := n.sole().choice()
	ref := &ast.ShowRef{ShowType: ast.ShowTypeDescribe}
	ref.SetSpan(invalidSpan)
	switch alt.name() {
	case "ShowSelect":
		if isSummarize(alt.child(0)) {
			ref.ShowType = ast.ShowTypeSummary
		}
		ref.Query = tc.transformSelectInternal(alt.child(1))
	case "ShowAllTables":
		ref.ShowType = ast.ShowTypeShowUnqualified
		ref.TableName = "__show_tables_expanded"
	case "ShowQualifiedName":
		if isSummarize(alt.child(0)) {
			ref.ShowType = ast.ShowTypeSummary
		}
		target, ok := alt.child(1).opt()
		if !ok {
			shapeError(alt, "SHOW without a target")
		}
		_, t := target.sole().choice()
		switch t.name() {
		case "DescribeBaseTableName":
			btr := &ast.BaseTableRef{}
			btr.SetSpan(invalidSpan)
			tc.fillBaseTableName(btr, t.sole())
			if btr.CatalogName == "" && btr.SchemaName == "" &&
				ref.ShowType == ast.ShowTypeDescribe && specialShowNames[strings.ToLower(btr.TableName)] {
				ref.ShowType = ast.ShowTypeShowUnqualified
				ref.TableName = "\"" + strings.ToLower(btr.TableName) + "\""
				break
			}
			// DESCRIBE/SUMMARIZE t wraps the relation as SELECT * FROM t
			ref.Query = starSelect(btr, invalidSpan)
		case "DescribeStringLiteral":
			// DESCRIBE 'file.csv' wraps SELECT * FROM 'file.csv'
			s, _ := t.sole().stringLit()
			btr := &ast.BaseTableRef{TableName: s}
			btr.SetSpan(invalidSpan)
			ref.Query = starSelect(btr, invalidSpan)
		}
	case "ShowTables":
		// 'SHOW' 'TABLES' 'FROM' QualifiedName
		ref.ShowType = ast.ShowTypeShowFrom
		cat, schema, name := tc.transformQualifiedNameParts(alt.child(3))
		if cat == "" && schema == "" {
			schema = name
		} else if cat == "" {
			cat, schema = schema, name
		}
		ref.CatalogName, ref.SchemaName = cat, schema
	default:
		shapeError(alt, "unknown describe statement")
	}
	return starSelect(ref, invalidSpan)
}

// isSummarize reports whether a ShowOrDescribeOrSummarize (or
// ShowOrDescribe) node spelled SUMMARIZE.
func isSummarize(n tnode) bool {
	_, alt := n.sole().choice()
	return alt.name() == "Summarize"
}

// ---- samples -----------------------------------------------------------

// SampleClause <- (TableSample / UsingSample) SampleEntry
func (tc *transformContext) transformSampleClause(n tnode) *ast.SampleOptions {
	s := &ast.SampleOptions{Seed: -1}
	s.SetSpan(n.span())
	// SampleEntry <- SampleEntryFunction / SampleEntryCount
	_, entry := n.child(1).sole().choice()
	switch entry.name() {
	case "SampleEntryCount":
		// SampleCount Parens(SampleProperties)?
		tc.applySampleCount(s, entry.child(0), "")
		if props, ok := entry.child(1).opt(); ok {
			// SampleProperties <- ColId (',' SampleSeed)?
			inner := props.parens()
			s.Method = formatSampleMethod(identifierText(inner.child(0)))
			if seedPart, ok := inner.child(1).opt(); ok {
				s.Seed = parseSeed(seedPart.child(1).sole().number())
				s.Repeatable = true
			}
		}
	case "SampleEntryFunction":
		// SampleFunction? Parens(SampleCount) RepeatableSample?
		method := ""
		if fn, ok := entry.child(0).opt(); ok {
			method = identifierText(fn.sole())
		}
		tc.applySampleCount(s, entry.child(1).parens(), method)
		if rep, ok := entry.child(2).opt(); ok {
			// RepeatableSample <- 'REPEATABLE' Parens(SampleSeed)
			s.Seed = parseSeed(rep.child(1).parens().sole().number())
			s.Repeatable = true
		}
	default:
		shapeError(entry, "unknown sample entry")
	}
	return s
}

func parseSeed(text string) int64 {
	v, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		raise("invalid sample seed")
	}
	return v
}

// applySampleCount fills size/percentage/method from a SampleCount node.
// SampleCount <- SampleValue SampleUnit?
func (tc *transformContext) applySampleCount(s *ast.SampleOptions, n tnode, method string) {
	value := n.child(0)
	isPercent := false
	hasUnit := false
	if unit, ok := n.child(1).opt(); ok {
		hasUnit = true
		_, u := unit.sole().choice()
		isPercent = u.name() == "SamplePercentage"
	}
	_ = hasUnit // a bare count is a row count
	s.IsPercentage = isPercent
	_, v := value.sole().choice()
	if v.name() != "NumberLiteral" {
		raise("parameters are not supported in SAMPLE clauses yet")
	}
	text := v.number()
	if isPercent {
		f, err := strconv.ParseFloat(text, 64)
		if err != nil {
			raise("invalid sample size")
		}
		s.SampleSize = ast.Value{Type: ast.LogicalType{ID: "DOUBLE"}, Kind: ast.ValueDouble, Float64: f}
		if method == "" {
			method = "system"
		}
	} else {
		iv, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			raise("invalid sample size")
		}
		s.SampleSize = ast.Value{Type: ast.LogicalType{ID: "BIGINT"}, Kind: ast.ValueInt64, Int64: iv}
		if method == "" {
			method = "reservoir"
		}
	}
	s.Method = formatSampleMethod(method)
}

// formatSampleMethod capitalizes the method name the way DuckDB
// serializes it ("System", "Reservoir", "Bernoulli").
func formatSampleMethod(m string) ast.SampleMethod {
	if m == "" {
		return ""
	}
	lower := strings.ToLower(m)
	return ast.SampleMethod(strings.ToUpper(lower[:1]) + lower[1:])
}

// ExpressionStatement <- List(ExpressionAlias): a bare expression list is
// a SELECT without a FROM clause.
func init() {
	register("ExpressionStatement", func(tc *transformContext, n tnode) ast.Stmt {
		node := &ast.SelectNode{AggregateHandling: ast.StandardHandling}
		node.SetSpan(n.span())
		empty := &ast.EmptyTableRef{}
		empty.SetSpan(invalidSpan)
		node.FromTable = empty
		for _, e := range n.sole().listElems() {
			node.SelectList = append(node.SelectList, tc.transformAliasedExpression(e))
		}
		// a single bare column reference is a table query (`foo` is
		// upstream's SELECT * FROM foo)
		if len(node.SelectList) == 1 {
			if ref, ok := node.SelectList[0].(*ast.ColumnRefExpression); ok && ref.GetAlias() == "" && len(ref.ColumnNames) <= 3 {
				table := &ast.BaseTableRef{}
				table.SetSpan(invalidSpan)
				names := ref.ColumnNames
				table.TableName = names[len(names)-1]
				if len(names) >= 2 {
					table.SchemaName = names[len(names)-2]
				}
				if len(names) == 3 {
					table.CatalogName = names[0]
				}
				sel := starSelect(table, n.span())
				stmt := &ast.SelectStatement{Node: sel}
				stmt.SetSpan(n.span())
				return stmt
			}
		}
		stmt := &ast.SelectStatement{Node: node}
		stmt.SetSpan(n.span())
		return stmt
	})
}

// normalizeKeyword upper-cases a keyword spelling for comparisons.
func normalizeKeyword(s string) string { return strings.ToUpper(s) }
