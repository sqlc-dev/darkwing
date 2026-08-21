// transform_copy.go: COPY ... TO/FROM (with the generic and PostgreSQL-
// style specialized option lists) and COPY FROM DATABASE. The generic
// option machinery here is shared by ATTACH, CONNECT, EXPORT, EXPLAIN,
// CREATE SECRET and the external resource statements. Port of upstream's
// transformer/transform_copy.cpp and transform_generic_copy_option.cpp.
package parser

import (
	"strings"

	"github.com/sqlc-dev/darkwing/ast"
)

func init() {
	register("CopyStatement", func(tc *transformContext, n tnode) ast.Stmt {
		return tc.transformCopy(n)
	})
}

// ---- generic options ---------------------------------------------------

// setGenericOptionExpr folds one option argument, mirroring upstream's
// SetGenericCopyOptionExpression: constants and bare identifiers become
// values, `*` becomes the value "*", boolean-ish casts of 't'/'f' fold to
// booleans, anything else is kept as an expression (upstream raises
// NotImplemented post-parse for genuinely unknown shapes).
func setGenericOptionExpr(opt *ast.GenericOption, e ast.Expr) {
	switch v := e.(type) {
	case *ast.ConstantExpression:
		opt.Values = append(opt.Values, v.Value)
	case *ast.ColumnRefExpression:
		opt.Values = append(opt.Values, varcharValue(v.ColumnNames[len(v.ColumnNames)-1]))
	case *ast.StarExpression:
		opt.Values = append(opt.Values, varcharValue("*"))
	case *ast.CastExpression:
		if c, isConst := v.Child.(*ast.ConstantExpression); isConst && c.Value.Kind == ast.ValueString {
			switch c.Value.Str {
			case "t":
				opt.Values = append(opt.Values, boolValue(true))
				return
			case "f":
				opt.Values = append(opt.Values, boolValue(false))
				return
			}
		}
		opt.Expr = e
	default:
		opt.Expr = e
	}
}

func boolValue(b bool) ast.Value {
	return ast.Value{Type: ast.LogicalType{ID: "BOOLEAN"}, Kind: ast.ValueBool, Bool: b}
}

// applyGenericOptionValue maps a GenericCopyOptionValue onto the option.
// GenericCopyOptionValue <- GenericCopyOptionOrderList / GenericCopyOptionExpression
func (tc *transformContext) applyGenericOptionValue(opt *ast.GenericOption, n tnode) {
	_, alt := n.sole().choice()
	switch alt.name() {
	case "GenericCopyOptionOrderList":
		// Parens(OrderByExpressionList): a parenthesized argument list,
		// parsed as order-by entries so ORDER_BY can carry modifiers.
		var orders []ast.OrderByNode
		for _, e := range alt.sole().parens().listElems() {
			orders = append(orders, ast.OrderByNode{
				Type:       orderTypeOf(e.child(1)),
				NullOrder:  nullOrderOf(e.child(2)),
				Expression: tc.transformExpression(e.child(0)),
			})
		}
		hasModifier := false
		for _, o := range orders {
			if o.Type != ast.OrderDefault || o.NullOrder != ast.NullOrderDefault {
				hasModifier = true
				break
			}
		}
		if strings.EqualFold(opt.Name, "order_by") {
			// upstream renders each entry to text; darkwing keeps the
			// expressions (modifiers recorded on the row arguments only)
			opt.Expr = rowFunction(orderExprs(orders))
			return
		}
		if hasModifier {
			raise("ORDER BY modifiers are only supported in the ORDER BY option")
		}
		if len(orders) == 1 {
			setGenericOptionExpr(opt, orders[0].Expression)
			return
		}
		opt.Expr = rowFunction(orderExprs(orders))
	case "GenericCopyOptionExpression":
		setGenericOptionExpr(opt, tc.transformExpression(alt.sole()))
	default:
		shapeError(alt, "unknown option value")
	}
}

func orderExprs(orders []ast.OrderByNode) []ast.Expr {
	out := make([]ast.Expr, len(orders))
	for i := range orders {
		out[i] = orders[i].Expression
	}
	return out
}

func rowFunction(args []ast.Expr) *ast.FunctionExpression {
	return fnCall(invalidSpan, "row", args...)
}

// transformGenericOption maps GenericCopyOption <- CopyOptionName GenericCopyOptionValue?
func (tc *transformContext) transformGenericOption(n tnode) ast.GenericOption {
	opt := ast.GenericOption{Name: strings.ToLower(identifierText(n.child(0)))}
	if value, ok := n.child(1).opt(); ok {
		tc.applyGenericOptionValue(&opt, value)
	}
	return opt
}

