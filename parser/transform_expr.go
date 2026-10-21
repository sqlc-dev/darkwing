// transform_expr.go: expression transformers — the port of upstream's
// transformer/expression/*.cpp. One function per rule of expression.gram;
// the precedence ladder rules fold their tail lists exactly as upstream
// does (conjunction flattening, constant folding of negated literals,
// operator calls as FunctionExpressions).
package parser

import (
	"math/big"
	"strconv"
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

// constExpr builds the constant for a value the transformer synthesizes
// (ConstantExpression::FromValue): its literal is the atom that re-binds
// to the value.
func constExpr(sp ast.Span, v ast.Value) *ast.ConstantExpression {
	c := &ast.ConstantExpression{Value: v, Literal: literalFromValue(v)}
	c.SetSpan(sp)
	return c
}

// literalExpr builds the constant for a literal atom; its Value is
// derived (Literal::ToValue).
func literalExpr(sp ast.Span, l ast.Literal) *ast.ConstantExpression {
	c := &ast.ConstantExpression{Literal: l, Value: literalValue(l)}
	c.SetSpan(sp)
	return c
}

// nullConst is ConstantExpression::Null().
func nullConst(sp ast.Span) *ast.ConstantExpression {
	return literalExpr(sp, ast.Literal{Kind: ast.LiteralNull})
}

// boolConst is ConstantExpression::Boolean().
func boolConst(sp ast.Span, b bool) *ast.ConstantExpression {
	return literalExpr(sp, ast.Literal{Kind: ast.LiteralBoolean, Text: strconv.FormatBool(b)})
}

// intConst is ConstantExpression::Integer().
func intConst(sp ast.Span, v int64) *ast.ConstantExpression {
	return literalExpr(sp, ast.Literal{Kind: ast.LiteralInteger, Text: strconv.FormatInt(v, 10)})
}

// stringConst is ConstantExpression::String().
func stringConst(sp ast.Span, s string) *ast.ConstantExpression {
	return literalExpr(sp, ast.Literal{Kind: ast.LiteralString, Text: s})
}

// literalFromValue picks the literal atom for the scalar values the
// transformers synthesize (the literal branch of
// ConstantExpression::FromValue).
func literalFromValue(v ast.Value) ast.Literal {
	if v.IsNull || v.Kind == ast.ValueNull {
		return ast.Literal{Kind: ast.LiteralNull}
	}
	switch v.Kind {
	case ast.ValueBool:
		return ast.Literal{Kind: ast.LiteralBoolean, Text: strconv.FormatBool(v.Bool)}
	case ast.ValueInt64, ast.ValueHugeint:
		s := valueString(v)
		if v.Type.ID == "DECIMAL" {
			return ast.Literal{Kind: ast.LiteralNumeric, Text: s}
		}
		return ast.Literal{Kind: ast.LiteralInteger, Text: s}
	case ast.ValueDouble:
		return ast.Literal{Kind: ast.LiteralNumeric, Text: valueString(v)}
	}
	return ast.Literal{Kind: ast.LiteralString, Text: v.Str}
}

// literalValue ports Literal::ToValue for the transformers' own use.
func literalValue(l ast.Literal) ast.Value {
	switch l.Kind {
	case ast.LiteralBoolean:
		return ast.Value{Type: ast.LogicalType{ID: "BOOLEAN"}, Kind: ast.ValueBool, Bool: l.Text == "true"}
	case ast.LiteralInteger, ast.LiteralNumeric:
		if v, ok := numberValue(l.Text); ok {
			return v
		}
	case ast.LiteralString:
		return varcharValue(l.Text)
	case ast.LiteralHex:
		blob, _ := hexToBlob(l.Text)
		return ast.Value{Type: ast.LogicalType{ID: "BLOB"}, Kind: ast.ValueString, Str: blob}
	case ast.LiteralBit:
		return ast.Value{Type: ast.LogicalType{ID: "BIT"}, Kind: ast.ValueString, Str: l.Text}
	}
	return ast.Value{Type: ast.LogicalType{ID: "NULL"}, IsNull: true, Kind: ast.ValueNull}
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
		expr = applyNotKeywords(notNode, expr, n.span().End)
	}
	return expr
}

