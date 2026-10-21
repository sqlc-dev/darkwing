// transform_func.go: function calls, window functions, the special
// function forms (EXTRACT, TRIM, SUBSTRING, ...), list/struct/map
// constructors, comprehensions and interval literals. Port of upstream's
// transform_function.cpp / transform_interval.cpp / transform_lambda.cpp.
package parser

import (
	"strconv"
	"strings"

	"github.com/sqlc-dev/darkwing/ast"
)

// typeCastFunctions maps type-named single-argument calls to the
// resolved logical type they cast to.
var typeCastFunctions = map[string]string{
	"time":      "TIME",
	"timestamp": "TIMESTAMP",
	"interval":  "INTERVAL",
}

// windowTypes maps unqualified function names to specialized window
// expression types.
var windowTypes = map[string]ast.ExpressionType{
	"row_number":   "WINDOW_ROW_NUMBER",
	"rank":         "WINDOW_RANK",
	"rank_dense":   "WINDOW_RANK_DENSE",
	"dense_rank":   "WINDOW_RANK_DENSE",
	"percent_rank": "WINDOW_PERCENT_RANK",
	"cume_dist":    "WINDOW_CUME_DIST",
	"ntile":        "WINDOW_NTILE",
	"lead":         "WINDOW_LEAD",
	"lag":          "WINDOW_LAG",
	"first_value":  "WINDOW_FIRST_VALUE",
	"first":        "WINDOW_FIRST_VALUE",
	"last_value":   "WINDOW_LAST_VALUE",
	"last":         "WINDOW_LAST_VALUE",
	"nth_value":    "WINDOW_NTH_VALUE",
	"fill":         "WINDOW_FILL",
}

// functionParts is the pre-window shape of a call.
type functionParts struct {
	distinct    bool
	args        []ast.FunctionArgument
	orderBys    []ast.OrderByNode
	ignoreNulls bool
	hasNullsOpt bool
}

// FunctionExpression <- FunctionIdentifier FunctionExpressionArguments
// WithinGroupClause? FilterClause? ExportClause? OverClause?
func (tc *transformContext) transformFunction(n tnode) ast.Expr {
	qn, rawName := tc.transformFunctionIdentifier(n.child(0))
	catalog, schema, name := qn.catalog, qn.schema, strings.ToLower(rawName)
	parts := tc.transformFunctionArgumentList(n.child(1).sole().parens())
	wg, hasWithinGroup := n.child(2).opt()
	if hasWithinGroup {
		// WITHIN GROUP (ORDER BY ...) replaces the function's ORDER BY
		if parts.distinct {
			raise("DISTINCT is not allowed in combination with WITHIN GROUP")
		}
		if len(parts.orderBys) > 0 {
			raise("Cannot use multiple ORDER BY statements with WITHIN GROUP")
		}
		parts.orderBys = tc.transformOrderByClause(wg.child(2).parens())
		if len(parts.orderBys) != 1 {
			raise("Cannot use multiple ORDER BY clauses with WITHIN GROUP")
		}
	}
	starCallRewrite(&parts)
	var filter ast.Expr
	if fc, ok := n.child(3).opt(); ok {
		// FilterClause <- 'FILTER' Parens('WHERE'? Expression)
		contents := fc.child(1).sole().parens()
		filter = tc.transformExpression(contents.child(1))
	}
	_, exportState := n.child(4).opt()

	if over, ok := n.child(5).opt(); ok {
		if exportState {
			raise("EXPORT_STATE is not supported for window functions!")
		}
		return tc.transformOver(over, qn, name, parts, filter, n.span())
	}
	// zero-argument count() is count_star(), for non-window calls only
	if name == "count" && len(parts.args) == 0 && schema == "" && catalog == "" {
		name = "count_star"
	}
	if out := parserFunctionRewrite(n.span(), name, parts.args, false); out != nil {
		return out
	}
	// single-argument calls named after a type are casts (TIME('...'))
	if id, isType := typeCastFunctions[name]; isType && schema == "" && catalog == "" &&
		len(parts.args) == 1 && parts.args[0].Name == "" && !parts.distinct && filter == nil {
		cast := &ast.CastExpression{Child: parts.args[0].Expr, ResolvedType: id}
		cast.SetSpan(n.span())
		return cast
	}
	if parts.hasNullsOpt {
		raise("RESPECT/IGNORE NULLS is not supported for non-window functions")
	}
	if hasWithinGroup {
		// only the ordered-set aggregates are accepted (renamed to their
		// quantile implementations)
		switch name {
		case "percentile_cont":
			if len(parts.args) != 1 {
				raise("Wrong number of arguments for PERCENTILE_CONT")
			}
			name = "quantile_cont"
		case "percentile_disc":
			if len(parts.args) != 1 {
				raise("Wrong number of arguments for PERCENTILE_DISC")
			}
			name = "quantile_disc"
		case "mode":
			if len(parts.args) != 0 {
				raise("Wrong number of arguments for MODE")
			}
		default:
			raise("Unknown ordered aggregate %s.", quoteIdent(rawName))
		}
	}
	f := &ast.FunctionExpression{
		Catalog:       catalog,
		Schema:        schema,
		Qualification: qn.path,
		FunctionName:  name,
		Arguments:     parts.args,
		Distinct:      parts.distinct,
		ExportState:   exportState,
		Filter:        filter,
		OrderBys:      parts.orderBys,
	}
	f.SetSpan(n.span())
	return f
}

