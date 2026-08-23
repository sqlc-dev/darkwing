// transform_misc.go: the milestone-5 statement transformers — SET/RESET,
// PRAGMA, CALL, USE, CHECKPOINT, transactions, VACUUM/ANALYZE,
// EXPORT/IMPORT, prepared statements, EXPLAIN, ATTACH/DETACH/CONNECT,
// COMMENT ON and the extension statements. Port of upstream's
// transformer/transform_set.cpp, transform_pragma.cpp, transform_call.cpp,
// transform_use.cpp, transform_checkpoint.cpp, transform_transaction.cpp,
// transform_vacuum.cpp, transform_analyze.cpp, transform_export.cpp,
// transform_prepare.cpp, transform_execute.cpp, transform_explain.cpp,
// transform_attach.cpp, transform_detach.cpp, transform_connect.cpp,
// transform_comment.cpp, transform_deallocate.cpp, transform_load.cpp and
// transform_external_resource.cpp.
//
// Upstream raises two flavors of error from these transformers:
// ParserException (a parse-time reject, reproduced here with raise) and
// NotImplementedException/InvalidInputException/BinderException (post-parse
// classification for the corpus oracle, so darkwing records the construct
// and accepts). Each site is annotated.
package parser

import (
	"strconv"
	"strings"

	"github.com/sqlc-dev/darkwing/ast"
)

func init() {
	register("SetStatement", func(tc *transformContext, n tnode) ast.Stmt {
		return tc.transformSet(n)
	})
	register("ResetStatement", func(tc *transformContext, n tnode) ast.Stmt {
		return tc.transformReset(n)
	})
	register("PragmaStatement", func(tc *transformContext, n tnode) ast.Stmt {
		return tc.transformPragma(n)
	})
	register("CallStatement", func(tc *transformContext, n tnode) ast.Stmt {
		return tc.transformCall(n)
	})
	register("UseStatement", func(tc *transformContext, n tnode) ast.Stmt {
		return tc.transformUse(n)
	})
	register("CheckpointStatement", func(tc *transformContext, n tnode) ast.Stmt {
		return tc.transformCheckpoint(n)
	})
	register("TransactionStatement", func(tc *transformContext, n tnode) ast.Stmt {
		return tc.transformTransaction(n)
	})
	register("VacuumStatement", func(tc *transformContext, n tnode) ast.Stmt {
		return tc.transformVacuum(n)
	})
	register("AnalyzeStatement", func(tc *transformContext, n tnode) ast.Stmt {
		return tc.transformAnalyze(n)
	})
	register("ExportStatement", func(tc *transformContext, n tnode) ast.Stmt {
		return tc.transformExport(n)
	})
	register("ImportStatement", func(tc *transformContext, n tnode) ast.Stmt {
		stmt := &ast.ImportStatement{Path: identifierText(n.child(2))}
		stmt.SetSpan(n.span())
		return stmt
	})
	register("DeallocateStatement", func(tc *transformContext, n tnode) ast.Stmt {
		// 'DEALLOCATE' DeallocatePrepare? Identifier
		stmt := &ast.DeallocateStatement{Name: identifierText(n.child(2))}
		stmt.SetSpan(n.span())
		return stmt
	})
	register("PrepareStatement", func(tc *transformContext, n tnode) ast.Stmt {
		return tc.transformPrepare(n)
	})
	register("ExecuteStatement", func(tc *transformContext, n tnode) ast.Stmt {
		return tc.transformExecute(n)
	})
	register("ExplainStatement", func(tc *transformContext, n tnode) ast.Stmt {
		return tc.transformExplain(n)
	})
	register("AttachStatement", func(tc *transformContext, n tnode) ast.Stmt {
		return tc.transformAttach(n)
	})
	register("DetachStatement", func(tc *transformContext, n tnode) ast.Stmt {
		// 'DETACH' Database? IfExists? CatalogName
		stmt := &ast.DetachStatement{Name: identifierText(n.child(3))}
		_, stmt.IfExists = n.child(2).opt()
		stmt.SetSpan(n.span())
		return stmt
	})
	register("ConnectStatement", func(tc *transformContext, n tnode) ast.Stmt {
		return tc.transformConnect(n)
	})
	register("DisconnectStatement", func(tc *transformContext, n tnode) ast.Stmt {
		stmt := &ast.DisconnectStatement{}
		stmt.SetSpan(n.span())
		return stmt
	})
	register("CommentStatement", func(tc *transformContext, n tnode) ast.Stmt {
		return tc.transformComment(n)
	})
	register("LoadStatement", func(tc *transformContext, n tnode) ast.Stmt {
		return tc.transformLoad(n)
	})
	register("InstallStatement", func(tc *transformContext, n tnode) ast.Stmt {
		return tc.transformInstall(n)
	})
	register("UpdateExtensionsStatement", func(tc *transformContext, n tnode) ast.Stmt {
		// 'UPDATE' 'EXTENSIONS' Parens(List(Identifier))?
		stmt := &ast.UpdateExtensionsStatement{}
		stmt.SetSpan(n.span())
		if list, ok := n.child(2).opt(); ok {
			stmt.Extensions = identifierList(list.parens())
		}
		return stmt
	})
	register("ExternalResourceStatement", func(tc *transformContext, n tnode) ast.Stmt {
		return tc.transformExternalResource(n)
	})
}

