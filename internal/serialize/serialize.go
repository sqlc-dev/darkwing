// Package serialize renders darkwing's AST in json_serialize_sql-compatible
// JSON: the same maps, keys and enum spellings the pinned DuckDB binary
// emits for SELECT statements. The conformance suite diffs this output
// against oracle output structurally (parsed JSON, not bytes), so key
// order is irrelevant; values and shapes are not.
package serialize

import (
	"encoding/json"
	"fmt"
	"math"

	"github.com/sqlc-dev/darkwing/ast"
)

// invalidLocation is DuckDB's DConstants::INVALID_INDEX, serialized for
// nodes with no source location.
const invalidLocation = ^uint64(0)

// Script renders a statement list the way json_serialize_sql renders a
// script: {"error": false, "statements": [...]}. Only SELECT statements
// are supported, mirroring upstream's restriction.
func Script(stmts []ast.Stmt) ([]byte, error) {
	out := map[string]any{"error": false}
	list := make([]any, 0, len(stmts))
	for _, stmt := range stmts {
		sel, ok := stmt.(*ast.SelectStatement)
		if !ok {
			return nil, fmt.Errorf("serialize: only SELECT statements are supported, got %T", stmt)
		}
		list = append(list, selectStatement(sel))
	}
	out["statements"] = list
	return json.Marshal(out)
}

// SelectStatementJSON renders one statement wrapper ({"node": ...,
// "named_param_map": ...}).
func SelectStatementJSON(sel *ast.SelectStatement) ([]byte, error) {
	return json.Marshal(selectStatement(sel))
}

func selectStatement(sel *ast.SelectStatement) map[string]any {
	params := make([]any, 0, len(sel.NamedParams))
	for _, p := range sel.NamedParams {
		params = append(params, map[string]any{"key": p.Name, "value": p.Index})
	}
	return map[string]any{
		"node":            queryNode(sel.Node),
		"named_param_map": params,
	}
}

func location(sp ast.Span) (uint64, uint64) {
	if sp.Start < 0 {
		return invalidLocation, 0
	}
	length := sp.End - sp.Start
	if length < 0 {
		length = 0
	}
	return uint64(sp.Start), uint64(length)
}

func locate(m map[string]any, sp ast.Span) map[string]any {
	loc, length := location(sp)
	m["query_location"] = loc
	m["query_location_length"] = length
	return m
}

// ---- query nodes -------------------------------------------------------

