package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/extender"
)

// The admission limits of the connectctl extender (EXTENDER.md A12): the
// flags that set them, the log lines that report them, and the command
// applying them to every carrier it serves.

// The grammar takes both limits once and the unlimited source more than once,
// and each prefix arrives masked.
func TestExtenderUsageParsesTheAdmissionFlags(t *testing.T) {
	argv := []string{
		"extender",
		"--jwt=test-jwt",
		"--api_url=" + testOptionsApiUrl,
		"--admission_subnets_per_minute=500",
		"--admission_actions_per_subnet_per_minute=4",
		"--admission_unlimited_source=192.0.2.0/24",
		"--admission_unlimited_source= 198.51.100.7/24 ",
		"--admission_unlimited_source=2001:db8:1::/48",
	}
	opts, err := docopt.ParseArgs(connectCtlUsage(), argv, ConnectCtlVersion)
	if err != nil {
		t.Fatal(err)
	}
	options, err := extenderOptionsFromOpts(opts)
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, options.admissionSubnetsPerMinute, 500)
	connect.AssertEqual(t, options.admissionActionsPerSubnetPerMinute, 4)
	connect.AssertEqual(t, options.admissionUnlimitedSources, []netip.Prefix{
		netip.MustParsePrefix("192.0.2.0/24"),
		netip.MustParsePrefix("198.51.100.0/24"),
		netip.MustParsePrefix("2001:db8:1::/48"),
	})
}

// Without the flags the extender defaults stand, and no source is unlimited.
// A limit of 0 disables it.
func TestExtenderAdmissionOptionDefaults(t *testing.T) {
	defaults := extender.DefaultExtenderSettings()
	opts, err := docopt.ParseArgs(
		connectCtlUsage(), []string{"extender", "--jwt=test-jwt"}, ConnectCtlVersion)
	if err != nil {
		t.Fatal(err)
	}
	options, err := extenderOptionsFromOpts(opts)
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, options.admissionSubnetsPerMinute, defaults.AdmissionSubnetsPerMinute)
	connect.AssertEqual(t, options.admissionActionsPerSubnetPerMinute, defaults.AdmissionActionsPerSubnetPerMinute)
	if 0 < len(options.admissionUnlimitedSources) {
		t.Fatalf("unlimited sources = %v, expected none", options.admissionUnlimitedSources)
	}

	opts, err = docopt.ParseArgs(connectCtlUsage(), []string{
		"extender",
		"--jwt=test-jwt",
		"--admission_subnets_per_minute=0",
		"--admission_actions_per_subnet_per_minute=0",
	}, ConnectCtlVersion)
	if err != nil {
		t.Fatal(err)
	}
	options, err = extenderOptionsFromOpts(opts)
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, options.admissionSubnetsPerMinute, 0)
	connect.AssertEqual(t, options.admissionActionsPerSubnetPerMinute, 0)
}

// A limit that is not a count is refused, and the message names the flag that
// was wrong.
func TestExtenderOptionsRefuseABadAdmissionCount(t *testing.T) {
	for _, flag := range []string{
		"--admission_subnets_per_minute",
		"--admission_actions_per_subnet_per_minute",
	} {
		for _, value := range []string{"-1", "many", "1.5", ""} {
			options, err := extenderOptionsFromOpts(docopt.Opts{
				"--jwt":     "test-jwt",
				"--api_url": testOptionsApiUrl,
				flag:        value,
			})
			if err == nil {
				t.Errorf("%s=%q was accepted as %+v", flag, value, options)
				continue
			}
			if !strings.Contains(err.Error(), flag) {
				t.Errorf("%s=%q failed with %q, which does not name the flag", flag, value, err)
			}
		}
	}
}

// A source that is not a cidr prefix fails the command at start. The message
// names the flag and which of its values was wrong, never the value, so a
// mistake in the arguments is not repeated into the log.
func TestExtenderOptionsRefuseAnInvalidAdmissionUnlimitedSource(t *testing.T) {
	for _, value := range []string{
		"not-a-prefix",
		// an address is not a prefix
		"192.0.2.7",
		"192.0.2.0/33",
		"2001:db8::/129",
		"",
	} {
		options, err := extenderOptionsFromOpts(docopt.Opts{
			"--jwt":                        "test-jwt",
			"--api_url":                    testOptionsApiUrl,
			"--admission_unlimited_source": []string{"192.0.2.0/24", value},
		})
		if err == nil {
			t.Errorf("unlimited source %q was accepted as %v", value, options.admissionUnlimitedSources)
			continue
		}
		connect.AssertEqual(t, err.Error(), "--admission_unlimited_source 1 is not a cidr prefix")
		if value != "" && strings.Contains(err.Error(), value) {
			t.Errorf("the error %q repeats the value", err)
		}
	}
}