// quoteIdent renders an identifier the way upstream formats an Identifier
// into an exception message (SQLQuotedIdentifier: always double-quoted,
// embedded quotes doubled).
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// parserFunctionRewrite ports TransformParserFunctionRewrite: the names
// the parser turns into a syntax node instead of a catalog function call.
// Both the plain call and the method chain (x.f(y)) go through it;
// COALESCE has a grammar rule of its own, so only the method chain
// rewrites it. It returns nil for an ordinary function.
func parserFunctionRewrite(sp ast.Span, name string, args []ast.FunctionArgument, methodChain bool) ast.Expr {
	rejectNamed := func(context string) {
		for _, a := range args {
			if a.Name != "" {
				raise("Named arguments are not supported in %s", context)
			}
		}
	}
	children := func() []ast.Expr {
		out := make([]ast.Expr, len(args))
		for i, a := range args {
			out[i] = a.Expr
		}
		return out
	}
	switch name {
	case "unpack":
		if len(args) != 1 {
			raise("Wrong number of arguments to the UNPACK operator")
		}
		rejectNamed("UNPACK operator")
		return opExpr(sp, ast.OperatorUnpack, children()...)
	case "try":
		if len(args) != 1 {
			raise("Wrong number of arguments provided to TRY expression")
		}
		rejectNamed("TRY expression")
		return opExpr(sp, ast.OperatorTry, children()...)
	case "construct_array":
		rejectNamed("array constructors")
		return opExpr(sp, ast.ArrayConstructor, children()...)
	case "ifnull":
		if len(args) != 2 {
			raise("Wrong number of arguments to IFNULL.")
		}
		rejectNamed("IFNULL expressions")
		return opExpr(sp, ast.OperatorCoalesce, children()...)
	case "coalesce":
		if !methodChain {
			return nil
		}
		rejectNamed("COALESCE expressions")
		return opExpr(sp, ast.OperatorCoalesce, children()...)
	case "date":
		if len(args) != 1 {
			raise("Wrong number of arguments provided to DATE function")
		}
		cast := &ast.CastExpression{Child: args[0].Expr, ResolvedType: "DATE"}
		cast.SetSpan(sp)
		return cast
	}
	return nil
}

// qualifiedName is a parsed function/type/table qualification: catalog
// and schema per QualifiedName::Catalog()/Schema(), and the full
// qualification path when it is deeper than catalog.schema.
type qualifiedName struct {
	catalog, schema string
	path            []string // set only for nested (> 2 component) qualifications
}

// newQualifiedName ports the QualifiedName(vector<Identifier>
// qualification, name) constructor's accessors: with one qualifier it is
// the schema; with two or more the first is the catalog and the last the
// schema.
func newQualifiedName(quals []string) qualifiedName {
	switch len(quals) {
	case 0:
		return qualifiedName{}
	case 1:
		return qualifiedName{schema: quals[0]}
	case 2:
		return qualifiedName{catalog: quals[0], schema: quals[1]}
	}
	return qualifiedName{catalog: quals[0], schema: quals[len(quals)-1], path: append([]string{}, quals...)}
}

// FunctionIdentifier <- CatalogReservedSchemaFunctionName /
// SchemaReservedFunctionName / FunctionNameAsQualifiedName
// The name is returned as written (callers lowercase it).
func (tc *transformContext) transformFunctionIdentifier(n tnode) (qualifiedName, string) {
	_, alt := n.sole().choice()
	var quals []string
	var name string
	switch alt.name() {
	case "CatalogReservedSchemaFunctionName":
		// CatalogQualification ReservedSchemaQualification* ReservedFunctionName
		quals = append(quals, identifierText(alt.child(0).child(0)))
		for _, sq := range alt.child(1).repeat() {
			quals = append(quals, identifierText(sq.child(0)))
		}
		name = identifierText(alt.child(2))
	case "SchemaReservedFunctionName":
		quals = append(quals, identifierText(alt.child(0).child(0)))
		name = identifierText(alt.child(1))
	case "FunctionNameAsQualifiedName":
		name = identifierText(alt.sole())
	default:
		shapeError(alt, "unknown function identifier")
	}
	return newQualifiedName(quals), name
}

// transformFunctionArgumentList handles FunctionExpressionArgumentList
// and MethodExpressionArgumentList (same shape):
// DistinctOrAll? FunctionArgumentList? OrderByClause? IgnoreOrRespectNulls?
func (tc *transformContext) transformFunctionArgumentList(n tnode) functionParts {
	var parts functionParts
	list, hasArgs := n.child(1).opt()
	if hasArgs {
		for _, argNode := range list.sole().listElems() {
			parts.args = append(parts.args, tc.transformFunctionArgument(argNode))
		}
	}
	if da, ok := n.child(0).opt(); ok {
		_, kind := da.sole().choice()
		parts.distinct = kind.name() == "DistinctKeyword"
		if !hasArgs && n.name() == "FunctionExpressionArgumentList" {
			// TransformFunctionExpressionArgumentList: f(DISTINCT) / f(ALL)
			// (the method-call list has no such check)
			if parts.distinct {
				raise("DISTINCT requires at least one argument")
			}
			raise("ALL requires at least one argument")
		}
	}
	if ob, ok := n.child(2).opt(); ok {
		parts.orderBys = tc.transformOrderByClause(ob)
	}
	if irn, ok := n.child(3).opt(); ok {
		parts.hasNullsOpt = true
		_, kind := irn.sole().choice()
		parts.ignoreNulls = kind.name() == "IgnoreNulls"
	}
	return parts
}

// starCallRewrite ports upstream's bare-star call handling: a single
// unmodified, unqualified `*` argument of a non-DISTINCT call without
// ORDER BY is dropped (COUNT(*) is COUNT(); COUNT(t.*) is not).
func starCallRewrite(parts *functionParts) {
	if parts.distinct || len(parts.orderBys) > 0 || len(parts.args) != 1 || parts.args[0].Name != "" {
		return
	}
	star, ok := parts.args[0].Expr.(*ast.StarExpression)
	if !ok || !isEmptyStar(star) {
		return
	}
	parts.args = nil
}

// isEmptyStar ports ExpressionIsUnqualifiedStar: a bare `*` — no
// relation, no COLUMNS, no EXCLUDE/REPLACE/RENAME.
func isEmptyStar(star *ast.StarExpression) bool {
	return !star.Columns && star.RelationName == "" && star.Expr == nil &&
		len(star.ExcludeList) == 0 && len(star.QualifiedExcludeList) == 0 &&
		len(star.ReplaceList) == 0 && len(star.RenameList) == 0
}

