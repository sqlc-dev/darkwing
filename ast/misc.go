package ast

// misc.go: the milestone-5 statement nodes — DuckDB-native statements
// outside the SELECT/DML/DDL cores: SET/RESET, PRAGMA, CALL, transactions,
// ATTACH/DETACH/USE/CONNECT, COPY, EXPORT/IMPORT, extensions
// (INSTALL/LOAD/UPDATE EXTENSIONS, external resources), CHECKPOINT,
// VACUUM/ANALYZE, COMMENT ON, prepared statements and EXPLAIN. Node names
// follow DuckDB's statement classes; where upstream desugars a statement
// into another family (USE into SET, CHECKPOINT into CALL, IMPORT into
// PRAGMA, DEALLOCATE into DROP, ANALYZE into VACUUM), darkwing keeps a
// dedicated node and notes the upstream shape, for sqlc's benefit.

// GenericOption is one entry of the COPY/ATTACH/SECRET/CONNECT/EXPLAIN
// option soup. Port of GenericCopyOption: constant-ish arguments are
// folded into Values; anything else stays an expression.
type GenericOption struct {
	Name   string  `json:"name"`
	Values []Value `json:"values,omitempty"`
	Expr   Expr    `json:"expression,omitempty"`
}

// optionExprs collects the expression payloads of an option list for
// Children traversal.
func optionExprs(nodes []Node, opts []GenericOption) []Node {
	for _, o := range opts {
		nodes = add(nodes, o.Expr)
	}
	return nodes
}

// SetScope mirrors DuckDB's SetScope spelling.
type SetScope string

const (
	SetScopeAutomatic SetScope = "AUTOMATIC"
	SetScopeLocal     SetScope = "LOCAL"
	SetScopeSession   SetScope = "SESSION"
	SetScopeGlobal    SetScope = "GLOBAL"
	SetScopeVariable  SetScope = "VARIABLE"
)

// SetStatement is SET <setting> = <value> (and the SET SCHEMA / SET TIME
// ZONE special forms). Port of SetVariableStatement.
type SetStatement struct {
	stmtBase
	Name        string       `json:"name"`
	Scope       SetScope     `json:"scope"`
	Value       Expr         `json:"value"`
	NamedParams []NamedParam `json:"named_param_map,omitempty"`
}

func (s *SetStatement) Children() []Node { return add(nil, s.Value) }

// ResetStatement is RESET <setting> (and SET ... TO DEFAULT / SET TIME
// ZONE LOCAL, which upstream folds into resets). Port of
// ResetVariableStatement.
type ResetStatement struct {
	stmtBase
	Name  string   `json:"name"`
	Scope SetScope `json:"scope"`
}

func (s *ResetStatement) Children() []Node { return nil }

// NamedValue is a name/expression pair (PRAGMA named parameters, EXECUTE
// arguments).
type NamedValue struct {
	Name string `json:"name"`
	Expr Expr   `json:"expression"`
}

// PragmaStatement is PRAGMA <name>(...) — the call form, plus the
// assignment forms upstream keeps as pragmas (SQLite-compatible pragmas,
// IMPORT DATABASE and COPY FROM DATABASE desugarings). Port of
// PragmaStatement/PragmaInfo.
type PragmaStatement struct {
	stmtBase
	Name            string       `json:"name"`
	Parameters      []Expr       `json:"parameters,omitempty"`
	NamedParameters []NamedValue `json:"named_parameters,omitempty"`
	NamedParams     []NamedParam `json:"named_param_map,omitempty"`
}

func (s *PragmaStatement) Children() []Node {
	nodes := exprs(s.Parameters)
	for _, nv := range s.NamedParameters {
		nodes = add(nodes, nv.Expr)
	}
	return nodes
}

// CallStatement is CALL fn(...). Port of CallStatement.
type CallStatement struct {
	stmtBase
	Function    *FunctionExpression `json:"function"`
	NamedParams []NamedParam        `json:"named_param_map,omitempty"`
}

func (s *CallStatement) Children() []Node { return add(nil, s.Function) }

// UseStatement is USE db / USE db.schema. Upstream desugars it into
// SET schema; darkwing keeps the target. A single name is recorded in
// Name, USE db.schema fills Catalog and Name.
type UseStatement struct {
	stmtBase
	Catalog string `json:"catalog,omitempty"`
	Name    string `json:"name"`
}

func (s *UseStatement) Children() []Node { return nil }

// CheckpointStatement is [FORCE] CHECKPOINT [catalog]. Upstream desugars
// it into CALL checkpoint(...) / force_checkpoint(...).
type CheckpointStatement struct {
	stmtBase
	Force   bool   `json:"force,omitempty"`
	Catalog string `json:"catalog,omitempty"`
}

