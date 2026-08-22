// transform_single.go: the SingleExpression alternatives — literals,
// parameters, function calls, CASE/CAST/star/subquery and the special
// function forms. Port of upstream's transformer/expression/*.cpp
// (transform_constant, transform_function, transform_case, ...).
package parser

import (
	"errors"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/sqlc-dev/darkwing/ast"
	"github.com/sqlc-dev/darkwing/internal/matcher"
)

// identifierText descends single-child rules and choices to the
// identifier/keyword/string token below and returns its text.
func identifierText(n tnode) string {
	switch r := n.r.(type) {
	case *matcher.IdentifierResult:
		return r.Identifier
	case *matcher.StringResult:
		return r.Value
	case *matcher.KeywordResult:
		return r.Keyword
	case *matcher.ChoiceResult:
		return identifierText(tnode{r.Result})
	case *matcher.ListResult:
		if len(r.Children) == 1 {
			return identifierText(tnode{r.Children[0]})
		}
	case *matcher.OptionalResult:
		if r.Result != nil {
			return identifierText(tnode{r.Result})
		}
	}
	shapeError(n, "cannot extract identifier text (%T)", n.r)
	return ""
}

// identifierList maps List(X) of identifier-ish rules to their texts.
func identifierList(n tnode) []string {
	elems := n.listElems()
	out := make([]string, len(elems))
	for i, e := range elems {
		out[i] = identifierText(e)
	}
	return out
}

func (tc *transformContext) transformSingle(n tnode) ast.Expr {
	_, alt := n.sole().choice()
	switch alt.name() {
	case "ParensExpression":
		return tc.transformExpression(alt.parens())
	case "LiteralExpression":
		return tc.transformLiteral(alt)
	case "Parameter":
		return tc.transformParameter(alt)
	case "SubqueryExpression":
		return tc.transformSubqueryExpr(alt)
	case "SpecialFunctionExpression":
		return tc.transformSpecialFunction(alt)
	case "ParenthesisExpression":
		// row constructor: (a, b, ...); () is row()
		sp := alt.span()
		var args []ast.Expr
		if inner, ok := alt.sole().parens().opt(); ok {
			args = tc.transformExpressionList(inner)
		}
		return fnCall(sp, "row", args...)
	case "IntervalLiteral":
		return tc.transformIntervalLiteral(alt)
	case "TypeLiteral":
		// Type StringLiteral: DATE '2020-01-01' -> CAST('...' AS DATE);
		// the string constant carries no location
		str, _ := alt.child(1).stringLit()
		child := constExpr(invalidSpan, varcharValue(str))
		return tc.castTo(alt.span(), child, alt.child(0))
	case "CaseExpression":
		return tc.transformCase(alt)
	case "StarExpression":
		return tc.transformStar(alt)
	case "CastExpression":
		// CastOrTryCast Parens(CastArguments)
		_, kind := alt.child(0).sole().choice()
		args := alt.child(1).parens()
		child := tc.transformExpression(args.child(0))
		cast := tc.castTo(alt.span(), child, args.child(2))
		cast.TryCast = kind.name() == "TryCastKeyword"
		return cast
	case "GroupingExpression":
		args, ok := alt.child(1).parens().opt()
		var children []ast.Expr
		if ok {
			children = tc.transformExpressionList(args)
		}
		return opExpr(alt.span(), ast.GroupingFunction, children...)
	case "MapExpression":
		return tc.transformMap(alt)
	case "FunctionExpression":
		return tc.transformFunction(alt)
	case "ColumnReference":
		return tc.transformColumnReference(alt)
	case "ListComprehensionExpression":
		return tc.transformListComprehension(alt)
	case "ListExpression":
		return tc.transformList(alt)
	case "StructExpression":
		return tc.transformStruct(alt)
	case "PositionalExpression":
		idx, err := strconv.ParseUint(alt.child(1).number(), 10, 64)
		if err != nil {
			raise("invalid positional reference")
		}
		if idx == 0 {
			raise("Positional reference node needs to be >= 1")
		}
		p := &ast.PositionalReferenceExpression{Index: idx}
		p.SetSpan(alt.span())
		return p
	case "DefaultExpression":
		d := &ast.DefaultExpression{}
		d.SetSpan(alt.span())
		return d
	}
	shapeError(alt, "unknown SingleExpression alternative")
	return nil
}