func queryNode(n ast.QueryNode) map[string]any {
	if n == nil {
		return nil
	}
	base := map[string]any{
		"modifiers": modifiers(*n.ModifiersRef()),
		"cte_map":   cteMap(*n.CTEMapRef()),
	}
	switch node := n.(type) {
	case *ast.SelectNode:
		base["type"] = "SELECT_NODE"
		base["select_list"] = exprList(node.SelectList)
		base["from_table"] = tableRef(node.FromTable)
		base["where_clause"] = exprOrNil(node.Where)
		base["group_expressions"] = exprList(node.GroupExpressions)
		base["group_sets"] = groupSets(node.GroupSets)
		base["aggregate_handling"] = string(node.AggregateHandling)
		base["having"] = exprOrNil(node.Having)
		base["sample"] = sampleOptions(node.Sample)
		base["qualify"] = exprOrNil(node.Qualify)
	case *ast.SetOperationNode:
		children := make([]any, 0, len(node.Inputs))
		for _, c := range node.Inputs {
			children = append(children, queryNode(c))
		}
		base["type"] = "SET_OPERATION_NODE"
		base["setop_type"] = string(node.SetOpType)
		base["setop_all"] = node.SetOpAll
		base["left"] = nil
		base["right"] = nil
		base["children"] = children
	case *ast.RecursiveCTENode:
		base["type"] = "RECURSIVE_CTE_NODE"
		base["cte_name"] = node.CTEName
		base["union_all"] = node.UnionAll
		base["left"] = queryNode(node.Left)
		base["right"] = queryNode(node.Right)
		base["aliases"] = stringList(node.Aliases)
		base["key_targets"] = exprList(node.KeyTargets)
	case *ast.InsertQueryNode:
		ins := node.Insert
		base["type"] = "INSERT_QUERY_NODE"
		if ins.Query != nil {
			base["select_statement"] = selectStatement(ins.Query)
		} else {
			base["select_statement"] = nil
		}
		base["columns"] = stringList(ins.Columns)
		base["table"] = ins.Table
		base["schema"] = ins.Schema
		base["catalog"] = ins.Catalog
		base["returning_list"] = exprList(ins.Returning)
		base["on_conflict_info"] = onConflictInfo(ins.OnConflict)
		// upstream materializes the insert target as a table ref only
		// when an ON CONFLICT clause needs it
		if ins.OnConflict != nil {
			ref := &ast.BaseTableRef{
				CatalogName: ins.Catalog, SchemaName: ins.Schema, TableName: ins.Table,
			}
			ref.Alias = ins.TableAlias
			ref.SetSpan(ast.Span{Start: -1, End: -1})
			base["table_ref"] = tableRef(ref)
		} else {
			base["table_ref"] = nil
		}
		base["default_values"] = ins.DefaultValues
		order := ins.ColumnOrder
		if order == "" {
			order = ast.InsertByPosition
		}
		base["column_order"] = string(order)
	case *ast.UpdateQueryNode:
		up := node.Update
		base["type"] = "UPDATE_QUERY_NODE"
		base["table"] = tableRef(up.Table)
		base["from_table"] = tableRef(up.From)
		base["returning_list"] = exprList(up.Returning)
		base["set_info"] = updateSetInfo(up.SetInfo, up.Where)
		base["prioritize_table_when_binding"] = false
	case *ast.DeleteQueryNode:
		del := node.Delete
		base["type"] = "DELETE_QUERY_NODE"
		base["condition"] = exprOrNil(del.Where)
		base["table"] = tableRef(del.Table)
		using := make([]any, 0, len(del.Using))
		for _, u := range del.Using {
			using = append(using, tableRef(u))
		}
		base["using_clauses"] = using
		base["returning_list"] = exprList(del.Returning)
	default:
		panic(fmt.Sprintf("serialize: unknown query node %T", n))
	}
	return base
}

func groupSets(sets []ast.GroupingSet) []any {
	out := make([]any, 0, len(sets))
	for _, s := range sets {
		set := make([]any, 0, len(s))
		for _, idx := range s {
			set = append(set, idx)
		}
		out = append(out, set)
	}
	return out
}

func modifiers(mods []ast.ResultModifier) []any {
	out := make([]any, 0, len(mods))
	for _, m := range mods {
		switch mod := m.(type) {
		case *ast.DistinctModifier:
			out = append(out, map[string]any{
				"type":                "DISTINCT_MODIFIER",
				"distinct_on_targets": exprList(mod.DistinctOnTargets),
			})
		case *ast.OrderModifier:
			out = append(out, orderModifier(mod.Orders))
		case *ast.LimitModifier:
			out = append(out, map[string]any{
				"type":       "LIMIT_MODIFIER",
				"limit":      exprOrNil(mod.Limit),
				"offset":     exprOrNil(mod.Offset),
				"limit_type": string(mod.LimitType),
			})
		default:
			panic(fmt.Sprintf("serialize: unknown result modifier %T", m))
		}
	}
	return out
}

func orderModifier(orders []ast.OrderByNode) map[string]any {
	return map[string]any{"type": "ORDER_MODIFIER", "orders": orderList(orders)}
}

func orderList(orders []ast.OrderByNode) []any {
	out := make([]any, 0, len(orders))
	for _, o := range orders {
		out = append(out, map[string]any{
			"type":       string(o.Type),
			"null_order": string(o.NullOrder),
			"expression": expr(o.Expression),
		})
	}
	return out
}