func (s *CheckpointStatement) Children() []Node { return nil }

// TransactionKind mirrors DuckDB's TransactionType spelling.
type TransactionKind string

const (
	TransactionBegin    TransactionKind = "BEGIN_TRANSACTION"
	TransactionCommit   TransactionKind = "COMMIT"
	TransactionRollback TransactionKind = "ROLLBACK"
)

// TransactionModifier mirrors DuckDB's TransactionModifierType.
type TransactionModifier string

const (
	TransactionModifierNone TransactionModifier = ""
	TransactionReadOnly     TransactionModifier = "TRANSACTION_READ_ONLY"
	TransactionReadWrite    TransactionModifier = "TRANSACTION_READ_WRITE"
)

// TransactionStatement is BEGIN/COMMIT/ROLLBACK (with the START/END/ABORT
// spellings). Port of TransactionStatement/TransactionInfo.
type TransactionStatement struct {
	stmtBase
	Kind     TransactionKind     `json:"kind"`
	Modifier TransactionModifier `json:"modifier,omitempty"`
}

func (s *TransactionStatement) Children() []Node { return nil }

// VacuumStatement is VACUUM [options] [table [(columns)]]. Port of
// VacuumStatement/VacuumInfo; upstream errors NotImplemented on
// FULL/FREEZE/VERBOSE after parsing, so darkwing records the raw options
// and accepts.
type VacuumStatement struct {
	stmtBase
	Vacuum  bool `json:"vacuum,omitempty"`
	Analyze bool `json:"analyze,omitempty"`
	// Options is the raw option spelling list ("full", "freeze", ...).
	Options []string `json:"options,omitempty"`
	Table   TableRef `json:"table,omitempty"`
	Columns []string `json:"columns,omitempty"`
}

func (s *VacuumStatement) Children() []Node { return add(nil, s.Table) }

// AnalyzeStatement is ANALYZE [VERBOSE] [table [(columns)]]. Upstream
// desugars it into a VacuumStatement with the analyze option.
type AnalyzeStatement struct {
	stmtBase
	Verbose bool     `json:"verbose,omitempty"`
	Table   TableRef `json:"table,omitempty"`
	Columns []string `json:"columns,omitempty"`
}

func (s *AnalyzeStatement) Children() []Node { return add(nil, s.Table) }

// ExportStatement is EXPORT DATABASE [db TO] 'path' (options). Port of
// ExportStatement (whose payload is a CopyInfo; darkwing keeps the
// relevant fields flat).
type ExportStatement struct {
	stmtBase
	Database string `json:"database,omitempty"`
	Path     string `json:"path"`
	// Format defaults to csv like upstream.
	Format      string          `json:"format"`
	Options     []GenericOption `json:"options,omitempty"`
	NamedParams []NamedParam    `json:"named_param_map,omitempty"`
}

func (s *ExportStatement) Children() []Node { return optionExprs(nil, s.Options) }

// ImportStatement is IMPORT DATABASE 'path'. Upstream desugars it into
// PRAGMA import_database('path').
type ImportStatement struct {
	stmtBase
	Path string `json:"path"`
}

func (s *ImportStatement) Children() []Node { return nil }

// DeallocateStatement is DEALLOCATE [PREPARE] name. Upstream desugars it
// into DROP PREPARED_STATEMENT.
type DeallocateStatement struct {
	stmtBase
	Name string `json:"name"`
}

func (s *DeallocateStatement) Children() []Node { return nil }

// PrepareStatement is PREPARE name AS <statement>. Port of
// PrepareStatement.
type PrepareStatement struct {
	stmtBase
	Name      string `json:"name"`
	Statement Stmt   `json:"statement"`
}

func (s *PrepareStatement) Children() []Node { return add(nil, s.Statement) }

// ExecuteStatement is EXECUTE name(args). Port of ExecuteStatement:
// positional arguments are named by their 1-based position, mirroring
// upstream's named_values map (kept ordered here).
type ExecuteStatement struct {
	stmtBase
	Name        string       `json:"name"`
	Values      []NamedValue `json:"named_values,omitempty"`
	NamedParams []NamedParam `json:"named_param_map,omitempty"`
}

func (s *ExecuteStatement) Children() []Node {
	var nodes []Node
	for _, v := range s.Values {
		nodes = add(nodes, v.Expr)
	}
	return nodes
}

// ExplainType mirrors DuckDB's ExplainType.
type ExplainType string

