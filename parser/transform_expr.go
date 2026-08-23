// transform_expr.go: expression transformers — the port of upstream's
// transformer/expression/*.cpp. One function per rule of expression.gram;
// the precedence ladder rules fold their tail lists exactly as upstream
// does (conjunction flattening, constant folding of negated literals,
// operator calls as FunctionExpressions).
package parser

import (
	"strings"

	"github.com/sqlc-dev/darkwing/ast"
	"github.com/sqlc-dev/darkwing/internal/matcher"
)

// ---- small builders ----------------------------------------------------

func fnCall(sp ast.Span, name string, args ...ast.Expr) *ast.FunctionExpression {
	f := &ast.FunctionExpression{FunctionName: name}
	f.SetSpan(sp)
	for _, a := range args {
		f.Arguments = append(f.Arguments, ast.FunctionArgument{Expr: a})
	}
	return f
}

func opCall(sp ast.Span, name string, args ...ast.Expr) *ast.FunctionExpression {
	f := fnCall(sp, name, args...)
	f.IsOperator = true
	return f
}

func opExpr(sp ast.Span, t ast.ExpressionType, children ...ast.Expr) *ast.OperatorExpression {
	o := &ast.OperatorExpression{Type: t, Operands: children}
	o.SetSpan(sp)
	return o
}

func notExpr(sp ast.Span, child ast.Expr) *ast.OperatorExpression {
	return opExpr(sp, ast.OperatorNot, child)
}

func constExpr(sp ast.Span, v ast.Value) *ast.ConstantExpression {
	c := &ast.ConstantExpression{Value: v}
	c.SetSpan(sp)
	return c
}

func varcharValue(s string) ast.Value {
	return ast.Value{Type: ast.LogicalType{ID: "VARCHAR"}, Kind: ast.ValueString, Str: s}
}

// point collapses a span to its start (upstream often stores a bare token
// offset with no length).
func point(sp ast.Span) ast.Span { return ast.Span{Start: sp.Start, End: sp.Start} }

// ---- entry points ------------------------------------------------------

// transformExpression transforms an Expression node (or any node whose
// sole child chain reaches the ladder).
func (tc *transformContext) transformExpression(n tnode) ast.Expr {
	return tc.transformLambdaArrow(n.sole())
}

// transformExpressionList unwraps List(Expression).
func (tc *transformContext) transformExpressionList(n tnode) []ast.Expr {
	elems := n.listElems()
	out := make([]ast.Expr, len(elems))
	for i, e := range elems {
		out[i] = tc.transformExpression(e)
	}
	return out
}

// ---- the precedence ladder --------------------------------------------

// LambdaArrowExpression <- LogicalOrExpression SingleArrowPair*
// Left-associative like upstream: x -> y -> z is LAMBDA(LAMBDA(x, y), z);
// only the outermost node carries a location.
func (tc *transformContext) transformLambdaArrow(n tnode) ast.Expr {
	result := tc.transformLogicalOr(n.child(0))
	tails := n.child(1).repeat()
	for i, t := range tails {
		rhs := tc.transformLogicalOr(t.child(1))
		l := &ast.LambdaExpression{LHS: result, Expr: rhs, SyntaxType: ast.LambdaSingleArrow}
		if i == len(tails)-1 {
			l.SetSpan(ast.Span{Start: n.span().Start, End: t.span().End})
		} else {
			l.SetSpan(invalidSpan)
		}
		result = l
	}
	return result
}

// LogicalOrExpression / LogicalAndExpression fold into one flattened
// conjunction (upstream's TransformConjunction).
func (tc *transformContext) transformLogicalOr(n tnode) ast.Expr {
	return tc.transformConjunction(n, ast.ConjunctionOr, tc.transformLogicalAnd)
}

func (tc *transformContext) transformLogicalAnd(n tnode) ast.Expr {
	return tc.transformConjunction(n, ast.ConjunctionAnd, tc.transformLogicalNot)
}

func (tc *transformContext) transformConjunction(n tnode, t ast.ExpressionType, next func(tnode) ast.Expr) ast.Expr {
	first := next(n.child(0))
	tails := n.child(1).repeat()
	if len(tails) == 0 {
		return first
	}
	children := appendConjunction(nil, t, first)
	for _, tail := range tails {
		children = appendConjunction(children, t, next(tail.child(1)))
	}
	c := &ast.ConjunctionExpression{Type: t, Operands: children}
	c.SetSpan(n.span())
	return c
}

// appendConjunction splices nested conjunctions of the same type into
// the parent (upstream flattens through parentheses).
func appendConjunction(children []ast.Expr, t ast.ExpressionType, e ast.Expr) []ast.Expr {
	if nested, ok := e.(*ast.ConjunctionExpression); ok && nested.Type == t {
		return append(children, nested.Operands...)
	}
	return append(children, e)
}

