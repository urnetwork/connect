//go:build linux

// Linux-only kernel facts: the mountinfo parser and the ext4 test fixture.
package durablevolume

import (
	"strings"
	"testing"
)

// Fixture custody claims the platform's qualified filesystem type.
const testFilesystemType = "ext4"

// Kernel path escapes are decoded once, preserving exact namespace identity.
func TestMountParserPreservesEscapedCoordinates(t *testing.T) {
	raw := []byte("7 1 8:2 / /synthetic\\040volume rw,nosuid shared:9 - ext4 /dev/synthetic rw\n")
	mounts, err := parseMounts(raw)
	if err != nil || len(mounts) != 1 || mounts[0].Path != "/synthetic volume" || mounts[0].ReadOnly {
		t.Fatalf("parsed: %+v %v", mounts, err)
	}
	if _, err := parseMounts([]byte(strings.ReplaceAll(string(raw), `\040`, `\999`))); err == nil {
		t.Fatal("unknown escape admitted")
	}
}