// castTo builds a CastExpression to the given Type rule node. Explicit
// casts always stay unbound (only the interval-literal desugaring
// pre-resolves types).
func (tc *transformContext) castTo(sp ast.Span, child ast.Expr, typeNode tnode) *ast.CastExpression {
	cast := &ast.CastExpression{Child: child, CastType: tc.transformType(typeNode)}
	cast.SetSpan(sp)
	return cast
}

// ---- literals ----------------------------------------------------------

func (tc *transformContext) transformLiteral(n tnode) ast.Expr {
	_, alt := n.sole().choice()
	switch {
	case alt.name() == "StringLiteral" || alt.isString():
		s, kind := alt.stringLit()
		return tc.stringConstant(alt.span(), s, kind)
	case alt.name() == "NumberLiteral":
		return numberConstant(alt.span(), alt.number())
	case alt.name() == "ConstantLiteral":
		_, lit := alt.sole().choice()
		sp := lit.sole().span()
		switch lit.name() {
		case "NullLiteral":
			return constExpr(sp, ast.Value{Type: ast.LogicalType{ID: "NULL"}, IsNull: true, Kind: ast.ValueNull})
		case "TrueLiteral":
			return constExpr(sp, ast.Value{Type: ast.LogicalType{ID: "BOOLEAN"}, Kind: ast.ValueBool, Bool: true})
		case "FalseLiteral":
			return constExpr(sp, ast.Value{Type: ast.LogicalType{ID: "BOOLEAN"}, Kind: ast.ValueBool, Bool: false})
		}
	}
	shapeError(alt, "unknown literal")
	return nil
}

// stringConstant builds the constant for a string literal, honoring the
// prefix kind.
func (tc *transformContext) stringConstant(sp ast.Span, s string, kind matcher.StringKind) ast.Expr {
	switch kind {
	case matcher.StringHexadecimal:
		// X'FF' -> BLOB \xFF
		blob, ok := hexToBlob(s)
		if !ok {
			raise("invalid hexadecimal string literal")
		}
		return constExpr(sp, ast.Value{Type: ast.LogicalType{ID: "BLOB"}, Kind: ast.ValueString, Str: blob})
	case matcher.StringBit:
		// B'1010' stays a VARCHAR "b1010" at parse time (the binder
		// converts it)
		return constExpr(sp, varcharValue("b"+s))
	case matcher.StringNational:
		// N'abc' -> CAST('abc' AS VARCHAR)
		child := constExpr(invalidSpan, varcharValue(s))
		cast := &ast.CastExpression{Child: child, ResolvedType: "VARCHAR"}
		cast.SetSpan(sp)
		return cast
	case matcher.StringEscape:
		unescaped := unescapeString(s)
		if strings.IndexByte(unescaped, 0) >= 0 {
			raise("Null character not permitted in escape string literal")
		}
		if !utf8.ValidString(unescaped) {
			raise("Invalid UTF-8 in escape string literal at byte offset %d: invalid unicode codepoint",
				invalidUTF8Offset(unescaped))
		}
		return constExpr(sp, varcharValue(unescaped))
	default:
		return constExpr(sp, varcharValue(s))
	}
}

// unescapeString processes E'...' backslash escapes the way upstream
// does: the C escapes, \xHH hex and \ooo octal; any other escaped
// character stands for itself.
func unescapeString(s string) string {
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' || i+1 >= len(s) {
			sb.WriteByte(c)
			continue
		}
		i++
		switch e := s[i]; e {
		case 'b':
			sb.WriteByte('\b')
		case 'f':
			sb.WriteByte('\f')
		case 'n':
			sb.WriteByte('\n')
		case 'r':
			sb.WriteByte('\r')
		case 't':
			sb.WriteByte('\t')
		case 'x':
			v, n := 0, 0
			for n < 2 && i+1 < len(s) && isHexDigit(s[i+1]) {
				i++
				n++
				v = v*16 + hexVal(s[i])
			}
			if n == 0 {
				sb.WriteByte('x')
			} else {
				sb.WriteByte(byte(v))
			}
		case '0', '1', '2', '3', '4', '5', '6', '7':
			v := int(e - '0')
			for n := 1; n < 3 && i+1 < len(s) && s[i+1] >= '0' && s[i+1] <= '7'; n++ {
				i++
				v = v*8 + int(s[i]-'0')
			}
			sb.WriteByte(byte(v))
		default:
			sb.WriteByte(e)
		}
	}
	return sb.String()
}

// invalidUTF8Offset finds the first invalid byte offset of a non-UTF-8
// string (for the escape-literal error message).
func invalidUTF8Offset(s string) int {
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			return i
		}
		i += size
	}
	return len(s)
}

