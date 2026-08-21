// transform_dml.go: INSERT / UPDATE / DELETE / TRUNCATE / MERGE INTO —
// the milestone-4 DML transformers. Port of upstream's
// transformer/statement/transform_insert.cpp, transform_update.cpp,
// transform_delete.cpp and transform_merge_into.cpp.
package parser

import (
	"github.com/sqlc-dev/darkwing/ast"
)

func init() {
	register("InsertStatement", func(tc *transformContext, n tnode) ast.Stmt {
		return tc.transformInsert(n)
	})
	register("UpdateStatement", func(tc *transformContext, n tnode) ast.Stmt {
		return tc.transformUpdate(n)
	})
	register("DeleteStatement", func(tc *transformContext, n tnode) ast.Stmt {
		return tc.transformDelete(n)
	})
	register("TruncateStatement", func(tc *transformContext, n tnode) ast.Stmt {
		return tc.transformTruncate(n)
	})
	register("MergeIntoStatement", func(tc *transformContext, n tnode) ast.Stmt {
		return tc.transformMergeInto(n)
	})
}

// transformReturning maps a ReturningClause ('RETURNING' TargetList).
func (tc *transformContext) transformReturning(n tnode) []ast.Expr {
	var out []ast.Expr
	for _, e := range n.child(1).listElems() {
		out = append(out, tc.transformAliasedExpression(e))
	}
	return out
}

// InsertStatement <- WithClause? 'INSERT' OrAction? 'INTO' InsertTarget
// ByNameOrPosition? InsertColumnList? InsertValues OnConflictClause?
// ReturningClause?
func (tc *transformContext) transformInsert(n tnode) ast.Stmt {
	stmt := &ast.InsertStatement{}
	stmt.SetSpan(n.span())
	if wc, ok := n.child(0).opt(); ok {
		stmt.CTEs = tc.transformWithClause(wc)
	}
	if oa, ok := n.child(2).opt(); ok {
		// OrAction <- InsertOrReplace / InsertOrIgnore
		_, kind := oa.sole().choice()
		info := &ast.OnConflictInfo{Action: ast.OnConflictNothing}
		if kind.name() == "InsertOrReplace" {
			info.Action = ast.OnConflictReplace
		}
		info.SetSpan(oa.span())
		stmt.OnConflict = info
	}
	// InsertTarget <- BaseTableName InsertAlias?
	target := n.child(4)
	ref := &ast.BaseTableRef{}
	tc.fillBaseTableName(ref, target.child(0))
	stmt.Catalog, stmt.Schema, stmt.Table = ref.CatalogName, ref.SchemaName, ref.TableName
	if aliasNode, ok := target.child(1).opt(); ok {
		stmt.TableAlias = identifierText(aliasNode.child(1))
	}
	if bnp, ok := n.child(5).opt(); ok {
		stmt.ColumnOrder = byNameOrPosition(bnp)
	}
	if cols, ok := n.child(6).opt(); ok {
		stmt.Columns = identifierList(cols.sole().parens())
	}
	// InsertValues <- SelectInsertValues / DefaultValues
	_, values := n.child(7).sole().choice()
	switch values.name() {
	case "SelectInsertValues":
		stmt.Query = tc.transformSelectInternalStatement(values.sole())
	case "DefaultValues":
		stmt.DefaultValues = true
	default:
		shapeError(values, "unknown insert values")
	}
	if oc, ok := n.child(8).opt(); ok {
		if stmt.OnConflict != nil {
			raise("OR REPLACE|IGNORE is not compatible with ON CONFLICT")
		}
		stmt.OnConflict = tc.transformOnConflict(oc)
	}
	if ret, ok := n.child(9).opt(); ok {
		stmt.Returning = tc.transformReturning(ret)
	}
	return stmt
}

// byNameOrPosition maps ByNameOrPosition to the column order enum.
func byNameOrPosition(n tnode) ast.InsertColumnOrder {
	_, kind := n.sole().choice()
	if kind.name() == "InsertByNameOrder" {
		return ast.InsertByName
	}
	return ast.InsertByPosition
}

