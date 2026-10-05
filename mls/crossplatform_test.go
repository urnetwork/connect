// The cross platform obligation of spec A section 1, as a gate rather than as a sentence.
//
// Spec A: "Everything in connect/mls, connect/message, and sdk MUST build everywhere gomobile
// goes." That was true on the day it was measured and nothing held it there. This gate holds it,
// and it is placed before the key schedule rather than after it because the cheapest moment to
// discover that a package no longer builds for ios is the commit that broke it, not the release
// that needed it.
//
// What actually breaks this, in order of how likely it is: a cgo dependency, which ends the pure
// go build everywhere at once; a syscall or an os call with no implementation on a platform; a
// build constrained file that quietly excludes the real implementation and leaves a stub; and an
// unsafe or assembly path with no portable fallback. None of those is visible from a test that
// runs only on the machine that wrote it, which is every other test in this package.
//
// CGO_ENABLED=0 is not incidental. It is the condition that makes one pure go tree serve android,
// ios, windows, linux, macos and wasm from a single build, and a dependency that needs cgo does
// not announce itself: it builds fine on the developer's machine, where cgo is available, and
// fails only in the cross compile nobody runs.
package mls

import (
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// The platforms the product ships apps on, plus the servers it runs on. This list is typed out
// rather than derived, and that is correct here where it is wrong nearly everywhere else on this
// project: which devices the product supports is a product decision, and there is no source of
// truth to derive it from. What IS derived is the check that every entry names a platform the go
// tool actually recognises, so a typo fails here rather than silently testing nothing.
//
// wasm is on the list deliberately, though no app ships on it today. It is the strictest target of
// the set — no syscalls, no cgo, no assembly — so it catches a portability defect earlier and more
// loudly than the platforms that actually matter, and it costs one build.
var productPlatforms = []string{
	"android/arm64", // gomobile aar
	"android/arm",   // gomobile aar, 32 bit
	"ios/arm64",     // gomobile xcframework
	"darwin/arm64",  // macos, apple silicon
	"darwin/amd64",  // macos, intel
	"windows/amd64",
	"linux/amd64",
	"linux/arm64",
	"js/wasm",
}

// The package trees the obligation covers, spelled the way the go tool takes a pattern rather
// than the way every other scope in this suite spells a directory. That spelling is why the
// split of connect/message went two rounds with this scope missed: a grep for "../message"
// does not find it. sdk is a separate module and is not reachable from here; spec A names it
// too, and its own repository owes the same gate.
//
// A package tree left off this list is not a failure. It is nine platforms silently no longer
// built for it -- the quietest of the scopes the split moved, and the only one with no
// observable consequence at all until a platform breaks. The count in the t.Logf below is
// the only thing that reports it.
var crossPlatformPackages = []string{"./mls/...", "./message/...", "./messagegroup/..."}

// Every product platform builds every covered package, with cgo off.
func TestTheCryptoBuildsForEveryPlatformTheProductShipsOn(t *testing.T) {
	known := distList(t)
	for _, platform := range productPlatforms {
		if !slices.Contains(known, platform) {
			t.Fatalf("%s is not a platform this go tool recognises, so building for it proves nothing; go tool dist list named %d platforms",
				platform, len(known))
		}
	}

	for _, platform := range productPlatforms {
		goos, goarch, _ := strings.Cut(platform, "/")
		for _, packages := range crossPlatformPackages {
			t.Run(platform+" "+packages, func(t *testing.T) {
				if out, err := buildFor(goos, goarch, packages); err != nil {
					t.Fatalf("%s does not build for %s with cgo off:\n%s", packages, platform, out)
				}
			})
		}
	}
	t.Logf("%d platforms x %d package trees built with CGO_ENABLED=0, from %s",
		len(productPlatforms), len(crossPlatformPackages), runtime.GOOS+"/"+runtime.GOARCH)
}

// The control. A gate that shells out to a compiler and reports what it thinks the compiler said
// is a gate that can report success having run nothing — a wrong path, a swallowed error, an exec
// that never started. Building for a platform the go tool does not have must FAIL here, which is
// only true if buildFor really invokes the toolchain and really reads its result.
func TestTheCrossPlatformHarnessActuallyInvokesTheCompiler(t *testing.T) {
	if out, err := buildFor("nosuchos", "nosucharch", "./mls/..."); err == nil {
		t.Fatalf("building for nosuchos/nosucharch succeeded, so this harness is not running the compiler and every platform above passed vacuously:\n%s", out)
	}
	// and the positive half: the host builds, so a failure above is the platform and not the harness
	if out, err := buildFor(runtime.GOOS, runtime.GOARCH, "./mls/..."); err != nil {
		t.Fatalf("the host platform does not build, so nothing above can be attributed to a cross compile:\n%s", out)
	}
}

func buildFor(goos string, goarch string, packages string) (string, error) {
	command := exec.Command("go", "build", packages)
	command.Dir = ".."
	command.Env = append(command.Environ(), "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=0")
	out, err := command.CombinedOutput()
	return string(out), err
}

// The platforms this go tool knows, read from the tool rather than listed, so productPlatforms is
// checked against the toolchain that will do the building.
func distList(t *testing.T) []string {
	t.Helper()
	out, err := exec.Command("go", "tool", "dist", "list").CombinedOutput()
	if err != nil {
		t.Fatalf("go tool dist list: %v\n%s", err, out)
	}
	platforms := strings.Fields(string(out))
	if len(platforms) == 0 {
		t.Fatal("go tool dist list named no platform, so the check below would hold vacuously")
	}
	return platforms
}
