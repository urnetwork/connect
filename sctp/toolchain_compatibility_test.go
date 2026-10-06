// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

// Checks Go's selection of version-specific tests against their required api.
package sctp

import (
	_ "embed"
	"fmt"
	"go/build"
	"io"
	"strings"
	"testing"
)

// The compiler supplies the selected source, including private Go overlays.
//
//go:embed association_service_window_test.go
var serviceWindowTestSource string

// Native synctest coverage starts at Go 1.25 and stays enabled on newer Go.
func TestServiceWindowToolchainCompatibility(t *testing.T) {
	cases := []struct {
		minor    int
		goos     string
		goarch   string
		selected bool
	}{
		{minor: 24, goos: "linux", goarch: "amd64"},
		{minor: 25, goos: "linux", goarch: "amd64", selected: true},
		{minor: 26, goos: "darwin", goarch: "arm64", selected: true},
		{minor: 24, goos: "js", goarch: "wasm"},
		{minor: 25, goos: "js", goarch: "wasm"},
		{minor: 26, goos: "js", goarch: "wasm"},
	}
	for _, c := range cases {
		buildContext := build.Default
		buildContext.GOOS = c.goos
		buildContext.GOARCH = c.goarch
		buildContext.ReleaseTags = nil
		for minor := 1; minor <= c.minor; minor++ {
			buildContext.ReleaseTags = append(buildContext.ReleaseTags, fmt.Sprintf("go1.%d", minor))
		}
		buildContext.OpenFile = func(string) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader(serviceWindowTestSource)), nil
		}
		selected, err := buildContext.MatchFile(".", "association_service_window_test.go")
		if err != nil || selected != c.selected {
			t.Errorf("go1.%d %s/%s: service-window selection=(%v, %v), want %v", c.minor, c.goos, c.goarch, selected, err, c.selected)
		}
	}
}
