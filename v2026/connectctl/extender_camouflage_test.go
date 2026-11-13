package main

// The tcp carrier camouflage of the connectctl extender (EXTENDER.md P7,
// EXTENDER-UPDATE2 §3, EXTENDER-SPLICE-ROLLOUT.md): the flags, and the command
// running with them against the fixture operator. Every list is synthetic, and
// the borrowed site is a loopback tls server behind the splice dial seam, so
// nothing leaves the machine.

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/extender"
	"github.com/urnetwork/connect/v2026/protocol"
)

// The synthetic borrowed name of these cases.
const testCamouflageBorrowName = "borrow.example"

// The grammar takes the three flags, and they arrive as the options.
func TestExtenderUsageParsesTheCamouflageFlags(t *testing.T) {
	opts, err := docopt.ParseArgs(connectCtlUsage(), []string{
		"extender",
		"--jwt=test-jwt",
		"--extender_key_file=/state/extender.key",
		"--camouflage",
		"--camouflage_splice",
		"--camouflage_revalidate_interval=12h",
	}, ConnectCtlVersion)
	if err != nil {
		t.Fatal(err)
	}
	options, err := extenderOptionsFromOpts(opts)
	if err != nil {
		t.Fatal(err)
	}
	if !options.camouflageEnabled || !options.camouflageSplice {
		t.Errorf("camouflage = %t, splice = %t, expected both", options.camouflageEnabled, options.camouflageSplice)
	}
	if options.camouflageRevalidationInterval != 12*time.Hour {
		t.Errorf("revalidation interval = %s", options.camouflageRevalidationInterval)
	}

	// --camouflage alone is Phase A: it does not read as a prefix of the others
	opts, err = docopt.ParseArgs(connectCtlUsage(), []string{
		"extender", "--jwt=test-jwt", "--extender_key_file=/state/extender.key", "--camouflage",
	}, ConnectCtlVersion)
	if err != nil {
		t.Fatal(err)
	}
	if options, err = extenderOptionsFromOpts(opts); err != nil {
		t.Fatal(err)
	}
	if !options.camouflageEnabled || options.camouflageSplice || options.camouflageRevalidationInterval != 0 {
		t.Errorf("options of --camouflage alone = %t, %t, %s",
			options.camouflageEnabled, options.camouflageSplice, options.camouflageRevalidationInterval)
	}
}

// Off unless asked: the options of a host that gives no flag ask for nothing.
func TestExtenderCamouflageFlagsAreOffByDefault(t *testing.T) {
	opts, err := docopt.ParseArgs(connectCtlUsage(), []string{"extender", "--jwt=test-jwt"}, ConnectCtlVersion)
	if err != nil {
		t.Fatal(err)
	}
	options, err := extenderOptionsFromOpts(opts)
	if err != nil {
		t.Fatal(err)
	}
	if options.camouflageEnabled || options.camouflageSplice || options.camouflageRevalidationInterval != 0 {
		t.Fatalf("options = %t, %t, %s, expected nothing asked for",
			options.camouflageEnabled, options.camouflageSplice, options.camouflageRevalidationInterval)
	}
}

// A flag that means nothing without --camouflage, or a value that is not a usable
// interval, is refused rather than ignored, and the message names the flag.
func TestExtenderCamouflageFlagsRefuseWhatTheyCannotMean(t *testing.T) {
	cases := []struct {
		name     string
		argv     []string
		expected string
	}{
		{
			name:     "the splice alone",
			argv:     []string{"--camouflage_splice"},
			expected: "--camouflage_splice needs --camouflage",
		},
		{
			name:     "the interval alone",
			argv:     []string{"--camouflage_revalidate_interval=6h"},
			expected: "--camouflage_revalidate_interval needs --camouflage",
		},
		{
			name:     "an interval that does not parse",
			argv:     []string{"--camouflage", "--camouflage_revalidate_interval=often"},
			expected: "--camouflage_revalidate_interval must be a duration",
		},
		{
			name:     "an interval under the floor",
			argv:     []string{"--camouflage", "--camouflage_revalidate_interval=1m"},
			expected: "--camouflage_revalidate_interval must be at least",
		},
		{
			name:     "a negative interval",
			argv:     []string{"--camouflage", "--camouflage_revalidate_interval=-6h"},
			expected: "--camouflage_revalidate_interval must be at least",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			opts, err := docopt.ParseArgs(
				connectCtlUsage(),
				append([]string{"extender", "--jwt=test-jwt"}, c.argv...),
				ConnectCtlVersion,
			)
			if err != nil {
				t.Fatal(err)
			}
			options, err := extenderOptionsFromOpts(opts)
			if err == nil {
				t.Fatalf("the flags were accepted as %+v", options)
			}
			if !strings.Contains(err.Error(), c.expected) {
				t.Fatalf("error = %q, expected %q", err, c.expected)
			}
		})
	}
}

// A borrowed site for the splice dial seam: a loopback tls server with its own
// leaf, so a prober can tell it from the extender, and a switch that makes it
// unreachable. Stops with the test.
type testCamouflageSite struct {
	address     string
	leaf        []byte
	unreachable atomic.Bool
	dialCount   atomic.Int64
}

