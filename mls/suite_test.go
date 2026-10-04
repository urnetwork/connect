// The registry is closed, holds exactly two entries, and discriminates between them.
// The last of those is the one worth stating: a lookup that returned the same suite
// whatever it was asked is exactly the hardcoded singleton the two entry design
// exists to rule out, and only a test that asks for both and compares the answers can
// see the difference. Both entries are therefore asserted whole, against literals
// written out here rather than read back from the registry, so this file disagrees
// with suite.go when suite.go changes.
package mls

import (
	"errors"
	"testing"
)

// TestRegistryHasExactlyTwoSuites pins the count and the order. Ascending by code
// point is what lets every other test index the slice.
func TestRegistryHasExactlyTwoSuites(t *testing.T) {
	suites := Suites()
	if len(suites) != 2 {
		t.Fatalf("registry has %d suites, want 2: %v", len(suites), suites)
	}
	if suites[0] != CipherSuiteX25519AesGcm128Sha256Ed25519 {
		t.Errorf("suites[0] = %#04x, want 0x0001", uint16(suites[0]))
	}
	if suites[1] != CipherSuiteX25519ChaCha20Sha256Ed25519 {
		t.Errorf("suites[1] = %#04x, want 0x0003", uint16(suites[1]))
	}
}

// TestChaChaSuiteParams pins every field of 0x0003, the suite groups are created at.
// A whole struct comparison rather than field by field checks, so a field added to
// SuiteParams later cannot be born unasserted.
func TestChaChaSuiteParams(t *testing.T) {
	params, err := LookupSuite(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("LookupSuite: %v", err)
	}
	want := SuiteParams{
		Suite:       CipherSuiteX25519ChaCha20Sha256Ed25519,
		Name:        "MLS_128_DHKEMX25519_CHACHA20POLY1305_SHA256_Ed25519",
		KemId:       0x0020,
		KdfId:       0x0001,
		AeadId:      0x0003,
		SignatureId: 0x0807,
		Nh:          32, Nk: 32, Nn: 12, Nt: 16,
		Nsecret: 32, Nenc: 32, Npk: 32, Nsk: 32,
		NsigPub: 32, NsigPriv: 32,
		Nsig: 64,
	}
	if *params != want {
		t.Fatalf("params = %+v, want %+v", *params, want)
	}
}

// TestAesGcmSuiteParams pins every field of 0x0001. It is registered and implemented
// here and refused at group creation by the policy check in group.go, so this is the
// only place its parameters are checked at all.
func TestAesGcmSuiteParams(t *testing.T) {
	params, err := LookupSuite(CipherSuiteX25519AesGcm128Sha256Ed25519)
	if err != nil {
		t.Fatalf("LookupSuite: %v", err)
	}
	want := SuiteParams{
		Suite:       CipherSuiteX25519AesGcm128Sha256Ed25519,
		Name:        "MLS_128_DHKEMX25519_AES128GCM_SHA256_Ed25519",
		KemId:       0x0020,
		KdfId:       0x0001,
		AeadId:      0x0001,
		SignatureId: 0x0807,
		Nh:          32, Nk: 16, Nn: 12, Nt: 16,
		Nsecret: 32, Nenc: 32, Npk: 32, Nsk: 32,
		NsigPub: 32, NsigPriv: 32,
		Nsig: 64,
	}
	if *params != want {
		t.Fatalf("params = %+v, want %+v", *params, want)
	}
}

// TestEveryListedSuiteResolves asserts the three entry points describe one registry:
// everything Suites lists is registered, resolves, and resolves to itself rather than
// to its neighbour. Without it a predicate hardwired to false passes every other test
// in this file, since nothing else asks it a question whose answer is yes.
func TestEveryListedSuiteResolves(t *testing.T) {
	suites := Suites()
	if len(suites) == 0 {
		t.Fatal("Suites is empty, so the loop below asserts nothing")
	}
	for _, suite := range suites {
		if !IsRegisteredSuite(suite) {
			t.Errorf("%#04x is listed by Suites but reports as unregistered", uint16(suite))
		}
		params, err := LookupSuite(suite)
		if err != nil {
			t.Errorf("LookupSuite(%#04x): %v", uint16(suite), err)
			continue
		}
		if params.Suite != suite {
			t.Errorf("LookupSuite(%#04x) returned the params of %#04x", uint16(suite), uint16(params.Suite))
		}
	}
}