// transformFunctionArgumentsInto is the method-call variant used by
// `expr.f(...)`: it builds a bare FunctionExpression from an argument
// list node.
func (tc *transformContext) transformFunctionArgumentsInto(argList tnode, name string) *ast.FunctionExpression {
	parts := tc.transformFunctionArgumentList(argList)
	if len(parts.args) == 1 && parts.args[0].Name == "" {
		// TransformMethodExpression: x.count(*) is x.count()
		if star, ok := parts.args[0].Expr.(*ast.StarExpression); ok && isEmptyStar(star) {
			parts.args = nil
		}
	}
	if parts.hasNullsOpt {
		raise("RESPECT/IGNORE NULLS is not supported for non-window functions")
	}
	return &ast.FunctionExpression{
		FunctionName: strings.ToLower(name),
		Arguments:    parts.args,
		Distinct:     parts.distinct,
		OrderBys:     parts.orderBys,
	}
}

// FunctionArgument <- NamedFunctionArgument / PositionalFunctionArgument
func (tc *transformContext) transformFunctionArgument(n tnode) ast.FunctionArgument {
	_, alt := n.sole().choice()
	switch alt.name() {
	case "NamedFunctionArgument":
		// NamedFunctionArgument <- TypeFuncName NamedParameterAssignment Expression
		name := identifierText(alt.child(0))
		expr := tc.transformExpression(alt.child(2))
		// upstream also stamps the argument name as the expression alias
		expr.SetAlias(name)
		return ast.FunctionArgument{Name: name, Expr: expr}
	case "PositionalFunctionArgument":
		return ast.FunctionArgument{Expr: tc.transformExpression(alt.sole())}
	}
	shapeError(alt, "unknown function argument")
	return ast.FunctionArgument{}
}

// ---- window functions --------------------------------------------------

// transformOver builds the WindowExpression for a call with an OVER
// clause.
func (tc *transformContext) transformOver(n tnode, qn qualifiedName, name string, parts functionParts, filter ast.Expr, sp ast.Span) ast.Expr {
	if tc.inWindowDefinition {
		raise("window functions are not allowed in window definitions")
	}
	catalog, schema := qn.catalog, qn.schema
	w := &ast.WindowExpression{
		Catalog:        catalog,
		Schema:         schema,
		Qualification:  qn.path,
		FunctionName:   name,
		Arguments:      parts.args,
		Distinct:       parts.distinct,
		IgnoreNulls:    parts.ignoreNulls,
		HasIgnoreNulls: parts.hasNullsOpt,
		FilterExpr:     filter,
		ArgOrders:      parts.orderBys,
		FrameStart:     ast.WindowUnboundedPreceding,
		FrameEnd:       ast.WindowCurrentRowRange,
		ExcludeClause:  ast.WindowExcludeNoOther,
	}
	w.SetSpan(sp)
	if t, ok := windowTypes[name]; ok && schema == "" && catalog == "" {
		w.Type = t
		// the short spellings serialize under their canonical names
		switch name {
		case "first":
			w.FunctionName = "first_value"
		case "last":
			w.FunctionName = "last_value"
		}
	} else {
		w.Type = "WINDOW_AGGREGATE"
	}
	// WindowFrame <- ParensIdentifier / WindowFrameDefinition / IdentifierWindowFrame
	_, frame := n.child(1).sole().choice()
	switch frame.name() {
	case "ParensIdentifier":
		name := identifierText(frame.sole().parens())
		tc.applyNamedWindow(w, name)
		if windowHasFrameClause(w) {
			raise("cannot copy window \"%s\" because it has a frame clause", name)
		}
	case "IdentifierWindowFrame":
		tc.applyNamedWindow(w, identifierText(frame.sole()))
	case "WindowFrameDefinition":
		tc.inWindowDefinition = true
		tc.transformWindowFrameDefinition(w, frame)
		tc.inWindowDefinition = false
	default:
		shapeError(frame, "unknown window frame")
	}
	return w
}

// applyNamedWindow copies a named WINDOW definition into w.
func (tc *transformContext) applyNamedWindow(w *ast.WindowExpression, name string) {
	base := tc.namedWindow(name)
	if base == nil {
		raise("window %s does not exist", quoteIdent(name))
	}
	copyWindowSpec(w, base)
}

func copyWindowSpec(dst, src *ast.WindowExpression) {
	dst.Partitions = append([]ast.Expr{}, src.Partitions...)
	dst.Orders = append([]ast.OrderByNode{}, src.Orders...)
	dst.FrameStart = src.FrameStart
	dst.FrameEnd = src.FrameEnd
	dst.StartExpr = src.StartExpr
	dst.EndExpr = src.EndExpr
	dst.ExcludeClause = src.ExcludeClause
}

// transformWindowFrameDefinition fills w from a WindowFrameDefinition
// node (used both for OVER (...) and WINDOW name AS (...)).
func (tc *transformContext) transformWindowFrameDefinition(w *ast.WindowExpression, n tnode) {
	_, alt := n.sole().choice()
	var contents tnode
	var baseName tnode
	hasBase := false
	switch alt.name() {
	case "WindowFrameNameContentsParens":
		inner := alt.sole().parens()
		// WindowFrameNameContents <- BaseWindowName? WindowFrameContents
		baseName, hasBase = inner.child(0).opt()
		contents = inner.child(1)
	case "WindowFrameContentsParens":
		contents = alt.sole().parens()
	default:
		shapeError(alt, "unknown window frame definition")
	}
	if !hasBase {
		tc.fillWindowContents(w, contents)
		return
	}
	// OVER (base ...) extends a named window; upstream forbids copying a
	// window with a frame clause and overriding its ORDER/PARTITION BY
	name := identifierText(baseName)
	switch strings.ToLower(name) {
	case "partition", "range", "rows", "groups":
		raise("Invalid window name \"%s\"", name)
	}
	tc.applyNamedWindow(w, name)
	if windowHasFrameClause(w) {
		raise("cannot copy window \"%s\" because it has a frame clause", name)
	}
	over := &ast.WindowExpression{
		FrameStart: ast.WindowUnboundedPreceding, FrameEnd: ast.WindowCurrentRowRange,
		ExcludeClause: ast.WindowExcludeNoOther,
	}
	tc.fillWindowContents(over, contents)
	w.FrameStart, w.FrameEnd = over.FrameStart, over.FrameEnd
	w.StartExpr, w.EndExpr = over.StartExpr, over.EndExpr
	w.ExcludeClause = over.ExcludeClause
	if len(w.Orders) > 0 && len(over.Orders) > 0 {
		raise("Cannot override ORDER BY clause of window \"%s\"", name)
	}
	if len(w.Orders) == 0 {
		w.Orders = over.Orders
	}
	if len(w.Partitions) > 0 && len(over.Partitions) > 0 {
		raise("Cannot override PARTITION BY clause of window \"%s\"", name)
	}
	if len(w.Partitions) == 0 {
		w.Partitions = over.Partitions
	}
}

