package fingerprint

// quic_initial_test.go -- the QUIC Initial parser and drift check, and the QUIC
// endpoint capturing a real on-wire Initial from a quic-go client (the
// impl-independent stand-in for Docker Chrome). the version drift the merged
// QuicVersionPolicy must not regress is proven here without connect.

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"net"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
)

// buildSyntheticQuicInitial builds a minimal valid Initial long header for the
// version, padded to totalLength, for the parser and diff tests.
func buildSyntheticQuicInitial(version uint32, totalLength int) []byte {
	typeBits := byte(0x00) // version 1 Initial
	if version == QuicVersion2 {
		typeBits = 0x01 // version 2 Initial (RFC 9369)
	}
	datagram := []byte{0xC0 | (typeBits << 4)} // long header, fixed bit, type
	datagram = binary.BigEndian.AppendUint32(datagram, version)
	dcid := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	datagram = append(datagram, byte(len(dcid)))
	datagram = append(datagram, dcid...)
	scid := []byte{9, 10, 11, 12}
	datagram = append(datagram, byte(len(scid)))
	datagram = append(datagram, scid...)
	datagram = append(datagram, 0x00)       // token length varint = 0
	datagram = append(datagram, 0x44, 0x00) // length varint (2-byte form), value unused by the parser
	for len(datagram) < totalLength {
		datagram = append(datagram, 0x00) // padding
	}
	return datagram
}

// The parser reads the long-header form, fixed bit, version, the version's
// Initial packet type, the connection-id lengths and the token length.
func TestParseQuicInitialReadsLongHeader(t *testing.T) {
	cases := []struct {
		description string
		version     uint32
	}{
		{description: "version 2", version: QuicVersion2},
		{description: "version 1", version: QuicVersion1},
	}
	for _, c := range cases {
		datagram := buildSyntheticQuicInitial(c.version, quicMinInitialDatagramLength)
		fingerprint, err := ParseQuicInitial(datagram)
		if err != nil {
			t.Fatalf("%s: %s", c.description, err)
		}
		if !fingerprint.LongHeader || !fingerprint.FixedBit {
			t.Errorf("%s: long header %t fixed bit %t, want both set", c.description, fingerprint.LongHeader, fingerprint.FixedBit)
		}
		if fingerprint.Version != c.version {
			t.Errorf("%s: version %08x, want %08x", c.description, fingerprint.Version, c.version)
		}
		if fingerprint.PacketType != QuicPacketInitial {
			t.Errorf("%s: packet type %q, want initial", c.description, fingerprint.PacketType)
		}
		if fingerprint.DcidLength != 8 || fingerprint.ScidLength != 4 || fingerprint.TokenLength != 0 {
			t.Errorf("%s: dcid %d scid %d token %d, want 8 4 0", c.description, fingerprint.DcidLength, fingerprint.ScidLength, fingerprint.TokenLength)
		}
	}
}

// A datagram too short, or not a long header, is a named error, not a panic.
func TestParseQuicInitialRejectsMalformed(t *testing.T) {
	short := []byte{0xC0, 0x00, 0x00}
	if _, err := ParseQuicInitial(short); err == nil {
		t.Error("parsed a too-short datagram without error")
	}
	// a short-header packet (form bit clear).
	shortHeader := make([]byte, 64)
	shortHeader[0] = 0x40
	if _, err := ParseQuicInitial(shortHeader); err == nil {
		t.Error("parsed a short-header packet as an initial")
	}
}

