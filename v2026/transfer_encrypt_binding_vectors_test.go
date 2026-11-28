package connect

import (
	"bytes"
	"crypto/ecdh"
	"encoding/hex"
	"testing"
)

// Independent X25519/HKDF-SHA256 and Ed25519 fixtures generated with
// Python cryptography. Keep transcript order, directions and signed bytes stable.
func TestForwardingAndIdentityKnownAnswers(t *testing.T) {
	decode := func(s string) []byte {
		t.Helper()
		b, err := hex.DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	key, err := ecdh.X25519().NewPrivateKey(decode("0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20"))
	if err != nil {
		t.Fatal(err)
	}
	origin, destination := key.PublicKey().Bytes(), decode("5869aff450549732cbaaed5e5df9b30a6da31cb0e5742bad5ad4a1a768f1a67b")
	grant := streamForwardingGrant{path: TransferPath{SourceId: Id{1}, DestinationId: Id{2}, StreamId: Id{3}}, contractId: Id{4}, sequenceId: Id{5}, introductionId: Id{6}}
	keys, ok := grant.derive(key, destination, origin, destination)
	if !ok || !bytes.Equal(keys.transcript[:], decode("e6682643e8129c5cad764cacc3a3c77522ed87a50f8f7806ee1846644b5803db")) {
		t.Fatal("forwarding transcript changed")
	}
	var got []byte
	for _, part := range [][32]byte{keys.forwardReadiness, keys.reverseReadiness, keys.forwardApplication, keys.reverseApplication} {
		got = append(got, part[:]...)
	}
	if !bytes.Equal(got, decode("837d49fe40911611d790731d4346be03ba3d80bf3080517eb7f7993216448f7b514e5d7de4412374521821b4533d265f1dcd1fc16a31135ea2fc508fa99090aa820f1f1696c831d93f4a3cd04521e60646860a3feb7283be6a31181bdc467652c24f298f2f49b7431636e7c0ee91584b525e92740924a6b3a6fed4fe78549afa")) {
		t.Fatal("directional keys changed")
	}
	public, signature, exporter := decode("79b5562e8fe654f94078b112e8a98ba7901f853ae695bed7e0e3910bad049664"), decode("05888cd2c6a40481b75c89576e28578ed8c2b8ffe4a9086c0d63d32ecc1697381b5aef24c278e57a238754e1847e3f283eda0ccd7ac8017a16bdb19d5478e109"), decode("2122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f40")
	if !VerifyClientKeySignature(public, exporter, signature) {
		t.Fatal("identity proof rejected")
	}
	exporter[0] ^= 1
	if VerifyClientKeySignature(public, exporter, signature) {
		t.Fatal("proof applied to a different exporter")
	}
}
