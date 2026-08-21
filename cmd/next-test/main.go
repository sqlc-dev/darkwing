// Command next-test picks the next todo case from the conformance corpus:
// the dev loop is next-test -> implement -> `go test ./parser
// -check-parse '<fragment>'` (see CLAUDE.md).
//
// Usage:
//
//	next-test [-testdata parser/testdata] [-all]
package main

import (
	"flag"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sqlc-dev/darkwing/internal/testfile"
)

func main() {
	log.SetFlags(0)
	testdata := flag.String("testdata", "parser/testdata", "testdata directory")
	all := flag.Bool("all", false, "list every todo case instead of just the first")
	flag.Parse()

	root := filepath.Join(*testdata, "corpus")
	var paths []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".test") {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		log.Fatalf("error: %v", err)
	}
	sort.Strings(paths)

	total := 0
	for _, path := range paths {
		meta, err := testfile.ReadMetadata(path)
		if err != nil {
			log.Fatalf("error: %v", err)
		}
		if len(meta.Todo) == 0 {
			continue
		}
		file, err := testfile.Read(path)
		if err != nil {
			log.Fatalf("error: %v", err)
		}
		byKey := make(map[string]*testfile.Case)
		for i := range file.Cases {
			byKey[file.Cases[i].Key()] = &file.Cases[i]
		}
		for _, key := range testfile.SortKeys(meta.Todo) {
			c, ok := byKey[key]
			if !ok {
				continue
			}
			total++
			want := "accept"
			if c.Reject {
				want = "reject: " + c.Error
			}
			fmt.Printf("%s\ncase: %s (%s)\nwant: %s\nsql:\n%s\n\n", path, key, meta.Todo[key], want, c.SQL)
			if !*all {
				return
			}
		}
	}
	if total == 0 {
		fmt.Println("no todo cases - the corpus gate is green")
		return
	}
	if *all {
		fmt.Printf("%d todo cases\n", total)
	}
	os.Exit(0)
}