// ---- SET / RESET -------------------------------------------------------

// settingTarget is the (name, scope) pair of SetVariableOrSetting.
// SetVariableOrSetting <- SetVariable / SetSetting
func (tc *transformContext) transformSettingTarget(n tnode) (string, ast.SetScope) {
	_, alt := n.sole().choice()
	switch alt.name() {
	case "SetVariable":
		// VariableScope Identifier
		return identifierText(alt.child(1)), ast.SetScopeVariable
	case "SetSetting":
		// SettingScope? SettingName
		scope := ast.SetScopeAutomatic
		if sc, ok := alt.child(0).opt(); ok {
			_, kind := sc.sole().choice()
			switch kind.name() {
			case "LocalScope":
				scope = ast.SetScopeLocal
			case "SessionScope":
				scope = ast.SetScopeSession
			case "GlobalScope":
				scope = ast.SetScopeGlobal
			}
		}
		return identifierText(alt.child(1)), scope
	}
	shapeError(alt, "unknown setting target")
	return "", ast.SetScopeAutomatic
}

// SetStatement <- 'SET' SetAssignmentOrTimeZone
func (tc *transformContext) transformSet(n tnode) ast.Stmt {
	sp := n.span()
	_, alt := n.child(1).sole().choice()
	switch alt.name() {
	case "SetSchema":
		// 'SCHEMA' StringLiteral
		stmt := &ast.SetStatement{Name: "schema", Scope: ast.SetScopeAutomatic}
		stmt.Value = constExpr(invalidSpan, varcharValue(identifierText(alt.child(1))))
		stmt.SetSpan(sp)
		return stmt
	case "StandardAssignment":
		// SetVariableOrSetting SetAssignment
		name, scope := tc.transformSettingTarget(alt.child(0))
		// SET LOCAL raises NotImplemented upstream (post-parse for the
		// corpus); darkwing keeps the scope and accepts.
		// SetAssignment <- VariableAssign VariableList
		values := tc.transformExpressionList(alt.child(1).child(1))
		if len(values) > 1 {
			raise("SET can only contain a single value")
		}
		value := values[0]
		switch v := value.(type) {
		case *ast.ColumnRefExpression:
			// a bare identifier value is its name as a string
			value = constExpr(invalidSpan, varcharValue(v.ColumnNames[len(v.ColumnNames)-1]))
		case *ast.DefaultExpression:
			stmt := &ast.ResetStatement{Name: name, Scope: scope}
			stmt.SetSpan(sp)
			return stmt
		}
		stmt := &ast.SetStatement{Name: name, Scope: scope, Value: value}
		stmt.SetSpan(sp)
		return stmt
	case "SetTimeZone":
		// 'TIME' 'ZONE' ZoneValue
		value := tc.transformZoneValue(alt.child(2))
		if _, isDefault := value.(*ast.DefaultExpression); isDefault {
			stmt := &ast.ResetStatement{Name: "timezone", Scope: ast.SetScopeAutomatic}
			stmt.SetSpan(sp)
			return stmt
		}
		stmt := &ast.SetStatement{Name: "timezone", Scope: ast.SetScopeAutomatic, Value: value}
		stmt.SetSpan(sp)
		return stmt
	}
	shapeError(alt, "unknown SET form")
	return nil
}

