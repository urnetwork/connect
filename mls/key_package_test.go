// The tests of the RFC 9420 section 10 KeyPackage: its construction, its reference, its
// validation, and the two properties of key_package.go that nothing behavioural can see.
//
// Three of the tests below exist because of a substitution rather than because of a rule, and
// each names the substitution it was written against:
//
//   - TestKeyPackageValidateReadsTheClockItWasHanded, against a Validate that never reads its
//     now argument. Every test that drives one timestamp passes over that body, and this
//     project has shipped exactly that shape at another layer.
//   - TestNewKeyPackageDrawsTheInitAndEncryptionKeysFromSeparateEntropy, against a constructor
//     that answers one key pair twice. It survived a whole green suite one plan ago, because
//     nothing that round trips or that checks a length can see two keys that are one.
//   - TestTheKeyPackageSignaturePreimageIsAssembledExactlyOnce, against a second assembly of
//     the signed prefix written with the fields in the same order. That one produces the same
//     bytes, so it has no behaviour at all to observe and the gate is over the source.
package mls

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/mls/syntax"
)

// The suite every key package here is built at, written once so that a test which means to
// vary the suite is visibly doing so.
const keyPackageTestSuite = CipherSuiteX25519ChaCha20Sha256Ed25519

func testKeyPackageCapabilities() Capabilities {
	return Capabilities{
		Versions:     []ProtocolVersion{ProtocolVersionMls10},
		CipherSuites: []CipherSuite{keyPackageTestSuite},
		Extensions:   []ExtensionType{ExtensionTypeUrmessageLeafKeys},
		Proposals:    []ProposalType{ProposalTypeAdd, ProposalTypeUpdate, ProposalTypeRemove},
		Credentials:  []CredentialType{CredentialTypeBasic},
	}
}

// One key package over a provider of its own, with both HPKE private halves.
func newTestKeyPackage(t *testing.T) (CryptoProvider, *KeyPackage, HpkePrivateKey, HpkePrivateKey) {
	t.Helper()
	crypto, err := NewCryptoProvider(keyPackageTestSuite)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	kp, initPriv, encPriv, err := NewKeyPackage(crypto, keyPackageTestSuite,
		BasicCredential([]byte("alice")), testKeyPackageCapabilities(), nil)
	if err != nil {
		t.Fatalf("NewKeyPackage: %v", err)
	}
	return crypto, kp, initPriv, encPriv
}

// testResignKeyPackage puts a valid signature back on a key package a test has altered, using
// the seed the constructor kept.
//
// It exists so that a test can reach a refusal that lives BELOW the signature check. Validate
// verifies the key package signature before it hands the leaf on, and every field of the leaf
// is inside that signature, so an altered leaf is refused as a forgery and the rule the test
// was aiming at is never reached.
func testResignKeyPackage(t *testing.T, crypto CryptoProvider, kp *KeyPackage) {
	t.Helper()
	content, err := kp.signedPreimage()
	if err != nil {
		t.Fatalf("signedPreimage: %v", err)
	}
	signature, err := crypto.SignWithLabel(kp.signPriv, keyPackageSignatureLabel, content)
	if err != nil {
		t.Fatalf("SignWithLabel: %v", err)
	}
	kp.Signature = signature
}

func TestNewKeyPackageRoundTripsAndValidates(t *testing.T) {
	crypto, err := NewCryptoProvider(keyPackageTestSuite)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	leafKeys := &LeafKeysExtension{
		AlgId:          AlgIdXwing,
		DeviceXwingPub: make([]byte, XwingPublicKeyLen),
	}
	ext, err := leafKeys.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	kp, initPriv, encPriv, err := NewKeyPackage(crypto, keyPackageTestSuite,
		BasicCredential([]byte("alice")), testKeyPackageCapabilities(), []Extension{ext})
	if err != nil {
		t.Fatalf("NewKeyPackage: %v", err)
	}
	if len(initPriv) == 0 || len(encPriv) == 0 {
		t.Fatalf("NewKeyPackage returned empty private keys")
	}
	if bytes.Equal(initPriv, encPriv) {
		t.Fatalf("the init and encryption key pairs are the same")
	}
	if bytes.Equal(kp.InitKey, kp.LeafNode.EncryptionKey) {
		t.Fatalf("init_key equals the leaf encryption key")
	}
	if kp.Version != ProtocolVersionMls10 || kp.CipherSuite != keyPackageTestSuite {
		t.Fatalf("version %d suite %#04x, want %d and %#04x",
			kp.Version, uint16(kp.CipherSuite), ProtocolVersionMls10, uint16(keyPackageTestSuite))
	}
	// the extensions argument is the LEAF's, which is where this profile puts
	// urmessage_leaf_keys; see NewKeyPackage's own comment
	if len(kp.LeafNode.Extensions) != 1 || kp.LeafNode.Extensions[0].ExtensionType != ExtensionTypeUrmessageLeafKeys {
		t.Fatalf("the leaf carries %v, want the urmessage_leaf_keys extension it was handed",
			kp.LeafNode.Extensions)
	}
	if err := kp.Validate(crypto, keyPackageTestSuite, time.Now()); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	encoded, err := syntax.Marshal(kp)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	out := &KeyPackage{}
	if err := syntax.Unmarshal(encoded, out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	reencoded, err := syntax.Marshal(out)
	if err != nil {
		t.Fatalf("re-Marshal: %v", err)
	}
	if !bytes.Equal(reencoded, encoded) {
		t.Fatalf("re-encode differs")
	}
	if err := out.Validate(crypto, keyPackageTestSuite, time.Now()); err != nil {
		t.Fatalf("decoded Validate: %v", err)
	}
	if err := syntax.Unmarshal(append(encoded, 0x00), &KeyPackage{}); !errors.Is(err, syntax.ErrTrailingBytes) {
		t.Fatalf("trailing byte err = %v, want ErrTrailingBytes", err)
	}
}

func TestKeyPackageRefIsStableAndBindsEveryField(t *testing.T) {
	crypto, kp, _, _ := newTestKeyPackage(t)
	ref, err := kp.Ref(crypto)
	if err != nil {
		t.Fatalf("Ref: %v", err)
	}
	if len(ref) != crypto.HashSize() {
		t.Fatalf("ref length = %d, want %d", len(ref), crypto.HashSize())
	}
	again, err := kp.Ref(crypto)
	if err != nil {
		t.Fatalf("Ref: %v", err)
	}
	if !bytes.Equal(ref, again) {
		t.Fatalf("Ref is not deterministic")
	}
	other, _, _, err := NewKeyPackage(crypto, keyPackageTestSuite,
		BasicCredential([]byte("alice")), testKeyPackageCapabilities(), nil)
	if err != nil {
		t.Fatalf("NewKeyPackage: %v", err)
	}
	otherRef, err := other.Ref(crypto)
	if err != nil {
		t.Fatalf("Ref: %v", err)
	}
	if bytes.Equal(ref, otherRef) {
		t.Fatalf("two key packages with fresh keys share a ref")
	}
}

// TestKeyPackageRefCoversTheSignatureAndNotOnlyTheSignedPrefix is the reference's own
// statement of WHICH bytes it hashes, and it is here because the two candidates differ by one
// field and agree on everything a length or a determinism check can ask.
//
// A Ref taken over the KeyPackageTBS prefix is the same length, is just as deterministic, and
// differs between any two key packages built from fresh keys -- so the test above passes over
// it unchanged. What it stops doing is distinguishing two key packages that carry the same
// fields under different signatures, which is exactly the pair a commit has to be able to
// name apart: a member holding one and a member holding the other would agree they were adding
// the same joiner while holding two different structures, and every later tree hash disagrees.
func TestKeyPackageRefCoversTheSignatureAndNotOnlyTheSignedPrefix(t *testing.T) {
	crypto, kp, _, _ := newTestKeyPackage(t)
	ref, err := kp.Ref(crypto)
	if err != nil {
		t.Fatalf("Ref: %v", err)
	}
	// the independent computation, assembled here rather than read back through Ref
	encoded, err := syntax.Marshal(kp)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if want := mustKeyPackageRef(t, crypto, encoded); !bytes.Equal(ref, want) {
		t.Fatalf("Ref answered %x and RefHash over the whole encoding is %x", ref, want)
	}
	// and the candidate it must NOT be
	tbs, err := kp.signedPreimage()
	if err != nil {
		t.Fatalf("signedPreimage: %v", err)
	}
	if bytes.Equal(ref, mustKeyPackageRef(t, crypto, tbs)) {
		t.Fatalf("Ref hashed the KeyPackageTBS prefix; two key packages differing only in their signature would then share a reference")
	}
	// the property that says so without reference to either assembly: moving the signature
	// alone moves the ref
	moved := *kp
	moved.Signature = append([]byte(nil), kp.Signature...)
	moved.Signature[0] ^= 0x01
	movedRef, err := moved.Ref(crypto)
	if err != nil {
		t.Fatalf("Ref: %v", err)
	}
	if bytes.Equal(ref, movedRef) {
		t.Fatalf("the ref did not move when the signature did, so it is not taken over the whole key package")
	}
}

func TestKeyPackageValidateRejects(t *testing.T) {
	crypto, err := NewCryptoProvider(keyPackageTestSuite)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	build := func(t *testing.T) *KeyPackage {
		t.Helper()
		kp, _, _, err := NewKeyPackage(crypto, keyPackageTestSuite,
			BasicCredential([]byte("alice")), testKeyPackageCapabilities(), nil)
		if err != nil {
			t.Fatalf("NewKeyPackage: %v", err)
		}
		return kp
	}

	wrongVersion := build(t)
	wrongVersion.Version = ProtocolVersion(0x0002)
	testResignKeyPackage(t, crypto, wrongVersion)
	if err := wrongVersion.Validate(crypto, keyPackageTestSuite, time.Now()); !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("version mismatch err = %v, want ErrUnsupportedVersion", err)
	}

	wrongSuite := build(t)
	if err := wrongSuite.Validate(crypto, CipherSuiteX25519AesGcm128Sha256Ed25519, time.Now()); !errors.Is(err, errProfileCiphersuite) {
		t.Fatalf("suite mismatch err = %v, want errProfileCiphersuite", err)
	}
	// and the same refusal reached from the other side: the key package names another suite
	// and the group runs this one, which is the direction a joiner's advertisement arrives in
	otherSuite := build(t)
	otherSuite.CipherSuite = CipherSuiteX25519AesGcm128Sha256Ed25519
	testResignKeyPackage(t, crypto, otherSuite)
	if err := otherSuite.Validate(crypto, keyPackageTestSuite, time.Now()); !errors.Is(err, errProfileCiphersuite) {
		t.Fatalf("advertised suite mismatch err = %v, want errProfileCiphersuite", err)
	}

	tampered := build(t)
	tampered.InitKey = HpkePublicKey(bytes.Repeat([]byte{0xEE}, len(tampered.InitKey)))
	err = tampered.Validate(crypto, keyPackageTestSuite, time.Now())
	if !errors.Is(err, errKeyPackageBadSignature) {
		t.Fatalf("tampered init key err = %v, want errKeyPackageBadSignature", err)
	}
	// the broad question the wrap keeps answerable, which is what a caller with no interest in
	// which structure failed asks
	if !errors.Is(err, errBadSignature) {
		t.Fatalf("tampered init key err = %v, which does not answer the package's signature sentinel", err)
	}

	// an update-source leaf, NOT re-signed: it is refused as a forgery, because the source is
	// inside the key package's own preimage
	wrongSource := build(t)
	wrongSource.LeafNode.LeafNodeSource = LeafNodeSourceUpdate
	if err := wrongSource.Validate(crypto, keyPackageTestSuite, time.Now()); !errors.Is(err, errKeyPackageBadSignature) {
		t.Fatalf("an update-source leaf err = %v, want errKeyPackageBadSignature", err)
	}
	// and re-signed, which is what a hostile peer would send: now the leaf's own section 7.3
	// source rule is what refuses it, which is the delegation this Validate is made of
	resigned := build(t)
	resigned.LeafNode.LeafNodeSource = LeafNodeSourceUpdate
	testResignKeyPackage(t, crypto, resigned)
	if err := resigned.Validate(crypto, keyPackageTestSuite, time.Now()); !errors.Is(err, ErrLeafNodeSourceMismatch) {
		t.Fatalf("a re-signed update-source leaf err = %v, want ErrLeafNodeSourceMismatch", err)
	}

	expired := build(t)
	far := time.Unix(int64(expired.LeafNode.Lifetime.NotAfter)+2*3600, 0)
	if err := expired.Validate(crypto, keyPackageTestSuite, far); !errors.Is(err, ErrLeafNodeLifetime) {
		t.Fatalf("expired err = %v, want ErrLeafNodeLifetime", err)
	}
}

