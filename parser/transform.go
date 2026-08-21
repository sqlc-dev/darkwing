// transform.go: the transformer registry and shared transform state — the
// port of upstream's PEGTransformerFactory (transformer/peg_transformer.cpp).
// One transform function per grammar rule, registered by rule name;
// rules not in the registry are either consumed structurally by their
// parent's transformer or belong to later milestones (ErrUnsupported).
package parser

import (
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

// unsupportedStatements names the Statement alternatives that belong to
// milestone 5; the engine accepts them but Parse reports ErrUnsupported.
var unsupportedStatements = map[string]bool{
	"ExternalResourceStatement": true, // INSTALL/LOAD/UPDATE EXTENSIONS/...
	"SetStatement":              true,
	"PragmaStatement":           true,
	"CallStatement":             true,
	"CopyStatement":             true,
	"ExplainStatement":          true,
	"PrepareStatement":          true,
	"ExecuteStatement":          true,
	"TransactionStatement":      true,
	"AttachStatement":           true,
	"UseStatement":              true,
	"DetachStatement":           true,
	"CheckpointStatement":       true,
	"VacuumStatement":           true,
	"ResetStatement":            true,
	"ExportStatement":           true,
	"ImportStatement":           true,
	"CommentStatement":          true,
	"DeallocateStatement":       true,
	"LoadStatement":             true,
	"InstallStatement":          true,
	"UpdateExtensionsStatement": true,
	"AnalyzeStatement":          true,
	"ConnectStatement":          true,
	"DisconnectStatement":       true,
}

// transformStatement dispatches one Statement node (LIST(Statement)
// wrapping a choice of statement rules).
func (tc *transformContext) transformStatement(n tnode) ast.Stmt {
	_, inner := n.sole().choice()
	name := inner.name()
	if fn, ok := statementTransforms[name]; ok {
		return fn(tc, inner)
	}
	if unsupportedStatements[name] {
		panic(&internalErrorUnsupported{rule: name})
	}
	shapeError(inner, "no statement transform registered")
	return nil
}

// internalErrorUnsupported is panicked for milestone-5 statements and
// converted to *unsupportedError at the Parse boundary.
type internalErrorUnsupported struct {
	rule string
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
	if tc.hasNamed && tc.hasPosition {
		raise("Mixing named and positional parameters is not supported yet")
	}
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

// namedWindow resolves a window name in the innermost scope.
func (tc *transformContext) namedWindow(name string) *ast.WindowExpression {
	for i := len(tc.windows) - 1; i >= 0; i-- {
		if w, ok := tc.windows[i][name]; ok {
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
	if _, dup := scope[name]; dup {
		raise("window \"%s\" is already defined", name)
	}
	scope[name] = w
}
