package main

import (
	"os"
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