// applyNotKeywords ports ApplyNotKeywords: NotExpression <- NotKeyword+
// wraps the operand in one NOT per keyword, each spanning its keyword
// through end.
func applyNotKeywords(notNode tnode, expr ast.Expr, end int) ast.Expr {
	nots := notNode.sole().repeat()
	for i := len(nots) - 1; i >= 0; i-- {
		expr = notExpr(ast.Span{Start: nots[i].sole().span().Start, End: end}, expr)
	}
	return expr
}

// isTail is one IsExpressionTail of upstream's IsExpressionContinuation,
// tagged with its ExpressionTailType.
type isTail struct {
	kind string // IsTestTail / IsDistinctTail / IsComparisonTail / IsOtherOperatorTail / IsCastTail
	node tnode  // the tail's inner node
}

// IsExpression <- IsDistinctFromExpression IsExpressionContinuation?
// IsExpressionContinuation <- IsTest IsExpressionTail*
// Port of TransformIsExpression: after the first IS test, further IS
// tests, IS DISTINCT FROM, comparisons, other operators and casts fold
// left to right; two consecutive DISTINCT or comparison tails are the
// chained-comparison error. Synthesized nodes carry no location except
// the outermost, which spans the whole expression.
func (tc *transformContext) transformIs(n tnode) ast.Expr {
	expr := tc.transformIsDistinctFrom(n.child(0))
	cont, ok := n.child(1).opt()
	if !ok {
		return expr
	}
	tails := []isTail{{kind: "IsTestTail", node: cont.child(0)}}
	for _, t := range cont.child(1).repeat() {
		_, alt := t.sole().choice()
		tails = append(tails, isTail{kind: alt.name(), node: alt.sole()})
	}
	// upstream transforms every tail before folding them, so operand
	// errors precede the chained-comparison check
	type folded struct {
		kind string
		fold func(ast.Expr) ast.Expr
	}
	folds := make([]folded, len(tails))
	for i, t := range tails {
		t := t
		switch t.kind {
		case "IsTestTail":
			folds[i] = folded{t.kind, func(e ast.Expr) ast.Expr { return tc.transformIsTest(e, t.node) }}
		case "IsDistinctTail":
			t2, right := tc.isDistinctFromTail(t.node)
			folds[i] = folded{t.kind, func(e ast.Expr) ast.Expr {
				cmp := &ast.ComparisonExpression{Type: t2, Left: e, Right: right}
				cmp.SetSpan(invalidSpan)
				return cmp
			}}
		case "IsComparisonTail":
			t2, right := tc.comparisonTail(t.node)
			folds[i] = folded{t.kind, func(e ast.Expr) ast.Expr {
				cmp := &ast.ComparisonExpression{Type: t2, Left: e, Right: right}
				cmp.SetSpan(invalidSpan)
				return cmp
			}}
		case "IsOtherOperatorTail":
			fold := tc.otherOperatorTail(t.node)
			folds[i] = folded{t.kind, func(e ast.Expr) ast.Expr { return fold(e, -1, false) }}
		case "IsCastTail":
			typ := tc.transformType(t.node.child(1))
			sp := t.node.span()
			folds[i] = folded{t.kind, func(e ast.Expr) ast.Expr {
				cast := &ast.CastExpression{Child: e, CastType: typ}
				cast.SetSpan(sp)
				return cast
			}}
		default:
			shapeError(t.node, "unknown IsExpressionTail %q", t.kind)
		}
	}
	prev := "IsTestTail"
	for _, f := range folds {
		if f.kind == prev && (f.kind == "IsDistinctTail" || f.kind == "IsComparisonTail") {
			raise("Chained comparisons are not supported, use AND to combine comparisons")
		}
		expr = f.fold(expr)
		prev = f.kind
	}
	// the rule's own location lands on the result when it has none
	if expr.Pos() < 0 {
		setSpan(expr, n.span())
	}
	return expr
}