// TestKeyPackageValidateReadsTheClockItWasHanded is the one test in this file written against a
// body rather than against a rule.
//
// Validate takes a now, and a Validate that never reads it -- one that passed 0 for NowMs,
// which LeafValidationContext documents as an opt out, or one that stamped time.Now() over the
// argument -- answers nil for every key package this package mints and passes every test above.
// All of them drive one timestamp, taken from the same clock the constructor just stamped the
// lifetime off, so none of them can see it. This one holds ONE key package against three
// instants and a fourth that is not a clock at all, and the key package is built once so that
// the only thing moving between the cases is the argument.
func TestKeyPackageValidateReadsTheClockItWasHanded(t *testing.T) {
	crypto, kp, _, _ := newTestKeyPackage(t)
	notBefore := int64(kp.LeafNode.Lifetime.NotBefore)
	notAfter := int64(kp.LeafNode.Lifetime.NotAfter)
	skew := int64(leafLifetimeSkewSeconds)
	if notAfter <= notBefore {
		t.Fatalf("the minted lifetime is %d..%d, which contains no instant to be inside of",
			notBefore, notAfter)
	}

	cases := []struct {
		what string
		now  time.Time
		want error
	}{
		{
			what: "a minute before not_before, past the skew this validator tolerates",
			now:  time.Unix(notBefore-skew-60, 0),
			want: ErrLeafNodeLifetime,
		},
		{
			what: "halfway through the lifetime",
			now:  time.Unix(notBefore+(notAfter-notBefore)/2, 0),
			want: nil,
		},
		{
			what: "a minute after not_after, past the skew this validator tolerates",
			now:  time.Unix(notAfter+skew+60, 0),
			want: ErrLeafNodeLifetime,
		},
		{
			// the zero time.Time is a clock nobody set, and it must not become the
			// documented NowMs opt out on the way in; see the clamp in Validate
			what: "the zero time, which is a machine whose clock is not set",
			now:  time.Time{},
			want: ErrLeafNodeLifetime,
		},
	}
	// the cases must actually differ, or this test drives one instant under four names
	seen := map[int64]string{}
	for _, one := range cases {
		if already, repeated := seen[one.now.Unix()]; repeated {
			t.Fatalf("%q and %q are the same instant, so this test varies nothing", already, one.what)
		}
		seen[one.now.Unix()] = one.what
	}

	for _, one := range cases {
		err := kp.Validate(crypto, keyPackageTestSuite, one.now)
		if one.want == nil {
			if err != nil {
				t.Errorf("%s: Validate = %v, want nil", one.what, err)
			}
			continue
		}
		if !errors.Is(err, one.want) {
			t.Errorf("%s: Validate = %v, want %v -- a validator that answers the same thing at every instant is not reading the clock it was handed",
				one.what, err, one.want)
		}
	}
}

// TestKeyPackageSignsUnderTheRfcLabel pins the label and shows the signature verifying under
// it, taken apart from Validate.
//
// The label is the whole of what stops a key package signature being a valid signature over
// some other structure the same key signed, and the same key signs a LeafNodeTBS inside this
// very structure -- so of every neighbour in this package, that is the one the separation has
// to hold against. That the primitive is label bound at all is crypto_labels_test.go's; what
// is here is that THIS construction reaches it with the RFC's string.
func TestKeyPackageSignsUnderTheRfcLabel(t *testing.T) {
	if keyPackageSignatureLabel != "KeyPackageTBS" {
		t.Fatalf("the key package signature label is %q, and RFC 9420 section 10 writes KeyPackageTBS",
			keyPackageSignatureLabel)
	}
	crypto, kp, _, _ := newTestKeyPackage(t)
	content, err := kp.signedPreimage()
	if err != nil {
		t.Fatalf("signedPreimage: %v", err)
	}
	if err := crypto.VerifyWithLabel(kp.LeafNode.SignatureKey, "KeyPackageTBS",
		content, kp.Signature); err != nil {
		t.Fatalf("the signature does not verify under the literal RFC label over the KeyPackageTBS bytes: %v", err)
	}
	// every other label this package signs under, plus the two spellings a reader's eye slides
	// over. The identifiers are in-package, so a label renamed away fails to compile here
	// rather than leaving this list naming a neighbour that no longer exists.
	for _, label := range []string{
		leafNodeSignatureLabel, framedContentTBSLabel, updatePathNodeLabel, "KeyPackageTbs", "",
	} {
		if label == keyPackageSignatureLabel {
			t.Fatalf("%q is offered here as a label the key package signature must NOT verify under, and it is the label it signs under",
				label)
		}
		if err := crypto.VerifyWithLabel(kp.LeafNode.SignatureKey, label,
			content, kp.Signature); err == nil {
			t.Errorf("the key package signature verifies under %q as well, so the label separates nothing", label)
		}
	}
}

// TestNewKeyPackageDrawsTheInitAndEncryptionKeysFromSeparateEntropy holds the constructor to
// producing two key pairs rather than one used twice.
//
// This is the entropy substitution, and it is the reason the test opens a message under each
// public half with the private half it was handed rather than comparing the two byte strings.
// A comparison catches the crude form -- one draw feeding both DeriveKeyPair calls -- and
// catches neither of the two next to it: a constructor that answers the encryption private key
// in the init position, and one that publishes the encryption public key as the init_key. Both
// hand back a key package that encodes, validates, refs and round trips, and both leave the
// joiner unable to open the Welcome that was sealed to what it published.
//
// RFC 9420 has these as two keys because they are used by different parties at different
// times: the init key opens the Welcome that admits this member, the encryption key opens
// every commit's path secret afterwards. A member whose two keys are one is a member for whom
// compromising either compromises both, for the life of the group.
func TestNewKeyPackageDrawsTheInitAndEncryptionKeysFromSeparateEntropy(t *testing.T) {
	crypto, kp, initPriv, encPriv := newTestKeyPackage(t)
	if bytes.Equal(initPriv, encPriv) {
		t.Fatalf("the two private halves are one key")
	}
	if bytes.Equal(kp.InitKey, kp.LeafNode.EncryptionKey) {
		t.Fatalf("init_key and the leaf's encryption_key are one key")
	}

	probe := []byte("the message a joiner has to be able to open")
	info := []byte("key package entropy probe")
	sealTo := func(t *testing.T, what string, pub HpkePublicKey) ([]byte, []byte) {
		t.Helper()
		kemOutput, ciphertext, err := crypto.HpkeSeal(pub, info, nil, probe)
		if err != nil {
			t.Fatalf("seal to the %s: %v", what, err)
		}
		return kemOutput, ciphertext
	}
	opens := func(priv HpkePrivateKey, kemOutput []byte, ciphertext []byte) bool {
		opened, err := crypto.HpkeOpen(priv, kemOutput, info, nil, ciphertext)
		return err == nil && bytes.Equal(opened, probe)
	}

	initKem, initCiphertext := sealTo(t, "published init_key", kp.InitKey)
	if !opens(initPriv, initKem, initCiphertext) {
		t.Errorf("the init private key this constructor returned does not open a message sealed to the init_key it published; the caller holds a key package it cannot be admitted with")
	}
	if opens(encPriv, initKem, initCiphertext) {
		t.Errorf("the encryption private key opens a message sealed to the init_key, so the two key pairs are one")
	}

	encKem, encCiphertext := sealTo(t, "leaf encryption_key", kp.LeafNode.EncryptionKey)
	if !opens(encPriv, encKem, encCiphertext) {
		t.Errorf("the encryption private key this constructor returned does not open a message sealed to the leaf's encryption_key; the caller holds a leaf it cannot decrypt a commit at")
	}
	if opens(initPriv, encKem, encCiphertext) {
		t.Errorf("the init private key opens a message sealed to the leaf's encryption_key, so the two key pairs are one")
	}
}

// TestNewKeyPackageKeepsTheSigningSeedOffTheWireAndBesideItsOwnLeaf is the field the plan calls
// signPriv, in both directions.
//
// Beside its own leaf: the seed has to be the one the leaf named as its signature_key, or the
// group lifecycle plan assembles JoinKeyMaterial around a key that signs nothing the group will
// accept, and the first Update that member sends is refused with nothing to point at.
//
// Off the wire: it is a private key, and a decode over a receiver that already held one has to
// clear it. Otherwise a caller that decoded a stranger's key package into a value its own
// constructor had filled in holds that stranger's public half beside its own signing seed, and
// nothing in the value says the two do not belong together.
func TestNewKeyPackageKeepsTheSigningSeedOffTheWireAndBesideItsOwnLeaf(t *testing.T) {
	_, kp, _, _ := newTestKeyPackage(t)
	if len(kp.signPriv) == 0 {
		t.Fatalf("NewKeyPackage kept no signature seed, so nothing can sign this member's later updates")
	}
	pub, err := signaturePublicKeyOf(kp.signPriv)
	if err != nil {
		t.Fatalf("signaturePublicKeyOf: %v", err)
	}
	if !bytes.Equal(pub, kp.LeafNode.SignatureKey) {
		t.Fatalf("the kept seed derives %x and the leaf names %x as its signature_key",
			pub, kp.LeafNode.SignatureKey)
	}

	encoded, err := syntax.Marshal(kp)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if bytes.Contains(encoded, kp.signPriv) {
		t.Fatalf("the signature seed is inside the %d encoded octets of the key package", len(encoded))
	}
	fresh := &KeyPackage{}
	if err := syntax.Unmarshal(encoded, fresh); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(fresh.signPriv) != 0 {
		t.Fatalf("a decoded key package carries a signature seed")
	}
	// the direction that needs a decoder which STAGES: a receiver that already held one
	reused := &KeyPackage{}
	*reused = *kp
	if err := syntax.Unmarshal(encoded, reused); err != nil {
		t.Fatalf("Unmarshal over a used receiver: %v", err)
	}
	if len(reused.signPriv) != 0 {
		t.Fatalf("a decode over a receiver that held a signature seed left it there, beside a public key that came off the wire")
	}
}

// ---------------------------------------------------------------------------
// the one assembly of the signed preimage
// ---------------------------------------------------------------------------

