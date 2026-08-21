package ast

// stmtBase gives statements their span. Statement spans tile the input
// (see Span).
type stmtBase struct {
	spanned
}

func (s *stmtBase) stmtNode() {}

// NamedParam is one entry of a statement's parameter map: the parameter
// identifier ("1", "name") and its 1-based index.
type NamedParam struct {
	Name  string `json:"key"`
	Index int    `json:"value"`
}

// SelectStatement wraps a query-node tree. Port of SelectStatement.
type SelectStatement struct {
	stmtBase
	Node QueryNode `json:"node"`
	// NamedParams maps parameter identifiers to indexes, populated only
	// on top-level statements (like upstream).
	NamedParams []NamedParam `json:"named_param_map,omitempty"`
}

func (s *SelectStatement) Children() []Node { return add(nil, s.Node) }

// OnConflictAction mirrors DuckDB's OnConflictAction spelling.
type OnConflictAction string

const (
	OnConflictThrow   OnConflictAction = "THROW"
	OnConflictNothing OnConflictAction = "NOTHING"
	OnConflictUpdate  OnConflictAction = "UPDATE"
	OnConflictReplace OnConflictAction = "REPLACE"
)

// OnConflictInfo is the ON CONFLICT clause of INSERT (and the OR
// REPLACE / OR IGNORE shorthands). Port of OnConflictInfo.
type OnConflictInfo struct {
	spanned
	Action OnConflictAction `json:"action"`
	// IndexedColumns is the conflict target column list.
	IndexedColumns []string `json:"indexed_columns,omitempty"`
	// ConstraintName is set for ON CONFLICT ON CONSTRAINT name.
	ConstraintName string `json:"constraint_name,omitempty"`
	// TargetWhere is the WHERE on the conflict target.
	TargetWhere Expr `json:"target_where,omitempty"`
	// SetInfo is the DO UPDATE SET payload.
	SetInfo *UpdateSetInfo `json:"set_info,omitempty"`
}

func (o *OnConflictInfo) Children() []Node {
	nodes := add(nil, o.TargetWhere)
	if o.SetInfo != nil {
		nodes = add(nodes, o.SetInfo)
	}
	return nodes
}

// InsertColumnOrder mirrors DuckDB's InsertColumnOrder.
type InsertColumnOrder string

const (
	InsertByPosition InsertColumnOrder = "INSERT_BY_POSITION"
	InsertByName     InsertColumnOrder = "INSERT_BY_NAME"
)

// InsertStatement is INSERT INTO. Port of InsertStatement.
type InsertStatement struct {
	stmtBase
	Catalog string `json:"catalog,omitempty"`
	Schema  string `json:"schema,omitempty"`
	Table   string `json:"table"`
	// TableAlias is `INSERT INTO t AS alias`.
	TableAlias string `json:"table_alias,omitempty"`
	// Columns is the explicit insert column list.
	Columns     []string          `json:"columns,omitempty"`
	ColumnOrder InsertColumnOrder `json:"column_order,omitempty"`
	// Query is the inserted data; nil for DEFAULT VALUES.
	Query         *SelectStatement `json:"select_statement,omitempty"`
	DefaultValues bool             `json:"default_values,omitempty"`
	OnConflict    *OnConflictInfo  `json:"on_conflict_info,omitempty"`
	Returning     []Expr           `json:"returning_list,omitempty"`
	CTEs          CTEMap           `json:"cte_map,omitempty"`
	NamedParams   []NamedParam     `json:"named_param_map,omitempty"`
}

func (s *InsertStatement) Children() []Node {
	var nodes []Node
	for _, e := range s.CTEs.Entries {
		nodes = add(nodes, e.CTE)
	}
	nodes = add(nodes, s.Query)
	if s.OnConflict != nil {
		nodes = add(nodes, s.OnConflict)
	}
	return append(nodes, exprs(s.Returning)...)
}

// UpdateSetInfo is the SET list of UPDATE / ON CONFLICT DO UPDATE /
// MERGE. Port of UpdateSetInfo.
type UpdateSetInfo struct {
	spanned
	// Columns[i] is assigned Expressions[i]. Nested struct-field targets
	// keep their dotted path.
	Columns     [][]string `json:"columns"`
	Expressions []Expr     `json:"expressions"`
	// Condition is the WHERE of ON CONFLICT DO UPDATE SET ... WHERE.
	Condition Expr `json:"condition,omitempty"`
}

func (u *UpdateSetInfo) Children() []Node {
	return add(exprs(u.Expressions), u.Condition)
}

// UpdateStatement is UPDATE. Port of UpdateStatement.
type UpdateStatement struct {
	stmtBase
	// Table is the update target (a BaseTableRef).
	Table TableRef `json:"table"`
	// From is the optional FROM clause.
	From        TableRef       `json:"from_table,omitempty"`
	SetInfo     *UpdateSetInfo `json:"set_info"`
	Where       Expr           `json:"condition,omitempty"`
	Returning   []Expr         `json:"returning_list,omitempty"`
	CTEs        CTEMap         `json:"cte_map,omitempty"`
	NamedParams []NamedParam   `json:"named_param_map,omitempty"`
}

