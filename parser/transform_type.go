// transform_type.go: the Type rule of common.gram — producing unbound
// TypeExpressions (DuckDB v2 resolves type names at bind time, so the
// parser records the spelled name plus modifier/child expressions). Port
// of upstream's transform_type.cpp.
package parser

import (
	"github.com/sqlc-dev/darkwing/ast"
)

func typeExpr(sp ast.Span, name string, args ...ast.Expr) *ast.TypeExpression {
	t := &ast.TypeExpression{TypeName: name, Args: args}
	t.SetSpan(sp)
	return t
}

// Type <- TypeVariations ArrayBounds*
func (tc *transformContext) transformType(n tnode) *ast.TypeExpression {
	base := tc.transformTypeVariation(n.child(0))
	bounds := n.child(1).repeat()
	for _, bound := range bounds {
		base = tc.applyArrayBound(base, bound)
	}
	if len(bounds) == 0 {
		base.SetSpan(n.span())
	}
	return base
}

// applyArrayBound wraps a type into list/array per one ArrayBounds entry.
// applyArrayBound wraps a type into list/array; the synthesized wrapper
// carries no location (only named leaf types do).
func (tc *transformContext) applyArrayBound(base *ast.TypeExpression, n tnode) *ast.TypeExpression {
	_, alt := n.sole().choice()
	sp := invalidSpan
	switch alt.name() {
	case "ArrayKeyword":
		return typeExpr(sp, "list", base)
	case "SquareBracketsArray":
		return tc.squareBracketArray(base, alt, sp)
	case "ArrayKeywordWithBounds":
		return tc.squareBracketArray(base, alt.child(1), sp)
	}
	shapeError(alt, "unknown array bound")
	return nil
}

// squareBracketArray handles `[]` (list) and `[n]` (fixed-size array).
func (tc *transformContext) squareBracketArray(base *ast.TypeExpression, n tnode, sp ast.Span) *ast.TypeExpression {
	expr, ok := n.child(1).opt()
	if !ok {
		return typeExpr(sp, "list", base)
	}
	bound := tc.transformExpression(expr)
	// the bound must be a constant; a constant integer is re-synthesized
	// as a location-free BIGINT value, and negative sizes are a parser
	// error upstream (non-integer constants fail at conversion, post-parse)
	c, isConst := bound.(*ast.ConstantExpression)
	if !isConst {
		raise("Expected a constant number as array size")
	}
	if c.Value.Kind == ast.ValueInt64 {
		if c.Value.Int64 < 0 {
			raise("Array size must be greater than 0")
		}
		c.Value.Type = ast.LogicalType{ID: "BIGINT"}
		c.SetSpan(invalidSpan)
	}
	return typeExpr(sp, "array", base, bound)
}

func (tc *transformContext) transformTypeVariation(n tnode) *ast.TypeExpression {
	_, alt := n.sole().choice()
	sp := alt.span()
	switch alt.name() {
	case "TimeType":
		return tc.transformTimeType(alt)
	case "IntervalType":
		return tc.transformIntervalType(alt)
	case "BitType":
		// 'BIT' 'VARYING'? Parens(List(Expression))? — the modifiers are
		// discarded by upstream
		return typeExpr(sp, "BIT")
	case "RowType":
		// RowOrStruct ColIdTypeList?
		var args []ast.Expr
		if fields, ok := alt.child(1).opt(); ok {
			args = tc.transformColIdTypeList(fields)
		}
		return typeExpr(sp, "STRUCT", args...)
	case "VariantType":
		return typeExpr(sp, "VARIANT")
	case "MapType":
		// 'MAP' Parens(List(Type))?
		var args []ast.Expr
		if types, ok := alt.child(1).opt(); ok {
			for _, t := range types.parens().listElems() {
				args = append(args, tc.transformType(t))
			}
		}
		return typeExpr(sp, "MAP", args...)
	case "TupleType":
		var args []ast.Expr
		for _, t := range alt.child(1).parens().listElems() {
			args = append(args, tc.transformType(t))
		}
		return typeExpr(sp, "TUPLE", args...)
	case "GeometryType":
		var args []ast.Expr
		if mod, ok := alt.child(1).opt(); ok {
			e := tc.transformExpression(mod.parens())
			if _, isConst := e.(*ast.ConstantExpression); !isConst {
				raise("Expected a constant as type modifier")
			}
			args = append(args, e)
		}
		return typeExpr(sp, "GEOMETRY", args...)
	case "UnionType":
		return typeExpr(sp, "UNION", tc.transformColIdTypeList(alt.child(1))...)
	case "NumericType":
		return tc.transformNumericType(alt)
	case "SetofType":
		// SETOF is dropped at parse time; the inner type stands alone
		return tc.transformType(alt.child(1))
	case "SimpleType":
		return tc.transformSimpleType(alt)
	}
	shapeError(alt, "unknown type variation")
	return nil
}

