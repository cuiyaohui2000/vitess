/*
Copyright 2021 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"vitess.io/vitess/go/mysql/collations/tools/makecolldata/codegen"
)

type versionInfo struct {
	id        uint
	alias     map[string]byte
	isdefault byte
}

type alias struct {
	mask byte
	name string
}

func makeversions(output string) {
	flag.Parse()

	versionfiles, err := filepath.Glob("testdata/versions/collations_*.csv")
	if err != nil {
		log.Fatal(err)
	}
	sort.Strings(versionfiles)

	versioninfo := make(map[uint]*versionInfo)
	for v, versionCsv := range versionfiles {
		f, err := os.Open(versionCsv)
		if err != nil {
			log.Fatal(err)
		}

		scan := bufio.NewScanner(f)
		var row int
		for scan.Scan() {
			if row == 0 {
				row++
				continue
			}

			cols := strings.Split(scan.Text(), "\t")
			collid, err := strconv.ParseUint(cols[2], 10, 16)
			if err != nil {
				log.Fatal(err)
			}

			vi := versioninfo[uint(collid)]
			if vi == nil {
				vi = &versionInfo{id: uint(collid), alias: make(map[string]byte)}
				versioninfo[uint(collid)] = vi
			}

			vi.alias[cols[0]] |= 1 << v
			switch cols[3] {
			case "Yes":
				vi.isdefault |= 1 << v
			case "No", "":
			default:
				log.Fatalf("unknown value for IS_DEFAULT: %q", cols[3])
			}

			row++
		}
	}

	var versions []string
	for _, versionCsv := range versionfiles {
		base := filepath.Base(versionCsv)
		base = strings.TrimPrefix(base, "collations_")
		base = strings.TrimSuffix(base, ".csv")
		versions = append(versions, base)
	}

	var g = codegen.NewGenerator("vitess.io/vitess/go/mysql/collations")
	g.P("type collver byte")
	g.P("const (")
	g.P("collverInvalid collver = 0")
	for n, version := range versions {
		g.P("collver", version, " collver = 1 << ", n)
	}
	g.P(")")
	g.P()
	g.P("func (v collver) String() string {")
	g.P("switch v {")
	g.P("case collverInvalid: return \"Invalid\"")
	for _, version := range versions {
		g.P("case collver", version, ": return ", codegen.Quote(version))
	}
	g.P("default: panic(\"invalid version identifier\")")
	g.P("}")
	g.P("}")
	g.P()
	g.P("var globalVersionInfo = map[ID]struct{alias map[collver]string; isdefault collver}{")

	var sorted []*versionInfo
	for _, vi := range versioninfo {
		sorted = append(sorted, vi)
	}
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].id < sorted[j].id
	})
	for _, vi := range sorted {
		var reverse []alias
		for a, m := range vi.alias {
			reverse = append(reverse, alias{m, a})
		}
		sort.Slice(reverse, func(i, j int) bool {
			return reverse[i].name < reverse[j].name
		})
		fmt.Fprintf(g, "%d: {alias: map[collver]string{", vi.id)
		for _, a := range reverse {
			fmt.Fprintf(g, "0b%08b: %q,", a.mask, a.name)
		}
		fmt.Fprintf(g, "}, isdefault: 0b%08b},\n", vi.isdefault)
	}
	g.P("}")

	g.WriteToFile(path.Join(output, "mysqlversion.go"))
}