// OnConflictClause <- 'ON' 'CONFLICT' OnConflictTarget? OnConflictAction
func (tc *transformContext) transformOnConflict(n tnode) *ast.OnConflictInfo {
	info := &ast.OnConflictInfo{}
	info.SetSpan(n.span())
	if target, ok := n.child(2).opt(); ok {
		_, alt := target.sole().choice()
		switch alt.name() {
		case "OnConflictExpressionTarget":
			// ColumnIdList WhereClause?
			info.IndexedColumns = identifierList(alt.child(0).sole().parens())
			if wc, ok := alt.child(1).opt(); ok {
				info.TargetWhere = tc.transformExpression(wc.child(1))
			}
		case "OnConflictIndexTarget":
			// 'ON' 'CONSTRAINT' ConstraintName (rejected upstream only
			// post-parse, as a Not implemented Error)
			info.ConstraintName = identifierText(alt.child(2))
		default:
			shapeError(alt, "unknown on-conflict target")
		}
	}
	_, action := n.child(3).sole().choice()
	switch action.name() {
	case "OnConflictUpdate":
		// 'DO' 'UPDATE' 'SET' UpdateSetClause WhereClause?
		info.Action = ast.OnConflictUpdate
		set := tc.transformUpdateSetClause(action.child(3))
		if wc, ok := action.child(4).opt(); ok {
			set.Condition = tc.transformExpression(wc.child(1))
		}
		info.SetInfo = set
	case "OnConflictNothing":
		info.Action = ast.OnConflictNothing
	default:
		shapeError(action, "unknown on-conflict action")
	}
	return info
}

// UpdateSetClause <- UpdateSetElementList / UpdateSetTuple
func (tc *transformContext) transformUpdateSetClause(n tnode) *ast.UpdateSetInfo {
	set := &ast.UpdateSetInfo{}
	set.SetSpan(n.span())
	_, alt := n.sole().choice()
	switch alt.name() {
	case "UpdateSetElementList":
		for _, e := range alt.sole().listElems() {
			// UpdateSetElement <- UpdateSetColumnTarget '=' Expression
			target := updateSetColumnTarget(e.child(0))
			if len(target) > 1 {
				raise("Qualified column names in UPDATE .. SET not supported")
			}
			set.Columns = append(set.Columns, target)
			set.Expressions = append(set.Expressions, tc.transformExpression(e.child(2)))
		}
	case "UpdateSetTuple":
		// Parens(List(ColumnName)) '=' Expression: every column is
		// assigned from the single row-valued expression
		for _, c := range alt.child(0).parens().listElems() {
			set.Columns = append(set.Columns, []string{identifierText(c)})
		}
		set.Expressions = append(set.Expressions, tc.transformExpression(alt.child(2)))
	default:
		shapeError(alt, "unknown update set clause")
	}
	return set
}

// updateSetColumnTarget flattens ColumnName DotIdentifier* into a dotted
// path.
func updateSetColumnTarget(n tnode) []string {
	path := []string{identifierText(n.child(0))}
	for _, dot := range n.child(1).repeat() {
		path = append(path, identifierText(dot.child(1)))
	}
	return path
}