func newTestCamouflageSite(t *testing.T) *testCamouflageSite {
	t.Helper()
	certificate, _ := testSiteCertificate(t, testCamouflageBorrowName)
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{certificate}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				conn.(*tls.Conn).Handshake()
				io.Copy(io.Discard, conn)
			}()
		}
	}()
	return &testCamouflageSite{address: listener.Addr().String(), leaf: certificate.Leaf.Raw}
}

// Implements the splice dial seam: every borrowed name is this site.
func (self *testCamouflageSite) dialContext(ctx context.Context, network string, address string) (net.Conn, error) {
	self.dialCount.Add(1)
	if self.unreachable.Load() {
		return nil, fmt.Errorf("the site is unreachable")
	}
	return (&net.Dialer{}).DialContext(ctx, "tcp", self.address)
}

// Installs one measured list as the only bundled one for the rest of the test.
func installTestCamouflageBorrowList(t *testing.T, names ...string) {
	t.Helper()
	borrowList := &connect.ExtenderBorrowList{
		Version:           1,
		Measured:          true,
		MeasuredAt:        time.Now(),
		MaxMeasurementAge: 365 * 24 * time.Hour,
	}
	for _, name := range names {
		borrowList.Names = append(borrowList.Names, connect.ExtenderBorrowName{Name: name})
	}
	t.Cleanup(connect.Testing_SetExtenderCamouflageBorrowLists([]*connect.ExtenderBorrowList{borrowList}))
}

// The clock a test moves, for the records and the gate alike.
type testCamouflageClock struct {
	stateLock sync.Mutex
	now       time.Time
}

func (self *testCamouflageClock) Now() time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.now
}

func (self *testCamouflageClock) advance(wait time.Duration) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.now = self.now.Add(wait)
}

// One running extender command on a loopback tcp carrier, and what it passed.
type testCamouflageCommand struct {
	t        *testing.T
	operator *testExtenderOperator
	run      *extenderRun
	tcpPort  int
	// the server's settings as the command passed them
	serverSettings *extender.ExtenderSettings
	cancel         context.CancelFunc
	runDone        chan error
}

// Runs the command against operator with the options configure adds to a tcp-only
// extender on one loopback listener. Stops with the test.
func startTestCamouflageCommand(
	t *testing.T,
	operator *testExtenderOperator,
	configure func(options *extenderOptions),
) *testCamouflageCommand {
	t.Helper()
	operatorAddress := operator.server.Listener.Addr().String()
	_, operatorPort, err := net.SplitHostPort(operatorAddress)
	if err != nil {
		t.Fatal(err)
	}
	tcpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tcpPort := tcpListener.Addr().(*net.TCPAddr).Port

	command := &testCamouflageCommand{
		t:        t,
		operator: operator,
		tcpPort:  tcpPort,
		runDone:  make(chan error, 1),
	}
	runs := make(chan *extenderRun, 1)
	options := &extenderOptions{
		jwt:     "test-jwt",
		apiUrl:  fmt.Sprintf("http://%s:%s", testExtenderApiHost, operatorPort),
		tcpPort: tcpPort,
		udpPort: tcpPort + 1,
		dnsPort: tcpPort + 2,
		listen: func(network string, address string) (net.Listener, error) {
			return tcpListener, nil
		},
		listenPacket: func(network string, address string) (net.PacketConn, error) {
			return nil, fmt.Errorf("this test serves tcp only")
		},
		dialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", operatorAddress)
		},
		packetConnFactory:      testLoopbackPacketConn,
		configureNetworkClient: testHermeticNetworkClient,
		configureServer: func(settings *extender.ExtenderSettings) {
			copied := *settings
			command.serverSettings = &copied
		},
		onStart: func(run *extenderRun) {
			runs <- run
		},
	}
	configure(options)

	ctx, cancel := context.WithCancel(context.Background())
	command.cancel = cancel
	go func() {
		command.runDone <- runExtender(ctx, options)
	}()
	t.Cleanup(command.stop)
	select {
	case command.run = <-runs:
	case err := <-command.runDone:
		t.Fatalf("the extender exited before it started: %v", err)
	case <-time.After(60 * time.Second):
		t.Fatal("the extender did not start")
	}
	return command
}

// Stops the command and waits for it. Safe to call twice.
func (self *testCamouflageCommand) stop() {
	self.t.Helper()
	self.cancel()
	select {
	case err := <-self.runDone:
		if err != nil {
			self.t.Errorf("the extender stopped with %v", err)
		}
		self.runDone <- nil
	case <-time.After(60 * time.Second):
		self.t.Error("the extender did not stop")
	}
}

// The next activation the operator saw.
func (self *testCamouflageCommand) waitActivation() *connect.ExtenderActivateArgs {
	self.t.Helper()
	select {
	case args := <-self.operator.activations:
		return args
	case <-time.After(60 * time.Second):
		self.t.Fatal("the extender did not activate")
		return nil
	}
}