// setSpan sets an expression's location (all AST expressions embed
// exprBase).
func setSpan(e ast.Expr, sp ast.Span) {
	if s, ok := e.(interface{ SetSpan(ast.Span) }); ok {
		s.SetSpan(sp)
	}
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
		t, right := tc.isDistinctFromTail(tail)
		cmp := &ast.ComparisonExpression{Type: t, Left: expr, Right: right}
		cmp.SetSpan(ast.Span{Start: n.span().Start, End: tail.span().End})
		expr = cmp
	}
	return expr
}

// IsDistinctFromTail <- IsDistinctFromOp ComparisonExpression
func (tc *transformContext) isDistinctFromTail(tail tnode) (ast.ExpressionType, ast.Expr) {
	op := tail.child(0) // 'IS' 'NOT'? 'DISTINCT' 'FROM'
	_, negated := op.child(1).opt()
	t := ast.CompareDistinctFrom
	if negated {
		t = ast.CompareNotDistinctFrom
	}
	return t, tc.transformComparison(tail.child(1))
}

// ComparisonExpressionTail <- ComparisonOperator ComparisonOperand
func (tc *transformContext) comparisonTail(tail tnode) (ast.ExpressionType, ast.Expr) {
	_, opAlt := tail.child(0).sole().choice()
	t, ok := comparisonOps[opAlt.name()]
	if !ok {
		shapeError(opAlt, "unknown comparison operator")
	}
	return t, tc.transformComparisonOperand(tail.child(1))
}

// ComparisonOperand <- NotComparisonOperand / BetweenInLikeExpression
// NotComparisonOperand <- NotExpression IsExpression: NOT after a
// comparison operator binds everything up to the IS level.
func (tc *transformContext) transformComparisonOperand(n tnode) ast.Expr {
	_, alt := n.sole().choice()
	if alt.name() == "NotComparisonOperand" {
		return applyNotKeywords(alt.child(0), tc.transformIs(alt.child(1)), alt.span().End)
	}
	return tc.transformBetweenInLike(alt)
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
		// upstream bans chained comparisons in the transformer, but only
		// after transforming the operands - a deeper operand error (e.g.
		// an empty subscript) takes precedence over the chain ban
		for _, tail := range tails {
			tc.comparisonTail(tail)
		}
		raise("Chained comparisons are not supported, use AND to combine comparisons")
	}
	t, right := tc.comparisonTail(tails[0])
	cmp := &ast.ComparisonExpression{Type: t, Left: expr, Right: right}
	cmp.SetSpan(n.span())
	return cmp
}

