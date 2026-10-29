package connect

// These tests pin the delay header on the H3 carriers: the auth negotiation
// matrix against old and new peers, the header inside the DATAGRAM envelope
// and the size budgets that subtract it, and both lanes stamping, stripping
// and publishing on a live QUIC connection.

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/protocol"
)

func testTransportDelayH3Request() *protocol.Auth {
	return &protocol.Auth{
		ByJwt:              "synthetic-h3-delay-credential",
		AppVersion:         "synthetic-version",
		ClientInfo:         `{"v":1,"device_type":"synthetic"}`,
		StreamLeaseVersion: 1,
		InstanceId:         NewId().Bytes(),
	}
}

// Returns auth as it arrives after crossing the wire, which is also exactly
// what an old server's raw echo returns.
func testTransportDelayH3WireCopy(t *testing.T, auth *protocol.Auth) *protocol.Auth {
	t.Helper()
	authBytes, err := EncodeFrame(auth, DefaultProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	defer MessagePoolReturn(authBytes)
	decoded, err := DecodeFrame(authBytes)
	if err != nil {
		t.Fatal(err)
	}
	return decoded.(*protocol.Auth)
}

// New peers negotiate the header with DATAGRAM on and off, and the strict auth
// echo still validates the re-encoded response.
func TestTransportDelayH3AuthNegotiatesNewPeers(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, datagrams := range []bool{false, true} {
		request := testTransportDelayH3Request()
		SetH3DatagramAuthOffer(request, datagrams)
		SetTransportDelayH3Offer(request, DefaultTransportDelaySettings(), false)
		if request.TransportDelayVersion != TransportDelayVersion || request.TransportDelayAcceptedVersion != 0 {
			t.Fatalf("datagrams=%t offer=%d/%d", datagrams, request.TransportDelayVersion, request.TransportDelayAcceptedVersion)
		}
		serverRequest := testTransportDelayH3WireCopy(t, request)
		response, datagramsAccepted := AcceptH3DatagramAuthOffer(serverRequest, true, true, true)
		if datagramsAccepted != datagrams {
			t.Fatalf("datagrams=%t server DATAGRAM acceptance=%t", datagrams, datagramsAccepted)
		}
		if !AcceptTransportDelayH3Offer(serverRequest, response, true) ||
			response.TransportDelayAcceptedVersion != TransportDelayVersion {
			t.Fatalf("datagrams=%t server did not accept the delay offer: %d", datagrams, response.TransportDelayAcceptedVersion)
		}
		clientResponse := testTransportDelayH3WireCopy(t, response)
		if accepted, err := negotiateH3DatagramAuthResponse(request, clientResponse, datagrams, true, true); accepted != datagrams || err != nil {
			t.Fatalf("datagrams=%t DATAGRAM negotiation = (%t, %v)", datagrams, accepted, err)
		}
		if !transportDelayH3Accepted(request, clientResponse) {
			t.Fatalf("datagrams=%t client did not see the acceptance", datagrams)
		}
		if accepted, err := negotiateTransportDelayH3AuthResponse(request, clientResponse); !accepted || err != nil {
			t.Fatalf("datagrams=%t delay negotiation = (%t, %v)", datagrams, accepted, err)
		}
	}
}

// An old server echoes the request's raw bytes when it does not accept
// DATAGRAM. The echo returns the offer with the accepted field still 0, so the
// client keeps the legacy wire, and the echo still validates.
func TestTransportDelayH3AuthOldServerRawEchoKeepsLegacyWire(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, datagrams := range []bool{false, true} {
		request := testTransportDelayH3Request()
		SetH3DatagramAuthOffer(request, datagrams)
		// a stale acceptance never rides the offer
		request.TransportDelayAcceptedVersion = TransportDelayVersion
		SetTransportDelayH3Offer(request, DefaultTransportDelaySettings(), false)
		echo := testTransportDelayH3WireCopy(t, request)
		if echo.TransportDelayVersion != TransportDelayVersion || echo.TransportDelayAcceptedVersion != 0 {
			t.Fatalf("datagrams=%t raw echo=%d/%d", datagrams, echo.TransportDelayVersion, echo.TransportDelayAcceptedVersion)
		}
		if accepted, err := negotiateH3DatagramAuthResponse(request, echo, datagrams, true, false); accepted || err != nil {
			t.Fatalf("datagrams=%t raw echo DATAGRAM negotiation = (%t, %v)", datagrams, accepted, err)
		}
		if accepted, err := negotiateTransportDelayH3AuthResponse(request, echo); accepted || err != nil {
			t.Fatalf("datagrams=%t raw echo delay negotiation = (%t, %v)", datagrams, accepted, err)
		}
	}
}

// Servers that predate the field answer DATAGRAM with a response built from
// named fields, which carries neither delay field: the current hybrid server,
// and the pre-session server whose response the client reconstructs the
// request for. The reconstructed request validates with the offer present.
func TestTransportDelayH3AuthOldServerResponseKeepsLegacyWire(t *testing.T) {
	assertMessagePoolOwnership(t)
	request := testTransportDelayH3Request()
	SetH3DatagramAuthOffer(request, true)
	SetTransportDelayH3Offer(request, DefaultTransportDelaySettings(), false)

	modernResponse, _ := AcceptH3DatagramAuthOffer(testTransportDelayH3WireCopy(t, request), true, true, true)
	modernResponse = testTransportDelayH3WireCopy(t, modernResponse)
	if accepted, err := negotiateH3DatagramAuthResponse(request, modernResponse, true, true, true); !accepted || err != nil {
		t.Fatalf("pre-delay hybrid DATAGRAM negotiation = (%t, %v)", accepted, err)
	}
	if accepted, err := negotiateTransportDelayH3AuthResponse(request, modernResponse); accepted || err != nil {
		t.Fatalf("pre-delay hybrid delay negotiation = (%t, %v)", accepted, err)
	}

	requestBytes, err := EncodeFrame(request, DefaultProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	defer MessagePoolReturn(requestBytes)
	legacyRequest, err := decodeLegacyH3Auth(legacyH3AuthDescriptor(t), requestBytes)
	if err != nil {
		t.Fatal(err)
	}
	legacyResponse, accepted := acceptLegacyH3DatagramAuthOffer(legacyRequest, true, true, true)
	if !accepted {
		t.Fatal("historical helper did not accept the offer")
	}
	legacyResponse = testTransportDelayH3WireCopy(t, legacyResponse)
	requestBefore := proto.Clone(request)
	if accepted, err := negotiateH3DatagramAuthResponse(request, legacyResponse, true, true, true); !accepted || err != nil {
		t.Fatalf("legacy reconstruction DATAGRAM negotiation = (%t, %v)", accepted, err)
	}
	if accepted, err := negotiateTransportDelayH3AuthResponse(request, legacyResponse); accepted || err != nil {
		t.Fatalf("legacy reconstruction delay negotiation = (%t, %v)", accepted, err)
	}
	if !proto.Equal(request, requestBefore) {
		t.Fatal("negotiation mutated the borrowed request")
	}
}

// A new server accepts only this build's offered version while its setting
// allows it; anything else clears a stale acceptance.
func TestTransportDelayH3AuthServerAcceptsOnlyAnOfferedVersion(t *testing.T) {
	for _, c := range []struct {
		offeredVersion uint32
		accept         bool
	}{
		// an old client never offers
		{offeredVersion: 0, accept: true},
		{offeredVersion: TransportDelayVersion + 1, accept: true},
		// the server setting is off
		{offeredVersion: TransportDelayVersion, accept: false},
	} {
		request := testTransportDelayH3Request()
		request.TransportDelayVersion = c.offeredVersion
		response, _ := AcceptH3DatagramAuthOffer(request, true, true, true)
		response.TransportDelayAcceptedVersion = TransportDelayVersion
		if AcceptTransportDelayH3Offer(request, response, c.accept) || response.TransportDelayAcceptedVersion != 0 {
			t.Fatalf("offered=%d accept=%t accepted=%d", c.offeredVersion, c.accept, response.TransportDelayAcceptedVersion)
		}
		if accepted, err := negotiateTransportDelayH3AuthResponse(request, response); accepted || err != nil {
			t.Fatalf("offered=%d accept=%t client negotiation = (%t, %v)", c.offeredVersion, c.accept, accepted, err)
		}
	}
}

// A connection declaring provide intent never offers, and a server never
// accepts for one whatever its request carries (DELAY.md D3). Settings that do
// not offer leave the field 0.
func TestTransportDelayH3AuthOfferRequiresSettingsAndNoProvideIntent(t *testing.T) {
	for _, c := range []struct {
		settings      *TransportDelaySettings
		provideIntent bool
	}{
		{settings: nil},
		{settings: &TransportDelaySettings{Version: 0}},
		{settings: DefaultTransportDelaySettings(), provideIntent: true},
	} {
		request := testTransportDelayH3Request()
		request.ProvideIntent = c.provideIntent
		SetTransportDelayH3Offer(request, c.settings, c.provideIntent)
		if request.TransportDelayVersion != 0 {
			t.Fatalf("settings=%+v provide=%t offered %d", c.settings, c.provideIntent, request.TransportDelayVersion)
		}
	}
	request := testTransportDelayH3Request()
	request.ProvideIntent = true
	// an offer forced past SetTransportDelayH3Offer
	request.TransportDelayVersion = TransportDelayVersion
	response, _ := AcceptH3DatagramAuthOffer(request, true, true, true)
	if AcceptTransportDelayH3Offer(request, response, true) || response.TransportDelayAcceptedVersion != 0 {
		t.Fatalf("provide intent accepted=%d", response.TransportDelayAcceptedVersion)
	}
}

// An acceptance the client did not offer, or of another version, is a protocol
// error: the server would stamp a header the client does not strip.
func TestTransportDelayH3AuthRejectsUnofferedAcceptance(t *testing.T) {
	request := testTransportDelayH3Request()
	SetTransportDelayH3Offer(request, nil, false)
	response, _ := AcceptH3DatagramAuthOffer(request, true, true, true)
	response.TransportDelayAcceptedVersion = TransportDelayVersion
	if accepted, err := negotiateTransportDelayH3AuthResponse(request, response); accepted || err == nil {
		t.Fatalf("unoffered acceptance = (%t, %v)", accepted, err)
	}
	SetTransportDelayH3Offer(request, DefaultTransportDelaySettings(), false)
	response.TransportDelayAcceptedVersion = TransportDelayVersion + 1
	if accepted, err := negotiateTransportDelayH3AuthResponse(request, response); accepted || err == nil {
		t.Fatalf("unknown accepted version = (%t, %v)", accepted, err)
	}
}

// Allows fragmented hybrid messages so the header can fall across fragments.
func testH3DatagramHeaderSettings() *H3DatagramSettings {
	settings := DefaultH3DatagramSettings()
	settings.MaxFragmentCount = 16
	settings.HybridDatagramMessageByteCount = settings.MaxMessageByteCount
	return settings
}

// Copies each datagram, as the fragmenter reuses one scratch buffer.
func captureH3DatagramsWithHeader(
	t *testing.T,
	fragmenter *H3DatagramFragmenter,
	message []byte,
	header []byte,
	maxDatagramByteCount int,
) [][]byte {
	t.Helper()
	var datagrams [][]byte
	useStream, _, err := fragmenter.SendHybridWithHeader(message, header, maxDatagramByteCount, func(datagram []byte) error {
		datagrams = append(datagrams, bytes.Clone(datagram))
		return nil
	})
	if useStream || err != nil {
		t.Fatalf("message bytes=%d path=%d send = (stream %t, %v)", len(message), maxDatagramByteCount, useStream, err)
	}
	return datagrams
}

// The header leads the message inside the envelope. Every fragment geometry,
// including fragments shorter than the header and a header split across
// fragments, reassembles in reverse order to the original header and message,
// and only the pooled message is reserved from the budgets.
func TestH3DatagramWithHeaderRoundTrip(t *testing.T) {
	assertMessagePoolOwnership(t)
	settings := testH3DatagramHeaderSettings()
	header := newTransportDelayHeader(0x01020304, 0x05060708, 0x090a0b0c)
	for _, fragmentPayloadByteCount := range []int{5, 8, 12, 16, 1332} {
		for _, messageByteCount := range []int{1, 4, 5, 16, 17, 100} {
			totalByteCount := transportDelayHeaderByteCount + messageByteCount
			wantFragmentCount := (totalByteCount + fragmentPayloadByteCount - 1) / fragmentPayloadByteCount
			if settings.MaxFragmentCount < wantFragmentCount {
				continue
			}
			message := make([]byte, messageByteCount)
			for i := range message {
				message[i] = byte(i*31 + 3)
			}
			stats := &H3DatagramStats{}
			fragmenter, err := NewH3DatagramFragmenter(settings, stats)
			if err != nil {
				t.Fatal(err)
			}
			datagrams := captureH3DatagramsWithHeader(t, fragmenter, message, header[:], H3DatagramHeaderByteCount+fragmentPayloadByteCount)
			if len(datagrams) != wantFragmentCount {
				t.Fatalf("payload=%d message=%d fragments=%d want=%d", fragmentPayloadByteCount, messageByteCount, len(datagrams), wantFragmentCount)
			}
			for _, datagram := range datagrams {
				if binary.BigEndian.Uint32(datagram[12:16]) != uint32(totalByteCount) {
					t.Fatalf("payload=%d message=%d envelope total=%d want=%d", fragmentPayloadByteCount, messageByteCount, binary.BigEndian.Uint32(datagram[12:16]), totalByteCount)
				}
			}
			budget := NewH3DatagramReassemblyBudget(settings.ProcessReassemblyByteCount)
			reassembler, err := NewH3DatagramReassembler(settings, budget, stats)
			if err != nil {
				t.Fatal(err)
			}
			var receivedHeader transportDelayHeader
			var received []byte
			for i := len(datagrams) - 1; 0 <= i; i -= 1 {
				message := reassembler.AcceptWithHeader(datagrams[i], time.Unix(100, 0), receivedHeader[:])
				if message == nil {
					if receivedHeader != (transportDelayHeader{}) {
						t.Fatalf("payload=%d message=%d header written before completion", fragmentPayloadByteCount, messageByteCount)
					}
					if used := budget.Used(); used != int64(messageByteCount) {
						t.Fatalf("payload=%d message=%d reserved=%d want=%d", fragmentPayloadByteCount, messageByteCount, used, messageByteCount)
					}
					continue
				}
				if received != nil {
					t.Fatalf("payload=%d message=%d delivered twice", fragmentPayloadByteCount, messageByteCount)
				}
				received = message
			}
			if !bytes.Equal(received, message) || receivedHeader != header {
				t.Fatalf("payload=%d message=%d received=%x header=%x", fragmentPayloadByteCount, messageByteCount, received, receivedHeader)
			}
			MessagePoolReturn(received)
			reassembler.Close()
			if budget.Used() != 0 {
				t.Fatalf("payload=%d message=%d retained shared bytes=%d", fragmentPayloadByteCount, messageByteCount, budget.Used())
			}
			snapshot := stats.Snapshot()
			if snapshot.SentMessageByteCount != uint64(messageByteCount) ||
				snapshot.ReceivedMessageByteCount != uint64(messageByteCount) ||
				snapshot.SentFragmentCount != uint64(wantFragmentCount) ||
				snapshot.SentByteCount != uint64(wantFragmentCount*H3DatagramHeaderByteCount+totalByteCount) {
				t.Fatalf("payload=%d message=%d stats=%+v", fragmentPayloadByteCount, messageByteCount, snapshot)
			}
		}
	}
}

// Every budget that fits a message to the DATAGRAM lane subtracts the header:
// the hybrid threshold, the live path after a reduction, and the serialized
// limit a server hands its resident. A zero header is the legacy predicate.
func TestH3DatagramWithHeaderSizeLimits(t *testing.T) {
	settings := DefaultH3DatagramSettings()
	header := newTransportDelayHeader(1, 2, 3)
	target := settings.TargetDatagramByteCount
	threshold := settings.HybridDatagramMessageByteCount - transportDelayHeaderByteCount
	if threshold != 1360-H3DatagramHeaderByteCount-transportDelayHeaderByteCount {
		t.Fatalf("header threshold=%d", threshold)
	}
	if !settings.UseDatagramForPath(threshold+transportDelayHeaderByteCount, target) ||
		settings.UseDatagramForPath(threshold+transportDelayHeaderByteCount+1, target) {
		t.Fatal("legacy hybrid threshold changed")
	}
	if !settings.UseDatagramForPathWithHeader(threshold, target, transportDelayHeaderByteCount) ||
		settings.UseDatagramForPathWithHeader(threshold+1, target, transportDelayHeaderByteCount) ||
		settings.UseDatagramForPathWithHeader(0, target, transportDelayHeaderByteCount) {
		t.Fatal("header hybrid threshold was not enforced")
	}
	if settings.UseDatagramForPathWithHeader(threshold+transportDelayHeaderByteCount, target, 0) !=
		settings.UseDatagramForPath(threshold+transportDelayHeaderByteCount, target) {
		t.Fatal("zero header differs from the legacy predicate")
	}

	stats := &H3DatagramStats{}
	fragmenter, err := NewH3DatagramFragmenter(settings, stats)
	if err != nil {
		t.Fatal(err)
	}
	var datagramByteCounts []int
	capture := func(datagram []byte) error {
		datagramByteCounts = append(datagramByteCounts, len(datagram))
		return nil
	}
	// the largest message fills the datagram exactly; one more byte moves to the stream
	useStream, _, err := fragmenter.SendHybridWithHeader(make([]byte, threshold), header[:], target, capture)
	if useStream || err != nil || len(datagramByteCounts) != 1 || datagramByteCounts[0] != target {
		t.Fatalf("threshold send stream=%t datagrams=%v err=%v", useStream, datagramByteCounts, err)
	}
	useStream, _, err = fragmenter.SendHybridWithHeader(make([]byte, threshold+1), header[:], target, capture)
	if !useStream || err != nil || len(datagramByteCounts) != 1 {
		t.Fatalf("threshold+1 send stream=%t datagrams=%v err=%v", useStream, datagramByteCounts, err)
	}

	// a path reduction applies the header against the reported limit
	const reducedDatagramByteCount = 1200
	reducedThreshold := reducedDatagramByteCount - H3DatagramHeaderByteCount - transportDelayHeaderByteCount
	for _, c := range []struct {
		messageByteCount int
		wantStream       bool
	}{
		{messageByteCount: reducedThreshold, wantStream: false},
		{messageByteCount: reducedThreshold + 1, wantStream: true},
	} {
		sendByteCounts := []int{}
		useStream, nextMax, err := fragmenter.SendHybridWithHeader(
			make([]byte, c.messageByteCount),
			header[:],
			target,
			func(datagram []byte) error {
				sendByteCounts = append(sendByteCounts, len(datagram))
				if len(sendByteCounts) == 1 {
					return &quic.DatagramTooLargeError{MaxDatagramPayloadSize: reducedDatagramByteCount}
				}
				return nil
			},
		)
		wantSendCount := 2
		if c.wantStream {
			wantSendCount = 1
		}
		if err != nil || useStream != c.wantStream || nextMax != reducedDatagramByteCount || len(sendByteCounts) != wantSendCount {
			t.Fatalf("reduced message=%d stream=%t max=%d sends=%v err=%v", c.messageByteCount, useStream, nextMax, sendByteCounts, err)
		}
		if !c.wantStream && sendByteCounts[1] != reducedDatagramByteCount {
			t.Fatalf("reduced retry datagram=%d want=%d", sendByteCounts[1], reducedDatagramByteCount)
		}
	}
	if snapshot := stats.Snapshot(); snapshot.SendErrorCount != 0 {
		t.Fatalf("size limits counted send errors: %+v", snapshot)
	}

	// the serialized limit agrees with the live predicate at every size
	for _, pathByteCount := range []int{H3InitialDatagramByteCount, reducedDatagramByteCount, target} {
		limit := H3DatagramTransferFrameByteLimitWithHeader(settings, pathByteCount, transportDelayHeaderByteCount)
		if limit != H3DatagramTransferFrameByteLimit(settings, pathByteCount)-transportDelayHeaderByteCount {
			t.Fatalf("path=%d header limit=%d", pathByteCount, limit)
		}
		if H3DatagramTransferFrameByteLimitWithHeader(settings, pathByteCount, 0) != H3DatagramTransferFrameByteLimit(settings, pathByteCount) {
			t.Fatalf("path=%d zero header limit differs from the legacy limit", pathByteCount)
		}
		for byteCount := 1; byteCount <= settings.MaxMessageByteCount; byteCount += 1 {
			if (byteCount <= limit) != settings.UseDatagramForPathWithHeader(byteCount, pathByteCount, transportDelayHeaderByteCount) {
				t.Fatalf("path=%d bytes=%d serialized limit=%d disagrees with the live predicate", pathByteCount, byteCount, limit)
			}
		}
	}
	if limit := H3DatagramTransferFrameByteLimitWithHeader(settings, H3DatagramHeaderByteCount+transportDelayHeaderByteCount, transportDelayHeaderByteCount); limit != 0 {
		t.Fatalf("a path that fits only the header selected messages up to %d", limit)
	}
}

// Header bytes are part of the reassembled envelope: a conflicting copy of a
// fragment that carries header bytes is malformed, corrupt header bytes fail
// the checksum, and an envelope with no room for a message byte after the
// header is malformed. None of them writes the caller's header or keeps a
// reservation.
func TestH3DatagramWithHeaderRejectsMalformedHeaderBytes(t *testing.T) {
	assertMessagePoolOwnership(t)
	settings := testH3DatagramHeaderSettings()
	header := newTransportDelayHeader(0x11111111, 0x22222222, 0x33333333)
	message := []byte("header-split-message")
	stats := &H3DatagramStats{}
	fragmenter, err := NewH3DatagramFragmenter(settings, stats)
	if err != nil {
		t.Fatal(err)
	}
	budget := NewH3DatagramReassemblyBudget(settings.ProcessReassemblyByteCount)
	reassembler, err := NewH3DatagramReassembler(settings, budget, stats)
	if err != nil {
		t.Fatal(err)
	}
	defer reassembler.Close()
	now := time.Unix(200, 0)
	var receivedHeader transportDelayHeader
	accept := func(datagram []byte) {
		t.Helper()
		if message := reassembler.AcceptWithHeader(datagram, now, receivedHeader[:]); message != nil {
			MessagePoolReturn(message)
			t.Fatal("malformed input completed a message")
		}
	}

	// 8-byte fragments: header[0:8], header[8:12] and the first 4 message bytes, ...
	datagrams := captureH3DatagramsWithHeader(t, fragmenter, message, header[:], H3DatagramHeaderByteCount+8)
	if len(datagrams) != 4 {
		t.Fatalf("fragments=%d want=4", len(datagrams))
	}
	accept(datagrams[0])
	accept(datagrams[0])
	if snapshot := stats.Snapshot(); snapshot.DuplicateFragmentCount != 1 || snapshot.MalformedFragmentCount != 0 {
		t.Fatalf("exact header duplicate stats=%+v", snapshot)
	}
	conflicting := bytes.Clone(datagrams[0])
	conflicting[H3DatagramHeaderByteCount+3] ^= 0xff
	accept(conflicting)
	if snapshot := stats.Snapshot(); snapshot.MalformedFragmentCount != 1 {
		t.Fatalf("conflicting header duplicate stats=%+v", snapshot)
	}
	// the retired message never completes
	for _, datagram := range datagrams[1:] {
		accept(datagram)
	}

	datagrams = captureH3DatagramsWithHeader(t, fragmenter, message, header[:], H3DatagramHeaderByteCount+8)
	// header[8], in the fragment that also starts the message
	datagrams[1][H3DatagramHeaderByteCount] ^= 0xff
	for _, datagram := range datagrams {
		accept(datagram)
	}
	if snapshot := stats.Snapshot(); snapshot.ChecksumFailureCount != 1 {
		t.Fatalf("corrupt header stats=%+v", snapshot)
	}

	var legacyDatagrams [][]byte
	if _, err := fragmenter.Send(make([]byte, transportDelayHeaderByteCount), settings.TargetDatagramByteCount, func(datagram []byte) error {
		legacyDatagrams = append(legacyDatagrams, bytes.Clone(datagram))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	accept(legacyDatagrams[0])
	if snapshot := stats.Snapshot(); snapshot.MalformedFragmentCount != 2 {
		t.Fatalf("header-only envelope stats=%+v", snapshot)
	}
	if receivedHeader != (transportDelayHeader{}) {
		t.Fatalf("rejected input wrote the header: %x", receivedHeader)
	}
	if budget.Used() != 0 {
		t.Fatalf("rejected input retained %d shared bytes", budget.Used())
	}
}

// A measuring client and a stamp-and-echo peer on both lanes. Payloads arrive
// without their headers, the peer echoes the client's one-way delay, a message
// too large for DATAGRAM records no DATAGRAM write, and the client's flushed
// interval accounts every wire byte of both lanes in both directions.
func TestTransportDelayH3LanesStampStripAndEcho(t *testing.T) {
	assertMessagePoolOwnership(t)
	framerSettings := DefaultFramerSettings(4096)
	clientId := NewId()
	delaySettings := testTransportDelaySettings()
	// one interval, closed only by the flush
	delaySettings.Interval = time.Hour
	var published []*TransportDelayStats
	client := newTransportDelayH3(
		newTransportDelay(delaySettings, clientId, TransportTypeH3Dns, func(stats *TransportDelayStats) {
			published = append(published, stats)
		}),
		framerSettings,
	)
	peer := NewTransportDelayH3Peer(clientId, framerSettings)
	if client.HeaderByteCount() != transportDelayHeaderByteCount || (*TransportDelayH3)(nil).HeaderByteCount() != 0 {
		t.Fatal("header byte count")
	}

	upMessages := [][]byte{[]byte("first-stream-message"), bytes.Repeat([]byte{0x5a}, 600)}
	upByteCount := 2*(4+transportDelayHeaderByteCount) + len(upMessages[0]) + len(upMessages[1])
	up := &bytes.Buffer{}
	if err := client.WriteBatchWithStorage(up, upMessages, make([]byte, 4096)); err != nil {
		t.Fatal(err)
	}
	if up.Len() != upByteCount {
		t.Fatalf("stream batch wire bytes=%d want=%d", up.Len(), upByteCount)
	}
	for _, want := range upMessages {
		message, err := peer.Read(up)
		if err != nil {
			t.Fatal(err)
		}
		matches := bytes.Equal(message, want)
		MessagePoolReturn(message)
		if !matches {
			t.Fatal("stream message changed")
		}
	}
	if peer.delay.echo.Load() == 0 {
		t.Fatal("peer holds no echo after a client write")
	}

	down := &bytes.Buffer{}
	// a heartbeat without storage takes the singleton path
	if err := peer.WriteBatchWithStorage(down, [][]byte{nil}, nil); err != nil {
		t.Fatal(err)
	}
	if down.Len() != 4+transportDelayHeaderByteCount {
		t.Fatalf("heartbeat wire bytes=%d", down.Len())
	}
	heartbeat, err := client.Read(down)
	if err != nil || len(heartbeat) != 0 {
		t.Fatalf("heartbeat=%x err=%v", heartbeat, err)
	}
	MessagePoolReturn(heartbeat)

	datagramSettings := DefaultH3DatagramSettings()
	budget := NewH3DatagramReassemblyBudget(datagramSettings.ProcessReassemblyByteCount)
	newLane := func() (*H3DatagramFragmenter, *H3DatagramReassembler) {
		fragmenter, err := NewH3DatagramFragmenter(datagramSettings, &H3DatagramStats{})
		if err != nil {
			t.Fatal(err)
		}
		reassembler, err := NewH3DatagramReassembler(datagramSettings, budget, &H3DatagramStats{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(reassembler.Close)
		return fragmenter, reassembler
	}
	clientFragmenter, clientReassembler := newLane()
	peerFragmenter, peerReassembler := newLane()
	datagramMessage := []byte("datagram-lane-message")
	datagramByteCount := H3DatagramHeaderByteCount + transportDelayHeaderByteCount + len(datagramMessage)
	var datagrams [][]byte
	capture := func(datagram []byte) error {
		datagrams = append(datagrams, bytes.Clone(datagram))
		return nil
	}
	exchange := func(
		sender *TransportDelayH3,
		fragmenter *H3DatagramFragmenter,
		receiver *TransportDelayH3,
		reassembler *H3DatagramReassembler,
	) {
		t.Helper()
		useStream, _, err := sender.SendHybrid(fragmenter, datagramMessage, datagramSettings.TargetDatagramByteCount, capture)
		if useStream || err != nil {
			t.Fatalf("DATAGRAM send = (stream %t, %v)", useStream, err)
		}
		datagram := datagrams[len(datagrams)-1]
		if len(datagram) != datagramByteCount {
			t.Fatalf("DATAGRAM wire bytes=%d want=%d", len(datagram), datagramByteCount)
		}
		message := receiver.Accept(reassembler, datagram, time.Now())
		matches := bytes.Equal(message, datagramMessage)
		MessagePoolReturn(message)
		if !matches {
			t.Fatal("DATAGRAM message changed")
		}
	}
	exchange(client, clientFragmenter, peer, peerReassembler)
	exchange(peer, peerFragmenter, client, clientReassembler)
	useStream, _, err := client.SendHybrid(clientFragmenter, make([]byte, 2000), datagramSettings.TargetDatagramByteCount, capture)
	if !useStream || err != nil || len(datagrams) != 2 {
		t.Fatalf("oversized DATAGRAM = (stream %t, %v) datagrams=%d", useStream, err, len(datagrams))
	}

	peer.flush()
	client.flush()
	if len(published) != 1 {
		t.Fatalf("published %d intervals, want 1", len(published))
	}
	stats := published[0]
	if stats.ClientId != clientId || stats.TransportType != TransportTypeH3Dns ||
		stats.SendByteCount != ByteCount(upByteCount+datagramByteCount) ||
		stats.ReceiveByteCount != ByteCount(4+transportDelayHeaderByteCount+datagramByteCount) ||
		!stats.SendQueueDelayValid || !stats.ReceiveQueueDelayValid {
		t.Fatalf("client interval=%+v", stats)
	}
}

// The server half of a live H3 negotiation.
type h3DelayRoundTripCase struct {
	datagrams bool
	// the server accepts the offered delay header
	acceptDelay bool
	// an old server that echoes the request's raw bytes, as it does whenever it
	// does not accept DATAGRAM
	rawEcho bool
}

// A real QUIC peer authenticates the client, then exchanges one stream message
// and, with DATAGRAM, one DATAGRAM message each way while both sides keep
// stamped heartbeats flowing. Payloads must arrive unchanged on every lane. A
// negotiated connection publishes intervals with delay samples in both
// directions; the legacy wire publishes nothing, including at teardown. Either
// way the server's answer replaces the one the route manager recorded for the
// client's P2P.
func testPlatformTransportH3DelayRoundTrip(t *testing.T, ipVersion int, c h3DelayRoundTripCase) {
	t.Helper()
	testCtx, testCancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer testCancel()
	certPem, keyPem, err := selfSign([]string{testLoopbackIp(ipVersion)}, testLoopbackIp(ipVersion), 24*time.Hour, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(certPem, keyPem)
	if err != nil {
		t.Fatal(err)
	}
	const nextProto = "urnetwork-h3-delay-test"
	listener, err := quic.ListenAddrEarly(
		testLoopbackHostPort(ipVersion, 0),
		&tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{nextProto}},
		&quic.Config{EnableDatagrams: true, MaxIdleTimeout: 30 * time.Second},
	)
	if err != nil {
		t.Fatal(err)
	}

	framerSettings := DefaultFramerSettings(int(DefaultClientSettings().MinimumMessageLenLimit()))
	datagramSettings := DefaultH3DatagramSettings()
	clientStreamMessage := bytes.Repeat([]byte("client-stream-message-"), 100)
	clientDatagramMessage := []byte("client-datagram-message")
	serverStreamMessage := bytes.Repeat([]byte("server-stream-message-"), 100)
	serverDatagramMessage := []byte("server-datagram-message")
	// the client's ping also sets its QUIC idle timeout (4x), so it stays long;
	// the server's heartbeats keep both directions sampling between client writes
	const clientPingTimeout = 250 * time.Millisecond
	const serverHeartbeatInterval = 25 * time.Millisecond

	serverErrors := make(chan error, 4)
	serverReceived := make(chan []byte, 8)
	serverStop := make(chan struct{})
	var serverWorkers sync.WaitGroup
	defer serverWorkers.Wait()
	defer listener.Close()
	var stopServerOnce sync.Once
	stopServer := func() {
		stopServerOnce.Do(func() {
			close(serverStop)
		})
	}
	defer stopServer()
	startServerWorker := func(run func() error) {
		serverWorkers.Add(1)
		go func() {
			defer serverWorkers.Done()
			if err := run(); err != nil {
				serverErrors <- err
			}
		}()
	}
	startServerWorker(func() error {
		connection, err := listener.Accept(testCtx)
		if err != nil {
			return err
		}
		defer connection.CloseWithError(0, "test complete")
		stream, err := connection.AcceptStream(testCtx)
		if err != nil {
			return err
		}
		framer := NewFramer(framerSettings)
		authBytes, err := framer.Read(stream)
		if err != nil {
			return err
		}
		defer MessagePoolReturn(authBytes)
		decoded, err := DecodeFrame(authBytes)
		if err != nil {
			return err
		}
		auth, ok := decoded.(*protocol.Auth)
		if !ok {
			return fmt.Errorf("auth type=%T", decoded)
		}
		if auth.TransportDelayVersion != TransportDelayVersion {
			return fmt.Errorf("client did not offer the delay header: %d", auth.TransportDelayVersion)
		}
		state := connection.ConnectionState()
		response, datagramsAccepted := AcceptH3DatagramAuthOffer(auth, c.datagrams, state.SupportsDatagrams.Local, state.SupportsDatagrams.Remote)
		if datagramsAccepted != c.datagrams {
			return fmt.Errorf("server DATAGRAM acceptance=%t want %t", datagramsAccepted, c.datagrams)
		}
		delayAccepted := AcceptTransportDelayH3Offer(auth, response, c.acceptDelay)
		if c.rawEcho {
			if writeErr := framer.Write(stream, authBytes); writeErr != nil {
				return writeErr
			}
		} else {
			responseBytes, encodeErr := EncodeFrame(response, DefaultProtocolVersion)
			if encodeErr != nil {
				return encodeErr
			}
			writeErr := framer.Write(stream, responseBytes)
			MessagePoolReturn(responseBytes)
			if writeErr != nil {
				return writeErr
			}
		}

		var streamFraming h3StreamFraming = framer
		var peer *TransportDelayH3
		if delayAccepted {
			peer = NewTransportDelayH3Peer(NewId(), framerSettings)
			streamFraming = peer
		}
		// each lane keeps one reader and one writer, as in production
		startServerWorker(func() error {
			for {
				message, err := streamFraming.Read(stream)
				if err != nil {
					// the connection closed
					return nil
				}
				if len(message) != 0 {
					serverReceived <- bytes.Clone(message)
				}
				MessagePoolReturn(message)
			}
		})
		if datagramsAccepted {
			reassembler, err := NewH3DatagramReassembler(
				datagramSettings,
				NewH3DatagramReassemblyBudget(datagramSettings.ProcessReassemblyByteCount),
				&H3DatagramStats{},
			)
			if err != nil {
				return err
			}
			startServerWorker(func() error {
				defer reassembler.Close()
				for {
					datagram, err := connection.ReceiveDatagram(testCtx)
					if err != nil {
						return nil
					}
					var message []byte
					if peer != nil {
						message = peer.Accept(reassembler, datagram, time.Now())
					} else {
						message = reassembler.Accept(datagram, time.Now())
					}
					if message != nil {
						serverReceived <- bytes.Clone(message)
						MessagePoolReturn(message)
					}
				}
			})
		}

		storage := make([]byte, 64*1024)
		write := func(messages [][]byte) error {
			stream.SetWriteDeadline(time.Now().Add(5 * time.Second))
			return streamFraming.WriteBatchWithStorage(stream, messages, storage)
		}
		if err := write([][]byte{serverStreamMessage}); err != nil {
			return err
		}
		if datagramsAccepted {
			fragmenter, err := NewH3DatagramFragmenter(datagramSettings, &H3DatagramStats{})
			if err != nil {
				return err
			}
			var useStream bool
			if peer != nil {
				useStream, _, err = peer.SendHybrid(fragmenter, serverDatagramMessage, datagramSettings.TargetDatagramByteCount, connection.SendDatagram)
			} else {
				useStream, _, err = fragmenter.SendHybrid(serverDatagramMessage, datagramSettings.TargetDatagramByteCount, connection.SendDatagram)
			}
			if err != nil || useStream {
				return fmt.Errorf("server DATAGRAM send = (stream %t, %v)", useStream, err)
			}
		}
		for {
			select {
			case <-serverStop:
				return nil
			case <-testCtx.Done():
				return nil
			case <-time.After(serverHeartbeatInterval):
			}
			if err := write([][]byte{nil}); err != nil {
				return nil
			}
		}
	})

	settings := testingPlatformTransportSettings()
	settings.H3Port = listener.Addr().(*net.UDPAddr).Port
	settings.resolveH3AddrsForTest = testingH3LoopbackResolver(ipVersion, settings.H3Port)
	settings.QuicTlsConfig = &tls.Config{
		InsecureSkipVerify: true, // test-only self-signed endpoint
		NextProtos:         []string{nextProto},
	}
	settings.FramerSettings = framerSettings
	settings.EnableH3Datagrams = c.datagrams
	settings.H3DatagramSettings = datagramSettings
	settings.PingTimeout = clientPingTimeout
	settings.TransportDelay = &TransportDelaySettings{
		Version:             TransportDelayVersion,
		Interval:            100 * time.Millisecond,
		MinDelaySampleCount: 1,
	}
	sendRoutes := make(chan struct{}, 1)
	settings.SendRouteObserver = func(_ Transport, _ Route, connected bool) {
		if connected {
			select {
			case sendRoutes <- struct{}{}:
			default:
			}
		}
	}
	clientReceived := make(chan []byte, 8)
	settings.afterH3ReceiveEnqueueForTest = func(message []byte) {
		select {
		case clientReceived <- bytes.Clone(message):
		default:
		}
	}

	negotiated := c.acceptDelay && !c.rawEcho
	routeManager := NewRouteManager(testCtx, "h3-delay-round-trip")
	// the opposite of this edge's answer, which must replace it (DELAY.md §6.1)
	routeManager.noteTransportDelayEdgeAnswer(!negotiated)
	var statsLock sync.Mutex
	var publishedStats []TransportDelayStats
	statsPublished := make(chan struct{}, 1)
	removeStatsCallback := routeManager.addTransportDelayStatsCallback(func(stats *TransportDelayStats) {
		func() {
			statsLock.Lock()
			defer statsLock.Unlock()
			publishedStats = append(publishedStats, *stats)
		}()
		select {
		case statsPublished <- struct{}{}:
		default:
		}
	})
	defer removeStatsCallback()
	// production always drains the receive routes; the hybrid stream route is
	// unbuffered
	reader := routeManager.OpenMultiRouteReader(DestinationId(NewId()))
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			message, readErr := reader.Read(testCtx, time.Second)
			if message != nil {
				MessagePoolReturn(message)
			}
			if readErr != nil {
				return
			}
		}
	}()
	defer func() {
		routeManager.CloseMultiRouteReader(reader)
		<-readerDone
	}()
	transport := NewPlatformTransportWithTargetMode(
		testCtx,
		NewClientStrategyWithDefaults(testCtx),
		routeManager,
		"https://"+testLoopbackHost(ipVersion),
		&ClientAuth{ByJwt: "testing", InstanceId: NewId(), AppVersion: "testing"},
		TransportModeH3,
		settings,
	)
	closeTransport := func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		if err := transport.CloseAndWait(closeCtx); err != nil {
			t.Errorf("v%d transport cleanup: %v", ipVersion, err)
		}
	}
	defer closeTransport()

	select {
	case <-sendRoutes:
	case err := <-serverErrors:
		t.Fatalf("v%d server before route: %v", ipVersion, err)
	case <-testCtx.Done():
		t.Fatalf("v%d wait for send route: %v", ipVersion, testCtx.Err())
	}
	// the auth exchange precedes the route
	if accepted := routeManager.transportDelayEdgeAccepted.Load(); accepted != negotiated {
		t.Fatalf("v%d recorded edge acceptance=%t, expected %t", ipVersion, accepted, negotiated)
	}
	writer := routeManager.OpenMultiRouteWriter(DestinationId(NewId()))
	defer routeManager.CloseMultiRouteWriter(writer)
	carrierWriter := writer.(transferCarrierMultiRouteWriter)
	write := func(message []byte, wantUnreliable bool) {
		t.Helper()
		pooledMessage := MessagePoolCopy(message)
		success, disposition, writeErr := carrierWriter.writeDetailedWithCarrier(testCtx, pooledMessage, time.Second)
		if writeErr != nil || !success {
			MessagePoolReturn(pooledMessage)
			t.Fatalf("v%d route write = (%t, %+v, %v)", ipVersion, success, disposition, writeErr)
		}
		if disposition.transportType != TransportTypeH3 || disposition.unreliable != wantUnreliable {
			t.Fatalf("v%d disposition=%+v want unreliable=%t", ipVersion, disposition, wantUnreliable)
		}
	}
	write(clientStreamMessage, false)
	wantClientReceived := map[string]bool{string(serverStreamMessage): true}
	wantServerReceived := map[string]bool{string(clientStreamMessage): true}
	if c.datagrams {
		write(clientDatagramMessage, true)
		wantClientReceived[string(serverDatagramMessage)] = true
		wantServerReceived[string(clientDatagramMessage)] = true
	}
	for 0 < len(wantClientReceived) || 0 < len(wantServerReceived) {
		select {
		case message := <-clientReceived:
			if !wantClientReceived[string(message)] {
				t.Fatalf("v%d client received an unexpected %d-byte payload", ipVersion, len(message))
			}
			delete(wantClientReceived, string(message))
		case message := <-serverReceived:
			if !wantServerReceived[string(message)] {
				t.Fatalf("v%d server received an unexpected %d-byte payload", ipVersion, len(message))
			}
			delete(wantServerReceived, string(message))
		case err := <-serverErrors:
			t.Fatalf("v%d server: %v", ipVersion, err)
		case <-testCtx.Done():
			t.Fatalf("v%d wait for payloads: client missing %d, server missing %d", ipVersion, len(wantClientReceived), len(wantServerReceived))
		}
	}

	sampledStats := func() (TransportDelayStats, bool) {
		statsLock.Lock()
		defer statsLock.Unlock()
		for _, stats := range publishedStats {
			if stats.TransportType == TransportTypeH3 && 0 < stats.SendByteCount && 0 < stats.ReceiveByteCount &&
				stats.SendQueueDelayValid && stats.ReceiveQueueDelayValid {
				return stats, true
			}
		}
		return TransportDelayStats{}, false
	}
	if negotiated {
		for {
			if _, ok := sampledStats(); ok {
				break
			}
			select {
			case <-statsPublished:
			case err := <-serverErrors:
				t.Fatalf("v%d server: %v", ipVersion, err)
			case <-testCtx.Done():
				statsLock.Lock()
				defer statsLock.Unlock()
				t.Fatalf("v%d no interval with delay samples in both directions: %+v", ipVersion, publishedStats)
			}
		}
	}
	closeTransport()
	stopServer()
	if !negotiated {
		statsLock.Lock()
		defer statsLock.Unlock()
		if len(publishedStats) != 0 {
			t.Fatalf("v%d the legacy wire published %d intervals", ipVersion, len(publishedStats))
		}
	}
}

// New/new hybrid: the stream and DATAGRAM lanes both carry the header.
func TestPlatformTransportH3DelayHeaderHybridRoundTrip(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, ipVersion := range testIpVersions {
		testPlatformTransportH3DelayRoundTrip(t, ipVersion, h3DelayRoundTripCase{datagrams: true, acceptDelay: true})
	}
}

// New/new without DATAGRAM: the server must encode its response to accept.
func TestPlatformTransportH3DelayHeaderStreamRoundTrip(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, ipVersion := range testIpVersions {
		testPlatformTransportH3DelayRoundTrip(t, ipVersion, h3DelayRoundTripCase{acceptDelay: true})
	}
}

// A new client against old servers keeps the legacy wire byte for byte: the
// raw echo of a stream-only server and the encoded response of a hybrid server
// that predates the field.
func TestPlatformTransportH3DelayHeaderOldServerKeepsLegacyWire(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, c := range []h3DelayRoundTripCase{
		{rawEcho: true},
		{datagrams: true},
	} {
		for _, ipVersion := range testIpVersions {
			testPlatformTransportH3DelayRoundTrip(t, ipVersion, c)
		}
	}
}