const (
	ExplainStandard ExplainType = "EXPLAIN_STANDARD"
	ExplainAnalyze  ExplainType = "EXPLAIN_ANALYZE"
)

// ExplainStatement is EXPLAIN [ANALYZE] [(options)] <statement>. Port of
// ExplainStatement.
type ExplainStatement struct {
	stmtBase
	Statement Stmt        `json:"statement"`
	Type      ExplainType `json:"explain_type"`
	// Format is the FORMAT option value (lowercased), empty for default.
	Format      string       `json:"format,omitempty"`
	NamedParams []NamedParam `json:"named_param_map,omitempty"`
}

func (s *ExplainStatement) Children() []Node { return add(nil, s.Statement) }

// AttachStatement is ATTACH [DATABASE] 'path' [AS alias] [(options)].
// Port of AttachStatement/AttachInfo.
type AttachStatement struct {
	stmtBase
	// Path is the database path expression (usually a string constant).
	Path        Expr             `json:"path"`
	Alias       string           `json:"alias,omitempty"`
	OnConflict  OnCreateConflict `json:"on_conflict"`
	Options     []GenericOption  `json:"options,omitempty"`
	NamedParams []NamedParam     `json:"named_param_map,omitempty"`
}

func (s *AttachStatement) Children() []Node { return optionExprs(add(nil, s.Path), s.Options) }

// DetachStatement is DETACH [DATABASE] [IF EXISTS] name. Port of
// DetachStatement/DetachInfo.
type DetachStatement struct {
	stmtBase
	Name     string `json:"name"`
	IfExists bool   `json:"if_exists,omitempty"`
}

func (s *DetachStatement) Children() []Node { return nil }

// ConnectStatement is CONNECT [LOCAL | 'target' (options) | catalog].
// Port of ConnectStatement/ConnectInfo.
type ConnectStatement struct {
	stmtBase
	// Local is CONNECT LOCAL.
	Local bool   `json:"local,omitempty"`
	Name  string `json:"name,omitempty"`
	// NameIsString marks a string-literal target.
	NameIsString bool            `json:"name_is_string,omitempty"`
	Options      []GenericOption `json:"options,omitempty"`
	NamedParams  []NamedParam    `json:"named_param_map,omitempty"`
}

func (s *ConnectStatement) Children() []Node { return optionExprs(nil, s.Options) }

// DisconnectStatement is DISCONNECT. Port of DisconnectStatement.
type DisconnectStatement struct {
	stmtBase
}

func (s *DisconnectStatement) Children() []Node { return nil }

// LoadKind mirrors DuckDB's LoadType.
type LoadKind string

const (
	LoadTypeLoad         LoadKind = "LOAD"
	LoadTypeLoadAs       LoadKind = "LOAD_AS"
	LoadTypeInstall      LoadKind = "INSTALL"
	LoadTypeForceInstall LoadKind = "FORCE_INSTALL"
)

// LoadStatement is LOAD / INSTALL / FORCE INSTALL. Port of
// LoadStatement/LoadInfo (upstream models INSTALL as a load kind).
type LoadStatement struct {
	stmtBase
	Kind LoadKind `json:"kind"`
	// Name is the extension name or filename.
	Name string `json:"name"`
	// Alias is LOAD ... AS alias.
	Alias string `json:"alias,omitempty"`
	// Repository is INSTALL ... FROM <repo>; RepoIsAlias marks the
	// identifier (alias) form as opposed to a literal URL.
	Repository  string `json:"repository,omitempty"`
	RepoIsAlias bool   `json:"repo_is_alias,omitempty"`
	Version     string `json:"version,omitempty"`
}

func (s *LoadStatement) Children() []Node { return nil }

// UpdateExtensionsStatement is UPDATE EXTENSIONS [(names)]. Port of
// UpdateExtensionsStatement.
type UpdateExtensionsStatement struct {
	stmtBase
	Extensions []string `json:"extensions,omitempty"`
}

func (s *UpdateExtensionsStatement) Children() []Node { return nil }

// ExternalResourceOperation mirrors DuckDB's ExternalResourceOperation.
type ExternalResourceOperation string

const (
	ExternalResourceCreate   ExternalResourceOperation = "CREATE"
	ExternalResourceRegister ExternalResourceOperation = "REGISTER"
	ExternalResourceDestroy  ExternalResourceOperation = "DESTROY"
	ExternalResourceShow     ExternalResourceOperation = "SHOW"
)