// ZoneValue <- ZoneIntervalWithPrecision / ZoneIntervalWithInterval /
// ZoneLocal / ZoneDefault / ZoneStringLiteral / ZoneIdentifier / NumberLiteral
func (tc *transformContext) transformZoneValue(n tnode) ast.Expr {
	_, alt := n.sole().choice()
	switch alt.name() {
	case "ZoneLocal", "ZoneDefault":
		d := &ast.DefaultExpression{}
		d.SetSpan(invalidSpan)
		return d
	case "ZoneStringLiteral", "ZoneIdentifier":
		return constExpr(invalidSpan, varcharValue(identifierText(alt.sole())))
	case "ZoneIntervalWithInterval":
		// 'INTERVAL' StringLiteral Interval? -> CAST(str AS INTERVAL)
		return intervalCast(identifierText(alt.child(1)))
	case "ZoneIntervalWithPrecision":
		// 'INTERVAL' Parens(NumberLiteral) StringLiteral
		return intervalCast(identifierText(alt.child(2)))
	}
	// the last alternative is a bare NumberLiteral token
	return numberConstant(invalidSpan, alt.number())
}

func intervalCast(s string) ast.Expr {
	cast := &ast.CastExpression{
		Child:        constExpr(invalidSpan, varcharValue(s)),
		ResolvedType: "INTERVAL",
	}
	cast.SetSpan(invalidSpan)
	return cast
}

// ResetStatement <- 'RESET' SetVariableOrSetting. RESET LOCAL raises
// NotImplemented upstream (post-parse); darkwing keeps the scope.
func (tc *transformContext) transformReset(n tnode) ast.Stmt {
	name, scope := tc.transformSettingTarget(n.child(1))
	stmt := &ast.ResetStatement{Name: name, Scope: scope}
	stmt.SetSpan(n.span())
	return stmt
}

// ---- PRAGMA ------------------------------------------------------------

// sqlitePragmas are parsed as pragma calls even in assignment form, for
// SQLite compatibility (upstream's sqlite_compat_pragmas).
var sqlitePragmas = map[string]bool{"table_info": true}

// PragmaStatement <- 'PRAGMA' PragmaAssignOrFunction
func (tc *transformContext) transformPragma(n tnode) ast.Stmt {
	sp := n.span()
	_, alt := n.child(1).sole().choice()
	switch alt.name() {
	case "PragmaAssign":
		// SettingName '=' VariableList
		name := identifierText(alt.child(0))
		values := tc.transformExpressionList(alt.child(2))
		if len(values) != 1 {
			raise("PRAGMA statement with assignment should contain exactly one parameter")
		}
		value := pragmaParamValue(values[0])
		if sqlitePragmas[strings.ToLower(name)] {
			stmt := &ast.PragmaStatement{Name: name, Parameters: []ast.Expr{value}}
			stmt.SetSpan(sp)
			return stmt
		}
		// other assignment pragmas are SET statements upstream
		stmt := &ast.SetStatement{Name: name, Scope: ast.SetScopeAutomatic, Value: value}
		stmt.SetSpan(sp)
		return stmt
	case "PragmaFunction":
		// PragmaName PragmaParameters?
		stmt := &ast.PragmaStatement{Name: identifierText(alt.child(0))}
		stmt.SetSpan(sp)
		if params, ok := alt.child(1).opt(); ok {
			for _, e := range tc.transformExpressionList(params.parens()) {
				if cmp, isCmp := e.(*ast.ComparisonExpression); isCmp && cmp.Type == ast.CompareEqual {
					ref, isRef := cmp.Left.(*ast.ColumnRefExpression)
					if !isRef {
						raise("Named parameter requires a column reference on the LHS")
					}
					name := ref.ColumnNames[len(ref.ColumnNames)-1]
					stmt.NamedParameters = append(stmt.NamedParameters, ast.NamedValue{Name: name, Expr: cmp.Right})
					continue
				}
				stmt.Parameters = append(stmt.Parameters, pragmaParamValue(e))
			}
		}
		return stmt
	}
	shapeError(alt, "unknown pragma form")
	return nil
}

// pragmaParamValue folds bare identifiers into string constants the way
// upstream does for PRAGMA parameters.
func pragmaParamValue(e ast.Expr) ast.Expr {
	ref, ok := e.(*ast.ColumnRefExpression)
	if !ok {
		return e
	}
	if len(ref.ColumnNames) == 1 {
		return constExpr(invalidSpan, varcharValue(ref.ColumnNames[0]))
	}
	return constExpr(invalidSpan, varcharValue(strings.Join(ref.ColumnNames, ".")))
}

// ---- CALL / USE / CHECKPOINT ------------------------------------------

// CallStatement <- 'CALL' QualifiedTableFunction TableFunctionArguments
func (tc *transformContext) transformCall(n tnode) ast.Stmt {
	stmt := &ast.CallStatement{Function: tc.qualifiedFunctionExpr(n.child(1), n.child(2))}
	stmt.SetSpan(n.span())
	return stmt
}