// LogicalNotExpression <- NotExpression? IsExpression
func (tc *transformContext) transformLogicalNot(n tnode) ast.Expr {
	expr := tc.transformIs(n.child(1))
	if notNode, ok := n.child(0).opt(); ok {
		// NotExpression <- NotKeyword+
		end := n.span().End
		nots := notNode.sole().repeat()
		for i := len(nots) - 1; i >= 0; i-- {
			expr = notExpr(ast.Span{Start: nots[i].sole().span().Start, End: end}, expr)
		}
	}
	return expr
}

// IsExpression <- IsDistinctFromExpression IsTest*
func (tc *transformContext) transformIs(n tnode) ast.Expr {
	expr := tc.transformIsDistinctFrom(n.child(0))
	for _, test := range n.child(1).repeat() {
		expr = tc.transformIsTest(expr, test)
	}
	return expr
}

func (tc *transformContext) transformIsTest(input ast.Expr, n tnode) ast.Expr {
	_, alt := n.sole().choice()
	sp := ast.Span{Start: n.span().Start, End: n.span().End}
	switch alt.name() {
	case "IsLiteral":
		// 'IS' 'NOT'? IsLiteralValue
		_, negated := alt.child(1).opt()
		_, value := alt.child(2).sole().choice()
		switch value.name() {
		case "NullLiteral":
			if negated {
				return opExpr(sp, ast.OperatorIsNotNull, input)
			}
			return opExpr(sp, ast.OperatorIsNull, input)
		case "UnknownLiteral":
			if negated {
				return opExpr(sp, ast.OperatorIsNotNull, input)
			}
			return opExpr(sp, ast.OperatorIsNull, input)
		case "TrueLiteral", "FalseLiteral":
			// IS [NOT] TRUE/FALSE: (CAST(x AS BOOLEAN)) IS [NOT] DISTINCT FROM literal
			cast := &ast.CastExpression{Child: input, ResolvedType: "BOOLEAN"}
			cast.SetSpan(invalidSpan)
			lit := constExpr(invalidSpan, ast.Value{
				Type: ast.LogicalType{ID: "BOOLEAN"}, Kind: ast.ValueBool,
				Bool: value.name() == "TrueLiteral",
			})
			t := ast.CompareNotDistinctFrom
			if negated {
				t = ast.CompareDistinctFrom
			}
			cmp := &ast.ComparisonExpression{Type: t, Left: cast, Right: lit}
			cmp.SetSpan(sp)
			return cmp
		}
	case "NotNull":
		return opExpr(sp, ast.OperatorIsNotNull, input)
	case "IsNull":
		return opExpr(sp, ast.OperatorIsNull, input)
	}
	shapeError(alt, "unknown IsTest")
	return nil
}

// IsDistinctFromExpression <- ComparisonExpression IsDistinctFromTail*
func (tc *transformContext) transformIsDistinctFrom(n tnode) ast.Expr {
	expr := tc.transformComparison(n.child(0))
	for _, tail := range n.child(1).repeat() {
		// IsDistinctFromTail <- IsDistinctFromOp ComparisonExpression
		op := tail.child(0) // 'IS' 'NOT'? 'DISTINCT' 'FROM'
		_, negated := op.child(1).opt()
		t := ast.CompareDistinctFrom
		if negated {
			t = ast.CompareNotDistinctFrom
		}
		right := tc.transformComparison(tail.child(1))
		cmp := &ast.ComparisonExpression{Type: t, Left: expr, Right: right}
		cmp.SetSpan(ast.Span{Start: n.span().Start, End: tail.span().End})
		expr = cmp
	}
	return expr
}

var comparisonOps = map[string]ast.ExpressionType{
	"OperatorEqual":             ast.CompareEqual,
	"OperatorNotEqual":          ast.CompareNotEqual,
	"OperatorLessThan":          ast.CompareLessThan,
	"OperatorGreaterThan":       ast.CompareGreaterThan,
	"OperatorLessThanEquals":    ast.CompareLessThanOrEqual,
	"OperatorGreaterThanEquals": ast.CompareGreaterThanOrEqual,
}

// ComparisonExpression <- BetweenInLikeExpression ComparisonExpressionTail*
func (tc *transformContext) transformComparison(n tnode) ast.Expr {
	expr := tc.transformBetweenInLike(n.child(0))
	tails := n.child(1).repeat()
	if len(tails) == 0 {
		return expr
	}
	if len(tails) > 1 {
		// upstream bans chained comparisons in the transformer
		raise("Chained comparisons (e.g. a < b < c) are not supported")
	}
	tail := tails[0]
	_, opAlt := tail.child(0).sole().choice()
	t, ok := comparisonOps[opAlt.name()]
	if !ok {
		shapeError(opAlt, "unknown comparison operator")
	}
	right := tc.transformBetweenInLike(tail.child(2))
	if notNode, hasNot := tail.child(1).opt(); hasNot {
		nots := notNode.sole().repeat()
		for i := len(nots) - 1; i >= 0; i-- {
			right = notExpr(ast.Span{Start: nots[i].sole().span().Start, End: right.End()}, right)
		}
	}
	cmp := &ast.ComparisonExpression{Type: t, Left: expr, Right: right}
	cmp.SetSpan(n.span())
	return cmp
}