// Waits until a condition of the running command holds.
func (self *testCamouflageCommand) waitUntil(name string, condition func() bool) {
	self.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for !condition() {
		if deadline.Before(time.Now()) {
			self.t.Fatalf("%s never held: %+v", name, self.run.server.CamouflageStatus())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A raw prober's tls handshake with the carrier: the leaf it was shown. The chain
// is not checked, which is what a prober that wants to see what answers does.
func (self *testCamouflageCommand) probeLeaf() []byte {
	self.t.Helper()
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 60 * time.Second}, "tcp", fmt.Sprintf("127.0.0.1:%d", self.tcpPort), &tls.Config{
		ServerName:         testCamouflageBorrowName,
		InsecureSkipVerify: true,
	})
	if err != nil {
		self.t.Fatalf("the prober's handshake failed: %v", err)
	}
	defer conn.Close()
	return conn.ConnectionState().PeerCertificates[0].Raw
}

// Writes a seed to a key file only the test user can read, and returns the file
// and what the seed derives.
func writeTestExtenderKeyFile(t *testing.T) (keyFile string, identityPublicKey []byte, realityPublicKey []byte) {
	t.Helper()
	seed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	keyFile = filepath.Join(t.TempDir(), "extender.key")
	if err := os.WriteFile(keyFile, []byte(connect.ExtenderKeySeedHex(seed)), 0600); err != nil {
		t.Fatal(err)
	}
	if identityPublicKey, err = connect.ExtenderPublicKeyFromSeed(seed); err != nil {
		t.Fatal(err)
	}
	if realityPublicKey, err = connect.ExtenderRealityStaticPublicKey(seed); err != nil {
		t.Fatal(err)
	}
	return keyFile, identityPublicKey, realityPublicKey
}

// A v4 host, whatever this machine is, so the verification dials one family.
func testCamouflageV4Host(settings *extender.ExtenderCamouflageRoleSettings) {
	settings.IpVersionSupported = func(ipVersion int) bool { return ipVersion == 4 }
}

// A host that gives no flag runs what it ran: the server is passed none of the
// camouflage, no key is published and nothing is verified, whatever list is
// bundled.
func TestExtenderCommandWithoutCamouflagePassesNoneOfIt(t *testing.T) {
	installTestCamouflageBorrowList(t, testCamouflageBorrowName)
	operator := newTestExtenderOperator(t)
	keyFile, _, _ := writeTestExtenderKeyFile(t)
	command := startTestCamouflageCommand(t, operator, func(options *extenderOptions) {
		options.keyFile = keyFile
	})

	if command.run.camouflage != nil {
		t.Fatal("the command built a camouflage role")
	}
	settings := command.serverSettings
	if settings.CamouflageEnabled || settings.ExtenderCamouflageSplice {
		t.Fatalf("the server was passed camouflage %t, splice %t", settings.CamouflageEnabled, settings.ExtenderCamouflageSplice)
	}
	if settings.CamouflageSpliceGate != nil {
		t.Fatal("the server was passed a splice gate")
	}
	if 0 < len(settings.CamouflageBorrowNames) {
		t.Fatalf("the server was passed names %v", settings.CamouflageBorrowNames)
	}
	if key := command.run.server.RealityPublicKey(); key != nil {
		t.Fatal("the server publishes a key")
	}
	if status := command.run.server.CamouflageStatus(); status.Phase != extender.ExtenderCamouflagePhaseOff || status.Covered {
		t.Fatalf("status = %+v, expected off", status)
	}
	if args := command.waitActivation(); args.RealityPublicKeyHex != "" {
		t.Fatalf("the activation posted the key %q", args.RealityPublicKeyHex)
	}
}

// --camouflage is Phase A: the server is passed the camouflage and the gate with
// the posture off, the names verify over the splice dial and reach the running
// server, the activation posts the very key the server opens tags under, an
// authenticated hello takes that path, a legacy one is still served, and the log
// says where it stands without saying the key.
func TestExtenderCommandCamouflagePhaseAPublishesTheKeyItOpensTagsUnder(t *testing.T) {
	logBuffer := captureTestExtenderLog(t)
	installTestCamouflageBorrowList(t, testCamouflageBorrowName)
	site := newTestCamouflageSite(t)
	operator := newTestExtenderOperator(t)
	keyFile, identityPublicKey, realityPublicKey := writeTestExtenderKeyFile(t)
	command := startTestCamouflageCommand(t, operator, func(options *extenderOptions) {
		options.keyFile = keyFile
		options.camouflageEnabled = true
		options.admissionLogTimeout = 5 * time.Millisecond
		options.configureCamouflage = func(settings *extender.ExtenderCamouflageRoleSettings) {
			testCamouflageV4Host(settings)
			settings.SpliceDialContext = site.dialContext
		}
	})

	settings := command.serverSettings
	if !settings.CamouflageEnabled {
		t.Fatal("the server was not passed the camouflage")
	}
	if settings.ExtenderCamouflageSplice {
		t.Fatal("the server was passed Phase B")
	}
	if settings.CamouflageSpliceGate == nil {
		t.Fatal("the server was not passed the gate")
	}
	if 0 < len(settings.CamouflageBorrowNames) {
		t.Fatalf("the server was passed names %v before they verified", settings.CamouflageBorrowNames)
	}
	server := command.run.server
	if hex.EncodeToString(server.RealityPublicKey()) != hex.EncodeToString(realityPublicKey) {
		t.Fatal("the server's key is not the one the identity derives")
	}
	if args := command.waitActivation(); args.RealityPublicKeyHex != hex.EncodeToString(realityPublicKey) {
		t.Fatalf("the activation posted %q, expected the server's key", args.RealityPublicKeyHex)
	}
	if site.dialCount.Load() == 0 {
		t.Fatal("the verification did not dial the site")
	}

	status := server.CamouflageStatus()
	if status.Phase != extender.ExtenderCamouflagePhaseAuthenticatedRollout ||
		status.DegradedReason != extender.ExtenderCamouflageDegradedSpliceOff ||
		status.VerifiedTargetCount != 1 ||
		status.Covered {
		t.Fatalf("status = %+v, expected Phase A with the name verified and no coverage", status)
	}

	// the client's race: an authenticated hello, then a legacy one for a client
	// with no key, which Phase A exists to keep serving
	feed := func(clientRealityPublicKey []byte) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		stream, err := connect.DialExtenderFeed(
			ctx,
			connect.DefaultConnectSettings(),
			&connect.ExtenderConfig{
				Profile: connect.ExtenderProfile{
					ConnectMode: connect.ExtenderConnectModeTcpTls,
					Port:        command.tcpPort,
				},
				Ip:               netip.MustParseAddr("127.0.0.1"),
				PublicKey:        identityPublicKey,
				RealityPublicKey: clientRealityPublicKey,
			},
			&protocol.ExtenderFeedRequest{SampleCount: 1},
		)
		if err != nil {
			t.Fatal(err)
		}
		stream.Close()
	}
	// the legacy attempt of the race can win on a slow machine and cancel the
	// camouflaged one before it is read, so this asks until one is
	command.waitUntil("an authenticated hello", func() bool {
		feed(realityPublicKey)
		return 0 < server.CamouflageStats().AuthenticatedCount
	})
	terminatedCount := server.CamouflageStats().TerminatedCount
	feed(nil)
	if count := server.CamouflageStats().TerminatedCount; count != terminatedCount+1 {
		t.Fatalf("terminated hellos went from %d to %d, expected the legacy client's", terminatedCount, count)
	}
	if leaf := command.probeLeaf(); slices.Equal(leaf, site.leaf) {
		t.Fatal("Phase A spliced a prober")
	}

	line := "extender camouflage: authenticated_rollout, not covered: splice_off; 1 verified names, 0 failed verifications"
	command.waitUntil("the status line", func() bool {
		return strings.Contains(logBuffer.String(), line)
	})
	if logs := logBuffer.String(); strings.Contains(logs, hex.EncodeToString(realityPublicKey)) {
		t.Fatal("the log carries the camouflage key")
	}
}