// windowHasFrameClause ports IsWindowFrameDefault's negation plus the
// explicit bound expressions.
func windowHasFrameClause(w *ast.WindowExpression) bool {
	return w.StartExpr != nil || w.EndExpr != nil ||
		w.FrameStart != ast.WindowUnboundedPreceding || w.FrameEnd != ast.WindowCurrentRowRange
}

// fillWindowContents maps WindowFrameContents
// (WindowPartition? OrderByClause? FrameClause?).
func (tc *transformContext) fillWindowContents(w *ast.WindowExpression, contents tnode) {
	if part, ok := contents.child(0).opt(); ok {
		w.Partitions = tc.transformExpressionList(part.child(2))
	}
	if ob, ok := contents.child(1).opt(); ok {
		orders := tc.transformOrderByClause(ob)
		for _, o := range orders {
			if star, isStar := o.Expression.(*ast.StarExpression); isStar && star.Expr == nil {
				raise("Cannot ORDER BY ALL in a window expression")
			}
		}
		w.Orders = append(w.Orders, orders...)
	}
	if fc, ok := contents.child(2).opt(); ok {
		tc.transformFrameClause(w, fc)
	}
}

// transformFrameClause fills the frame bounds.
// FrameClause <- Framing FrameExtent WindowExcludeClause?
func (tc *transformContext) transformFrameClause(w *ast.WindowExpression, n tnode) {
	_, framing := n.child(0).sole().choice()
	kind := "RANGE"
	switch framing.name() {
	case "RowsFraming":
		kind = "ROWS"
	case "GroupsFraming":
		kind = "GROUPS"
	}
	_, extent := n.child(1).sole().choice()
	switch extent.name() {
	case "SingleFrameExtent":
		start, startExpr := tc.transformFrameBound(extent.sole(), kind, true)
		if frameBoundKind(start) == "EXPR_FOLLOWING" {
			// TransformSingleFrameExtent
			raise("Frame starting from following row cannot end with current row")
		}
		w.FrameStart, w.StartExpr = start, startExpr
		switch kind {
		case "ROWS":
			w.FrameEnd = ast.WindowCurrentRowRows
		case "GROUPS":
			w.FrameEnd = ast.WindowCurrentRowGroups
		default:
			w.FrameEnd = ast.WindowCurrentRowRange
		}
	case "BetweenFrameExtent":
		start, startExpr := tc.transformFrameBound(extent.child(1), kind, true)
		end, endExpr := tc.transformFrameBound(extent.child(3), kind, false)
		w.FrameStart, w.StartExpr = start, startExpr
		w.FrameEnd, w.EndExpr = end, endExpr
	default:
		shapeError(extent, "unknown frame extent")
	}
	// TransformFrameClause rejects frames that run backwards
	startKind, endKind := frameBoundKind(w.FrameStart), frameBoundKind(w.FrameEnd)
	if startKind == "CURRENT_ROW" && endKind == "EXPR_PRECEDING" {
		raise("Frame starting from current row cannot have preceding rows")
	}
	if startKind == "EXPR_FOLLOWING" && (endKind == "CURRENT_ROW" || endKind == "EXPR_PRECEDING") {
		raise("Frame starting from following row cannot have preceding rows")
	}
	// upstream validates the start bound before the end bound
	if w.FrameStart == ast.WindowUnboundedFollowing {
		raise("Frame start cannot be UNBOUNDED FOLLOWING")
	}
	if w.FrameEnd == ast.WindowUnboundedPreceding {
		raise("Frame end cannot be UNBOUNDED PRECEDING")
	}
	if excl, ok := n.child(2).opt(); ok {
		_, e := excl.child(1).sole().choice()
		switch e.name() {
		case "ExcludeCurrentRow":
			w.ExcludeClause = ast.WindowExcludeCurrentRow
		case "ExcludeGroup":
			w.ExcludeClause = ast.WindowExcludeGroup
		case "ExcludeTies":
			w.ExcludeClause = ast.WindowExcludeTies
		case "ExcludeNoOthers":
			w.ExcludeClause = ast.WindowExcludeNoOther
		}
	}
}

// transformFrameBound maps one FrameBound to (boundary, expr).
func (tc *transformContext) transformFrameBound(n tnode, kind string, isStart bool) (ast.WindowBoundary, ast.Expr) {
	_, alt := n.sole().choice()
	switch alt.name() {
	case "FrameUnbounded":
		if preceding(alt.child(1)) {
			return ast.WindowUnboundedPreceding, nil
		}
		return ast.WindowUnboundedFollowing, nil
	case "FrameCurrentRow":
		switch kind {
		case "ROWS":
			return ast.WindowCurrentRowRows, nil
		case "GROUPS":
			return ast.WindowCurrentRowGroups, nil
		}
		return ast.WindowCurrentRowRange, nil
	case "FrameExpression":
		expr := tc.transformExpression(alt.child(0))
		dir := "FOLLOWING"
		if preceding(alt.child(1)) {
			dir = "PRECEDING"
		}
		return ast.WindowBoundary("EXPR_" + dir + "_" + kind), expr
	}
	shapeError(alt, "unknown frame bound")
	return "", nil
}