// BetweenInLikeExpression <- OtherOperatorExpression BetweenInLikeOp?
func (tc *transformContext) transformBetweenInLike(n tnode) ast.Expr {
	expr := tc.transformOtherOperator(n.child(0))
	opNode, ok := n.child(1).opt()
	if !ok {
		return expr
	}
	// BetweenInLikeOp <- 'NOT'? BetweenInLikeOpExpression
	_, negated := opNode.child(0).opt()
	_, alt := opNode.child(1).sole().choice()
	// the whole-expression span, used by the NOT wrapper
	wholeSp := ast.Span{Start: n.span().Start, End: alt.span().End}
	var result ast.Expr
	switch alt.name() {
	case "BetweenClause":
		lower := tc.transformOtherOperator(alt.child(1))
		upper := tc.transformOtherOperator(alt.child(3))
		b := &ast.BetweenExpression{Input: expr, Lower: lower, Upper: upper}
		b.SetSpan(alt.span())
		result = b
	case "InClause":
		result = tc.transformInClause(expr, alt)
	case "LikeClause":
		result = tc.transformLikeClause(expr, alt, negated, wholeSp)
		// like handles its own negation
		return result
	}
	if negated {
		result = notExpr(wholeSp, result)
	}
	return result
}

// InClause <- 'IN' InExpression: the result's location is the IN target
// (the parenthesized list/subquery), matching upstream.
func (tc *transformContext) transformInClause(input ast.Expr, n tnode) ast.Expr {
	_, alt := n.child(1).sole().choice()
	switch alt.name() {
	case "InExpressionList":
		list := tc.transformExpressionList(alt.parens())
		// a single scalar subquery in the list is the subquery form
		if len(list) == 1 {
			if sub, ok := list[0].(*ast.SubqueryExpression); ok && sub.SubqueryType == ast.SubqueryScalar {
				sub.SubqueryType = ast.SubqueryAny
				sub.Child = input
				sub.ComparisonType = ast.CompareEqual
				sub.SetSpan(alt.span())
				return sub
			}
		}
		children := append([]ast.Expr{input}, list...)
		return opExpr(alt.span(), ast.CompareIn, children...)
	case "InSelectStatement":
		stmt := tc.transformSelectInternalStatement(alt.parens())
		sub := &ast.SubqueryExpression{
			SubqueryType:   ast.SubqueryAny,
			Subquery:       stmt,
			Child:          input,
			ComparisonType: ast.CompareEqual,
		}
		sub.SetSpan(alt.span())
		return sub
	case "InContainsExpression":
		// x IN y -> contains(y, x)
		haystack := tc.transformOtherOperator(alt.sole())
		return fnCall(alt.span(), "contains", haystack, input)
	}
	shapeError(alt, "unknown InExpression")
	return nil
}

// likeFunc maps the like-variation rule (plus negation) to the function
// upstream emits: (name, is_operator, wrapInNot).
func likeFunc(variation string, negated bool) (string, bool, bool) {
	switch variation {
	case "LikeToken":
		if negated {
			return "!~~", true, false
		}
		return "~~", true, false
	case "ILikeToken":
		if negated {
			return "!~~*", true, false
		}
		return "~~*", true, false
	case "GlobToken":
		return "~~~", true, negated
	case "SimilarToToken":
		return "regexp_full_match", false, negated
	case "RegexMatchToken":
		return "regexp_matches", false, negated
	case "RegexInsensitiveMatchToken":
		return "regexp_matches", false, negated // + 'i' flag argument
	case "NotLikeOp":
		return "!~~", true, negated
	case "NotILikeOp":
		return "!~~*", true, negated
	case "NotRegexInsensitiveMatchOp":
		return "regexp_matches", false, !negated // + 'i' flag
	case "NotSimilarToOp":
		// '!~' is the negated regex match operator
		return "regexp_matches", false, !negated
	}
	return "", false, false
}

// LikeClause <- LikeVariations OtherOperatorExpression EscapeClause?
// The function's location is the like clause itself; a NOT wrapper covers
// the whole expression (wholeSp).
func (tc *transformContext) transformLikeClause(input ast.Expr, n tnode, negated bool, wholeSp ast.Span) ast.Expr {
	sp := n.span()
	_, variation := n.child(0).sole().choice()
	pattern := tc.transformOtherOperator(n.child(1))
	name, isOp, wrapNot := likeFunc(variation.name(), negated)
	if name == "" {
		shapeError(variation, "unknown like variation")
	}
	args := []ast.Expr{input, pattern}
	insensitiveRegex := variation.name() == "RegexInsensitiveMatchToken" || variation.name() == "NotRegexInsensitiveMatchOp"
	if insensitiveRegex {
		args = append(args, constExpr(invalidSpan, varcharValue("i")))
	}
	if escNode, ok := n.child(2).opt(); ok {
		// EscapeClause <- 'ESCAPE' ComparisonExpression; negation stays a
		// NOT wrapper around the positive escape function
		esc := tc.transformComparison(escNode.child(1))
		switch name {
		case "~~":
			name, isOp = "like_escape", true
		case "!~~":
			name, isOp, wrapNot = "like_escape", true, !wrapNot
		case "~~*":
			name, isOp = "ilike_escape", true
		case "!~~*":
			name, isOp, wrapNot = "ilike_escape", true, !wrapNot
		default:
			raise("ESCAPE is only supported for LIKE/ILIKE")
		}
		args = append(args, esc)
	}
	var result ast.Expr
	if isOp {
		result = opCall(sp, name, args...)
	} else {
		result = fnCall(sp, name, args...)
	}
	if wrapNot {
		result = notExpr(wholeSp, result)
	}
	return result
}