// transformGenericOptionList unwraps GenericCopyOptionList
// (Parens(List(GenericCopyOption))).
func (tc *transformContext) transformGenericOptionList(n tnode) []ast.GenericOption {
	var out []ast.GenericOption
	for _, o := range n.parens().listElems() {
		out = append(out, tc.transformGenericOption(o))
	}
	return out
}

// ---- COPY option lists -------------------------------------------------

// transformCopyOptions maps CopyOptions ('WITH'? CopyOptionList).
func (tc *transformContext) transformCopyOptions(n tnode) []ast.GenericOption {
	_, alt := n.child(1).sole().choice()
	switch alt.name() {
	case "CopyGenericOptionList":
		// Parens(List(CopyGenericOption))
		var out []ast.GenericOption
		for _, o := range alt.sole().parens().listElems() {
			out = append(out, tc.transformCopyGenericOption(o))
		}
		return out
	case "SpecializedOptionList":
		// SpecializedOption SpecializedOptionTail*
		out := []ast.GenericOption{tc.transformSpecializedOption(alt.child(0))}
		for _, tail := range alt.child(1).repeat() {
			// SpecializedOptionTail <- ','? SpecializedOption
			out = append(out, tc.transformSpecializedOption(tail.child(1)))
		}
		return out
	}
	shapeError(alt, "unknown copy option list")
	return nil
}

// CopyGenericOption <- OrderByCopyOption / PartitionedByCopyOption / GenericCopyOption
func (tc *transformContext) transformCopyGenericOption(n tnode) ast.GenericOption {
	_, alt := n.sole().choice()
	switch alt.name() {
	case "OrderByCopyOption":
		// 'ORDER' 'BY' GenericCopyOptionValue?
		opt := ast.GenericOption{Name: "order_by"}
		if value, ok := alt.child(2).opt(); ok {
			tc.applyGenericOptionValue(&opt, value)
		}
		return opt
	case "PartitionedByCopyOption":
		// ('PARTITION' / 'PARTITIONED') 'BY' GenericCopyOptionValue?
		opt := ast.GenericOption{Name: "partition_by"}
		if value, ok := alt.child(2).opt(); ok {
			tc.applyGenericOptionValue(&opt, value)
		}
		return opt
	case "GenericCopyOption":
		return tc.transformGenericOption(alt)
	}
	shapeError(alt, "unknown copy generic option")
	return ast.GenericOption{}
}

// transformSpecializedOption maps the PostgreSQL-style COPY option
// spellings onto generic options, mirroring the Transform*Option family.
func (tc *transformContext) transformSpecializedOption(n tnode) ast.GenericOption {
	_, alt := n.sole().choice()
	switch alt.name() {
	case "SingleOption":
		_, single := alt.sole().choice()
		switch single.name() {
		case "BinaryOption":
			return ast.GenericOption{Name: "format", Values: []ast.Value{varcharValue("binary")}}
		case "FreezeOption":
			return ast.GenericOption{Name: "freeze", Values: []ast.Value{nullValue()}}
		case "OidsOption":
			return ast.GenericOption{Name: "oids", Values: []ast.Value{nullValue()}}
		case "CsvOption":
			return ast.GenericOption{Name: "format", Values: []ast.Value{varcharValue("csv")}}
		case "HeaderOption":
			return ast.GenericOption{Name: "header", Values: []ast.Value{boolValue(true)}}
		}
		shapeError(single, "unknown single copy option")
	case "NullAsOption":
		// 'NULL' 'AS'? StringLiteral
		return ast.GenericOption{Name: "null", Values: []ast.Value{varcharValue(identifierText(alt.child(2)))}}
	case "DelimiterAsOption":
		return ast.GenericOption{Name: "delimiter", Values: []ast.Value{varcharValue(identifierText(alt.child(2)))}}
	case "QuoteAsOption":
		return ast.GenericOption{Name: "quote", Values: []ast.Value{varcharValue(identifierText(alt.child(2)))}}
	case "EscapeAsOption":
		return ast.GenericOption{Name: "escape", Values: []ast.Value{varcharValue(identifierText(alt.child(2)))}}
	case "EncodingOption":
		// 'ENCODING' StringLiteral
		return ast.GenericOption{Name: "encoding", Values: []ast.Value{varcharValue(identifierText(alt.child(1)))}}
	case "ForceQuoteOption":
		// ForceQuote? 'QUOTE' StarSymbolColumnList
		opt := ast.GenericOption{Name: "quote"}
		if _, force := alt.child(0).opt(); force {
			opt.Name = "force_quote"
		}
		applyStarOrColumns(&opt, alt.child(2))
		return opt
	case "PartitionByOption":
		// ('PARTITION' / 'PARTITIONED') 'BY' PartitionByColumnList
		opt := ast.GenericOption{Name: "partition_by"}
		_, list := alt.child(2).sole().choice()
		switch list.name() {
		case "StarPartitionByColumnList":
			star := &ast.StarExpression{}
			star.SetSpan(invalidSpan)
			opt.Expr = star
		case "ParenthesizedPartitionByColumnList":
			for _, col := range identifierList(list.sole().parens()) {
				opt.Values = append(opt.Values, varcharValue(col))
			}
		case "SinglePartitionByColumnList":
			opt.Values = []ast.Value{varcharValue(identifierText(list.sole()))}
		default:
			shapeError(list, "unknown partition-by column list")
		}
		return opt
	case "ForceNullOption":
		// 'FORCE' ForceNotNull? 'NULL' ColumnList
		opt := ast.GenericOption{Name: "force_null"}
		if _, notNull := alt.child(1).opt(); notNull {
			opt.Name = "force_not_null"
		}
		for _, col := range identifierList(alt.child(3)) {
			opt.Values = append(opt.Values, varcharValue(col))
		}
		return opt
	}
	shapeError(alt, "unknown specialized copy option")
	return ast.GenericOption{}
}