// frameBoundKind strips the framing suffix from a boundary
// (EXPR_PRECEDING_ROWS -> EXPR_PRECEDING): upstream checks frame
// direction on the RANGE spelling before converting to the framing.
func frameBoundKind(b ast.WindowBoundary) string {
	s := string(b)
	for _, suffix := range []string{"_ROWS", "_RANGE", "_GROUPS"} {
		if strings.HasSuffix(s, suffix) {
			return strings.TrimSuffix(s, suffix)
		}
	}
	return s
}

func preceding(n tnode) bool {
	_, alt := n.sole().choice()
	return alt.name() == "PrecedingFrame"
}

// ---- special function forms -------------------------------------------

func (tc *transformContext) transformSpecialFunction(n tnode) ast.Expr {
	_, alt := n.sole().choice()
	sp := alt.span()
	switch alt.name() {
	case "CoalesceExpression":
		return opExpr(sp, ast.OperatorCoalesce, tc.transformExpressionList(alt.child(1).parens())...)
	case "UnpackExpression":
		return opExpr(sp, ast.OperatorUnpack, tc.transformExpression(alt.child(1).parens()))
	case "TryExpression":
		return opExpr(sp, ast.OperatorTry, tc.transformExpression(alt.child(1).parens()))
	case "ColumnsExpression":
		return tc.transformColumnsExpr(alt)
	case "ExtractExpression":
		args := alt.child(1).parens()
		part := extractPartName(args.child(0))
		expr := tc.transformExpression(args.child(2))
		return fnCall(sp, "date_part", constExpr(args.child(0).span(), varcharValue(part)), expr)
	case "LambdaExpression":
		params := identifierList(alt.child(1))
		body := tc.transformExpression(alt.child(3))
		l := &ast.LambdaExpression{LHS: lambdaParams(sp, params), Expr: body, SyntaxType: ast.LambdaKeyword}
		l.SetSpan(sp)
		return l
	case "NullIfExpression":
		args := alt.child(1).parens()
		return fnCall(sp, "nullif", tc.transformExpression(args.child(0)), tc.transformExpression(args.child(2)))
	case "PositionExpression":
		args := alt.child(1).parens()
		needle := tc.transformOtherOperator(args.child(0))
		haystack := tc.transformExpression(args.child(2))
		return fnCall(sp, "position", haystack, needle)
	case "RowExpression":
		var args []ast.Expr
		if list, ok := alt.child(1).parens().opt(); ok {
			args = tc.transformExpressionList(list)
		}
		return fnCall(sp, "row", args...)
	case "SubstringExpression":
		return tc.transformSubstring(alt, sp)
	case "TrimExpression":
		return tc.transformTrim(alt, sp)
	case "OverlayExpression":
		return tc.transformOverlay(alt, sp)
	}
	shapeError(alt, "unknown special function")
	return nil
}

// datePartNames maps the grammar's date part keyword rules to DuckDB's
// DatePartSpecifier spelling (microseconds and milliseconds are plural).
var datePartNames = map[string]string{
	"YearKeyword":        "YEAR",
	"MonthKeyword":       "MONTH",
	"DayKeyword":         "DAY",
	"HourKeyword":        "HOUR",
	"MinuteKeyword":      "MINUTE",
	"SecondKeyword":      "SECOND",
	"MillisecondKeyword": "MILLISECONDS",
	"MicrosecondKeyword": "MICROSECONDS",
	"WeekKeyword":        "WEEK",
	"QuarterKeyword":     "QUARTER",
	"DecadeKeyword":      "DECADE",
	"CenturyKeyword":     "CENTURY",
	"MillenniumKeyword":  "MILLENNIUM",
}

// extractPartName returns the date part of an EXTRACT: keyword parts use
// their canonical enum spelling, identifier and string parts stay
// verbatim.
func extractPartName(n tnode) string {
	_, alt := n.sole().choice()
	switch alt.name() {
	case "ExtractDatePartArgument":
		_, kw := alt.sole().sole().choice()
		if name, ok := datePartNames[kw.name()]; ok {
			return name
		}
		return strings.ToUpper(identifierText(alt.sole()))
	case "ExtractIdentifierArgument", "ExtractStringArgument":
		return identifierText(alt.sole())
	}
	shapeError(alt, "unknown extract argument")
	return ""
}

// lambdaParams builds the lambda LHS: one param is a bare column ref, a
// list becomes row(...).
func lambdaParams(sp ast.Span, names []string) ast.Expr {
	refs := make([]ast.Expr, len(names))
	for i, name := range names {
		r := &ast.ColumnRefExpression{ColumnNames: []string{name}}
		r.SetSpan(invalidSpan)
		refs[i] = r
	}
	if len(refs) == 1 {
		return refs[0]
	}
	return fnCall(invalidSpan, "row", refs...)
}

// ColumnsExpression <- StarSymbol? 'COLUMNS' Parens(Expression)
func (tc *transformContext) transformColumnsExpr(n tnode) ast.Expr {
	_, unpack := n.child(0).opt()
	inner := tc.transformExpression(n.child(2).parens())
	star := &ast.StarExpression{Columns: true}
	star.SetSpan(n.span())
	if innerStar, ok := inner.(*ast.StarExpression); ok && !innerStar.Columns &&
		innerStar.RelationName == "" && innerStar.Expr == nil &&
		len(innerStar.ExcludeList) == 0 && len(innerStar.ReplaceList) == 0 &&
		len(innerStar.QualifiedExcludeList) == 0 && len(innerStar.RenameList) == 0 {
		// COLUMNS(*): a bare star collapses to expr == nil, keeping the
		// inner star's location
		star.SetSpan(ast.Span{Start: innerStar.Pos(), End: innerStar.End()})
	} else if lambda, ok := inner.(*ast.LambdaExpression); ok {
		// COLUMNS(c -> ...) filters the star through the lambda
		innerStar := &ast.StarExpression{}
		innerStar.SetSpan(invalidSpan)
		star.Expr = fnCall(invalidSpan, "list_filter", innerStar, lambda)
	} else if innerStar, ok := inner.(*ast.StarExpression); ok && !innerStar.Columns {
		// COLUMNS(* EXCLUDE (...)): keep the star's modifiers and location
		star.ExcludeList = innerStar.ExcludeList
		star.QualifiedExcludeList = innerStar.QualifiedExcludeList
		star.ReplaceList = innerStar.ReplaceList
		star.RenameList = innerStar.RenameList
		star.RelationName = innerStar.RelationName
		star.SetSpan(ast.Span{Start: innerStar.Pos(), End: innerStar.End()})
	} else {
		star.Expr = inner
	}
	if unpack {
		if star.Expr != nil {
			star.SetSpan(invalidSpan)
		}
		return opExpr(n.span(), ast.OperatorUnpack, star)
	}
	return star
}

