package connect

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"strings"
	"time"
)

// The run of this process that every extender activation reports (EXTENDER.md
// G3): an id drawn once when the process starts, the time it started, and how
// the run before it ended when the host that restarted it says so. The
// operator stores the three on each activation's history row, so a restart is
// a new run id among an extender's activations rather than something inferred
// from its platform connections, and a process that restarts every half hour
// shows as many runs a day (EXTENDER-ACTIVATION.md §7).
//
// The run belongs to the process, not to an activator: every activator of one
// process -- the sdk's provider role, connectctl's extender, each slot of an sn
// miner -- reports the same run, and it never changes while the process runs.

// The environment variable through which a host that restarts this process
// says how the run before it ended. The sn miner's supervisor sets it for a
// provider child it restarts (sn/miner/operators_supervisor.go). Its words are
// the operator's (EXTENDER.md G3): "oom", "stop", a signal such as "sigkill",
// or "exit-<code>". An activation carries the value when it is a short token
// of lower case letters, digits and dashes, and the operator keeps it only
// when it is one of those words.
const ExtenderPreviousExitEnv = "URNETWORK_PREVIOUS_EXIT"

// The random bytes of a run id, 32 hex characters on the wire.
const extenderRunIdByteCount = 16

// The longest previous exit an activation carries, the width of the
// operator's column.
const extenderPreviousExitMaxLength = 16

type ExtenderRun struct {
	// 32 lower case hex characters of 128 random bits
	Id string
	// when the process started, utc
	StartTime time.Time
	// how the run before this one ended, as the host reported it; empty when
	// it reported none
	PreviousExit string
}

// Drawn when the package initializes, which is the process start to within the
// runtime's own initialization, and read from the environment the process
// started with.
var extenderProcessRun = newExtenderRun(time.Now(), os.Getenv(ExtenderPreviousExitEnv))

// The run of this process, the same at every call.
func ExtenderProcessRun() *ExtenderRun {
	run := *extenderProcessRun
	return &run
}

func newExtenderRun(startTime time.Time, previousExit string) *ExtenderRun {
	idBytes := make([]byte, extenderRunIdByteCount)
	// never fails: crypto/rand crashes the process rather than return an error
	rand.Read(idBytes)
	return &ExtenderRun{
		Id:           hex.EncodeToString(idBytes),
		StartTime:    startTime.UTC(),
		PreviousExit: extenderPreviousExit(previousExit),
	}
}

// The host's previous exit as an activation carries it: trimmed and in lower
// case, when it is a token of letters, digits and dashes no longer than the
// operator's column, else none. The operator judges the word itself, so a
// word it learns later needs no new extender.
func extenderPreviousExit(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if extenderPreviousExitMaxLength < len(value) {
		return ""
	}
	for _, c := range value {
		if !('a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-') {
			return ""
		}
	}
	return value
}
