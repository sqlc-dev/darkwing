// transform_create_misc.go: the milestone-5 CREATE variants — macros,
// secrets and triggers. Port of upstream's
// transformer/transform_create_macro.cpp, transform_create_secret.cpp and
// transform_create_trigger.cpp.
package parser

import (
	"strings"

	"github.com/sqlc-dev/darkwing/ast"
)

// ---- CREATE MACRO ------------------------------------------------------

// CreateMacroStmt <- MacroOrFunction IfNotExists? QualifiedName List(MacroDefinition)
func (tc *transformContext) transformCreateMacro(n tnode) ast.CreateInfo {
	info := &ast.CreateMacroInfo{}
	info.SetSpan(n.span())
	applyIfNotExists(&info.CreateInfoBase, n.child(1))
	tc.fillQualifiedName(&info.CreateInfoBase, n.child(2))
	for _, def := range n.child(3).listElems() {
		info.Macros = append(info.Macros, tc.transformMacroDefinition(def))
	}
	info.IsTable = info.Macros[0].Query != nil
	for _, m := range info.Macros[1:] {
		if (m.Query != nil) != info.IsTable {
			raise("Cannot mix table and scalar macro function definitions")
		}
	}
	tc.pivotEntryCheck("macro")
	return info
}

// MacroDefinition <- Parens(MacroParameters?) 'AS' MacroDefinitionBody
func (tc *transformContext) transformMacroDefinition(n tnode) *ast.MacroFunction {
	m := &ast.MacroFunction{}
	m.SetSpan(n.span())
	if params, ok := n.child(0).parens().opt(); ok {
		seen := map[string]bool{}
		defaultSeen := false
		for _, p := range params.listElems() {
			mp := tc.transformMacroParameter(p)
			key := strings.ToLower(mp.Name)
			if seen[key] {
				raise("Duplicate parameter '%s' in macro definition", mp.Name)
			}
			seen[key] = true
			if mp.Default != nil {
				defaultSeen = true
			} else if defaultSeen {
				raise("Parameter without a default follows parameter with a default")
			}
			m.Parameters = append(m.Parameters, mp)
		}
	}
	// MacroDefinitionBody <- TableMacroDefinition / ScalarMacroDefinition
	_, body := n.child(2).sole().choice()
	if body.name() == "TableMacroDefinition" {
		// 'TABLE' SelectStatementInternal
		m.Query = tc.transformSelectInternalStatement(body.child(1))
	} else {
		m.Expr = tc.transformExpression(body.sole())
	}
	return m
}

// MacroParameter <- NamedParameter / SimpleParameter
func (tc *transformContext) transformMacroParameter(n tnode) ast.MacroParameter {
	_, alt := n.sole().choice()
	var mp ast.MacroParameter
	switch alt.name() {
	case "NamedParameter":
		// TypeFuncName Type? NamedParameterAssignment Expression
		mp.Name = identifierText(alt.child(0))
		if t, ok := alt.child(1).opt(); ok {
			mp.Type = tc.transformType(t)
		}
		mp.Default = tc.transformExpression(alt.child(3))
	case "SimpleParameter":
		// TypeFuncName Type?
		mp.Name = identifierText(alt.child(0))
		if t, ok := alt.child(1).opt(); ok {
			mp.Type = tc.transformType(t)
		}
	default:
		shapeError(alt, "unknown macro parameter")
	}
	return mp
}

// ---- CREATE SECRET -----------------------------------------------------

// optionValueExpr is one option's value as an expression, mirroring
// GenericCopyOption::GetFirstChildOrExpression. Nil for a bare flag.
func optionValueExpr(o ast.GenericOption) ast.Expr {
	if len(o.Values) > 0 {
		return constExpr(invalidSpan, o.Values[0])
	}
	return o.Expr
}