// mlsEncodingEmitters is every identifier that writes part of an MLS encoding, derived from
// three sources and listed in none of them.
//
// Derived because a list is the defect this gate exists to catch, one level up. The obvious
// spelling of this class is "the Write methods somebody thought of", and the encoder that a
// second assembly would actually be written with is whichever one that list forgot. So the
// class is: every Write method the compiler sees on *syntax.Writer, every method of
// syntax.Marshaler, and every package level function of this package's own non test source
// whose name begins with write in either case -- which is where WriteExtensions and
// writeUint16Vec come from without either being typed here.
//
// The anchors below are a guard on the SCAN and not on the class: a derivation that read
// nothing reports the same clean bill a complete one reports.
func mlsEncodingEmitters(t *testing.T) map[string]bool {
	t.Helper()
	emitters := map[string]bool{}
	// PRINT THE COMPLEMENT, AND PROVE THE SENTENCE THAT DESCRIBES IT. This name test removes
	// four methods of *syntax.Writer -- Bytes, Err, Len and MaxVectorLength -- and until now it
	// removed them in silence, which is the shape GATES.md indexes: a name-shaped narrowing over
	// a class the compiler can describe, with a complement nobody had read. The sentence that
	// puts them out is "they take nothing and answer the writer accumulated state; an emitter
	// puts something INTO it", and that sentence is a SHAPE, so it is read off the type
	// independently below and the two readings are required to agree. A Write-named method
	// taking no argument, or an emitter named anything else, is fatal here rather than joining
	// or leaving this class unremarked.
	writer := reflect.TypeOf(&syntax.Writer{})
	namedWrite, takesSomething, notEmitters := []string{}, []string{}, []string{}
	for i := 0; i < writer.NumMethod(); i++ {
		method := writer.Method(i)
		if strings.HasPrefix(method.Name, "Write") {
			namedWrite = append(namedWrite, method.Name)
			emitters[method.Name] = true
		} else {
			notEmitters = append(notEmitters, method.Name+" "+method.Type.String())
		}
		if method.Type.NumIn() > 1 {
			takesSomething = append(takesSomething, method.Name)
		}
	}
	if !slices.Equal(namedWrite, takesSomething) {
		t.Fatalf("the Write-named methods of *syntax.Writer are %v and the ones that take something to write are %v; this name test is only the property while those two are the same set",
			namedWrite, takesSomething)
	}
	if len(notEmitters) == 0 {
		t.Fatal("every method of *syntax.Writer is named Write<something>, so this name test removes nothing and its complement cannot be read")
	}
	t.Logf("%d emitters read off *syntax.Writer; the %d methods this name removes, each of which takes nothing and answers the writer own state: %v",
		len(namedWrite), len(notEmitters), notEmitters)
	marshaler := reflect.TypeOf((*syntax.Marshaler)(nil)).Elem()
	for i := 0; i < marshaler.NumMethod(); i++ {
		emitters[marshaler.Method(i).Name] = true
	}
	for name, file := range packageLevelDeclarations(t, ".") {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		if strings.HasPrefix(strings.ToLower(name), "write") {
			emitters[name] = true
		}
	}
	for _, anchor := range []string{"WriteUint16", "WriteOpaque", "MarshalMLS", "WriteExtensions"} {
		if !emitters[anchor] {
			t.Fatalf("the emitter derivation read %d names and %s is not among them, so it read something other than this package and the syntax writer",
				len(emitters), anchor)
		}
	}
	return emitters
}

// declarationsEmittingIn is every function of one parsed file whose body calls an emitter.
//
// Calls and not mentions, deliberately. key_package.go's signedPreimage hands marshalCore to
// marshalBytes as a VALUE, which is the whole point of it -- one assembly, reached rather than
// repeated -- and a scan over mentions would read that as a second writer.
func declarationsEmittingIn(parsed parsedSource, emitters map[string]bool) []string {
	return declarationsEmittingWhere(parsed, emitters, func(parsedSource, *ast.FuncDecl) bool { return true })
}

// declarationsEmittingWhere is the same walk narrowed to the declarations a caller's filter
// keeps, which is what lets one scan ask about one structure's codec across a whole package.
//
// The split exists because "every declaration that emits" is the right class for one file and
// far too wide for a package: mls declares dozens of codecs and all of them emit. The narrowing
// has to happen inside this walk rather than in a second one, or the widened gate below would be
// a re-implementation with its own bugs, which is how a gate ends up agreeing with itself.
func declarationsEmittingWhere(parsed parsedSource, emitters map[string]bool,
	keep func(parsedSource, *ast.FuncDecl) bool) []string {
	emitting := []string{}
	for _, declaration := range parsed.file.Decls {
		function, isFunction := declaration.(*ast.FuncDecl)
		if !isFunction || function.Body == nil || !keep(parsed, function) {
			continue
		}
		emits := false
		ast.Inspect(function.Body, func(node ast.Node) bool {
			call, isCall := node.(*ast.CallExpr)
			if !isCall {
				return true
			}
			switch callee := call.Fun.(type) {
			case *ast.Ident:
				emits = emits || emitters[callee.Name]
			case *ast.SelectorExpr:
				emits = emits || emitters[callee.Sel.Name]
			}
			return true
		})
		if emits {
			emitting = append(emitting, function.Name.Name)
		}
	}
	slices.Sort(emitting)
	return emitting
}

// keyPackageOwnFieldNames is every field name the KeyPackage declares and no other structure of
// this package's non test source does.
//
// It is the arm of the subject filter that a free function cannot get around by taking the
// structure apart. A second assembly written as a package level helper over a KeyPackage names
// the type in its signature and is caught by the first arm; one written over the fields does
// not, but it still has to write the init key, and InitKey is a name only this structure
// declares.
//
// DERIVED by walking every struct declaration of the package rather than typed out, because
// which of this structure's names are its own changes as its neighbours grow fields: Version and
// CipherSuite are GroupContext's too, Extensions and Signature belong to half the package, and a
// list written today is a list of the wrong names a plan from now.
func keyPackageOwnFieldNames(t *testing.T, sources []parsedSource) []string {
	t.Helper()
	subject := reflect.TypeOf(KeyPackage{}).Name()
	mine := []string{}
	elsewhere := map[string]bool{}
	for _, parsed := range sources {
		for _, declaration := range parsed.file.Decls {
			types, isTypeDeclaration := declaration.(*ast.GenDecl)
			if !isTypeDeclaration || types.Tok != token.TYPE {
				continue
			}
			for _, specification := range types.Specs {
				named, isNamed := specification.(*ast.TypeSpec)
				if !isNamed {
					continue
				}
				structure, isStruct := named.Type.(*ast.StructType)
				if !isStruct {
					continue
				}
				for _, field := range structure.Fields.List {
					for _, fieldName := range field.Names {
						if named.Name.Name == subject {
							mine = append(mine, fieldName.Name)
							continue
						}
						elsewhere[fieldName.Name] = true
					}
				}
			}
		}
	}
	own := []string{}
	for _, name := range mine {
		if !elsewhere[name] {
			own = append(own, name)
		}
	}
	slices.Sort(own)
	if len(own) == 0 {
		t.Fatalf("no field name is the %s's alone among this package's structures, so the subject filter below is one arm short and states less than it reads",
			subject)
	}
	return own
}

// keyPackageIsTheSubjectOf answers whether one declaration's subject is the key package.
//
// Two arms and both derived. The first is the structure itself: a method on it, or a parameter
// or result that names it. Rendered as TYPES and never as the text of the whole signature,
// because crypto_labels.go's MakeKeyPackageRef takes a parameter NAMED keyPackage carrying
// []byte, and a match on the spelling would pull the reference hash into a gate about the
// preimage. The second is keyPackageOwnFieldNames, which is what a helper written over the
// fields rather than over the value still has to name.
//
// The honest limit, stated rather than left for a reader to find. A helper declared in another
// file that took the five fields as its own parameters, under its own spellings, and was called
// from signedPreimage escapes both arms: it names no KeyPackage and mentions no field of one.
// What this gate is for is the second assembly somebody writes because they did not know the
// first existed, and that one is written over the structure. A deliberate one is not in reach of
// a source scan at all, and the behavioural half -- that the bytes signed are the bytes
// marshalled -- is what TestKeyPackageRefCoversTheSignatureAndNotOnlyTheSignedPrefix and the
// tampering rows of TestKeyPackageValidateRejects hold.
func keyPackageIsTheSubjectOf(parsed parsedSource, function *ast.FuncDecl, own []string) bool {
	subject := reflect.TypeOf(KeyPackage{}).Name()
	declared := []string{parsed.receiverOf(function)}
	for _, field := range function.Type.Params.List {
		declared = append(declared, parsed.render(field.Type))
	}
	if function.Type.Results != nil {
		for _, field := range function.Type.Results.List {
			declared = append(declared, parsed.render(field.Type))
		}
	}
	for _, text := range declared {
		if strings.Contains(text, subject) {
			return true
		}
	}
	names := false
	ast.Inspect(function.Body, func(node ast.Node) bool {
		identifier, isIdentifier := node.(*ast.Ident)
		if isIdentifier && slices.Contains(own, identifier.Name) {
			names = true
		}
		return true
	})
	return names
}

// The control: the mutation this gate exists for, landed as a file of its own.
//
// A byte identical second assembly. It writes the same fields in the same order as marshalCore,
// so every signature it produces verifies, every key package validates, every round trip round
// trips and the whole of mls and message stays green. There is no behaviour to observe; what
// there is, is a second declaration that emits.
//
// It is a SEPARATE FILE and that is the whole point of this control. The version of this gate it
// replaces read key_package.go by name, and a reviewer landed exactly this text in
// mls/key_package_tbs.go, pointed signedPreimage at it, and watched 6604 tests pass. The in-file
// form of the same mutation was caught; only the file boundary saved it. The control's file name
// is deliberately unrelated to key_package so that a scan narrowed by a name pattern rather than
// by a file list fails here too.
const keyPackageSecondAssemblyControl = `package control

func (self *KeyPackage) keyPackageTbs(w *syntax.Writer) error {
	w.WriteUint16(uint16(self.Version))
	w.WriteUint16(uint16(self.CipherSuite))
	w.WriteOpaque(self.InitKey)
	if err := self.LeafNode.MarshalMLS(w); err != nil {
		return err
	}
	return WriteExtensions(w, self.Extensions)
}
`

// namesOneMoreFileAdds reads one scan's answer as a DELTA against another's: the names the
// second answer holds that the first does not, and the names it lost, both as MULTISETS.
//
// Multisets and not sets, because the second assembly a control lands can carry the same
// SPELLING as one the package under test already holds -- which is one of the two arms the
// finding above was measured on -- and a set difference would answer "nothing added" for it
// and report a working scan as broken.
//
// dropped is returned rather than dropped on the floor because it is the one thing a delta
// cannot state on its own: a scan that answered a shorter list once another file was beside it
// would still add the control's name, and the added half alone would pass it.
func namesOneMoreFileAdds(without []string, with []string) (added []string, dropped []string) {
	remaining := slices.Clone(without)
	added = []string{}
	for _, name := range with {
		if at := slices.Index(remaining, name); at >= 0 {
			remaining = slices.Delete(remaining, at, at+1)
			continue
		}
		added = append(added, name)
	}
	return added, remaining
}

