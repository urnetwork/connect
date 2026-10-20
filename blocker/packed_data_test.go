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

// The mobile ABI checks type-check dependencies from source. Thousands of
// left-associated string additions make go/types repeatedly traverse all prior
// fragments to check string lengths. Assert the root cause directly, without a
// machine-dependent timing threshold, then type-check and compare every byte.
func TestEmitPackedDataUsesSingleStringLiterals(t *testing.T) {
	for _, count := range []int{0, 1 << 17} {
		name := "large"
		if count == 0 {
			name = "empty"
		}
		t.Run(name, func(t *testing.T) {
			pepper := make([]byte, 256)
			for i := range pepper {
				pepper[i] = byte(i)
			}
			keys := make([]uint64, count)
			wantHosts := make([]byte, 0, count*hostRecordLen)
			for i := range keys {
				keys[i] = uint64(i) * 0x010203
				var record [8]byte
				binary.BigEndian.PutUint64(record[:], keys[i])
				wantHosts = append(wantHosts, record[8-hostRecordLen:]...)
			}
			ranges := []iprange{{lo: 0x01020304, hi: 0xf1f2f3f4}}
			ranges6 := []ip6range{{
				lo: netip.MustParseAddr("2001:db8::1"),
				hi: netip.MustParseAddr("2001:db8::ffff"),
			}}
			want4 := "\x01\x02\x03\x04\xf1\xf2\xf3\xf4"
			want6 := "\x20\x01\x0d\xb8\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x01" +
				"\x20\x01\x0d\xb8\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\xff\xff"
			if count == 0 {
				pepper, ranges, ranges6 = nil, nil, nil
				want4, want6 = "", ""
			}
			source := emit("standard", string(pepper), keys, ranges, ranges6, nil)
			checkPackedStringConstants(t, source, map[string]string{
				"blockerPepper":             string(pepper),
				"blockerBlockedHostData":    string(wantHosts),
				"blockerBlockedPrefixData":  want4,
				"blockerBlockedPrefix6Data": want6,
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