// transformColIdTypeList maps Parens(List(ColIdType)) to aliased child
// types.
func (tc *transformContext) transformColIdTypeList(n tnode) []ast.Expr {
	var args []ast.Expr
	for _, field := range n.parens().listElems() {
		// ColIdType <- ColId Type
		t := tc.transformType(field.child(1))
		t.Alias = identifierText(field.child(0))
		args = append(args, t)
	}
	return args
}

// TimeType <- TimeOrTimestamp TypeModifiers? TimeZone?
func (tc *transformContext) transformTimeType(n tnode) *ast.TypeExpression {
	_, kind := n.child(0).sole().choice()
	name := "TIME"
	if kind.name() != "TimestampTypeId" {
		if _, hasMods := n.child(1).opt(); hasMods {
			raise("Type TIME does not allow any modifiers")
		}
	} else {
		name = "TIMESTAMP"
		// a precision modifier picks the timestamp variant
		if mods, ok := n.child(1).opt(); ok {
			args := tc.transformTypeModifiers(mods)
			if len(args) > 1 {
				raise("TIMESTAMP only supports a single modifier")
			}
			if len(args) == 1 {
				if c, isConst := args[0].(*ast.ConstantExpression); isConst && c.Value.Kind == ast.ValueInt64 {
					p := c.Value.Int64
					switch {
					case p > 10:
						raise("TIMESTAMP only supports until nano-second precision (9)")
					case p < 0:
						raise("TIMESTAMP precision should be between 0 and 10 (inclusive)")
					case p == 0:
						name = "TIMESTAMP_S"
					case p <= 3:
						name = "TIMESTAMP_MS"
					case p <= 6:
						name = "TIMESTAMP"
					default:
						name = "TIMESTAMP_NS"
					}
				}
			}
		}
	}
	// other precision modifiers are discarded by upstream
	if tz, ok := n.child(2).opt(); ok {
		// TimeZone <- WithOrWithout 'TIME' 'ZONE'
		_, w := tz.child(0).sole().choice()
		if w.name() == "WithRule" {
			name += " WITH TIME ZONE"
		}
	}
	return typeExpr(n.span(), name)
}

// IntervalType <- IntervalInterval / IntervalNumber
func (tc *transformContext) transformIntervalType(n tnode) *ast.TypeExpression {
	_, alt := n.sole().choice()
	sp := n.span()
	switch alt.name() {
	case "IntervalNumber":
		// 'INTERVAL' Parens(NumberLiteral): the precision is discarded by
		// upstream
		return typeExpr(sp, "INTERVAL")
	case "IntervalInterval":
		// range/simple specifiers are likewise discarded at the type level
		return typeExpr(sp, "INTERVAL")
	}
	shapeError(alt, "unknown interval type")
	return nil
}