// TestTheKeyPackageSignaturePreimageIsAssembledExactlyOnce is the file header's first claim,
// as a gate.
//
// A second assembly of the signed prefix is a second OPINION about what a key package signs.
// Two implementations of one preimage disagree by bytes the day one of them changes, and a key
// package that verifies under one and not the other is a joiner nobody can add -- discovered
// at somebody else's Welcome, not here. While they agree, they agree PERFECTLY: identical
// bytes, identical signatures, identical refs, no round trip and no verification anywhere in
// this package or in message able to tell them apart. That is why this gate is over the source
// and why it is derived.
//
// The subject is a PACKAGE and not a file, which is the correction this version carries. What
// the name claims is that the preimage is assembled exactly once; what the previous version
// checked is that key_package.go assembles it exactly once, and package mls is one package, so
// any later task can reach signedPreimage's job from any file without knowing this gate exists.
// A gate that derives one axis and enumerates another is not a derived gate: this one derived
// its emitter class off *syntax.Writer's method set, with four anchors guarding the scan, and
// then handed it one file name.
//
// So the scope is every non test file of the package, and the narrowing that makes that a
// useful question is derived too -- keyPackageIsTheSubjectOf, off the type and off the field
// names only this structure declares. Three controls, because a scan and a filter can each fail
// silently: the mutation must be READ when it is landed one file over, the subject filter must
// keep less than the package emits, and the anchor on the own-field derivation must hold.
func TestTheKeyPackageSignaturePreimageIsAssembledExactlyOnce(t *testing.T) {
	emitters := mlsEncodingEmitters(t)
	sources := packageSources(t)
	own := keyPackageOwnFieldNames(t, sources)
	// the anchor on the second arm: this structure certainly declares an init key, and a
	// derivation that read something other than this package would answer without it
	if !slices.Contains(own, "InitKey") {
		t.Fatalf("the struct walk read %v as the field names the key package alone declares, and it certainly declares InitKey, so it read something other than this package",
			own)
	}
	scan := func(over []parsedSource) []string {
		found := []string{}
		for _, parsed := range over {
			found = append(found, declarationsEmittingWhere(parsed, emitters,
				func(in parsedSource, function *ast.FuncDecl) bool {
					return keyPackageIsTheSubjectOf(in, function, own)
				})...)
		}
		slices.Sort(found)
		return found
	}

	emitting := scan(sources)

	// the control, landed one file over rather than inside key_package.go, and read as what
	// that file ADDS rather than as the whole answer the scan gives with it in place.
	//
	// The whole-answer form is what this replaces, and it was measured rather than reasoned
	// about. It compared scan(sources + control) against the one list a clean package plus the
	// control produces, so ANY second assembly already in sources inflated that list and this
	// control's own Fatalf fired -- ahead of the assertion below, and while reporting that
	// whatever it read, it was "not reading a second assembly next door", which is exactly what
	// it had just read. Both arms of the mutation were measured on it: a second assembly named
	// keyPackageTbs answered [MarshalMLS keyPackageTbs keyPackageTbs marshalCore], and the same
	// assembly under the non colliding name assembleSignedPrefix answered
	// [MarshalMLS assembleSignedPrefix keyPackageTbs marshalCore]. Right verdict both times,
	// wrong diagnostic both times, and the sentence this gate exists to print unreachable in
	// both -- a control eating the assertion it was written to protect, and sending a reader to
	// debug the scan rather than to delete their duplicate.
	//
	// A DELTA separates the control from the subject and gives up nothing the control was for.
	// Whatever the package under test holds cancels out of both sides, so what is left states
	// only what the scan does with the control FILE: a scan that reads one file, or narrows by
	// a name pattern rather than by a file list, or stopped reading altogether, adds nothing
	// and fails here. And a package carrying a real second assembly leaves this control silent
	// and reaches the assertion below, which names it.
	control := scan(append(slices.Clone(sources),
		mustParseText(t, "second_assembly_control.go", keyPackageSecondAssemblyControl)))
	added, dropped := namesOneMoreFileAdds(emitting, control)
	if want := []string{"keyPackageTbs"}; !slices.Equal(added, want) {
		t.Fatalf("landing a second assembly one file over added %v to the scan's answer for this package (%v), want %v added; the scan is not reading a second assembly next door, whatever it reports about the real package",
			added, emitting, want)
	}
	// and the delta is a delta rather than a fresh reading: one more file may only ADD to the
	// answer, and a scan whose reading of a file depends on what sits beside it is telling us
	// nothing about either package it was handed
	if len(dropped) != 0 {
		t.Fatalf("landing one more file took %v out of the scan's answer for this package (%v); what this scan reads in a file depends on what is beside it, so neither of its two answers is about the package",
			dropped, emitting)
	}

	if want := []string{"MarshalMLS", "marshalCore"}; !slices.Equal(emitting, want) {
		t.Errorf("this package assembles the key package encoding in %v, want %v. marshalCore is the one assembly of the signed prefix and MarshalMLS reaches it; anything else is a second opinion about what a key package signs, and two implementations of one preimage disagree by bytes the day one of them changes",
			emitting, want)
	}

	// and the filter is separating something rather than passing the package through, which is
	// the control on the widening itself: a filter that keeps everything it is shown, or a scan
	// that read one file, both answer a short list and both look like this one passing
	everything := []string{}
	for _, parsed := range sources {
		everything = append(everything, declarationsEmittingIn(parsed, emitters)...)
	}
	if len(everything) <= len(emitting) {
		t.Fatalf("the package wide scan read %d emitting declarations in all and %d of them as the key package's; the filter is keeping everything it is shown, or the scan read one file's worth of the package",
			len(everything), len(emitting))
	}
	t.Logf("%d emitting declarations across the %d non test files of this package, %d of them the key package's: %v",
		len(everything), len(sources), len(emitting), emitting)
}

// ---------------------------------------------------------------------------
// what the package wide stub gate cannot hold NewKeyPackage to
// ---------------------------------------------------------------------------

// keyPackageWithoutItsClock is one key package encoded with its leaf's Lifetime replaced by a
// fixed window and both signatures dropped.
//
// The lifetime is the one part of this structure that is not a function of what NewKeyPackage
// was handed -- NewLeafNode stamps it off the wall clock -- so two calls a second apart differ
// in it, and because it sits inside the LeafNodeTBS and inside the KeyPackageTBS, in BOTH
// signatures too. Normalising it is what makes two calls comparable at all, and it is exactly
// why the package wide stub gate excuses this constructor from every comparison it makes across
// calls.
//
// The whole structure is encoded rather than a chosen field, so an argument that reached any
// part of it is observed. The two SIGNATURES are dropped with the lifetime because they cover it
// and would carry the clock straight back in; what holds the key package signature to depending
// on the fields it covers is TestKeyPackageRefIsStableAndBindsEveryField and the tampering rows
// of TestKeyPackageValidateRejects, over the same encoding.
func keyPackageWithoutItsClock(t *testing.T, kp *KeyPackage) string {
	t.Helper()
	normalised := *kp
	normalised.LeafNode = *kp.LeafNode.Clone()
	normalised.LeafNode.Lifetime = Lifetime{NotBefore: 1, NotAfter: 2}
	normalised.LeafNode.Signature = nil
	normalised.Signature = nil
	encoded, err := syntax.Marshal(&normalised)
	if err != nil {
		t.Fatalf("encode a key package with its clock normalised out: %v", err)
	}
	return hex.EncodeToString(encoded)
}

// keyPackageCall is NewKeyPackage's whole argument list, one EXPORTED field per declared
// parameter.
//
// Exported because the sweep reaches each field through reflect and reflect refuses to write an
// unexported one. A parameter resolves to the field of its own name with the first letter
// raised, so a parameter this list has no field for is fatal rather than swept by nothing --
// which is the failure mode a written out argument list has, and the one three gates in this
// package have already been caught in.
type keyPackageCall struct {
	Crypto CryptoProvider
	Suite  CipherSuite
	Cred   Credential
	Caps   Capabilities
	Exts   []Extension
}

func (self *keyPackageCall) fieldFor(t *testing.T, parameter string) reflect.Value {
	t.Helper()
	name := strings.ToUpper(parameter[:1]) + parameter[1:]
	field := reflect.ValueOf(self).Elem().FieldByName(name)
	if !field.IsValid() {
		t.Fatalf("NewKeyPackage declares a parameter %s and this argument list has no %s field for it, so nothing below moves it",
			parameter, name)
	}
	return field
}

// build is one call, answered as a string so that a refusal and a key package are comparable.
//
// The answer is the ENCODING and not the two private halves beside it, and that is a measurement
// rather than a preference. The first version of this read the structure and both private keys,
// on the reasoning that they are results too; dropping them from the answer altogether changed
// no verdict of this sweep, because every argument this constructor is handed reaches the
// encoding and the private halves move only when a public half already has. An assertion no
// mutation can kill is weight a reader has to reason about for nothing, so it is not here. What
// holds the two private halves to being two rather than one is
// TestNewKeyPackageDrawsTheInitAndEncryptionKeysFromSeparateEntropy, which opens a message under
// each public half with the private half it was handed -- a property this sweep does not state.
func (self keyPackageCall) build(t *testing.T) string {
	t.Helper()
	kp, _, _, err := NewKeyPackage(self.Crypto, self.Suite, self.Cred, self.Caps, self.Exts)
	if err != nil {
		return "refused: " + err.Error()
	}
	return keyPackageWithoutItsClock(t, kp)
}

// One well formed urmessage_leaf_keys entry, so that the extensions argument has an element to
// move as well as a length. A slice that can only grow states a weaker property than one that
// can also be shortened and reached into.
func keyPackageSweepExtension(t *testing.T) Extension {
	t.Helper()
	entry, err := (&LeafKeysExtension{
		AlgId:          AlgIdXwing,
		DeviceXwingPub: repeatByte(0x55, XwingPublicKeyLen),
	}).Encode()
	if err != nil {
		t.Fatalf("LeafKeysExtension.Encode: %v", err)
	}
	return entry
}

// keyPackageSweepArguments is one complete argument list, built fresh on every call.
//
// Fresh because the derived edits write IN PLACE and a shallow copy shares every slice: a row
// built off a value an earlier row had edited moves away from something the constructor was
// never called with.
//
// The provider is over a FIXED entropy script rather than the process source. NewKeyPackage
// draws three key pairs of its own, so two calls over crypto/rand answer different bytes
// whatever they were handed, and every row below would report an argument as observed on the
// strength of the randomness.
//
// The capabilities advertise every REGISTERED suite rather than the one this file builds at, so
// the suite move below stays inside a leaf whose capabilities cover it and a refusal there is
// the constructor's rule rather than this test's own arguments.
func keyPackageSweepArguments(t *testing.T) keyPackageCall {
	t.Helper()
	caps := testKeyPackageCapabilities()
	caps.CipherSuites = Suites()
	return keyPackageCall{
		Crypto: mustProviderOver(t, keyPackageTestSuite, providerStubStream(0x80)),
		Suite:  keyPackageTestSuite,
		Cred:   BasicCredential([]byte("alice")),
		Caps:   caps,
		Exts:   []Extension{keyPackageSweepExtension(t)},
	}
}

// keyPackageConstructorParameters is NewKeyPackage's parameter list, read off its own
// declaration.
//
// Derived rather than typed out, and this is the SCOPE half of the rule the enumeration failures
// of this project keep landing on: a sweep with five names written down goes on reporting a
// clean run the day a sixth argument lands. The file declaring it is found rather than named,
// for the same reason one level up.
func keyPackageConstructorParameters(t *testing.T) []string {
	t.Helper()
	parsed := sourceDeclaringPackageFunction(t, "NewKeyPackage")
	names := []string{}
	for _, parameter := range parametersOf(t, parsed, "NewKeyPackage",
		parsed.declarationOf(t, "", "NewKeyPackage").Type) {
		names = append(names, parameter.name)
	}
	if len(names) == 0 {
		t.Fatal("NewKeyPackage was read as taking no argument at all, so the sweep below runs over nothing")
	}
	return names
}

// keyPackageArgumentMove is one way of making one argument different, named for the failure
// message and keyed by the parameter it moves.
type keyPackageArgumentMove struct {
	parameter string
	name      string
	apply     func(t *testing.T, call *keyPackageCall)
}

// keyPackageArgumentMoves is every move this sweep makes, derived off the TYPE of each declared
// parameter rather than off its name or off a list.
//
// Three rules, and which one applies is decided by the parameter's type, so a parameter that is
// renamed keeps its rule and a parameter that is retyped loses it loudly.
//
//   - the provider: another provider at the same suite over ANOTHER entropy script. Every key in
//     the answer is drawn through it, so a constructor that built a provider of its own out of a
//     hardcoded suite -- which is a WORKING constructor, because both registered suites are
//     X25519 and Ed25519 and every corpus here is at one of them -- answers the same bytes over
//     both scripts and is caught here.
//   - the ciphersuite: every OTHER registered suite, WITH the provider that runs it. The two move
//     together because NewKeyPackage refuses a provider that does not run the suite it was named,
//     so a suite moved alone is not an accepted call at all -- and a refusal would let a body
//     that stored a hardcoded suite pass on the strength of the guard that read the argument,
//     which is the shape this sweep exists to catch. Moving the pair separates them: the two
//     registered suites share X25519, SHA-256 and Ed25519 and differ only in their AEAD, so every
//     key and both signatures come back IDENTICAL and the stored ciphersuite is the only thing
//     that can have moved.
//   - everything else: leafNodeEditsOf, the same derivation the leaf's codec sweeps run on, so an
//     argument that grows a field is swept on the commit that lands it rather than when somebody
//     remembers to extend a list.
//
// A refusal counts as an observation, for the stub gate's reason: an argument that moved a call
// from accepted to rejected has been read just as surely as one that moved the bytes.
func keyPackageArgumentMoves(t *testing.T, parameters []string) []keyPackageArgumentMove {
	t.Helper()
	base := keyPackageSweepArguments(t)
	providerType := reflect.TypeOf((*CryptoProvider)(nil)).Elem()
	suiteType := reflect.TypeOf(CipherSuite(0))
	moves := []keyPackageArgumentMove{}
	for _, parameter := range parameters {
		field := base.fieldFor(t, parameter)
		before := len(moves)
		switch field.Type() {
		case providerType:
			moves = append(moves, keyPackageArgumentMove{
				parameter: parameter,
				name:      "a provider at the same suite over another entropy script",
				apply: func(t *testing.T, call *keyPackageCall) {
					call.Crypto = mustProviderOver(t, call.Suite, providerStubStream(0x40))
				},
			})
		case suiteType:
			for _, suite := range Suites() {
				if suite == base.Suite {
					continue
				}
				moves = append(moves, keyPackageArgumentMove{
					parameter: parameter,
					name:      fmt.Sprintf("suite %#04x, over the provider that runs it", uint16(suite)),
					apply: func(t *testing.T, call *keyPackageCall) {
						call.Suite = suite
						call.Crypto = mustProviderOver(t, suite, providerStubStream(0x80))
					},
				})
			}
		default:
			for _, edit := range leafNodeEditsOf(parameter, field, leafNodeSources(t)) {
				moves = append(moves, keyPackageArgumentMove{
					parameter: parameter,
					name:      edit.name,
					apply: func(t *testing.T, call *keyPackageCall) {
						edit.apply(call.fieldFor(t, parameter))
					},
				})
			}
		}
		if len(moves) == before {
			t.Fatalf("NewKeyPackage declares %s, of type %s, and no move was derived for it, so this sweep would state nothing about that argument",
				parameter, field.Type())
		}
	}
	return moves
}

