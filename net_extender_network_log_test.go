package connect

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// What the network client says about a candidate that failed, at default level and
// in its status (EXTENDER-UPDATE2 §1.3; EXTENDER.md R1).
//
// Root cause guarded: the feed and probe lines named each candidate's address at
// default level, the text of a failed dial names it again, and that text went into
// the status the apps read. A client holds gated records, whose addresses no public
// channel carries, and its logs and status leave the device in feedback and support.

// An error of every shape the dial path produces, and what is kept of it.
func TestExtenderErrorTextTakesEveryAddressOut(t *testing.T) {
	for _, test := range []struct {
		err      string
		expected string
	}{
		{
			err:      "dial tcp 192.0.2.1:443: i/o timeout",
			expected: "dial tcp <address>: i/o timeout",
		},
		{
			err:      "dial tcp [2001:db8::1]:443: connect: connection refused",
			expected: "dial tcp [<address>]:443: connect: connection refused",
		},
		{
			err:      "read tcp 10.0.0.2:51515->192.0.2.1:443: read: connection reset by peer",
			expected: "read tcp <address>-><address>: read: connection reset by peer",
		},
		{
			err:      "dial udp [fe80::1%en0]:53: no route to host",
			expected: "dial udp [<address>]:53: no route to host",
		},
		{
			err:      "tls: failed to verify certificate: x509: certificate is valid for 192.0.2.9, not 192.0.2.1",
			expected: "tls: failed to verify certificate: x509: certificate is valid for <address>, not <address>",
		},
		{
			err:      "the address ::ffff:192.0.2.1 did not answer.",
			expected: "the address <address> did not answer.",
		},
		{
			err:      "post https://192.0.2.1/hello: EOF",
			expected: "post https://<address>/hello: EOF",
		},
		// nothing that only looks like one is touched
		{
			err:      "the extender did not answer",
			expected: "the extender did not answer",
		},
		{
			err:      "dial tcp: lookup api.ur.example: no such host, at 12:30:45 over tls 1.3",
			expected: "dial tcp: lookup api.ur.example: no such host, at 12:30:45 over tls 1.3",
		},
		{
			err:      "add: bad: cafe: 1.2: a.b.c",
			expected: "add: bad: cafe: 1.2: a.b.c",
		},
	} {
		if text := extenderErrorText(errors.New(test.err)); text != test.expected {
			t.Errorf("%q became %q, expected %q", test.err, text, test.expected)
		}
	}

	// a real one, from the standard library
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	if _, dialErr := net.DialTimeout("tcp", address, time.Second); dialErr != nil {
		if text := extenderErrorText(dialErr); strings.Contains(text, "127.0.0.1") {
			t.Errorf("a real dial error still names its address: %q", text)
		}
	}
}

// At default level, and in the status, no failed dial of any route names an
// address: the feed, the probe pass, hello and the hint, all failing with what a
// real dial says.
func TestExtenderNetworkClientNamesNoCandidateAddress(t *testing.T) {
	clock := newTestClock()
	logger := &testActivateLogger{}
	const candidateIp = "192.0.2.77"
	failure := fmt.Errorf("dial tcp %s:443: connect: connection refused", candidateIp)

	rootSeed, err := NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	rootPrivateKey, err := ExtenderPrivateKeyFromSeed(rootSeed)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	directorySettings := DefaultExtenderDirectorySettings()
	directorySettings.Now = clock.Now
	directorySettings.NetworkHosts = []string{testExtenderNetworkHost}
	// no hold, so the feed's failure does not keep the probe pass from the
	// candidate, whichever runs first
	directorySettings.HoldTimeout = 0
	directorySettings.MaxHoldTimeout = 0
	directory := NewExtenderDirectory(ctx, directorySettings)
	defer directory.Close()
	directory.SetRootKeys(NewExtenderRootKeySet(rootPrivateKey.Public().(ed25519.PublicKey)))

	record := signTestTierRecord(t, rootPrivateKey, newTestExtenderKey(t), clock.Now(), ExtenderDirectoryTierOpen, candidateIp)
	if _, err := directory.ApplyRecord(record, ExtenderSourceFeed); err != nil {
		t.Fatal(err)
	}

	strategySettings := DefaultClientStrategySettings()
	strategySettings.ConnectSettings.DialContextSettings = &DialContextSettings{
		DialContext: func(ctx context.Context, network string, addr string) (net.Conn, error) {
			return nil, fmt.Errorf("dial %s %s: connect: connection refused", network, addr)
		},
		PacketConnFactory: func(ctx context.Context) (net.PacketConn, error) {
			return nil, fmt.Errorf("listen udp %s:0: no packet endpoint", candidateIp)
		},
	}
	clientStrategy := NewClientStrategy(ctx, strategySettings)
	defer clientStrategy.Close()

	settings := DefaultExtenderNetworkClientSettings()
	settings.Log = logger
	settings.Now = clock.Now
	settings.ExtenderDnsName = "extender.space.example"
	settings.MinBackoff = time.Millisecond
	settings.MaxBackoff = 10 * time.Millisecond
	settings.DialTimeout = 2 * time.Second
	settings.HelloTimeout = 2 * time.Second
	settings.IpVersionSupported = func(ipVersion int) bool { return ipVersion == 4 }
	settings.Hello = func(ctx context.Context) (*ExtenderHelloResult, error) {
		return nil, failure
	}
	settings.Hint = func(ctx context.Context) (*ExtenderHintResult, error) {
		return nil, failure
	}
	settings.ResolveDnsTxt = func(ctx context.Context, name string) ([]string, error) {
		return nil, failure
	}
	settings.ResolveDns = func(ctx context.Context, name string) ([]netip.Addr, error) {
		return nil, failure
	}
	settings.Probe = func(ctx context.Context, candidate *ExtenderCandidate, attestor *ExtenderProbeAttestor) (time.Duration, ExtenderPingOutcome, error) {
		return 0, ExtenderPingUnattested, fmt.Errorf("dial tcp %s:443: i/o timeout", candidate.Ip)
	}
	networkClient := NewExtenderNetworkClient(ctx, clientStrategy, directory, settings)
	defer networkClient.Close()

	// every route has failed and said so at least once
	deadline := time.Now().Add(30 * time.Second)
	prefixes := []string{"[extender]feed", "[extender]probe", "[extender]hello", "[extender]hint"}
	for {
		// past the constant the status says while there is no candidate
		status := networkClient.Status().LastError
		seen := status != "" && status != "no extender candidate"
		for _, prefix := range prefixes {
			seen = seen && 0 < len(logger.linesWithPrefix(prefix))
		}
		if seen {
			break
		}
		if deadline.Before(time.Now()) {
			t.Fatalf("not every route failed: status %q", networkClient.Status().LastError)
		}
		time.Sleep(5 * time.Millisecond)
	}

	if status := networkClient.Status().LastError; strings.Contains(status, candidateIp) {
		t.Errorf("the status names the address: %q", status)
	}
	for _, line := range logger.linesWithPrefix("[extender]") {
		if strings.Contains(line, candidateIp) {
			t.Errorf("a default-level line names the address: %q", line)
		}
	}
}