// negatedComparison maps an ExpressionType to its negation (for ALL
// desugaring: x > ALL(...) becomes NOT(x <= ANY(...))).
var negatedComparison = map[ast.ExpressionType]ast.ExpressionType{
	ast.CompareEqual:              ast.CompareNotEqual,
	ast.CompareNotEqual:           ast.CompareEqual,
	ast.CompareLessThan:           ast.CompareGreaterThanOrEqual,
	ast.CompareGreaterThan:        ast.CompareLessThanOrEqual,
	ast.CompareLessThanOrEqual:    ast.CompareGreaterThan,
	ast.CompareGreaterThanOrEqual: ast.CompareLessThan,
}

// comparisonFromOpText maps an operator spelling to a comparison type
// (nil for non-comparison operators).
func comparisonFromOpText(op string) (ast.ExpressionType, bool) {
	switch op {
	case "=", "==":
		return ast.CompareEqual, true
	case "!=", "<>":
		return ast.CompareNotEqual, true
	case "<":
		return ast.CompareLessThan, true
	case ">":
		return ast.CompareGreaterThan, true
	case "<=":
		return ast.CompareLessThanOrEqual, true
	case ">=":
		return ast.CompareGreaterThanOrEqual, true
	}
	return "", false
}

// OtherOperatorExpression <- BitwiseExpression OtherOperatorTail*
func (tc *transformContext) transformOtherOperator(n tnode) ast.Expr {
	expr := tc.transformBitwise(n.child(0))
	tails := n.child(1).repeat()
	for i, tail := range tails {
		_, opAlt := tail.child(0).sole().choice()
		rhsNode := tail.child(1)
		outermost := i == len(tails)-1
		switch opAlt.name() {
		case "AnyAllParsedOperator":
			expr = tc.transformAnyAll(expr, opAlt.sole(), rhsNode, n.span().Start)
		case "NamedOtherOperator":
			expr = tc.transformNamedOperator(expr, opAlt.sole(), rhsNode, n.span().Start, outermost)
		default:
			shapeError(opAlt, "unknown other-operator")
		}
	}
	return expr
}

// AnyAllOperator <- AnyOp AnyOrAll
func (tc *transformContext) transformAnyAll(input ast.Expr, n tnode, rhsNode tnode, exprStart int) ast.Expr {
	opText := keywordOfOp(n.child(0))
	_, anyOrAll := n.child(1).sole().choice()
	isAll := anyOrAll.name() == "SubqueryAll"
	rhs := tc.transformBitwise(rhsNode)
	whole := ast.Span{Start: exprStart, End: rhsNode.span().End}
	cmp, ok := comparisonFromOpText(opText)
	if !ok {
		if sub, isSub := rhs.(*ast.SubqueryExpression); isSub && sub.SubqueryType == ast.SubqueryScalar {
			raise("ANY and ALL operators require one of =,<>,>,<,>=,<= comparisons!")
		}
		switch opText {
		case "~", "~*", "!~", "!~*":
			return regexAnyAll(input, rhs, opText, isAll, whole)
		}
		raise("Unsupported comparison \"%s\" for ANY/ALL subquery", opText)
	}
	if isAll {
		cmp = negatedComparison[cmp]
	}
	sub := tc.anySubquery(input, rhs, cmp)
	if isAll {
		// x > ALL(...) becomes NOT(x <= ANY(...)); the synthesized
		// subquery node carries no location, the NOT the full extent
		sub.SetSpan(invalidSpan)
		return notExpr(whole, sub)
	}
	sub.SetSpan(whole)
	return sub
}

// anySubquery builds SUBQUERY(ANY): a subquery rhs is used directly, any
// other expression is wrapped in SELECT unnest(rhs) — upstream's
// TransformAnyAll.
func (tc *transformContext) anySubquery(input, rhs ast.Expr, cmp ast.ExpressionType) *ast.SubqueryExpression {
	var stmt *ast.SelectStatement
	if sub, ok := rhs.(*ast.SubqueryExpression); ok && sub.SubqueryType == ast.SubqueryScalar {
		stmt = sub.Subquery
	} else {
		sel := &ast.SelectNode{
			SelectList:        []ast.Expr{fnCall(invalidSpan, "unnest", rhs)},
			AggregateHandling: ast.StandardHandling,
		}
		sel.SetSpan(invalidSpan)
		empty := &ast.EmptyTableRef{}
		empty.SetSpan(invalidSpan)
		sel.FromTable = empty
		stmt = &ast.SelectStatement{Node: sel}
	}
	out := &ast.SubqueryExpression{
		SubqueryType:   ast.SubqueryAny,
		Subquery:       stmt,
		Child:          input,
		ComparisonType: cmp,
	}
	return out
}