// The QUIC diff discriminates: a matching v2 Initial is clean; a v1 Initial
// against an expected v2 drifts the version; an unpadded datagram drifts the
// padding; a short header drifts the form. this is the udp drift gate.
func TestDiffQuicInitialDiscriminates(t *testing.T) {
	v2 := mustParseQuicInitial(t, buildSyntheticQuicInitial(QuicVersion2, quicMinInitialDatagramLength))
	if drifts := DiffQuicInitial(v2, QuicDiffOptions{ExpectedVersion: QuicVersion2}); len(drifts) != 0 {
		t.Fatalf("a matching v2 initial drifted: %v", drifts)
	}

	v1 := mustParseQuicInitial(t, buildSyntheticQuicInitial(QuicVersion1, quicMinInitialDatagramLength))
	if !driftHasField(DiffQuicInitial(v1, QuicDiffOptions{ExpectedVersion: QuicVersion2}), "quic_version") {
		t.Error("a v1 initial did not drift the version against an expected v2")
	}

	unpadded := mustParseQuicInitial(t, buildSyntheticQuicInitial(QuicVersion2, 64))
	if !driftHasField(DiffQuicInitial(unpadded, QuicDiffOptions{ExpectedVersion: QuicVersion2}), "quic_initial_padding") {
		t.Error("an unpadded initial did not drift the padding")
	}
}

func mustParseQuicInitial(t *testing.T, datagram []byte) *QuicInitialFingerprint {
	t.Helper()
	fingerprint, err := ParseQuicInitial(datagram)
	if err != nil {
		t.Fatal(err)
	}
	return fingerprint
}

func driftHasField(drifts []Drift, field string) bool {
	for _, drift := range drifts {
		if drift.Field == field {
			return true
		}
	}
	return false
}

// The QUIC endpoint captures a real on-wire Initial from a quic-go client: the
// client offering version 2 first sends a version 2 Initial long header, padded
// to 1200, which the capture reads and the diff passes. the fail side is the
// version-1 client, whose Initial drifts against an expected version 2.
func TestQuicEndpointCapturesInitialFromQuicGoClient(t *testing.T) {
	cases := []struct {
		description   string
		offerVersions []quic.Version
		wantVersion   uint32
	}{
		{description: "prefer v2", offerVersions: []quic.Version{quic.Version2, quic.Version1}, wantVersion: QuicVersion2},
		{description: "v1 only", offerVersions: []quic.Version{quic.Version1}, wantVersion: QuicVersion1},
	}
	for _, c := range cases {
		got := captureQuicInitialFromClient(t, c.offerVersions)
		if drifts := DiffQuicInitial(got, QuicDiffOptions{ExpectedVersion: c.wantVersion}); len(drifts) != 0 {
			t.Errorf("%s: captured initial drifted: %v", c.description, drifts)
		}
		// the fail side: the same capture drifts against the other version.
		otherVersion := QuicVersion2
		if c.wantVersion == QuicVersion2 {
			otherVersion = QuicVersion1
		}
		if !driftHasField(DiffQuicInitial(got, QuicDiffOptions{ExpectedVersion: otherVersion}), "quic_version") {
			t.Errorf("%s: captured initial did not drift against the other version", c.description)
		}
	}
}

// captureQuicInitialFromClient starts the QUIC endpoint, dials it with a quic-go
// client offering offerVersions, and returns the captured first Initial parsed.
func captureQuicInitialFromClient(t *testing.T, offerVersions []quic.Version) *QuicInitialFingerprint {
	t.Helper()
	endpoint, err := NewQuicEndpoint(QuicEndpointOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(endpoint.Close)

	clientUdp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	transport := &quic.Transport{Conn: clientUdp}
	t.Cleanup(func() { transport.Close(); clientUdp.Close() })

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	conn, err := transport.Dial(ctx, endpoint.Addr(), &tls.Config{
		ServerName: ServerName,
		RootCAs:    endpoint.CaCertPool(),
		NextProtos: []string{"h3"},
	}, &quic.Config{Versions: offerVersions})
	if err != nil {
		t.Fatalf("quic dial: %s", err)
	}
	// the server read and captured the first Initial before the handshake this
	// dial just completed.
	_ = conn.CloseWithError(quic.ApplicationErrorCode(0), "")

	initials := endpoint.CapturedInitials()
	if len(initials) == 0 {
		t.Fatal("no quic initial captured")
	}
	fingerprint, err := ParseQuicInitial(initials[0])
	if err != nil {
		t.Fatal(err)
	}
	return fingerprint
}