// UseStatement <- 'USE' UseTarget
func (tc *transformContext) transformUse(n tnode) ast.Stmt {
	stmt := &ast.UseStatement{}
	stmt.SetSpan(n.span())
	_, alt := n.child(1).sole().choice()
	switch alt.name() {
	case "SchemaNameAsUseTarget", "CatalogNameAsUseTarget":
		stmt.Name = identifierText(alt.sole())
	case "UseTargetCatalogSchema":
		// CatalogName '.' ReservedSchemaName DotIdentifier*
		if len(alt.child(3).repeat()) > 0 {
			raise("Expected \"USE database\" or \"USE database.schema\"")
		}
		stmt.Catalog = identifierText(alt.child(0))
		stmt.Name = identifierText(alt.child(2))
	default:
		shapeError(alt, "unknown USE target")
	}
	return stmt
}

// CheckpointStatement <- CheckpointForce? 'CHECKPOINT' CatalogName?
// Upstream desugars into CALL [force_]checkpoint(catalog?).
func (tc *transformContext) transformCheckpoint(n tnode) ast.Stmt {
	stmt := &ast.CheckpointStatement{}
	stmt.SetSpan(n.span())
	_, stmt.Force = n.child(0).opt()
	if cat, ok := n.child(2).opt(); ok {
		stmt.Catalog = identifierText(cat)
	}
	return stmt
}

// ---- transactions ------------------------------------------------------

// TransactionStatement <- BeginTransaction / RollbackTransaction / CommitTransaction
func (tc *transformContext) transformTransaction(n tnode) ast.Stmt {
	stmt := &ast.TransactionStatement{}
	stmt.SetSpan(n.span())
	_, alt := n.sole().choice()
	switch alt.name() {
	case "BeginTransaction":
		// StartOrBegin Transaction? ReadOrWrite?
		stmt.Kind = ast.TransactionBegin
		if rw, ok := alt.child(2).opt(); ok {
			// ReadOrWrite <- 'READ' ReadOnlyOrReadWrite
			_, kind := rw.child(1).sole().choice()
			if kind.name() == "ReadOnly" {
				stmt.Modifier = ast.TransactionReadOnly
			} else {
				stmt.Modifier = ast.TransactionReadWrite
			}
		}
	case "CommitTransaction":
		stmt.Kind = ast.TransactionCommit
	case "RollbackTransaction":
		stmt.Kind = ast.TransactionRollback
	default:
		shapeError(alt, "unknown transaction form")
	}
	return stmt
}

// ---- VACUUM / ANALYZE --------------------------------------------------

// analyzeTarget maps AnalyzeTarget (BaseTableName NameList?).
func (tc *transformContext) transformAnalyzeTarget(n tnode) (ast.TableRef, []string) {
	ref := &ast.BaseTableRef{}
	ref.SetSpan(n.child(0).span())
	tc.fillBaseTableName(ref, n.child(0))
	var cols []string
	if list, ok := n.child(1).opt(); ok {
		cols = identifierList(list.parens())
	}
	return ref, cols
}

// VacuumStatement <- 'VACUUM' VacuumOptions? AnalyzeTarget?
// FULL/FREEZE/VERBOSE and unknown parenthesized options raise
// NotImplemented upstream (post-parse); darkwing records the raw options.
func (tc *transformContext) transformVacuum(n tnode) ast.Stmt {
	stmt := &ast.VacuumStatement{}
	stmt.SetSpan(n.span())
	if opts, ok := n.child(1).opt(); ok {
		stmt.Vacuum = true
		_, alt := opts.sole().choice()
		switch alt.name() {
		case "VacuumParensOptions":
			for _, o := range alt.sole().parens().listElems() {
				_, opt := o.sole().choice()
				var name string
				switch opt.name() {
				case "OptAnalyze":
					name = "analyze"
				case "OptFull":
					name = "full"
				case "OptFreeze":
					name = "freeze"
				case "OptVerbose":
					name = "verbose"
				default:
					name = identifierText(opt)
				}
				stmt.Options = append(stmt.Options, name)
				if strings.EqualFold(name, "analyze") {
					stmt.Analyze = true
				}
			}
		case "VacuumLegacyOptions":
			// OptFull? OptFreeze? OptVerbose? OptAnalyze?
			for i, name := range []string{"full", "freeze", "verbose", "analyze"} {
				if _, ok := alt.child(i).opt(); ok {
					stmt.Options = append(stmt.Options, name)
					if name == "analyze" {
						stmt.Analyze = true
					}
				}
			}
		default:
			shapeError(alt, "unknown vacuum options")
		}
	}
	if target, ok := n.child(2).opt(); ok {
		stmt.Table, stmt.Columns = tc.transformAnalyzeTarget(target)
	}
	return stmt
}