// ExternalResourceStatement is CREATE/REGISTER/DESTROY EXTERNAL RESOURCE
// and SHOW [ALL] EXTERNAL RESOURCES. Port of ExternalResourceStatement.
type ExternalResourceStatement struct {
	stmtBase
	Operation ExternalResourceOperation `json:"operation"`
	// Type is the resource type string literal of CREATE/REGISTER.
	Type string `json:"type,omitempty"`
	Name string `json:"name,omitempty"`
	// Handle is the REGISTER ... FROM expression.
	Handle  Expr            `json:"handle,omitempty"`
	Options []GenericOption `json:"options,omitempty"`
	// All is SHOW ALL EXTERNAL RESOURCES.
	All         bool         `json:"all,omitempty"`
	NamedParams []NamedParam `json:"named_param_map,omitempty"`
}

func (s *ExternalResourceStatement) Children() []Node {
	return optionExprs(add(nil, s.Handle), s.Options)
}

// CommentOnType names the object family a COMMENT ON statement targets.
type CommentOnType string

const (
	CommentOnTable      CommentOnType = "TABLE"
	CommentOnSequence   CommentOnType = "SEQUENCE"
	CommentOnMacro      CommentOnType = "MACRO"
	CommentOnMacroTable CommentOnType = "MACRO_TABLE"
	CommentOnView       CommentOnType = "VIEW"
	CommentOnDatabase   CommentOnType = "DATABASE"
	CommentOnIndex      CommentOnType = "INDEX"
	CommentOnSchema     CommentOnType = "SCHEMA"
	CommentOnTypeEntry  CommentOnType = "TYPE"
	CommentOnColumn     CommentOnType = "COLUMN"
)

// CommentOnStatement is COMMENT ON <object> <name> IS <value>. Upstream
// desugars it into ALTER with a SetCommentInfo; darkwing keeps the
// statement. For COMMENT ON COLUMN the column name is split into Column
// and the table path fills Catalog/Schema/Name.
type CommentOnStatement struct {
	stmtBase
	OnType  CommentOnType `json:"on_type"`
	Catalog string        `json:"catalog,omitempty"`
	Schema  string        `json:"schema,omitempty"`
	Name    string        `json:"name"`
	Column  string        `json:"column,omitempty"`
	// Value is the comment constant: a string or NULL.
	Value Expr `json:"value"`
}

func (s *CommentOnStatement) Children() []Node { return add(nil, s.Value) }

// CopyStatement is COPY table/select TO/FROM file. Port of
// CopyStatement/CopyInfo.
type CopyStatement struct {
	stmtBase
	Info        *CopyInfo    `json:"info"`
	NamedParams []NamedParam `json:"named_param_map,omitempty"`
}

func (s *CopyStatement) Children() []Node { return add(nil, s.Info) }

// CopyInfo is the payload of COPY (and, upstream, EXPORT). Port of
// CopyInfo.
type CopyInfo struct {
	spanned
	Catalog string `json:"catalog,omitempty"`
	Schema  string `json:"schema,omitempty"`
	Table   string `json:"table,omitempty"`
	// Columns is the copied column list.
	Columns []string `json:"select_list,omitempty"`
	// IsFrom is COPY ... FROM (true) vs COPY ... TO (false).
	IsFrom bool `json:"is_from,omitempty"`
	// FilePath is the literal file name; FilePathExpr is set instead when
	// the file name is a non-constant expression.
	FilePath     string `json:"file_path,omitempty"`
	FilePathExpr Expr   `json:"file_path_expression,omitempty"`
	// Format is the explicit or extension-derived format; empty means
	// auto-detected downstream.
	Format string `json:"format,omitempty"`
	// FormatExplicit marks a FORMAT option (upstream's
	// !is_format_auto_detected).
	FormatExplicit bool            `json:"format_explicit,omitempty"`
	Options        []GenericOption `json:"options,omitempty"`
	// Query is the COPY (SELECT ...) TO source.
	Query QueryNode `json:"select_statement,omitempty"`
}

func (i *CopyInfo) Children() []Node {
	nodes := add(nil, i.FilePathExpr)
	nodes = optionExprs(nodes, i.Options)
	return add(nodes, i.Query)
}

// CopyDatabaseType mirrors DuckDB's CopyDatabaseType.
type CopyDatabaseType string

const (
	CopyDatabaseSchema CopyDatabaseType = "COPY_SCHEMA"
	CopyDatabaseData   CopyDatabaseType = "COPY_DATA"
)

// CopyDatabaseStatement is COPY FROM DATABASE a TO b (SCHEMA|DATA). Port
// of CopyDatabaseStatement; the flagless form desugars into PRAGMA
// copy_database upstream and here.
type CopyDatabaseStatement struct {
	stmtBase
	FromDatabase string           `json:"from_database"`
	ToDatabase   string           `json:"to_database"`
	CopyType     CopyDatabaseType `json:"copy_type"`
}

