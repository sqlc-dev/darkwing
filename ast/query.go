package ast

// CTEMaterialize mirrors DuckDB's CTEMaterialize spelling.
type CTEMaterialize string

const (
	CTEMaterializeDefault CTEMaterialize = "CTE_MATERIALIZE_DEFAULT"
	CTEMaterializeAlways  CTEMaterialize = "CTE_MATERIALIZE_ALWAYS"
	CTEMaterializeNever   CTEMaterialize = "CTE_MATERIALIZE_NEVER"
)

// CommonTableExpr is one WITH entry. Port of CommonTableExpressionInfo.
type CommonTableExpr struct {
	spanned
	// Aliases are the parenthesized column aliases (`WITH t(a, b) AS ...`).
	Aliases      []string       `json:"aliases,omitempty"`
	Materialized CTEMaterialize `json:"materialized"`
	// KeyTargets are the USING KEY expressions.
	KeyTargets []Expr    `json:"key_targets,omitempty"`
	Query      QueryNode `json:"query_node"`
}

func (c *CommonTableExpr) Children() []Node {
	return add(exprs(c.KeyTargets), c.Query)
}

// CTEMapEntry is one named entry of a CTE map, in source order.
type CTEMapEntry struct {
	Name string           `json:"key"`
	CTE  *CommonTableExpr `json:"value"`
}

// CTEMap is the ordered WITH-clause map carried by every query node.
type CTEMap struct {
	Entries []CTEMapEntry `json:"map,omitempty"`
}

// InsertQueryNode wraps an INSERT statement used as a CTE body
// (upstream's INSERT_QUERY_NODE): DML with RETURNING feeding a WITH
// entry. The statement's own WITH clause moves onto the node's CTE map.
type InsertQueryNode struct {
	queryNodeBase
	Insert *InsertStatement `json:"insert"`
}

func (n *InsertQueryNode) Children() []Node { return add(n.baseChildren(), n.Insert) }

// UpdateQueryNode wraps an UPDATE statement used as a CTE body
// (upstream's UPDATE_QUERY_NODE).
type UpdateQueryNode struct {
	queryNodeBase
	Update *UpdateStatement `json:"update"`
}

func (n *UpdateQueryNode) Children() []Node { return add(n.baseChildren(), n.Update) }

// DeleteQueryNode wraps a DELETE statement used as a CTE body
// (upstream's DELETE_QUERY_NODE). TRUNCATE in a CTE lands here too,
// mirroring upstream's truncate-as-delete representation.
type DeleteQueryNode struct {
	queryNodeBase
	Delete *DeleteStatement `json:"delete"`
}

func (n *DeleteQueryNode) Children() []Node { return add(n.baseChildren(), n.Delete) }

// queryNodeBase carries what every DuckDB QueryNode has: result modifiers
// and a CTE map.
type queryNodeBase struct {
	spanned
	Modifiers []ResultModifier `json:"modifiers,omitempty"`
	CTEs      CTEMap           `json:"cte_map,omitempty"`
}

func (q *queryNodeBase) queryNode()                      {}
func (q *queryNodeBase) ModifiersRef() *[]ResultModifier { return &q.Modifiers }
func (q *queryNodeBase) CTEMapRef() *CTEMap              { return &q.CTEs }

func (q *queryNodeBase) baseChildren() []Node {
	var nodes []Node
	for _, m := range q.Modifiers {
		nodes = add(nodes, m)
	}
	for _, e := range q.CTEs.Entries {
		nodes = add(nodes, e.CTE)
	}
	return nodes
}

// AggregateHandling mirrors DuckDB's AggregateHandling spelling.
type AggregateHandling string

const (
	StandardHandling AggregateHandling = "STANDARD_HANDLING"
	// ForceAggregates is GROUP BY ALL.
	ForceAggregates AggregateHandling = "FORCE_AGGREGATES"
)

// GroupingSet is one grouping set: indexes into the group expression
// list.
type GroupingSet []int

// SelectNode is a plain SELECT ... FROM ... query node. Port of
// SelectNode.
type SelectNode struct {
	queryNodeBase
	SelectList        []Expr            `json:"select_list"`
	FromTable         TableRef          `json:"from_table,omitempty"`
	Where             Expr              `json:"where_clause,omitempty"`
	GroupExpressions  []Expr            `json:"group_expressions,omitempty"`
	GroupSets         []GroupingSet     `json:"group_sets,omitempty"`
	AggregateHandling AggregateHandling `json:"aggregate_handling"`
	Having            Expr              `json:"having,omitempty"`
	Qualify           Expr              `json:"qualify,omitempty"`
	Sample            *SampleOptions    `json:"sample,omitempty"`
}

