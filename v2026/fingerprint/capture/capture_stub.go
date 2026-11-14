//go:build !fingerprint_capture

// This stub builds in the default configuration so `go build ./...` and
// `go vet ./...` stay green over the connect module; the real Layer-B capture
// tool is behind the fingerprint_capture build tag (capture.go). Build it with
//
//	go run -tags fingerprint_capture ./fingerprint/capture [flags]
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "build this tool with -tags fingerprint_capture; see fingerprint/README.md")
	os.Exit(2)
}