// regexAnyAll desugars `x ~ ANY(list)` (and the ~*/!~/!~* and ALL
// variants) into upstream's CASE:
//
//	CASE WHEN list_contains(list_transform(list, LAMBDA p: regexp_matches(x, p)), true) THEN true
//	     WHEN len(list_filter(list_transform(...), LAMBDA m: m IS NULL)) > 0 THEN NULL
//	     ELSE false END
//
// ALL flips the contains probe and the THEN/ELSE booleans; negated
// operators wrap regexp_matches in NOT; ~*/!~* pass the 'i' flag. Every
// synthesized node is location-free except the top CASE, which spans the
// whole expression.
func regexAnyAll(input, list ast.Expr, opText string, isAll bool, whole ast.Span) ast.Expr {
	caseInsensitive := opText == "~*" || opText == "!~*"
	negated := opText == "!~" || opText == "!~*"

	nameRef := func(name string) *ast.ColumnRefExpression {
		r := &ast.ColumnRefExpression{ColumnNames: []string{name}}
		r.SetSpan(invalidSpan)
		return r
	}
	boolConst := func(b bool) *ast.ConstantExpression {
		return constExpr(invalidSpan, ast.Value{Type: ast.LogicalType{ID: "BOOLEAN"}, Kind: ast.ValueBool, Bool: b})
	}

	// LAMBDA __regex_pattern: [NOT] regexp_matches(input, __regex_pattern[, 'i'])
	matchArgs := []ast.Expr{input, nameRef("__regex_pattern")}
	if caseInsensitive {
		matchArgs = append(matchArgs, constExpr(invalidSpan, varcharValue("i")))
	}
	var body ast.Expr = fnCall(invalidSpan, "regexp_matches", matchArgs...)
	if negated {
		body = notExpr(invalidSpan, body)
	}
	regexLambda := &ast.LambdaExpression{LHS: nameRef("__regex_pattern"), Expr: body, SyntaxType: ast.LambdaKeyword}
	regexLambda.SetSpan(invalidSpan)
	transformed := fnCall(invalidSpan, "list_transform", list, regexLambda)

	contains := fnCall(invalidSpan, "list_contains", transformed, boolConst(!isAll))

	// LAMBDA __regex_match: __regex_match IS NULL
	nullLambda := &ast.LambdaExpression{
		LHS:        nameRef("__regex_match"),
		Expr:       opExpr(invalidSpan, ast.OperatorIsNull, nameRef("__regex_match")),
		SyntaxType: ast.LambdaKeyword,
	}
	nullLambda.SetSpan(invalidSpan)
	filtered := fnCall(invalidSpan, "list_filter", transformed, nullLambda)
	anyNull := &ast.ComparisonExpression{
		Type:  ast.CompareGreaterThan,
		Left:  fnCall(invalidSpan, "len", filtered),
		Right: constExpr(invalidSpan, ast.Value{Type: ast.LogicalType{ID: "INTEGER"}, Kind: ast.ValueInt64, Int64: 0}),
	}
	anyNull.SetSpan(invalidSpan)

	out := &ast.CaseExpression{
		CaseChecks: []ast.CaseCheck{
			{When: contains, Then: boolConst(!isAll)},
			{When: anyNull, Then: constExpr(invalidSpan, ast.Value{Type: ast.LogicalType{ID: "NULL"}, IsNull: true, Kind: ast.ValueNull})},
		},
		ElseExpr: boolConst(isAll),
	}
	out.SetSpan(whole)
	return out
}

// NamedOtherOperator <- QualifiedOperator / InetOperator / JsonOperator /
// ListOperator / StringOperator / OperatorLiteral
// Only the outermost fold of a chain carries a location, like the binary
// ladder levels.
func (tc *transformContext) transformNamedOperator(input ast.Expr, n tnode, rhsNode tnode, exprStart int, outermost bool) ast.Expr {
	_, alt := n.choice()
	rhs := tc.transformBitwise(rhsNode)
	sp := invalidSpan
	if outermost {
		sp = ast.Span{Start: exprStart, End: rhsNode.span().End}
	}
	if alt.name() == "QualifiedOperator" {
		// OPERATOR(schema.op) is always a function call — comparison
		// spellings included — with the qualifier as its schema
		schema, opText := qualifiedOperatorParts(alt)
		f := opCall(sp, opText, input, rhs)
		f.Schema = schema
		return f
	}
	var opText string
	switch alt.name() {
	case "InetOperator", "JsonOperator", "ListOperator", "StringOperator":
		opText = keywordOfOp(alt.sole())
	default:
		// OperatorLiteral: a free operator token
		op, ok := alt.r.(*matcher.OperatorResult)
		if !ok {
			shapeError(alt, "want operator literal, got %T", alt.r)
		}
		opText = op.Operator
	}
	if cmp, isCmp := comparisonFromOpText(opText); isCmp {
		c := &ast.ComparisonExpression{Type: cmp, Left: input, Right: rhs}
		c.SetSpan(sp)
		return c
	}
	return opCall(sp, opText, input, rhs)
}

