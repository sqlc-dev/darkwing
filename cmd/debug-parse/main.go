// Command debug-parse tokenizes and matches SQL from the command line,
// dumping either the token stream or the raw ParseResult tree — the
// milestone-1 window into the engine, before transformers and a public
// Parse API exist.
//
// Usage:
//
//	debug-parse [-tokens] 'SELECT 1'
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/sqlc-dev/darkwing/internal/matcher"
	"github.com/sqlc-dev/darkwing/lexer"
)

func main() {
	tokensOnly := flag.Bool("tokens", false, "dump the token stream instead of the parse tree")
	flag.Parse()
	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: debug-parse [-tokens] <sql>")
		os.Exit(2)
	}
	sql := strings.Join(flag.Args(), " ")
	if err := run(sql, *tokensOnly); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(sql string, tokensOnly bool) error {
	tokens, err := lexer.Tokenize(sql)
	if err != nil {
		return err
	}
	if tokensOnly {
		for _, t := range tokens {
			fmt.Printf("%-16s %4d..%-4d %q\n", t.Kind, t.Span.Start, t.Span.End, t.Text)
		}
		return nil
	}
	engine, err := matcher.Default()
	if err != nil {
		return err
	}
	// peel one TopLevelStatement at a time, exactly like upstream's
	// Parser::ParseQuery loop
	pos := 0
	statement := 0
	for pos < len(tokens) {
		result, newPos, err := engine.MatchTopLevel(tokens, pos)
		if err != nil {
			return err
		}
		if newPos == pos {
			// no progress; should be unreachable with a well-formed
			// TopLevelStatement rule
			return fmt.Errorf("matcher made no progress at token %d", pos)
		}
		pos = newPos
		// TopLevelStatement <- Statement? (';'+ / EndOfInput): a
		// separator-only match yields no statement, as upstream's
		// TransformTopLevelStatement returns nullptr for it
		if lr, ok := result.(*matcher.ListResult); ok && len(lr.Children) > 0 {
			if opt, ok := lr.Children[0].(*matcher.OptionalResult); ok && !opt.HasResult() {
				continue
			}
		}
		statement++
		fmt.Printf("-- statement %d\n", statement)
		fmt.Print(matcher.Dump(result))
	}
	return nil
}
