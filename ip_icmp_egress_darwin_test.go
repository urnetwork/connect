//go:build darwin

// The darwin echo backend over real loopback sockets, where every datagram
// icmp socket on the host receives every echo reply: a flow keeps a reply only
// under the identifier it chose and for a sequence number it sent and has not
// yet seen answered (QD-1159). Each test orders its echoes with the events the
// flows report, a reply kept or dropped, rather than with time.
package connect

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"

	"github.com/urnetwork/connect/protocol"
)

// What a flow under test did with one echo reply: kept it, or dropped it.
type icmpEchoTestEvent struct {
	kept           bool
	identifier     uint16
	sequenceNumber uint16
	payload        string
}

func icmpEchoKept(identifier uint16, sequenceNumber uint16, payload string) icmpEchoTestEvent {
	return icmpEchoTestEvent{kept: true, identifier: identifier, sequenceNumber: sequenceNumber, payload: payload}
}

func icmpEchoDropped(identifier uint16, sequenceNumber uint16) icmpEchoTestEvent {
	return icmpEchoTestEvent{identifier: identifier, sequenceNumber: sequenceNumber}
}

func icmpLoopbackIpForTest(ipVersion int) net.IP {
	if ipVersion == 6 {
		return net.IPv6loopback
	}
	return net.IPv4(127, 0, 0, 1).To4()
}

// Reads the egress's replies as a flow's reader goroutine does, until it
// closes, and reports each reply it keeps and each echo reply it drops. The
// egress closes at the test's end.
func readIcmpEgressForTest(t *testing.T, egress *icmpEgressConn) <-chan icmpEchoTestEvent {
	t.Helper()
	// whatever else this host echoes meanwhile is reported too
	events := make(chan icmpEchoTestEvent, 1024)
	stop := make(chan struct{})
	report := func(event icmpEchoTestEvent) {
		select {
		case events <- event:
		case <-stop:
		}
	}
	egress.afterReplyDropForTest = func(identifier uint16, sequenceNumber uint16) {
		report(icmpEchoDropped(identifier, sequenceNumber))
	}
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for {
			// no deadline: the close ends the read
			sequenceNumber, payload, err := egress.ReadEcho(time.Time{})
			if err != nil {
				return
			}
			report(icmpEchoKept(egress.identifier, sequenceNumber, string(payload)))
		}
	}()
	t.Cleanup(func() {
		close(stop)
		egress.Close()
		<-readDone
	})
	return events
}

// Waits for want among a flow's events. Any other reply kept fails the test,
// since each test expects every reply its flows keep; any other drop is an
// echo reply this host carried meanwhile.
func expectIcmpEchoTestEvent(t *testing.T, ctx context.Context, name string, events <-chan icmpEchoTestEvent, want icmpEchoTestEvent) {
	t.Helper()
	for {
		select {
		case event := <-events:
			if event == want {
				return
			}
			if event.kept {
				t.Fatalf("%s kept %+v while waiting for %+v", name, event, want)
			}
		case <-ctx.Done():
			t.Fatalf("%s: no %+v", name, want)
		}
	}
}

func writeIcmpEchoForTest(t *testing.T, egress *icmpEgressConn, sequenceNumber uint16, payload string) {
	t.Helper()
	if err := egress.WriteEcho(time.Time{}, 64, sequenceNumber, []byte(payload)); err != nil {
		t.Fatal(err)
	}
}

// A socket of another process's, echoing to the destination under whatever
// identifier it is given, as darwin lets it.
type icmpForeignEchoerForTest struct {
	t             *testing.T
	conn          *icmp.PacketConn
	ipVersion     int
	destinationIp net.IP
}

func newIcmpForeignEchoerForTest(t *testing.T, ipVersion int, destinationIp net.IP) *icmpForeignEchoerForTest {
	t.Helper()
	network := "udp4"
	if ipVersion == 6 {
		network = "udp6"
	}
	conn, err := icmp.ListenPacket(network, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		conn.Close()
	})
	return &icmpForeignEchoerForTest{
		t:             t,
		conn:          conn,
		ipVersion:     ipVersion,
		destinationIp: destinationIp,
	}
}