// qualifiedOperatorParts unpacks OPERATOR(Parens(ColIdDot* AnyOp)) into
// an optional schema qualifier and the operator spelling; more than one
// qualifier is upstream's transformer error.
func qualifiedOperatorParts(alt tnode) (schema, op string) {
	contents := alt.child(1).parens()
	quals := qualifiedOperatorQualifiers(contents)
	if len(quals) > 1 {
		raise("Too many identifiers found, expected schema.operator or operator")
	}
	if len(quals) == 1 {
		schema = quals[0]
	}
	return schema, keywordOfOp(contents.child(1))
}

func qualifiedOperatorQualifiers(contents tnode) []string {
	var quals []string
	if reps, ok := contents.child(0).opt(); ok {
		for _, cid := range reps.repeat() {
			quals = append(quals, identifierText(cid.child(0)))
		}
	}
	return quals
}

// binaryLadder folds `X <- Y Tail*` with Tail = [op, Y] into left-assoc
// operator function calls. Additive operators carry a bare op-token
// location; every other level spans the whole expression (upstream's
// inconsistency, pinned by the conformance suite).
func (tc *transformContext) binaryLadder(n tnode, next func(tnode) ast.Expr, opTokenLoc bool) ast.Expr {
	expr := next(n.child(0))
	tails := n.child(1).repeat()
	for i, tail := range tails {
		opNode := tail.child(0)
		opText := keywordOfOp(opNode)
		rhs := next(tail.child(1))
		var sp ast.Span
		switch {
		case opTokenLoc:
			sp = point(opNode.span())
		case i == len(tails)-1:
			// only the outermost fold of a chain carries a location
			sp = ast.Span{Start: n.span().Start, End: tail.span().End}
		default:
			sp = invalidSpan
		}
		expr = opCall(sp, opText, expr, rhs)
	}
	return expr
}

// keywordOfOp extracts the operator spelling from an op rule node (a
// keyword, or a single-child rule wrapping one).
func keywordOfOp(n tnode) string {
	switch r := n.r.(type) {
	case *matcher.KeywordResult:
		return r.Keyword
	case *matcher.ListResult:
		if len(r.Children) == 1 {
			return keywordOfOp(tnode{r.Children[0]})
		}
	case *matcher.ChoiceResult:
		return keywordOfOp(tnode{r.Result})
	}
	shapeError(n, "cannot extract operator text (%T)", n.r)
	return ""
}

func (tc *transformContext) transformBitwise(n tnode) ast.Expr {
	return tc.binaryLadder(n, tc.transformAdditive, false)
}

func (tc *transformContext) transformAdditive(n tnode) ast.Expr {
	return tc.binaryLadder(n, tc.transformMultiplicative, true)
}

func (tc *transformContext) transformMultiplicative(n tnode) ast.Expr {
	return tc.binaryLadder(n, tc.transformExponentiation, false)
}

func (tc *transformContext) transformExponentiation(n tnode) ast.Expr {
	return tc.binaryLadder(n, tc.transformCollate, false)
}

// CollateExpression <- AtTimeZoneExpression CollateExpressionTail*
func (tc *transformContext) transformCollate(n tnode) ast.Expr {
	expr := tc.transformAtTimeZone(n.child(0))
	for _, tail := range n.child(1).repeat() {
		rhs := tc.transformAtTimeZone(tail.child(1))
		collation := collationName(rhs)
		c := &ast.CollateExpression{Child: expr, Collation: collation}
		c.SetSpan(ast.Span{Start: n.span().Start, End: tail.span().End})
		expr = c
	}
	return expr
}

// collationName extracts the collation name from the right-hand
// expression of COLLATE (a bare identifier chain).
func collationName(e ast.Expr) string {
	ref, ok := e.(*ast.ColumnRefExpression)
	if !ok {
		raise("COLLATE expects a collation name")
	}
	return strings.Join(ref.ColumnNames, ".")
}

// AtTimeZoneExpression <- PrefixExpression AtTimeZoneExpressionTail*
// x AT TIME ZONE tz  ->  timezone(tz, x)
func (tc *transformContext) transformAtTimeZone(n tnode) ast.Expr {
	expr := tc.transformPrefix(n.child(0))
	for _, tail := range n.child(1).repeat() {
		rhs := tc.transformPrefix(tail.child(1))
		expr = fnCall(ast.Span{Start: n.span().Start, End: tail.span().End}, "timezone", rhs, expr)
	}
	return expr
}

