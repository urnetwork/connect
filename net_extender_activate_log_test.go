package connect

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
)

// What the activator logs when an activation post fails (connect#229).
//
// A post whose every attempt failed ends in an exhausted request, whose own
// message is the fixed "http request attempts exhausted": logged alone, it
// hides which candidate failed and how. The log line names the cause of each
// attempt after it, and never the jwt the post carried.
//
// The post runs through a real direct strategy, as the role builds one, whose
// tcp dial and udp socket each fail at once with their own cause, so the
// normal dialer and the two alt dialers fail on every attempt. It runs in a
// synctest bubble: the request budget ends in bubble time once every attempt
// has failed, with no wait and no network.

// Records every line the activator logs.
type testActivateLogger struct {
	stateLock sync.Mutex
	lines     []string
}

func (self *testActivateLogger) add(line string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.lines = append(self.lines, line)
}

func (self *testActivateLogger) Info(args ...any) {
	self.add(fmt.Sprint(args...))
}

func (self *testActivateLogger) Infof(format string, args ...any) {
	self.add(fmt.Sprintf(format, args...))
}

func (self *testActivateLogger) Warningf(format string, args ...any) {
	self.add(fmt.Sprintf(format, args...))
}

func (self *testActivateLogger) Errorf(format string, args ...any) {
	self.add(fmt.Sprintf(format, args...))
}

func (self *testActivateLogger) V(level int32) Verbose {
	return NewNoopLogger().V(level)
}

// The logged lines that start with `prefix`.
func (self *testActivateLogger) linesWithPrefix(prefix string) []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	lines := []string{}
	for _, line := range self.lines {
		if strings.HasPrefix(line, prefix) {
			lines = append(lines, line)
		}
	}
	return lines
}

func TestExtenderActivatorLogsTheCauseOfEveryAttempt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tcpCause := errors.New("test tcp dial refused")
		udpCause := errors.New("test udp socket refused")
		const byJwt = "test-activation-jwt"

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		strategySettings := DefaultClientStrategySettings()
		strategySettings.Log = NewNoopLogger()
		strategySettings.ConnectSettings.Log = NewNoopLogger()
		// the normal dialer and the two alt dialers: one attempt each per round
		strategySettings.EnableResilient = false
		// an ip literal, so the alt attempts resolve nothing
		strategySettings.AltUrl = "https://192.0.2.10"
		strategySettings.ConnectSettings.DialContextSettings = &DialContextSettings{
			DialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
				return nil, tcpCause
			},
			PacketConnFactory: func(ctx context.Context) (net.PacketConn, error) {
				return nil, udpCause
			},
		}
		clientStrategy := NewDirectClientStrategy(ctx, strategySettings, 0)
		defer clientStrategy.Close()

		log := &testActivateLogger{}
		settings := DefaultExtenderActivatorSettings()
		settings.Log = log
		settings.ApiUrlV4 = "https://192.0.2.4"
		settings.ByJwt = func() string {
			return byJwt
		}
		settings.ClientStrategy = clientStrategy
		settings.PublicKey = []byte("test extender public key")
		settings.Carriers = func() []string {
			return []string{ExtenderCarrierTcp}
		}
		settings.IpVersionSupported = func(ipVersion int) bool {
			return true
		}
		settings.AddNetworkChangeListener = func(listener func()) func() {
			return func() {}
		}
		// the bubble clock never sleeps, so there is no resume to watch
		settings.ResumeCheckTimeout = 0
		activator := NewExtenderActivator(ctx, settings)
		defer activator.Close()

		// the first recorded outcome is the failed post of the first pass
		for {
			version, change := activator.ChangeMonitor().Get()
			if 0 < version {
				break
			}
			<-change
		}
		status := activator.Status().Family(4)
		if status == nil || status.Activated || status.LastError == "" {
			t.Fatalf("v4 status = %+v, expected a failed activation", status)
		}

		lines := log.linesWithPrefix("[extender]activate v4 err = ")
		if len(lines) == 0 {
			t.Fatal("the failed activation logged no error")
		}
		line := lines[0]
		t.Logf("activation error log: %s", line)
		for _, expected := range []string{
			(&HttpRequestExhaustedError{}).Error(),
			tcpCause.Error(),
			udpCause.Error(),
		} {
			if !strings.Contains(line, expected) {
				t.Errorf("the activation error log %q does not name %q", line, expected)
			}
		}
		if strings.Contains(line, byJwt) {
			t.Errorf("the activation error log %q carries the jwt", line)
		}
	})
}