// onConflictInfo renders INSERT's ON CONFLICT payload for
// INSERT_QUERY_NODE.
func onConflictInfo(info *ast.OnConflictInfo) map[string]any {
	if info == nil {
		return nil
	}
	return map[string]any{
		"action_type":     string(info.Action),
		"indexed_columns": stringList(info.IndexedColumns),
		"set_info":        updateSetInfo(info.SetInfo, nil),
		"condition":       exprOrNil(info.TargetWhere),
	}
}

// updateSetInfo renders an UpdateSetInfo; where is UPDATE's own WHERE
// clause, which upstream stores as the set-info condition.
func updateSetInfo(set *ast.UpdateSetInfo, where ast.Expr) map[string]any {
	if set == nil {
		return nil
	}
	cond := set.Condition
	if cond == nil {
		cond = where
	}
	// SET targets are single names: the transformer rejects qualified
	// targets like upstream
	cols := make([]any, 0, len(set.Columns))
	for _, c := range set.Columns {
		cols = append(cols, c[0])
	}
	return map[string]any{
		"condition":   exprOrNil(cond),
		"columns":     cols,
		"expressions": exprList(set.Expressions),
	}
}

func cteMap(m ast.CTEMap) map[string]any {
	entries := make([]any, 0, len(m.Entries))
	for _, e := range m.Entries {
		entries = append(entries, map[string]any{
			"key": e.Name,
			"value": map[string]any{
				"aliases":            stringList(e.CTE.Aliases),
				"materialized":       string(e.CTE.Materialized),
				"key_targets":        exprList(e.CTE.KeyTargets),
				"payload_aggregates": []any{},
				"query_node":         queryNode(e.CTE.Query),
			},
		})
	}
	return map[string]any{"map": entries}
}

// ---- table refs --------------------------------------------------------