// PrefixExpression <- PrefixOperator* BaseExpression
func (tc *transformContext) transformPrefix(n tnode) ast.Expr {
	expr := tc.transformBase(n.child(1))
	ops := n.child(0).repeat()
	end := n.span().End
	for i := len(ops) - 1; i >= 0; i-- {
		_, alt := ops[i].sole().choice()
		// only the outermost prefix operator carries a location
		sp := invalidSpan
		if i == 0 {
			sp = ast.Span{Start: alt.span().Start, End: end}
		}
		switch alt.name() {
		case "MinusPrefixOperator":
			if folded, ok := negateConstant(expr, alt.span().Start, end, i == 0); ok {
				expr = folded
				continue
			}
			expr = opCall(sp, "-", expr)
		case "PlusPrefixOperator":
			expr = opCall(sp, "+", expr)
		case "TildePrefixOperator":
			expr = opCall(sp, "~", expr)
		case "QualifiedOperator":
			// the prefix form folds qualifiers into the name itself:
			// OPERATOR(main.-) x calls "main.-", mirroring upstream
			contents := alt.child(1).parens()
			name := keywordOfOp(contents.child(1))
			if quals := qualifiedOperatorQualifiers(contents); len(quals) > 0 {
				name = strings.Join(append(quals, name), ".")
			}
			expr = opCall(sp, name, expr)
		default:
			shapeError(alt, "unknown prefix operator")
		}
	}
	return expr
}

// negateConstant folds `-literal` into a negative constant, mirroring
// upstream's constant folding for numeric literals; the folded constant's
// location grows to include the minus sign.
func negateConstant(e ast.Expr, opStart, end int, outermost bool) (ast.Expr, bool) {
	c, ok := e.(*ast.ConstantExpression)
	if !ok {
		return nil, false
	}
	v := c.Value
	switch v.Kind {
	case ast.ValueInt64:
		v.Int64 = -v.Int64
	case ast.ValueDouble:
		v.Float64 = -v.Float64
	case ast.ValueHugeint:
		v.Hugeint = negateHugeint(v.Hugeint)
		if v.Type.ID == "UHUGEINT" {
			// -(2^127) is representable as HUGEINT; larger magnitudes
			// stay unfolded
			if uint64(v.Hugeint.Upper) < 1<<63 && !(v.Hugeint.Upper == 0 && v.Hugeint.Lower == 0) {
				return nil, false
			}
			v.Type = ast.LogicalType{ID: "HUGEINT"}
		}
		// a negated HUGEINT that now fits an int64 demotes (the INT64_MIN
		// literal is only representable negated) — but only when this
		// minus is the outermost prefix operator
		if outermost && v.Hugeint.Upper == -1 && v.Hugeint.Lower >= 1<<63 {
			v.Kind = ast.ValueInt64
			v.Int64 = int64(v.Hugeint.Lower)
			v.Hugeint = ast.Hugeint{}
		}
	default:
		return nil, false
	}
	// negating can shrink or widen the integer class (e.g. the INT32_MIN
	// literal is only representable negated); DECIMAL keeps its type
	if v.Kind == ast.ValueInt64 && v.Type.ID != "DECIMAL" {
		v.Type = ast.LogicalType{ID: integerTypeFor(v.Int64)}
	}
	out := &ast.ConstantExpression{Value: v}
	if outermost {
		// the folded constant spans the operator through the operand
		// (parentheses included)
		out.SetSpan(ast.Span{Start: opStart, End: end})
	} else {
		out.SetSpan(invalidSpan)
	}
	return out, true
}

func negateHugeint(h ast.Hugeint) ast.Hugeint {
	// two's complement negation of the 128-bit value
	upper := ^h.Upper
	lower := ^h.Lower + 1
	if lower == 0 {
		upper++
	}
	return ast.Hugeint{Upper: upper, Lower: lower}
}

// BaseExpression <- SingleExpression IndirectionList?
func (tc *transformContext) transformBase(n tnode) ast.Expr {
	single := n.child(0)
	expr := tc.transformSingle(single)
	// dot indirection behaves differently on a bare column reference
	// (qualified function names, column path extension) than on any other
	// base — including a parenthesized column reference
	_, singleAlt := single.sole().choice()
	bare := singleAlt.name() == "ColumnReference"
	if indirections, ok := n.child(1).opt(); ok {
		items := indirections.sole().repeat()
		prevWasCast := false
		for i, ind := range items {
			// upstream rejects an operator-class indirection (subscript /
			// slice) directly after a `::` cast
			_, kind := ind.sole().choice()
			if kind.name() == "SliceExpression" && prevWasCast {
				raise("Subscript/slice cannot be applied directly after a cast operator " +
					"(e.g. x::TYPE[1:3] is not allowed). Wrap the cast in parentheses: (x::TYPE)[1:3]")
			}
			prevWasCast = kind.name() == "CastOperator"
			expr, bare = tc.transformIndirection(expr, ind, n.span().Start, bare, i == len(items)-1)
		}
	}
	return expr
}

