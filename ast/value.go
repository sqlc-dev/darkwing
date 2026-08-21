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
