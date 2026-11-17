// A manual provider peer drives one actual gVisor HTTP receiver outside
// synctest. This proves wire compatibility, not provider scheduling or admission.
package connect

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// Already-acknowledged one-byte probes must elicit the actual current window
// without moving the HTTP byte stream, both before and after application drain.
func TestTunTcpPersistProbeReportsWindowWithoutDeliveringJunk(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	assertMessagePoolOwnership(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	settings := DefaultTunSettingsWithBufferSize(64)
	settings.Log = NewNoopLogger()
	settings.DialRace = 1
	settings.TcpReceiveBuffer = TcpBufferRange{Min: 16 * 1024, Default: 16 * 1024, Max: 16 * 1024}
	tun, err := CreateTun(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	type dialResult struct {
		connection net.Conn
		err        error
	}
	dialed := make(chan dialResult, 1)
	dialDone := make(chan struct{})
	var connection net.Conn
	defer func() {
		cancel()
		if connection != nil {
			connection.Close()
		}
		tun.Close()
		<-dialDone
		select {
		case result := <-dialed:
			if result.connection != nil {
				result.connection.Close()
			}
		default:
		}
		tun.stack.Wait()
	}()
	go func() {
		defer close(dialDone)
		connection, err := tun.DialContext(ctx, "tcp4", "198.51.100.88:8080")
		dialed <- dialResult{connection: connection, err: err}
	}()

	packetCount := 0
	// Takes and returns an outbound pool root; only immutable test copies escape.
	decode := func(packet []byte) parsedTcp {
		t.Helper()
		defer MessagePoolReturn(packet)
		packetCount++
		if packetCount > 256 {
			t.Fatal("native peer exceeded its fixed packet observation bound")
		}
		protocol, sourceIp, destinationIp, transport, ok := parseIpv4(packet)
		var segment parsedTcp
		if !ok || protocol != ipProtocolNumberTcp || !parseTcpPacket(sourceIp, destinationIp, transport, &segment) {
			t.Fatal("native peer received a malformed or non-TCP IPv4 packet")
		}
		segment.sourceIp = bytes.Clone(segment.sourceIp)
		segment.destinationIp = bytes.Clone(segment.destinationIp)
		segment.options = bytes.Clone(segment.options)
		segment.payload = bytes.Clone(segment.payload)
		return segment
	}
	read := func() parsedTcp {
		t.Helper()
		packet, err := tun.Read()
		if err != nil {
			t.Fatal("native peer read", err)
		}
		return decode(packet)
	}
	// Borrows through Tun.Write, then returns exactly the peer's original root.
	write := func(packet []byte) {
		t.Helper()
		defer MessagePoolReturn(packet)
		if n, err := tun.Write(packet); err != nil || n != len(packet) {
			t.Fatalf("native peer injection: n=%d want=%d err=%v", n, len(packet), err)
		}
	}
	syn := read()
	if !syn.syn || syn.ack || syn.rst || syn.fin || len(syn.payload) != 0 || !syn.enableTimestamp {
		t.Fatal("native peer did not receive the ordinary timestamp-enabled SYN")
	}
	peer := ConnectionState{
		ipVersion:          4,
		sourceIp:           syn.sourceIp,
		sourcePort:         syn.sourcePort,
		destinationIp:      syn.destinationIp,
		destinationPort:    syn.destinationPort,
		sendSeq:            syn.seq + 1,
		receiveSeq:         syn.seq,
		windowSize:         64*1024 - 1,
		enableWindowScale:  syn.enableWindowScale,
		receiveWindowScale: syn.windowScale,
		enableTimestamp:    syn.enableTimestamp,
		timestampRecent:    syn.timestampValue,
	}
	synAck, err := peer.SynAck(settings.Mtu)
	if err != nil {
		t.Fatal(err)
	}
	write(synAck)
	peer.receiveSeq++
	select {
	case result := <-dialed:
		if result.err != nil {
			t.Fatal(result.err)
		}
		connection = result.connection
	case <-ctx.Done():
		t.Fatal("native HTTP dial", ctx.Err())
	}
	deadline, _ := ctx.Deadline()
	if err := connection.SetDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	// There is one flow. A pure ACK also pins its exact tuple and sequence edges.
	checkAck := func(segment parsedTcp, expectedAck uint32) {
		t.Helper()
		if !segment.sourceIp.Equal(syn.sourceIp) || !segment.destinationIp.Equal(syn.destinationIp) ||
			segment.sourcePort != syn.sourcePort || segment.destinationPort != syn.destinationPort ||
			!segment.ack || segment.syn || segment.fin || segment.rst || len(segment.payload) != 0 ||
			segment.seq != peer.sendSeq || segment.ackNumber != expectedAck || !segment.enableTimestamp {
			t.Fatalf("unexpected native ACK: seq=%d ack=%d window=%d flags=%s payload=%x; want seq=%d ack=%d",
				segment.seq, segment.ackNumber, segment.windowSize, segment.flagsString(), segment.payload, peer.sendSeq, expectedAck)
		}
		peer.timestampRecent = segment.timestampValue
	}
	waitAck := func(expectedAck uint32) parsedTcp {
		t.Helper()
		for {
			segment := read()
			if int32(segment.ackNumber-expectedAck) < 0 && segment.ack && !segment.rst && !segment.syn && !segment.fin && len(segment.payload) == 0 {
				continue
			}
			checkAck(segment, expectedAck)
			return segment
		}
	}
	handshakeAck := waitAck(peer.receiveSeq)
	initialWindow := int(uint32(handshakeAck.windowSize) << syn.windowScale)
	if initialWindow < 4096 || initialWindow > settings.TcpReceiveBuffer.Max {
		t.Fatalf("unexpected fixed-buffer native receive window %d", initialWindow)
	}
	requestBytes := []byte("GET /persist HTTP/1.1\r\nHost: persist-peer.example\r\n\r\n")
	// Reads actual native request data and advances only the manual peer's ACK.
	request := func() {
		t.Helper()
		if n, err := connection.Write(requestBytes); err != nil || n != len(requestBytes) {
			t.Fatalf("HTTP request write n=%d err=%v", n, err)
		}
		var observed []byte
		for len(observed) < len(requestBytes) {
			segment := read()
			if len(segment.payload) == 0 {
				checkAck(segment, peer.receiveSeq)
				continue
			}
			if !segment.ack || segment.syn || segment.fin || segment.rst || segment.seq != peer.sendSeq ||
				segment.ackNumber != peer.receiveSeq || len(requestBytes)-len(observed) < len(segment.payload) {
				t.Fatal("HTTP request changed native ownership or connection generation")
			}
			observed = append(observed, segment.payload...)
			peer.sendSeq += uint32(len(segment.payload))
			peer.timestampRecent = segment.timestampValue
		}
		if !bytes.Equal(observed, requestBytes) {
			t.Fatalf("native HTTP request mismatch: %q", observed)
		}
	}
	request()
	body := bytes.Repeat([]byte("0123456789abcdef"), (initialWindow+512+15)/16)[:initialWindow+512]
	response := append([]byte(fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n", len(body))), body...)
	maxPayload := settings.Mtu - Ipv4HeaderSizeWithoutExtensions - TcpHeaderSizeWithoutExtensions - tcpTimestampOptionByteCount
	if syn.enableMss {
		maxPayload = min(maxPayload, int(syn.mss)-tcpTimestampOptionByteCount)
	}
	if maxPayload <= 0 {
		t.Fatal("native MSS cannot carry a response payload")
	}
	lastAck := handshakeAck
	for offset := 0; offset < initialWindow; {
		available := int(uint32(lastAck.windowSize) << syn.windowScale)
		count := min(maxPayload, min(available, initialWindow-offset))
		if count == 0 {
			t.Fatal("native receive window closed before its advertised prefix was accepted")
		}
		write(peer.tcpPacket(tcpFlagAck, peer.receiveSeq, response[offset:offset+count]))
		peer.receiveSeq += uint32(count)
		offset += count
		lastAck = waitAck(peer.receiveSeq)
	}
	if lastAck.windowSize != 0 {
		t.Fatalf("unread actual HTTP prefix did not close native receive window: %d", lastAck.windowSize)
	}
	// The stack ACKs processed data synchronously. No reader is running; discard
	// already-published duplicate receipts before demanding a fresh probe reply.
	drainPublished := func(expectZero bool) int {
		t.Helper()
		count := 0
		for {
			packet := tun.ep.Read()
			if packet == nil {
				return count
			}
			pooled := messagePoolCopyPacketBuffer(packet)
			packet.DecRef()
			segment := decode(pooled)
			checkAck(segment, peer.receiveSeq)
			if expectZero && segment.windowSize != 0 {
				t.Fatal("native window reopened without an application read")
			}
			count++
		}
	}
	drainPublished(true)
	frontier := peer.receiveSeq
	write(peer.tcpPacket(tcpFlagAck, frontier-1, []byte{0}))
	zeroReply := waitAck(frontier)
	if zeroReply.windowSize != 0 {
		t.Fatal("zero-window probe reply did not report the current zero window")
	}
	// Read completion is after gVisor synchronously publishes its window update.
	// No goroutine or future Read can publish another update before the probe.
	prefix := make([]byte, initialWindow)
	if _, err := io.ReadFull(connection, prefix); err != nil || !bytes.Equal(prefix, response[:initialWindow]) {
		t.Fatalf("native application prefix changed: err=%v", err)
	}
	if drainPublished(false) == 0 {
		t.Fatal("actual application drain did not publish its native window update")
	}
	write(peer.tcpPacket(tcpFlagAck, frontier-1, []byte{0}))
	openReply := waitAck(frontier)
	if openReply.windowSize == 0 {
		t.Fatal("post-drain probe failed to report the reopened native window")
	}
	tail := response[initialWindow:]
	if len(tail) > int(uint32(openReply.windowSize)<<syn.windowScale) {
		t.Fatal("remaining HTTP response exceeds the actual reopened window")
	}
	for offset := 0; offset < len(tail); {
		count := min(maxPayload, len(tail)-offset)
		write(peer.tcpPacket(tcpFlagAck, peer.receiveSeq, tail[offset:offset+count]))
		peer.receiveSeq += uint32(count)
		offset += count
		waitAck(peer.receiveSeq)
	}
	reader := bufio.NewReader(io.MultiReader(bytes.NewReader(prefix), connection))
	// A second actual HTTP exchange catches any trailing junk after the body.
	readResponse := func(expectedBody []byte) {
		t.Helper()
		response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodGet})
		if err != nil {
			t.Fatal("actual HTTP response parser", err)
		}
		got, readErr := io.ReadAll(response.Body)
		closeErr := response.Body.Close()
		if response.StatusCode != http.StatusOK || response.ContentLength != int64(len(expectedBody)) ||
			readErr != nil || closeErr != nil || !bytes.Equal(got, expectedBody) {
			t.Fatalf("actual HTTP body/status mismatch: status=%d length=%d bytes=%d read=%v close=%v", response.StatusCode, response.ContentLength, len(got), readErr, closeErr)
		}
	}
	readResponse(body)
	request()
	nextBody := []byte("same-native-connection")
	nextResponse := append([]byte(fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n", len(nextBody))), nextBody...)
	write(peer.tcpPacket(tcpFlagAck, peer.receiveSeq, nextResponse))
	peer.receiveSeq += uint32(len(nextResponse))
	waitAck(peer.receiveSeq)
	readResponse(nextBody)
	if drops := tun.LinkStats().OutboundDropCount; drops != 0 {
		t.Fatalf("native output queue dropped %d observation packets", drops)
	}
	t.Logf("actual native peer: zero_window=%d reopened_window=%d unchanged_probe_ack=%d HTTP_body=%d same_flow_second_body=%d packets=%d",
		zeroReply.windowSize, openReply.windowSize, frontier, len(body), len(nextBody), packetCount)
}
