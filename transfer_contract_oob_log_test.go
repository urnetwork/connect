package connect

// The "[contract]oob err" line names the causes an exhausted control post
// retained, as the platform transport's auth error does
// (transport_family_route_test.go). Its own message, "http request attempts
// exhausted", names none.

import (
	"context"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// exhaustedContractOob fails every contract request as a control post does
// whose every attempt met no route, and answers other control empty. Takes the
// frames, like every OutOfBandControl.
type exhaustedContractOob struct{}

func (self *exhaustedContractOob) SendControl(frames []*protocol.Frame, callback OobResultFunction) {
	contract := false
	for _, frame := range frames {
		if message, err := FromFrame(frame); err == nil {
			if _, ok := message.(*protocol.CreateContract); ok {
				contract = true
			}
		}
		MessagePoolReturn(frame.MessageBytes)
	}
	if !contract {
		callback(nil, nil)
		return
	}
	causes := newHttpRequestCauses(context.Background())
	causes.record(noRouteDialError("tcp6"))
	callback(nil, causes.exhausted(context.Background(), nil))
}

// The lines a capture logger recorded at INFO and at every verbose level that
// contain `substring`.
func captureLoggerLinesWith(log *captureLogger, substring string) []string {
	log.mu.Lock()
	defer log.mu.Unlock()
	matched := []string{}
	for _, line := range append(append([]string{}, log.info...), log.verbose...) {
		if strings.Contains(line, substring) {
			matched = append(matched, strings.TrimSpace(line))
		}
	}
	return matched
}

func TestContractOobErrLogNamesItsCauses(t *testing.T) {
	resetBackendDegraded()
	defer resetBackendDegraded()

	// throttled at INFO across the process, every failure at V(1)
	log := &captureLogger{enabled: true}
	settings := contractRetryTestSettings()
	settings.Log = log
	client := newContractTestClientWithOob(t, NewId(), &exhaustedContractOob{}, settings)
	sendContractRetryMessage(t, client, NewId())

	var lines []string
	if !waitForCondition(15*time.Second, func() bool {
		lines = captureLoggerLinesWith(log, "[contract]oob err")
		return 0 < len(lines)
	}) {
		t.Fatal("no oob error line for the exhausted contract request")
	}
	for _, line := range lines {
		for _, expected := range []string{
			(&HttpRequestExhaustedError{}).Error() + ": ",
			syscall.EHOSTUNREACH.Error(),
		} {
			if !strings.Contains(line, expected) {
				t.Errorf("oob error line %q does not name %q", line, expected)
			}
		}
	}
}