func (s *UpdateStatement) Children() []Node {
	var nodes []Node
	for _, e := range s.CTEs.Entries {
		nodes = add(nodes, e.CTE)
	}
	nodes = add(nodes, s.Table, s.From)
	if s.SetInfo != nil {
		nodes = add(nodes, s.SetInfo)
	}
	nodes = add(nodes, s.Where)
	return append(nodes, exprs(s.Returning)...)
}

// DeleteStatement is DELETE FROM. Port of DeleteStatement.
type DeleteStatement struct {
	stmtBase
	Table       TableRef     `json:"table"`
	Using       []TableRef   `json:"using_clauses,omitempty"`
	Where       Expr         `json:"condition,omitempty"`
	Returning   []Expr       `json:"returning_list,omitempty"`
	CTEs        CTEMap       `json:"cte_map,omitempty"`
	NamedParams []NamedParam `json:"named_param_map,omitempty"`
}

func (s *DeleteStatement) Children() []Node {
	var nodes []Node
	for _, e := range s.CTEs.Entries {
		nodes = add(nodes, e.CTE)
	}
	nodes = add(nodes, s.Table)
	for _, u := range s.Using {
		nodes = add(nodes, u)
	}
	nodes = add(nodes, s.Where)
	return append(nodes, exprs(s.Returning)...)
}

// TruncateStatement is TRUNCATE [TABLE] name — parsed as its own
// statement but represented like upstream as a delete without a WHERE.
type TruncateStatement struct {
	stmtBase
	Table       TableRef     `json:"table"`
	NamedParams []NamedParam `json:"named_param_map,omitempty"`
}

func (s *TruncateStatement) Children() []Node { return add(nil, s.Table) }

// MergeActionType mirrors DuckDB's MergeIntoActionType.
type MergeActionType string

const (
	MergeUpdate    MergeActionType = "UPDATE"
	MergeDelete    MergeActionType = "DELETE"
	MergeInsert    MergeActionType = "INSERT"
	MergeDoNothing MergeActionType = "DO NOTHING"
	MergeError     MergeActionType = "ERROR"
)

// MergeMatchKind is the WHEN arm kind of MERGE INTO.
type MergeMatchKind string

const (
	MergeWhenMatched            MergeMatchKind = "MATCHED"
	MergeWhenNotMatchedBySource MergeMatchKind = "NOT_MATCHED_BY_SOURCE"
	MergeWhenNotMatchedByTarget MergeMatchKind = "NOT_MATCHED_BY_TARGET"
)

// MergeIntoAction is one WHEN ... THEN arm of MERGE INTO. Port of
// MergeIntoAction.
type MergeIntoAction struct {
	spanned
	Kind      MergeMatchKind  `json:"kind"`
	Condition Expr            `json:"condition,omitempty"`
	Action    MergeActionType `json:"action_type"`
	// SetInfo is the UPDATE SET payload; a nil SetInfo with Action
	// MergeUpdate means UPDATE (SET *) by-position shorthand.
	SetInfo *UpdateSetInfo `json:"set_info,omitempty"`
	// Columns and Expressions are the INSERT payload.
	Columns     []string          `json:"insert_columns,omitempty"`
	Expressions []Expr            `json:"expressions,omitempty"`
	ColumnOrder InsertColumnOrder `json:"column_order,omitempty"`
	// DefaultValues is INSERT DEFAULT VALUES.
	DefaultValues bool `json:"default_values,omitempty"`
	// ErrorExpr is the optional message expression of THEN ERROR.
	ErrorExpr Expr `json:"error_expr,omitempty"`
}

func (a *MergeIntoAction) Children() []Node {
	nodes := add(nil, a.Condition)
	if a.SetInfo != nil {
		nodes = add(nodes, a.SetInfo)
	}
	nodes = append(nodes, exprs(a.Expressions)...)
	return add(nodes, a.ErrorExpr)
}

// MergeIntoStatement is MERGE INTO. Port of MergeIntoStatement.
type MergeIntoStatement struct {
	stmtBase
	Target        TableRef          `json:"target"`
	Source        TableRef          `json:"source"`
	JoinCondition Expr              `json:"join_condition,omitempty"`
	UsingColumns  []string          `json:"using_columns,omitempty"`
	Actions       []MergeIntoAction `json:"actions"`
	Returning     []Expr            `json:"returning_list,omitempty"`
	CTEs          CTEMap            `json:"cte_map,omitempty"`
	NamedParams   []NamedParam      `json:"named_param_map,omitempty"`
}

func (s *MergeIntoStatement) Children() []Node {
	var nodes []Node
	for _, e := range s.CTEs.Entries {
		nodes = add(nodes, e.CTE)
	}
	nodes = add(nodes, s.Target, s.Source, s.JoinCondition)
	for i := range s.Actions {
		nodes = add(nodes, &s.Actions[i])
	}
	return append(nodes, exprs(s.Returning)...)
}
