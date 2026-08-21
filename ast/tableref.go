package ast

// tableRefBase carries what every DuckDB TableRef has: alias and sample.
type tableRefBase struct {
	spanned
	Alias  string         `json:"alias,omitempty"`
	Sample *SampleOptions `json:"sample,omitempty"`
}

func (t *tableRefBase) tableRefNode() {}

// AtUnit is the unit of an AT clause (time travel).
type AtUnit string

const (
	AtVersion   AtUnit = "VERSION"
	AtTimestamp AtUnit = "TIMESTAMP"
)

// AtClause is `AT (VERSION => ...)` / `AT (TIMESTAMP => ...)`. Port of
// BoundAtClause's parsed form.
type AtClause struct {
	spanned
	Unit AtUnit `json:"unit"`
	Expr Expr   `json:"expr"`
}

func (a *AtClause) Children() []Node { return add(nil, a.Expr) }

// BaseTableRef is a plain (possibly qualified) table name. Port of
// BaseTableRef.
type BaseTableRef struct {
	tableRefBase
	CatalogName     string    `json:"catalog_name,omitempty"`
	SchemaName      string    `json:"schema_name,omitempty"`
	TableName       string    `json:"table_name"`
	ColumnNameAlias []string  `json:"column_name_alias,omitempty"`
	At              *AtClause `json:"at_clause,omitempty"`
}

func (t *BaseTableRef) Children() []Node {
	if t.At == nil {
		return nil
	}
	return []Node{t.At}
}

// EmptyTableRef is the absent FROM clause. Port of EmptyTableRef.
type EmptyTableRef struct {
	tableRefBase
}

func (t *EmptyTableRef) Children() []Node { return nil }

// JoinType mirrors DuckDB's JoinType spelling.
type JoinType string

const (
	JoinInner JoinType = "INNER"
	JoinLeft  JoinType = "LEFT"
	JoinRight JoinType = "RIGHT"
	JoinFull  JoinType = "FULL"
	JoinSemi  JoinType = "SEMI"
	JoinAnti  JoinType = "ANTI"
)

// JoinRefType mirrors DuckDB's JoinRefType spelling.
type JoinRefType string

const (
	JoinRefRegular    JoinRefType = "REGULAR"
	JoinRefNatural    JoinRefType = "NATURAL"
	JoinRefCross      JoinRefType = "CROSS"
	JoinRefPositional JoinRefType = "POSITIONAL"
	JoinRefAsof       JoinRefType = "ASOF"
	JoinRefNearest    JoinRefType = "NEAREST"
)

// JoinRef is a join between two table refs, including the implicit
// cross joins of comma-separated FROM lists. Port of JoinRef.
type JoinRef struct {
	tableRefBase
	Left         TableRef    `json:"left"`
	Right        TableRef    `json:"right"`
	Condition    Expr        `json:"condition,omitempty"`
	JoinType     JoinType    `json:"join_type"`
	RefType      JoinRefType `json:"ref_type"`
	UsingColumns []string    `json:"using_columns,omitempty"`
	// IsImplicit marks the cross joins synthesized from `FROM a, b`.
	IsImplicit bool `json:"is_implicit,omitempty"`
	// ColumnNameAlias holds the column aliases of an aliased
	// parenthesized join (`(a JOIN b USING (k)) t(x, y)`).
	ColumnNameAlias []string `json:"column_name_alias,omitempty"`
	// Nearest-join (JOIN ... NEAREST k BY ...) fields.
	RankingExpression Expr      `json:"ranking_expression,omitempty"`
	NearestCount      int64     `json:"nearest_count,omitempty"`
	NearestOrderType  OrderType `json:"nearest_order_type,omitempty"`
	NearestApprox     bool      `json:"nearest_approx,omitempty"`
}

func (t *JoinRef) Children() []Node {
	return add(nil, t.Left, t.Right, t.Condition, t.RankingExpression)
}

// SubqueryRef is a parenthesized subquery in FROM. Port of SubqueryRef.
type SubqueryRef struct {
	tableRefBase
	Subquery        *SelectStatement `json:"subquery"`
	ColumnNameAlias []string         `json:"column_name_alias,omitempty"`
}

