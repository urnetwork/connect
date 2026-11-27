package connect

import (
	"bytes"
	"crypto/tls"
	"encoding/hex"
	"testing"
)

// The AES-256 GCM all-zero vector pins nonce placement, ciphertext and tag.
func TestSequenceRecordKnownAnswer(t *testing.T) {
	zero := make([]byte, 32)
	c, err := newSequenceRecordCipher(zero)
	if err != nil {
		t.Fatal(err)
	}
	want, err := hex.DecodeString("000000000000000000000000cea7403d4d606b6e074ec5d3baf39d18d0d1c8a799996bf0265b98b5d48ab919")
	if err != nil {
		t.Fatal(err)
	}
	plain := make([]byte, 16)
	if got := c.sealRecord(make([]byte, 12), plain); !bytes.Equal(got, want) {
		t.Fatalf("record = %x", got)
	}
	opened, err := c.Open(want)
	if err != nil || !bytes.Equal(opened, plain) {
		t.Fatal("known record did not open", err)
	}
	MessagePoolReturn(opened)
	changed := bytes.Clone(want)
	changed[len(changed)-1] ^= 1
	other, err := newSequenceRecordCipher(bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	for _, pair := range []struct {
		cipher *sequenceCipher
		wire   []byte
	}{
		{cipher: c, wire: changed}, {cipher: c, wire: want[:27]}, {cipher: other, wire: want},
	} {
		if result, err := pair.cipher.Open(pair.wire); err == nil || result != nil {
			t.Fatal("invalid record opened")
		}
	}
	for _, n := range []int{0, 16, 24, 33} {
		if _, err := newSequenceRecordCipher(make([]byte, n)); err == nil {
			t.Fatalf("accepted %d-byte key", n)
		}
	}
}

// Both sides use the same two exporter labels, and derive neither value early.
func TestSequenceExportersBindBothPeers(t *testing.T) {
	if cipher, proof, err := deriveSequenceSecrets(tls.ConnectionState{}); err == nil || cipher != nil || proof != nil {
		t.Fatal("derived incomplete handshake")
	}
	certificate, _ := newCertEpochCertificate(t)
	client, server := newCertEpochTlsPair(t, certificate)
	c, cp, err := deriveSequenceSecrets(client.ConnectionState())
	if err != nil {
		t.Fatal(err)
	}
	s, sp, err := deriveSequenceSecrets(server.ConnectionState())
	if err != nil {
		t.Fatal(err)
	}
	state := server.ConnectionState()
	key, err := state.ExportKeyingMaterial("urnetwork-connect-aead", nil, 32)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := state.ExportKeyingMaterial("urnetwork-connect-identity-proof", nil, 32)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(key, cp) || !bytes.Equal(cp, sp) || !bytes.Equal(proof, sp) {
		t.Fatal("exporter agreement or label separation failed")
	}
	reference, err := newSequenceRecordCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	record := c.sealRecord(make([]byte, 12), cp)
	if !bytes.Equal(record, reference.sealRecord(make([]byte, 12), cp)) {
		t.Fatal("record exporter label changed")
	}
	plain, err := s.Open(record)
	if err != nil {
		t.Fatal(err)
	}
	defer MessagePoolReturn(plain)
	if !bytes.Equal(plain, sp) {
		t.Fatal("peer record mismatch")
	}
}

// Handshake role, phase and generation are sufficient to choose an action.
func TestSequenceHandshakeTransition(t *testing.T) {
	low, high := Id{1}, Id{2}
	for _, c := range []struct {
		role                 sequenceTlsRole
		hello, live, ordered bool
		current, incoming    Id
		want                 sequenceHandshakeAction
	}{
		{role: sequenceTlsRoleClient, hello: true, live: true, current: low, incoming: high, want: handshakeIgnore},
		{role: sequenceTlsRoleServer, live: true, current: low, incoming: high, want: handshakeIgnore},
		{role: sequenceTlsRoleServer, hello: true, live: true, ordered: true, current: high, incoming: low, want: handshakeIgnore},
		{role: sequenceTlsRoleServer, hello: true, live: true, current: high, incoming: low, want: handshakeStart},
		{role: sequenceTlsRoleServer, hello: true, ordered: true, current: high, incoming: low, want: handshakeStart},
		{role: sequenceTlsRoleServer, hello: true, live: true, incoming: low, want: handshakeDeliver},
		{role: sequenceTlsRoleClient, live: true, current: low, want: handshakeDeliver},
	} {
		if got := selectSequenceHandshake(c.role, c.hello, c.live, c.ordered, c.current, c.incoming); got != c.want {
			t.Errorf("%+v got %v", c, got)
		}
	}
}
