// Package parser is darkwing's public API: Parse SQL text into the typed
// AST of package ast. The engine (grammar + matcher) accepts or rejects;
// the transformer in this package shapes the generic parse tree into AST
// nodes, mirroring DuckDB's transformer/*.cpp.
package parser

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/sqlc-dev/darkwing/ast"
	"github.com/sqlc-dev/darkwing/internal/matcher"
	"github.com/sqlc-dev/darkwing/lexer"
	"github.com/sqlc-dev/darkwing/token"
)

// Error is a parse error, rendered like the pinned DuckDB build's parser
// errors.
type Error struct {
	// Msg is the bare message ("syntax error at or near ...").
	Msg string
	// Offset is the byte offset of the error in the input, or -1.
	Offset int
	// Line and Column are 1-based, derived from Offset.
	Line, Column int
}

func (e *Error) Error() string {
	return "Parser Error: " + e.Msg
}

// ErrUnsupported marked statements the transformer did not cover yet.
// Since milestone 5 every statement has a transformer and Parse no longer
// returns it; the variable stays for API compatibility.
var ErrUnsupported = errors.New("darkwing: statement not supported yet")

// Parse reads SQL from r and returns its statements. Statement spans tile
// the input: statement i covers [prev end, own end), the first starts at
// 0 and the last ends at len(input), so slicing the source by span
// recovers each statement's text with its leading comments.
func Parse(ctx context.Context, r io.Reader) ([]ast.Stmt, error) {
	src, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	return parseString(ctx, string(src))
}

// ParseStatement parses exactly one statement.
func ParseStatement(ctx context.Context, sql string) (ast.Stmt, error) {
	stmts, err := parseString(ctx, sql)
	if err != nil {
		return nil, err
	}
	if len(stmts) != 1 {
		return nil, fmt.Errorf("darkwing: expected exactly one statement, got %d", len(stmts))
	}
	return stmts[0], nil
}

// ParseExpr parses a single scalar expression.
func ParseExpr(ctx context.Context, sql string) (expr ast.Expr, err error) {
	engine, err := matcher.Default()
	if err != nil {
		return nil, err
	}
	tokens, err := lexer.Tokenize(sql)
	if err != nil {
		return nil, newError(sql, err)
	}
	result, pos, err := engine.MatchExpression(tokens)
	if err != nil {
		return nil, newError(sql, err)
	}
	// the expression must consume everything up to end of input
	if pos < len(tokens) && tokens[pos].Kind != token.EndOfInput {
		return nil, newError(sql, &matcher.SyntaxError{Token: tokens[pos]})
	}
	defer catchInternal(&err)
	tc := newTransformContext(sql)
	return tc.transformExpression(tnode{result}), nil
}

func parseString(ctx context.Context, src string) (stmts []ast.Stmt, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	engine, err := matcher.Default()
	if err != nil {
		return nil, err
	}
	tokens, err := lexer.Tokenize(src)
	if err != nil {
		return nil, newError(src, err)
	}
	defer catchInternal(&err)
	prevEnd := 0
	pos := 0
	for pos < len(tokens) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		result, newPos, err := engine.MatchTopLevel(tokens, pos)
		if err != nil {
			return nil, newError(src, err)
		}
		if newPos == pos {
			return nil, &internalError{msg: "matcher made no progress"}
		}
		// TopLevelStatement <- Statement? (';'+ / EndOfInput)
		stmtOpt, ok := tnode{result}.child(0).opt()
		end := consumedEnd(tokens, pos, newPos, len(src))
		pos = newPos
		if !ok {
			// empty statement: fold its extent into the next span
			continue
		}
		tc := newTransformContext(src)
		stmt := tc.transformStatement(stmtOpt)
		if stmt == nil {
			continue
		}
		// port of CreatePivotStatement's parameter check: data-extracted
		// pivot values cannot mix with prepared parameters
		if tc.pivotEntries > 0 && tc.pivotEntryHasParams {
			raise("PIVOT statements with pivot elements extracted from the data cannot have parameters in their source.\n" +
				"In order to use parameters the PIVOT values must be manually specified, e.g.:\n" +
				"PIVOT ... ON ... IN (val1, val2, ...)")
		}
		setStmtSpan(stmt, ast.Span{Start: prevEnd, End: end})
		tc.finishStatement(stmt)
		stmts = append(stmts, stmt)
		prevEnd = end
	}
	// the last span stretches to the end of the input so the spans tile
	if len(stmts) > 0 {
		last := stmts[len(stmts)-1]
		sp := spanOfStmt(last)
		if sp.End < len(src) {
			setStmtSpan(last, ast.Span{Start: sp.Start, End: len(src)})
		}
	}
	return stmts, nil
}

// consumedEnd returns the byte offset just past the tokens consumed by
// the match from pos to newPos (excluding the end-of-input sentinel).
func consumedEnd(tokens []token.Token, pos, newPos, srcLen int) int {
	for i := newPos - 1; i >= pos; i-- {
		if tokens[i].Kind == token.EndOfInput {
			continue
		}
		return tokens[i].Span.End
	}
	return srcLen
}

type spanSetter interface {
	SetSpan(ast.Span)
	Pos() int
	End() int
}

func setStmtSpan(stmt ast.Stmt, sp ast.Span) {
	if s, ok := stmt.(spanSetter); ok {
		s.SetSpan(sp)
	}
}

func spanOfStmt(stmt ast.Stmt) ast.Span {
	return ast.Span{Start: stmt.Pos(), End: stmt.End()}
}

// catchInternal converts transformer shape panics into errors.
func catchInternal(err *error) {
	if r := recover(); r != nil {
		switch e := r.(type) {
		case *internalError:
			*err = e
		case *Error:
			*err = e
		default:
			panic(r)
		}
	}
}

// newError renders a lexer or matcher error as *Error with position info.
func newError(src string, err error) error {
	var msg string
	offset := -1
	switch e := err.(type) {
	case *matcher.SyntaxError:
		msg = e.Error()
		offset = e.Token.Span.Start
	case *matcher.MatchError:
		msg = e.Msg
	case *lexer.Error:
		msg = e.Msg
		offset = e.Offset
	default:
		msg = err.Error()
	}
	pe := &Error{Msg: msg, Offset: offset, Line: 1, Column: 1}
	if offset >= 0 {
		line, col := 1, 1
		for i := 0; i < offset && i < len(src); i++ {
			if src[i] == '\n' {
				line++
				col = 1
			} else {
				col++
			}
		}
		pe.Line, pe.Column = line, col
	}
	return pe
}

// raise reports a transformer-level parse error (upstream's
// ParserException raised from transform functions).
func raise(format string, args ...any) {
	panic(&Error{Msg: fmt.Sprintf(format, args...), Offset: -1, Line: 1, Column: 1})
}

var _ = strings.TrimSpace
