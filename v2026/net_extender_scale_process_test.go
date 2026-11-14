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

const extenderScaleRootEnv = "URNETWORK_TEST_EXTENDER_SCALE_ROOT"

// RUSAGE_SELF counts every goroutine in the process. Keep the unchanged CPU
// budget attributable to this test by running its exact body in a fresh copy
// of the same test binary, including the same race instrumentation.
func extenderScaleInFreshProcess(t *testing.T) bool {
	t.Helper()
	if root := os.Getenv(extenderScaleRootEnv); root != "" {
		if root != t.Name() {
			t.Fatalf("extender scale child selected %q, running %q", root, t.Name())
		}
		return false
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bodyTimeout := 2 * time.Minute
	if deadline, ok := t.Deadline(); ok {
		bodyTimeout = min(bodyTimeout, time.Until(deadline)-10*time.Second)
	}
	if bodyTimeout <= 0 {
		t.Fatal("extender scale has no remaining child and join budget")
	}
	ctx, cancel := context.WithTimeout(t.Context(), bodyTimeout+5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "-test.run=^"+regexp.QuoteMeta(t.Name())+"$", "-test.v", "-test.count=1", "-test.timeout="+bodyTimeout.String())
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, extenderScaleRootEnv+"=") {
			command.Env = append(command.Env, entry)
		}
	}
	command.Env = append(command.Env, extenderScaleRootEnv+"="+t.Name())
	output, err := command.CombinedOutput()
	if err = errors.Join(err, ctx.Err()); err != nil {
		t.Fatalf("exclusive extender scale %s: %v\n%s", t.Name(), err, output)
	}
	t.Logf("exclusive extender scale child:\n%s", output)
	return true
}