// The lines themselves, including those a run does not reach: at default level in
// the walkers of the candidates, no argument is an address, and an error goes out
// only through extenderErrorText. What sits behind a verbosity a caller opted into
// is not read here.
func TestExtenderNetworkClientDefaultLogLinesNameNoAddress(t *testing.T) {
	for _, fileName := range []string{"net_extender_network.go", "net_extender_dns_progress.go", "net_extender_peer_pinger.go"} {
		t.Run(fileName, func(t *testing.T) {
			file, err := parser.ParseFile(token.NewFileSet(), fileName, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			logCallCount := 0
			var visit func(node ast.Node) bool
			visit = func(node ast.Node) bool {
				// behind `if self.log.V(n).Enabled() {`, opted into
				if statement, ok := node.(*ast.IfStmt); ok && testLogVerbosityGuard(statement.Cond) {
					if statement.Else != nil {
						ast.Inspect(statement.Else, visit)
					}
					return false
				}
				call, ok := node.(*ast.CallExpr)
				if !ok || !testDefaultLevelLogCall(call) {
					return true
				}
				logCallCount += 1
				for _, arg := range call.Args {
					if inner, ok := arg.(*ast.CallExpr); ok {
						if function, ok := inner.Fun.(*ast.Ident); ok && function.Name == "extenderErrorText" {
							continue
						}
					}
					ast.Inspect(arg, func(argNode ast.Node) bool {
						switch argNode := argNode.(type) {
						case *ast.SelectorExpr:
							if argNode.Sel.Name == "Ip" {
								t.Errorf("a default-level line names an address: %s", testLogCallFormat(call))
							}
						case *ast.Ident:
							if argNode.Name == "err" {
								t.Errorf("a default-level line carries an error as it came: %s", testLogCallFormat(call))
							}
						}
						return true
					})
				}
				return true
			}
			ast.Inspect(file, visit)
			if fileName == "net_extender_network.go" && logCallCount == 0 {
				t.Fatal("no log line was read, so this pins nothing")
			}
		})
	}
}

// Whether `condition` is `self.log.V(n).Enabled()`.
func testLogVerbosityGuard(condition ast.Expr) bool {
	call, ok := condition.(*ast.CallExpr)
	if !ok {
		return false
	}
	enabled, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || enabled.Sel.Name != "Enabled" {
		return false
	}
	verbose, ok := enabled.X.(*ast.CallExpr)
	if !ok {
		return false
	}
	verbosity, ok := verbose.Fun.(*ast.SelectorExpr)
	return ok && verbosity.Sel.Name == "V"
}

// Whether `call` is `self.log.Infof(...)` or another line with no verbosity.
func testDefaultLevelLogCall(call *ast.CallExpr) bool {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	switch selector.Sel.Name {
	case "Info", "Infof", "Warningf", "Errorf":
	default:
		return false
	}
	receiver, ok := selector.X.(*ast.SelectorExpr)
	return ok && receiver.Sel.Name == "log"
}

// The format string of a log call, to say which line it is.
func testLogCallFormat(call *ast.CallExpr) string {
	if 0 < len(call.Args) {
		if literal, ok := call.Args[0].(*ast.BasicLit); ok {
			return literal.Value
		}
	}
	return "a log line"
}
