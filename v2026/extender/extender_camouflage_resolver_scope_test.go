// Resolver lifetime regressions must start before any other test initializes
// the process-wide DNS configuration, and contain a fatal in a child process.
package extender

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"
)

// Run the actual shutdown fixture first in a fresh copy of this test binary,
// then enter the ordinary resolver path outside its bubble. A custom Dial
// alone did not prevent the old fixture from poisoning the global semaphore.
func TestExtenderCamouflageResolverDoesNotEscapeBubble(t *testing.T) {
	const childKey = "CONNECT_TEST_CAMOUFLAGE_RESOLVER_CHILD"
	const completed = "camouflage resolver bubble and outside lookup joined"
	if os.Getenv(childKey) == "1" {
		TestExtenderCamouflageShutdownJoinsResolverAndRefusesFallback(t)
		if t.Failed() {
			return
		}
		// Cancellation plus a refusing, per-instance Dial guarantees no socket
		// or DNS query leaves the child. On Go's Unix resolver the system
		// config semaphore is acquired before cancellation is returned.
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		resolver := &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				return nil, errors.New("resolver regression forbids network access")
			},
		}
		if _, err := resolver.LookupTXT(ctx, "after-bubble.example."); err == nil {
			t.Fatal("the canceled synthetic lookup unexpectedly succeeded")
		}
		fmt.Fprintln(os.Stdout, completed)
		return
	}

	executable, err := os.Executable()
	if err != nil {
		t.Fatal("could not locate this test executable")
	}
	// These are deadlock watchdogs only; neither decides a lifecycle result.
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable,
		"-test.run=^TestExtenderCamouflageResolverDoesNotEscapeBubble$",
		"-test.timeout=25s",
	)
	command.Env = append(os.Environ(), childKey+"=1")
	output, err := command.CombinedOutput()
	if err != nil {
		if bytes.Contains(output, []byte("fatal error: send on synctest channel from outside bubble")) {
			t.Fatal("the shutdown fixture left Go's DNS semaphore owned by its ended bubble")
		}
		// Child initialization and network logs are not safe test diagnostics.
		t.Fatalf("fresh resolver process did not complete: %v (child output withheld)", err)
	}
	if !bytes.Contains(output, []byte(completed+"\n")) {
		t.Fatal("fresh process did not confirm both sides of the resolver boundary")
	}
}
