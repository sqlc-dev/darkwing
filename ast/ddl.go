package ast

// QualifiedName is a possibly catalog/schema-qualified object name.
type QualifiedName struct {
	Catalog string `json:"catalog,omitempty"`
	Schema  string `json:"schema,omitempty"`
	Name    string `json:"name"`
}

// OnCreateConflict mirrors DuckDB's OnCreateConflict: what CREATE does
// when the object exists.
type OnCreateConflict string

const (
	CreateError   OnCreateConflict = "ERROR"
	CreateIgnore  OnCreateConflict = "IGNORE"  // IF NOT EXISTS
	CreateReplace OnCreateConflict = "REPLACE" // OR REPLACE
)

// CreateInfo is implemented by the per-object payloads of
// CreateStatement.
type CreateInfo interface {
	Node
	createInfo()
	// Base returns the shared fields.
	Base() *CreateInfoBase
}

// CreateInfoBase carries the fields shared by every CREATE variant.
type CreateInfoBase struct {
	spanned
	Catalog    string           `json:"catalog,omitempty"`
	Schema     string           `json:"schema,omitempty"`
	Name       string           `json:"name"`
	OnConflict OnCreateConflict `json:"on_conflict"`
	Temporary  bool             `json:"temporary,omitempty"`
}

func (b *CreateInfoBase) createInfo()           {}
func (b *CreateInfoBase) Base() *CreateInfoBase { return b }

// CreateStatement is CREATE <object>. Port of CreateStatement.
type CreateStatement struct {
	stmtBase
	Info        CreateInfo   `json:"info"`
	NamedParams []NamedParam `json:"named_param_map,omitempty"`
}

func (s *CreateStatement) Children() []Node { return add(nil, s.Info) }

// GeneratedColumnKind distinguishes VIRTUAL and STORED generated
// columns.
type GeneratedColumnKind string

const (
	GeneratedVirtual GeneratedColumnKind = "VIRTUAL"
	GeneratedStored  GeneratedColumnKind = "STORED"
)

// ColumnDef is one column of CREATE TABLE (or ALTER TABLE ADD COLUMN).
// Port of ColumnDefinition; the parsed column constraints stay attached
// here rather than being flattened, for sqlc's benefit.
type ColumnDef struct {
	spanned
	// Names is the (possibly dotted) column name path.
	Names []string        `json:"names"`
	Type  *TypeExpression `json:"type,omitempty"`
	// Generated is set for generated columns; Default holds the
	// generation expression in that case, mirroring upstream.
	Generated   GeneratedColumnKind `json:"generated,omitempty"`
	Default     Expr                `json:"default,omitempty"`
	Constraints []Constraint        `json:"constraints,omitempty"`
	Collation   string              `json:"collation,omitempty"`
	Compression string              `json:"compression,omitempty"`
}

func (c *ColumnDef) Children() []Node {
	var nodes []Node
	if c.Type != nil {
		nodes = add(nodes, c.Type)
	}
	nodes = add(nodes, c.Default)
	for _, con := range c.Constraints {
		nodes = add(nodes, con)
	}
	return nodes
}

// Constraint is a table or column constraint.
type Constraint interface {
	Node
	constraintNode()
}

type constraintBase struct {
	spanned
	// Name is the optional CONSTRAINT name.
	Name string `json:"name,omitempty"`
}

func (c *constraintBase) constraintNode() {}

// NotNullConstraint is NOT NULL (Negated false) or NULL (Negated true).
type NotNullConstraint struct {
	constraintBase
	// Null is true for an explicit NULL constraint.
	Null bool `json:"null,omitempty"`
}

func (c *NotNullConstraint) Children() []Node { return nil }

// UniqueConstraint is UNIQUE or PRIMARY KEY, as a column constraint
// (Columns empty) or a table constraint (Columns set).
type UniqueConstraint struct {
	constraintBase
	IsPrimaryKey bool     `json:"is_primary_key,omitempty"`
	Columns      []string `json:"columns,omitempty"`
}

func (c *UniqueConstraint) Children() []Node { return nil }

