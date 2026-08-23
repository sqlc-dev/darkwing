// Command difftest is the milestone-6 differential fuzzer: it mutates
// corpus seed statements at token boundaries and compares darkwing's
// accept/reject verdict (and, for syntax errors, the message) against the
// pinned DuckDB CLI — the strongest check on the matcher's ordered-choice
// fidelity, cheap to run for hours in a cron lane.
//
// Usage:
//
//	difftest [-n 500] [-duration 0] [-seed 1] [-jobs N] [-corpus dir]
//
// Deterministic for a given -seed/-corpus pair (a fixed -n; -duration
// bounds wall clock instead). Exit status 1 when any disagreement is
// found. The oracle binary is located via $DARKWING_DUCKDB (see
// internal/duckdbsrc).
//
// Verdict policy mirrors the corpus harness: darkwing must reject iff
// the oracle raises a Parser Error. Two known oracle imprecisions are
// filtered rather than reported: mutants that mention the nested-SQL
// functions (query, query_table, json_serialize_sql, ...) whose
// bind-time inner parse also raises Parser Error, and oracle rejects
// whose message is not a syntax error (bind-time ParserException raised
// outside the parse pipeline — the corpus todo category). Oracle
// timeouts and crashes leave a mutant unverified and are skipped.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sqlc-dev/darkwing/internal/duckdbsrc"
	"github.com/sqlc-dev/darkwing/internal/testfile"
	"github.com/sqlc-dev/darkwing/lexer"
	"github.com/sqlc-dev/darkwing/parser"
	"github.com/sqlc-dev/darkwing/token"
)

func main() {
	n := flag.Int("n", 500, "number of mutants to test (ignored with -duration)")
	duration := flag.Duration("duration", 0, "run for this long instead of a fixed -n")
	seed := flag.Int64("seed", 1, "mutation RNG seed")
	jobs := flag.Int("jobs", runtime.NumCPU(), "concurrent oracle processes")
	corpusDir := flag.String("corpus", filepath.Join("parser", "testdata", "corpus"), "corpus directory for seed statements")
	maxPrint := flag.Int("max-print", 25, "max disagreements to print in detail")
	verbose := flag.Bool("v", false, "print every mutant tested")
	flag.Parse()

	bin, err := duckdbsrc.Find()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	seeds, err := loadSeeds(*corpusDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if len(seeds) == 0 {
		fmt.Fprintf(os.Stderr, "no seed statements under %s (run cmd/regenerate first)\n", *corpusDir)
		os.Exit(1)
	}
	fmt.Printf("difftest: %d seeds, seed=%d, jobs=%d\n", len(seeds), *seed, *jobs)

	// one generator goroutine keeps mutation deterministic for a given
	// -seed; workers race only for oracle execution
	mutants := make(chan string, *jobs)
	go func() {
		defer close(mutants)
		rng := rand.New(rand.NewSource(*seed))
		deadline := time.Time{}
		if *duration > 0 {
			deadline = time.Now().Add(*duration)
		}
		for i := 0; *duration > 0 || i < *n; i++ {
			if !deadline.IsZero() && time.Now().After(deadline) {
				return
			}
			mutants <- mutate(rng, seeds)
		}
	}()

	var (
		mu            sync.Mutex
		disagreements int
		tested        atomic.Int64
		unverified    atomic.Int64
		softSkipped   atomic.Int64
	)
	var wg sync.WaitGroup
	for w := 0; w < *jobs; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			oracle := &duckdbsrc.Oracle{Binary: bin}
			for sql := range mutants {
				if nestedParseSQL(sql) || shellArtifact(sql) {
					softSkipped.Add(1)
					continue
				}
				verdict, err := oracle.Run(sql)
				if err != nil {
					fmt.Fprintf(os.Stderr, "oracle: %v\n", err)
					os.Exit(1)
				}
				if verdict.TimedOut || verdict.Crashed {
					unverified.Add(1)
					continue
				}
				got := classify(sql)
				if !verdict.Reject && got.reject && verdict.PostParseError && multiStatement(sql) {
					// the batch-mode CLI stops at the first failing
					// statement: a post-parse error on an earlier
					// statement leaves the rest unparsed, so the
					// oracle's accept does not cover darkwing's reject
					unverified.Add(1)
					continue
				}
				tested.Add(1)
				if *verbose {
					fmt.Printf("mutant: %q oracle-reject=%v darkwing-reject=%v\n", sql, verdict.Reject, got.reject)
				}
				if diff := compare(verdict, got); diff != "" {
					if verdict.Reject && !got.reject && !isSyntaxError(verdict.Error) {
						// bind-time ParserException outside the parse
						// pipeline: the corpus todo category, not a bug
						softSkipped.Add(1)
						continue
					}
					mu.Lock()
					disagreements++
					if disagreements <= *maxPrint {
						fmt.Printf("DISAGREEMENT (%s)\nSQL: %s\noracle:   %s\ndarkwing: %s\n\n",
							diff, sql, describe(verdict.Reject, verdict.Error), describe(got.reject, got.detail))
					}
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()

	fmt.Printf("tested %d mutants: %d disagreements (%d unverified, %d skipped as bind-time/nested-parse)\n",
		tested.Load(), disagreements, unverified.Load(), softSkipped.Load())
	if disagreements > 0 {
		os.Exit(1)
	}
}

// loadSeeds collects every corpus statement that is not skip-listed —
// including must-rejects, whose mutants probe the error paths.
func loadSeeds(root string) ([]string, error) {
	var seeds []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".test") {
			return nil
		}
		file, err := testfile.Read(path)
		if err != nil {
			return err
		}
		meta, err := testfile.ReadMetadata(path)
		if err != nil {
			return err
		}
		for i := range file.Cases {
			c := &file.Cases[i]
			if _, skip := meta.Skip[c.Key()]; skip {
				continue
			}
			if _, ok := lexer.Tokenize(c.SQL); ok == nil {
				seeds = append(seeds, c.SQL)
			}
		}
		return nil
	})
	return seeds, err
}