func (n *SelectNode) Children() []Node {
	nodes := n.baseChildren()
	nodes = append(nodes, exprs(n.SelectList)...)
	nodes = add(nodes, n.FromTable, n.Where)
	nodes = append(nodes, exprs(n.GroupExpressions)...)
	return add(nodes, n.Having, n.Qualify)
}

// SetOperationType mirrors DuckDB's SetOperationType spelling.
type SetOperationType string

const (
	SetOpUnion       SetOperationType = "UNION"
	SetOpExcept      SetOperationType = "EXCEPT"
	SetOpIntersect   SetOperationType = "INTERSECT"
	SetOpUnionByName SetOperationType = "UNION_BY_NAME"
)

// SetOperationNode is a UNION/EXCEPT/INTERSECT node. DuckDB v2's set
// operations are n-ary: same-kind UNION chains flatten into one node
// with many inputs. Port of SetOperationNode.
type SetOperationNode struct {
	queryNodeBase
	SetOpType SetOperationType `json:"setop_type"`
	SetOpAll  bool             `json:"setop_all"`
	Inputs    []QueryNode      `json:"children"`
}

func (n *SetOperationNode) Children() []Node {
	nodes := n.baseChildren()
	for _, c := range n.Inputs {
		nodes = add(nodes, c)
	}
	return nodes
}

// RecursiveCTENode is the body of a recursive CTE: base UNION [ALL]
// recursive part. Port of RecursiveCTENode.
type RecursiveCTENode struct {
	queryNodeBase
	CTEName    string    `json:"cte_name"`
	UnionAll   bool      `json:"union_all"`
	Left       QueryNode `json:"left"`
	Right      QueryNode `json:"right"`
	Aliases    []string  `json:"aliases,omitempty"`
	KeyTargets []Expr    `json:"key_targets,omitempty"`
}

func (n *RecursiveCTENode) Children() []Node {
	return add(add(n.baseChildren(), n.Left, n.Right), exprs(n.KeyTargets)...)
}

// DistinctModifier is SELECT DISTINCT [ON (...)]. Port of
// DistinctModifier.
type DistinctModifier struct {
	spanned
	DistinctOnTargets []Expr `json:"distinct_on_targets,omitempty"`
}

func (m *DistinctModifier) resultModifierNode() {}
func (m *DistinctModifier) Children() []Node    { return exprs(m.DistinctOnTargets) }

// OrderModifier is an ORDER BY list. Port of OrderModifier.
type OrderModifier struct {
	spanned
	Orders []OrderByNode `json:"orders"`
}

func (m *OrderModifier) resultModifierNode() {}
func (m *OrderModifier) Children() []Node {
	var nodes []Node
	for i := range m.Orders {
		nodes = add(nodes, m.Orders[i].Expression)
	}
	return nodes
}

// LimitType distinguishes LIMIT n from LIMIT n%.
type LimitType string

const (
	LimitRowCount   LimitType = "ROW_COUNT"
	LimitPercentage LimitType = "PERCENTAGE"
)

// LimitModifier is LIMIT/OFFSET/FETCH. Port of LimitModifier and
// LimitPercentModifier (folded via LimitType).
type LimitModifier struct {
	spanned
	Limit     Expr      `json:"limit,omitempty"`
	Offset    Expr      `json:"offset,omitempty"`
	LimitType LimitType `json:"limit_type"`
}

func (m *LimitModifier) resultModifierNode() {}
func (m *LimitModifier) Children() []Node    { return add(nil, m.Limit, m.Offset) }

// SampleMethod is a sampling method name, capitalized as DuckDB
// serializes it ("System", "Reservoir", "Bernoulli").
type SampleMethod string

// SampleOptions is a TABLESAMPLE / USING SAMPLE specification. Port of
// SampleOptions.
type SampleOptions struct {
	spanned
	SampleSize   Value        `json:"sample_size"`
	IsPercentage bool         `json:"is_percentage"`
	Method       SampleMethod `json:"method"`
	Seed         int64        `json:"seed"`
	Repeatable   bool         `json:"repeatable"`
}

func (s *SampleOptions) Children() []Node { return nil }