func isHexDigit(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func hexVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	default:
		return int(c-'A') + 10
	}
}

func hexToBlob(s string) (string, bool) {
	if len(s)%2 != 0 {
		return "", false
	}
	var sb strings.Builder
	for i := 0; i < len(s); i += 2 {
		b, err := strconv.ParseUint(s[i:i+2], 16, 8)
		if err != nil {
			return "", false
		}
		sb.WriteByte(byte(b))
	}
	return sb.String(), true
}

// integerTypeFor picks the narrowest DuckDB integer class upstream uses
// for integer literals.
func integerTypeFor(v int64) string {
	if v >= math.MinInt32 && v <= math.MaxInt32 {
		return "INTEGER"
	}
	return "BIGINT"
}

// numberConstant ports upstream's TransformNumberLiteral: integers become
// INTEGER/BIGINT/HUGEINT (then DOUBLE), plain decimals become DECIMAL
// with derived width/scale (up to width 38), exponent forms become
// DOUBLE.
func numberConstant(sp ast.Span, text string) ast.Expr {
	clean := strings.ReplaceAll(text, "_", "")
	lower := strings.ToLower(clean)
	if strings.ContainsAny(lower, "e") {
		f, err := parseDouble(clean)
		if err != nil {
			raise("invalid number literal \"%s\"", text)
		}
		return constExpr(sp, ast.Value{Type: ast.LogicalType{ID: "DOUBLE"}, Kind: ast.ValueDouble, Float64: f})
	}
	if dot := strings.IndexByte(clean, '.'); dot >= 0 {
		return decimalConstant(sp, clean, dot)
	}
	// integer path
	if v, err := strconv.ParseInt(clean, 10, 64); err == nil {
		return constExpr(sp, ast.Value{Type: ast.LogicalType{ID: integerTypeFor(v)}, Kind: ast.ValueInt64, Int64: v})
	}
	if h, ok := parseHugeint(clean); ok {
		return constExpr(sp, ast.Value{Type: ast.LogicalType{ID: "HUGEINT"}, Kind: ast.ValueHugeint, Hugeint: h})
	}
	if h, ok := parseUint128(clean); ok {
		return constExpr(sp, ast.Value{Type: ast.LogicalType{ID: "UHUGEINT"}, Kind: ast.ValueHugeint, Hugeint: h})
	}
	f, err := parseDouble(clean)
	if err != nil {
		raise("invalid number literal \"%s\"", text)
	}
	return constExpr(sp, ast.Value{Type: ast.LogicalType{ID: "DOUBLE"}, Kind: ast.ValueDouble, Float64: f})
}

// parseDouble parses a float literal the way upstream does: values whose
// magnitude exceeds the double range become ±Inf rather than errors.
func parseDouble(s string) (float64, error) {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil && errors.Is(err, strconv.ErrRange) {
		return f, nil // ParseFloat already returned ±Inf
	}
	return f, err
}

// decimalConstant derives DECIMAL(width, scale) from the literal's
// digits: width counts the integer digits as written (leading zeros
// included) plus the scale; literals too wide for DECIMAL(38) fall back
// to DOUBLE.
func decimalConstant(sp ast.Span, clean string, dot int) ast.Expr {
	intPart := strings.TrimLeft(clean[:dot], "+-")
	fracPart := clean[dot+1:]
	digits := strings.TrimLeft(intPart, "0")
	scale := len(fracPart)
	width := len(intPart) + scale
	if width == 0 {
		width = 1
	}
	if width > 38 {
		f, err := parseDouble(clean)
		if err != nil {
			raise("invalid number literal \"%s\"", clean)
		}
		return constExpr(sp, ast.Value{Type: ast.LogicalType{ID: "DOUBLE"}, Kind: ast.ValueDouble, Float64: f})
	}
	unscaled := digits + fracPart
	if unscaled == "" {
		unscaled = "0"
	}
	typ := ast.LogicalType{ID: "DECIMAL", Width: width, Scale: scale}
	if v, err := strconv.ParseInt(unscaled, 10, 64); err == nil && width <= 18 {
		return constExpr(sp, ast.Value{Type: typ, Kind: ast.ValueInt64, Int64: v})
	}
	h, ok := parseHugeint(unscaled)
	if !ok {
		raise("invalid number literal \"%s\"", clean)
	}
	return constExpr(sp, ast.Value{Type: typ, Kind: ast.ValueHugeint, Hugeint: h})
}