// applyStarOrColumns maps StarSymbolColumnList (StarSymbol / ColumnList):
// `*` becomes a star expression, a column list becomes values.
func applyStarOrColumns(opt *ast.GenericOption, n tnode) {
	_, alt := n.sole().choice()
	if alt.name() == "StarSymbol" {
		star := &ast.StarExpression{}
		star.SetSpan(invalidSpan)
		opt.Expr = star
		return
	}
	for _, col := range identifierList(alt) {
		opt.Values = append(opt.Values, varcharValue(col))
	}
}

func nullValue() ast.Value {
	return ast.Value{Type: ast.LogicalType{ID: "NULL"}, IsNull: true, Kind: ast.ValueNull}
}

// setCopyOptions folds an option list onto the CopyInfo, mirroring
// upstream's SetCopyOptions: PARTITIONED_BY is normalized, duplicates are
// a parser error, and a constant-valued FORMAT option is extracted.
func setCopyOptions(info *ast.CopyInfo, opts []ast.GenericOption) {
	seen := map[string]bool{}
	for i := range opts {
		if strings.EqualFold(opts[i].Name, "partitioned_by") {
			opts[i].Name = "partition_by"
		}
		key := strings.ToLower(opts[i].Name)
		if seen[key] {
			raise("Unexpected duplicate option %s", opts[i].Name)
		}
		seen[key] = true
	}
	kept := opts[:0]
	for _, o := range opts {
		if strings.EqualFold(o.Name, "format") && o.Expr == nil {
			if len(o.Values) == 0 {
				raise("Unsupported parameter type for FORMAT: expected e.g. FORMAT 'csv', 'parquet'")
			}
			info.Format = o.Values[0].Str
			info.FormatExplicit = true
			continue
		}
		kept = append(kept, o)
	}
	info.Options = kept
}

// extractFormat derives the format from a file extension, mirroring
// upstream's ExtractFormat (compression suffixes stripped first).
func extractFormat(path string) string {
	f := strings.ToLower(path)
	if strings.HasSuffix(f, ".gz") {
		f = f[:len(f)-3]
	} else if strings.HasSuffix(f, ".zst") {
		f = f[:len(f)-4]
	}
	dot := strings.LastIndexByte(f, '.')
	if dot < 0 || dot == len(f)-1 {
		return ""
	}
	return f[dot+1:]
}

// ---- the COPY statement ------------------------------------------------

// transformCopyFileName maps CopyFileName into a literal path or a path
// expression.
func (tc *transformContext) transformCopyFileName(n tnode) (string, ast.Expr) {
	_, alt := n.sole().choice()
	switch alt.name() {
	case "CopyFileNameExpression":
		// Parameter / ParensExpression
		_, inner := alt.sole().choice()
		var e ast.Expr
		if inner.name() == "Parameter" {
			e = tc.transformParameter(inner)
		} else {
			e = tc.transformExpression(inner.parens())
		}
		if c, isConst := e.(*ast.ConstantExpression); isConst && c.Value.Kind == ast.ValueString {
			return c.Value.Str, nil
		}
		return "", e
	case "CopyFileNameStringLiteral":
		return identifierText(alt.sole()), nil
	case "CopyFileNameIdentifier":
		id := identifierText(alt.sole())
		if strings.EqualFold(id, "stdout") {
			return "/dev/stdout", nil
		}
		return id, nil
	case "CopyFileNameIdentifierColId":
		// IdentifierColId <- Identifier '.' ColId
		ic := alt.sole()
		return identifierText(ic.child(0)) + "." + identifierText(ic.child(2)), nil
	}
	shapeError(alt, "unknown copy file name")
	return "", nil
}

