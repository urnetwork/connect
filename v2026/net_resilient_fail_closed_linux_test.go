//go:build linux

package connect

import (
	"os"
	"testing"
	"time"
)

// countOpenFds counts the process's open file descriptors via /proc/self/fd,
// which only exists on Linux.
func countOpenFds(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("read /proc/self/fd: %v", err)
	}
	return len(entries)
}

// TestResilientTlsConnFragmentDoesNotLeakFileDescriptors verifies the
// fragment+reorder path closes the dup'd socket fd on failure.
func TestResilientTlsConnFragmentDoesNotLeakFileDescriptors(t *testing.T) {
	record := buildClientHelloRecord(t)

	before := countOpenFds(t)
	for i := 0; i < 20; i++ {
		client, server := newTcpPair(t)
		client.SetWriteDeadline(time.Now().Add(-time.Second))
		rconn := NewResilientTlsConn(client, true, true)
		if _, err := rconn.Write(record); err == nil {
			t.Fatalf("iteration %d: expected write error, got nil", i)
		}
		client.Close()
		server.Close()
	}
	after := countOpenFds(t)
	if after > before+5 {
		t.Fatalf("file descriptors grew from %d to %d over 20 failed fragment writes", before, after)
	}
}

// TestResilientTlsConnReorderOnlyDoesNotLeakFileDescriptors verifies the
// reorder-only path closes the dup'd socket fd on failure.
func TestResilientTlsConnReorderOnlyDoesNotLeakFileDescriptors(t *testing.T) {
	record := buildClientHelloRecord(t)

	before := countOpenFds(t)
	for i := 0; i < 20; i++ {
		client, server := newTcpPair(t)
		client.SetWriteDeadline(time.Now().Add(-time.Second))
		rconn := NewResilientTlsConn(client, false, true)
		if _, err := rconn.Write(record); err == nil {
			t.Fatalf("iteration %d: expected write error, got nil", i)
		}
		client.Close()
		server.Close()
	}
	after := countOpenFds(t)
	if after > before+5 {
		t.Fatalf("file descriptors grew from %d to %d over 20 failed reorder writes", before, after)
	}
}
