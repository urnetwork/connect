package main

import (
	"encoding/binary"
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"go/types"
	"net/netip"
	"os"
	"testing"
)

// Assert the expression shape that prevents the source-based mobile ABI check
// from repeatedly traversing a growing string-concatenation tree. Type-checking
// and byte comparisons also preserve the tables' constant, read-only semantics.
func TestEmitPackedDataUsesSingleStringLiterals(t *testing.T) {
	for _, count := range []int{0, 1 << 14} {
		name := "large"
		if count == 0 {
			name = "empty"
		}
		t.Run(name, func(t *testing.T) {
			ranges := make([]iprange, count)
			ranges6 := make([]ip6range, count)
			want4 := make([]byte, 0, count*8)
			want6 := make([]byte, 0, count*32)
			for i := range ranges {
				ranges[i] = iprange{lo: uint32(i) * 256, hi: uint32(i)*256 + 255}
				want4 = binary.BigEndian.AppendUint32(want4, ranges[i].lo)
				want4 = binary.BigEndian.AppendUint32(want4, ranges[i].hi)
				var lo, hi [16]byte
				for j := range lo {
					lo[j] = byte(i + j)
					hi[j] = byte(i + j + 1)
				}
				ranges6[i] = ip6range{lo: netip.AddrFrom16(lo), hi: netip.AddrFrom16(hi)}
				want6 = append(want6, lo[:]...)
				want6 = append(want6, hi[:]...)
			}
			checkPackedStringConstants(t, emit(ranges, ranges6, "", nil), map[string]string{
				"cfaaBlockedPrefixData":  string(want4),
				"cfaaBlockedPrefix6Data": string(want6),
			})
		})
	}
}

func TestCheckedInPackedDataUsesSingleStringLiterals(t *testing.T) {
	source, err := os.ReadFile("../" + outputFile)
	if err != nil {
		t.Fatal(err)
	}
	checkPackedStringConstants(t, source, nil)
}

func checkPackedStringConstants(t *testing.T, source []byte, want map[string]string) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, outputFile, source, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, declaration := range file.Decls {
		gen, ok := declaration.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			value := spec.(*ast.ValueSpec)
			for index, expression := range value.Values {
				literal, ok := expression.(*ast.BasicLit)
				if !ok {
					t.Errorf("%s is %T; packed constants must use one literal, avoiding recursive string-length checks", value.Names[index], expression)
					continue
				}
				if literal.Kind != token.STRING && literal.Kind != token.INT {
					t.Fatalf("unexpected constant kind %s", literal.Kind)
				}
			}
		}
	}
	if t.Failed() {
		return
	}
	config := types.Config{}
	pkg, err := config.Check("fixture", fset, []*ast.File{file}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, expected := range want {
		object, ok := pkg.Scope().Lookup(name).(*types.Const)
		if !ok || object.Val().Kind() != constant.String {
			t.Fatalf("%s is not a string constant", name)
		}
		if actual := constant.StringVal(object.Val()); actual != expected {
			t.Errorf("%s bytes differ (got %d bytes, want %d)", name, len(actual), len(expected))
		}
	}
}