func tableRef(r ast.TableRef) map[string]any {
	if r == nil {
		return nil
	}
	switch ref := r.(type) {
	case *ast.EmptyTableRef:
		return locate(map[string]any{
			"type": "EMPTY", "alias": ref.Alias, "sample": sampleOptions(ref.Sample),
		}, ref.Span)
	case *ast.BaseTableRef:
		path := []any{}
		if ref.CatalogName != "" {
			path = append(path, ref.CatalogName)
		}
		if ref.SchemaName != "" {
			path = append(path, ref.SchemaName)
		}
		path = append(path, ref.TableName)
		return locate(map[string]any{
			"type": "BASE_TABLE", "alias": ref.Alias, "sample": sampleOptions(ref.Sample),
			"schema_name": ref.SchemaName, "table_name": ref.TableName,
			"column_name_alias": stringList(ref.ColumnNameAlias),
			"catalog_name":      ref.CatalogName,
			"at_clause":         atClause(ref.At),
			"qualified_name":    map[string]any{"path": path},
		}, ref.Span)
	case *ast.JoinRef:
		m := locate(map[string]any{
			"type": "JOIN", "alias": ref.Alias, "sample": sampleOptions(ref.Sample),
			"left": tableRef(ref.Left), "right": tableRef(ref.Right),
			"condition": exprOrNil(ref.Condition),
			"join_type": string(ref.JoinType), "ref_type": string(ref.RefType),
			"using_columns":                stringList(ref.UsingColumns),
			"delim_flipped":                false,
			"duplicate_eliminated_columns": []any{},
			"is_implicit":                  ref.IsImplicit,
			"ranking_expression":           exprOrNil(ref.RankingExpression),
			"nearest_count":                ref.NearestCount,
			"nearest_order_type":           string(ref.NearestOrderType),
			"nearest_approx":               ref.NearestApprox,
		}, ref.Span)
		// upstream writes the join's column aliases only for aliased joins
		if ref.Alias != "" || len(ref.ColumnNameAlias) > 0 {
			m["column_name_alias"] = stringList(ref.ColumnNameAlias)
		}
		return m
	case *ast.SubqueryRef:
		return locate(map[string]any{
			"type": "SUBQUERY", "alias": ref.Alias, "sample": sampleOptions(ref.Sample),
			"subquery":          selectStatement(ref.Subquery),
			"column_name_alias": stringList(ref.ColumnNameAlias),
		}, ref.Span)
	case *ast.TableFunctionRef:
		ordinality := "WITHOUT_ORDINALITY"
		if ref.WithOrdinality {
			ordinality = "WITH_ORDINALITY"
		}
		return locate(map[string]any{
			"type": "TABLE_FUNCTION", "alias": ref.Alias, "sample": sampleOptions(ref.Sample),
			"function":          expr(ref.Function),
			"column_name_alias": stringList(ref.ColumnNameAlias),
			"with_ordinality":   ordinality,
		}, ref.Span)
	case *ast.ExpressionListRef:
		values := make([]any, 0, len(ref.Values))
		for _, row := range ref.Values {
			values = append(values, exprList(row))
		}
		return locate(map[string]any{
			"type": "EXPRESSION_LIST", "alias": ref.Alias, "sample": sampleOptions(ref.Sample),
			"expected_names": stringList(ref.ExpectedNames),
			"expected_types": []any{},
			"values":         values,
		}, ref.Span)
	case *ast.PivotRef:
		pivots := make([]any, 0, len(ref.Pivots))
		for _, p := range ref.Pivots {
			pivots = append(pivots, pivotColumn(p))
		}
		return locate(map[string]any{
			"type": "PIVOT", "alias": ref.Alias, "sample": sampleOptions(ref.Sample),
			"source":            tableRef(ref.Source),
			"aggregates":        exprList(ref.Aggregates),
			"unpivot_names":     stringList(ref.UnpivotNames),
			"pivots":            pivots,
			"groups":            stringList(ref.GroupBy),
			"column_name_alias": stringList(ref.ColumnNameAlias),
			"include_nulls":     ref.IncludeNulls,
		}, ref.Span)
	case *ast.ShowRef:
		return locate(map[string]any{
			"type": "SHOW_REF", "alias": ref.Alias, "sample": sampleOptions(ref.Sample),
			"table_name":   ref.TableName,
			"query":        queryNode(ref.Query),
			"show_type":    string(ref.ShowType),
			"catalog_name": ref.CatalogName,
			"schema_name":  ref.SchemaName,
		}, ref.Span)
	}
	panic(fmt.Sprintf("serialize: unknown table ref %T", r))
}

// qualifiedColumnName renders a dotted column path the way upstream
// serializes QualifiedColumnName: all four fields, empty when absent.
func qualifiedColumnName(q ast.QualifiedColumnName) map[string]any {
	parts := q.Path
	out := map[string]any{"catalog": "", "schema": "", "table": "", "column": ""}
	fields := []string{"column", "table", "schema", "catalog"}
	for i := 0; i < len(parts) && i < 4; i++ {
		out[fields[i]] = parts[len(parts)-1-i]
	}
	return out
}

// blobText renders raw blob bytes the way DuckDB prints them: printable
// ASCII stays literal (except backslash), everything else is \xHH.
func blobText(raw string) string {
	var sb []byte
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if c >= 32 && c <= 126 && c != '\\' {
			sb = append(sb, c)
			continue
		}
		const hex = "0123456789ABCDEF"
		sb = append(sb, '\\', 'x', hex[c>>4], hex[c&0xf])
	}
	return string(sb)
}

// Fingerprint renders an expression's serialized form with locations
// stripped — the transformer uses it for GROUP BY deduplication.
func Fingerprint(e ast.Expr) string {
	m := expr(e)
	stripLocations(m)
	b, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return string(b)
}

func stripLocations(v any) {
	switch t := v.(type) {
	case map[string]any:
		delete(t, "query_location")
		delete(t, "query_location_length")
		for _, child := range t {
			stripLocations(child)
		}
	case []any:
		for _, child := range t {
			stripLocations(child)
		}
	}
}