func (s *CopyDatabaseStatement) Children() []Node { return nil }

// ---- CREATE MACRO / SECRET / TRIGGER payloads --------------------------

// MacroParameter is one macro parameter: a name, an optional type, and an
// optional default (`x := expr`).
type MacroParameter struct {
	Name    string          `json:"name"`
	Type    *TypeExpression `json:"type,omitempty"`
	Default Expr            `json:"default,omitempty"`
}

// MacroFunction is one AS (...) definition of CREATE MACRO: a scalar
// expression or a TABLE query. Port of ScalarMacroFunction /
// TableMacroFunction.
type MacroFunction struct {
	spanned
	Parameters []MacroParameter `json:"parameters"`
	// Expr is the scalar macro body; Query is the table macro body.
	Expr  Expr             `json:"expression,omitempty"`
	Query *SelectStatement `json:"query,omitempty"`
}

func (m *MacroFunction) Children() []Node {
	var nodes []Node
	for i := range m.Parameters {
		if m.Parameters[i].Type != nil {
			nodes = add(nodes, m.Parameters[i].Type)
		}
		nodes = add(nodes, m.Parameters[i].Default)
	}
	return add(nodes, m.Expr, m.Query)
}

// CreateMacroInfo is CREATE MACRO / FUNCTION. Port of CreateMacroInfo.
type CreateMacroInfo struct {
	CreateInfoBase
	// IsTable marks table macros (every definition is AS TABLE).
	IsTable bool             `json:"is_table,omitempty"`
	Macros  []*MacroFunction `json:"macros"`
}

func (i *CreateMacroInfo) Children() []Node {
	var nodes []Node
	for _, m := range i.Macros {
		nodes = add(nodes, m)
	}
	return nodes
}

// CreateSecretInfo is CREATE SECRET. Port of CreateSecretInfo: TYPE,
// PROVIDER and SCOPE are pulled out of the option list, the rest stays in
// Options.
type CreateSecretInfo struct {
	CreateInfoBase
	// Storage is the IN <storage> specifier (lowercased).
	Storage  string          `json:"storage,omitempty"`
	Type     Expr            `json:"type,omitempty"`
	Provider Expr            `json:"provider,omitempty"`
	Scope    Expr            `json:"scope,omitempty"`
	Options  []GenericOption `json:"options,omitempty"`
}

func (i *CreateSecretInfo) Children() []Node {
	return optionExprs(add(nil, i.Type, i.Provider, i.Scope), i.Options)
}

// TriggerTiming mirrors DuckDB's TriggerTiming.
type TriggerTiming string

const (
	TriggerBefore    TriggerTiming = "BEFORE"
	TriggerAfter     TriggerTiming = "AFTER"
	TriggerInsteadOf TriggerTiming = "INSTEAD_OF"
)

// TriggerEvent mirrors DuckDB's TriggerEventType.
type TriggerEvent string

const (
	TriggerEventInsert TriggerEvent = "INSERT"
	TriggerEventDelete TriggerEvent = "DELETE"
	TriggerEventUpdate TriggerEvent = "UPDATE"
)

// TriggerForEach mirrors DuckDB's TriggerForEach.
type TriggerForEach string

const (
	TriggerForEachRow       TriggerForEach = "ROW"
	TriggerForEachStatement TriggerForEach = "STATEMENT"
)

// CreateTriggerInfo is CREATE TRIGGER. Port of CreateTriggerInfo.
type CreateTriggerInfo struct {
	CreateInfoBase
	Timing TriggerTiming `json:"timing"`
	Event  TriggerEvent  `json:"event"`
	// Columns is UPDATE OF (columns).
	Columns []string `json:"columns,omitempty"`
	// Table is the ON target.
	Table *BaseTableRef `json:"table"`
	// ReferencingNew/Old are the REFERENCING NEW/OLD TABLE AS aliases.
	ReferencingNew string `json:"referencing_new_table,omitempty"`
	ReferencingOld string `json:"referencing_old_table,omitempty"`
	// ForEach is empty when no FOR EACH clause is given.
	ForEach TriggerForEach `json:"for_each,omitempty"`
	// Body is the trigger action (INSERT/UPDATE/DELETE/MERGE).
	Body Stmt `json:"body"`
}

func (i *CreateTriggerInfo) Children() []Node {
	var nodes []Node
	if i.Table != nil {
		nodes = add(nodes, i.Table)
	}
	return add(nodes, i.Body)
}