// The admission counts as the log reports them, and nothing before the limits
// have refused or waved anything through.
func TestExtenderAdmissionStatsLine(t *testing.T) {
	connect.AssertEqual(t, extenderAdmissionStatsLine(extender.ExtenderAdmissionStats{}), "")
	connect.AssertEqual(
		t,
		extenderAdmissionStatsLine(extender.ExtenderAdmissionStats{
			LimitedBySubnetsCount: 3,
			LimitedBySourceCount:  5,
			UnlimitedCount:        7,
		}),
		"extender admission: limited 3 by subnets, 5 by source; 7 unlimited",
	)
	connect.AssertEqual(
		t,
		extenderAdmissionStatsLine(extender.ExtenderAdmissionStats{UnlimitedCount: 1}),
		"extender admission: limited 0 by subnets, 0 by source; 1 unlimited",
	)
}

// One listener over several, which is how one tcp carrier accepts on both
// loopback families here.
type testMuxListener struct {
	listeners []net.Listener
	conns     chan net.Conn
	closed    chan struct{}
	closeOnce sync.Once
}

// A listener accepting on every one given, until it is closed.
func newTestMuxListener(listeners ...net.Listener) *testMuxListener {
	self := &testMuxListener{
		listeners: listeners,
		conns:     make(chan net.Conn),
		closed:    make(chan struct{}),
	}
	for _, listener := range listeners {
		go func() {
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				select {
				case self.conns <- conn:
				case <-self.closed:
					conn.Close()
					return
				}
			}
		}()
	}
	return self
}

// Implements net.Listener.
func (self *testMuxListener) Accept() (net.Conn, error) {
	select {
	case conn := <-self.conns:
		return conn, nil
	case <-self.closed:
		return nil, net.ErrClosed
	}
}

// Implements net.Listener: closes every listener, once.
func (self *testMuxListener) Close() error {
	self.closeOnce.Do(func() {
		close(self.closed)
		for _, listener := range self.listeners {
			listener.Close()
		}
	})
	return nil
}

// Implements net.Listener: the first listener's address.
func (self *testMuxListener) Addr() net.Addr {
	return self.listeners[0].Addr()
}

