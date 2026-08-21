// Command debug-parse tokenizes and matches SQL from the command line,
// dumping the token stream, the raw ParseResult tree, or the typed AST
// (as JSON).
//
// Usage:
//
//	debug-parse [-tokens|-ast] 'SELECT 1'
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/sqlc-dev/darkwing/internal/matcher"
	"github.com/sqlc-dev/darkwing/lexer"
	"github.com/sqlc-dev/darkwing/parser"
)

func main() {
	tokensOnly := flag.Bool("tokens", false, "dump the token stream instead of the parse tree")
	astMode := flag.Bool("ast", false, "dump the typed AST as JSON instead of the parse tree")
	flag.Parse()
	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: debug-parse [-tokens|-ast] <sql>")
		os.Exit(2)
	}
	sql := strings.Join(flag.Args(), " ")
	if *astMode {
		if err := runAST(sql); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		return
	}
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
	results, err := engine.MatchAll(tokens)
	if err != nil {
		return err
	}
	statement := 0
	for _, result := range results {
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

func runAST(sql string) error {
	stmts, err := parser.Parse(context.Background(), strings.NewReader(sql))
	if err != nil {
		return err
	}
	for i, stmt := range stmts {
		fmt.Printf("-- statement %d (%T)\n", i+1, stmt)
		b, err := json.MarshalIndent(stmt, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(b))
	}
	return nil
}