// UpdateStatement <- WithClause? 'UPDATE' UpdateTarget UpdateSetClause
// FromClause? WhereClause? ReturningClause?
func (tc *transformContext) transformUpdate(n tnode) ast.Stmt {
	stmt := &ast.UpdateStatement{}
	stmt.SetSpan(n.span())
	if wc, ok := n.child(0).opt(); ok {
		stmt.CTEs = tc.transformWithClause(wc)
	}
	// UpdateTarget <- BaseTableSet / BaseTableAliasSet (both end in 'SET')
	_, target := n.child(2).sole().choice()
	ref := &ast.BaseTableRef{}
	// upstream's update target location runs through the SET keyword
	// (the grammar node ends at 'SET')
	ref.SetSpan(target.span())
	tc.fillBaseTableName(ref, target.child(0))
	if target.name() == "BaseTableAliasSet" {
		if aliasNode, ok := target.child(1).opt(); ok {
			// UpdateAlias <- 'AS'? ColId
			ref.Alias = identifierText(aliasNode.child(1))
		}
	}
	stmt.Table = ref
	stmt.SetInfo = tc.transformUpdateSetClause(n.child(3))
	if fc, ok := n.child(4).opt(); ok {
		stmt.From = tc.transformFromClause(fc)
	}
	if wc, ok := n.child(5).opt(); ok {
		stmt.Where = tc.transformExpression(wc.child(1))
	}
	if ret, ok := n.child(6).opt(); ok {
		stmt.Returning = tc.transformReturning(ret)
	}
	return stmt
}

// transformTargetOptAlias maps TargetOptAlias (BaseTableName 'AS'? ColId?).
func (tc *transformContext) transformTargetOptAlias(n tnode) *ast.BaseTableRef {
	ref := &ast.BaseTableRef{}
	ref.SetSpan(n.span())
	tc.fillBaseTableName(ref, n.child(0))
	if alias, ok := n.child(2).opt(); ok {
		ref.Alias = identifierText(alias)
	}
	return ref
}

// DeleteStatement <- WithClause? 'DELETE' 'FROM' TargetOptAlias
// DeleteUsingClause? WhereClause? ReturningClause?
func (tc *transformContext) transformDelete(n tnode) ast.Stmt {
	stmt := &ast.DeleteStatement{}
	stmt.SetSpan(n.span())
	if wc, ok := n.child(0).opt(); ok {
		stmt.CTEs = tc.transformWithClause(wc)
	}
	target := tc.transformTargetOptAlias(n.child(3))
	// upstream's delete target carries no location
	target.SetSpan(invalidSpan)
	stmt.Table = target
	if using, ok := n.child(4).opt(); ok {
		// DeleteUsingClause <- 'USING' List(TableRef)
		for _, r := range using.child(1).listElems() {
			stmt.Using = append(stmt.Using, tc.transformTableRef(r))
		}
	}
	if wc, ok := n.child(5).opt(); ok {
		stmt.Where = tc.transformExpression(wc.child(1))
	}
	if ret, ok := n.child(6).opt(); ok {
		stmt.Returning = tc.transformReturning(ret)
	}
	return stmt
}

// TruncateStatement <- 'TRUNCATE' 'TABLE'? BaseTableName
func (tc *transformContext) transformTruncate(n tnode) ast.Stmt {
	stmt := &ast.TruncateStatement{}
	stmt.SetSpan(n.span())
	ref := &ast.BaseTableRef{}
	// like DELETE, the truncate target carries no location
	ref.SetSpan(invalidSpan)
	tc.fillBaseTableName(ref, n.child(2))
	stmt.Table = ref
	return stmt
}

// MergeIntoStatement <- WithClause? 'MERGE' 'INTO' TargetOptAlias
// MergeIntoUsingClause JoinQualifier MergeMatch+ ReturningClause?
func (tc *transformContext) transformMergeInto(n tnode) ast.Stmt {
	stmt := &ast.MergeIntoStatement{}
	stmt.SetSpan(n.span())
	if wc, ok := n.child(0).opt(); ok {
		stmt.CTEs = tc.transformWithClause(wc)
	}
	stmt.Target = tc.transformTargetOptAlias(n.child(3))
	stmt.Source = tc.transformTableRef(n.child(4).child(1))
	// JoinQualifier <- OnClause / UsingClause
	_, qual := n.child(5).sole().choice()
	switch qual.name() {
	case "OnClause":
		stmt.JoinCondition = tc.transformExpression(qual.child(1))
	case "UsingClause":
		stmt.UsingColumns = identifierList(qual.child(1).parens())
	default:
		shapeError(qual, "unknown merge join qualifier")
	}
	for _, match := range n.child(6).repeat() {
		stmt.Actions = append(stmt.Actions, tc.transformMergeMatch(match))
	}
	if ret, ok := n.child(7).opt(); ok {
		stmt.Returning = tc.transformReturning(ret)
	}
	return stmt
}

