package testfile

import (
	"path/filepath"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	file := &File{
		Header: []string{"source: test/sql/sample.test", "pin: abc"},
		Cases: []Case{
			{SQL: "SELECT 1;", Index: 0},
			{SQL: "SELECT\n  2", Index: 1},
			{SQL: "SELECT )", Reject: true, Error: `Parser Error: syntax error at or near ")"`, Index: 2},
		},
	}
	path := filepath.Join(t.TempDir(), "sample.test")
	if err := Write(path, file); err != nil {
		t.Fatal(err)
	}
	got, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Header) != 2 || got.Header[0] != "source: test/sql/sample.test" {
		t.Errorf("header = %q", got.Header)
	}
	if len(got.Cases) != len(file.Cases) {
		t.Fatalf("cases = %d, want %d", len(got.Cases), len(file.Cases))
	}
	for i := range file.Cases {
		a, b := file.Cases[i], got.Cases[i]
		if a.SQL != b.SQL || a.Reject != b.Reject || a.Error != b.Error {
			t.Errorf("case %d = %+v, want %+v", i, b, a)
		}
		if got.Cases[i].Index != i {
			t.Errorf("case %d index = %d", i, got.Cases[i].Index)
		}
	}
}

func TestCanStore(t *testing.T) {
	if CanStore("SELECT 1\n--\nSELECT 2") {
		t.Error("a lone -- line must not be storable")
	}
	if CanStore("==") {
		t.Error("a lone == line must not be storable")
	}
	if !CanStore("SELECT 1 -- comment\nFROM t") {
		t.Error("-- with trailing text is storable")
	}
}

func TestMetadataRoundTrip(t *testing.T) {
	corpus := filepath.Join(t.TempDir(), "sample.test")
	m, err := ReadMetadata(corpus)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Todo) != 0 || len(m.Skip) != 0 {
		t.Errorf("missing sidecar should read empty, got %+v", m)
	}
	m.Todo = map[string]string{CaseKey("SELECT 1"): "needs transformer"}
	if err := WriteMetadata(corpus, m); err != nil {
		t.Fatal(err)
	}
	got, err := ReadMetadata(corpus)
	if err != nil {
		t.Fatal(err)
	}
	if got.Todo[CaseKey("SELECT 1")] != "needs transformer" {
		t.Errorf("todo = %+v", got.Todo)
	}
	// clearing removes the sidecar file
	if err := WriteMetadata(corpus, &Metadata{}); err != nil {
		t.Fatal(err)
	}
	got, err = ReadMetadata(corpus)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Todo) != 0 {
		t.Errorf("cleared sidecar still has todos: %+v", got)
	}
}

func TestCaseKeyStable(t *testing.T) {
	if CaseKey("SELECT 1") != CaseKey("SELECT 1") {
		t.Error("CaseKey must be deterministic")
	}
	if CaseKey("SELECT 1") == CaseKey("SELECT 2") {
		t.Error("CaseKey must differ for different SQL")
	}
	if len(CaseKey("SELECT 1")) != 16 {
		t.Errorf("CaseKey length = %d, want 16", len(CaseKey("SELECT 1")))
	}
}
