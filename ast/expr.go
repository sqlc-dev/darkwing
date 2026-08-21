package ast

// ExpressionType mirrors DuckDB's ExpressionType enum, spelled exactly as
// json_serialize_sql serializes it (e.g. COMPARE_EQUAL, OPERATOR_NOT).
type ExpressionType string

const (
	// Comparisons.
	CompareEqual              ExpressionType = "COMPARE_EQUAL"
	CompareNotEqual           ExpressionType = "COMPARE_NOTEQUAL"
	CompareLessThan           ExpressionType = "COMPARE_LESSTHAN"
	CompareGreaterThan        ExpressionType = "COMPARE_GREATERTHAN"
	CompareLessThanOrEqual    ExpressionType = "COMPARE_LESSTHANOREQUALTO"
	CompareGreaterThanOrEqual ExpressionType = "COMPARE_GREATERTHANOREQUALTO"
	CompareDistinctFrom       ExpressionType = "COMPARE_DISTINCT_FROM"
	CompareNotDistinctFrom    ExpressionType = "COMPARE_NOT_DISTINCT_FROM"
	CompareIn                 ExpressionType = "COMPARE_IN"
	CompareNotIn              ExpressionType = "COMPARE_NOT_IN"
	CompareBetween            ExpressionType = "COMPARE_BETWEEN"

	// Conjunctions.
	ConjunctionAnd ExpressionType = "CONJUNCTION_AND"
	ConjunctionOr  ExpressionType = "CONJUNCTION_OR"

	// Operators.
	OperatorNot       ExpressionType = "OPERATOR_NOT"
	OperatorIsNull    ExpressionType = "OPERATOR_IS_NULL"
	OperatorIsNotNull ExpressionType = "OPERATOR_IS_NOT_NULL"
	OperatorCoalesce  ExpressionType = "OPERATOR_COALESCE"
	OperatorTry       ExpressionType = "OPERATOR_TRY"
	OperatorUnpack    ExpressionType = "OPERATOR_UNPACK"
	OperatorCast      ExpressionType = "OPERATOR_CAST"
	ArrayExtract      ExpressionType = "ARRAY_EXTRACT"
	ArraySlice        ExpressionType = "ARRAY_SLICE"
	ArrayConstructor  ExpressionType = "ARRAY_CONSTRUCTOR"
	StructExtract     ExpressionType = "STRUCT_EXTRACT"
	GroupingFunction  ExpressionType = "GROUPING_FUNCTION"

	// Invalid marks an absent comparison (SubqueryExpression on EXISTS /
	// SCALAR subqueries).
	InvalidExpressionType ExpressionType = "INVALID"
)

// exprBase carries the fields every DuckDB ParsedExpression has: the query
// location (as Span) and the alias.
type exprBase struct {
	spanned
	Alias string `json:"alias,omitempty"`
}

func (e *exprBase) exprNode()         {}
func (e *exprBase) GetAlias() string  { return e.Alias }
func (e *exprBase) SetAlias(a string) { e.Alias = a }

// ColumnRefExpression is an unresolved column reference: the dotted parts
// in source order (column, table.column, ...). Port of DuckDB's
// ColumnRefExpression.
type ColumnRefExpression struct {
	exprBase
	ColumnNames []string `json:"column_names"`
}

func (e *ColumnRefExpression) Children() []Node { return nil }

// ConstantExpression is a literal value. Port of ConstantExpression.
type ConstantExpression struct {
	exprBase
	Value Value `json:"value"`
}

func (e *ConstantExpression) Children() []Node { return nil }

// ParameterKind distinguishes the four prepared-parameter spellings.
type ParameterKind uint8

const (
	// ParameterAnonymous is `?` (auto-numbered in order of appearance).
	ParameterAnonymous ParameterKind = iota
	// ParameterQuestionNumbered is `?3`.
	ParameterQuestionNumbered
	// ParameterNumbered is `$3`.
	ParameterNumbered
	// ParameterNamed is `$name`.
	ParameterNamed
)

// ParameterExpression is a prepared-statement parameter — the single most
// important node for sqlc's parameter inference: the spelling kind, the
// resolved number, and the name (for `$name`) are all preserved. Port of
// ParameterExpression.
type ParameterExpression struct {
	exprBase
	Kind ParameterKind `json:"kind"`
	// Number is the parameter's 1-based index: explicit for `$3`/`?3`,
	// assigned in order of appearance for `?`, and assigned in order of
	// first use for `$name`.
	Number int `json:"number"`
	// Name is the identifier for `$name` parameters.
	Name string `json:"name,omitempty"`
}

func (e *ParameterExpression) Children() []Node { return nil }