// CheckConstraint is CHECK (expr).
type CheckConstraint struct {
	constraintBase
	Expr Expr `json:"expression"`
}

func (c *CheckConstraint) Children() []Node { return add(nil, c.Expr) }

// DefaultConstraint is a column DEFAULT clause.
type DefaultConstraint struct {
	constraintBase
	Expr Expr `json:"expression"`
}

func (c *DefaultConstraint) Children() []Node { return add(nil, c.Expr) }

// KeyAction mirrors foreign-key referential actions.
type KeyAction string

const (
	KeyActionNone       KeyAction = "NO ACTION"
	KeyActionRestrict   KeyAction = "RESTRICT"
	KeyActionCascade    KeyAction = "CASCADE"
	KeyActionSetNull    KeyAction = "SET NULL"
	KeyActionSetDefault KeyAction = "SET DEFAULT"
)

// ForeignKeyConstraint is REFERENCES / FOREIGN KEY.
type ForeignKeyConstraint struct {
	constraintBase
	// Columns are the referencing columns (table-level form only).
	Columns []string `json:"columns,omitempty"`
	// Referenced table and columns.
	Table             QualifiedName `json:"table"`
	ReferencedColumns []string      `json:"referenced_columns,omitempty"`
	OnUpdate          KeyAction     `json:"on_update,omitempty"`
	OnDelete          KeyAction     `json:"on_delete,omitempty"`
}

func (c *ForeignKeyConstraint) Children() []Node { return nil }

// CreateTableInfo is CREATE TABLE. Port of CreateTableInfo.
type CreateTableInfo struct {
	CreateInfoBase
	Columns     []ColumnDef  `json:"columns,omitempty"`
	Constraints []Constraint `json:"constraints,omitempty"`
	// Query is CREATE TABLE ... AS.
	Query *SelectStatement `json:"query,omitempty"`
	// QueryAliases are the CTAS column aliases (`CREATE TABLE t (a, b) AS`).
	QueryAliases []string `json:"query_aliases,omitempty"`
}

func (i *CreateTableInfo) Children() []Node {
	var nodes []Node
	for j := range i.Columns {
		nodes = add(nodes, &i.Columns[j])
	}
	for _, c := range i.Constraints {
		nodes = add(nodes, c)
	}
	return add(nodes, i.Query)
}

// CreateViewInfo is CREATE VIEW. Port of CreateViewInfo.
type CreateViewInfo struct {
	CreateInfoBase
	Recursive bool             `json:"recursive,omitempty"`
	Aliases   []string         `json:"aliases,omitempty"`
	Query     *SelectStatement `json:"query"`
}

func (i *CreateViewInfo) Children() []Node { return add(nil, i.Query) }

// CreateSchemaInfo is CREATE SCHEMA. Port of CreateSchemaInfo.
type CreateSchemaInfo struct {
	CreateInfoBase
}

func (i *CreateSchemaInfo) Children() []Node { return nil }

// IndexElement is one indexed expression with its ordering.
type IndexElement struct {
	Expr      Expr      `json:"expression"`
	OrderType OrderType `json:"order_type,omitempty"`
	NullOrder NullOrder `json:"null_order,omitempty"`
}

// CreateIndexInfo is CREATE INDEX. Port of CreateIndexInfo.
type CreateIndexInfo struct {
	CreateInfoBase
	Unique bool          `json:"unique,omitempty"`
	Table  QualifiedName `json:"table"`
	// IndexType is the USING method (ART by default upstream; recorded
	// verbatim here).
	IndexType string         `json:"index_type,omitempty"`
	Elements  []IndexElement `json:"elements,omitempty"`
	Where     Expr           `json:"where,omitempty"`
}

func (i *CreateIndexInfo) Children() []Node {
	var nodes []Node
	for j := range i.Elements {
		nodes = add(nodes, i.Elements[j].Expr)
	}
	return add(nodes, i.Where)
}

