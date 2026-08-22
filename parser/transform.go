// transform.go: the transformer registry and shared transform state — the
// port of upstream's PEGTransformerFactory (transformer/peg_transformer.cpp).
// One transform function per grammar rule, registered by rule name; since
// milestone 5 every Statement alternative is registered, and rules not in
// the registry are consumed structurally by their parent's transformer.
package parser

import (
	"strings"

	"github.com/sqlc-dev/darkwing/ast"
)

// transformContext carries per-statement transform state.
type transformContext struct {
	src string

	// Prepared-parameter numbering state (upstream keeps the same
	// counters on the transformer).
	paramCount  int
	paramSeen   map[string]int // identifier -> index
	paramOrder  []string
	hasNamed    bool
	hasPosition bool

	// windows maps named WINDOW definitions of the innermost SELECT
	// (stack, innermost last).
	windows []map[string]*ast.WindowExpression

	// inWindowDefinition is set while transforming a window frame
	// definition; window functions are not allowed inside one.
	inWindowDefinition bool

	// pivotEntries counts PIVOT columns whose values must be extracted
	// from the data (no IN list, no enum) — upstream's pivot_entries,
	// kept as a count since darkwing does not expand them into enum
	// creation statements. pivotEntryHasParams records whether prepared
	// parameters had been seen when such an entry was recorded.
	pivotEntries        int
	pivotEntryHasParams bool
}

// pivotEntryCheck ports PEGTransformer::PivotEntryCheck: CREATE VIEW and
// CREATE MACRO bodies cannot contain pivots whose values come from the
// data.
func (tc *transformContext) pivotEntryCheck(kind string) {
	if tc.pivotEntries > 0 {
		raise("PIVOT statements with pivot elements extracted from the data cannot be used in %ss.\n"+
			"In order to use PIVOT in a %s the PIVOT values must be manually specified, e.g.:\n"+
			"PIVOT ... ON ... IN (val1, val2, ...)", kind, kind)
	}
}

func newTransformContext(src string) *transformContext {
	return &transformContext{src: src, paramSeen: map[string]int{}}
}

// stmtTransform is a statement-producing transform function. The node is
// the named rule's own list node (e.g. LIST(SelectStatement)).
type stmtTransform func(*transformContext, tnode) ast.Stmt

// statementTransforms is the registry keyed by grammar rule name,
// populated by the transform_*.go files' init functions.
var statementTransforms = map[string]stmtTransform{}

func register(rule string, fn stmtTransform) {
	if _, dup := statementTransforms[rule]; dup {
		panic("duplicate statement transform: " + rule)
	}
	statementTransforms[rule] = fn
}

// transformStatement dispatches one Statement node (LIST(Statement)
// wrapping a choice of statement rules).
func (tc *transformContext) transformStatement(n tnode) ast.Stmt {
	_, inner := n.sole().choice()
	if fn, ok := statementTransforms[inner.name()]; ok {
		return fn(tc, inner)
	}
	shapeError(inner, "no statement transform registered")
	return nil
}

// finishStatement attaches the collected parameter map to the statement.
func (tc *transformContext) finishStatement(stmt ast.Stmt) {
	if len(tc.paramOrder) == 0 {
		return
	}
	params := make([]ast.NamedParam, 0, len(tc.paramOrder))
	for _, id := range tc.paramOrder {
		params = append(params, ast.NamedParam{Name: id, Index: tc.paramSeen[id]})
	}
	switch s := stmt.(type) {
	case *ast.SelectStatement:
		s.NamedParams = params
	case *ast.InsertStatement:
		s.NamedParams = params
	case *ast.UpdateStatement:
		s.NamedParams = params
	case *ast.DeleteStatement:
		s.NamedParams = params
	case *ast.MergeIntoStatement:
		s.NamedParams = params
	case *ast.TruncateStatement:
		s.NamedParams = params
	case *ast.CreateStatement:
		s.NamedParams = params
	case *ast.DropStatement:
		s.NamedParams = params
	case *ast.AlterStatement:
		s.NamedParams = params
	case *ast.SetStatement:
		s.NamedParams = params
	case *ast.PragmaStatement:
		s.NamedParams = params
	case *ast.CallStatement:
		s.NamedParams = params
	case *ast.ExplainStatement:
		s.NamedParams = params
	case *ast.ExecuteStatement:
		s.NamedParams = params
	case *ast.CopyStatement:
		s.NamedParams = params
	case *ast.AttachStatement:
		s.NamedParams = params
	case *ast.ConnectStatement:
		s.NamedParams = params
	case *ast.ExportStatement:
		s.NamedParams = params
	case *ast.ExternalResourceStatement:
		s.NamedParams = params
	}
}

// registerParam assigns a parameter its index, mirroring upstream: `?`
// takes max-seen+1, explicit numbers raise the high-water mark, named
// parameters are numbered by first use, and mixing named with positional
// raises upstream's transformer error.
func (tc *transformContext) registerParam(p *ast.ParameterExpression) {
	switch p.Kind {
	case ast.ParameterAnonymous:
		tc.hasPosition = true
		tc.paramCount++
		p.Number = tc.paramCount
	case ast.ParameterQuestionNumbered, ast.ParameterNumbered:
		tc.hasPosition = true
		if p.Number > tc.paramCount {
			tc.paramCount = p.Number
		}
	case ast.ParameterNamed:
		tc.hasNamed = true
		if idx, ok := tc.paramSeen[p.Name]; ok {
			p.Number = idx
		} else {
			tc.paramCount++
			p.Number = tc.paramCount
		}
	}
	// mixing named and positional parameters raises NotImplemented
	// upstream (post-parse for the corpus oracle), so darkwing keeps
	// numbering and accepts
	id := p.Identifier()
	if _, ok := tc.paramSeen[id]; !ok {
		tc.paramSeen[id] = p.Number
		tc.paramOrder = append(tc.paramOrder, id)
	}
}

// pushWindowScope/popWindowScope bracket a SimpleSelect's named-window
// scope.
func (tc *transformContext) pushWindowScope() {
	tc.windows = append(tc.windows, map[string]*ast.WindowExpression{})
}

func (tc *transformContext) popWindowScope() {
	tc.windows = tc.windows[:len(tc.windows)-1]
}

// namedWindow resolves a window name in the innermost scope (window
// names are case-insensitive identifiers).
func (tc *transformContext) namedWindow(name string) *ast.WindowExpression {
	key := strings.ToLower(name)
	for i := len(tc.windows) - 1; i >= 0; i-- {
		if w, ok := tc.windows[i][key]; ok {
			return w
		}
	}
	return nil
}

func (tc *transformContext) defineWindow(name string, w *ast.WindowExpression) {
	if len(tc.windows) == 0 {
		tc.pushWindowScope()
	}
	scope := tc.windows[len(tc.windows)-1]
	key := strings.ToLower(name)
	if _, dup := scope[key]; dup {
		raise("window \"%s\" is already defined", name)
	}
	scope[key] = w
}