// AnalyzeStatement <- AnalyzeKeyword AnalyzeVerbose? AnalyzeTarget?
// Upstream desugars into VACUUM with the analyze option; ANALYZE VERBOSE
// raises NotImplemented upstream (post-parse).
func (tc *transformContext) transformAnalyze(n tnode) ast.Stmt {
	stmt := &ast.AnalyzeStatement{}
	stmt.SetSpan(n.span())
	_, stmt.Verbose = n.child(1).opt()
	if target, ok := n.child(2).opt(); ok {
		stmt.Table, stmt.Columns = tc.transformAnalyzeTarget(target)
	}
	return stmt
}

// ---- EXPORT ------------------------------------------------------------

// ExportStatement <- 'EXPORT' 'DATABASE' ExportSource? StringLiteral GenericCopyOptionList?
func (tc *transformContext) transformExport(n tnode) ast.Stmt {
	stmt := &ast.ExportStatement{Format: "csv"}
	stmt.SetSpan(n.span())
	if src, ok := n.child(2).opt(); ok {
		// ExportSource <- CatalogName 'TO'
		stmt.Database = identifierText(src.child(0))
	}
	stmt.Path = identifierText(n.child(3))
	if opts, ok := n.child(4).opt(); ok {
		for _, option := range tc.transformGenericOptionList(opts) {
			if strings.EqualFold(option.Name, "format") {
				if option.Expr != nil {
					raise("Unsupported parameter type for FORMAT: expected e.g. FORMAT 'csv', 'parquet'")
				}
				if len(option.Values) == 0 {
					raise("FORMAT requires a parameter, e.g. FORMAT 'csv' or FORMAT 'parquet'")
				}
				stmt.Format = option.Values[0].Str
				continue
			}
			stmt.Options = append(stmt.Options, option)
		}
	}
	return stmt
}

// ---- PREPARE / EXECUTE -------------------------------------------------

// statementTypeName names a statement the way upstream's StatementType
// spellings do, for transformer error messages.
func statementTypeName(stmt ast.Stmt) string {
	switch stmt.(type) {
	case *ast.SelectStatement:
		return "SELECT_STATEMENT"
	case *ast.InsertStatement:
		return "INSERT_STATEMENT"
	case *ast.UpdateStatement:
		return "UPDATE_STATEMENT"
	case *ast.DeleteStatement, *ast.TruncateStatement:
		return "DELETE_STATEMENT"
	case *ast.CopyStatement:
		return "COPY_STATEMENT"
	case *ast.CreateStatement:
		return "CREATE_STATEMENT"
	case *ast.DropStatement, *ast.DeallocateStatement:
		return "DROP_STATEMENT"
	case *ast.AlterStatement, *ast.CommentOnStatement:
		return "ALTER_STATEMENT"
	case *ast.MergeIntoStatement:
		return "MERGE_INTO_STATEMENT"
	case *ast.TransactionStatement:
		return "TRANSACTION_STATEMENT"
	case *ast.PragmaStatement, *ast.ImportStatement:
		return "PRAGMA_STATEMENT"
	case *ast.SetStatement, *ast.ResetStatement, *ast.UseStatement:
		return "SET_STATEMENT"
	case *ast.CallStatement, *ast.CheckpointStatement:
		return "CALL_STATEMENT"
	case *ast.VacuumStatement, *ast.AnalyzeStatement:
		return "VACUUM_STATEMENT"
	case *ast.ExplainStatement:
		return "EXPLAIN_STATEMENT"
	case *ast.PrepareStatement:
		return "PREPARE_STATEMENT"
	case *ast.ExecuteStatement:
		return "EXECUTE_STATEMENT"
	case *ast.ExportStatement:
		return "EXPORT_STATEMENT"
	case *ast.AttachStatement:
		return "ATTACH_STATEMENT"
	case *ast.DetachStatement:
		return "DETACH_STATEMENT"
	case *ast.LoadStatement:
		return "LOAD_STATEMENT"
	case *ast.CopyDatabaseStatement:
		return "COPY_DATABASE_STATEMENT"
	case *ast.UpdateExtensionsStatement:
		return "UPDATE_EXTENSIONS_STATEMENT"
	default:
		return "INVALID_STATEMENT"
	}
}