// TestNewKeyPackageReadsEveryArgumentItWasHanded is the half of the package wide stub gate that
// the wall clock exemption takes away, put back over the arguments this constructor has.
//
// The exemption is real -- two calls a second apart sign different key packages for a reason
// that is not the arguments -- and until this landed it was a HOLE. crypto_test.go's loop
// continues before the per argument perturbation for every name in
// providerConstructionsAnsweringOffTheWallClock, so unobserved was never populated for this
// constructor, and NewLeafNode's twin of this test had no equivalent here. Measured on the
// committed tree, twice by a reviewer and once by the owner: a body that replaced the credential
// it was handed with BasicCredential("mallory"), and a body that read the suite in a guard and
// then stored a hardcoded one, each passed 6604 tests with zero failures. Every key package in
// the system would have been minted under an identity and a suite the caller never named.
//
// The property is the gate's: an argument that changes must change the answer, or the
// constructor is a function of fewer things than its signature says. Three differences from the
// leaf's twin. The answer is read with the lifetime normalised out, which is what makes the
// comparison stable across a second boundary. The parameter list is DERIVED off the declaration,
// so a sixth argument is swept on the commit that lands it or fails here. And the answer carries
// the two private halves as well as the structure, because they are results too.
//
// It is not a source shape check and it must not become one. TestNoStubShapesRemainInSource
// catches the crude form -- an argument never mentioned at all -- and records its own limit
// where it is declared: a body which reads a parameter and then ignores the value still passes
// it. That is exactly what both measured substitutions do, so this one drives the constructor
// and reads the argument out of the OUTPUT.
func TestNewKeyPackageReadsEveryArgumentItWasHanded(t *testing.T) {
	parameters := keyPackageConstructorParameters(t)
	answer := keyPackageSweepArguments(t).build(t)
	if strings.HasPrefix(answer, "refused") {
		t.Fatalf("NewKeyPackage refused this test's own arguments (%s), so every row below compares two refusals", answer)
	}
	// the control on the normalisation and on the fixed script: two calls with one argument
	// list answer the same thing, or every "it moved" below is the clock or the entropy rather
	// than the argument
	if repeated := keyPackageSweepArguments(t).build(t); repeated != answer {
		t.Fatalf("NewKeyPackage answered\n %s\nand then\n %s\nfor one argument list with the clock normalised out",
			answer, repeated)
	}
	moves := keyPackageArgumentMoves(t, parameters)
	moved := map[string]int{}
	for _, move := range moves {
		with := keyPackageSweepArguments(t)
		move.apply(t, &with)
		if with.build(t) == answer {
			t.Errorf("NewKeyPackage answered the same key package with %s, so it does not read the %s it was handed",
				move.name, move.parameter)
			continue
		}
		moved[move.parameter] += 1
	}
	for _, parameter := range parameters {
		if moved[parameter] == 0 {
			t.Errorf("no derived move to %s changed what NewKeyPackage answered, so this sweep observed nothing about that argument",
				parameter)
		}
	}
	t.Logf("%d derived moves across the %d arguments NewKeyPackage declares (%v)",
		len(moves), len(parameters), parameters)
}

// TestNewKeyPackageRefusesAProviderThatDoesNotRunTheSuiteItWasNamed pins the decision
// errKeyPackageProviderSuite records.
//
// On the committed tree NewKeyPackage(NewCryptoProvider(0x0001), 0x0003, ...) answered no error,
// produced a key package advertising 0x0003, and Validate(crypto, 0x0003, now) ACCEPTED it --
// because Validate compares the structure's suite against its argument and never against the
// provider. Harmless only while the two registered suites share X25519, SHA-256 and Ed25519 and
// differ solely in their AEAD; a third suite that moves any of those makes it a key package
// whose signature no peer can check, published before anybody finds out.
//
// The sweep is over every ORDERED PAIR of registered suites rather than over the one pair that
// exists today, so the suite p8 registers is covered by the commit that registers it. The
// matched pairs are in the sweep as well as the mismatched ones: a guard that refused
// everything would satisfy a test that only drove mismatches, and it would be a guard nothing
// could mint a key package through.
func TestNewKeyPackageRefusesAProviderThatDoesNotRunTheSuiteItWasNamed(t *testing.T) {
	suites := Suites()
	if len(suites) < 2 {
		t.Fatalf("this package registers %v, and a mismatch has to be made of two suites", suites)
	}
	caps := testKeyPackageCapabilities()
	caps.CipherSuites = suites
	for _, running := range suites {
		for _, named := range suites {
			crypto := mustProviderOver(t, running, providerStubStream(0x80))
			kp, initPriv, encPriv, err := NewKeyPackage(crypto, named,
				BasicCredential([]byte("alice")), caps, nil)
			if running == named {
				if err != nil {
					t.Errorf("NewKeyPackage over a provider running %#04x and naming %#04x: %v",
						uint16(running), uint16(named), err)
				}
				continue
			}
			if !errors.Is(err, errKeyPackageProviderSuite) {
				t.Errorf("NewKeyPackage over a provider running %#04x and naming %#04x answered %v, want errKeyPackageProviderSuite",
					uint16(running), uint16(named), err)
			}
			if kp != nil || initPriv != nil || encPriv != nil {
				t.Errorf("NewKeyPackage answered a key package and %d and %d private octets alongside the refusal of a provider running %#04x named %#04x",
					len(initPriv), len(encPriv), uint16(running), uint16(named))
			}
		}
	}
	// and the refusal comes before anything is drawn, which is what keeps a caller's mistake
	// from costing three key pairs of entropy and what makes it reproducible
	counting := &countingReader{inner: providerStubStream(0x80)}
	crypto := mustProviderOver(t, suites[0], counting)
	if _, _, _, err := NewKeyPackage(crypto, suites[1], BasicCredential([]byte("alice")), caps, nil); err == nil {
		t.Fatalf("a provider running %#04x minted a key package naming %#04x", uint16(suites[0]), uint16(suites[1]))
	}
	if counting.drawn != 0 {
		t.Errorf("NewKeyPackage drew %d octets before refusing a provider that does not run the suite it was named",
			counting.drawn)
	}
}

// TestTheZeroKeyPackageIsRefusedOnItsCredentialAndNotItsLeafSource is the claim
// provider_nil_test.go's two key package rows rest on, as an assertion rather than as prose.
//
// Those rows drive a ZERO valued KeyPackage at a nil provider to say the provider is judged
// before the receiver, and the comment above them has to name the refusal the receiver WOULD
// have produced -- otherwise they state that one of two orders was taken without saying what the
// other one answers. That comment said ErrTreeMalformed, on the leaf's source. It is
// errProfileCredentialType, on the leaf's CREDENTIAL: Credential.MarshalMLS refuses a type that
// is not basic before it writes an octet, and the credential is the third field of the leaf the
// structure Ref marshals carries, so the encoder never reaches the source at all.
//
// This project reads a justification comment as a claim, and a claim nothing checks is exactly
// how a wrong one survives review. This is the check.
func TestTheZeroKeyPackageIsRefusedOnItsCredentialAndNotItsLeafSource(t *testing.T) {
	crypto, err := NewCryptoProvider(keyPackageTestSuite)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	ref, refErr := (&KeyPackage{}).Ref(crypto)
	if !errors.Is(refErr, errProfileCredentialType) {
		t.Errorf("(&KeyPackage{}).Ref answered %v, want errProfileCredentialType: the zero leaf's credential type is 0 and the encoder refuses it before it reaches the source",
			refErr)
	}
	if errors.Is(refErr, ErrTreeMalformed) {
		t.Errorf("(&KeyPackage{}).Ref answered %v, which answers ErrTreeMalformed; the reason written above provider_nil_test.go's key package rows named that refusal and it does not happen",
			refErr)
	}
	if ref != nil {
		t.Errorf("(&KeyPackage{}).Ref answered the reference %x alongside its refusal", ref)
	}
	// and the neighbouring row's reason, which WAS right: a zero key package names version 0,
	// so a Validate that judged its receiver first would answer for a version nobody chose
	if err := (&KeyPackage{}).Validate(crypto, keyPackageTestSuite, time.Now()); !errors.Is(err, ErrUnsupportedVersion) {
		t.Errorf("(&KeyPackage{}).Validate answered %v, want ErrUnsupportedVersion", err)
	}
}

// ---------------------------------------------------------------------------
// j1 task 1: the key package minted against a caller's signing key
// ---------------------------------------------------------------------------

// The signer every NewKeyPackageWithSigner test below binds to, written once so a test that
// means to vary it is visibly doing so. A caller's key here is the device's long term MLS leaf
// signature key -- MASTER section 5.2's device_sig -- and the whole of this constructor is that
// the four places a key package names a signing key name THAT one.
func testKeyPackageSigner(fill byte) SignaturePrivateKey {
	return SignaturePrivateKey(bytes.Repeat([]byte{fill}, 32))
}

// TestNewKeyPackageWithSignerBindsAllFourToTheCallersSigner is the whole of what this
// constructor is for, and it is FOUR clauses rather than a call to Validate.
//
// lifecycle_fixtures_test.go's own header is the argument, in this package's words: rebinding
// the leaf alone "leaves kp.Signature over the old leaf, which KeyPackage.Validate refuses with
// errKeyPackageBadSignature, and leaves kp.signPriv holding a private key whose public half the
// leaf no longer names, which nothing refuses at all -- it is read, used to sign a joiner's
// first message, and rejected by every peer."
//
// One of the four failures is LOUD and one is SILENT. Validate sees the leaf's signature_key,
// the leaf's own signature and the key package signature; it cannot see the fourth, because the
// seed is not a field of the encoding and no verifier reads it. So a gate that called Validate
// and stopped would pass the one mutant that costs a joiner its first message.
func TestNewKeyPackageWithSignerBindsAllFourToTheCallersSigner(t *testing.T) {
	crypto, err := NewCryptoProvider(keyPackageTestSuite)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	signer := testKeyPackageSigner(0x41)
	want, err := signaturePublicKeyOf(signer)
	if err != nil {
		t.Fatalf("signaturePublicKeyOf over the signer this test injects: %v", err)
	}
	kp, initPriv, encPriv, err := NewKeyPackageWithSigner(crypto, keyPackageTestSuite, signer,
		BasicCredential([]byte("the device this key package names")), testKeyPackageCapabilities(), nil)
	if err != nil {
		t.Fatalf("NewKeyPackageWithSigner: %v", err)
	}
	if len(initPriv) == 0 || len(encPriv) == 0 {
		t.Fatalf("the constructor answered %d init octets and %d encryption octets", len(initPriv), len(encPriv))
	}

	// (1) the leaf NAMES the caller's key
	if !bytes.Equal(kp.LeafNode.SignatureKey, want) {
		t.Errorf("the leaf names %x as its signature_key and the caller's signer derives %x",
			kp.LeafNode.SignatureKey, want)
	}
	// (2) the leaf's own signature verifies under it. This is the clause mutation 1 attacks: a
	// leaf whose signature_key was set from the signer and whose signature was made with some
	// other key is a leaf every peer refuses and nothing here would otherwise read.
	if err := kp.LeafNode.VerifySignature(crypto, nil, 0); err != nil {
		t.Errorf("the leaf this constructor built does not verify under the key it names: %v", err)
	}
	// (3) the KeyPackageTBS signature verifies, which is what Validate reads. Mutation 2 leaves
	// this one over a preimage taken before the leaf was rebound.
	if err := kp.Validate(crypto, keyPackageTestSuite, time.Now()); err != nil {
		t.Errorf("Validate refused a key package this constructor minted: %v", err)
	}
	// (4) the SILENT one. Nothing in this package or in any peer reads the retained seed, so a
	// constructor that kept a different key answers a key package that verifies, validates,
	// refs and round trips, and the member it describes is rejected at its first update.
	if len(kp.signPriv) == 0 {
		t.Fatalf("the constructor kept no signature seed, so nothing can sign this member's later updates")
	}
	kept, err := signaturePublicKeyOf(kp.signPriv)
	if err != nil {
		t.Fatalf("signaturePublicKeyOf over the seed the constructor kept: %v", err)
	}
	if !bytes.Equal(kept, want) {
		t.Errorf("the retained seed derives %x, the caller's signer derives %x, and the leaf names %x",
			kept, want, kp.LeafNode.SignatureKey)
	}
}

