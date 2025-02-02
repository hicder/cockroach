// Copyright 2023 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package main

import (
	"flag"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/cockroachdb/cockroach/pkg/build/bazel"
)

var outFile = flag.String("out-file", "",
	"file to write the generated test into")

func main() {
	flag.Parse()

	var args struct {
		Files []string
	}

	logicTestDir, err := bazel.Runfile("pkg/sql/logictest/testdata/logic_test")
	if err != nil {
		panic(err)
	}

	if err := filepath.WalkDir(logicTestDir, func(filePath string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasPrefix(path.Base(filePath), "_") {
			return err
		}
		splitPath := strings.Split(filePath, "pkg/")
		relPathIncludingPkg := "pkg/" + splitPath[len(splitPath)-1]
		args.Files = append(args.Files, relPathIncludingPkg)
		return nil
	}); err != nil {
		panic(err)
	}

	f, err := os.Create(*outFile)
	if err != nil {
		panic(err)
	}
	defer func() { _ = f.Close() }()

	if err := templ.Execute(f, args); err != nil {
		panic(err)
	}
}

func basename(in string) string {
	out := filepath.Base(in)
	out = strings.ReplaceAll(out, ".", "_")
	out = strings.ReplaceAll(out, "-", "_")
	return out
}

var templ = template.Must(template.New("t").Funcs(template.FuncMap{
	"basename": basename,
}).Parse(`// Copyright 2023 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

// Code generated by sccomparatortestgen, DO NOT EDIT.

package schemachanger_test

import (
	"testing"

	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
)

{{ range $index, $file := $.Files -}}

func TestSchemaChangeComparator_{{ basename $file }}(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	var logicTestFile = "{{ $file }}"
	runSchemaChangeComparatorTest(t, logicTestFile)
}
{{ end -}}
`))