// Identifier returns DuckDB's parameter identifier: the name for named
// parameters, the decimal number otherwise.
func (e *ParameterExpression) Identifier() string {
	if e.Kind == ParameterNamed {
		return e.Name
	}
	return itoa(e.Number)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [24]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// FunctionArgument is one (possibly named) argument of a function call.
type FunctionArgument struct {
	Name string `json:"name,omitempty"`
	Expr Expr   `json:"expression"`
}

// FunctionExpression is a function call — including binary and prefix
// operators, which DuckDB's transformer represents as operator-named
// functions (`+`, `~~`, ...) with IsOperator set. Port of
// FunctionExpression.
type FunctionExpression struct {
	exprBase
	Catalog      string             `json:"catalog,omitempty"`
	Schema       string             `json:"schema,omitempty"`
	FunctionName string             `json:"function_name"`
	Arguments    []FunctionArgument `json:"arguments"`
	Distinct     bool               `json:"distinct,omitempty"`
	IsOperator   bool               `json:"is_operator,omitempty"`
	ExportState  bool               `json:"export_state,omitempty"`
	Filter       Expr               `json:"filter,omitempty"`
	OrderBys     []OrderByNode      `json:"order_bys,omitempty"`
}

func (e *FunctionExpression) Children() []Node {
	var nodes []Node
	for _, a := range e.Arguments {
		nodes = add(nodes, a.Expr)
	}
	nodes = add(nodes, e.Filter)
	for i := range e.OrderBys {
		nodes = add(nodes, e.OrderBys[i].Expression)
	}
	return nodes
}

// OperatorExpression is an n-ary special operator (NOT, IS NULL, IN,
// array indexing, ...). Port of OperatorExpression.
type OperatorExpression struct {
	exprBase
	Type     ExpressionType `json:"type"`
	Operands []Expr         `json:"children"`
}

func (e *OperatorExpression) Children() []Node { return exprs(e.Operands) }

// ComparisonExpression is a binary comparison. Port of
// ComparisonExpression.
type ComparisonExpression struct {
	exprBase
	Type  ExpressionType `json:"type"`
	Left  Expr           `json:"left"`
	Right Expr           `json:"right"`
}

func (e *ComparisonExpression) Children() []Node { return add(nil, e.Left, e.Right) }

// ConjunctionExpression is an AND/OR chain. Port of
// ConjunctionExpression.
type ConjunctionExpression struct {
	exprBase
	Type     ExpressionType `json:"type"`
	Operands []Expr         `json:"children"`
}

func (e *ConjunctionExpression) Children() []Node { return exprs(e.Operands) }

// CastExpression is CAST/TRY_CAST/`::`. Port of CastExpression. The
// target is an unbound type expression (DuckDB v2 resolves types at bind
// time), except for the plain INTERVAL type which the transformer
// pre-resolves.
type CastExpression struct {
	exprBase
	Child Expr `json:"child"`
	// CastType is the unbound target type; nil when ResolvedType is set.
	CastType *TypeExpression `json:"cast_type,omitempty"`
	// ResolvedType is a pre-resolved logical type id (only "INTERVAL" and
	// the internal casts the transformer synthesizes).
	ResolvedType string `json:"resolved_type,omitempty"`
	TryCast      bool   `json:"try_cast,omitempty"`
}

func (e *CastExpression) Children() []Node { return add(add(nil, e.Child), nodeOrNil(e.CastType)) }

func nodeOrNil(t *TypeExpression) Node {
	if t == nil {
		return nil
	}
	return t
}

// CaseCheck is one WHEN/THEN arm.
type CaseCheck struct {
	When Expr `json:"when_expr"`
	Then Expr `json:"then_expr"`
}

// CaseExpression is CASE [expr] WHEN ... THEN ... ELSE ... END; the
// transformer rewrites the `CASE expr WHEN v` form into `WHEN expr = v`
// like upstream. Port of CaseExpression.
type CaseExpression struct {
	exprBase
	CaseChecks []CaseCheck `json:"case_checks"`
	ElseExpr   Expr        `json:"else_expr"`
}

func (e *CaseExpression) Children() []Node {
	var nodes []Node
	for _, c := range e.CaseChecks {
		nodes = add(nodes, c.When, c.Then)
	}
	return add(nodes, e.ElseExpr)
}

// SubqueryType is DuckDB's SubqueryType enum.
type SubqueryType string

const (
	SubqueryScalar SubqueryType = "SCALAR"
	SubqueryExists SubqueryType = "EXISTS"
	SubqueryAny    SubqueryType = "ANY"
)

// SubqueryExpression is a scalar/EXISTS/ANY subquery; `x IN (SELECT ...)`
// and quantified comparisons desugar to ANY. Port of SubqueryExpression.
type SubqueryExpression struct {
	exprBase
	SubqueryType SubqueryType     `json:"subquery_type"`
	Subquery     *SelectStatement `json:"subquery"`
	// Child is the left-hand side for ANY subqueries.
	Child Expr `json:"child,omitempty"`
	// ComparisonType is the comparison for ANY subqueries; INVALID
	// otherwise.
	ComparisonType ExpressionType `json:"comparison_type"`
}

func (e *SubqueryExpression) Children() []Node { return add(add(nil, e.Child), e.Subquery) }

// StarReplaceEntry is one `expr AS name` inside `* REPLACE (...)`.
type StarReplaceEntry struct {
	Key  string `json:"key"`
	Expr Expr   `json:"value"`
}

// StarRenameEntry is one `name AS newname` inside `* RENAME (...)`.
type StarRenameEntry struct {
	Key  QualifiedColumnName `json:"key"`
	Name string              `json:"value"`
}

// QualifiedColumnName is a dotted column path (for qualified EXCLUDE /
// RENAME entries).
type QualifiedColumnName struct {
	Path []string `json:"path"`
}

// StarExpression is `*`, `t.*`, `COLUMNS(...)` and their EXCLUDE /
// REPLACE / RENAME modifiers. Port of StarExpression.
type StarExpression struct {
	exprBase
	RelationName string `json:"relation_name,omitempty"`
	// ExcludeList holds unqualified excluded names; QualifiedExcludeList
	// holds dotted ones (upstream keeps them separate).
	ExcludeList          []string              `json:"exclude_list,omitempty"`
	QualifiedExcludeList []QualifiedColumnName `json:"qualified_exclude_list,omitempty"`
	ReplaceList          []StarReplaceEntry    `json:"replace_list,omitempty"`
	RenameList           []StarRenameEntry     `json:"rename_list,omitempty"`
	// Columns marks COLUMNS(...); Expr is its argument (pattern or
	// lambda), or nil for COLUMNS(*).
	Columns bool `json:"columns,omitempty"`
	Expr    Expr `json:"expr,omitempty"`
}

func (e *StarExpression) Children() []Node {
	var nodes []Node
	for _, r := range e.ReplaceList {
		nodes = add(nodes, r.Expr)
	}
	return add(nodes, e.Expr)
}

// LambdaSyntaxType records which spelling produced a lambda.
type LambdaSyntaxType string

const (
	LambdaSingleArrow LambdaSyntaxType = "SINGLE_ARROW"
	LambdaKeyword     LambdaSyntaxType = "LAMBDA_KEYWORD"
)

// LambdaExpression is `x -> expr` or `LAMBDA x, y : expr`. LHS is a
// column ref (single parameter) or row function call (parameter list),
// mirroring upstream. Port of LambdaExpression.
type LambdaExpression struct {
	exprBase
	LHS        Expr             `json:"lhs"`
	Expr       Expr             `json:"expr"`
	SyntaxType LambdaSyntaxType `json:"syntax_type"`
}

func (e *LambdaExpression) Children() []Node { return add(nil, e.LHS, e.Expr) }

// BetweenExpression is `input BETWEEN lower AND upper`. Port of
// BetweenExpression.
type BetweenExpression struct {
	exprBase
	Input Expr `json:"input"`
	Lower Expr `json:"lower"`
	Upper Expr `json:"upper"`
}

func (e *BetweenExpression) Children() []Node { return add(nil, e.Input, e.Lower, e.Upper) }

// CollateExpression is `child COLLATE collation`. Port of
// CollateExpression.
type CollateExpression struct {
	exprBase
	Child     Expr   `json:"child"`
	Collation string `json:"collation"`
}

func (e *CollateExpression) Children() []Node { return add(nil, e.Child) }

// PositionalReferenceExpression is `#n`. Port of
// PositionalReferenceExpression.
type PositionalReferenceExpression struct {
	exprBase
	Index uint64 `json:"index"`
}

func (e *PositionalReferenceExpression) Children() []Node { return nil }

// DefaultExpression is the DEFAULT keyword in expression position. Port
// of DefaultExpression.
type DefaultExpression struct {
	exprBase
}

func (e *DefaultExpression) Children() []Node { return nil }

// TypeExpression is an unbound type in expression position — DuckDB v2
// defers type resolution to the binder, so `CAST(x AS my.tp(3))` carries
// the type as an expression: qualified name plus modifier/child
// expressions (nested TypeExpressions carry struct field names in their
// alias). Port of upstream's TYPE expression class.
type TypeExpression struct {
	exprBase
	Catalog  string `json:"catalog,omitempty"`
	Schema   string `json:"schema,omitempty"`
	TypeName string `json:"type_name"`
	// Args holds type modifiers (constants) and element types (nested
	// TypeExpressions; a struct field's name is the child's Alias).
	Args []Expr `json:"children,omitempty"`
}

func (e *TypeExpression) Children() []Node { return exprs(e.Args) }

// WindowBoundary mirrors DuckDB's WindowBoundary enum spelling.
type WindowBoundary string

const (
	WindowUnboundedPreceding  WindowBoundary = "UNBOUNDED_PRECEDING"
	WindowUnboundedFollowing  WindowBoundary = "UNBOUNDED_FOLLOWING"
	WindowCurrentRowRange     WindowBoundary = "CURRENT_ROW_RANGE"
	WindowCurrentRowRows      WindowBoundary = "CURRENT_ROW_ROWS"
	WindowCurrentRowGroups    WindowBoundary = "CURRENT_ROW_GROUPS"
	WindowExprPrecedingRows   WindowBoundary = "EXPR_PRECEDING_ROWS"
	WindowExprFollowingRows   WindowBoundary = "EXPR_FOLLOWING_ROWS"
	WindowExprPrecedingRange  WindowBoundary = "EXPR_PRECEDING_RANGE"
	WindowExprFollowingRange  WindowBoundary = "EXPR_FOLLOWING_RANGE"
	WindowExprPrecedingGroups WindowBoundary = "EXPR_PRECEDING_GROUPS"
	WindowExprFollowingGroups WindowBoundary = "EXPR_FOLLOWING_GROUPS"
)

// WindowExcludeClause mirrors DuckDB's WindowExcludeMode spelling.
type WindowExcludeClause string

const (
	WindowExcludeNoOther    WindowExcludeClause = "NO_OTHER"
	WindowExcludeCurrentRow WindowExcludeClause = "CURRENT_ROW"
	WindowExcludeGroup      WindowExcludeClause = "GROUP"
	WindowExcludeTies       WindowExcludeClause = "TIES"
)

// WindowExpression is a function call with an OVER clause. Type is the
// specialized WINDOW_* expression type (WINDOW_AGGREGATE for ordinary
// aggregates). Port of WindowExpression.
type WindowExpression struct {
	exprBase
	Type         ExpressionType     `json:"type"`
	Catalog      string             `json:"catalog,omitempty"`
	Schema       string             `json:"schema,omitempty"`
	FunctionName string             `json:"function_name"`
	Arguments    []FunctionArgument `json:"arguments"`
	Partitions   []Expr             `json:"partitions,omitempty"`
	Orders       []OrderByNode      `json:"orders,omitempty"`
	FrameStart   WindowBoundary     `json:"start"`
	FrameEnd     WindowBoundary     `json:"end"`
	StartExpr    Expr               `json:"start_expr,omitempty"`
	EndExpr      Expr               `json:"end_expr,omitempty"`
	OffsetExpr   Expr               `json:"offset_expr,omitempty"`
	DefaultExpr  Expr               `json:"default_expr,omitempty"`
	IgnoreNulls  bool               `json:"ignore_nulls,omitempty"`
	// HasIgnoreNulls records that IGNORE or RESPECT NULLS was written.
	HasIgnoreNulls bool                `json:"has_ignore_nulls,omitempty"`
	Distinct       bool                `json:"distinct,omitempty"`
	FilterExpr     Expr                `json:"filter_expr,omitempty"`
	ExcludeClause  WindowExcludeClause `json:"exclude_clause"`
	ArgOrders      []OrderByNode       `json:"arg_orders,omitempty"`
}

func (e *WindowExpression) Children() []Node {
	var nodes []Node
	for _, a := range e.Arguments {
		nodes = add(nodes, a.Expr)
	}
	for _, p := range e.Partitions {
		nodes = add(nodes, p)
	}
	for i := range e.Orders {
		nodes = add(nodes, e.Orders[i].Expression)
	}
	for i := range e.ArgOrders {
		nodes = add(nodes, e.ArgOrders[i].Expression)
	}
	return add(nodes, e.StartExpr, e.EndExpr, e.OffsetExpr, e.DefaultExpr, e.FilterExpr)
}

// OrderType mirrors DuckDB's OrderType spelling.
type OrderType string

const (
	OrderDefault    OrderType = "ORDER_DEFAULT"
	OrderAscending  OrderType = "ASCENDING"
	OrderDescending OrderType = "DESCENDING"
)

// NullOrder mirrors DuckDB's OrderByNullType spelling (note the spaces).
type NullOrder string

const (
	NullOrderDefault NullOrder = "ORDER_DEFAULT"
	NullsFirst       NullOrder = "NULLS FIRST"
	NullsLast        NullOrder = "NULLS LAST"
)

// OrderByNode is one ORDER BY entry.
type OrderByNode struct {
	Type       OrderType `json:"type"`
	NullOrder  NullOrder `json:"null_order"`
	Expression Expr      `json:"expression"`
}
