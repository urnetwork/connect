//go:build linux

// Native device fields may be narrower than the stable public identity.
package durablesys

import (
	"math"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Every native device bit survives without sign extension or 64-bit truncation.
func TestStatDevicePreservesNativeWidth(t *testing.T) {
	var stat unix.Stat_t
	stat.Dev = ^stat.Dev
	want := uint64(math.MaxUint64)
	if unsafe.Sizeof(stat.Dev) == 4 {
		want = math.MaxUint32
	}
	if got := StatDevice(&stat); got != want {
		t.Fatalf("device identity %#x, want %#x", got, want)
	}
}

// FileInfo uses syscall.Stat_t, independently of the x/sys descriptor layout.
func TestFileInfoDevicePreservesNativeWidth(t *testing.T) {
	info, err := openTestFile(t).Stat()
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("unexpected Linux stat type %T", info.Sys())
	}
	// Mutate only the returned metadata snapshot to exercise every native bit.
	stat.Dev = 0
	stat.Dev = ^stat.Dev
	want := uint64(math.MaxUint64)
	if unsafe.Sizeof(stat.Dev) == 4 {
		want = math.MaxUint32
	}
	if got, ok := FileInfoDevice(info); !ok || got != want {
		t.Fatalf("file info device identity %#x, %v; want %#x, true", got, ok, want)
	}
}
