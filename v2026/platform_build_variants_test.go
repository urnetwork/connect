// Cross-compiles the packages each platform build imports. A native test run
// only type-checks the host's files, so a symbol that exists on unix but not
// on windows or js/wasm (syscall.MSG_PEEK) passes every native test while the
// windows client and the browser sdk no longer build.
package connect

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The connect packages the js/wasm sdk (sdk/js) and the native clients import.
var platformBuildVariantPackages = []string{".", "./protocol", "./emoji"}

type platformBuildVariant struct {
	goos   string
	goarch string
	// also compile the root test package, so js tests can run under node
	compileTests bool
}

var platformBuildVariants = []platformBuildVariant{
	{goos: "js", goarch: "wasm", compileTests: true},
	{goos: "windows", goarch: "amd64"},
	{goos: "darwin", goarch: "arm64"},
	{goos: "ios", goarch: "arm64"},
	{goos: "linux", goarch: "amd64"},
	{goos: "android", goarch: "arm64"},
}

// Runs the toolchain that built this test with only GOOS/GOARCH changed.
// cgo is off so every variant compiles without a cross C toolchain.
func runPlatformBuildVariant(t *testing.T, variant platformBuildVariant, args ...string) {
	t.Helper()
	goPath := filepath.Join(runtime.GOROOT(), "bin", "go")
	if _, err := os.Stat(goPath); err != nil {
		var lookErr error
		goPath, lookErr = exec.LookPath("go")
		if lookErr != nil {
			t.Skip("go toolchain not available")
		}
	}
	command := exec.CommandContext(t.Context(), goPath, args...)
	command.Env = append(
		os.Environ(),
		"GOOS="+variant.goos,
		"GOARCH="+variant.goarch,
		"CGO_ENABLED=0",
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Errorf(
			"GOOS=%s GOARCH=%s go %s: %s\n%s",
			variant.goos,
			variant.goarch,
			strings.Join(args, " "),
			err,
			output,
		)
	}
}

func TestPlatformBuildVariantsCompile(t *testing.T) {
	if testing.Short() {
		t.Skip("cross-compiles every platform variant")
	}
	if runtime.GOOS == "js" || runtime.GOOS == "wasip1" {
		t.Skip("no toolchain subprocess under wasm")
	}
	for _, variant := range platformBuildVariants {
		runPlatformBuildVariant(t, variant, append([]string{"build"}, platformBuildVariantPackages...)...)
		if variant.compileTests {
			testBinaryPath := filepath.Join(t.TempDir(), "connect.test")
			runPlatformBuildVariant(t, variant, "test", "-c", "-vet=off", "-o", testBinaryPath, ".")
		}
	}
}
