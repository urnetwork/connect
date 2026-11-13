package extender

// Regression tests for the private-extender admission check. The expected
// signature is an hmac under the extender secret and the presented one is
// client-supplied, so the two must be compared in constant time.

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/urnetwork/connect/v2026/protocol"
)

// Constant time is not observable from a behavioural test, so this reads the
// method's own source: the signature comparison must be hmac.Equal, and no
// early-exit equality may appear in the method.
func TestAllowedSecretComparesInConstantTime(t *testing.T) {
	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, "extender.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var method *ast.FuncDecl
	for _, decl := range file.Decls {
		if funcDecl, ok := decl.(*ast.FuncDecl); ok && funcDecl.Recv != nil && funcDecl.Name.Name == "IsAllowedSecret" {
			method = funcDecl
		}
	}
	if method == nil {
		t.Fatal("IsAllowedSecret not found in extender.go")
	}
	calls := map[string]bool{}
	ast.Inspect(method.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if packageIdent, ok := selector.X.(*ast.Ident); ok {
			calls[packageIdent.Name+"."+selector.Sel.Name] = true
		}
		return true
	})
	if !calls["hmac.Equal"] {
		t.Error("the admission signature is not compared with hmac.Equal")
	}
	for _, earlyExit := range []string{"slices.Equal", "bytes.Equal", "reflect.DeepEqual"} {
		if calls[earlyExit] {
			t.Errorf("the admission signature is compared with %s, which stops at the first differing byte", earlyExit)
		}
	}
}

// Only the whole hmac admits a header: a prefix, an extension, an empty
// signature or one flipped bit is refused.
func TestAllowedSecretRefusesPartialSignatures(t *testing.T) {
	server := &ExtenderServer{allowedSecrets: []string{testSecret}}
	header := &protocol.ExtenderHeader{
		Timestamp: 1,
		Nonce:     []byte("synthetic-nonce-0001"),
	}
	mac := hmac.New(sha256.New, []byte(testSecret))
	timestampBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(timestampBytes, header.Timestamp)
	mac.Write(timestampBytes)
	mac.Write(header.Nonce)
	signature := mac.Sum(nil)

	header.Signature = signature
	if !server.IsAllowedSecret(header) {
		t.Fatal("the whole signature was refused")
	}

	flipped := bytes.Clone(signature)
	flipped[len(flipped)-1] ^= 0x01
	partialSignatures := [][]byte{
		signature[:len(signature)-1],
		append(bytes.Clone(signature), 0),
		nil,
		flipped,
	}
	for _, partialSignature := range partialSignatures {
		header.Signature = partialSignature
		if server.IsAllowedSecret(header) {
			t.Errorf("a %d-byte partial signature was admitted", len(partialSignature))
		}
	}
}
