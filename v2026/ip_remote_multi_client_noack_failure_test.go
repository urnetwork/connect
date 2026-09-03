package connect

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// A direct P2P UDP packet uses Transfer NoAck. Its terminal callback is the
// initial route-write disposition, so a transient Timeout is one dropped
// datagram, not a verdict on every TCP flow sharing the provider channel.
func TestNoAckWriteTimeoutDoesNotPoisonSharedTcpClient(t *testing.T) {
	client := stallTestChannel()
	tcpBytes := ByteCount(1440)
	udpBytes := ByteCount(1000)

	client.addSend(tcpBytes, icmpTcpTestPath(4))
	client.addSend(udpBytes, udpTestPath(4))
	client.observePacketTransferCompletion(
		udpBytes,
		time.Now(),
		false,
		errTransferRouteWriteTimeout,
	)

	client.stateLock.Lock()
	defer client.stateLock.Unlock()
	if client.endErr != nil {
		t.Fatalf("NoAck timeout poisoned provider: %v", client.endErr)
	}
	if got := client.packetStats.sendNackCount; got != 1 {
		t.Fatalf("outstanding packets = %d, want only the TCP packet", got)
	}
	if got := client.packetStats.sendNackByteCount; got != tcpBytes {
		t.Fatalf("outstanding bytes = %d, want TCP bytes %d", got, tcpBytes)
	}
}

// Provider-return and coalesced reads can admit several UDP packets as one
// logical group. The same timeout boundary must retire every group member
// without resetting an unrelated reliable flow.
func TestNoAckGroupWriteTimeoutDoesNotPoisonSharedTcpClient(t *testing.T) {
	client := stallTestChannel()
	tcpBytes := ByteCount(1440)
	udpPath := udpTestPath(4)
	group := &parsedPacketGroup{
		packets: []parsedPacket{
			{ipPath: udpPath},
			{ipPath: udpPath},
		},
		ipPath:    udpPath,
		byteCount: 2100,
	}

	client.addSend(tcpBytes, icmpTcpTestPath(4))
	client.addSendGroup(group)
	client.observePacketGroupTransferCompletion(group, false, errTransferRouteWriteTimeout)

	client.stateLock.Lock()
	defer client.stateLock.Unlock()
	if client.endErr != nil {
		t.Fatalf("NoAck group timeout poisoned provider: %v", client.endErr)
	}
	if got := client.packetStats.sendNackCount; got != 1 {
		t.Fatalf("outstanding packets = %d, want only the TCP packet", got)
	}
	if got := client.packetStats.sendNackByteCount; got != tcpBytes {
		t.Fatalf("outstanding bytes = %d, want TCP bytes %d", got, tcpBytes)
	}
}

// NoAck does not make structural failures harmless. A closed sequence,
// contract failure, or encryption failure still carries provider-level
// evidence; only the typed bounded route-write timeout is packet-local.
func TestNoAckStructuralFailureStillPoisonsProvider(t *testing.T) {
	client := stallTestChannel()
	udpBytes := ByteCount(1000)
	wantErr := errors.New("Send sequence closed.")

	client.addSend(udpBytes, udpTestPath(4))
	client.observePacketTransferCompletion(udpBytes, time.Now(), false, wantErr)

	client.stateLock.Lock()
	defer client.stateLock.Unlock()
	if !errors.Is(client.endErr, wantErr) {
		t.Fatalf("provider error = %v, want %v", client.endErr, wantErr)
	}
}

// Ack-required failures retain their old meaning: Transfer exhausted reliable
// recovery, which is hard provider evidence and must still wake removal.
func TestAckFailureStillPoisonsProvider(t *testing.T) {
	client := stallTestChannel()
	tcpBytes := ByteCount(1440)
	wantErr := errors.New("Send sequence closed.")

	client.addSend(tcpBytes, icmpTcpTestPath(4))
	client.observePacketTransferCompletion(tcpBytes, time.Now(), true, wantErr)

	client.stateLock.Lock()
	defer client.stateLock.Unlock()
	if !errors.Is(client.endErr, wantErr) {
		t.Fatalf("provider error = %v, want %v", client.endErr, wantErr)
	}
	if got := client.packetStats.sendNackCount; got != 1 {
		t.Fatalf("reliable outstanding packets = %d, want 1", got)
	}
}

// Pin the production callbacks to the classifier exercised above. A helper
// that tests correctly but is bypassed by either hot path would silently
// restore the provider-wide reset.
func TestPacketTransferCallbacksUseNoAckFailureClassifier(t *testing.T) {
	source, err := readSource("ip_remote_multi_client.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		function string
		call     string
	}{
		{
			function: "func (self *multiClientChannel) SendDetailedWithAck(",
			call:     "self.observePacketTransferCompletion(packetByteCount, sendTime, ack, err)",
		},
		{
			function: "func (self *multiClientChannel) SendGroupDetailedWithAck(",
			call:     "self.observePacketGroupTransferCompletion(sendPacketGroup, ack, err)",
		},
	} {
		body, ok := functionBody(source, tc.function)
		if !ok {
			t.Fatalf("could not find %s", tc.function)
		}
		if !strings.Contains(body, tc.call) {
			t.Errorf("%s bypasses NoAck failure classifier", tc.function)
		}
	}
}
