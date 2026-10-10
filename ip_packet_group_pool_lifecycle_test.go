// Cold processes keep process-lifetime pool diagnostics outside fake-time bubbles.
package connect

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"regexp"
	"testing"
	"time"
)

// Both child streams use this same comparable writer. exec.Cmd serializes
// its writes, and the test reads only after Run has joined the child and copy.
type nativeBatchColdPoolOutput struct {
	data     [32 * 1024]byte
	count    int
	overflow bool
}

// Retains a fixed prefix and drains any excess without blocking child exit.
// Overflow is invalid evidence, not permission to inspect a truncated trace.
func (self *nativeBatchColdPoolOutput) Write(data []byte) (int, error) {
	count := copy(self.data[self.count:], data)
	self.count += count
	self.overflow = self.overflow || count != len(data)
	return len(data), nil
}

// A fresh copy of this exact compiled test binary has no prior pool use.
// The wall deadlines only bound unrelated failure and cannot satisfy the
// exact natural bubble-exit signature. No Go command or pool reset is used.
func testNativeBatchColdPoolLifecycle(t *testing.T, testName string) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal("cold pool fixture could not resolve the current test binary")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable,
		"-test.run=^"+testName+"$", "-test.count=1", "-test.v", "-test.timeout=25s")
	var output nativeBatchColdPoolOutput
	command.Stdout, command.Stderr = &output, &output
	command.WaitDelay = 2 * time.Second
	err = command.Run()
	if ctx.Err() != nil {
		t.Fatal("cold pool fixture exceeded its outer execution bound")
	}
	if output.overflow {
		t.Fatal("cold pool fixture exceeded its fixed diagnostic bound")
	}
	data := output.data[:output.count]
	if bytes.Count(data, []byte("=== RUN   "+testName+"\n")) != 1 {
		t.Fatal("cold pool fixture did not run its selected test exactly once")
	}
	if bytes.Contains(data, []byte("WARNING: DATA RACE")) ||
		bytes.Contains(data, []byte("panic: test timed out")) ||
		regexp.MustCompile(`(?m)^[\t ]*[^\n]*\.go:[0-9]+: `).Match(data) {
		t.Fatal("cold pool fixture reported a separate assertion, race or timeout")
	}
	if err != nil {
		var childExit *exec.ExitError
		if errors.As(err, &childExit) && childExit.ExitCode() == 2 &&
			bytes.Count(data, []byte("--- FAIL: "+testName+" (")) == 1 &&
			bytes.Contains(data, []byte("panic: deadlock: main bubble goroutine has exited but blocked goroutines remain")) &&
			bytes.Contains(data, []byte("github.com/urnetwork/connect.poolStats(")) &&
			bytes.Contains(data, []byte("/message_pool.go:493")) {
			// Keep child panic output out of the parent Go JSON stream.
			t.Fatal("cold pool diagnostics worker escaped its synctest bubble")
		}
		t.Fatal("cold pool fixture child failed without the required lifetime signature")
	}
	if bytes.Count(data, []byte("--- PASS: "+testName+" (")) != 1 {
		t.Fatal("cold pool fixture did not pass its selected test exactly once")
	}
}

// Encryption refusal must retain its selected owner when it is the first
// packet-path test in the process, not only after another test warmed the pool.
func TestNativeBatchReadyEncryptionColdPoolLifecycle(t *testing.T) {
	testNativeBatchColdPoolLifecycle(t, "TestNativeBatchReadyEncryptionRefusalKeepsSelectedOwner")
}

// The sibling hard-error path must independently end the same ownership scope
// after its bubble joins, with no unrelated earlier pool initialization.
func TestNativeBatchReadyHardErrorColdPoolLifecycle(t *testing.T) {
	testNativeBatchColdPoolLifecycle(t, "TestNativeBatchReadyHardErrorIsNotRetried")
}