// verdict mirrors the corpus harness's classification: tokenizer,
// matcher and transformer parse errors are rejects.
type verdict struct {
	reject bool
	detail string
}

func classify(sql string) (v verdict) {
	defer func() {
		if r := recover(); r != nil {
			v = verdict{reject: false, detail: fmt.Sprintf("transformer panic: %v", r)}
		}
	}()
	_, err := parser.Parse(context.Background(), strings.NewReader(sql))
	if err == nil {
		return verdict{}
	}
	var pe *parser.Error
	if errors.As(err, &pe) {
		return verdict{reject: true, detail: pe.Error()}
	}
	return verdict{reject: false, detail: "internal error: " + err.Error()}
}

func isSyntaxError(msg string) bool {
	return strings.HasPrefix(msg, "Parser Error: syntax error at or near")
}

// compare returns a non-empty disagreement kind when darkwing diverges
// from the oracle on the mutant.
func compare(oracle duckdbsrc.Verdict, got verdict) string {
	if !got.reject && got.detail != "" {
		return "internal error"
	}
	if oracle.Reject != got.reject {
		if oracle.Reject {
			return "oracle rejects, darkwing accepts"
		}
		return "darkwing rejects, oracle accepts"
	}
	// message fidelity: pinned only for syntax errors — a bind-time
	// oracle reject can coincide with a darkwing reject for a different
	// (legitimate) reason
	if oracle.Reject && isSyntaxError(oracle.Error) {
		// the oracle records the trimmed first line of a message; a token
		// spelling with an embedded newline (unterminated dollar quote)
		// spills across lines, so darkwing's line is trimmed to match
		firstLine, _, _ := strings.Cut(got.detail, "\n")
		if strings.TrimSpace(firstLine) != oracle.Error {
			return "message mismatch"
		}
	}
	return ""
}