// The key derives from the identity, so a restart on the kept file posts the same
// key, by --extender_key_file and by --state_dir alike, and another identity posts
// another. No second file appears beside the identity's.
func TestExtenderCommandCamouflageKeyIsStableAcrossRestarts(t *testing.T) {
	installTestCamouflageBorrowList(t, testCamouflageBorrowName)
	site := newTestCamouflageSite(t)
	postedKey := func(configure func(options *extenderOptions)) string {
		t.Helper()
		operator := newTestExtenderOperator(t)
		command := startTestCamouflageCommand(t, operator, func(options *extenderOptions) {
			options.camouflageEnabled = true
			options.configureCamouflage = func(settings *extender.ExtenderCamouflageRoleSettings) {
				testCamouflageV4Host(settings)
				settings.SpliceDialContext = site.dialContext
			}
			configure(options)
		})
		defer command.stop()
		key := command.waitActivation().RealityPublicKeyHex
		if key == "" {
			t.Fatal("the activation posted no key")
		}
		return key
	}

	keyFile, _, realityPublicKey := writeTestExtenderKeyFile(t)
	byKeyFile := func(options *extenderOptions) { options.keyFile = keyFile }
	first := postedKey(byKeyFile)
	second := postedKey(byKeyFile)
	if first != hex.EncodeToString(realityPublicKey) || second != first {
		t.Fatalf("keys across a restart = %q, %q", first, second)
	}
	entries, err := os.ReadDir(filepath.Dir(keyFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), "camouflage") || strings.Contains(entry.Name(), "reality") {
			t.Fatalf("a key file of its own appeared: %s", entry.Name())
		}
	}

	stateDir := t.TempDir()
	byStateDir := func(options *extenderOptions) { options.stateDir = stateDir }
	created := postedKey(byStateDir)
	if again := postedKey(byStateDir); again != created {
		t.Fatalf("keys across a restart on the state dir = %q, %q", created, again)
	}
	if created == first {
		t.Fatal("another identity posted the same key")
	}
}