// CopyStatement <- 'COPY' CopyVariations
func (tc *transformContext) transformCopy(n tnode) ast.Stmt {
	sp := n.span()
	_, alt := n.child(1).sole().choice()
	switch alt.name() {
	case "CopyTable":
		return tc.transformCopyTable(alt, sp)
	case "CopySelect":
		return tc.transformCopySelect(alt, sp)
	case "CopyFromDatabase":
		return tc.transformCopyFromDatabase(alt, sp)
	}
	shapeError(alt, "unknown copy variation")
	return nil
}

// CopyTable <- BaseTableName InsertColumnList? FromOrTo CopyFileName CopyOptions?
func (tc *transformContext) transformCopyTable(n tnode, sp ast.Span) ast.Stmt {
	info := &ast.CopyInfo{}
	info.SetSpan(n.span())
	ref := &ast.BaseTableRef{}
	tc.fillBaseTableName(ref, n.child(0))
	info.Catalog, info.Schema, info.Table = ref.CatalogName, ref.SchemaName, ref.TableName
	if cols, ok := n.child(1).opt(); ok {
		info.Columns = identifierList(cols.parens())
	}
	// FromOrTo <- CopyFrom / CopyTo
	_, fromTo := n.child(2).sole().choice()
	info.IsFrom = fromTo.name() == "CopyFrom"
	info.FilePath, info.FilePathExpr = tc.transformCopyFileName(n.child(3))
	info.Format = extractFormat(info.FilePath)
	if opts, ok := n.child(4).opt(); ok {
		setCopyOptions(info, tc.transformCopyOptions(opts))
	}
	stmt := &ast.CopyStatement{Info: info}
	stmt.SetSpan(sp)
	return stmt
}

// CopySelect <- Parens(SelectStatementInternal) 'TO' CopyFileName CopyOptions?
func (tc *transformContext) transformCopySelect(n tnode, sp ast.Span) ast.Stmt {
	info := &ast.CopyInfo{}
	info.SetSpan(n.span())
	info.Query = tc.transformSelectInternalStatement(n.child(0).parens()).Node
	info.FilePath, info.FilePathExpr = tc.transformCopyFileName(n.child(2))
	if opts, ok := n.child(3).opt(); ok {
		setCopyOptions(info, tc.transformCopyOptions(opts))
	}
	stmt := &ast.CopyStatement{Info: info}
	stmt.SetSpan(sp)
	return stmt
}

// CopyFromDatabase <- CopyFromDatabaseWithFlag / CopyFromDatabaseWithoutFlag
func (tc *transformContext) transformCopyFromDatabase(n tnode, sp ast.Span) ast.Stmt {
	_, alt := n.sole().choice()
	switch alt.name() {
	case "CopyFromDatabaseWithFlag":
		// 'FROM' 'DATABASE' ColId 'TO' ColId CopyDatabaseFlag
		stmt := &ast.CopyDatabaseStatement{
			FromDatabase: identifierText(alt.child(2)),
			ToDatabase:   identifierText(alt.child(4)),
		}
		// CopyDatabaseFlag <- Parens(SchemaOrData)
		_, kind := alt.child(5).parens().sole().choice()
		if kind.name() == "CopySchema" {
			stmt.CopyType = ast.CopyDatabaseSchema
		} else {
			stmt.CopyType = ast.CopyDatabaseData
		}
		stmt.SetSpan(sp)
		return stmt
	case "CopyFromDatabaseWithoutFlag":
		// upstream desugars into PRAGMA copy_database(from, to)
		stmt := &ast.PragmaStatement{
			Name: "copy_database",
			Parameters: []ast.Expr{
				constExpr(invalidSpan, varcharValue(identifierText(alt.child(2)))),
				constExpr(invalidSpan, varcharValue(identifierText(alt.child(4)))),
			},
		}
		stmt.SetSpan(sp)
		return stmt
	}
	shapeError(alt, "unknown copy-from-database form")
	return nil
}