// CreateSecretStmt <- 'SECRET' IfNotExists? SecretName? SecretStorageSpecifier? GenericCopyOptionList
func (tc *transformContext) transformCreateSecret(n tnode) ast.CreateInfo {
	info := &ast.CreateSecretInfo{}
	info.SetSpan(n.span())
	applyIfNotExists(&info.CreateInfoBase, n.child(1))
	if name, ok := n.child(2).opt(); ok {
		info.Name = identifierText(name.sole())
	}
	if storage, ok := n.child(3).opt(); ok {
		// SecretStorageSpecifier <- 'IN' Identifier
		info.Storage = strings.ToLower(identifierText(storage.child(1)))
	}
	for _, o := range tc.transformGenericOptionList(n.child(4)) {
		switch strings.ToLower(o.Name) {
		case "scope":
			info.Scope = optionValueExpr(o)
		case "type":
			info.Type = optionValueExpr(o)
		case "provider":
			info.Provider = optionValueExpr(o)
		default:
			// duplicates raise a binder error upstream (post-parse)
			info.Options = append(info.Options, o)
		}
	}
	if info.Name == "" {
		if info.Type == nil {
			raise("Failed to create secret - secret must have a type defined")
		}
		// a non-constant type with a default name raises InvalidInput
		// upstream (post-parse); darkwing leaves the name empty then
		if c, ok := info.Type.(*ast.ConstantExpression); ok && c.Value.Kind == ast.ValueString {
			info.Name = "__default_" + strings.ToLower(c.Value.Str)
		}
	}
	return info
}

// ---- CREATE TRIGGER ----------------------------------------------------

// CreateTriggerStmt <- 'TRIGGER' IfNotExists? TriggerName TriggerTiming
// TriggerEvent 'ON' BaseTableName ReferencingClause? ForEachClause? TriggerBody
func (tc *transformContext) transformCreateTrigger(n tnode) ast.CreateInfo {
	info := &ast.CreateTriggerInfo{}
	info.SetSpan(n.span())
	applyIfNotExists(&info.CreateInfoBase, n.child(1))
	info.Name = identifierText(n.child(2))
	_, timing := n.child(3).sole().choice()
	switch timing.name() {
	case "TriggerBefore":
		info.Timing = ast.TriggerBefore
	case "TriggerAfter":
		info.Timing = ast.TriggerAfter
	case "TriggerInsteadOf":
		info.Timing = ast.TriggerInsteadOf
	}
	// TriggerEvent <- TriggerEventUpdateOf / TriggerEventInsert /
	// TriggerEventDelete / TriggerEventUpdate
	_, event := n.child(4).sole().choice()
	switch event.name() {
	case "TriggerEventInsert":
		info.Event = ast.TriggerEventInsert
	case "TriggerEventDelete":
		info.Event = ast.TriggerEventDelete
	case "TriggerEventUpdate":
		info.Event = ast.TriggerEventUpdate
	case "TriggerEventUpdateOf":
		// 'UPDATE' 'OF' TriggerColumnList
		info.Event = ast.TriggerEventUpdate
		info.Columns = identifierList(event.child(2))
	}
	table := &ast.BaseTableRef{}
	table.SetSpan(n.child(6).span())
	tc.fillBaseTableName(table, n.child(6))
	info.Table = table
	if ref, ok := n.child(7).opt(); ok {
		tc.transformReferencingClause(info, ref)
	}
	if each, ok := n.child(8).opt(); ok {
		_, kind := each.sole().choice()
		if kind.name() == "ForEachRow" {
			info.ForEach = ast.TriggerForEachRow
		} else {
			info.ForEach = ast.TriggerForEachStatement
		}
	}
	// TriggerBody <- InsertStatement / UpdateStatement / DeleteStatement /
	// MergeIntoStatement — the same shape as a Statement choice
	info.Body = tc.transformStatement(n.child(9))
	return info
}

// ReferencingClause <- 'REFERENCING' ReferencingItem ReferencingItem?
func (tc *transformContext) transformReferencingClause(info *ast.CreateTriggerInfo, n tnode) {
	newTable, oldTable := referencingItem(n.child(1))
	info.ReferencingNew, info.ReferencingOld = newTable, oldTable
	if item, ok := n.child(2).opt(); ok {
		newTable, oldTable = referencingItem(item)
		if newTable != "" {
			if info.ReferencingNew != "" {
				raise("NEW TABLE cannot be specified multiple times in REFERENCING clause")
			}
			info.ReferencingNew = newTable
		}
		if oldTable != "" {
			if info.ReferencingOld != "" {
				raise("OLD TABLE cannot be specified multiple times in REFERENCING clause")
			}
			info.ReferencingOld = oldTable
		}
	}
	if info.ReferencingNew != "" && info.ReferencingOld != "" && info.ReferencingNew == info.ReferencingOld {
		raise("REFERENCING aliases must be distinct")
	}
}

// ReferencingItem <- ReferencingNewTableAs / ReferencingOldTableAs
func referencingItem(n tnode) (newTable, oldTable string) {
	_, alt := n.sole().choice()
	// 'NEW'/'OLD' 'TABLE' 'AS' ColId
	if alt.name() == "ReferencingNewTableAs" {
		return identifierText(alt.child(3)), ""
	}
	return "", identifierText(alt.child(3))
}
