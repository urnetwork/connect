// Late generator results retain ownership even when they also carry an error.
package connect

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

// The real deadline wrapper hands the late result to the production cleanup.
// Explicit barriers force cancellation before the generator returns.
func testGeneratorDeadlineLateOwnership(t *testing.T, partial bool) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		entered, release, returned, cleaned := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
		var args *MultiClientGeneratorClientArgs
		if partial {
			args = &MultiClientGeneratorClientArgs{ClientId: NewId()}
		}
		removed := 0
		generator := &TestMultiClientGenerator{removeClientArgs: func(got *MultiClientGeneratorClientArgs) {
			if got == nil || got != args {
				t.Error("late cleanup did not retain the exact returned identity")
			}
			removed++
		}}
		window := &multiClientWindow{generator: generator, log: newRecordingLogger()}
		go func() {
			defer close(returned)
			got, err := windowGeneratorCall(ctx, time.Hour, func() (*MultiClientGeneratorClientArgs, error) {
				close(entered)
				<-release
				return args, errors.New("synthetic partial mint failure")
			}, func(args *MultiClientGeneratorClientArgs, err error) {
				window.removeLateClientArgs(args, err)
				close(cleaned)
			})
			if got != nil || err == nil {
				t.Errorf("canceled generator returned args=%t error=%v", got != nil, err)
			}
		}()
		<-entered
		cancel()
		<-returned
		close(release)
		<-cleaned
		want := 0
		if partial {
			want = 1
		}
		if removed != want {
			t.Fatalf("late result removals=%d, want %d", removed, want)
		}
	})
}

// A returned identity remains owned even if a later step failed.
func TestGeneratorDeadlineLatePartialArgsRetireOnce(t *testing.T) {
	testGeneratorDeadlineLateOwnership(t, true)
}

// An error without a returned identity must not invent retirement work.
func TestGeneratorDeadlineLateNilArgsDoNotRetire(t *testing.T) {
	testGeneratorDeadlineLateOwnership(t, false)
}
