// tree.go: navigation helpers over the matcher's generic parse tree.
// Transformer functions index into ParseResult nodes by grammar position,
// exactly as upstream's transformer does; a shape mismatch means the
// transformer disagrees with the vendored grammar and is reported as an
// internal error (panic caught at the Parse boundary).
package parser

import (
	"fmt"

	"github.com/sqlc-dev/darkwing/ast"
	"github.com/sqlc-dev/darkwing/internal/matcher"
)

// tnode wraps one ParseResult for ergonomic navigation.
type tnode struct {
	r matcher.ParseResult
}

// shapeError reports a transformer/grammar shape disagreement. It panics;
// Parse converts it into an internal error naming the rule.
func shapeError(n tnode, format string, args ...any) {
	name := ""
	if n.r != nil {
		name = n.r.Name()
	}
	panic(&internalError{msg: fmt.Sprintf("rule %q: %s", name, fmt.Sprintf(format, args...))})
}

type internalError struct {
	msg string
}

func (e *internalError) Error() string {
	return "darkwing internal error (transformer/grammar mismatch): " + e.msg
}

func (n tnode) valid() bool { return n.r != nil }

func (n tnode) name() string {
	if n.r == nil {
		return ""
	}
	return n.r.Name()
}

// span returns the source byte range the node covers.
func (n tnode) span() ast.Span {
	if n.r == nil {
		return ast.Span{}
	}
	off, length, ok := n.r.Loc()
	if !ok {
		return ast.Span{}
	}
	return ast.Span{Start: off, End: off + length}
}

// list asserts the node is a ListResult.
func (n tnode) list() *matcher.ListResult {
	l, ok := n.r.(*matcher.ListResult)
	if !ok {
		shapeError(n, "want list, got %T", n.r)
	}
	return l
}

// count returns the number of children of a list node.
func (n tnode) count() int { return len(n.list().Children) }

// child returns the i-th child of a list node.
func (n tnode) child(i int) tnode {
	l := n.list()
	if i < 0 || i >= len(l.Children) {
		shapeError(n, "child %d out of range (%d children)", i, len(l.Children))
	}
	return tnode{l.Children[i]}
}

// sole unwraps a single-child list node (`Rule <- Other`).
func (n tnode) sole() tnode {
	l := n.list()
	if len(l.Children) != 1 {
		shapeError(n, "want single child, got %d", len(l.Children))
	}
	return tnode{l.Children[0]}
}

// choice unwraps a ChoiceResult into (alternative index, inner node).
func (n tnode) choice() (int, tnode) {
	c, ok := n.r.(*matcher.ChoiceResult)
	if !ok {
		shapeError(n, "want choice, got %T", n.r)
	}
	return c.Index, tnode{c.Result}
}

// opt unwraps an OptionalResult; ok is false when it matched empty. A
// non-optional node passes through unchanged (repetition bodies collapse
// the optional in some positions).
func (n tnode) opt() (tnode, bool) {
	o, ok := n.r.(*matcher.OptionalResult)
	if !ok {
		return n, true
	}
	if o.Result == nil {
		return tnode{}, false
	}
	return tnode{o.Result}, true
}

// repeat returns the items of a `*` or `+` repetition: an optional
// wrapping a RepeatResult (for `*`), a bare RepeatResult (for `+`), or
// empty.
func (n tnode) repeat() []tnode {
	inner, ok := n.opt()
	if !ok {
		return nil
	}
	rep, isRep := inner.r.(*matcher.RepeatResult)
	if !isRep {
		shapeError(n, "want repeat, got %T", inner.r)
	}
	items := make([]tnode, len(rep.Children))
	for i, c := range rep.Children {
		items[i] = tnode{c}
	}
	return items
}

// macroNode descends single-child rule wrappers until it reaches the
// named macro instance node ("List" or "Parens").
func (n tnode) macroNode(macro string) tnode {
	cur := n
	for {
		if cur.name() == macro {
			return cur
		}
		l, ok := cur.r.(*matcher.ListResult)
		if !ok || len(l.Children) != 1 {
			return cur
		}
		cur = tnode{l.Children[0]}
	}
}

// parens unwraps the Parens(D) macro: LIST['(', D, ')'].
func (n tnode) parens() tnode {
	inner := n.macroNode("Parens")
	l := inner.list()
	if len(l.Children) != 3 {
		shapeError(n, "want Parens shape, got %d children", len(l.Children))
	}
	return tnode{l.Children[1]}
}

// listElems unwraps the List(D) macro — LIST[D, (',' D)*, ','?] — into
// its elements.
func (n tnode) listElems() []tnode {
	inner := n.macroNode("List")
	l := inner.list()
	if len(l.Children) != 3 {
		shapeError(n, "want List macro shape, got %d children", len(l.Children))
	}
	elems := []tnode{{l.Children[0]}}
	for _, item := range (tnode{l.Children[1]}).repeat() {
		elems = append(elems, item.child(1))
	}
	return elems
}

// ident returns the text of an IdentifierResult.
func (n tnode) ident() string {
	id, ok := n.r.(*matcher.IdentifierResult)
	if !ok {
		shapeError(n, "want identifier, got %T", n.r)
	}
	return id.Identifier
}

// keyword returns the raw spelling of a KeywordResult.
func (n tnode) keyword() string {
	k, ok := n.r.(*matcher.KeywordResult)
	if !ok {
		shapeError(n, "want keyword, got %T", n.r)
	}
	return k.Keyword
}

// number returns the raw text of a NumberResult.
func (n tnode) number() string {
	num, ok := n.r.(*matcher.NumberResult)
	if !ok {
		shapeError(n, "want number, got %T", n.r)
	}
	return num.Number
}

// stringLit returns the unescaped value and kind of a StringResult.
func (n tnode) stringLit() (string, matcher.StringKind) {
	s, ok := n.r.(*matcher.StringResult)
	if !ok {
		shapeError(n, "want string literal, got %T", n.r)
	}
	return s.Value, s.StringKind
}

// isString reports whether the node is a string literal.
func (n tnode) isString() bool {
	_, ok := n.r.(*matcher.StringResult)
	return ok
}