// Indirection <- CastOperator / DotOperator / SliceExpression / PostfixOperator
// baseStart is the whole base expression's starting offset (dot and
// postfix operators span from there). bare reports whether the base is
// still a bare column-reference chain; the return value updates it.
func (tc *transformContext) transformIndirection(base ast.Expr, n tnode, baseStart int, bare, last bool) (ast.Expr, bool) {
	_, alt := n.sole().choice()
	switch alt.name() {
	case "CastOperator":
		// '::' Type — the cast's location is the operator through the type
		cast := &ast.CastExpression{Child: base, CastType: tc.transformType(alt.child(1))}
		cast.SetSpan(alt.span())
		return cast, false
	case "DotOperator":
		return tc.transformDot(base, alt, baseStart, bare, last)
	case "SliceExpression":
		return tc.transformSlice(base, alt), false
	case "PostfixOperator":
		return fnCall(alt.span(), "factorial", base), false
	}
	shapeError(alt, "unknown indirection")
	return nil, false
}

// transformDot handles `.name` and `.method(...)` indirection.
func (tc *transformContext) transformDot(base ast.Expr, n tnode, baseStart int, bare, last bool) (ast.Expr, bool) {
	_, alt := n.sole().choice()
	switch alt.name() {
	case "DotColumnOperator":
		name := identifierText(alt.child(1))
		if ref, ok := base.(*ast.ColumnRefExpression); ok && bare {
			if len(ref.ColumnNames) < 4 {
				out := &ast.ColumnRefExpression{ColumnNames: append(append([]string{}, ref.ColumnNames...), name)}
				out.SetSpan(ast.Span{Start: ref.Pos(), End: alt.span().End})
				return out, true
			}
		}
		key := constExpr(alt.span(), varcharValue(name))
		// only the outermost extraction carries a location
		sp := invalidSpan
		if last {
			sp = ast.Span{Start: baseStart, End: alt.span().End}
		}
		return opExpr(sp, ast.StructExtract, base, key), false
	case "DotMethodOperator":
		// MethodExpression <- ColLabel MethodExpressionArguments
		method := alt.child(1)
		name := identifierText(method.child(0))
		f := tc.transformFunctionArgumentsInto(method.child(1).parens(), name)
		// a bare column-ref base becomes the function's qualification;
		// any other base is prepended as the first argument
		if ref, ok := base.(*ast.ColumnRefExpression); ok && bare {
			if len(ref.ColumnNames) > 2 {
				raise("syntax error at or near \"(\"")
			}
			if len(ref.ColumnNames) == 1 {
				f.Schema = ref.ColumnNames[0]
			} else {
				f.Catalog = ref.ColumnNames[0]
				f.Schema = ref.ColumnNames[1]
			}
			f.SetSpan(ast.Span{Start: base.Pos(), End: alt.span().End})
			return f, false
		}
		f.SetSpan(method.span())
		f.Arguments = append([]ast.FunctionArgument{{Expr: base}}, f.Arguments...)
		return f, false
	}
	shapeError(alt, "unknown dot operator")
	return nil, false
}

// emptyListValue is the sentinel constant upstream uses for omitted slice
// bounds: an empty LIST(INTEGER).
func emptyListValue(sp ast.Span) *ast.ConstantExpression {
	return constExpr(sp, ast.Value{
		Type: ast.LogicalType{ID: "LIST", Child: &ast.LogicalType{ID: "INTEGER"}},
		Kind: ast.ValueEmptyList,
	})
}

// SliceExpression <- '[' SliceBound ']'
// SliceBound <- Expression? EndSliceBound? StepSliceBound?
func (tc *transformContext) transformSlice(base ast.Expr, n tnode) ast.Expr {
	bound := n.child(1)
	// the slice operator's location is the bracketed subscript itself
	sp := n.span()
	var start ast.Expr
	if s, ok := bound.child(0).opt(); ok {
		start = tc.transformExpression(s)
	}
	endBound, hasEnd := bound.child(1).opt()
	if !hasEnd {
		if start == nil {
			raise("empty subscript is not allowed")
		}
		return opExpr(sp, ast.ArrayExtract, base, start)
	}
	// an omitted start bound has no location; an omitted end bound sits
	// on the ':'
	colon := endBound.child(0).span()
	sentinelSpan := ast.Span{Start: colon.Start, End: colon.Start + 1}
	if start == nil {
		start = emptyListValue(invalidSpan)
	}
	// EndSliceBound <- ':' EndSliceValue?
	var end ast.Expr
	if ev, ok := endBound.child(1).opt(); ok {
		_, alt := ev.sole().choice()
		if alt.name() == "EndSliceMinus" {
			// explicit '-': open end, sentinel located on the '-'
			minus := alt.sole().span()
			end = emptyListValue(ast.Span{Start: minus.Start, End: minus.Start + 1})
		} else {
			end = tc.transformExpression(alt)
		}
	}
	if end == nil {
		end = emptyListValue(sentinelSpan)
	}
	children := []ast.Expr{base, start, end}
	if stepBound, ok := bound.child(2).opt(); ok {
		// StepSliceBound <- ':' Expression?
		if sv, ok := stepBound.child(1).opt(); ok {
			children = append(children, tc.transformExpression(sv))
		}
	}
	return opExpr(sp, ast.ArraySlice, children...)
}