// TestNewKeyPackageWithSignerClonesTheCallersSigner is the clause that makes this design safe,
// and it does not exist for NewKeyPackage because there the seed has no other owner.
//
// Here the seed IS device_sig. (*KeyPackage).Zeroize erases whatever signPriv points at, and
// (*JoinKeyMaterial).Zeroize calls it -- so a constructor that stored the caller's slice hands
// every holder of the key package a licence to erase the device's long term signing key out from
// under the engine that owns it. connect/messagegroup's engine is required to erase the value
// this constructor answers, so that caller is not hypothetical.
//
// THE OBSERVATION IS ON THE CALLER'S ARRAY AFTER Zeroize, not on the key package. A gate that
// read only the key package is green over a body that assigned the caller's slice in: the field
// holds the right octets either way, and what differs is whose array it is.
func TestNewKeyPackageWithSignerClonesTheCallersSigner(t *testing.T) {
	crypto, err := NewCryptoProvider(keyPackageTestSuite)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	signer := testKeyPackageSigner(0x42)
	asHandedIn := bytes.Clone(signer)
	kp, _, _, err := NewKeyPackageWithSigner(crypto, keyPackageTestSuite, signer,
		BasicCredential([]byte("the device whose identity this erase must not reach")),
		testKeyPackageCapabilities(), nil)
	if err != nil {
		t.Fatalf("NewKeyPackageWithSigner: %v", err)
	}
	if len(kp.signPriv) != len(signer) {
		t.Fatalf("the constructor kept %d octets of a %d octet signer", len(kp.signPriv), len(signer))
	}
	if &kp.signPriv[0] == &signer[0] {
		t.Errorf("the key package's seed IS the caller's array, so every Zeroize on it reaches the device's identity")
	}

	kp.Zeroize()

	if !bytes.Equal(signer, asHandedIn) {
		t.Errorf("the caller's signer was %x before the erase and is %x after it; the device's long term signing key was destroyed by a key package it minted",
			asHandedIn, signer)
	}
	for _, octet := range kp.signPriv {
		if octet != 0 {
			t.Errorf("the key package's own copy of the seed survives its Zeroize: %x", kp.signPriv)
			break
		}
	}
}

// TestNewKeyPackageWithSignerDrawsTheInitAndEncryptionKeysFromSeparateEntropy is the half of
// this constructor's entropy claim that a COUNT cannot see.
//
// providerStreamDraws holds the count -- 2*KDF.Nh and no NsigPriv -- and that gate catches a
// body which derives both pairs from one draw, because it draws KDF.Nh fewer octets. It does
// NOT catch a body that draws twice and derives both pairs from the FIRST draw: that one draws
// exactly 2*KDF.Nh and passes the count. Nor does it catch a body that derives the init pair
// from the signer, which is precisely the shape a constructor that "already has a key" invites.
// So this gate is written over the ANSWERS.
//
// The messages are opened rather than the byte strings compared, for the reason NewKeyPackage's
// own entropy test gives: a comparison catches the crude form and catches neither a constructor
// that answers the encryption private key in the init position nor one that publishes the
// encryption public key as the init_key.
func TestNewKeyPackageWithSignerDrawsTheInitAndEncryptionKeysFromSeparateEntropy(t *testing.T) {
	crypto, err := NewCryptoProvider(keyPackageTestSuite)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	signer := testKeyPackageSigner(0x43)
	kp, initPriv, encPriv, err := NewKeyPackageWithSigner(crypto, keyPackageTestSuite, signer,
		BasicCredential([]byte("alice")), testKeyPackageCapabilities(), nil)
	if err != nil {
		t.Fatalf("NewKeyPackageWithSigner: %v", err)
	}
	if bytes.Equal(initPriv, encPriv) {
		t.Fatalf("the two private halves are one key")
	}
	if bytes.Equal(kp.InitKey, kp.LeafNode.EncryptionKey) {
		t.Fatalf("init_key and the leaf's encryption_key are one key")
	}

	probe := []byte("the message a joiner has to be able to open")
	info := []byte("key package with signer entropy probe")
	sealTo := func(what string, pub HpkePublicKey) ([]byte, []byte) {
		t.Helper()
		kemOutput, ciphertext, sealErr := crypto.HpkeSeal(pub, info, nil, probe)
		if sealErr != nil {
			t.Fatalf("seal to the %s: %v", what, sealErr)
		}
		return kemOutput, ciphertext
	}
	opens := func(priv HpkePrivateKey, kemOutput []byte, ciphertext []byte) bool {
		opened, openErr := crypto.HpkeOpen(priv, kemOutput, info, nil, ciphertext)
		return openErr == nil && bytes.Equal(opened, probe)
	}
	initKem, initCiphertext := sealTo("published init_key", kp.InitKey)
	if !opens(initPriv, initKem, initCiphertext) {
		t.Errorf("the init private key this constructor returned does not open a message sealed to the init_key it published")
	}
	if opens(encPriv, initKem, initCiphertext) {
		t.Errorf("the encryption private key opens a message sealed to the init_key, so the two key pairs are one")
	}
	encKem, encCiphertext := sealTo("leaf encryption_key", kp.LeafNode.EncryptionKey)
	if !opens(encPriv, encKem, encCiphertext) {
		t.Errorf("the encryption private key this constructor returned does not open a message sealed to the leaf's encryption_key")
	}
	if opens(initPriv, encKem, encCiphertext) {
		t.Errorf("the init private key opens a message sealed to the leaf's encryption_key, so the two key pairs are one")
	}

	// and NEITHER pair is a function of the signer. One signer, two different entropy streams:
	// a body that derived either pair from the caller's key answers the same public half twice,
	// while drawing exactly the octets providerStreamDraws expects.
	overOneStream := func(fill byte) *KeyPackage {
		t.Helper()
		fixed := mustProviderOver(t, keyPackageTestSuite, constantReader{value: fill})
		minted, _, _, streamErr := NewKeyPackageWithSigner(fixed, keyPackageTestSuite, signer,
			BasicCredential([]byte("alice")), testKeyPackageCapabilities(), nil)
		if streamErr != nil {
			t.Fatalf("NewKeyPackageWithSigner over a fixed stream: %v", streamErr)
		}
		return minted
	}
	over11, over22 := overOneStream(0x11), overOneStream(0x22)
	if bytes.Equal(over11.InitKey, over22.InitKey) {
		t.Errorf("one signer over two entropy streams answered the init_key %x both times, so the init key pair is derived from the signer",
			over11.InitKey)
	}
	if bytes.Equal(over11.LeafNode.EncryptionKey, over22.LeafNode.EncryptionKey) {
		t.Errorf("one signer over two entropy streams answered the leaf encryption_key %x both times, so the encryption key pair is derived from the signer",
			over11.LeafNode.EncryptionKey)
	}
}

// keyPackageAnsweringConstructions is the class the seed-and-leaf gate below is stated over, and
// it is DERIVED rather than named: every function declaration of this package's non test source
// whose signature mentions a *KeyPackage in any position is the universe, and the ones that
// ANSWER one are the class.
//
// It is an AST question and not a grep question, and that is the whole finding this helper
// carries. The line command this class used to be published beside --
// "^func .*\*KeyPackage" over non test mls -- returns SEVEN lines and NONE of them names
// NewKeyPackage: that declaration WRAPS, and *KeyPackage sits on a continuation line which does
// not begin with func. The seven lines it does return are exactly this class's COMPLEMENT. So
// the class the narrowing existed to derive was entirely outside the command printed beside it,
// at every commit, and a third constructor spelled in this package's own multi line style would
// leave that answer at seven.
//
// Both sets are printed on every run, so a constructor added next month is a number that changed
// rather than a door nobody drove.
func keyPackageAnsweringConstructions(t *testing.T) (universe []string, class []string, complement []string) {
	t.Helper()
	checked := typeCheckedRoot(t, cryptoOwnRoot)
	declared := checked.pkg.Scope().Lookup("KeyPackage")
	if declared == nil {
		t.Fatalf("this package declares no KeyPackage, so the class below is read off nothing")
	}
	is := sameTypeAs(types.NewPointer(declared.Type()))
	mentions := func(signature *types.Signature) bool {
		if receiver := signature.Recv(); receiver != nil && is(receiver.Type()) {
			return true
		}
		for _, tuple := range []*types.Tuple{signature.Params(), signature.Results()} {
			for at := 0; at < tuple.Len(); at++ {
				if is(tuple.At(at).Type()) {
					return true
				}
			}
		}
		return false
	}
	answers := func(signature *types.Signature) bool {
		results := signature.Results()
		for at := 0; at < results.Len(); at++ {
			if is(results.At(at).Type()) {
				return true
			}
		}
		return false
	}
	for _, function := range declaredFunctionsOf(t, cryptoOwnRoot) {
		if !mentions(function.signature) {
			continue
		}
		universe = append(universe, function.name)
		if answers(function.signature) {
			class = append(class, function.name)
		} else {
			complement = append(complement, function.name)
		}
	}
	slices.Sort(universe)
	slices.Sort(class)
	slices.Sort(complement)
	if len(universe) != len(class)+len(complement) {
		t.Fatalf("the universe reads %v, the class %v and the complement %v, and the two do not sum to the whole",
			universe, class, complement)
	}
	return universe, class, complement
}