func pivotColumn(p ast.PivotColumn) map[string]any {
	entries := make([]any, 0, len(p.Entries))
	for _, e := range p.Entries {
		values := make([]any, 0, len(e.Values))
		for _, v := range e.Values {
			values = append(values, value(v))
		}
		entries = append(entries, map[string]any{
			"values":    values,
			"star_expr": exprOrNil(e.StarExpr),
			"alias":     e.Alias,
		})
	}
	out := map[string]any{
		"pivot_expressions": exprList(p.PivotExpressions),
		"unpivot_names":     stringList(p.UnpivotNames),
		"entries":           entries,
		"pivot_enum":        p.PivotEnum,
	}
	if p.Subquery != nil {
		out["subquery"] = selectStatement(p.Subquery)
	}
	return out
}

func atClause(a *ast.AtClause) any {
	if a == nil {
		return nil
	}
	return map[string]any{"unit": string(a.Unit), "expr": expr(a.Expr)}
}

func sampleOptions(s *ast.SampleOptions) any {
	if s == nil {
		return nil
	}
	return map[string]any{
		"sample_size":   value(s.SampleSize),
		"is_percentage": s.IsPercentage,
		"method":        string(s.Method),
		"seed":          s.Seed,
		"repeatable":    s.Repeatable,
		"sample_rate":   -1.0,
	}
}

// ---- expressions -------------------------------------------------------

func exprList(list []ast.Expr) []any {
	out := make([]any, 0, len(list))
	for _, e := range list {
		out = append(out, expr(e))
	}
	return out
}

func exprOrNil(e ast.Expr) any {
	if e == nil {
		return nil
	}
	return expr(e)
}

func stringList(list []string) []any {
	out := make([]any, 0, len(list))
	for _, s := range list {
		out = append(out, s)
	}
	return out
}

func functionArguments(args []ast.FunctionArgument) []any {
	out := make([]any, 0, len(args))
	for _, a := range args {
		out = append(out, map[string]any{"name": a.Name, "expression": expr(a.Expr)})
	}
	return out
}

