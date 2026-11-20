package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

// connectctl's extender runs G2 without a provider, so a lost carrier is the
// server's to rebuild and the activation's to hear of, and a server that ended,
// with no role to replace it, is an exit with an error (EXTENDER.md G2, G4).

// One running extender command on loopback, whose tcp listener a test can close.
type testExtenderCarrierRun struct {
	operator *testExtenderOperator
	run      *extenderRun
	listener net.Listener
	// a rebind of the tcp port waits until this is called
	releaseBind func()
	// closed when the command has returned, with what it returned
	runDone chan struct{}
	runErr  error
}

func newTestExtenderCarrierRun(t *testing.T) *testExtenderCarrierRun {
	t.Helper()
	operator := newTestExtenderOperator(t)
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
	listeners := make(chan net.Listener, 1)
	listeners <- tcpListener
	quicPacketConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dnsPacketConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	quicPort := quicPacketConn.LocalAddr().(*net.UDPAddr).Port
	dnsPort := dnsPacketConn.LocalAddr().(*net.UDPAddr).Port

	runs := make(chan *extenderRun, 1)
	holdBind := make(chan struct{})
	stateDir := t.TempDir()
	options := &extenderOptions{
		jwt:      "test-jwt",
		apiUrl:   fmt.Sprintf("http://%s:%s", testExtenderApiHost, operatorPort),
		keyFile:  filepath.Join(stateDir, "extender.key"),
		stateDir: stateDir,
		tcpPort:  tcpPort,
		udpPort:  quicPort,
		dnsPort:  dnsPort,
		listen: func(network string, address string) (net.Listener, error) {
			select {
			case listener := <-listeners:
				return listener, nil
			default:
			}
			<-holdBind
			return net.Listen("tcp", net.JoinHostPort("127.0.0.1", fmt.Sprint(tcpPort)))
		},
		listenPacket: func(network string, address string) (net.PacketConn, error) {
			if address == fmt.Sprintf(":%d", quicPort) {
				return quicPacketConn, nil
			}
			return dnsPacketConn, nil
		},
		dialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			if !strings.HasSuffix(host, testExtenderNetworkHost) {
				return nil, fmt.Errorf("unexpected dial %s %s", network, address)
			}
			return (&net.Dialer{}).DialContext(ctx, "tcp", operatorAddress)
		},
		packetConnFactory:      testLoopbackPacketConn,
		configureNetworkClient: testHermeticNetworkClient,
		configureActivator: func(settings *connect.ExtenderActivatorSettings) {
			// the return's post goes at once, which is what a test here waits on
			settings.CarrierReturnJitter = -1
		},
		onStart: func(run *extenderRun) {
			runs <- run
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	carrierRun := &testExtenderCarrierRun{
		operator: operator,
		runDone:  make(chan struct{}),
	}
	releaseBind := sync.OnceFunc(func() { close(holdBind) })
	carrierRun.releaseBind = releaseBind
	t.Cleanup(releaseBind)
	go func() {
		defer close(carrierRun.runDone)
		carrierRun.runErr = runExtender(ctx, options)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-carrierRun.runDone:
		case <-time.After(30 * time.Second):
			t.Error("the extender did not exit")
		}
	})
	select {
	case carrierRun.run = <-runs:
	case <-carrierRun.runDone:
		t.Fatalf("the extender exited before it started: %v", carrierRun.runErr)
	case <-time.After(60 * time.Second):
		t.Fatal("the extender did not start")
	}
	carrierRun.listener = tcpListener
	return carrierRun
}