// preparableStatement mirrors upstream's IsPrepareableStatement:
// SELECT/INSERT/UPDATE/DELETE/COPY (TRUNCATE is a delete upstream).
func preparableStatement(stmt ast.Stmt) bool {
	switch stmt.(type) {
	case *ast.SelectStatement, *ast.InsertStatement, *ast.UpdateStatement,
		*ast.DeleteStatement, *ast.TruncateStatement, *ast.CopyStatement:
		return true
	}
	return false
}

// PrepareStatement <- 'PREPARE' Identifier TypeList? 'AS' Statement
// A TypeList raises NotImplemented upstream (post-parse); darkwing
// ignores it. The inner statement's parameters belong to EXECUTE, so the
// collected parameter state is cleared, mirroring upstream's
// ClearParameters.
func (tc *transformContext) transformPrepare(n tnode) ast.Stmt {
	stmt := &ast.PrepareStatement{Name: identifierText(n.child(1))}
	stmt.SetSpan(n.span())
	inner := tc.transformStatement(n.child(4))
	if !preparableStatement(inner) {
		raise("%s is not a preparable statement", statementTypeName(inner))
	}
	stmt.Statement = inner
	tc.paramCount = 0
	tc.paramSeen = map[string]int{}
	tc.paramOrder = nil
	tc.hasNamed, tc.hasPosition = false, false
	return stmt
}

// ExecuteStatement <- 'EXECUTE' Identifier TableFunctionArguments?
// Non-scalar and mixed named/positional arguments raise
// InvalidInput/NotImplemented upstream (post-parse); darkwing records
// the arguments as written.
func (tc *transformContext) transformExecute(n tnode) ast.Stmt {
	stmt := &ast.ExecuteStatement{Name: identifierText(n.child(1))}
	stmt.SetSpan(n.span())
	args, ok := n.child(2).opt()
	if !ok {
		return stmt
	}
	positional := 0
	if list, hasArgs := args.sole().parens().opt(); hasArgs {
		for _, a := range list.listElems() {
			arg := tc.transformFunctionArgument(a)
			name := arg.Name
			if name == "" {
				positional++
				name = strconv.Itoa(positional)
			}
			arg.Expr.SetAlias("")
			stmt.Values = append(stmt.Values, ast.NamedValue{Name: name, Expr: arg.Expr})
		}
	}
	return stmt
}

// ---- EXPLAIN -----------------------------------------------------------

// ExplainStatement <- 'EXPLAIN' AnalyzeKeyword? ExplainOptionList? ExplainableStatements
// Unknown options and malformed FORMAT arguments raise
// NotImplemented/InvalidInput upstream (post-parse); darkwing records
// what it recognizes and accepts.
func (tc *transformContext) transformExplain(n tnode) ast.Stmt {
	stmt := &ast.ExplainStatement{Type: ast.ExplainStandard}
	stmt.SetSpan(n.span())
	if _, ok := n.child(1).opt(); ok {
		stmt.Type = ast.ExplainAnalyze
	}
	if opts, ok := n.child(2).opt(); ok {
		// ExplainOptionList <- Parens(List(ExplainOption))
		for _, o := range opts.sole().parens().listElems() {
			// ExplainOption <- ExplainOptionName Expression?
			name := strings.ToLower(identifierText(o.child(0)))
			option := ast.GenericOption{Name: name}
			if e, hasExpr := o.child(1).opt(); hasExpr {
				setGenericOptionExpr(&option, tc.transformExpression(e))
			}
			switch name {
			case "format":
				if len(option.Values) > 0 && option.Values[0].Kind == ast.ValueString {
					stmt.Format = strings.ToLower(option.Values[0].Str)
				}
			case "analyze":
				stmt.Type = ast.ExplainAnalyze
			}
		}
	}
	// ExplainableStatements is a choice of statement rules (with
	// ExplainSelectStatement wrapping SelectStatementInternal).
	_, inner := n.child(3).sole().choice()
	if inner.name() == "ExplainSelectStatement" {
		stmt.Statement = tc.transformSelectInternalStatement(inner.sole())
	} else if fn, ok := statementTransforms[inner.name()]; ok {
		stmt.Statement = fn(tc, inner)
	} else {
		shapeError(inner, "unknown explainable statement")
	}
	return stmt
}

// ---- ATTACH / CONNECT --------------------------------------------------

// checkSingleValuedOptions ports SplitGenericOptions' parse-time check:
// an option folded to constants can carry at most one argument.
func checkSingleValuedOptions(opts []ast.GenericOption) {
	for _, o := range opts {
		if o.Expr == nil && len(o.Values) > 1 {
			raise("Option %s can only have one argument", o.Name)
		}
	}
}

