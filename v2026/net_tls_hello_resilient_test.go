//go:build unix || windows

package connect

// net_tls_hello_resilient_test.go — the Chrome hello record through the
// resilient layer (net_resilient.go): it parses into the fragment path with
// its server name found, fragments into records that join back into it, and
// goes out block by block under the reorder ttl alternation.

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"testing"
)

// The record a Chrome-hello dial writes first, its client hello, from the
// dial path's own handshaker. The handshake then fails: the peer closes after
// reading the record.
func captureTestChromeClientHelloRecord(t *testing.T) []byte {
	t.Helper()
	defaultTlsConfig, err := DefaultTlsConfig()
	if err != nil {
		t.Fatal(err)
	}
	config := newClientTlsConfig(defaultTlsConfig, clientWebSocketNextProtos)
	handshaker := newClientTlsHandshaker(TlsClientHelloFingerprintChrome, config)
	if !handshaker.chrome {
		t.Fatal("the websocket path does not present the Chrome hello")
	}
	dialConfig := config.Clone()
	dialConfig.ServerName = testTlsHelloServerName

	clientConn, serverConn := net.Pipe()
	handshakeErrs := make(chan error, 1)
	go func() {
		_, err := handshaker.handshake(t.Context(), clientConn, dialConfig)
		handshakeErrs <- err
	}()
	header := make([]byte, 5)
	if _, err := io.ReadFull(serverConn, header); err != nil {
		t.Fatal(err)
	}
	body := make([]byte, int(binary.BigEndian.Uint16(header[3:5])))
	if _, err := io.ReadFull(serverConn, body); err != nil {
		t.Fatal(err)
	}
	serverConn.Close()
	if err := <-handshakeErrs; err == nil {
		t.Fatal("a handshake completed against a peer that only read")
	}
	if header[0] != TlsContentTypeHandshake {
		t.Fatalf("first record type %d, want a handshake", header[0])
	}
	return append(header, body...)
}

// The Chrome hello parses into the fragment path with its server name found,
// which is what lets the fragmenting dialers split it.
func TestChromeClientHelloRecordParsesForFragmentation(t *testing.T) {
	record := captureTestChromeClientHelloRecord(t)
	t.Logf("Chrome hello record: %d bytes", len(record))
	clientHello, meta := UnmarshalClientHello(record[5:])
	if clientHello == nil || meta == nil {
		t.Fatal("the Chrome hello does not parse")
	}
	if clientHello.Info.ServerName == nil || *clientHello.Info.ServerName != testTlsHelloServerName {
		t.Fatalf("parsed server name %v, want %s", clientHello.Info.ServerName, testTlsHelloServerName)
	}
	if meta.ServerNameValueEnd <= meta.ServerNameValueStart {
		t.Fatalf("server name span %d..%d is empty", meta.ServerNameValueStart, meta.ServerNameValueEnd)
	}
}

// The fragment-only layer re-frames the Chrome hello into several records
// whose payloads join back into the hello.
func TestResilientTlsConnFragmentsChromeClientHello(t *testing.T) {
	record := captureTestChromeClientHelloRecord(t)
	client, server := newTcpPair(t)
	rconn := NewResilientTlsConn(client, true, false)
	n, err := rconn.Write(record)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != len(record) {
		t.Fatalf("write n=%d want %d", n, len(record))
	}

	var payload []byte
	recordCount := 0
	for len(payload) < len(record)-5 {
		header := make([]byte, 5)
		if _, err := io.ReadFull(server, header); err != nil {
			t.Fatalf("read record header: %v", err)
		}
		if header[0] != TlsContentTypeHandshake {
			t.Fatalf("record type %d, want a handshake", header[0])
		}
		body := make([]byte, int(binary.BigEndian.Uint16(header[3:5])))
		if _, err := io.ReadFull(server, body); err != nil {
			t.Fatalf("read record body: %v", err)
		}
		payload = append(payload, body...)
		recordCount += 1
	}
	if recordCount < 2 {
		t.Fatalf("the hello went out in %d record, want fragments", recordCount)
	}
	if !bytes.Equal(payload, record[5:]) {
		t.Fatal("the fragments do not join back into the hello")
	}
}

// The reorder-only layer sends the Chrome hello as raw 64-byte blocks under
// the alternating ttl, then restores the native ttl; the peer receives the
// hello unchanged. Mirrors TestResilientTlsConnReorderOnlyAlternatesTtl with
// the larger hello.
func TestResilientTlsConnReordersChromeClientHello(t *testing.T) {
	record := captureTestChromeClientHelloRecord(t)
	client, server := newTcpPair(t)
	setSocketTtl(t, client, 42)
	nativeTtl := 42
	// the block size of net_resilient.go's reorder-only path
	const blockSize = 64

	seam := &ttlSeam{passthrough: true}
	rconn := NewResilientTlsConn(client, false, true)
	rconn.setTtl = seam.set
	n, err := rconn.Write(record)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != len(record) {
		t.Fatalf("write n=%d want %d", n, len(record))
	}

	var want []int
	for i := 0; i*blockSize < len(record); i += 1 {
		if 0 == i%2 {
			want = append(want, resilientLowTtl)
		} else {
			want = append(want, nativeTtl)
		}
	}
	if len(seam.applied) < len(want)+1 {
		t.Fatalf("applied ttl sequence = %v, want the %d block ttls %v and a restore", seam.applied, len(want), want)
	}
	for i := range want {
		if seam.applied[i] != want[i] {
			t.Fatalf("applied ttl sequence = %v, want it to begin %v (first difference at %d)", seam.applied, want, i)
		}
	}
	for i := len(want); i < len(seam.applied); i += 1 {
		if seam.applied[i] != nativeTtl {
			t.Fatalf("applied ttl sequence = %v, want only native restores after the blocks; index %d is %d", seam.applied, i, seam.applied[i])
		}
	}

	got := make([]byte, len(record))
	if _, err := io.ReadFull(server, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, record) {
		t.Fatal("the peer received different bytes than the hello")
	}
}