func (t *SubqueryRef) Children() []Node { return add(nil, t.Subquery) }

// TableFunctionRef is a table function call in FROM. Port of
// TableFunctionRef.
type TableFunctionRef struct {
	tableRefBase
	// Function is the call, a FunctionExpression (or a SubqueryExpression
	// argument wrapper for table-in-out functions).
	Function        Expr     `json:"function"`
	ColumnNameAlias []string `json:"column_name_alias,omitempty"`
	WithOrdinality  bool     `json:"with_ordinality,omitempty"`
}

func (t *TableFunctionRef) Children() []Node { return add(nil, t.Function) }

// ExpressionListRef is a VALUES list used as a table. Port of
// ExpressionListRef.
type ExpressionListRef struct {
	tableRefBase
	ExpectedNames []string `json:"expected_names,omitempty"`
	Values        [][]Expr `json:"values"`
}

func (t *ExpressionListRef) Children() []Node {
	var nodes []Node
	for _, row := range t.Values {
		nodes = append(nodes, exprs(row)...)
	}
	return nodes
}

// PivotColumn is one FOR entry of a PIVOT (or the ON list of the
// simplified syntax). Port of PivotColumn.
type PivotColumn struct {
	PivotExpressions []Expr             `json:"pivot_expressions,omitempty"`
	UnpivotNames     []string           `json:"unpivot_names,omitempty"`
	Entries          []PivotColumnEntry `json:"entries,omitempty"`
	PivotEnum        string             `json:"pivot_enum,omitempty"`
	Subquery         *SelectStatement   `json:"subquery,omitempty"`
}

// PivotColumnEntry is one IN entry: literal values plus optional alias.
type PivotColumnEntry struct {
	Values   []Value `json:"values,omitempty"`
	StarExpr Expr    `json:"star_expr,omitempty"`
	Alias    string  `json:"alias,omitempty"`
	Expr     Expr    `json:"expr,omitempty"`
}

// PivotRef is a PIVOT/UNPIVOT table reference (both the statement form,
// which desugars to it, and the table-ref form). Port of PivotRef.
type PivotRef struct {
	tableRefBase
	Source TableRef `json:"source"`
	// Aggregates are the USING aggregates (PIVOT); nil for UNPIVOT.
	Aggregates []Expr `json:"aggregates,omitempty"`
	// UnpivotNames are the value column names of UNPIVOT.
	UnpivotNames    []string      `json:"unpivot_names,omitempty"`
	Pivots          []PivotColumn `json:"pivots"`
	GroupBy         []string      `json:"groups,omitempty"`
	ColumnNameAlias []string      `json:"column_name_alias,omitempty"`
	IncludeNulls    bool          `json:"include_nulls,omitempty"`
}

func (t *PivotRef) Children() []Node {
	nodes := add(nil, t.Source)
	return append(nodes, exprs(t.Aggregates)...)
}

// ShowRefType distinguishes SHOW/DESCRIBE/SUMMARIZE targets.
type ShowType string

const (
	ShowTypeDescribe        ShowType = "DESCRIBE"
	ShowTypeSummary         ShowType = "SUMMARY"
	ShowTypeShowFrom        ShowType = "SHOW_FROM"
	ShowTypeShowUnqualified ShowType = "SHOW_UNQUALIFIED"
)

// ShowRef is the relation behind SHOW/DESCRIBE/SUMMARIZE. Port of
// ShowRef.
type ShowRef struct {
	tableRefBase
	TableName   string `json:"table_name,omitempty"`
	CatalogName string `json:"catalog_name,omitempty"`
	SchemaName  string `json:"schema_name,omitempty"`
	// Query is set when describing a query (`DESCRIBE SELECT ...`).
	Query    QueryNode `json:"query,omitempty"`
	ShowType ShowType  `json:"show_type"`
}

func (t *ShowRef) Children() []Node { return add(nil, t.Query) }

// ColumnDataRef only appears in serialized plans, not in parser output;
// it is intentionally absent from darkwing.