// parseHugeint parses a decimal digit string into a 128-bit integer.
func parseHugeint(s string) (ast.Hugeint, bool) {
	neg := false
	if strings.HasPrefix(s, "-") {
		neg, s = true, s[1:]
	} else if strings.HasPrefix(s, "+") {
		s = s[1:]
	}
	if s == "" {
		return ast.Hugeint{}, false
	}
	var hi uint64 // upper 64 bits (unsigned during accumulation)
	var lo uint64
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return ast.Hugeint{}, false
		}
		// value = value*10 + d, 128-bit
		var carry uint64
		lo, carry = mul64(lo, 10)
		hiNew := hi*10 + carry
		if hiNew < carry {
			return ast.Hugeint{}, false
		}
		hi = hiNew
		lo += uint64(c - '0')
		if lo < uint64(c-'0') {
			hi++
			if hi == 0 {
				return ast.Hugeint{}, false
			}
		}
	}
	// range check: |v| < 2^127 (DuckDB's HUGEINT min is -(2^127-1))
	if hi > math.MaxInt64 {
		return ast.Hugeint{}, false
	}
	h := ast.Hugeint{Upper: int64(hi), Lower: lo}
	if neg {
		h = negateHugeint(h)
	}
	return h, true
}

// parseUint128 parses literals in (2^127-1, 2^128-1] as UHUGEINT.
func parseUint128(s string) (ast.Hugeint, bool) {
	if strings.HasPrefix(s, "+") {
		s = s[1:]
	}
	if s == "" || strings.HasPrefix(s, "-") {
		return ast.Hugeint{}, false
	}
	var hi, lo uint64
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return ast.Hugeint{}, false
		}
		var carry uint64
		lo, carry = mul64(lo, 10)
		if hi > (math.MaxUint64-carry)/10 {
			return ast.Hugeint{}, false
		}
		hi = hi*10 + carry
		d := uint64(c - '0')
		lo += d
		if lo < d {
			if hi == math.MaxUint64 {
				return ast.Hugeint{}, false
			}
			hi++
		}
	}
	return ast.Hugeint{Upper: int64(hi), Lower: lo}, true
}

func mul64(v, by uint64) (lo, carry uint64) {
	const mask = 0xffffffff
	vlo := v & mask
	vhi := v >> 32
	l := vlo * by
	h := vhi*by + (l >> 32)
	return (h&mask)<<32 | (l & mask), h >> 32
}

// ---- parameters --------------------------------------------------------

func (tc *transformContext) transformParameter(n tnode) ast.Expr {
	_, alt := n.sole().choice()
	p := &ast.ParameterExpression{}
	p.SetSpan(alt.span())
	switch alt.name() {
	case "QuestionMarkNumberedParameter":
		p.Kind = ast.ParameterQuestionNumbered
		p.Number = atoiOrRaise(alt.child(1).number())
	case "AnonymousParameter":
		p.Kind = ast.ParameterAnonymous
	case "NumberedParameter":
		p.Kind = ast.ParameterNumbered
		p.Number = atoiOrRaise(alt.child(1).number())
	case "ColLabelParameter":
		p.Kind = ast.ParameterNamed
		p.Name = identifierText(alt.child(1))
	default:
		shapeError(alt, "unknown parameter form")
	}
	tc.registerParam(p)
	return p
}

func atoiOrRaise(s string) int {
	v, err := strconv.Atoi(s)
	if err != nil {
		raise("invalid parameter number \"%s\"", s)
	}
	return v
}

// ---- subqueries, case, star -------------------------------------------

// SubqueryExpression <- SubqueryNot? SubqueryExists? SubqueryReference
func (tc *transformContext) transformSubqueryExpr(n tnode) ast.Expr {
	_, hasNot := n.child(0).opt()
	_, hasExists := n.child(1).opt()
	stmt := tc.transformSelectInternalStatement(n.child(2).sole().parens())
	sub := &ast.SubqueryExpression{Subquery: stmt, ComparisonType: ast.InvalidExpressionType}
	sub.SetSpan(n.span())
	if hasExists {
		sub.SubqueryType = ast.SubqueryExists
	} else {
		sub.SubqueryType = ast.SubqueryScalar
	}
	if hasNot {
		return notExpr(n.span(), sub)
	}
	return sub
}