func (self *icmpForeignEchoerForTest) echo(identifier uint16, sequenceNumber uint16, payload string) {
	self.t.Helper()
	var messageType icmp.Type = ipv4.ICMPTypeEcho
	if self.ipVersion == 6 {
		messageType = ipv6.ICMPTypeEchoRequest
	}
	// the v4 checksum is computed here; the kernel computes the v6 one
	message, err := (&icmp.Message{
		Type: messageType,
		Body: &icmp.Echo{
			ID:   int(identifier),
			Seq:  int(sequenceNumber),
			Data: []byte(payload),
		},
	}).Marshal(nil)
	if err != nil {
		self.t.Fatal(err)
	}
	if _, err := self.conn.WriteTo(message, &net.UDPAddr{IP: self.destinationIp}); err != nil {
		self.t.Fatal(err)
	}
}

// Two flows that chose one identifier, as two processes on one Mac can, echo
// to one loopback address in turn. Each socket receives both replies, and each
// flow keeps only those to its own echoes: neither takes the other's reply,
// with its payload. Each flow's identifier set stands in for its process's.
func TestIcmpEgressFlowsSharingAnIdentifierKeepOnlyTheirOwnRepliesDarwin(t *testing.T) {
	for _, ipVersion := range []int{4, 6} {
		requireIcmpEgressVersion(t, ipVersion)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		destinationIp := icmpLoopbackIpForTest(ipVersion)
		settings := DefaultIcmpBufferSettings()
		identifier := uint16(0xa5c3)

		egressA, err := listenIcmpEgress(ipVersion, destinationIp, settings, newIcmpEchoIdentifierSetForTest(t, identifier))
		if err != nil {
			t.Fatal(err)
		}
		eventsA := readIcmpEgressForTest(t, egressA)
		egressB, err := listenIcmpEgress(ipVersion, destinationIp, settings, newIcmpEchoIdentifierSetForTest(t, identifier))
		if err != nil {
			t.Fatal(err)
		}
		eventsB := readIcmpEgressForTest(t, egressB)
		AssertEqual(t, egressA.identifier, identifier)
		AssertEqual(t, egressB.identifier, identifier)

		writeIcmpEchoForTest(t, egressA, 1, "a1")
		expectIcmpEchoTestEvent(t, ctx, fmt.Sprintf("v%d flow a", ipVersion), eventsA, icmpEchoKept(identifier, 1, "a1"))
		expectIcmpEchoTestEvent(t, ctx, fmt.Sprintf("v%d flow b", ipVersion), eventsB, icmpEchoDropped(identifier, 1))

		writeIcmpEchoForTest(t, egressB, 2, "b2")
		expectIcmpEchoTestEvent(t, ctx, fmt.Sprintf("v%d flow b", ipVersion), eventsB, icmpEchoKept(identifier, 2, "b2"))
		expectIcmpEchoTestEvent(t, ctx, fmt.Sprintf("v%d flow a", ipVersion), eventsA, icmpEchoDropped(identifier, 2))

		writeIcmpEchoForTest(t, egressA, 3, "a3")
		expectIcmpEchoTestEvent(t, ctx, fmt.Sprintf("v%d flow a", ipVersion), eventsA, icmpEchoKept(identifier, 3, "a3"))
		expectIcmpEchoTestEvent(t, ctx, fmt.Sprintf("v%d flow b", ipVersion), eventsB, icmpEchoDropped(identifier, 3))
	}
}