func expr(e ast.Expr) map[string]any {
	if e == nil {
		return nil
	}
	base := map[string]any{"alias": e.GetAlias()}
	switch x := e.(type) {
	case *ast.ColumnRefExpression:
		base["class"] = "COLUMN_REF"
		base["type"] = "COLUMN_REF"
		base["column_names"] = stringList(x.ColumnNames)
		locate(base, x.Span)
	case *ast.ConstantExpression:
		base["class"] = "CONSTANT"
		base["type"] = "VALUE_CONSTANT"
		base["value"] = value(x.Value)
		locate(base, x.Span)
	case *ast.ParameterExpression:
		base["class"] = "PARAMETER"
		base["type"] = "VALUE_PARAMETER"
		base["identifier"] = x.Identifier()
		locate(base, x.Span)
	case *ast.FunctionExpression:
		base["class"] = "FUNCTION"
		base["type"] = "FUNCTION"
		base["function_name"] = x.FunctionName
		base["schema"] = x.Schema
		base["catalog"] = x.Catalog
		base["filter"] = exprOrNil(x.Filter)
		base["order_bys"] = orderModifier(x.OrderBys)
		base["distinct"] = x.Distinct
		base["is_operator"] = x.IsOperator
		base["export_state"] = x.ExportState
		base["arguments"] = functionArguments(x.Arguments)
		locate(base, x.Span)
	case *ast.OperatorExpression:
		base["class"] = "OPERATOR"
		base["type"] = string(x.Type)
		base["children"] = exprList(x.Operands)
		locate(base, x.Span)
	case *ast.ComparisonExpression:
		base["class"] = "COMPARISON"
		base["type"] = string(x.Type)
		base["left"] = expr(x.Left)
		base["right"] = expr(x.Right)
		locate(base, x.Span)
	case *ast.ConjunctionExpression:
		base["class"] = "CONJUNCTION"
		base["type"] = string(x.Type)
		base["children"] = exprList(x.Operands)
		locate(base, x.Span)
	case *ast.CastExpression:
		base["class"] = "CAST"
		base["type"] = "OPERATOR_CAST"
		base["child"] = expr(x.Child)
		base["cast_type"] = castType(x)
		base["try_cast"] = x.TryCast
		locate(base, x.Span)
	case *ast.CaseExpression:
		checks := make([]any, 0, len(x.CaseChecks))
		for _, c := range x.CaseChecks {
			checks = append(checks, map[string]any{
				"when_expr": expr(c.When),
				"then_expr": expr(c.Then),
			})
		}
		base["class"] = "CASE"
		base["type"] = "CASE_EXPR"
		base["case_checks"] = checks
		base["else_expr"] = exprOrNil(x.ElseExpr)
		locate(base, x.Span)
	case *ast.SubqueryExpression:
		base["class"] = "SUBQUERY"
		base["type"] = "SUBQUERY"
		base["subquery_type"] = string(x.SubqueryType)
		base["subquery"] = selectStatement(x.Subquery)
		base["child"] = exprOrNil(x.Child)
		base["comparison_type"] = string(x.ComparisonType)
		locate(base, x.Span)
	case *ast.StarExpression:
		excl := make([]any, 0, len(x.QualifiedExcludeList))
		for _, q := range x.QualifiedExcludeList {
			excl = append(excl, qualifiedColumnName(q))
		}
		repl := make([]any, 0, len(x.ReplaceList))
		for _, r := range x.ReplaceList {
			repl = append(repl, map[string]any{"key": r.Key, "value": expr(r.Expr)})
		}
		ren := make([]any, 0, len(x.RenameList))
		for _, r := range x.RenameList {
			ren = append(ren, map[string]any{
				"key":   qualifiedColumnName(r.Key),
				"value": r.Name,
			})
		}
		base["class"] = "STAR"
		base["type"] = "STAR"
		base["relation_name"] = x.RelationName
		base["exclude_list"] = stringList(x.ExcludeList)
		base["qualified_exclude_list"] = excl
		base["replace_list"] = repl
		base["rename_list"] = ren
		base["columns"] = x.Columns
		base["expr"] = exprOrNil(x.Expr)
		locate(base, x.Span)
	case *ast.LambdaExpression:
		base["class"] = "LAMBDA"
		base["type"] = "LAMBDA"
		base["lhs"] = expr(x.LHS)
		base["expr"] = expr(x.Expr)
		base["syntax_type"] = string(x.SyntaxType)
		locate(base, x.Span)
	case *ast.BetweenExpression:
		base["class"] = "BETWEEN"
		base["type"] = "COMPARE_BETWEEN"
		base["input"] = expr(x.Input)
		base["lower"] = expr(x.Lower)
		base["upper"] = expr(x.Upper)
		locate(base, x.Span)
	case *ast.CollateExpression:
		base["class"] = "COLLATE"
		base["type"] = "COLLATE"
		base["child"] = expr(x.Child)
		base["collation"] = x.Collation
		locate(base, x.Span)
	case *ast.PositionalReferenceExpression:
		base["class"] = "POSITIONAL_REFERENCE"
		base["type"] = "POSITIONAL_REFERENCE"
		base["index"] = x.Index
		locate(base, x.Span)
	case *ast.DefaultExpression:
		base["class"] = "DEFAULT"
		base["type"] = "VALUE_DEFAULT"
		locate(base, x.Span)
	case *ast.TypeExpression:
		base["class"] = "TYPE"
		base["type"] = "TYPE"
		base["catalog"] = x.Catalog
		base["schema"] = x.Schema
		base["type_name"] = x.TypeName
		base["children"] = exprList(x.Args)
		locate(base, x.Span)
	case *ast.WindowExpression:
		base["class"] = "WINDOW"
		base["type"] = string(x.Type)
		base["function_name"] = x.FunctionName
		base["schema"] = x.Schema
		base["catalog"] = x.Catalog
		base["partitions"] = exprList(x.Partitions)
		base["orders"] = orderList(x.Orders)
		base["start"] = string(x.FrameStart)
		base["end"] = string(x.FrameEnd)
		base["start_expr"] = exprOrNil(x.StartExpr)
		base["end_expr"] = exprOrNil(x.EndExpr)
		base["offset_expr"] = exprOrNil(x.OffsetExpr)
		base["default_expr"] = exprOrNil(x.DefaultExpr)
		base["ignore_nulls"] = x.IgnoreNulls
		base["has_ignore_nulls"] = x.HasIgnoreNulls
		base["filter_expr"] = exprOrNil(x.FilterExpr)
		base["exclude_clause"] = string(x.ExcludeClause)
		base["distinct"] = x.Distinct
		base["arg_orders"] = orderList(x.ArgOrders)
		base["arguments"] = functionArguments(x.Arguments)
		locate(base, x.Span)
	default:
		panic(fmt.Sprintf("serialize: unknown expression %T", e))
	}
	return base
}