// AttachStatement <- 'ATTACH' OrReplace? IfNotExists? Database? DatabasePath AttachAlias? AttachOptions?
func (tc *transformContext) transformAttach(n tnode) ast.Stmt {
	stmt := &ast.AttachStatement{OnConflict: ast.CreateError}
	stmt.SetSpan(n.span())
	_, orReplace := n.child(1).opt()
	_, ifNotExists := n.child(2).opt()
	if orReplace && ifNotExists {
		raise("Cannot specify both OR REPLACE and IF NOT EXISTS at the same time")
	}
	if orReplace {
		stmt.OnConflict = ast.CreateReplace
	} else if ifNotExists {
		stmt.OnConflict = ast.CreateIgnore
	}
	stmt.Path = tc.transformExpression(n.child(4).sole())
	if alias, ok := n.child(5).opt(); ok {
		// AttachAlias <- 'AS' ColId
		stmt.Alias = identifierText(alias.child(1))
	}
	if opts, ok := n.child(6).opt(); ok {
		stmt.Options = tc.transformGenericOptionList(opts.sole())
		checkSingleValuedOptions(stmt.Options)
	}
	return stmt
}

// ConnectStatement <- 'CONNECT' SessionTarget?
func (tc *transformContext) transformConnect(n tnode) ast.Stmt {
	stmt := &ast.ConnectStatement{}
	stmt.SetSpan(n.span())
	target, ok := n.child(1).opt()
	if !ok {
		return stmt
	}
	_, alt := target.sole().choice()
	switch alt.name() {
	case "LocalSessionTarget":
		stmt.Local = true
	case "StringSessionTarget":
		// StringLiteral GenericCopyOptionList?
		stmt.Name = identifierText(alt.child(0))
		stmt.NameIsString = true
		if opts, hasOpts := alt.child(1).opt(); hasOpts {
			stmt.Options = tc.transformGenericOptionList(opts)
			checkSingleValuedOptions(stmt.Options)
		}
	case "CatalogSessionTarget":
		stmt.Name = identifierText(alt.sole())
	default:
		shapeError(alt, "unknown session target")
	}
	return stmt
}

// ---- COMMENT ON --------------------------------------------------------

// commentOnTypes maps the CommentOnType alternative rules.
var commentOnTypes = map[string]ast.CommentOnType{
	"CommentTable":      ast.CommentOnTable,
	"CommentSequence":   ast.CommentOnSequence,
	"CommentFunction":   ast.CommentOnMacro, // FUNCTION and MACRO share the macro entry
	"CommentMacroTable": ast.CommentOnMacroTable,
	"CommentMacro":      ast.CommentOnMacro,
	"CommentView":       ast.CommentOnView,
	"CommentDatabase":   ast.CommentOnDatabase,
	"CommentIndex":      ast.CommentOnIndex,
	"CommentSchema":     ast.CommentOnSchema,
	"CommentType":       ast.CommentOnTypeEntry,
	"CommentColumn":     ast.CommentOnColumn,
}

// CommentStatement <- 'COMMENT' 'ON' CommentOnType DottedIdentifier 'IS' CommentValue
// Upstream desugars into ALTER with a comment info; COMMENT ON
// DATABASE/SCHEMA raise NotImplemented upstream (post-parse).
func (tc *transformContext) transformComment(n tnode) ast.Stmt {
	stmt := &ast.CommentOnStatement{}
	stmt.SetSpan(n.span())
	_, kind := n.child(2).sole().choice()
	onType, known := commentOnTypes[kind.name()]
	if !known {
		shapeError(kind, "unknown comment target")
	}
	stmt.OnType = onType
	parts := dottedIdentifier(n.child(3))
	if onType == ast.CommentOnColumn {
		stmt.Column = parts[len(parts)-1]
		parts = parts[:len(parts)-1]
		if len(parts) == 0 {
			raise("Invalid column reference: '%s'", stmt.Column)
		}
	}
	stmt.Catalog, stmt.Schema, stmt.Name = qualifiedNameFromParts(parts)
	// CommentValue <- NullLiteral / StringLiteralValue
	_, value := n.child(5).sole().choice()
	if value.name() == "NullLiteral" {
		stmt.Value = constExpr(invalidSpan, ast.Value{Type: ast.LogicalType{ID: "NULL"}, IsNull: true, Kind: ast.ValueNull})
	} else {
		stmt.Value = constExpr(invalidSpan, varcharValue(identifierText(value)))
	}
	return stmt
}