// TestRegisteredSuitesAreNotInterchangeable states the property the two entry design
// is for: the entries must differ in what they determine about key material, or the
// registry is a singleton wearing two code points. The aead and its key size are
// where they differ; the nonce and tag sizes coincide because chacha20-poly1305 and
// aes-128-gcm happen to agree there, and asserting those apart would be asserting
// something false.
func TestRegisteredSuitesAreNotInterchangeable(t *testing.T) {
	aesGcm, err := LookupSuite(CipherSuiteX25519AesGcm128Sha256Ed25519)
	if err != nil {
		t.Fatalf("LookupSuite 0x0001: %v", err)
	}
	chaCha, err := LookupSuite(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("LookupSuite 0x0003: %v", err)
	}
	if aesGcm.AeadId == chaCha.AeadId {
		t.Errorf("both suites report aead %#04x", aesGcm.AeadId)
	}
	if aesGcm.Nk == chaCha.Nk {
		t.Errorf("both suites report a %d byte aead key", aesGcm.Nk)
	}
	if aesGcm.Name == chaCha.Name {
		t.Errorf("both suites are named %q", aesGcm.Name)
	}
	if aesGcm.Nn != chaCha.Nn {
		t.Errorf("nonce sizes %d and %d differ; both aeads take 12 bytes", aesGcm.Nn, chaCha.Nn)
	}
	if aesGcm.Nt != chaCha.Nt {
		t.Errorf("tag sizes %d and %d differ; both aeads produce 16 bytes", aesGcm.Nt, chaCha.Nt)
	}
}

// TestUnregisteredSuitesAreRefused covers the code points RFC 9420 defines and this
// package does not implement, plus the ends of the range. They must be refused rather
// than silently defaulted to 0x0003, and refused with nothing in hand: a zero valued
// SuiteParams returned beside the error reads as a suite with 0 byte keys to any
// caller that checks the wrong half of the pair first.
func TestUnregisteredSuitesAreRefused(t *testing.T) {
	for _, suite := range []CipherSuite{0x0000, 0x0002, 0x0004, 0x0005, 0x0006, 0x0007, 0xFFFF} {
		if IsRegisteredSuite(suite) {
			t.Errorf("suite %#04x reports as registered", uint16(suite))
		}
		params, err := LookupSuite(suite)
		if !errors.Is(err, ErrUnknownCipherSuite) {
			t.Errorf("LookupSuite(%#04x) error = %v, want ErrUnknownCipherSuite", uint16(suite), err)
		}
		if params != nil {
			t.Errorf("LookupSuite(%#04x) refused and returned params anyway: %+v", uint16(suite), *params)
		}
	}
}

// TestLookupSuiteDoesNotAliasTheRegistry asserts a caller that mutates the returned
// params cannot corrupt every later lookup. The pointer comparison is the second half
// of it: equal contents on the second read could also mean the caller's write went
// somewhere else entirely, and only distinct pointers show the copy is per call.
func TestLookupSuiteDoesNotAliasTheRegistry(t *testing.T) {
	a, err := LookupSuite(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("LookupSuite: %v", err)
	}
	a.Nk = 999
	b, err := LookupSuite(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("LookupSuite: %v", err)
	}
	if b.Nk != 32 {
		t.Fatalf("registry was mutated through a returned pointer: Nk = %d", b.Nk)
	}
	if a == b {
		t.Fatalf("LookupSuite returned the same pointer twice")
	}
}