// BetweenInLikeExpression <- OtherOperatorExpression InPredicate* BetweenLikeOp?
// InPredicate <- 'NOT'? InClause CastOperator*
// BetweenLikeOp <- 'NOT'? BetweenLikeOpExpression
// Port of TransformBetweenInLikeExpression: the IN predicates (each with
// its trailing casts) apply left to right, then the BETWEEN/LIKE.
// Synthesized NOT wrappers carry no location except as the outermost
// result, which takes the whole expression's.
func (tc *transformContext) transformBetweenInLike(n tnode) ast.Expr {
	expr := tc.transformOtherOperator(n.child(0))
	preds := n.child(1).repeat()
	opNode, hasOp := n.child(2).opt()
	if len(preds) == 0 && !hasOp {
		return expr
	}
	whole := n.span()
	for _, p := range preds {
		_, negated := p.child(0).opt()
		expr = tc.transformInClause(expr, p.child(1))
		if negated {
			expr = notExpr(invalidSpan, expr)
		}
		for _, c := range p.child(2).repeat() {
			cast := &ast.CastExpression{Child: expr, CastType: tc.transformType(c.child(1))}
			cast.SetSpan(c.span())
			expr = cast
		}
	}
	if hasOp {
		_, negated := opNode.child(0).opt()
		_, alt := opNode.child(1).sole().choice()
		switch alt.name() {
		case "BetweenClause":
			// 'BETWEEN' Asymmetric? OtherOperatorExpression 'AND' OtherOperatorExpression
			lower := tc.transformOtherOperator(alt.child(2))
			upper := tc.transformOtherOperator(alt.child(4))
			b := &ast.BetweenExpression{Input: expr, Lower: lower, Upper: upper}
			b.SetSpan(alt.span())
			expr = b
			if negated {
				expr = notExpr(invalidSpan, expr)
			}
		case "LikeClause":
			_, variation := alt.child(0).sole().choice()
			// like handles its own negation
			expr = tc.transformLikeClause(expr, alt, negated, invalidSpan)
			if negated && strings.HasPrefix(variation.name(), "Not") {
				// TransformBetweenLikeOp: the variation's function is
				// already negated ("!~~", "!~~*", "!regexp_matches")
				raise("NOT cannot precede an operator that is already negated (!~~, !~~*, !~ or !~*)")
			}
		default:
			shapeError(alt, "unknown BetweenLikeOpExpression")
		}
	}
	if expr.Pos() < 0 {
		setSpan(expr, whole)
	}
	return expr
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

// OtherOperatorExpression <- InfixOtherOperatorExpression / CustomPrefixExpression
func (tc *transformContext) transformOtherOperator(n tnode) ast.Expr {
	_, alt := n.sole().choice()
	switch alt.name() {
	case "InfixOtherOperatorExpression":
		return tc.transformInfixOtherOperator(alt)
	case "CustomPrefixExpression":
		// CustomPrefixExpression <- OperatorLiteral OtherOperatorExpression
		operand := tc.transformOtherOperator(alt.child(1))
		return operatorFunction(alt.span(), keywordOfOp(alt.child(0)), operand)
	}
	shapeError(alt, "unknown OtherOperatorExpression alternative")
	return nil
}

// InfixOtherOperatorExpression <- BitwiseExpression OtherOperatorTail*
func (tc *transformContext) transformInfixOtherOperator(n tnode) ast.Expr {
	expr := tc.transformBitwise(n.child(0))
	tails := n.child(1).repeat()
	folds := make([]func(ast.Expr, int, bool) ast.Expr, len(tails))
	for i, tail := range tails {
		folds[i] = tc.otherOperatorTail(tail)
	}
	for i, fold := range folds {
		expr = fold(expr, n.span().Start, i == len(tails)-1)
	}
	return expr
}

// otherOperatorTail transforms OtherOperatorTail <- OtherOperator
// BitwiseExpression and returns the fold applying it to a left operand.
// exprStart < 0 means the fold's result gets no location of its own.
func (tc *transformContext) otherOperatorTail(tail tnode) func(ast.Expr, int, bool) ast.Expr {
	_, opAlt := tail.child(0).sole().choice()
	rhsNode := tail.child(1)
	rhs := tc.transformBitwise(rhsNode)
	end := rhsNode.span().End
	span := func(exprStart int, outermost bool) ast.Span {
		if exprStart < 0 || !outermost {
			return invalidSpan
		}
		return ast.Span{Start: exprStart, End: end}
	}
	switch opAlt.name() {
	case "AnyAllParsedOperator":
		return func(input ast.Expr, exprStart int, outermost bool) ast.Expr {
			whole := ast.Span{Start: exprStart, End: end}
			if exprStart < 0 {
				whole = invalidSpan
			}
			return tc.transformAnyAll(input, opAlt.sole(), rhs, whole)
		}
	case "NamedOtherOperator":
		return func(input ast.Expr, exprStart int, outermost bool) ast.Expr {
			return tc.transformNamedOperator(input, opAlt.sole(), rhs, span(exprStart, outermost))
		}
	}
	shapeError(opAlt, "unknown other-operator")
	return nil
}

// operatorFunction ports TransformOperatorFunction: an operator call,
// "schema.op" splitting into a schema-qualified call.
func operatorFunction(sp ast.Span, name string, args ...ast.Expr) *ast.FunctionExpression {
	parts := strings.Split(name, ".")
	if len(parts) > 2 {
		raise("Too many identifiers found, expected schema.operator or operator")
	}
	f := opCall(sp, parts[len(parts)-1], args...)
	if len(parts) == 2 {
		f.Schema = parts[0]
	}
	return f
}

// AnyAllOperator <- AnyOperatorLiteral AnyOrAll
func (tc *transformContext) transformAnyAll(input ast.Expr, n tnode, rhs ast.Expr, whole ast.Span) ast.Expr {
	opText := keywordOfOp(n.child(0))
	_, anyOrAll := n.child(1).sole().choice()
	isAll := anyOrAll.name() == "SubqueryAll"
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

// NamedOtherOperator <- QualifiedOperator / OperatorLiteral
// Only the outermost fold of a chain carries a location, like the binary
// ladder levels.
func (tc *transformContext) transformNamedOperator(input ast.Expr, n tnode, rhs ast.Expr, sp ast.Span) ast.Expr {
	_, alt := n.choice()
	if alt.name() == "QualifiedOperator" {
		// OPERATOR(schema.op) is always a function call — comparison
		// spellings included — with the qualifier as its schema
		schema, opText := qualifiedOperatorParts(alt)
		f := opCall(sp, opText, input, rhs)
		f.Schema = schema
		return f
	}
	// OperatorLiteral: a free operator token
	op, ok := alt.r.(*matcher.OperatorResult)
	if !ok {
		shapeError(alt, "want operator literal, got %T", alt.r)
	}
	opText := op.Operator
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
	case *matcher.OperatorResult:
		// OperatorLiteral / AnyOperatorLiteral
		return r.Operator
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

// BitwiseExpression <- TildeExpression BitwiseExpressionTail*
func (tc *transformContext) transformBitwise(n tnode) ast.Expr {
	return tc.binaryLadder(n, tc.transformTilde, false)
}

// TildeExpression <- TildePrefixOperator* AdditiveExpression
// Port of TransformTildeExpression: bitwise NOT binds looser than
// arithmetic (~a + b is ~(a + b)); only the outermost operator carries
// a location, the whole expression's.
func (tc *transformContext) transformTilde(n tnode) ast.Expr {
	expr := tc.transformAdditive(n.child(1))
	ops := n.child(0).repeat()
	for i := len(ops) - 1; i >= 0; i-- {
		sp := invalidSpan
		if i == 0 {
			sp = n.span()
		}
		expr = operatorFunction(sp, "~", expr)
	}
	return expr
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
		collation := tc.collationName(rhs)
		c := &ast.CollateExpression{Child: expr, Collation: collation}
		c.SetSpan(ast.Span{Start: n.span().Start, End: tail.span().End})
		expr = c
	}
	return expr
}

// collationName extracts the collation name from the right-hand
// expression of COLLATE: an identifier chain joined with '.', or a
// constant rendered with Value::ToString semantics ('x' COLLATE 'nocase'
// and even COLLATE 3 parse; the name fails lookup at bind time). For any
// other expression upstream throws NotImplementedException ("Unexpected
// expression encountered for collate, ...") — a post-parse error under
// the oracle's classification — so darkwing accepts and records the
// source spelling.
func (tc *transformContext) collationName(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.ColumnRefExpression:
		return strings.Join(v.ColumnNames, ".")
	case *ast.ConstantExpression:
		return valueString(v.Value)
	}
	return tc.exprSourceText(e)
}

// valueString ports Value::ToString for the constants the transformer
// produces.
func valueString(v ast.Value) string {
	if v.IsNull {
		return "NULL"
	}
	switch v.Kind {
	case ast.ValueBool:
		if v.Bool {
			return "true"
		}
		return "false"
	case ast.ValueInt64:
		if v.Type.ID == "DECIMAL" {
			return decimalValueString(strconv.FormatInt(v.Int64, 10), v.Type.Scale)
		}
		return strconv.FormatInt(v.Int64, 10)
	case ast.ValueHugeint:
		s := hugeintString(v.Hugeint)
		if v.Type.ID == "DECIMAL" {
			return decimalValueString(s, v.Type.Scale)
		}
		return s
	case ast.ValueDouble:
		return strconv.FormatFloat(v.Float64, 'g', -1, 64)
	case ast.ValueString:
		return v.Str
	}
	return ""
}

// hugeintString renders the 128-bit integer in decimal.
func hugeintString(h ast.Hugeint) string {
	n := new(big.Int).SetInt64(h.Upper)
	n.Lsh(n, 64)
	n.Add(n, new(big.Int).SetUint64(h.Lower))
	return n.String()
}

// decimalValueString inserts the decimal point into an unscaled integer
// rendering, Decimal::ToString-style ("35" scale 1 -> "3.5", "5" scale 2
// -> "0.05").
func decimalValueString(digits string, scale int) string {
	if scale <= 0 {
		return digits
	}
	sign := ""
	if strings.HasPrefix(digits, "-") {
		sign, digits = "-", digits[1:]
	}
	for len(digits) <= scale {
		digits = "0" + digits
	}
	cut := len(digits) - scale
	return sign + digits[:cut] + "." + digits[cut:]
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
			// fold the sign into the number text so
			// -9223372036854775808 stays exact (Literal::Negate)
			if c, ok := expr.(*ast.ConstantExpression); ok && c.Literal.IsNumeric() {
				expr = literalExpr(sp, c.Literal.Negate())
				continue
			}
			expr = opCall(sp, "-", expr)
		case "PlusPrefixOperator":
			expr = opCall(sp, "+", expr)
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
		// a method-chained call names a function too, so it takes the
		// same rewrites (TransformBaseExpression); the rewritten node has
		// no location unless it is the base expression's result
		sp := invalidSpan
		if last {
			sp = ast.Span{Start: baseStart, End: alt.span().End}
		}
		if out := parserFunctionRewrite(sp, f.FunctionName, f.Arguments, true); out != nil {
			upper := strings.ToUpper(f.FunctionName)
			if f.Distinct {
				raise("DISTINCT is not supported in %s expressions", upper)
			}
			if len(f.OrderBys) > 0 {
				raise("ORDER BY is not supported in %s expressions", upper)
			}
			return out, false
		}
		return f, false
	}
	shapeError(alt, "unknown dot operator")
	return nil, false
}