// TestEveryConstructionAnsweringAKeyPackageBindsItsSeedToItsOwnLeaf drives the class rather than
// a name, which is the difference between this gate and
// TestNewKeyPackageKeepsTheSigningSeedOffTheWireAndBesideItsOwnLeaf above.
//
// That one holds the same sentence over ONE HAND NAMED constructor, and it PASSES with a second
// minting door present and undriven -- measured, at the commit that added
// NewKeyPackageWithSigner and before this gate existed. A sibling added beside a hand named gate
// is a second door outside it, which is this project's most expensive shape, so the class here
// is read off the package's own declarations and every member of it is required to have a
// driver.
//
// The complement is printed with the class, and it is seven methods rather than an empty set: a
// narrowing to "functions whose RESULT is a *KeyPackage" removes nothing today and starts
// removing real members the day one appears, which GATES.md's table calls the dangerous reading.
// Widening the universe to the whole *KeyPackage signature and splitting it on "answers one"
// gives the same class, a complement of seven, and a predicate one run of this gate prints.
func TestEveryConstructionAnsweringAKeyPackageBindsItsSeedToItsOwnLeaf(t *testing.T) {
	crypto, err := NewCryptoProvider(keyPackageTestSuite)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	cred := BasicCredential([]byte("the device every driver below mints for"))
	// one driver per member of the class. A member with no driver is a t.Errorf naming it and
	// never a continue: an underived member is exactly the door this gate exists to find.
	drivers := map[string]func() (*KeyPackage, error){
		"NewKeyPackage": func() (*KeyPackage, error) {
			kp, _, _, mintErr := NewKeyPackage(crypto, keyPackageTestSuite, cred,
				testKeyPackageCapabilities(), nil)
			return kp, mintErr
		},
		"NewKeyPackageWithSigner": func() (*KeyPackage, error) {
			kp, _, _, mintErr := NewKeyPackageWithSigner(crypto, keyPackageTestSuite,
				testKeyPackageSigner(0x44), cred, testKeyPackageCapabilities(), nil)
			return kp, mintErr
		},
	}

	universe, class, complement := keyPackageAnsweringConstructions(t)
	t.Logf("%d declarations of this package mention a *KeyPackage: %d answer one (%v) and %d do not (%v)",
		len(universe), len(class), class, len(complement), complement)
	if len(complement) == 0 {
		t.Errorf("the complement of this narrowing is empty, so it removes nothing today and starts removing real members the day one appears")
	}

	driven := []string{}
	for name := range drivers {
		driven = append(driven, name)
	}
	slices.Sort(driven)
	for _, name := range class {
		driver, held := drivers[name]
		if !held {
			t.Errorf("%s answers a *KeyPackage and this gate drives it with nothing; the class this package declares is %v and the drivers are %v -- a minting door outside the gate is what this gate exists to find",
				name, class, driven)
			continue
		}
		kp, mintErr := driver()
		if mintErr != nil {
			t.Errorf("%s refused this gate's arguments: %v", name, mintErr)
			continue
		}
		if len(kp.signPriv) == 0 {
			t.Errorf("%s kept no signature seed, so nothing can sign this member's later updates", name)
			continue
		}
		pub, keyErr := signaturePublicKeyOf(kp.signPriv)
		if keyErr != nil {
			t.Errorf("%s kept a seed signaturePublicKeyOf refuses: %v", name, keyErr)
			continue
		}
		if !bytes.Equal(pub, kp.LeafNode.SignatureKey) {
			t.Errorf("%s kept a seed deriving %x and answered a leaf naming %x as its signature_key",
				name, pub, kp.LeafNode.SignatureKey)
		}
	}
	for _, name := range driven {
		if !slices.Contains(class, name) {
			t.Errorf("this gate drives %s, and no declaration of this package answering a *KeyPackage is spelled that way",
				name)
		}
	}
}

// TestNewKeyPackageWithSignerRefusesExactlyWhatItMust is NewKeyPackage's refusal set, minus the
// one this constructor cannot have and plus the one it must.
//
// MINUS: NewKeyPackage can fail inside crypto.SignatureKeyPair(). This one never calls it, and
// the gate says so rather than leaving a reader to wonder -- over a source holding exactly
// 2*KDF.Nh octets this constructor completes, where a body that drew a signature key pair first
// would leave the two derivations short and take the process down inside Random.
//
// PLUS: a signer that is not a valid signature private key. The length is NOT open coded here:
// signaturePublicKeyOf checks against ed25519.SeedSize and answers ErrBadSignatureKey, and a
// second length literal in key_package.go is a second place the constant can be wrong. So the
// assertion is on the SENTINEL a caller matches with errors.Is and not on "an error came back".
//
// KEPT, in NewKeyPackage's order and for its reasons: the nil provider refused before any
// argument is judged, and the suite refused before anything is drawn. The signer is judged
// between them -- after the suite and before the two draws -- so a caller's mistake costs no
// entropy either, and the gate asserts the DRAW COUNT on each refusal rather than only the
// error.
func TestNewKeyPackageWithSignerRefusesExactlyWhatItMust(t *testing.T) {
	params, err := LookupSuite(keyPackageTestSuite)
	if err != nil {
		t.Fatalf("look up the suite this gate is built over: %v", err)
	}
	cred := BasicCredential([]byte("alice"))

	// the nil provider, refused rather than dereferenced. A body that reached crypto.Suite()
	// first takes the caller's process instead of its call, so this row is written as a panic
	// catch rather than as a bare call.
	var refusal error
	if raised := recoveredPanic(func() {
		_, _, _, refusal = NewKeyPackageWithSigner(nil, keyPackageTestSuite,
			testKeyPackageSigner(0x45), cred, testKeyPackageCapabilities(), nil)
	}); raised != nil {
		t.Errorf("a nil provider took the process down with %v rather than being refused", raised)
	} else if !errors.Is(refusal, ErrNilCryptoProvider) {
		t.Errorf("a nil provider answered %v, want ErrNilCryptoProvider", refusal)
	}

	// and the two refusals that must cost no entropy, each read off a counting source
	for _, testCase := range []struct {
		what   string
		suite  CipherSuite
		signer SignaturePrivateKey
		want   error
	}{
		{what: "a provider running another suite", suite: CipherSuiteX25519AesGcm128Sha256Ed25519,
			signer: testKeyPackageSigner(0x46), want: errKeyPackageProviderSuite},
		{what: "a nil signer", suite: keyPackageTestSuite, signer: nil, want: ErrBadSignatureKey},
		{what: "a signer one octet short", suite: keyPackageTestSuite,
			signer: SignaturePrivateKey(bytes.Repeat([]byte{0x47}, 31)), want: ErrBadSignatureKey},
		{what: "a signer one octet long", suite: keyPackageTestSuite,
			signer: SignaturePrivateKey(bytes.Repeat([]byte{0x48}, 33)), want: ErrBadSignatureKey},
	} {
		counting := &countingReader{inner: providerStubStream(0x80)}
		crypto := mustProviderOver(t, keyPackageTestSuite, counting)
		_, _, _, refused := NewKeyPackageWithSigner(crypto, testCase.suite, testCase.signer, cred,
			testKeyPackageCapabilities(), nil)
		if !errors.Is(refused, testCase.want) {
			t.Errorf("%s answered %v, want %v", testCase.what, refused, testCase.want)
		}
		if counting.drawn != 0 {
			t.Errorf("%s was refused after %d octets had been drawn; a caller's mistake must cost no entropy",
				testCase.what, counting.drawn)
		}
	}

	// the MINUS clause, as a source that is exactly the two draws long. A body that called
	// SignatureKeyPair first consumes NsigPriv of these before the derivations start.
	exact := &countingReader{inner: bytes.NewReader(ascendingBytes(0x90, 2*params.Nh))}
	crypto := mustProviderOver(t, keyPackageTestSuite, exact)
	kp, _, _, err := NewKeyPackageWithSigner(crypto, keyPackageTestSuite, testKeyPackageSigner(0x49),
		cred, testKeyPackageCapabilities(), nil)
	if err != nil {
		t.Errorf("a source holding exactly the two KDF.Nh draws was not enough for this constructor: %v; it drew %d of %d",
			err, exact.drawn, 2*params.Nh)
	}
	if kp != nil && exact.drawn != 2*params.Nh {
		t.Errorf("the constructor drew %d octets where the two key pair derivations are %d; the difference is a draw this gate cannot account for",
			exact.drawn, 2*params.Nh)
	}
}

// ---------------------------------------------------------------------------
// j1 task 2: one minting body, and the seed the wrapper must not leave behind
// ---------------------------------------------------------------------------

// seedRetainingProvider hands back what the real provider hands back and RETAINS the signature
// seed's own array rather than a copy of it.
//
// It is the ALIAS the erase property below is observed through, and an alias is the only thing
// that can observe it. NewKeyPackage's drawn seed is a LOCAL of its body: no test can read a
// returned function's local, so "the wrapper's array is zero after the call returns" names a
// value and not a route. The route exists because (*suiteCryptoProvider).SignatureKeyPair hands
// back the seed BUFFER itself rather than a copy -- so a wrapper that keeps the slice header it
// was answered holds the same array the body holds, and reading it afterwards IS reading the
// body's local.
//
// A provider that copied on the way out would make this property green under the mutant it
// exists for. That is the general shape and it is written here because this package meets it
// twice: an erase is observable only through an alias of the array erased, and wherever the far
// side copies, the property must build the alias or it is measuring a photograph.
//
// It EMBEDS rather than writing the interface out, which is the opposite of
// taggingCryptoProvider's choice and is deliberate for a different reason: what this instrument
// overrides is one method, and a method added to CryptoProvider tomorrow must keep working here
// untouched, exactly as it does for the embedding doubles in group_test.go and
// framing_protect_test.go. Nothing here tags an answer, so a promoted method narrows nothing.
type seedRetainingProvider struct {
	CryptoProvider
	retained []SignaturePrivateKey
	// and a COPY of the same seed, which is a different instrument answering a different
	// question. retained is the ALIAS the erase is read through and is zero afterwards by
	// construction; copies is what the provider actually ANSWERED, and it is the only way to
	// ask whether the seed the key package kept is the seed the provider drew. A gate that
	// recovered "the key it drew" off kp.signPriv is green over a wrapper that handed its
	// delegate the PUBLIC half: that answer is self consistent at every clause.
	copies [][]byte
	// when set, DeriveKeyPair refuses. It is how this gate reaches an exit the DELEGATE takes
	// AFTER the wrapper has already drawn: NewLeafNode refuses only a nil provider and a bad
	// signer, and the wrapper hands it neither, so the only refusal downstream of the draw is
	// one the provider raises.
	refuseDerive bool
}

var errSeedRetainingProviderRefuses = errors.New("mls: the retaining provider refuses to derive")

func (self *seedRetainingProvider) DeriveKeyPair(ikm []byte) (HpkePrivateKey, HpkePublicKey, error) {
	if self.refuseDerive {
		return nil, nil, errSeedRetainingProviderRefuses
	}
	return self.CryptoProvider.DeriveKeyPair(ikm)
}

func (self *seedRetainingProvider) SignatureKeyPair() (SignaturePrivateKey, SignaturePublicKey, error) {
	priv, pub, err := self.CryptoProvider.SignatureKeyPair()
	if err == nil {
		// the slice HEADER, not a clone. The array behind it is the one NewKeyPackage's body
		// is about to hold, and a clone here would answer this property from a photograph.
		self.retained = append(self.retained, priv)
		self.copies = append(self.copies, bytes.Clone(priv))
	}
	return priv, pub, err
}

// keyPackageDelegationStream is the entropy both halves of the equality below are run over.
//
// It is long enough for a signature seed and two KDF.Nh derivations at either registered suite,
// and it ascends rather than repeating so that a body reading the wrong window answers something
// visibly different rather than something accidentally equal.
func keyPackageDelegationStream(t *testing.T) []byte {
	t.Helper()
	return ascendingBytes(0x10, 4096)
}