// An empty list is no coverage and no error: the command starts, activates with no
// key, dials nothing, and says no_verified_target. So is a list none of whose names
// verifies, whose failures are counted.
func TestExtenderCommandCamouflageWithNoVerifiedNameIsNoCoverageNotAnError(t *testing.T) {
	cases := []struct {
		name             string
		names            []string
		unreachable      bool
		splice           bool
		expectedDials    bool
		expectedFailures bool
	}{
		{name: "an empty list"},
		{name: "an empty list in phase b", splice: true},
		{
			name:             "a list that does not verify",
			names:            []string{testCamouflageBorrowName},
			unreachable:      true,
			expectedDials:    true,
			expectedFailures: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			logBuffer := captureTestExtenderLog(t)
			installTestCamouflageBorrowList(t, c.names...)
			site := newTestCamouflageSite(t)
			site.unreachable.Store(c.unreachable)
			operator := newTestExtenderOperator(t)
			keyFile, _, _ := writeTestExtenderKeyFile(t)
			command := startTestCamouflageCommand(t, operator, func(options *extenderOptions) {
				options.keyFile = keyFile
				options.camouflageEnabled = true
				options.camouflageSplice = c.splice
				options.admissionLogTimeout = 5 * time.Millisecond
				options.configureCamouflage = func(settings *extender.ExtenderCamouflageRoleSettings) {
					testCamouflageV4Host(settings)
					settings.SpliceDialContext = site.dialContext
				}
			})
			if args := command.waitActivation(); args.RealityPublicKeyHex != "" {
				t.Fatalf("the activation posted a key with no verified name: %q", args.RealityPublicKeyHex)
			}
			status := command.run.server.CamouflageStatus()
			if !status.Enabled || status.Covered || status.VerifiedTargetCount != 0 ||
				status.DegradedReason != extender.ExtenderCamouflageDegradedNoVerifiedTarget {
				t.Fatalf("status = %+v, expected no_verified_target", status)
			}
			if dialed := 0 < site.dialCount.Load(); dialed != c.expectedDials {
				t.Fatalf("the splice dial ran %d times", site.dialCount.Load())
			}
			failures := command.run.server.CamouflageStats().BorrowVerifyFailCount
			if counted := 0 < failures; counted != c.expectedFailures {
				t.Fatalf("failed verifications = %d", failures)
			}
			command.waitUntil("the status line", func() bool {
				return strings.Contains(logBuffer.String(), "not covered: no_verified_target; 0 verified names")
			})
		})
	}
}

