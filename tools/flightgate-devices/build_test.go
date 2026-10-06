package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestResolveAndroidNDKUsesExplicitThenEnvironmentThenPinnedVersion(t *testing.T) {
	dir := t.TempDir()
	gradle := filepath.Join(dir, "build.gradle")
	if err := os.WriteFile(gradle, []byte("android { ndkVersion = '29.0.14206865' }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	explicit, env, pinned := filepath.Join(dir, "explicit"), filepath.Join(dir, "environment"), filepath.Join(dir, "ndk", "29.0.14206865")
	for _, path := range []string{explicit, env, pinned} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "source.properties"), []byte("Pkg.Revision=29.0.14206865\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct{ name, explicit, env, want string }{
		{"explicit", explicit, env, explicit}, {"environment", "", env, env}, {"pinned", "", "", pinned},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveAndroidNDK(tc.explicit, tc.env, dir, gradle)
			if err != nil || got != tc.want {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
	if _, err := resolveAndroidNDK(filepath.Join(dir, "missing"), env, dir, gradle); err == nil {
		t.Fatal("silently replaced invalid explicit NDK with another version")
	}
}

func TestArtifactCopyDoesNotOverwriteAndPreservesHash(t *testing.T) {
	dir := t.TempDir()
	source, target := filepath.Join(dir, "source"), filepath.Join(dir, "target")
	if err := os.WriteFile(source, []byte("artifact contents\x00\x01\xff"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := copyArtifact(source, target); err != nil {
		t.Fatal(err)
	}
	srcHash, err := fileSHA256(source)
	if err != nil {
		t.Fatal(err)
	}
	dstHash, err := fileSHA256(target)
	if err != nil || dstHash != srcHash || len(dstHash) != 64 {
		t.Fatalf("copy hash mismatch: %q/%q %v", srcHash, dstHash, err)
	}
	if err := copyArtifact(source, target); err == nil {
		t.Fatal("overwrote existing acceptance artifact")
	}
}

func TestLoadBuildIsIndependentOfWorkingDirectory(t *testing.T) {
	t.Chdir(t.TempDir())
	path := filepath.Join(t.TempDir(), "flightgate-load")
	cmd := loadBuildCommand(path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("helper build from unrelated cwd: %v: %s", err, out)
	}
	if info, err := os.Stat(path); err != nil || info.Size() == 0 {
		t.Fatalf("helper artifact missing: %v", err)
	}
}

func TestLoadCompletionRequiresOneValidSummary(t *testing.T) {
	for _, tc := range []struct {
		name, log     string
		bytes, errors int64
		wantErr       bool
	}{
		{"valid", "1 bytes_per_second=123 errors=0\ndone total_bytes=123 errors=0\n", 123, 0, false},
		{"failed requests retained", "done total_bytes=123 errors=2\n", 123, 2, false},
		{"unfinished", "1 bytes_per_second=123 errors=0\n", 0, 0, true},
		{"missing error count", "done total_bytes=123\n", 0, 0, true},
		{"duplicate", "done total_bytes=123 errors=0\ndone total_bytes=123 errors=0\n", 0, 0, true},
		{"negative", "done total_bytes=-1 errors=0\n", 0, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "load.log")
			if err := os.WriteFile(path, []byte(tc.log), 0o600); err != nil {
				t.Fatal(err)
			}
			bytes, count, err := loadLogSummary(path)
			if (err != nil) != tc.wantErr || (!tc.wantErr && (bytes != tc.bytes || count != tc.errors)) {
				t.Fatalf("summary %d, %d, %v", bytes, count, err)
			}
		})
	}
}

// The sdk build module's replace directives as of the sdk's local gVisor fork
// (2026-10-02): every local path resolves beside the frozen sdk worktree.
const buildSiblingTestSdkBuildGoMod = `module github.com/urnetwork/sdk/build

go 1.26

replace github.com/urnetwork/sdk => ..

replace github.com/urnetwork/connect => ../../connect

replace github.com/pion/sctp => ../../connect/sctp

replace github.com/urnetwork/glog => ../../glog

replace github.com/urnetwork/goidenticons => ../../goidenticons

replace gvisor.dev/gvisor => ../../gvisor
`

// Creates a shared checkout tree as it stands after operator-proxy's
// retirement, and an empty build root holding the detached connect and sdk
// worktrees that build-item adds before it links the siblings.
func newBuildSiblingTestDirs(t *testing.T) (string, string) {
	tree, root := t.TempDir(), t.TempDir()
	for _, name := range []string{"android", "connect", "glog", "goidenticons", "gvisor", "proxy", "sdk", "server", "sn", "userwireguard", "warp"} {
		if err := os.MkdirAll(filepath.Join(tree, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{filepath.Join("connect", "sctp"), filepath.Join("sdk", "build")} {
		if err := os.MkdirAll(filepath.Join(root, path), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return tree, root
}

// Since the sdk pinned its local gVisor fork, the sdk build module replaces
// gvisor with ../../gvisor; without that link the frozen build fails at its
// first go command.
func TestLinkBuildSiblingsResolvesSdkBuildModuleReplacements(t *testing.T) {
	tree, root := newBuildSiblingTestDirs(t)
	sdkBuild := filepath.Join(root, "sdk", "build")
	if err := os.WriteFile(filepath.Join(sdkBuild, "go.mod"), []byte(buildSiblingTestSdkBuildGoMod), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := linkBuildSiblings(tree, root); err != nil {
		t.Fatal(err)
	}
	editCmd := exec.Command("go", "mod", "edit", "-json", "go.mod")
	editCmd.Dir = sdkBuild
	out, err := editCmd.Output()
	if err != nil {
		t.Fatalf("go mod edit -json: %v", err)
	}
	var goMod struct {
		Replace []struct {
			Old struct{ Path string }
			New struct{ Path string }
		}
	}
	if err := json.Unmarshal(out, &goMod); err != nil {
		t.Fatal(err)
	}
	if len(goMod.Replace) != 6 {
		t.Fatalf("parsed %d replace directives, want 6", len(goMod.Replace))
	}
	for _, replace := range goMod.Replace {
		if _, err := os.Stat(filepath.Join(sdkBuild, replace.New.Path)); err != nil {
			t.Errorf("%s => %s does not resolve in the build root: %v", replace.Old.Path, replace.New.Path, err)
		}
	}
}

// operator-proxy is retired and gone from the shared tree, so the build root
// must not link it: every link in the root resolves.
func TestLinkBuildSiblingsLeavesNoDanglingLink(t *testing.T) {
	tree, root := newBuildSiblingTestDirs(t)
	if err := linkBuildSiblings(tree, root); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if _, err := os.Stat(filepath.Join(root, entry.Name())); err != nil {
			t.Errorf("build root link %s does not resolve: %v", entry.Name(), err)
		}
	}
	if _, err := os.Lstat(filepath.Join(root, "operator-proxy")); !os.IsNotExist(err) {
		t.Errorf("build root links the retired operator-proxy checkout: %v", err)
	}
}