// castType renders a CastExpression's target: a resolved logical type id
// or an UNBOUND type wrapping the type expression.
func castType(x *ast.CastExpression) map[string]any {
	if x.ResolvedType != "" {
		return map[string]any{"id": x.ResolvedType, "type_info": nil}
	}
	return map[string]any{
		"id": "UNBOUND",
		"type_info": map[string]any{
			"type":           "UNBOUND_TYPE_INFO",
			"alias":          "",
			"extension_info": nil,
			"expr":           expr(x.CastType),
		},
	}
}

// ---- values ------------------------------------------------------------

func logicalType(t ast.LogicalType) map[string]any {
	out := map[string]any{"id": t.ID}
	switch t.ID {
	case "DECIMAL":
		out["type_info"] = map[string]any{
			"type":           "DECIMAL_TYPE_INFO",
			"alias":          "",
			"extension_info": nil,
			"width":          t.Width,
			"scale":          t.Scale,
		}
	case "LIST":
		var child map[string]any
		if t.Child != nil {
			child = logicalType(*t.Child)
		}
		out["type_info"] = map[string]any{
			"type":           "LIST_TYPE_INFO",
			"alias":          "",
			"extension_info": nil,
			"child_type":     child,
		}
	default:
		out["type_info"] = nil
	}
	return out
}

func value(v ast.Value) map[string]any {
	out := map[string]any{
		"type":    logicalType(v.Type),
		"is_null": v.IsNull,
	}
	if v.IsNull {
		return out
	}
	switch v.Kind {
	case ast.ValueBool:
		out["value"] = v.Bool
	case ast.ValueInt64:
		out["value"] = v.Int64
	case ast.ValueHugeint:
		if v.Type.ID == "UHUGEINT" {
			out["value"] = map[string]any{"upper": uint64(v.Hugeint.Upper), "lower": v.Hugeint.Lower}
		} else {
			out["value"] = map[string]any{"upper": v.Hugeint.Upper, "lower": v.Hugeint.Lower}
		}
	case ast.ValueDouble:
		// upstream prints non-finite doubles as bare Infinity/-Infinity/NaN
		// tokens (invalid JSON, replicated verbatim)
		switch {
		case math.IsInf(v.Float64, 1):
			out["value"] = json.RawMessage("Infinity")
		case math.IsInf(v.Float64, -1):
			out["value"] = json.RawMessage("-Infinity")
		case math.IsNaN(v.Float64):
			out["value"] = json.RawMessage("NaN")
		default:
			out["value"] = v.Float64
		}
	case ast.ValueString:
		if v.Type.ID == "BLOB" {
			out["value"] = blobText(v.Str)
		} else {
			out["value"] = v.Str
		}
	case ast.ValueEmptyList:
		out["value"] = map[string]any{"children": []any{}}
	case ast.ValueNull:
		// no payload
	}
	return out
}
