// Package duckdbsrc locates and drives the pinned DuckDB CLI binary — the
// conformance oracle. The pin is a source commit plus the nightly CLI build
// of that commit; grammar, corpus expectations, and oracle must all come
// from the same commit (see internal/grammar/README.md).
package duckdbsrc

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Pinned build identity. Advancing the pin means updating these alongside
// the vendored grammar and regenerated corpus.
const (
	// PinnedCommit is the DuckDB source commit of the pin.
	PinnedCommit = "8cbdaba6acb50db7780d22a65dd131584d472262"
	// PinnedVersion is the CLI version string of the matching nightly.
	PinnedVersion = "v2.0.0-alpha38195"
	// NightlyURL is where DuckDB serves the current nightly CLI bundle.
	// "latest" moves: once it no longer matches PinnedCommit, the pin
	// must be advanced (or a cached binary used).
	NightlyURL = "https://artifacts.duckdb.org/latest/duckdb-binaries-linux-amd64.zip"
)

// EnvBinary is the environment variable naming the pinned CLI binary.
const EnvBinary = "DARKWING_DUCKDB"

// Find locates the pinned DuckDB CLI: $DARKWING_DUCKDB, or "duckdb" on
// PATH. The binary's version is verified against the pin.
func Find() (string, error) {
	path := os.Getenv(EnvBinary)
	if path == "" {
		p, err := exec.LookPath("duckdb")
		if err != nil {
			return "", fmt.Errorf("pinned DuckDB CLI not found: set %s or put duckdb on PATH (want %s @ %.10s, from %s)",
				EnvBinary, PinnedVersion, PinnedCommit, NightlyURL)
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
	out, err := exec.Command(path, "--version").Output()
	if err != nil {
		return fmt.Errorf("running %s --version: %w", path, err)
	}
	version := strings.TrimSpace(string(out))
	if !strings.Contains(version, PinnedVersion) || !strings.Contains(version, PinnedCommit[:10]) {
		return fmt.Errorf("binary %s reports %q, want %s @ %.10s: advance the pin or point %s at the matching nightly",
			path, version, PinnedVersion, PinnedCommit, EnvBinary)
	}
	return nil
}

// Verdict is the oracle's classification of one statement.
type Verdict struct {
	// Reject is true when the CLI raised a Parser Error. Success and any
	// post-parse error (Binder, Catalog, ...) are must-accept, as are
	// timeouts and crashes (execution ran, so parsing succeeded).
	Reject bool
	// Error is the first "Parser Error" line for rejected statements.
	Error string
	// TimedOut records that execution was cut short (still must-accept).
	TimedOut bool
	// Crashed records a CLI crash after parsing (still must-accept).
	Crashed bool
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
	if err != nil {
		var exitErr *exec.ExitError
		if isExit(err, &exitErr) {
			if exitErr.ExitCode() == -1 || exitErr.ExitCode() > 1 {
				// killed by a signal or an abnormal exit: the CLI got past
				// parsing and died executing (fuzzer statements do this)
				return Verdict{Crashed: true}, nil
			}
			// plain error exit with a non-parser error on stderr:
			// post-parse failure, must-accept
			return Verdict{}, nil
		}
		return Verdict{}, fmt.Errorf("running oracle: %w", err)
	}
	return Verdict{}, nil
}

func isExit(err error, target **exec.ExitError) bool {
	if e, ok := err.(*exec.ExitError); ok {
		*target = e
		return true
	}
	return false
}

// classify scans CLI stderr for the first error line. Only "Parser Error"
// means reject; everything else is post-parse.
func classify(stderr string) (Verdict, bool) {
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Parser Error") {
			return Verdict{Reject: true, Error: line}, true
		}
	}
	return Verdict{}, false
}
