package ast

// LogicalType is the small slice of DuckDB's type system the parser's
// constants can carry: primitives, DECIMAL (width/scale) and LIST (the
// empty-list sentinels in slice bounds). Everything else stays unbound
// (see TypeExpression).
type LogicalType struct {
	// ID is the LogicalTypeId spelling as serialized (INTEGER, DECIMAL,
	// LIST, ...).
	ID string `json:"id"`
	// Width and Scale are set for DECIMAL.
	Width int `json:"width,omitempty"`
	Scale int `json:"scale,omitempty"`
	// Child is set for LIST.
	Child *LogicalType `json:"child,omitempty"`
}

// ValueKind discriminates the payload of a Value.
type ValueKind uint8

const (
	// ValueNull has no payload.
	ValueNull ValueKind = iota
	// ValueBool uses Bool.
	ValueBool
	// ValueInt64 uses Int64 (INTEGER, BIGINT, and the unscaled value of
	// DECIMALs up to width 18).
	ValueInt64
	// ValueHugeint uses Hugeint (HUGEINT and wider DECIMALs).
	ValueHugeint
	// ValueDouble uses Float64.
	ValueDouble
	// ValueString uses Str.
	ValueString
	// ValueEmptyList has no payload (the slice-bound sentinel).
	ValueEmptyList
)

// Hugeint is a 128-bit signed integer, upper:lower.
type Hugeint struct {
	Upper int64  `json:"upper"`
	Lower uint64 `json:"lower"`
}

// Value is a constant value with its DuckDB logical type — the payload of
// a ConstantExpression. Port of duckdb::Value restricted to what the
// transformer produces.
type Value struct {
	Type    LogicalType `json:"type"`
	IsNull  bool        `json:"is_null,omitempty"`
	Kind    ValueKind   `json:"-"`
	Bool    bool        `json:"bool,omitempty"`
	Int64   int64       `json:"int64,omitempty"`
	Hugeint Hugeint     `json:"hugeint,omitempty"`
	Float64 float64     `json:"float64,omitempty"`
	Str     string      `json:"str,omitempty"`
}

// LiteralKind is the syntactic kind of a literal atom, spelled as
// DuckDB's LiteralKind enum serializes.
type LiteralKind string

const (
	LiteralNull    LiteralKind = "NULL_LITERAL"
	LiteralBoolean LiteralKind = "BOOLEAN"
	// LiteralInteger is digits with an optional leading '-' and '_'
	// separators; no '.' and no exponent.
	LiteralInteger LiteralKind = "INTEGER"
	// LiteralNumeric is number text with a '.' and/or an exponent.
	LiteralNumeric LiteralKind = "NUMERIC"
	// LiteralString is decoded string text (quotes stripped, escapes
	// decoded).
	LiteralString LiteralKind = "STRING"
	// LiteralHex is X'..': the hex digits without prefix and quotes.
	LiteralHex LiteralKind = "HEX"
	// LiteralBit is B'..': the bit digits without prefix and quotes.
	LiteralBit LiteralKind = "BIT"
)

// Literal is a literal atom as written in the query text — what a
// ConstantExpression serializes since DuckDB v2.0. Port of
// duckdb::Literal; it holds no type information (the binder turns it
// into a Value).
type Literal struct {
	Kind LiteralKind `json:"kind"`
	Text string      `json:"text"`
}

// IsNumeric reports whether the literal is an INTEGER or NUMERIC atom.
func (l Literal) IsNumeric() bool { return l.Kind == LiteralInteger || l.Kind == LiteralNumeric }

// Negate toggles the leading '-' of a numeric literal (Literal::Negate).
func (l Literal) Negate() Literal {
	if len(l.Text) > 0 && l.Text[0] == '-' {
		return Literal{Kind: l.Kind, Text: l.Text[1:]}
	}
	return Literal{Kind: l.Kind, Text: "-" + l.Text}
}
