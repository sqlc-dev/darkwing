// Package ast defines darkwing's typed syntax tree for the DuckDB SQL
// dialect. Node names follow DuckDB's parsed statement and expression
// classes (SelectStatement, ColumnRefExpression, ...) so the transformer
// and serializer read side-by-side with upstream's transformer/*.cpp;
// sqlc's future convert.go owns the mapping to sqlc's own sql/ast.
package ast

// Span is a half-open byte range [Start, End) into the original SQL text.
//
// Statement spans tile the input: sqlc slices the source by statement span
// to find `-- name:` comments. Expression spans mirror DuckDB's
// query_location bookkeeping (the position upstream reports in errors and
// json_serialize_sql), which for some nodes is the operator token rather
// than the full extent of the expression.
type Span struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// Node is implemented by every syntax tree node.
type Node interface {
	// Pos returns the byte offset where the node begins.
	Pos() int
	// End returns the byte offset just past the node.
	End() int
	// Children returns the node's direct children, for generic walks.
	Children() []Node
}

// spanned supplies the Span field plus the Pos/End half of Node; every
// concrete node embeds it.
type spanned struct {
	Span Span `json:"span"`
}

func (s *spanned) Pos() int { return s.Span.Start }
func (s *spanned) End() int { return s.Span.End }

// SetSpan overwrites the node's span; used by the transformer.
func (s *spanned) SetSpan(sp Span) { s.Span = sp }

// Stmt is implemented by all statement nodes.
type Stmt interface {
	Node
	stmtNode()
}

// Expr is implemented by all expression nodes (DuckDB's ParsedExpression
// hierarchy).
type Expr interface {
	Node
	exprNode()
	// GetAlias and SetAlias expose the alias every DuckDB expression
	// carries (e.g. `expr AS name` in a select list).
	GetAlias() string
	SetAlias(string)
}

// TableRef is implemented by all table reference nodes.
type TableRef interface {
	Node
	tableRefNode()
}

// QueryNode is a node of the query tree wrapped by SelectStatement:
// SelectNode, SetOperationNode, RecursiveCTENode or CTENode — DuckDB's
// shape.
type QueryNode interface {
	Node
	queryNode()
	// ModifiersRef exposes the node's result modifier list (ORDER BY,
	// LIMIT, ...) for the transformer to append to.
	ModifiersRef() *[]ResultModifier
	// CTEMapRef exposes the node's CTE map for the transformer.
	CTEMapRef() *CTEMap
}

// ResultModifier is a modifier applied to a query node's result: ORDER BY,
// LIMIT, DISTINCT, LIMIT PERCENT.
type ResultModifier interface {
	Node
	resultModifierNode()
}

// exprs converts a []Expr to []Node, skipping nils. The transformer
// stores untyped nils in absent interface fields, so a plain nil check
// suffices.
func exprs(list []Expr) []Node {
	nodes := make([]Node, 0, len(list))
	for _, e := range list {
		if e != nil {
			nodes = append(nodes, e)
		}
	}
	return nodes
}

// add appends non-nil nodes. Callers pass interface fields directly; the
// transformer never stores typed nils, so a plain nil check suffices.
func add(nodes []Node, extra ...Node) []Node {
	for _, n := range extra {
		if n != nil {
			nodes = append(nodes, n)
		}
	}
	return nodes
}