// CaseExpression <- 'CASE' Expression? CaseWhenThen+ CaseElse? 'END'
func (tc *transformContext) transformCase(n tnode) ast.Expr {
	c := &ast.CaseExpression{}
	c.SetSpan(n.span())
	var operand ast.Expr
	if op, ok := n.child(1).opt(); ok {
		operand = tc.transformExpression(op)
	}
	for _, arm := range n.child(2).repeat() {
		when := tc.transformExpression(arm.child(1))
		then := tc.transformExpression(arm.child(3))
		if operand != nil {
			// synthesized `operand = when`, location-free like upstream
			cmp := &ast.ComparisonExpression{Type: ast.CompareEqual, Left: operand, Right: when}
			cmp.SetSpan(invalidSpan)
			when = cmp
		}
		c.CaseChecks = append(c.CaseChecks, ast.CaseCheck{When: when, Then: then})
	}
	if els, ok := n.child(3).opt(); ok {
		c.ElseExpr = tc.transformExpression(els.child(1))
	} else {
		c.ElseExpr = constExpr(invalidSpan, ast.Value{Type: ast.LogicalType{ID: "NULL"}, IsNull: true, Kind: ast.ValueNull})
	}
	return c
}

// StarExpression <- StarQualifierList? '*' ExcludeList? ReplaceList? RenameList?
func (tc *transformContext) transformStar(n tnode) ast.Expr {
	star := &ast.StarExpression{}
	star.SetSpan(n.span())
	if quals, ok := n.child(0).opt(); ok {
		var parts []string
		for _, q := range quals.sole().repeat() {
			parts = append(parts, identifierText(q.child(0)))
		}
		star.RelationName = strings.Join(parts, ".")
	}
	var excludePaths [][]string
	if excl, ok := n.child(2).opt(); ok {
		// ExcludeList <- ExcludeOrExcept ExcludeNames
		_, names := excl.child(1).sole().choice()
		var entries []tnode
		if names.name() == "ExcludeNameList" {
			entries = names.sole().parens().listElems()
		} else {
			entries = []tnode{names.sole()}
		}
		for _, e := range entries {
			path := excludeNamePath(e)
			for _, prev := range excludePaths {
				if qualifiedColumnsMatch(prev, path) {
					raise("Duplicate entry \"%s\" in EXCLUDE list", strings.Join(path, "."))
				}
			}
			excludePaths = append(excludePaths, path)
			if len(path) == 1 {
				star.ExcludeList = append(star.ExcludeList, path[0])
			} else {
				star.QualifiedExcludeList = append(star.QualifiedExcludeList, ast.QualifiedColumnName{Path: path})
			}
		}
	}
	inExclude := func(path []string) bool {
		for _, prev := range excludePaths {
			if qualifiedColumnsMatch(prev, path) {
				return true
			}
		}
		return false
	}
	replaced := map[string]bool{}
	if repl, ok := n.child(3).opt(); ok {
		// ReplaceList <- 'REPLACE' ReplaceEntries
		_, entriesAlt := repl.child(1).sole().choice()
		var entries []tnode
		if entriesAlt.name() == "ReplaceEntryList" {
			entries = entriesAlt.sole().parens().listElems()
		} else {
			entries = []tnode{entriesAlt.sole()}
		}
		for _, e := range entries {
			// ReplaceEntry <- Expression 'AS' ColumnReference
			expr := tc.transformExpression(e.child(0))
			ref := tc.transformColumnReference(e.child(2))
			cr, ok := ref.(*ast.ColumnRefExpression)
			if !ok || len(cr.ColumnNames) != 1 {
				raise("REPLACE target must be a single column name")
			}
			key := cr.ColumnNames[0]
			if replaced[strings.ToLower(key)] {
				raise("Duplicate entry \"%s\" in REPLACE list", key)
			}
			replaced[strings.ToLower(key)] = true
			star.ReplaceList = append(star.ReplaceList, ast.StarReplaceEntry{Key: key, Expr: expr})
		}
		for _, r := range star.ReplaceList {
			if inExclude([]string{r.Key}) {
				raise("Column \"%s\" cannot occur in both EXCLUDE and REPLACE list", r.Key)
			}
		}
	}
	if ren, ok := n.child(4).opt(); ok {
		// RenameList <- 'RENAME' RenameEntries
		_, entriesAlt := ren.child(1).sole().choice()
		var entries []tnode
		if entriesAlt.name() == "RenameEntryList" {
			entries = entriesAlt.sole().parens().listElems()
		} else {
			entries = []tnode{entriesAlt.sole()}
		}
		for _, e := range entries {
			// RenameEntry <- ExcludeName 'AS' Identifier
			path := excludeNamePath(e.child(0))
			star.RenameList = append(star.RenameList, ast.StarRenameEntry{
				Key:  ast.QualifiedColumnName{Path: path},
				Name: identifierText(e.child(2)),
			})
		}
		for _, r := range star.RenameList {
			if inExclude(r.Key.Path) {
				raise("Column \"%s\" cannot occur in both EXCLUDE and RENAME list", strings.Join(r.Key.Path, "."))
			}
			if replaced[strings.ToLower(r.Key.Path[len(r.Key.Path)-1])] {
				raise("Column \"%s\" cannot occur in both REPLACE and RENAME list", strings.Join(r.Key.Path, "."))
			}
		}
	}
	return star
}