// Waits for every family to stand as `accept` says, and returns no sooner.
func (self *testExtenderCarrierRun) waitFamilies(
	t *testing.T,
	what string,
	accept func(family *connect.ExtenderFamilyActivationStatus) bool,
) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		families := self.run.activator.Status().Families
		if 0 < len(families) && !slices.ContainsFunc(families, func(family *connect.ExtenderFamilyActivationStatus) bool {
			return !accept(family)
		}) {
			return
		}
		if deadline.Before(time.Now()) {
			t.Fatalf("%s: families = %+v", what, families)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The activations posted so far, which leaves none waiting.
func (self *testExtenderCarrierRun) drainActivations() int {
	count := 0
	for {
		select {
		case <-self.operator.activations:
			count += 1
		default:
			return count
		}
	}
}

func (self *testExtenderCarrierRun) waitTcpServing(t *testing.T, serving bool) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for slices.Contains(self.run.server.Carriers(), connect.ExtenderCarrierTcp) != serving {
		if deadline.Before(time.Now()) {
			t.Fatalf("tcp serving != %t, carriers = %v", serving, self.run.server.Carriers())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A revocation that lands with the listener gone posts nothing, and the listener's
// return posts at once, by way of the run loop's telling the activation.
func TestExtenderCommandPostsOnceALostTcpCarrierReturns(t *testing.T) {
	carrierRun := newTestExtenderCarrierRun(t)
	carrierRun.waitFamilies(t, "at start", func(family *connect.ExtenderFamilyActivationStatus) bool {
		return family.Activated
	})
	carrierRun.drainActivations()

	carrierRun.listener.Close()
	carrierRun.waitTcpServing(t, false)

	revocation, err := connect.SignExtenderRevocation(
		carrierRun.operator.rootPrivateKey,
		&protocol.ExtenderRevocationBody{
			PublicKey:   carrierRun.run.publicKey,
			IssueTimeMs: uint64(time.Now().Add(time.Second).UnixMilli()),
			NetworkHost: carrierRun.run.networkHost,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := carrierRun.run.directory.ApplyRevocation(revocation); err != nil {
		t.Fatal(err)
	}
	carrierRun.waitFamilies(t, "held with the listener gone", func(family *connect.ExtenderFamilyActivationStatus) bool {
		return !family.Activated && family.LastError == "the tcp carrier is not listening"
	})
	if count := carrierRun.drainActivations(); count != 0 {
		t.Fatalf("%d activations posted with the listener gone", count)
	}

	// the holds are 10 minutes and more: only the return can post in this test
	carrierRun.releaseBind()
	carrierRun.waitTcpServing(t, true)
	carrierRun.waitFamilies(t, "after the tcp carrier returned", func(family *connect.ExtenderFamilyActivationStatus) bool {
		return family.Activated
	})
	if count := carrierRun.drainActivations(); count == 0 {
		t.Fatal("no activation was posted after the tcp carrier returned")
	}
	select {
	case <-carrierRun.runDone:
		t.Fatalf("the extender exited with %v", carrierRun.runErr)
	default:
	}
}

// With no role to replace it, a server that ended is the process's end, with an
// error for whatever runs it to read.
func TestExtenderCommandExitsWithAnErrorWhenItsServerEnds(t *testing.T) {
	carrierRun := newTestExtenderCarrierRun(t)

	carrierRun.run.server.Close()
	select {
	case <-carrierRun.runDone:
		if carrierRun.runErr == nil || errors.Is(carrierRun.runErr, context.Canceled) {
			t.Fatalf("exit = %v, expected the server's end as an error", carrierRun.runErr)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the extender did not exit")
	}
}

// A carrier whose first bind failed stays down (G2), and the operator accepts no
// activation without tcp, so a run that starts with no tcp could never post and would
// never end. It says why and exits, for whatever runs it.
func TestExtenderCommandExitsWithAnErrorWhenTcpDoesNotBindAtStart(t *testing.T) {
	operator := newTestExtenderOperator(t)
	operatorAddress := operator.server.Listener.Addr().String()
	_, operatorPort, err := net.SplitHostPort(operatorAddress)
	if err != nil {
		t.Fatal(err)
	}
	quicPacketConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dnsPacketConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	quicPort := quicPacketConn.LocalAddr().(*net.UDPAddr).Port
	stateDir := t.TempDir()
	options := &extenderOptions{
		jwt:      "test-jwt",
		apiUrl:   fmt.Sprintf("http://%s:%s", testExtenderApiHost, operatorPort),
		keyFile:  filepath.Join(stateDir, "extender.key"),
		stateDir: stateDir,
		tcpPort:  quicPort,
		udpPort:  quicPort,
		dnsPort:  dnsPacketConn.LocalAddr().(*net.UDPAddr).Port,
		listen: func(network string, address string) (net.Listener, error) {
			return nil, errors.New("the test's port is in use")
		},
		listenPacket: func(network string, address string) (net.PacketConn, error) {
			if address == fmt.Sprintf(":%d", quicPort) {
				return quicPacketConn, nil
			}
			return dnsPacketConn, nil
		},
		dialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", operatorAddress)
		},
		packetConnFactory:      testLoopbackPacketConn,
		configureNetworkClient: testHermeticNetworkClient,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() {
		runDone <- runExtender(ctx, options)
	}()
	select {
	case err := <-runDone:
		if err == nil || !strings.Contains(err.Error(), "the test's port is in use") {
			t.Fatalf("exit = %v, expected the bind's error", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the extender did not exit")
	}
	select {
	case args := <-operator.activations:
		t.Fatalf("posted an activation with no tcp: %+v", args)
	default:
	}
}