// transformNumericType maps the named numeric type rules.
func (tc *transformContext) transformNumericType(n tnode) *ast.TypeExpression {
	_, alt := n.sole().choice()
	sp := n.span()
	switch alt.name() {
	case "SimpleNumericType":
		_, t := alt.sole().choice()
		switch t.name() {
		case "IntType", "IntegerType":
			return typeExpr(sp, "INTEGER")
		case "SmallintType":
			return typeExpr(sp, "SMALLINT")
		case "BigintType":
			return typeExpr(sp, "BIGINT")
		case "RealType":
			// REAL is spelled FLOAT in the unbound type
			return typeExpr(sp, "FLOAT")
		case "BooleanType":
			return typeExpr(sp, "BOOLEAN")
		case "DoubleType":
			return typeExpr(sp, "DOUBLE")
		}
		shapeError(t, "unknown simple numeric type")
	case "DecimalNumericType":
		_, t := alt.sole().choice()
		switch t.name() {
		case "FloatType":
			// 'FLOAT' Parens(NumberLiteral)?
			var args []ast.Expr
			if mod, ok := t.child(1).opt(); ok {
				args = append(args, numberConstant(mod.parens().span(), mod.parens().number()))
			}
			return typeExpr(sp, "FLOAT", args...)
		case "DecimalType", "DecType", "NumericModType":
			var args []ast.Expr
			if mods, ok := t.child(1).opt(); ok {
				args = tc.transformTypeModifiers(mods)
			}
			return typeExpr(sp, "DECIMAL", args...)
		}
		shapeError(t, "unknown decimal numeric type")
	}
	shapeError(alt, "unknown numeric type")
	return nil
}

// TypeModifiers <- Parens(List(Expression)?)
func (tc *transformContext) transformTypeModifiers(n tnode) []ast.Expr {
	list, ok := n.parens().opt()
	if !ok {
		return nil
	}
	mods := tc.transformExpressionList(list)
	for _, m := range mods {
		if _, isConst := m.(*ast.ConstantExpression); !isConst {
			raise("Expected a constant as type modifier")
		}
	}
	return mods
}

// SimpleType <- CharacterSimpleType / QualifiedSimpleType
func (tc *transformContext) transformSimpleType(n tnode) *ast.TypeExpression {
	_, alt := n.sole().choice()
	sp := n.span()
	switch alt.name() {
	case "CharacterSimpleType":
		var args []ast.Expr
		if mods, ok := alt.child(1).opt(); ok {
			args = tc.transformTypeModifiers(mods)
		}
		return typeExpr(sp, "VARCHAR", args...)
	case "QualifiedSimpleType":
		// QualifiedTypeName TypeModifiers?
		catalog, schema, name := tc.transformQualifiedTypeName(alt.child(0))
		var args []ast.Expr
		if mods, ok := alt.child(1).opt(); ok {
			args = tc.transformTypeModifiers(mods)
		}
		t := typeExpr(sp, name, args...)
		t.Catalog, t.Schema = catalog, schema
		return t
	}
	shapeError(alt, "unknown simple type")
	return nil
}

// QualifiedTypeName <- CatalogReservedSchemaTypeName /
// SchemaReservedTypeName / TypeNameAsQualifiedName
func (tc *transformContext) transformQualifiedTypeName(n tnode) (catalog, schema, name string) {
	_, alt := n.sole().choice()
	switch alt.name() {
	case "CatalogReservedSchemaTypeName":
		catalog = identifierText(alt.child(0).child(0))
		schema = identifierText(alt.child(1).child(0))
		name = identifierText(alt.child(2))
	case "SchemaReservedTypeName":
		schema = identifierText(alt.child(0).child(0))
		name = identifierText(alt.child(1))
	case "TypeNameAsQualifiedName":
		name = identifierText(alt.sole())
	default:
		shapeError(alt, "unknown qualified type name")
	}
	// a catalog with no schema is a schema qualification
	if catalog != "" && schema == "" {
		catalog, schema = "", catalog
	}
	return catalog, schema, name
}