// TestNewKeyPackageAndTheSignerTakingConstructorAnswerOneKeyPackage is the equality that says
// the two bodies are one body.
//
// NewCryptoProviderWithRandom is what makes it observable: run NewKeyPackage over a recorded
// stream, recover the signature key it drew, then run NewKeyPackageWithSigner with that key over
// the SAME stream ADVANCED PAST the signature draw, and compare the two encodings octet for
// octet along with both private halves.
//
// The comparison reports the first differing offset rather than a boolean, because a difference
// in the leaf and a difference in the signature are two different defects and a caller reading
// "not equal" cannot tell them apart.
//
// The leaf carries a wall clock Lifetime and the signature covers it, so two mints a second
// apart differ for a reason that is not the bodies. The loop retries rather than normalising:
// normalising the lifetime out would also have to re-sign, which would compare two preimages
// this test built instead of the two the constructors did.
func TestNewKeyPackageAndTheSignerTakingConstructorAnswerOneKeyPackage(t *testing.T) {
	params, err := LookupSuite(keyPackageTestSuite)
	if err != nil {
		t.Fatalf("look up the suite this equality is taken at: %v", err)
	}
	stream := keyPackageDelegationStream(t)
	cred := BasicCredential([]byte("the device both constructors mint for"))

	for attempt := 0; ; attempt++ {
		if attempt == 8 {
			t.Fatalf("eight attempts and the two mints never landed inside one wall clock second")
		}
		drawn := &seedRetainingProvider{
			CryptoProvider: mustProviderOver(t, keyPackageTestSuite, bytes.NewReader(stream)),
		}
		drew, drewInit, drewEnc, drewErr := NewKeyPackage(drawn, keyPackageTestSuite, cred,
			testKeyPackageCapabilities(), nil)
		if drewErr != nil {
			t.Fatalf("NewKeyPackage over the recorded stream: %v", drewErr)
		}
		// THE SEED THE PROVIDER ANSWERED, and not the one the key package kept. The two are the
		// same value under a correct wrapper and that is the point: a wrapper that handed its
		// delegate the drawn PUBLIC half, or one that erased the seed before delegating, answers
		// a key package whose every field agrees with itself, and recovering "the key it drew"
		// off kp.signPriv would compare that key package against itself.
		if len(drawn.copies) != 1 {
			t.Fatalf("the wrapper drew %d signature key pairs, want exactly one", len(drawn.copies))
		}
		signer := drawn.copies[0]
		if len(signer) != params.NsigPriv {
			t.Fatalf("the drawn seed is %d octets and the suite's NsigPriv is %d", len(signer), params.NsigPriv)
		}
		if !bytes.Equal(drew.signPriv, signer) {
			t.Fatalf("the provider answered the seed %x and the key package kept %x", signer, drew.signPriv)
		}
		advanced := mustProviderOver(t, keyPackageTestSuite, bytes.NewReader(stream[params.NsigPriv:]))
		handed, handedInit, handedEnc, handedErr := NewKeyPackageWithSigner(advanced,
			keyPackageTestSuite, SignaturePrivateKey(signer), cred, testKeyPackageCapabilities(), nil)
		if handedErr != nil {
			t.Fatalf("NewKeyPackageWithSigner over the advanced stream: %v", handedErr)
		}
		if drew.LeafNode.Lifetime != handed.LeafNode.Lifetime {
			continue
		}

		drewBytes, err := syntax.Marshal(drew)
		if err != nil {
			t.Fatalf("Marshal what NewKeyPackage answered: %v", err)
		}
		handedBytes, err := syntax.Marshal(handed)
		if err != nil {
			t.Fatalf("Marshal what NewKeyPackageWithSigner answered: %v", err)
		}
		if !bytes.Equal(drewBytes, handedBytes) {
			at := 0
			for at < len(drewBytes) && at < len(handedBytes) && drewBytes[at] == handedBytes[at] {
				at++
			}
			t.Fatalf("the two constructors answered %d and %d octets, first differing at offset %d: %x against %x",
				len(drewBytes), len(handedBytes), at,
				drewBytes[at:min(at+16, len(drewBytes))], handedBytes[at:min(at+16, len(handedBytes))])
		}
		// the PRIVATE halves as well as the encoding. An encoding comparison alone is satisfied
		// by a wrapper that answered the two HPKE privates in the opposite order, and by one
		// that erased the seed it drew before the delegate cloned it.
		if !bytes.Equal(drewInit, handedInit) {
			t.Errorf("the init private halves are %x and %x", drewInit, handedInit)
		}
		if !bytes.Equal(drewEnc, handedEnc) {
			t.Errorf("the encryption private halves are %x and %x", drewEnc, handedEnc)
		}
		if !bytes.Equal(drew.signPriv, handed.signPriv) {
			t.Errorf("the retained seeds are %x and %x", drew.signPriv, handed.signPriv)
		}

		// PROPERTY 2, and it is what makes the ordering claim falsifiable rather than
		// decorative: the equality holds ONLY over the advanced stream. Over the UNADVANCED one
		// the signer-taking constructor derives its init pair from the octets the signature draw
		// consumed, so the two answers differ -- which is exactly what every deterministic
		// provider test in this package observes about the draw order.
		unadvanced := mustProviderOver(t, keyPackageTestSuite, bytes.NewReader(stream))
		fromTheTop, _, _, err := NewKeyPackageWithSigner(unadvanced, keyPackageTestSuite,
			SignaturePrivateKey(signer), cred, testKeyPackageCapabilities(), nil)
		if err != nil {
			t.Fatalf("NewKeyPackageWithSigner over the unadvanced stream: %v", err)
		}
		if bytes.Equal(fromTheTop.InitKey, drew.InitKey) {
			t.Errorf("the signer-taking constructor answered the same init_key %x over the stream from its start as NewKeyPackage did after a signature draw, so the signature key is not drawn first",
				fromTheTop.InitKey)
		}
		return
	}
}

// TestNewKeyPackageErasesTheSeedItDrewBeforeItReturns is the orphan half of this package's erase
// discipline, and it is the half no field-by-field gate can reach.
//
// Before task 2 the freshly drawn signature seed had exactly ONE holder and
// (*KeyPackage).Zeroize reached it. After it there are TWO: SignatureKeyPair hands back the seed
// buffer itself, the wrapper passes it to the delegate, and the delegate CLONES it -- so the
// wrapper is left holding an array only it can reach and nothing else erases. mls's own
// staged_erase_test.go cannot see this: that gate holds struct fields and this is a local.
//
// The observation is taken on the ERROR path as well as on the success path, because a body that
// erased after the delegation returned successfully leaves the seed drawn on every refusal the
// delegate has.
func TestNewKeyPackageErasesTheSeedItDrewBeforeItReturns(t *testing.T) {
	inner, err := NewCryptoProvider(keyPackageTestSuite)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	// the alias control first: the instrument has to actually alias, or this whole gate is a
	// photograph of an array nobody erased.
	control := &seedRetainingProvider{CryptoProvider: inner}
	seed, _, err := control.SignatureKeyPair()
	if err != nil {
		t.Fatalf("SignatureKeyPair through the retaining provider: %v", err)
	}
	if len(control.retained) != 1 || &control.retained[0][0] != &seed[0] {
		t.Fatalf("the retaining provider kept a copy rather than the array it answered, so this gate observes nothing")
	}

	for _, testCase := range []struct {
		what string
		call func(crypto CryptoProvider) error
	}{
		{what: "a mint that succeeded", call: func(crypto CryptoProvider) error {
			_, _, _, mintErr := NewKeyPackage(crypto, keyPackageTestSuite,
				BasicCredential([]byte("alice")), testKeyPackageCapabilities(), nil)
			return mintErr
		}},
		// a refusal the DELEGATE raises, after the wrapper has already drawn. It is the
		// provider that refuses, because NewLeafNode's only refusals are a nil provider and a
		// bad signer and the wrapper hands it neither -- so a body that erased after a
		// SUCCESSFUL delegation is green everywhere except here.
		{what: "a mint the delegate refused", call: func(crypto CryptoProvider) error {
			crypto.(*seedRetainingProvider).refuseDerive = true
			_, _, _, mintErr := NewKeyPackage(crypto, keyPackageTestSuite,
				BasicCredential([]byte("alice")), testKeyPackageCapabilities(), nil)
			if !errors.Is(mintErr, errSeedRetainingProviderRefuses) {
				return fmt.Errorf("this row meant to reach the delegate's refusal and the mint answered %v", mintErr)
			}
			return nil
		}},
	} {
		retaining := &seedRetainingProvider{CryptoProvider: inner}
		if err := testCase.call(retaining); err != nil {
			t.Errorf("%s: %v", testCase.what, err)
			continue
		}
		if len(retaining.retained) != 1 {
			t.Errorf("%s drew %d signature seeds, want exactly one", testCase.what, len(retaining.retained))
			continue
		}
		for _, octet := range retaining.retained[0] {
			if octet != 0 {
				t.Errorf("%s left the seed it drew in the heap: %x", testCase.what, retaining.retained[0])
				break
			}
		}
	}
}

// keyPackageSignatureLabelReferences reads this package's non test source and splits every
// reference to the identifier keyPackageSignatureLabel into three: the sites that hand it to a
// SIGNING call, the sites that hand it to a VERIFYING one, and everything else -- which at this
// commit is the constant's own declaration.
//
// It is read off the parse tree rather than grepped, because the question is which CALL an
// identifier is an argument to and a line command cannot answer that. The query a reader can run
// beside it is `grep -rn "keyPackageSignatureLabel" --include=*.go mls/`, which answers 11 lines
// at a1f8025: three production and eight across five test files, of which four are SignWithLabel
// sites of their own. This gate's class is the PRODUCTION signing sites alone, and the two
// production members it removes are printed rather than swallowed -- a constant cannot sign and a
// verifier is the reader rather than the writer, and a class that swallowed them could not tell a
// second signer from a second verifier.
func keyPackageSignatureLabelReferences(t *testing.T) (signing []string, verifying []string, other []string) {
	t.Helper()
	for _, path := range packageLevelFunctions(t).files {
		parsed := mustParseSource(t, path)
		ast.Inspect(parsed.file, func(node ast.Node) bool {
			call, isCall := node.(*ast.CallExpr)
			if !isCall {
				return true
			}
			for _, argument := range call.Args {
				identifier, isIdentifier := argument.(*ast.Ident)
				if !isIdentifier || identifier.Name != keyPackageSignatureLabelIdentifier {
					continue
				}
				at := fmt.Sprintf("%s:%d", path, parsed.fileSet.Position(identifier.Pos()).Line)
				callee := ""
				if selector, isSelector := call.Fun.(*ast.SelectorExpr); isSelector {
					callee = selector.Sel.Name
				} else if name, isName := call.Fun.(*ast.Ident); isName {
					callee = name.Name
				}
				switch {
				case strings.Contains(callee, "Verify"):
					verifying = append(verifying, at+" "+callee)
				case strings.Contains(callee, "Sign"):
					signing = append(signing, at+" "+callee)
				default:
					other = append(other, at+" "+callee)
				}
			}
			return true
		})
		// and the references that are not arguments to a call at all -- the declaration itself
		ast.Inspect(parsed.file, func(node ast.Node) bool {
			spec, isSpec := node.(*ast.ValueSpec)
			if !isSpec {
				return true
			}
			for _, name := range spec.Names {
				if name.Name == keyPackageSignatureLabelIdentifier {
					other = append(other, fmt.Sprintf("%s:%d declaration",
						path, parsed.fileSet.Position(name.Pos()).Line))
				}
			}
			return true
		})
	}
	slices.Sort(signing)
	slices.Sort(verifying)
	slices.Sort(other)
	return signing, verifying, other
}

// The identifier the gate above reads, spelled once as a string so that the gate names the
// SYMBOL and a body that spelled the label as a literal cannot satisfy it by accident.
const keyPackageSignatureLabelIdentifier = "keyPackageSignatureLabel"

// TestExactlyOneProductionSiteSignsUnderTheKeyPackageLabel is the half of task 2's one-body claim
// that the landed composition gate cannot see.
//
// labelled_composition_test.go keys on the SignWithLabel CALL and not on how its label argument
// was written, so a second constructor spelling "KeyPackageTBS" as a string literal keeps its row
// and that gate stays green. A second spelling of the label is one of the two ways over this
// package's wall -- messagegroup/engine.go names both -- so this one is over the IDENTIFIER.
//
// The complement is printed with the class: the constant's own declaration and the
// VerifyWithLabel inside Validate. Both are production references and neither is a signer, and a
// class that took "every production reference" would have counted three where the property is
// one.
func TestExactlyOneProductionSiteSignsUnderTheKeyPackageLabel(t *testing.T) {
	signing, verifying, other := keyPackageSignatureLabelReferences(t)
	t.Logf("production references to %s: %d signing (%v), %d verifying (%v), %d neither (%v)",
		keyPackageSignatureLabelIdentifier, len(signing), signing, len(verifying), verifying,
		len(other), other)
	if len(signing) != 1 {
		t.Errorf("%d production sites hand %s to a signing call: %v. After task 2 there is exactly one body in this package that assembles a KeyPackageTBS, and exactly one that signs it",
			len(signing), keyPackageSignatureLabelIdentifier, signing)
	}
	if len(signing) == 1 && !strings.Contains(signing[0], "key_package.go") {
		t.Errorf("the one signing site is %s, and the assembly this label covers lives in key_package.go", signing[0])
	}
	// the complement, and it is not empty: a narrowing whose complement is empty removes nothing
	// today and starts removing real members the day one appears.
	if len(verifying)+len(other) == 0 {
		t.Errorf("this narrowing removed nothing from the production references, so it is not a narrowing")
	}
	if len(verifying) != 1 {
		t.Errorf("%d production sites hand %s to a verifying call: %v, want the one inside Validate",
			len(verifying), keyPackageSignatureLabelIdentifier, verifying)
	}
	if len(other) != 1 {
		t.Errorf("%d production references to %s are neither: %v, want the constant's own declaration",
			len(other), keyPackageSignatureLabelIdentifier, other)
	}
}