// SubstringArguments <- SubstringParameters / SubstringExpressionList
func (tc *transformContext) transformSubstring(n tnode, sp ast.Span) ast.Expr {
	_, alt := n.child(1).parens().sole().choice()
	switch alt.name() {
	case "SubstringExpressionList":
		return fnCall(sp, "substring", tc.transformExpressionList(alt.sole())...)
	case "SubstringParameters":
		// Expression SubstringFromFor
		src := tc.transformExpression(alt.child(0))
		_, ff := alt.child(1).sole().choice()
		args := []ast.Expr{src}
		switch ff.name() {
		case "SubstringFromOptionalFor":
			args = append(args, tc.transformExpression(ff.child(0).child(1)))
			if forNode, ok := ff.child(1).opt(); ok {
				args = append(args, tc.transformExpression(forNode.child(1)))
			}
		case "SubstringFor":
			// substring(s FOR n) == substring(s, 1, n)
			one := constExpr(invalidSpan, ast.Value{Type: ast.LogicalType{ID: "INTEGER"}, Kind: ast.ValueInt64, Int64: 1})
			args = append(args, one, tc.transformExpression(ff.sole().child(1)))
		}
		return fnCall(sp, "substring", args...)
	}
	shapeError(alt, "unknown substring arguments")
	return nil
}

// TrimArguments <- TrimDirection? TrimSource? List(Expression)
func (tc *transformContext) transformTrim(n tnode, sp ast.Span) ast.Expr {
	args := n.child(1).parens()
	name := "trim"
	if dir, ok := args.child(0).opt(); ok {
		_, d := dir.sole().choice()
		switch d.name() {
		case "TrimLeading":
			name = "ltrim"
		case "TrimTrailing":
			name = "rtrim"
		}
	}
	var sourceChars ast.Expr
	if src, ok := args.child(1).opt(); ok {
		// TrimSource <- Expression? 'FROM'
		if e, ok := src.child(0).opt(); ok {
			sourceChars = tc.transformExpression(e)
		}
	}
	list := tc.transformExpressionList(args.child(2))
	var callArgs []ast.Expr
	callArgs = append(callArgs, list...)
	if sourceChars != nil {
		callArgs = append(callArgs, sourceChars)
	}
	return fnCall(sp, name, callArgs...)
}

// OverlayArguments <- OverlayParameters / OverlayExpressionList
func (tc *transformContext) transformOverlay(n tnode, sp ast.Span) ast.Expr {
	_, alt := n.child(1).parens().sole().choice()
	switch alt.name() {
	case "OverlayExpressionList":
		return fnCall(sp, "overlay", tc.transformExpressionList(alt.sole())...)
	case "OverlayParameters":
		// Expression 'PLACING' Expression FromExpression ForExpression?
		args := []ast.Expr{
			tc.transformExpression(alt.child(0)),
			tc.transformExpression(alt.child(2)),
			tc.transformExpression(alt.child(3).child(1)),
		}
		if forNode, ok := alt.child(4).opt(); ok {
			args = append(args, tc.transformExpression(forNode.child(1)))
		}
		return fnCall(sp, "overlay", args...)
	}
	shapeError(alt, "unknown overlay arguments")
	return nil
}

// ---- constructors ------------------------------------------------------

// ListExpression <- ArrayBoundedListExpression / ArrayParensSelect
func (tc *transformContext) transformList(n tnode) ast.Expr {
	_, alt := n.sole().choice()
	sp := alt.span()
	switch alt.name() {
	case "ArrayBoundedListExpression":
		_, hasArray := alt.child(0).opt()
		bounded := alt.child(1)
		var elems []ast.Expr
		if list, ok := bounded.child(1).opt(); ok {
			elems = tc.transformExpressionList(list)
		}
		if hasArray {
			return opExpr(sp, ast.ArrayConstructor, elems...)
		}
		return fnCall(sp, "list_value", elems...)
	case "ArrayParensSelect":
		// ARRAY(SELECT ...) desugars to a scalar subquery:
		// SELECT CASE WHEN array_agg(#1) IS NULL THEN list_value()
		//        ELSE array_agg(#1) END FROM (<query>)
		stmt := tc.transformSelectInternalStatement(alt.child(1).parens())
		src := &ast.SubqueryRef{Subquery: stmt}
		src.SetSpan(invalidSpan)
		// a plain select aggregates its first item (#1); set operations
		// and other node shapes aggregate *COLUMNS(*)
		aggArg := func() ast.Expr {
			if _, isSelect := stmt.Node.(*ast.SelectNode); isSelect {
				p := &ast.PositionalReferenceExpression{Index: 1}
				p.SetSpan(invalidSpan)
				return p
			}
			star := &ast.StarExpression{Columns: true}
			star.SetSpan(invalidSpan)
			return star
		}
		aggOrders := tc.arrayAggOrders(stmt.Node)
		agg := func() ast.Expr {
			f := fnCall(invalidSpan, "array_agg", aggArg())
			f.OrderBys = aggOrders
			return f
		}
		caseExpr := &ast.CaseExpression{
			CaseChecks: []ast.CaseCheck{{
				When: opExpr(invalidSpan, ast.OperatorIsNull, agg()),
				Then: fnCall(invalidSpan, "list_value"),
			}},
			ElseExpr: agg(),
		}
		caseExpr.SetSpan(invalidSpan)
		node := &ast.SelectNode{
			SelectList:        []ast.Expr{caseExpr},
			FromTable:         src,
			AggregateHandling: ast.StandardHandling,
		}
		node.SetSpan(invalidSpan)
		sub := &ast.SubqueryExpression{
			SubqueryType:   ast.SubqueryScalar,
			Subquery:       &ast.SelectStatement{Node: node},
			ComparisonType: ast.InvalidExpressionType,
		}
		sub.SetSpan(sp)
		return sub
	}
	shapeError(alt, "unknown list expression")
	return nil
}