// MergeMatch <- MatchedClause / NotMatchedClause
func (tc *transformContext) transformMergeMatch(n tnode) ast.MergeIntoAction {
	action := ast.MergeIntoAction{}
	action.SetSpan(n.span())
	_, alt := n.sole().choice()
	var actionNode tnode
	switch alt.name() {
	case "MatchedClause":
		// 'WHEN' 'MATCHED' AndExpression? 'THEN' MatchedClauseAction
		action.Kind = ast.MergeWhenMatched
		if and, ok := alt.child(2).opt(); ok {
			action.Condition = tc.transformExpression(and.child(1))
		}
		actionNode = alt.child(4)
	case "NotMatchedClause":
		// 'WHEN' 'NOT' 'MATCHED' BySourceOrTarget? AndExpression? 'THEN' MatchedClauseAction
		action.Kind = ast.MergeWhenNotMatchedByTarget
		if bst, ok := alt.child(3).opt(); ok {
			_, side := bst.sole().choice()
			if side.name() == "BySource" {
				action.Kind = ast.MergeWhenNotMatchedBySource
			}
		}
		if and, ok := alt.child(4).opt(); ok {
			action.Condition = tc.transformExpression(and.child(1))
		}
		actionNode = alt.child(6)
	default:
		shapeError(alt, "unknown merge match")
	}
	tc.fillMergeAction(&action, actionNode)
	return action
}

// fillMergeAction maps a MatchedClauseAction.
func (tc *transformContext) fillMergeAction(action *ast.MergeIntoAction, n tnode) {
	_, alt := n.sole().choice()
	switch alt.name() {
	case "UpdateMatchClause":
		// 'UPDATE' UpdateMatchInfo?
		action.Action = ast.MergeUpdate
		if info, ok := alt.child(1).opt(); ok {
			_, kind := info.sole().choice()
			switch kind.name() {
			case "UpdateMatchSetAction":
				// UpdateMatchSetClause <- 'SET' UpdateMatchSetInfo
				_, setInfo := kind.sole().child(1).sole().choice()
				if setInfo.name() == "UpdateSetClause" {
					action.SetInfo = tc.transformUpdateSetClause(setInfo)
				}
				// SET * keeps a nil SetInfo (update everything)
			case "UpdateByNameOrPosition":
				action.ColumnOrder = byNameOrPosition(kind.sole())
			}
		}
	case "DeleteMatchClause":
		action.Action = ast.MergeDelete
	case "InsertMatchClause":
		// 'INSERT' InsertMatchInfo?
		action.Action = ast.MergeInsert
		if info, ok := alt.child(1).opt(); ok {
			_, kind := info.sole().choice()
			switch kind.name() {
			case "InsertValuesList":
				// InsertColumnList? 'VALUES' Parens(List(Expression))
				if cols, ok := kind.child(0).opt(); ok {
					action.Columns = identifierList(cols.sole().parens())
				}
				action.Expressions = tc.transformExpressionList(kind.child(2).parens())
			case "InsertDefaultValues":
				action.DefaultValues = true
			case "InsertByNameOrPosition":
				// ByNameOrPosition? '*'?
				if bnp, ok := kind.child(0).opt(); ok {
					action.ColumnOrder = byNameOrPosition(bnp)
				}
			}
		}
	case "DoNothingMatchClause":
		action.Action = ast.MergeDoNothing
	case "ErrorMatchClause":
		action.Action = ast.MergeError
		if e, ok := alt.child(1).opt(); ok {
			action.ErrorExpr = tc.transformExpression(e)
		}
	default:
		shapeError(alt, "unknown merge action")
	}
}