// A host that opts in and cannot keep its key fails at start, telling it why,
// before it binds a carrier: an identity with nowhere to be kept, and a kept one
// that does not read. Without the option the ephemeral identity stands as it did.
func TestExtenderCommandCamouflageRefusesAnIdentityItCannotKeep(t *testing.T) {
	cases := []struct {
		name     string
		content  string
		noFile   bool
		expected []string
	}{
		{
			name:     "an ephemeral identity",
			noFile:   true,
			expected: []string{"--camouflage needs a kept identity", "--extender_key_file", "--state_dir"},
		},
		{
			name:     "a key file that is not hex",
			content:  "not a key",
			expected: []string{"--camouflage needs a usable identity key"},
		},
		{
			name:     "a key file of the wrong size",
			content:  "abcd",
			expected: []string{"--camouflage needs a usable identity key", "must be 32 bytes"},
		},
		{
			name:     "an empty key file",
			content:  "",
			expected: []string{"--camouflage needs a usable identity key"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var bound atomic.Bool
			options := &extenderOptions{
				jwt:     "test-jwt",
				apiUrl:  testOptionsApiUrl,
				tcpPort: 1,
				udpPort: 2,
				dnsPort: 3,
				listen: func(network string, address string) (net.Listener, error) {
					bound.Store(true)
					return nil, fmt.Errorf("this test binds nothing")
				},
				listenPacket: func(network string, address string) (net.PacketConn, error) {
					bound.Store(true)
					return nil, fmt.Errorf("this test binds nothing")
				},
				dialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
					return nil, fmt.Errorf("this test dials nothing")
				},
				packetConnFactory:      testLoopbackPacketConn,
				configureNetworkClient: testHermeticNetworkClient,
				camouflageEnabled:      true,
			}
			if !c.noFile {
				options.keyFile = filepath.Join(t.TempDir(), "extender.key")
				if err := os.WriteFile(options.keyFile, []byte(c.content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			err := runExtender(context.Background(), options)
			if err == nil {
				t.Fatal("the command started")
			}
			for _, expected := range c.expected {
				if !strings.Contains(err.Error(), expected) {
					t.Errorf("error = %q, expected %q", err, expected)
				}
			}
			if bound.Load() {
				t.Fatal("the command bound a carrier before it refused")
			}
		})
	}
}

// The safe flip in the command: with --camouflage_splice a prober is terminated
// while the records settle, the gate opens one lifetime and the margin after the
// first record that carries the key and the requirement, a prober then meets the
// borrowed site and the status calls it coverage, and the requirement leaving the
// record closes the gate at once.
func TestExtenderCommandCamouflageSpliceWaitsForItsRecordsToSettle(t *testing.T) {
	installTestCamouflageBorrowList(t, testCamouflageBorrowName)
	site := newTestCamouflageSite(t)
	clock := &testCamouflageClock{now: time.Now()}
	const recordLifetime = 24 * time.Hour
	margin := extender.DefaultExtenderCamouflageSpliceSettleSettings().Margin

	operator := newTestExtenderOperator(t)
	operator.signCamouflageKey = true
	operator.requireCamouflage = true
	operator.now = clock.Now
	operator.recordLifetime = recordLifetime
	keyFile, _, realityPublicKey := writeTestExtenderKeyFile(t)
	command := startTestCamouflageCommand(t, operator, func(options *extenderOptions) {
		options.keyFile = keyFile
		options.camouflageEnabled = true
		options.camouflageSplice = true
		options.configureCamouflage = func(settings *extender.ExtenderCamouflageRoleSettings) {
			testCamouflageV4Host(settings)
			settings.Now = clock.Now
			settings.SpliceDialContext = site.dialContext
		}
	})
	if !command.serverSettings.ExtenderCamouflageSplice {
		t.Fatal("the server was not passed Phase B")
	}
	server := command.run.server
	if args := command.waitActivation(); args.RealityPublicKeyHex != hex.EncodeToString(realityPublicKey) {
		t.Fatalf("the activation posted %q", args.RealityPublicKeyHex)
	}
	if leaf := command.probeLeaf(); slices.Equal(leaf, site.leaf) {
		t.Fatal("a prober was spliced before the records settled")
	}
	status := server.CamouflageStatus()
	if status.Covered || status.Phase != extender.ExtenderCamouflagePhaseAuthenticatedRollout ||
		status.DegradedReason != extender.ExtenderCamouflageDegradedGateClosed {
		t.Fatalf("status before the records settled = %+v", status)
	}

	// each activation renews the record, and the gate reads each one. The first
	// record reaches the gate just after the operator answers it, so the run may
	// start a step after this; it can never start before
	renew := func() {
		t.Helper()
		version, _ := command.run.activator.ChangeMonitor().Get()
		command.run.activator.Reactivate()
		command.waitUntil("the renewal", func() bool {
			current, _ := command.run.activator.ChangeMonitor().Get()
			return version < current
		})
	}
	const step = 6 * time.Hour
	firstRecord := clock.Now()
	for !server.CamouflageStatus().SpliceGateOpen {
		if recordLifetime+margin+3*step < clock.Now().Sub(firstRecord) {
			t.Fatalf("the gate is still closed after %s: %+v", clock.Now().Sub(firstRecord), server.CamouflageStatus())
		}
		clock.advance(step)
		renew()
	}
	if elapsed := clock.Now().Sub(firstRecord); elapsed < recordLifetime+margin {
		t.Fatalf("the gate opened after %s, expected a lifetime and the margin", elapsed)
	}
	if leaf := command.probeLeaf(); !slices.Equal(leaf, site.leaf) {
		t.Fatal("a prober was not spliced after the records settled")
	}
	status = server.CamouflageStatus()
	if !status.Covered || status.Phase != extender.ExtenderCamouflagePhaseSplicePilot {
		t.Fatalf("status after the records settled = %+v", status)
	}

	// the cohort rollback reaching the extender through the record
	func() {
		operator.stateLock.Lock()
		defer operator.stateLock.Unlock()
		operator.requireCamouflage = false
	}()
	renew()
	command.waitUntil("the gate closing", func() bool {
		return !server.CamouflageStatus().SpliceGateOpen
	})
	if leaf := command.probeLeaf(); slices.Equal(leaf, site.leaf) {
		t.Fatal("a prober was spliced after the requirement left the record")
	}
}

// A name that verifies after the first post went out with no key is posted at once,
// and the server takes it: a host that started before its network was up recovers
// without a restart. The first wait is cut short so the post goes out first.
func TestExtenderCommandCamouflagePostsTheKeyWhenANameVerifiesLater(t *testing.T) {
	installTestCamouflageBorrowList(t, testCamouflageBorrowName)
	site := newTestCamouflageSite(t)
	site.unreachable.Store(true)
	retries := make(chan time.Time)
	operator := newTestExtenderOperator(t)
	keyFile, _, realityPublicKey := writeTestExtenderKeyFile(t)
	command := startTestCamouflageCommand(t, operator, func(options *extenderOptions) {
		options.keyFile = keyFile
		options.camouflageEnabled = true
		options.configureCamouflage = func(settings *extender.ExtenderCamouflageRoleSettings) {
			testCamouflageV4Host(settings)
			settings.SpliceDialContext = site.dialContext
			settings.After = func(wait time.Duration) <-chan time.Time {
				return retries
			}
		}
	})
	if args := command.waitActivation(); args.RealityPublicKeyHex != "" {
		t.Fatalf("the first post carried %q with no verified name", args.RealityPublicKeyHex)
	}

	site.unreachable.Store(false)
	select {
	case retries <- time.Now():
	case <-time.After(60 * time.Second):
		t.Fatal("the role is not waiting to retry")
	}
	deadline := time.After(60 * time.Second)
	for {
		select {
		case args := <-operator.activations:
			if args.RealityPublicKeyHex == hex.EncodeToString(realityPublicKey) {
				if count := command.run.server.CamouflageStatus().VerifiedTargetCount; count != 1 {
					t.Fatalf("verified names = %d, expected the one the post is for", count)
				}
				return
			}
		case <-deadline:
			t.Fatal("no post carried the key after the name verified")
		}
	}
}

// --camouflage_revalidate_interval is how long the next pass waits, and with no flag
// it is the role's default: the wait is the interval, less at most a tenth.
func TestExtenderCommandCamouflageRevalidatesAtItsInterval(t *testing.T) {
	installTestCamouflageBorrowList(t, testCamouflageBorrowName)
	site := newTestCamouflageSite(t)
	cases := []struct {
		name     string
		interval time.Duration
		expected time.Duration
	}{
		{name: "the flag", interval: 12 * time.Hour, expected: 12 * time.Hour},
		{name: "the default", expected: extender.DefaultExtenderCamouflageRevalidationInterval},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			waits := make(chan time.Duration, 16)
			keyFile, _, _ := writeTestExtenderKeyFile(t)
			command := startTestCamouflageCommand(t, newTestExtenderOperator(t), func(options *extenderOptions) {
				options.keyFile = keyFile
				options.camouflageEnabled = true
				options.camouflageRevalidationInterval = c.interval
				options.configureCamouflage = func(settings *extender.ExtenderCamouflageRoleSettings) {
					testCamouflageV4Host(settings)
					settings.SpliceDialContext = site.dialContext
					settings.After = func(wait time.Duration) <-chan time.Time {
						waits <- wait
						return make(chan time.Time)
					}
				}
			})
			defer command.stop()
			select {
			case wait := <-waits:
				if c.expected < wait || wait < c.expected-c.expected/10 {
					t.Fatalf("wait = %s, expected within a tenth under %s", wait, c.expected)
				}
			case <-time.After(60 * time.Second):
				t.Fatal("the role did not finish a pass")
			}
		})
	}
}