// emptySliceBound ports OperatorExpression::EmptySliceBound: an omitted
// slice bound is the call list_value() (v2.0; formerly an empty
// INTEGER[] constant).
func emptySliceBound(sp ast.Span) *ast.FunctionExpression {
	return fnCall(sp, "list_value")
}

// SliceExpression <- '[' SliceBound ']'
// SliceBound <- Expression? SliceUpperBound?
// SliceUpperBound <- MinusStepSliceBound / EndStepSliceBound
func (tc *transformContext) transformSlice(base ast.Expr, n tnode) ast.Expr {
	bound := n.child(1)
	// the slice operator's location is the bracketed subscript itself
	sp := n.span()
	var start ast.Expr
	if s, ok := bound.child(0).opt(); ok {
		start = tc.transformExpression(s)
	}
	upper, hasUpper := bound.child(1).opt()
	if !hasUpper {
		if start == nil {
			raise("Empty subscript '[]' is not allowed")
		}
		return opExpr(sp, ast.ArrayExtract, base, start)
	}
	// an omitted start bound has no location
	if start == nil {
		start = emptySliceBound(invalidSpan)
	}
	children := []ast.Expr{base, start}
	_, alt := upper.sole().choice()
	switch alt.name() {
	case "MinusStepSliceBound":
		// ':' '-' StepSliceBound: an explicit open end, location-free
		children = append(children, emptySliceBound(invalidSpan), tc.sliceBoundValue(alt.child(2)))
	case "EndStepSliceBound":
		// EndSliceBound StepSliceBound?
		children = append(children, tc.sliceBoundValue(alt.child(0)))
		if step, ok := alt.child(1).opt(); ok {
			children = append(children, tc.sliceBoundValue(step))
		}
	default:
		shapeError(alt, "unknown SliceUpperBound")
	}
	return opExpr(sp, ast.ArraySlice, children...)
}

// sliceBoundValue transforms EndSliceBound / StepSliceBound <- ':'
// Expression?: an omitted expression is the empty slice bound, located
// on the ':'.
func (tc *transformContext) sliceBoundValue(n tnode) ast.Expr {
	if e, ok := n.child(1).opt(); ok {
		return tc.transformExpression(e)
	}
	return emptySliceBound(n.span())
}