func describe(reject bool, detail string) string {
	if !reject {
		if detail != "" {
			return "accept (" + detail + ")"
		}
		return "accept"
	}
	return "reject (" + detail + ")"
}

// shellArtifact reports mutants the CLI shell intercepts before the
// parser sees them: a line starting with '#' in column 0 is a shell
// comment (an indented '#' reaches the parser as an operator) and a
// line starting with '.' is a dot command.
func shellArtifact(sql string) bool {
	for _, line := range strings.Split(sql, "\n") {
		if strings.HasPrefix(line, "#") || strings.HasPrefix(line, ".") {
			return true
		}
	}
	return false
}

// multiStatement reports whether sql contains a statement separator with
// content after it (a ';' inside a string or comment also matches — the
// check only widens the unverified skip, never a reported disagreement).
func multiStatement(sql string) bool {
	return strings.Contains(strings.TrimRight(sql, "; \t\r\n"), ";")
}

// nestedParseSQL reports statements that mention the functions whose
// arguments the oracle parses as SQL at bind time — their Parser Errors
// come from the inner parse the grammar never sees.
func nestedParseSQL(sql string) bool {
	l := strings.ToLower(sql)
	for _, fn := range []string{"query", "query_table", "json_serialize_sql", "nextval", "currval"} {
		if strings.Contains(l, fn) {
			return true
		}
	}
	return false
}

// mutate produces one mutant from the seed pool. All mutations cut and
// splice the original source at token span boundaries, preserving the
// seed's exact spelling (strings, comments, whitespace).
func mutate(rng *rand.Rand, seeds []string) string {
	src := seeds[rng.Intn(len(seeds))]
	toks := realTokens(src)
	if len(toks) == 0 {
		return src
	}
	switch rng.Intn(6) {
	case 0: // delete a token
		t := toks[rng.Intn(len(toks))]
		return src[:t.Span.Start] + src[t.Span.End:]
	case 1: // duplicate a token
		t := toks[rng.Intn(len(toks))]
		return src[:t.Span.End] + " " + src[t.Span.Start:t.Span.End] + src[t.Span.End:]
	case 2: // swap two adjacent tokens
		if len(toks) < 2 {
			return src
		}
		i := rng.Intn(len(toks) - 1)
		a, b := toks[i], toks[i+1]
		return src[:a.Span.Start] + src[b.Span.Start:b.Span.End] + src[a.Span.End:b.Span.Start] +
			src[a.Span.Start:a.Span.End] + src[b.Span.End:]
	case 3: // replace a token with one drawn from another seed
		t := toks[rng.Intn(len(toks))]
		other := seeds[rng.Intn(len(seeds))]
		otherToks := realTokens(other)
		if len(otherToks) == 0 {
			return src
		}
		o := otherToks[rng.Intn(len(otherToks))]
		return src[:t.Span.Start] + other[o.Span.Start:o.Span.End] + src[t.Span.End:]
	case 4: // truncate at a token boundary
		t := toks[rng.Intn(len(toks))]
		return src[:t.Span.Start]
	default: // splice a prefix of one seed onto a suffix of another
		other := seeds[rng.Intn(len(seeds))]
		otherToks := realTokens(other)
		if len(otherToks) == 0 {
			return src
		}
		t := toks[rng.Intn(len(toks))]
		o := otherToks[rng.Intn(len(otherToks))]
		return src[:t.Span.End] + " " + other[o.Span.Start:]
	}
}

// realTokens tokenizes src, drops the end-of-input sentinel, and clamps
// spans to the source (dollar-quoted string tokens record the rewritten
// literal's length, which can overrun the source extent).
func realTokens(src string) []token.Token {
	toks, err := lexer.Tokenize(src)
	if err != nil {
		return nil
	}
	for len(toks) > 0 && toks[len(toks)-1].Kind == token.EndOfInput {
		toks = toks[:len(toks)-1]
	}
	for i := range toks {
		if toks[i].Span.End > len(src) {
			toks[i].Span.End = len(src)
		}
	}
	return toks
}