// qualifiedColumnsMatch ports upstream's QualifiedColumnEquality: dotted
// exclude/rename paths compare by column name, with missing qualifiers
// acting as wildcards ("tbl.i" matches "i" but not "tbl2.i").
func qualifiedColumnsMatch(a, b []string) bool {
	if !strings.EqualFold(a[len(a)-1], b[len(b)-1]) {
		return false
	}
	aq, bq := a[:len(a)-1], b[:len(b)-1]
	for i := 1; i <= 3; i++ {
		var av, bv string
		if len(aq) >= i {
			av = aq[len(aq)-i]
		}
		if len(bq) >= i {
			bv = bq[len(bq)-i]
		}
		if av != "" && bv != "" && !strings.EqualFold(av, bv) {
			return false
		}
	}
	return true
}

// excludeNamePath extracts the dotted path of an ExcludeName.
func excludeNamePath(n tnode) []string {
	_, alt := n.sole().choice()
	switch alt.name() {
	case "ExcludeDottedName":
		// DottedIdentifier <- Identifier DotColLabel*
		d := alt.sole()
		path := []string{identifierText(d.child(0))}
		for _, dot := range d.child(1).repeat() {
			path = append(path, identifierText(dot.child(1)))
		}
		return path
	case "ExcludeColumnName":
		return []string{identifierText(alt.sole())}
	}
	shapeError(alt, "unknown exclude name")
	return nil
}

// ---- column references -------------------------------------------------

func (tc *transformContext) transformColumnReference(n tnode) ast.Expr {
	_, alt := n.sole().choice()
	var names []string
	switch alt.name() {
	case "CatalogReservedSchemaTableColumnName":
		names = qualificationChain(alt, 4)
	case "SchemaReservedTableColumnName":
		names = qualificationChain(alt, 3)
	case "TableReservedColumnName":
		names = qualificationChain(alt, 2)
	case "NestedColumnName":
		// IdentifierDot* ColumnName
		var spans []ast.Span
		for _, id := range alt.child(0).repeat() {
			names = append(names, identifierText(id.child(0)))
			spans = append(spans, id.child(0).span())
		}
		names = append(names, identifierText(alt.child(1)))
		spans = append(spans, alt.child(1).span())
		if len(names) > 4 {
			return deepColumnRef(n.span(), names, spans)
		}
	default:
		shapeError(alt, "unknown column reference")
	}
	ref := &ast.ColumnRefExpression{ColumnNames: names}
	ref.SetSpan(n.span())
	return ref
}

// deepColumnRef splits a >4-part dotted path: the first four parts stay
// a column reference (catalog.schema.table.column), the rest become
// struct extraction. Only the outermost extract carries the full
// location; the keys span their dot through their identifier.
func deepColumnRef(whole ast.Span, names []string, spans []ast.Span) ast.Expr {
	ref := &ast.ColumnRefExpression{ColumnNames: names[:4]}
	ref.SetSpan(ast.Span{Start: whole.Start, End: spans[3].End})
	var out ast.Expr = ref
	for i := 4; i < len(names); i++ {
		key := constExpr(ast.Span{Start: spans[i-1].End, End: spans[i].End}, varcharValue(names[i]))
		sp := invalidSpan
		if i == len(names)-1 {
			sp = whole
		}
		out = opExpr(sp, ast.StructExtract, out, key)
	}
	return out
}

// qualificationChain reads count-1 qualification nodes (each `Name '.'`)
// plus the final name from a qualification rule node.
func qualificationChain(n tnode, count int) []string {
	names := make([]string, 0, count)
	for i := 0; i < count-1; i++ {
		names = append(names, identifierText(n.child(i).child(0)))
	}
	names = append(names, identifierText(n.child(count-1)))
	return names
}
