// Package duckdbsrc locates and drives the pinned DuckDB CLI binary — the
// conformance oracle. The pin is a tagged source commit plus the release
// CLI built from it; grammar, corpus expectations, and oracle must all come
// from the same commit (see internal/grammar/README.md).
package duckdbsrc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Pinned build identity. Advancing the pin means updating these alongside
// the vendored grammar and regenerated corpus.
const (
	// PinnedCommit is the DuckDB source commit of the pin (the v2.0.0 tag).
	PinnedCommit = "d54cbbe3df17e8e5ce68312f483b4155836112aa"
	// PinnedVersion is the CLI version string of the pinned release.
	PinnedVersion = "v2.0.0"
	// ReleaseURL is where DuckDB serves the pinned release's CLI. Unlike
	// the nightly bundle a tagged release never moves.
	ReleaseURL = "https://github.com/duckdb/duckdb/releases/download/v2.0.0/duckdb_cli-linux-amd64.zip"
)

// EnvBinary is the environment variable naming the pinned CLI binary.
const EnvBinary = "DARKWING_DUCKDB"

// Env returns the environment for running the CLI: the current one with
// agent mode forced off. Since v2.0.0 the CLI switches to a JSON/markdown
// "agent mode" output format when its stdout is a pipe and it detects a
// coding agent in the environment (AI_AGENT, CLAUDECODE, ...), which would
// change the error lines the oracle classifies; DUCKDB_AGENT_MODE=0
// overrides the detection (ShellState::DetectAgentMode in shell.cpp).
func Env() []string {
	return append(os.Environ(), "DUCKDB_AGENT_MODE=0")
}

// Find locates the pinned DuckDB CLI: $DARKWING_DUCKDB, or "duckdb" on
// PATH. The binary's version is verified against the pin.
func Find() (string, error) {
	path := os.Getenv(EnvBinary)
	if path == "" {
		p, err := exec.LookPath("duckdb")
		if err != nil {
			return "", fmt.Errorf("pinned DuckDB CLI not found: set %s or put duckdb on PATH (want %s @ %.10s, from %s)",
				EnvBinary, PinnedVersion, PinnedCommit, ReleaseURL)
		}
		path = p
	}
	if err := Verify(path); err != nil {
		return "", err
	}
	return path, nil
}

// Verify checks that the binary at path reports the pinned version and
// commit.
func Verify(path string) error {
	cmd := exec.Command(path, "--version")
	cmd.Env = Env()
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("running %s --version: %w", path, err)
	}
	version := strings.TrimSpace(string(out))
	if !strings.Contains(version, PinnedVersion) || !strings.Contains(version, PinnedCommit[:10]) {
		return fmt.Errorf("binary %s reports %q, want %s @ %.10s: advance the pin or point %s at the matching release CLI",
			path, version, PinnedVersion, PinnedCommit, EnvBinary)
	}
	return nil
}

// Verdict is the oracle's classification of one statement.
type Verdict struct {
	// Reject is true when the CLI raised a Parser Error. Success and any
	// post-parse error (Binder, Catalog, ...) are must-accept.
	Reject bool
	// Error is the first "Parser Error" line for rejected statements.
	Error string
	// TimedOut records that the CLI was cut off before printing a
	// verdict. No verdict was observed - the hang could even be inside
	// the CLI's parser - so callers must treat the statement as
	// unverified, not as must-accept.
	TimedOut bool
	// Crashed records that the CLI died before printing a verdict; like
	// TimedOut, the statement is unverified.
	Crashed bool
	// PostParseError records that the CLI reported a non-parser error
	// (Binder Error, Catalog Error, ...). Still must-accept — but the
	// batch-mode CLI stops at the first failing statement, so for
	// multi-statement input everything after that statement went
	// unparsed and the accept verdict does not cover it.
	PostParseError bool
}

// Oracle runs statements through the pinned CLI against :memory:.
type Oracle struct {
	Binary string
	// Timeout bounds one statement's execution (parsing is instant; the
	// bound is for statements that execute expensive work). Default 5s.
	Timeout time.Duration
	// Dir is the working directory for the CLI (statements may create
	// files); default os.TempDir().
	Dir string
}

// preamble disables extension auto-loading and bounds resource use so the
// oracle's verdicts depend only on parsing and cheap post-parse checks.
const preamble = "SET autoinstall_known_extensions=false;\n" +
	"SET autoload_known_extensions=false;\n" +
	"SET memory_limit='512MB';\n" +
	"SET threads=1;\n"

// Run classifies one statement.
func (o *Oracle) Run(sql string) (Verdict, error) {
	timeout := o.Timeout
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, o.Binary, "-batch", "-noheader", ":memory:")
	cmd.Env = Env()
	cmd.Dir = o.Dir
	if cmd.Dir == "" {
		cmd.Dir = os.TempDir()
	}
	cmd.Stdin = strings.NewReader(preamble + sql + "\n")
	var stderr bytes.Buffer
	cmd.Stdout = nil
	cmd.Stderr = &stderr
	err := cmd.Run()

	if v, found := classify(stderr.String()); found {
		return v, nil
	}
	if ctx.Err() == context.DeadlineExceeded {
		// execution ran long: parsing succeeded
		return Verdict{TimedOut: true}, nil
	}
	postParse := hasPostParseError(stderr.String())
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if exitErr.ExitCode() == -1 || exitErr.ExitCode() > 1 {
				// killed by a signal or an abnormal exit with no verdict
				// printed; possibly inside the CLI's own parser, so the
				// caller must not guess accept or reject from this
				return Verdict{Crashed: true}, nil
			}
			// plain error exit with a non-parser error on stderr:
			// post-parse failure, must-accept
			return Verdict{PostParseError: postParse}, nil
		}
		return Verdict{}, fmt.Errorf("running oracle: %w", err)
	}
	return Verdict{PostParseError: postParse}, nil
}

// errorLine matches the CLI's non-parser error headers ("Binder Error:",
// "Catalog Error: ", "Invalid Input Error: ", ...).
var errorLine = regexp.MustCompile(`^[A-Za-z][A-Za-z ]*Error: `)

// hasPostParseError reports whether stderr carries a non-parser error
// line.
func hasPostParseError(stderr string) bool {
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Parser Error") {
			continue
		}
		if errorLine.MatchString(line) {
			return true
		}
	}
	return false
}

// classify scans CLI stderr for the first error line. Only "Parser Error"
// means reject; everything else is post-parse.
//
// One known imprecision is inherent to this interface: a bind-time nested
// parse (query('...'), query_table, json_serialize_sql parsing its string
// argument) also raises "Parser Error" even though the outer statement
// parsed fine, so such statements are recorded must-reject. The grammar
// never sees the inner SQL, so those corpus cases are excluded via skip
// entries in the metadata sidecars - triage stays manual because a
// syntax-error-shaped disagreement is otherwise indistinguishable from a
// genuine engine bug.
func classify(stderr string) (Verdict, bool) {
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Parser Error") {
			return Verdict{Reject: true, Error: line}, true
		}
	}
	return Verdict{}, false
}