// The command applies the limits its flags set to what its carriers admit
// (A12): with one action a minute from a subnet, the second dial from v4
// loopback is answered 429 with a Retry-After in the default range, while v6
// loopback, which the unlimited source names, is admitted past the limit on
// every dial. The limits are logged at start and the counts once they change.
func TestExtenderCommandAppliesTheAdmissionLimits(t *testing.T) {
	// the command logs through the package loggers, which keep writing where
	// they did and also into the buffer this test reads
	logBuffer := &lockedLogBuffer{}
	outWriter := Out.Writer()
	errWriter := Err.Writer()
	Out.SetOutput(io.MultiWriter(logBuffer, outWriter))
	Err.SetOutput(io.MultiWriter(logBuffer, errWriter))
	t.Cleanup(func() {
		Out.SetOutput(outWriter)
		Err.SetOutput(errWriter)
	})

	operator := newTestExtenderOperator(t)
	operatorAddress := operator.server.Listener.Addr().String()
	_, operatorPort, err := net.SplitHostPort(operatorAddress)
	if err != nil {
		t.Fatal(err)
	}

	v4Listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	v6Listener, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		v4Listener.Close()
		t.Fatalf("ipv6 loopback is required for dual-stack tests: %v", err)
	}
	tcpListener := newTestMuxListener(v4Listener, v6Listener)
	t.Cleanup(func() { tcpListener.Close() })
	v4Port := v4Listener.Addr().(*net.TCPAddr).Port
	v6Port := v6Listener.Addr().(*net.TCPAddr).Port

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runs := make(chan *extenderRun, 1)
	options := &extenderOptions{
		jwt:      "test-jwt",
		apiUrl:   fmt.Sprintf("http://%s:%s", testExtenderApiHost, operatorPort),
		stateDir: t.TempDir(),
		// the udp carriers are configured apart, as a carrier must be, and
		// never bind here
		tcpPort:                            v4Port,
		udpPort:                            v6Port,
		dnsPort:                            v4Port,
		admissionSubnetsPerMinute:          1000,
		admissionActionsPerSubnetPerMinute: 1,
		admissionUnlimitedSources:          []netip.Prefix{netip.MustParsePrefix("::1/128")},
		listen: func(network string, address string) (net.Listener, error) {
			return tcpListener, nil
		},
		listenPacket: func(network string, address string) (net.PacketConn, error) {
			return nil, fmt.Errorf("this test serves tcp only")
		},
		dialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", operatorAddress)
		},
		configureNetworkClient: func(settings *connect.ExtenderNetworkClientSettings) {
			settings.ResolveDns = func(ctx context.Context, name string) ([]netip.Addr, error) {
				return nil, nil
			}
		},
		onStart: func(run *extenderRun) {
			runs <- run
		},
		admissionLogTimeout: 50 * time.Millisecond,
	}
	runDone := make(chan error, 1)
	go func() {
		runDone <- runExtender(ctx, options)
	}()
	var run *extenderRun
	select {
	case run = <-runs:
	case err := <-runDone:
		t.Fatalf("the extender exited before it started: %v", err)
	case <-time.After(60 * time.Second):
		t.Fatal("the extender did not start")
	}

	// a destination the whitelist never allows, so an admitted dial is
	// refused before anything is resolved or dialed
	dial := func(ip string, port int) error {
		dialCtx, dialCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer dialCancel()
		conn, _, err := connect.DialExtender(
			dialCtx,
			connect.DefaultConnectSettings(),
			&connect.ExtenderConfig{
				Profile: connect.ExtenderProfile{
					ConnectMode: connect.ExtenderConnectModeTcpTls,
					Port:        port,
				},
				Ip: netip.MustParseAddr(ip),
			},
			&connect.ExtenderDial{
				DestinationHost: "blocked.example",
				DestinationPort: 443,
			},
		)
		if err == nil {
			conn.Close()
		}
		return err
	}
	var refusedErr *connect.ExtenderRefusedError
	var limitedErr *connect.ExtenderLimitedError

	// the subnet's one action is admitted, and refused for its destination;
	// the next is over the limit
	if err := dial("127.0.0.1", v4Port); !errors.As(err, &refusedErr) || refusedErr.StatusCode != http.StatusForbidden {
		t.Fatalf("the first v4 dial got %v, expected the destination refusal", err)
	}
	if err := dial("127.0.0.1", v4Port); !errors.As(err, &limitedErr) {
		t.Fatalf("the second v4 dial got %v, expected the admission limit", err)
	}
	defaults := extender.DefaultExtenderSettings()
	if limitedErr.RetryAfter < defaults.AdmissionRetryAfterMin || defaults.AdmissionRetryAfterMax < limitedErr.RetryAfter {
		t.Fatalf(
			"retry after = %s, expected within [%s, %s]",
			limitedErr.RetryAfter,
			defaults.AdmissionRetryAfterMin,
			defaults.AdmissionRetryAfterMax,
		)
	}
	// the unlimited source is admitted past the limit every time
	for i := range 3 {
		if err := dial("::1", v6Port); !errors.As(err, &refusedErr) || refusedErr.StatusCode != http.StatusForbidden {
			t.Fatalf("v6 dial %d got %v, expected the destination refusal", i, err)
		}
	}
	connect.AssertEqual(t, run.server.AdmissionStats(), extender.ExtenderAdmissionStats{
		LimitedBySourceCount: 1,
		UnlimitedCount:       3,
	})

	startLine := "extender admission: 1000 subnets a minute, 1 actions a subnet a minute, 1 unlimited source prefixes"
	countsLine := "extender admission: limited 0 by subnets, 1 by source; 3 unlimited"
	deadline := time.Now().Add(30 * time.Second)
	for {
		logs := logBuffer.String()
		if strings.Contains(logs, startLine) && strings.Contains(logs, countsLine) {
			// each change is logged once
			connect.AssertEqual(t, strings.Count(logs, countsLine), 1)
			break
		}
		if deadline.Before(time.Now()) {
			t.Fatalf("the log has no %q and %q:\n%s", startLine, countsLine, logs)
		}
		select {
		case <-time.After(50 * time.Millisecond):
		case err := <-runDone:
			t.Fatalf("the extender exited: %v", err)
		}
	}
	// the prefix itself is never logged, only how many there are
	if slices.ContainsFunc(strings.Split(logBuffer.String(), "\n"), func(line string) bool {
		return strings.Contains(line, "extender admission") && strings.Contains(line, "::1")
	}) {
		t.Fatalf("an admission line names the unlimited source:\n%s", logBuffer.String())
	}

	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the extender did not stop")
	}
}