// A reply under the flow's identifier to an echo the flow did not send drops,
// and the flow still keeps the replies to its own.
func TestIcmpEgressDropsAReplyToAnEchoItDidNotSendDarwin(t *testing.T) {
	requireIcmpEgress(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	destinationIp := icmpLoopbackIpForTest(4)
	identifier := uint16(0xb4d2)
	egress, err := listenIcmpEgress(4, destinationIp, DefaultIcmpBufferSettings(), newIcmpEchoIdentifierSetForTest(t, identifier))
	if err != nil {
		t.Fatal(err)
	}
	events := readIcmpEgressForTest(t, egress)
	foreign := newIcmpForeignEchoerForTest(t, 4, destinationIp)

	foreign.echo(identifier, 7, "foreign")
	expectIcmpEchoTestEvent(t, ctx, "flow", events, icmpEchoDropped(identifier, 7))
	writeIcmpEchoForTest(t, egress, 1, "own")
	expectIcmpEchoTestEvent(t, ctx, "flow", events, icmpEchoKept(identifier, 1, "own"))
	// with an echo awaiting its reply, another sequence number still drops
	writeIcmpEchoForTest(t, egress, 2, "own")
	expectIcmpEchoTestEvent(t, ctx, "flow", events, icmpEchoKept(identifier, 2, "own"))
	foreign.echo(identifier, 3, "foreign")
	expectIcmpEchoTestEvent(t, ctx, "flow", events, icmpEchoDropped(identifier, 3))
}

// A second reply to an echo the flow has seen answered drops: a sequence
// number awaits as many replies as the flow sent echoes with it.
func TestIcmpEgressDropsADuplicateReplyOnceAnsweredDarwin(t *testing.T) {
	requireIcmpEgress(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	destinationIp := icmpLoopbackIpForTest(4)
	identifier := uint16(0xc3e1)
	egress, err := listenIcmpEgress(4, destinationIp, DefaultIcmpBufferSettings(), newIcmpEchoIdentifierSetForTest(t, identifier))
	if err != nil {
		t.Fatal(err)
	}
	events := readIcmpEgressForTest(t, egress)
	foreign := newIcmpForeignEchoerForTest(t, 4, destinationIp)

	writeIcmpEchoForTest(t, egress, 1, "one")
	expectIcmpEchoTestEvent(t, ctx, "flow", events, icmpEchoKept(identifier, 1, "one"))
	// the same reply again
	foreign.echo(identifier, 1, "one")
	expectIcmpEchoTestEvent(t, ctx, "flow", events, icmpEchoDropped(identifier, 1))

	writeIcmpEchoForTest(t, egress, 2, "two")
	writeIcmpEchoForTest(t, egress, 2, "two")
	expectIcmpEchoTestEvent(t, ctx, "flow", events, icmpEchoKept(identifier, 2, "two"))
	expectIcmpEchoTestEvent(t, ctx, "flow", events, icmpEchoKept(identifier, 2, "two"))
	foreign.echo(identifier, 2, "two")
	expectIcmpEchoTestEvent(t, ctx, "flow", events, icmpEchoDropped(identifier, 2))
}

// An echo the socket refused awaits no reply, so the retry of it awaits one,
// not two.
func TestIcmpEgressAwaitsNoReplyToAnEchoItCouldNotSendDarwin(t *testing.T) {
	requireIcmpEgress(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	destinationIp := icmpLoopbackIpForTest(4)
	identifier := uint16(0xd2f0)
	egress, err := listenIcmpEgress(4, destinationIp, DefaultIcmpBufferSettings(), newIcmpEchoIdentifierSetForTest(t, identifier))
	if err != nil {
		t.Fatal(err)
	}
	events := readIcmpEgressForTest(t, egress)
	foreign := newIcmpForeignEchoerForTest(t, 4, destinationIp)

	// a passed deadline fails the send before anything leaves
	if err := egress.WriteEcho(time.Now().Add(-time.Second), 64, 9, []byte("nine")); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("a send past its deadline returned %v", err)
	}
	foreign.echo(identifier, 9, "nine")
	expectIcmpEchoTestEvent(t, ctx, "flow", events, icmpEchoDropped(identifier, 9))

	writeIcmpEchoForTest(t, egress, 9, "nine")
	expectIcmpEchoTestEvent(t, ctx, "flow", events, icmpEchoKept(identifier, 9, "nine"))
	foreign.echo(identifier, 9, "nine")
	expectIcmpEchoTestEvent(t, ctx, "flow", events, icmpEchoDropped(identifier, 9))
}

// A flow whose socket the kernel bound to an identifier, as linux's are, takes
// nothing from the identifier set and awaits no sequence numbers: it keeps a
// reply by identifier and peer alone, as it always has, since that kernel
// delivers a socket only its own replies. A darwin socket handed a kernel
// identifier stands in for linux's here; ip_icmp_egress_linux_test.go checks
// linux's own.
func TestIcmpEgressWithAKernelIdentifierFiltersAsBeforeDarwin(t *testing.T) {
	requireIcmpEgress(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	destinationIp := icmpLoopbackIpForTest(4)
	conn, err := listenEgressIcmp("udp4", destinationIp)
	if err != nil {
		t.Fatal(err)
	}
	// a draw fails the test
	identifiers := newIcmpEchoIdentifierSetForTest(t)
	kernelIdentifier := uint16(0x5a3c)
	egress, err := newIcmpEgressConn(conn, 4, destinationIp, kernelIdentifier, DefaultIcmpBufferSettings(), identifiers)
	if err != nil {
		t.Fatal(err)
	}
	events := readIcmpEgressForTest(t, egress)
	AssertEqual(t, egress.identifier, kernelIdentifier)
	AssertEqual(t, egress.identifiers == nil, true)
	AssertEqual(t, egress.echoesInFlight == nil, true)
	foreign := newIcmpForeignEchoerForTest(t, 4, destinationIp)

	foreign.echo(kernelIdentifier, 7, "any")
	expectIcmpEchoTestEvent(t, ctx, "flow", events, icmpEchoKept(kernelIdentifier, 7, "any"))
	writeIcmpEchoForTest(t, egress, 1, "own")
	expectIcmpEchoTestEvent(t, ctx, "flow", events, icmpEchoKept(kernelIdentifier, 1, "own"))
	foreign.echo(kernelIdentifier+1, 8, "other")
	expectIcmpEchoTestEvent(t, ctx, "flow", events, icmpEchoDropped(kernelIdentifier+1, 8))
	egress.Close()
	AssertEqual(t, icmpEchoIdentifierHeldCount(identifiers), 0)
}

// A flow made as a provider's is holds its identifier in the process's set
// while it lives, so no other flow of the process draws it, and gives it back
// once: a second close does not release it from a later flow.
func TestIcmpEgressHoldsItsIdentifierUntilClosedDarwin(t *testing.T) {
	requireIcmpEgress(t)
	destinationIp := icmpLoopbackIpForTest(4)
	settings := DefaultIcmpBufferSettings()
	egressIcmp, err := newIcmpEgress(context.Background(), 4, destinationIp, settings)
	if err != nil {
		t.Fatal(err)
	}
	egress := egressIcmp.(*icmpEgressConn)
	AssertEqual(t, egress.identifiers == icmpEgressProcessIdentifiers, true)
	AssertEqual(t, icmpEchoIdentifierHeld(icmpEgressProcessIdentifiers, egress.identifier), true)
	egress.Close()
	AssertEqual(t, icmpEchoIdentifierHeld(icmpEgressProcessIdentifiers, egress.identifier), false)

	identifier := uint16(0x7777)
	identifiers := newIcmpEchoIdentifierSetForTest(t, identifier, identifier)
	first, err := listenIcmpEgress(4, destinationIp, settings, identifiers)
	if err != nil {
		t.Fatal(err)
	}
	first.Close()
	second, err := listenIcmpEgress(4, destinationIp, settings, identifiers)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	AssertEqual(t, second.identifier, identifier)
	first.Close()
	AssertEqual(t, icmpEchoIdentifierHeld(identifiers, identifier), true)
}

// Two providers' NATs on one Mac, each with an echo flow under one identifier,
// hand each flow's source only the replies to its own echoes, rebuilt for its
// flow: one provider's peer never receives the other's reply, which carries
// the other's payload. Each NAT draws its flows' identifiers from a set of its
// own, standing in for its process's.
func TestLocalUserNatEchoFlowsSharingAnIdentifierKeepOnlyTheirOwnRepliesDarwin(t *testing.T) {
	requireIcmpEgress(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	identifier := uint16(0xe1c3)

	type natForTest struct {
		name    string
		nat     *LocalUserNat
		source  TransferPath
		replies chan []byte
		drops   chan icmpEchoTestEvent
	}
	newNat := func(name string) *natForTest {
		n := &natForTest{
			name:    name,
			source:  SourceId(NewId()),
			replies: make(chan []byte, 16),
			drops:   make(chan icmpEchoTestEvent, 1024),
		}
		identifiers := newIcmpEchoIdentifierSet(func() (uint16, error) {
			return identifier, nil
		})
		settings := DefaultLocalUserNatSettings()
		settings.IcmpBufferSettings.newEgressForTest = func(_ context.Context, ipVersion int, destinationIp net.IP, icmpBufferSettings *IcmpBufferSettings) (icmpEgress, error) {
			egress, err := listenIcmpEgress(ipVersion, destinationIp, icmpBufferSettings, identifiers)
			if err != nil {
				return nil, err
			}
			egress.afterReplyDropForTest = func(identifier uint16, sequenceNumber uint16) {
				select {
				case n.drops <- icmpEchoDropped(identifier, sequenceNumber):
				case <-ctx.Done():
				}
			}
			return egress, nil
		}
		n.nat = NewLocalUserNat(ctx, "testIcmpEchoIds"+name, settings)
		t.Cleanup(n.nat.Close)
		unsub := n.nat.AddReceivePacketCallback(func(source TransferPath, provideMode protocol.ProvideMode, ipPath *IpPath, packet []byte) {
			// the callback borrows the packet
			select {
			case n.replies <- slices.Clone(packet):
			default:
				t.Errorf("nat %s: more replies than the test expects", name)
			}
		})
		t.Cleanup(unsub)
		return n
	}
	send := func(n *natForTest, innerIdentifier int, sequenceNumber int, payload string) {
		t.Helper()
		request := testingIcmp4EchoPacket("10.0.0.1", "127.0.0.1", innerIdentifier, sequenceNumber, []byte(payload))
		if !n.nat.SendPacket(n.source, protocol.ProvideMode_Network, request, -1) {
			t.Fatalf("nat %s did not queue the echo", n.name)
		}
	}
	expectReply := func(n *natForTest, innerIdentifier int, sequenceNumber int, payload string) {
		t.Helper()
		select {
		case packet := <-n.replies:
			replyPath, replyPayload, err := ParseIpPathWithPayload(packet)
			if err != nil {
				t.Fatalf("nat %s reply parse = %v", n.name, err)
			}
			icmpMessage := packet[Ipv4HeaderSizeWithoutExtensions:]
			replySequenceNumber := int(icmpMessage[6])<<8 | int(icmpMessage[7])
			if replyPath.DestinationPort != innerIdentifier || replySequenceNumber != sequenceNumber || string(replyPayload) != payload {
				t.Fatalf("nat %s replied id %d seq %d %q, not id %d seq %d %q", n.name,
					replyPath.DestinationPort, replySequenceNumber, replyPayload, innerIdentifier, sequenceNumber, payload)
			}
		case <-ctx.Done():
			t.Fatalf("nat %s: no reply %d", n.name, sequenceNumber)
		}
	}

	natA := newNat("a")
	natB := newNat("b")

	send(natB, 0x2222, 10, "b10")
	expectReply(natB, 0x2222, 10, "b10")
	send(natA, 0x1111, 1, "a1")
	expectReply(natA, 0x1111, 1, "a1")
	expectIcmpEchoTestEvent(t, ctx, "nat b's flow", natB.drops, icmpEchoDropped(identifier, 1))
	send(natB, 0x2222, 11, "b11")
	expectReply(natB, 0x2222, 11, "b11")
	expectIcmpEchoTestEvent(t, ctx, "nat a's flow", natA.drops, icmpEchoDropped(identifier, 11))
	// each flow's reader handed on every reply before the drop it reported
	AssertEqual(t, len(natA.replies), 0)
	AssertEqual(t, len(natB.replies), 0)
}

const icmpEchoChildEnv = "CONNECT_ICMP_ECHO_CHILD"
const icmpEchoChildLinePrefix = "icmp echo child: "

// Two processes on one Mac, each with a flow made as a provider makes one,
// echo to the loopback address in turn, and each keeps only the replies to
// its own echoes though each socket receives both. Before QD-1159 every
// process counted its identifiers up from zero, so both flows had identifier
// 1 and each kept the other's replies, payload and all, as FS-56's revoked
// subject saw across sidecartest/fault's child processes.
func TestIcmpEgressInTwoProcessesKeepOnlyTheirOwnRepliesDarwin(t *testing.T) {
	if tag := os.Getenv(icmpEchoChildEnv); tag != "" {
		runIcmpEchoChildForTest(t, tag)
		return
	}
	requireIcmpEgress(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	// after the children's cleanups, which wait for each to exit by itself
	t.Cleanup(cancel)

	childA := startIcmpEchoChildForTest(t, ctx, "a")
	childB := startIcmpEchoChildForTest(t, ctx, "b")

	childA.send(100)
	childA.expect(icmpEchoKept(childA.identifier, 100, "a-100"))
	childB.expect(icmpEchoDropped(childA.identifier, 100))
	childB.send(200)
	childB.expect(icmpEchoKept(childB.identifier, 200, "b-200"))
	childA.expect(icmpEchoDropped(childB.identifier, 200))
	childA.send(101)
	childA.expect(icmpEchoKept(childA.identifier, 101, "a-101"))
	childB.expect(icmpEchoDropped(childA.identifier, 101))
	childA.end()
	childB.end()
}

// The child's side: one flow made by newIcmpEgress, an echo for each "send
// <sequence number>" line on stdin until "end", and a line on stdout for the
// flow's identifier and for each reply it keeps or drops.
func runIcmpEchoChildForTest(t *testing.T, tag string) {
	egressIcmp, err := newIcmpEgress(context.Background(), 4, icmpLoopbackIpForTest(4), DefaultIcmpBufferSettings())
	if err != nil {
		t.Fatal(err)
	}
	egress := egressIcmp.(*icmpEgressConn)
	var stdoutLock sync.Mutex
	report := func(format string, a ...any) {
		stdoutLock.Lock()
		defer stdoutLock.Unlock()
		fmt.Printf(icmpEchoChildLinePrefix+format+"\n", a...)
	}
	egress.afterReplyDropForTest = func(identifier uint16, sequenceNumber uint16) {
		report("dropped %d %d", identifier, sequenceNumber)
	}
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for {
			sequenceNumber, payload, err := egress.ReadEcho(time.Time{})
			if err != nil {
				return
			}
			report("kept %d %d %s", egress.identifier, sequenceNumber, payload)
		}
	}()
	report("identifier %d", egress.identifier)

	lines := bufio.NewScanner(os.Stdin)
	for lines.Scan() {
		var sequenceNumber uint16
		if _, err := fmt.Sscanf(lines.Text(), "send %d", &sequenceNumber); err != nil {
			break
		}
		writeIcmpEchoForTest(t, egress, sequenceNumber, fmt.Sprintf("%s-%d", tag, sequenceNumber))
	}
	egress.Close()
	<-readDone
}

// One child process of this test binary running the child's side.
type icmpEchoChildForTest struct {
	t          *testing.T
	ctx        context.Context
	tag        string
	identifier uint16
	stdin      io.WriteCloser
	events     chan icmpEchoTestEvent
	command    *exec.Cmd
	readDone   chan struct{}

	stateLock sync.Mutex
	// the child's lines that are not its reports, for a failure
	otherLines []string
	ended      bool
}

func startIcmpEchoChildForTest(t *testing.T, ctx context.Context, tag string) *icmpEchoChildForTest {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(ctx, executable,
		"-test.run=^"+regexp.QuoteMeta(t.Name())+"$",
		"-test.count=1",
		"-test.timeout=60s",
	)
	command.Env = append(os.Environ(), icmpEchoChildEnv+"="+tag)
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	command.Stderr = command.Stdout
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	child := &icmpEchoChildForTest{
		t:        t,
		ctx:      ctx,
		tag:      tag,
		stdin:    stdin,
		events:   make(chan icmpEchoTestEvent, 1024),
		command:  command,
		readDone: make(chan struct{}),
	}
	identifiers := make(chan uint16, 1)
	go func() {
		defer close(child.readDone)
		lines := bufio.NewScanner(stdout)
		for lines.Scan() {
			line, ok := strings.CutPrefix(lines.Text(), icmpEchoChildLinePrefix)
			if !ok {
				func() {
					child.stateLock.Lock()
					defer child.stateLock.Unlock()
					child.otherLines = append(child.otherLines, lines.Text())
				}()
				continue
			}
			var identifier uint16
			var sequenceNumber uint16
			var payload string
			var event icmpEchoTestEvent
			if _, err := fmt.Sscanf(line, "identifier %d", &identifier); err == nil {
				identifiers <- identifier
				continue
			} else if _, err := fmt.Sscanf(line, "kept %d %d %s", &identifier, &sequenceNumber, &payload); err == nil {
				event = icmpEchoKept(identifier, sequenceNumber, payload)
			} else if _, err := fmt.Sscanf(line, "dropped %d %d", &identifier, &sequenceNumber); err == nil {
				event = icmpEchoDropped(identifier, sequenceNumber)
			} else {
				continue
			}
			select {
			case child.events <- event:
			case <-ctx.Done():
				return
			}
		}
	}()
	t.Cleanup(func() {
		child.stdin.Close()
		<-child.readDone
		err := child.command.Wait()
		child.stateLock.Lock()
		defer child.stateLock.Unlock()
		if child.ended && err != nil {
			t.Errorf("child %s: %v\n%s", tag, err, strings.Join(child.otherLines, "\n"))
		}
	})
	select {
	case child.identifier = <-identifiers:
	case <-ctx.Done():
		t.Fatalf("child %s reported no identifier:\n%s", tag, child.output())
	}
	return child
}

func (self *icmpEchoChildForTest) output() string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return strings.Join(self.otherLines, "\n")
}

func (self *icmpEchoChildForTest) send(sequenceNumber uint16) {
	self.t.Helper()
	if _, err := fmt.Fprintf(self.stdin, "send %d\n", sequenceNumber); err != nil {
		self.t.Fatalf("child %s: %v", self.tag, err)
	}
}

func (self *icmpEchoChildForTest) expect(want icmpEchoTestEvent) {
	self.t.Helper()
	expectIcmpEchoTestEvent(self.t, self.ctx, "child "+self.tag, self.events, want)
}

// Ends the child's flow; the cleanup then holds it to a clean exit.
func (self *icmpEchoChildForTest) end() {
	self.t.Helper()
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.ended = true
	}()
	if _, err := fmt.Fprintf(self.stdin, "end\n"); err != nil {
		self.t.Fatalf("child %s: %v", self.tag, err)
	}
}