// StructExpression <- '{' List(StructField)? '}'
func (tc *transformContext) transformStruct(n tnode) ast.Expr {
	f := fnCall(n.span(), "struct_pack")
	if list, ok := n.child(1).opt(); ok {
		for _, field := range list.listElems() {
			// StructField <- ColIdOrString ':' Expression
			name := identifierText(field.child(0))
			expr := tc.transformExpression(field.child(2))
			expr.SetAlias(name)
			f.Arguments = append(f.Arguments, ast.FunctionArgument{Name: name, Expr: expr})
		}
	}
	return f
}

// MapExpression <- 'MAP' MapStructExpression
// MAP {k: v, ...} -> map(list_value(keys...), list_value(values...))
func (tc *transformContext) transformMap(n tnode) ast.Expr {
	sp := n.span()
	var keys, values []ast.Expr
	inner := n.child(1)
	if list, ok := inner.child(1).opt(); ok {
		for _, field := range list.listElems() {
			// MapStructField <- Expression ':' Expression
			keys = append(keys, tc.transformExpression(field.child(0)))
			values = append(values, tc.transformExpression(field.child(2)))
		}
	}
	return fnCall(sp, "map", fnCall(invalidSpan, "list_value", keys...), fnCall(invalidSpan, "list_value", values...))
}

// ListComprehensionExpression <-
//
//	'[' Expression 'FOR' List(ColIdOrString) 'IN' Expression ListComprehensionFilter? ']'
//
// Desugars exactly as upstream: list_apply(list_filter(list_apply(src,
// lambda v: struct_pack(filter := cond, result := expr)), lambda elem:
// elem.filter), lambda elem: elem.result) — or plain list_apply without
// the filter.
func (tc *transformContext) transformListComprehension(n tnode) ast.Expr {
	sp := n.span()
	expr := tc.transformExpression(n.child(1))
	params := identifierList(n.child(3))
	src := tc.transformExpression(n.child(5))
	lhs := lambdaParams(sp, params)

	filterNode, hasFilter := n.child(6).opt()
	if !hasFilter {
		l := &ast.LambdaExpression{LHS: lhs, Expr: expr, SyntaxType: ast.LambdaKeyword}
		l.SetSpan(invalidSpan)
		return fnCall(sp, "list_apply", src, l)
	}
	_ = sp
	cond := tc.transformExpression(filterNode.child(1))

	pack := &ast.FunctionExpression{
		FunctionName: "struct_pack",
		Arguments: []ast.FunctionArgument{
			{Name: "filter", Expr: cond},
			{Name: "result", Expr: expr},
		},
	}
	pack.SetSpan(invalidSpan)
	inner := &ast.LambdaExpression{LHS: lhs, Expr: pack, SyntaxType: ast.LambdaKeyword}
	inner.SetSpan(invalidSpan)
	// only the outermost call carries the comprehension's location
	mapped := fnCall(invalidSpan, "list_apply", src, inner)
	filtered := fnCall(invalidSpan, "list_filter", mapped, extractLambda(invalidSpan, "filter"))
	return fnCall(sp, "list_apply", filtered, extractLambda(invalidSpan, "result"))
}

// extractLambda builds `lambda elem: struct_extract(elem, 'field')`.
func extractLambda(sp ast.Span, field string) ast.Expr {
	elem := &ast.ColumnRefExpression{ColumnNames: []string{"elem"}}
	elem.SetSpan(invalidSpan)
	body := fnCall(invalidSpan, "struct_extract", elem, constExpr(invalidSpan, varcharValue(field)))
	lhs := &ast.ColumnRefExpression{ColumnNames: []string{"elem"}}
	lhs.SetSpan(invalidSpan)
	l := &ast.LambdaExpression{
		LHS:        lhs,
		Expr:       body,
		SyntaxType: ast.LambdaKeyword,
	}
	l.SetSpan(sp)
	return l
}

// ---- interval literals -------------------------------------------------

// arrayAggOrders ports upstream's ARRAY(...) ordering: an inner ORDER BY
// is mirrored on the array_agg. Constant integer keys become positional
// references; other keys are appended to a select node's list under
// synthesized __array_internal_idx_<n> aliases and referenced by name.
func (tc *transformContext) arrayAggOrders(node ast.QueryNode) []ast.OrderByNode {
	var orderMod *ast.OrderModifier
	for _, m := range *node.ModifiersRef() {
		if om, ok := m.(*ast.OrderModifier); ok {
			orderMod = om
			break
		}
	}
	if orderMod == nil {
		return nil
	}
	sel, isSelect := node.(*ast.SelectNode)
	var out []ast.OrderByNode
	for i, o := range orderMod.Orders {
		entry := ast.OrderByNode{Type: o.Type, NullOrder: o.NullOrder}
		if c, ok := o.Expression.(*ast.ConstantExpression); ok && c.Value.Kind == ast.ValueInt64 {
			p := &ast.PositionalReferenceExpression{Index: uint64(c.Value.Int64)}
			p.SetSpan(invalidSpan)
			entry.Expression = p
		} else if isSelect {
			alias := "__array_internal_idx_" + itoa(i+1)
			copyExpr := cloneWithAlias(o.Expression, alias)
			sel.SelectList = append(sel.SelectList, copyExpr)
			ref := &ast.ColumnRefExpression{ColumnNames: []string{alias}}
			ref.SetSpan(invalidSpan)
			entry.Expression = ref
		} else {
			expr := o.Expression
			// a qualified column key on a set operation keeps only its
			// final name (the qualifier cannot bind across the union)
			if ref, ok := expr.(*ast.ColumnRefExpression); ok && len(ref.ColumnNames) > 1 {
				stripped := &ast.ColumnRefExpression{ColumnNames: ref.ColumnNames[len(ref.ColumnNames)-1:]}
				stripped.SetSpan(ref.Span)
				expr = stripped
			}
			entry.Expression = expr
		}
		out = append(out, entry)
	}
	return out
}

