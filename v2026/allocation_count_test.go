package connect

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The test an allocation count child process runs, and the foreign
// allocation its control adds to it.
const allocationCountRootEnv = "URNETWORK_TEST_ALLOCATION_COUNT_ROOT"
const allocationCountForeignEnv = "URNETWORK_TEST_ALLOCATION_COUNT_FOREIGN"

// Runs the calling test's body in a fresh process of this test binary, unless
// the test already runs in one, and reports whether the caller should return
// because the child ran the body. testing.AllocsPerRun counts every malloc of
// its process, tiny allocations included, so in the suite's process any
// goroutine an earlier test left behind moves the count when it allocates
// during the runs. The child runs this test alone, with the existing
// same-binary, exact-root re-execution pattern (messagePoolSnapshotInFreshProcess).
// Besides the test, its only goroutines are the testing framework's, waiting
// for the test, and glog's flush daemon, asleep on a 30 s ticker started with
// the process. So the count is the measured code's alone, exact as
// AllocsPerRun reports it.
func allocationsInFreshProcess(t *testing.T) bool {
	t.Helper()
	if root := os.Getenv(allocationCountRootEnv); root != "" {
		if root != t.Name() {
			t.Fatalf("allocation count child selected %q, running %q", root, t.Name())
		}
		return false
	}
	if output, err := runAllocationCountChild(t, t.Name(), ""); err != nil {
		t.Fatalf("allocation count child %s: %v\n%s", t.Name(), err, output)
	}
	return true
}

// Runs root alone in a fresh process of this test binary, with foreign as
// the control's foreign allocation mode, and returns its verbose output.
func runAllocationCountChild(t *testing.T, root string, foreign string) ([]byte, error) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	bodyTimeout := time.Minute
	if deadline, ok := t.Deadline(); ok {
		bodyTimeout = min(bodyTimeout, time.Until(deadline)-10*time.Second)
	}
	if bodyTimeout <= 0 {
		return nil, errors.New("allocation count has no remaining child budget")
	}
	ctx, cancel := context.WithTimeout(t.Context(), bodyTimeout+5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable,
		"-test.run=^"+regexp.QuoteMeta(root)+"$",
		"-test.v",
		"-test.count=1",
		"-test.timeout="+bodyTimeout.String(),
	)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, allocationCountRootEnv+"=") && !strings.HasPrefix(entry, allocationCountForeignEnv+"=") {
			command.Env = append(command.Env, entry)
		}
	}
	command.Env = append(command.Env, allocationCountRootEnv+"="+root, allocationCountForeignEnv+"="+foreign)
	output, err := command.CombinedOutput()
	return output, errors.Join(err, ctx.Err())
}

// A goroutine another test left behind, for the control. In a child started
// in the lockstep foreign mode, each call wakes a goroutine of the child and
// waits until it has made five allocations. Anywhere else a call does nothing.
func allocationCountForeignStep(t *testing.T) func() {
	t.Helper()
	switch mode := os.Getenv(allocationCountForeignEnv); mode {
	case "":
		return func() {}
	case "lockstep":
	default:
		t.Fatalf("invalid allocation count foreign mode %q", mode)
	}
	wake := make(chan struct{})
	allocated := make(chan struct{})
	go func() {
		var sink [][]byte
		for range wake {
			for range 5 {
				sink = append(sink[:0], make([]byte, 64))
			}
			allocated <- struct{}{}
		}
	}()
	t.Cleanup(func() {
		close(wake)
	})
	return func() {
		wake <- struct{}{}
		<-allocated
	}
}

// The grouping comparison's flake, reproduced and removed. A goroutine of the
// measuring process that allocates in lockstep with the runs moves
// AllocsPerRun's count, and the comparison fails. Run in a process of its
// own, the same comparison passes, while a goroutine of this process
// allocates throughout.
func TestAllocationCountNeedsAProcessOfItsOwn(t *testing.T) {
	const root = "TestMultiClientBatchResultsLegacyGroupingAllocations"
	output, err := runAllocationCountChild(t, root, "lockstep")
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 ||
		!strings.Contains(string(output), "count-only grouping allocations=") ||
		!strings.Contains(string(output), "--- FAIL: "+root) {
		t.Fatalf("a foreign goroutine in the measuring process did not move the count: %v\n%s", err, output)
	}

	stop := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		var sink [][]byte
		for {
			select {
			case <-stop:
				return
			default:
			}
			sink = append(sink[:0], make([]byte, 64))
		}
	}()
	output, err = runAllocationCountChild(t, root, "")
	close(stop)
	<-stopped
	if err != nil || !strings.Contains(string(output), "--- PASS: "+root) {
		t.Fatalf("the comparison in a process of its own failed while this process allocated: %v\n%s", err, output)
	}
}

// An 8-byte pointer-free object goes to the tiny allocator, which packs two
// of them into one 16-byte block, so a heap profile sees only every other one.
// AllocsPerRun counts each.
func TestAllocationCountIncludesTinyAllocations(t *testing.T) {
	if allocationsInFreshProcess(t) {
		return
	}
	if got := testing.AllocsPerRun(100, func() {
		tinyAllocationSink = new([8]byte)
	}); got != 1 {
		t.Fatalf("tiny allocations per run = %g, want 1", got)
	}
}

var tinyAllocationSink *[8]byte