// qualifiedNameFromParts splits a dotted path into catalog/schema/name,
// mirroring upstream's QualifiedName::StringToQualifiedName.
func qualifiedNameFromParts(parts []string) (catalog, schema, name string) {
	switch len(parts) {
	case 1:
		return "", "", parts[0]
	case 2:
		return "", parts[0], parts[1]
	case 3:
		return parts[0], parts[1], parts[2]
	}
	raise("Too many qualifications for name \"%s\"", strings.Join(parts, "."))
	return "", "", ""
}

// ---- extensions --------------------------------------------------------

// LoadStatement <- 'LOAD' ColIdOrString ExtensionAlias?
func (tc *transformContext) transformLoad(n tnode) ast.Stmt {
	stmt := &ast.LoadStatement{Kind: ast.LoadTypeLoad, Name: identifierText(n.child(1))}
	stmt.SetSpan(n.span())
	if alias, ok := n.child(2).opt(); ok {
		// ExtensionAlias <- 'AS' Identifier
		stmt.Kind = ast.LoadTypeLoadAs
		stmt.Alias = identifierText(alias.child(1))
	}
	return stmt
}

// InstallStatement <- 'FORCE'? 'INSTALL' IdentifierOrStringLiteral FromSource? VersionNumber?
func (tc *transformContext) transformInstall(n tnode) ast.Stmt {
	stmt := &ast.LoadStatement{Kind: ast.LoadTypeInstall}
	stmt.SetSpan(n.span())
	if _, force := n.child(0).opt(); force {
		stmt.Kind = ast.LoadTypeForceInstall
	}
	stmt.Name = identifierText(n.child(2))
	if src, ok := n.child(3).opt(); ok {
		// FromSource <- FromSourceIdentifier / FromSourceString
		_, alt := src.sole().choice()
		stmt.Repository = identifierText(alt.child(1))
		stmt.RepoIsAlias = alt.name() == "FromSourceIdentifier"
	}
	if ver, ok := n.child(4).opt(); ok {
		// VersionNumber <- 'VERSION' IdentifierOrStringLiteral
		stmt.Version = identifierText(ver.child(1))
	}
	return stmt
}

// ExternalResourceStatement <- CreateExternalResourceStmt /
// RegisterExternalResourceStmt / DestroyExternalResourceStmt / ShowExternalResourcesStmt
func (tc *transformContext) transformExternalResource(n tnode) ast.Stmt {
	stmt := &ast.ExternalResourceStatement{}
	stmt.SetSpan(n.span())
	_, alt := n.sole().choice()
	switch alt.name() {
	case "CreateExternalResourceStmt":
		// 'CREATE' 'EXTERNAL' 'RESOURCE' StringLiteral AttachAlias? ExternalResourceCreationOptions?
		stmt.Operation = ast.ExternalResourceCreate
		stmt.Type = identifierText(alt.child(3))
		if alias, ok := alt.child(4).opt(); ok {
			stmt.Name = identifierText(alias.child(1))
		}
		if opts, ok := alt.child(5).opt(); ok {
			// a bare flag binds to boolean true, mirroring upstream
			for _, o := range tc.transformGenericOptionList(opts.sole()) {
				if o.Expr == nil && len(o.Values) == 0 {
					o.Values = []ast.Value{{Type: ast.LogicalType{ID: "BOOLEAN"}, Kind: ast.ValueBool, Bool: true}}
				}
				stmt.Options = append(stmt.Options, o)
			}
		}
	case "RegisterExternalResourceStmt":
		// 'REGISTER' 'EXTERNAL' 'RESOURCE' StringLiteral AttachAlias? 'FROM' Expression
		stmt.Operation = ast.ExternalResourceRegister
		stmt.Type = identifierText(alt.child(3))
		if alias, ok := alt.child(4).opt(); ok {
			stmt.Name = identifierText(alias.child(1))
		}
		stmt.Handle = tc.transformExpression(alt.child(6))
	case "DestroyExternalResourceStmt":
		// 'DESTROY' 'EXTERNAL' 'RESOURCE' ColId
		stmt.Operation = ast.ExternalResourceDestroy
		stmt.Name = identifierText(alt.child(3))
	case "ShowExternalResourcesStmt":
		// 'SHOW' ShowAllModifier? 'EXTERNAL' 'RESOURCES'
		stmt.Operation = ast.ExternalResourceShow
		_, stmt.All = alt.child(1).opt()
	default:
		shapeError(alt, "unknown external resource statement")
	}
	return stmt
}
