// transform_depth.go: the max_expression_depth limit — the port of
// PEGTransformer::ExpressionDepthWeight / EnterExpression and the
// generated expression-depth rule table in peg_transformer_factory.cpp.
// Upstream charges each transformed rule frame a weight while it is on
// the transform stack: NESTING rules weigh 1, CHAIN rules weigh the
// number of links of their operator chain. darkwing computes the deepest
// stack in a pre-pass over the statement's parse tree.
package parser

import "github.com/sqlc-dev/darkwing/internal/matcher"

// maxExpressionDepth is ParserOptions::max_expression_depth's default.
const maxExpressionDepth = 1000

type depthKind uint8

const (
	depthNesting depthKind = iota + 1
	depthChain
)

// expressionDepthRules is the START/END GENERATED EXPRESSION DEPTH RULES
// table.
var expressionDepthRules = map[string]depthKind{
	"SingleExpression":             depthNesting,
	"Type":                         depthNesting,
	"NestedType":                   depthNesting,
	"LambdaArrowExpression":        depthChain,
	"NotExpression":                depthChain,
	"IsExpressionContinuation":     depthChain,
	"IsDistinctFromExpression":     depthChain,
	"ComparisonExpression":         depthChain,
	"BetweenInLikeExpression":      depthChain,
	"InfixOtherOperatorExpression": depthChain,
	"BitwiseExpression":            depthChain,
	"TildeExpression":              depthChain,
	"AdditiveExpression":           depthChain,
	"MultiplicativeExpression":     depthChain,
	"ExponentiationExpression":     depthChain,
	"CollateExpression":            depthChain,
	"AtTimeZoneExpression":         depthChain,
	"PrefixExpression":             depthChain,
	"IndirectionList":              depthChain,
}

// checkExpressionDepth raises upstream's depth error when the deepest
// chain of weighted rule frames exceeds the limit.
func checkExpressionDepth(r matcher.ParseResult) {
	if maxDepthWeight(r, 0) > maxExpressionDepth {
		raise("Max expression depth limit of %d exceeded. Use \"SET max_expression_depth TO x\" to "+
			"increase the maximum expression depth.", maxExpressionDepth)
	}
}

// stackCheck ports PEGTransformer::StackCheck(extra) for the folds that
// call it with their operator count (ApplyNotKeywords,
// TransformTildeExpression): unlike the frame weights it uses >=.
func stackCheck(extra int) {
	if extra >= maxExpressionDepth {
		raise("Max expression depth limit of %d exceeded. Use \"SET max_expression_depth TO x\" to "+
			"increase the maximum expression depth.", maxExpressionDepth)
	}
}

// maxDepthWeight returns the largest accumulated weight on any path from
// r down, starting from the weight acc of r's ancestors. It stops early
// once the limit is exceeded.
func maxDepthWeight(r matcher.ParseResult, acc int) int {
	if r == nil || acc > maxExpressionDepth {
		return acc
	}
	// only a rule's own list node is a transform frame (its repetition
	// child can carry the rule's name too)
	if _, isList := r.(*matcher.ListResult); !isList {
		// fall through to the traversal without charging a weight
	} else if kind, ok := expressionDepthRules[r.Name()]; ok {
		switch kind {
		case depthNesting:
			acc++
		case depthChain:
			acc += chainLinks(r)
		}
	}
	best := acc
	visit := func(c matcher.ParseResult) {
		if d := maxDepthWeight(c, acc); d > best {
			best = d
		}
	}
	switch x := r.(type) {
	case *matcher.ListResult:
		for _, c := range x.Children {
			visit(c)
		}
	case *matcher.RepeatResult:
		for _, c := range x.Children {
			visit(c)
		}
	case *matcher.OptionalResult:
		visit(x.Result)
	case *matcher.ChoiceResult:
		visit(x.Result)
	}
	return best
}

// chainLinks ports CountChainLinks: the repeated elements directly in a
// rule's match.
func chainLinks(r matcher.ParseResult) int {
	switch x := r.(type) {
	case *matcher.RepeatResult:
		return len(x.Children)
	case *matcher.OptionalResult:
		if x.Result == nil {
			return 0
		}
		return chainLinks(x.Result)
	case *matcher.ListResult:
		links := 0
		for _, c := range x.Children {
			switch c.(type) {
			case *matcher.RepeatResult, *matcher.OptionalResult:
				links += chainLinks(c)
			}
		}
		return links
	}
	return 0
}