// CreateSequenceInfo is CREATE SEQUENCE. Port of CreateSequenceInfo.
// Option expressions stay as expressions (DuckDB evaluates them at bind
// time).
type CreateSequenceInfo struct {
	CreateInfoBase
	Increment  Expr `json:"increment,omitempty"`
	MinValue   Expr `json:"min_value,omitempty"`
	MaxValue   Expr `json:"max_value,omitempty"`
	StartValue Expr `json:"start_value,omitempty"`
	// NoMinValue / NoMaxValue record explicit NO MINVALUE / NO MAXVALUE.
	NoMinValue bool `json:"no_min_value,omitempty"`
	NoMaxValue bool `json:"no_max_value,omitempty"`
	// Cycle records CYCLE (true) / NO CYCLE (false, the default).
	Cycle bool `json:"cycle,omitempty"`
	// OwnedBy is the OWNED BY target.
	OwnedBy *QualifiedName `json:"owned_by,omitempty"`
}

func (i *CreateSequenceInfo) Children() []Node {
	return add(nil, i.Increment, i.MinValue, i.MaxValue, i.StartValue)
}

// CreateTypeInfo is CREATE TYPE. Port of CreateTypeInfo.
type CreateTypeInfo struct {
	CreateInfoBase
	// Type is the aliased type (`CREATE TYPE t AS <type>`).
	Type *TypeExpression `json:"type,omitempty"`
	// EnumValues is `AS ENUM ('a', 'b')`.
	EnumValues []string `json:"enum_values,omitempty"`
	// EnumQuery is `AS ENUM (SELECT ...)`.
	EnumQuery *SelectStatement `json:"enum_query,omitempty"`
	// IsEnum distinguishes an empty enum list from a missing one.
	IsEnum bool `json:"is_enum,omitempty"`
}

func (i *CreateTypeInfo) Children() []Node {
	var nodes []Node
	if i.Type != nil {
		nodes = add(nodes, i.Type)
	}
	return add(nodes, i.EnumQuery)
}

// DropType names the object family a DROP statement targets.
type DropType string

const (
	DropTable            DropType = "TABLE"
	DropView             DropType = "VIEW"
	DropMaterializedView DropType = "MATERIALIZED_VIEW"
	DropSchema           DropType = "SCHEMA"
	DropIndex            DropType = "INDEX"
	DropSequence         DropType = "SEQUENCE"
	DropCollation        DropType = "COLLATION"
	DropTypeType         DropType = "TYPE"
	DropMacro            DropType = "MACRO"
	DropMacroTable       DropType = "MACRO_TABLE"
	DropFunction         DropType = "FUNCTION"
	DropSecret           DropType = "SECRET"
	DropTrigger          DropType = "TRIGGER"
)

// DropStatement is DROP <object>. Port of DropStatement; the grammar
// allows a list of names, kept as-is.
type DropStatement struct {
	stmtBase
	DropType DropType        `json:"drop_type"`
	IfExists bool            `json:"if_exists,omitempty"`
	Names    []QualifiedName `json:"names"`
	// Cascade is CASCADE (true) / RESTRICT or absent (false).
	Cascade bool `json:"cascade,omitempty"`
	// Temporary and Storage apply to DROP SECRET.
	Temporary   bool         `json:"temporary,omitempty"`
	Storage     string       `json:"storage,omitempty"`
	NamedParams []NamedParam `json:"named_param_map,omitempty"`
}

func (s *DropStatement) Children() []Node { return nil }

// AlterInfo is one ALTER action; a single ALTER TABLE statement can
// carry several.
type AlterInfo interface {
	Node
	alterInfo()
}

type alterInfoBase struct {
	spanned
}

func (a *alterInfoBase) alterInfo() {}

// AlterEntityKind is what kind of object an AlterStatement targets.
type AlterEntityKind string

const (
	AlterEntityTable    AlterEntityKind = "TABLE"
	AlterEntityView     AlterEntityKind = "VIEW"
	AlterEntitySequence AlterEntityKind = "SEQUENCE"
	AlterEntityDatabase AlterEntityKind = "DATABASE"
	AlterEntitySchema   AlterEntityKind = "SCHEMA"
)

// AlterStatement is ALTER TABLE/VIEW/SEQUENCE/DATABASE/SCHEMA. Port of
// AlterStatement.
type AlterStatement struct {
	stmtBase
	Entity      AlterEntityKind `json:"entity"`
	IfExists    bool            `json:"if_exists,omitempty"`
	Name        QualifiedName   `json:"name"`
	Actions     []AlterInfo     `json:"actions"`
	NamedParams []NamedParam    `json:"named_param_map,omitempty"`
}