func itoa(n int) string { return strconv.Itoa(n) }

// cloneWithAlias shallow-copies an expression node with a new alias (the
// original keeps its own).
func cloneWithAlias(e ast.Expr, alias string) ast.Expr {
	switch x := e.(type) {
	case *ast.ColumnRefExpression:
		c := *x
		c.Alias = alias
		return &c
	case *ast.ConstantExpression:
		c := *x
		c.Alias = alias
		return &c
	case *ast.FunctionExpression:
		c := *x
		c.Alias = alias
		return &c
	case *ast.ComparisonExpression:
		c := *x
		c.Alias = alias
		return &c
	case *ast.CastExpression:
		c := *x
		c.Alias = alias
		return &c
	case *ast.OperatorExpression:
		c := *x
		c.Alias = alias
		return &c
	case *ast.CaseExpression:
		c := *x
		c.Alias = alias
		return &c
	case *ast.SubqueryExpression:
		c := *x
		c.Alias = alias
		return &c
	case *ast.BetweenExpression:
		c := *x
		c.Alias = alias
		return &c
	case *ast.WindowExpression:
		c := *x
		c.Alias = alias
		return &c
	case *ast.CollateExpression:
		c := *x
		c.Alias = alias
		return &c
	case *ast.LambdaExpression:
		c := *x
		c.Alias = alias
		return &c
	case *ast.PositionalReferenceExpression:
		c := *x
		c.Alias = alias
		return &c
	case *ast.ParameterExpression:
		c := *x
		c.Alias = alias
		return &c
	case *ast.StarExpression:
		// ORDER BY ALL inside ARRAY(...) clones the star
		c := *x
		c.Alias = alias
		return &c
	}
	return e
}

// intervalFuncs maps the *Keyword rules to conversion functions.
var intervalFuncs = map[string]string{
	"YearKeyword":        "to_years",
	"MonthKeyword":       "to_months",
	"DayKeyword":         "to_days",
	"HourKeyword":        "to_hours",
	"MinuteKeyword":      "to_minutes",
	"SecondKeyword":      "to_seconds",
	"MillisecondKeyword": "to_milliseconds",
	"MicrosecondKeyword": "to_microseconds",
	"WeekKeyword":        "to_weeks",
	"QuarterKeyword":     "to_quarters",
	"DecadeKeyword":      "to_decades",
	"CenturyKeyword":     "to_centuries",
	"MillenniumKeyword":  "to_millennia",
}

// intervalRangeSpecifiers spells the IntervalToInterval alternatives the
// way upstream's error message does.
var intervalRangeSpecifiers = map[string]string{
	"YearToMonth":    "YEAR TO MONTH",
	"DayToHour":      "DAY TO HOUR",
	"DayToMinute":    "DAY TO MINUTE",
	"DayToSecond":    "DAY TO SECOND",
	"HourToMinute":   "HOUR TO MINUTE",
	"HourToSecond":   "HOUR TO SECOND",
	"MinuteToSecond": "MINUTE TO SECOND",
}

// IntervalLiteral <- 'INTERVAL' IntervalParameter Interval?
func (tc *transformContext) transformIntervalLiteral(n tnode) ast.Expr {
	sp := n.span()
	_, param := n.child(1).sole().choice()
	var value ast.Expr
	isString := false
	switch param.name() {
	case "IntervalStringParameter":
		s, _ := param.sole().stringLit()
		value = constExpr(param.span(), varcharValue(s))
		isString = true
	case "NumberLiteral":
		value = numberConstant(param.span(), param.number())
	case "ParensExpression":
		value = tc.transformExpression(param.sole().parens())
	default:
		shapeError(param, "unknown interval parameter")
	}
	unitNode, hasUnit := n.child(2).opt()
	if !hasUnit {
		// INTERVAL '3 months' -> CAST(str AS INTERVAL)
		cast := &ast.CastExpression{Child: value, ResolvedType: "INTERVAL"}
		cast.SetSpan(sp)
		return cast
	}
	_, unit := unitNode.sole().choice()
	if unit.name() == "IntervalToInterval" {
		_, r := unit.sole().choice()
		spec, ok := intervalRangeSpecifiers[r.name()]
		if !ok {
			shapeError(r, "unknown interval range specifier")
		}
		raise("%s is not supported", spec)
	}
	fname, ok := intervalFuncs[unit.name()]
	if !ok {
		shapeError(unit, "unknown interval unit")
	}
	_ = isString
	// the synthesized casts carry no location. Seconds and milliseconds
	// keep their fraction — to_<unit>(CAST(v AS DOUBLE)); other units
	// truncate: to_<unit>(CAST(trunc(CAST(v AS DOUBLE)) AS <int>)), with
	// BIGINT for the fine units and INTEGER for day and coarser.
	toDouble := &ast.CastExpression{Child: value, ResolvedType: "DOUBLE"}
	toDouble.SetSpan(invalidSpan)
	switch unit.name() {
	case "SecondKeyword", "MillisecondKeyword":
		return fnCall(sp, fname, toDouble)
	}
	intType := "INTEGER"
	switch unit.name() {
	case "HourKeyword", "MinuteKeyword", "MicrosecondKeyword":
		intType = "BIGINT"
	}
	trunc := fnCall(invalidSpan, "trunc", toDouble)
	toInt := &ast.CastExpression{Child: trunc, ResolvedType: intType}
	toInt.SetSpan(invalidSpan)
	return fnCall(sp, fname, toInt)
}

var _ = strconv.Itoa