// The command waits for a verification in flight when it stops, past the cancel that
// ends it, so nothing dials through a server that has gone.
func TestExtenderCommandWaitsForAVerificationInFlightWhenItStops(t *testing.T) {
	installTestCamouflageBorrowList(t, testCamouflageBorrowName)
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	keyFile, _, _ := writeTestExtenderKeyFile(t)
	command := startTestCamouflageCommand(t, newTestExtenderOperator(t), func(options *extenderOptions) {
		options.keyFile = keyFile
		options.camouflageEnabled = true
		options.configureCamouflage = func(settings *extender.ExtenderCamouflageRoleSettings) {
			testCamouflageV4Host(settings)
			settings.FirstVerificationTimeout = time.Millisecond
			settings.VerifyBorrowName = func(ctx context.Context, network string, borrowName string) bool {
				once.Do(func() {
					close(entered)
					<-ctx.Done()
					<-release
				})
				return false
			}
		}
	})
	select {
	case <-entered:
	case <-time.After(60 * time.Second):
		t.Fatal("the verification never ran")
	}
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		command.stop()
	}()
	select {
	case <-stopped:
		t.Fatal("the command stopped with the verification in flight")
	case <-time.After(500 * time.Millisecond):
	}
	close(release)
	select {
	case <-stopped:
	case <-time.After(60 * time.Second):
		t.Fatal("the command did not stop once the verification ended")
	}
}

// The command reads the operator's clock from its own api, on the family hosts as
// well (net_extender_camouflage_clock.go).
func TestExtenderCommandNamesItsApiHostsForTheCamouflageClock(t *testing.T) {
	operator := newTestExtenderOperator(t)
	keyFile, _, _ := writeTestExtenderKeyFile(t)
	command := startTestCamouflageCommand(t, operator, func(options *extenderOptions) {
		options.keyFile = keyFile
	})
	expected := []string{
		testExtenderApiHost,
		"api-v4." + testExtenderNetworkHost,
		"api-v6." + testExtenderNetworkHost,
	}
	if hosts := command.run.clientStrategy.PlatformApiHosts(); !slices.Equal(hosts, expected) {
		t.Fatalf("hosts = %v, expected %v", hosts, expected)
	}
}

// A hop's camouflage key arrives on the hop's config, beside the identity key its
// leaf check needs, and the log says the hop is camouflaged without saying the key.
func TestExtenderHopSpecTakesTheCamouflageKey(t *testing.T) {
	_, _, realityPublicKey := writeTestExtenderKeyFile(t)
	realityPublicKeyHex := hex.EncodeToString(realityPublicKey)
	hop, err := parseExtenderHopSpec(fmt.Sprintf(
		"tcp://192.0.2.1?camouflage_key=%s&key=%s", realityPublicKeyHex, testHopPublicKeyHex,
	))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(hop.RealityPublicKey, realityPublicKey) {
		t.Fatalf("camouflage key = %x", hop.RealityPublicKey)
	}
	if hex.EncodeToString(hop.PublicKey) != testHopPublicKeyHex {
		t.Fatalf("identity key = %x", hop.PublicKey)
	}
	description := extenderHopDescription(hop)
	if !strings.Contains(description, "camouflaged") || strings.Contains(description, realityPublicKeyHex) {
		t.Fatalf("description = %q", description)
	}

	// a hop without the parameter is what it was
	hop, err = parseExtenderHopSpec("tcp://192.0.2.1?key=" + testHopPublicKeyHex)
	if err != nil {
		t.Fatal(err)
	}
	if 0 < len(hop.RealityPublicKey) || strings.Contains(extenderHopDescription(hop), "camouflaged") {
		t.Fatalf("a hop with no parameter is camouflaged: %+v", hop)
	}
}