func (s *AlterStatement) Children() []Node {
	var nodes []Node
	for _, a := range s.Actions {
		nodes = add(nodes, a)
	}
	return nodes
}

// AddColumnInfo is ALTER TABLE ADD COLUMN.
type AddColumnInfo struct {
	alterInfoBase
	IfNotExists bool      `json:"if_not_exists,omitempty"`
	Column      ColumnDef `json:"column"`
}

func (a *AddColumnInfo) Children() []Node { return add(nil, &a.Column) }

// DropColumnInfo is ALTER TABLE DROP COLUMN.
type DropColumnInfo struct {
	alterInfoBase
	IfExists bool     `json:"if_exists,omitempty"`
	Column   []string `json:"column"`
	Cascade  bool     `json:"cascade,omitempty"`
}

func (a *DropColumnInfo) Children() []Node { return nil }

// AlterColumnTypeInfo is ALTER TABLE ALTER COLUMN ... [SET DATA] TYPE.
type AlterColumnTypeInfo struct {
	alterInfoBase
	Column []string        `json:"column"`
	Type   *TypeExpression `json:"type,omitempty"`
	Using  Expr            `json:"using,omitempty"`
}

func (a *AlterColumnTypeInfo) Children() []Node {
	var nodes []Node
	if a.Type != nil {
		nodes = add(nodes, a.Type)
	}
	return add(nodes, a.Using)
}

// SetDefaultInfo is ALTER COLUMN ... SET DEFAULT / DROP DEFAULT (nil
// Expr).
type SetDefaultInfo struct {
	alterInfoBase
	Column []string `json:"column"`
	Expr   Expr     `json:"expression,omitempty"`
}

func (a *SetDefaultInfo) Children() []Node { return add(nil, a.Expr) }

// SetNotNullInfo is ALTER COLUMN ... SET/DROP NOT NULL.
type SetNotNullInfo struct {
	alterInfoBase
	Column []string `json:"column"`
	// Drop is true for DROP NOT NULL.
	Drop bool `json:"drop,omitempty"`
}

func (a *SetNotNullInfo) Children() []Node { return nil }

// RenameColumnInfo is ALTER TABLE RENAME COLUMN.
type RenameColumnInfo struct {
	alterInfoBase
	Column  []string `json:"column"`
	NewName string   `json:"new_name"`
}

func (a *RenameColumnInfo) Children() []Node { return nil }

// RenameInfo is ALTER ... RENAME TO.
type RenameInfo struct {
	alterInfoBase
	NewName string `json:"new_name"`
}

func (a *RenameInfo) Children() []Node { return nil }

// AddConstraintInfo is ALTER TABLE ADD <constraint>.
type AddConstraintInfo struct {
	alterInfoBase
	Constraint Constraint `json:"constraint"`
}

func (a *AddConstraintInfo) Children() []Node { return add(nil, a.Constraint) }

// SetSequenceOptionInfo is ALTER SEQUENCE <options>.
type SetSequenceOptionInfo struct {
	alterInfoBase
	Options CreateSequenceInfo `json:"options"`
}

func (a *SetSequenceOptionInfo) Children() []Node { return a.Options.Children() }

// SetDatabaseAliasInfo is ALTER DATABASE ... SET ALIAS TO.
type SetDatabaseAliasInfo struct {
	alterInfoBase
	NewName string `json:"new_name"`
}

func (a *SetDatabaseAliasInfo) Children() []Node { return nil }

// GenericAlterInfo covers the remaining ALTER TABLE options darkwing
// records but does not model structurally (partitioning, rel options).
type GenericAlterInfo struct {
	alterInfoBase
	// Kind names the option (e.g. "SET_PARTITIONED_BY").
	Kind        string `json:"kind"`
	Expressions []Expr `json:"expressions,omitempty"`
}

func (a *GenericAlterInfo) Children() []Node { return exprs(a.Expressions) }