// The parameter belongs to the tcp carrier and needs the identity key, and a value
// that is not a usable key is refused, each with a message that says which.
func TestExtenderHopSpecRefusesACamouflageKeyItCannotUse(t *testing.T) {
	_, _, realityPublicKey := writeTestExtenderKeyFile(t)
	realityPublicKeyHex := hex.EncodeToString(realityPublicKey)
	cases := []struct {
		name     string
		spec     string
		expected string
	}{
		{
			name:     "the quic carrier",
			spec:     fmt.Sprintf("quic://192.0.2.1?key=%s&camouflage_key=%s", testHopPublicKeyHex, realityPublicKeyHex),
			expected: "tcp carrier only",
		},
		{
			name:     "the dns carrier",
			spec:     fmt.Sprintf("dns://192.0.2.1?key=%s&camouflage_key=%s", testHopPublicKeyHex, realityPublicKeyHex),
			expected: "tcp carrier only",
		},
		{
			name:     "no identity key",
			spec:     fmt.Sprintf("tcp://192.0.2.1?camouflage_key=%s", realityPublicKeyHex),
			expected: "camouflage_key needs key",
		},
		{
			name:     "not hex",
			spec:     fmt.Sprintf("tcp://192.0.2.1?key=%s&camouflage_key=nothex", testHopPublicKeyHex),
			expected: "camouflage_key:",
		},
		{
			name:     "the wrong size",
			spec:     fmt.Sprintf("tcp://192.0.2.1?key=%s&camouflage_key=abcd", testHopPublicKeyHex),
			expected: "camouflage_key:",
		},
		{
			name:     "given twice",
			spec:     fmt.Sprintf("tcp://192.0.2.1?key=%s&camouflage_key=%s&camouflage_key=%s", testHopPublicKeyHex, realityPublicKeyHex, realityPublicKeyHex),
			expected: "given 2 times",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hop, err := parseExtenderHopSpec(c.spec)
			if err == nil {
				t.Fatalf("the spec was accepted as %+v", hop)
			}
			if !strings.Contains(err.Error(), c.expected) {
				t.Fatalf("error = %q, expected %q", err, c.expected)
			}
		})
	}
}

// What the parameter is for: a hop spec with the camouflage key reaches an extender
// that splices every other hello, and the same spec without it is spliced away.
func TestExtenderHopSpecReachesASplicingHopWithItsCamouflageKey(t *testing.T) {
	installTestCamouflageBorrowList(t, testCamouflageBorrowName)
	site := newTestCamouflageSite(t)
	seed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	identityPublicKey, err := connect.ExtenderPublicKeyFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	realityPublicKey, err := connect.ExtenderRealityStaticPublicKey(seed)
	if err != nil {
		t.Fatal(err)
	}

	siteAddress, _ := startTestSite(t, "through the hop")
	ctx, cancel := context.WithCancel(context.Background())
	hopListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hopPort := hopListener.Addr().(*net.TCPAddr).Port
	hopSettings := extender.DefaultExtenderSettings()
	hopSettings.IdentityKeySeed = seed
	hopSettings.CamouflageEnabled = true
	hopSettings.ExtenderCamouflageSplice = true
	hopSettings.CamouflageBorrowNames = []string{testCamouflageBorrowName}
	hopSettings.CamouflageSpliceDialContext = site.dialContext
	hopSettings.Listen = func(network string, address string) (net.Listener, error) {
		return hopListener, nil
	}
	hopSettings.DialContext = func(ctx context.Context, network string, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", siteAddress)
	}
	hopServer := extender.NewExtenderServer(
		ctx,
		nil,
		[]string{testExtenderApiHost},
		map[int][]connect.ExtenderConnectMode{hopPort: {connect.ExtenderConnectModeTcpTls}},
		&net.Dialer{},
		hopSettings,
	)
	hopDone := make(chan error, 1)
	go func() { hopDone <- hopServer.ListenAndServe() }()
	t.Cleanup(func() {
		hopServer.CloseAndWait()
		cancel()
		<-hopDone
	})
	select {
	case <-hopServer.Listening():
	case <-time.After(30 * time.Second):
		t.Fatal("the hop did not bind")
	}

	dial := func(spec string) error {
		t.Helper()
		hop, err := parseExtenderHopSpec(spec)
		if err != nil {
			t.Fatal(err)
		}
		dialCtx, dialCancel := context.WithTimeout(ctx, 30*time.Second)
		defer dialCancel()
		conn, _, err := connect.DialExtender(dialCtx, connect.DefaultConnectSettings(), hop, &connect.ExtenderDial{
			DestinationHost: testExtenderApiHost,
			DestinationPort: 443,
			Service:         connect.ExtenderServiceForward,
		})
		if err != nil {
			return err
		}
		conn.Close()
		return nil
	}

	withoutKey := fmt.Sprintf("tcp://127.0.0.1:%d?key=%s", hopPort, hex.EncodeToString(identityPublicKey))
	if err := dial(withoutKey); err == nil {
		t.Fatal("a hop that splices answered a hop dial with no camouflage key")
	}
	if count := hopServer.CamouflageStats().AuthenticatedCount; count != 0 {
		t.Fatalf("authenticated hellos with no key = %d", count)
	}

	if err := dial(withoutKey + "&camouflage_key=" + hex.EncodeToString(realityPublicKey)); err != nil {
		t.Fatalf("the hop dial with the camouflage key failed: %v", err)
	}
	if count := hopServer.CamouflageStats().AuthenticatedCount; count != 1 {
		t.Fatalf("authenticated hellos with the key = %d, expected 1", count)
	}
}

// The command's loggers also write into a buffer for the rest of the test.
func captureTestExtenderLog(t *testing.T) *lockedLogBuffer {
	t.Helper()
	logBuffer := &lockedLogBuffer{}
	outWriter := Out.Writer()
	errWriter := Err.Writer()
	Out.SetOutput(io.MultiWriter(logBuffer, outWriter))
	Err.SetOutput(io.MultiWriter(logBuffer, errWriter))
	t.Cleanup(func() {
		Out.SetOutput(outWriter)
		Err.SetOutput(errWriter)
	})
	return logBuffer
}
