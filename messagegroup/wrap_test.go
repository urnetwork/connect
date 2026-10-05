// The device wrap's door, held as properties rather than as a round trip.
//
// WHY A ROUND TRIP IS NOT ENOUGH HERE, which is the sentence the whole file is arranged around. A
// sealer and an opener written by one author agree about the order of wrap_key's nine info
// elements, about which thirty two of the fifty six octets are the key, about whether the AEAD
// takes an aad, and about which octets of the eleven-octet envelope anything covers -- whatever
// those answers happen to be. Every one of those mistakes returns well formed octets and round
// trips perfectly. So the round trip is one case here and the rest are: known answers a second
// implementation can reproduce, a structural reading of what the door reaches, an inline negative
// control that fires for its own reason in the same run, and one MEASUREMENT of how much of the
// envelope is authenticated -- which is the number this door's honesty rests on and which this
// file prints on every run rather than asserting from a table.
package messagegroup

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/urnetwork/connect/mls"
)

// ---------------------------------------------------------------------------
// the two exporter labels and the two kdf labels
// ---------------------------------------------------------------------------

// Property: env_key's exporter label is MASTER's, its width is a class key's, and it does not
// collide with the only other label this package exports under.
//
// The literals are TRANSCRIBED and never referenced, which is the whole point: a case that
// compared the constant against itself passes against any drift at all. Two clients disagreeing
// here seal every device wrap of every epoch under two different outer keys and neither ever opens
// the other's.
func TestTheEnvelopeExporterLabelIsPinnedAndDoesNotCollideWithTheStorageOne(t *testing.T) {
	const fromMaster = "URmessage/v1/envelope"
	if envKeyLabel != fromMaster {
		t.Errorf("this package exports env_key under %q and MASTER section 8.2 fixes %q", envKeyLabel, fromMaster)
	}
	if EnvKeyBytes != 32 {
		t.Errorf("this package exports %d octets of env_key and MASTER section 8.2 gives 32", EnvKeyBytes)
	}
	// and the width is a CLASS KEY's, because env_key[k] stands where the class key stands at the
	// head of the device wrap's ladder. RecordKeyZero refuses anything else, so a disagreement
	// between these two numbers is a panic on the fan-out's first leaf rather than a test.
	if EnvKeyBytes != classKeyBytes {
		t.Errorf("env_key is %d octets and a class key is %d; WrapRecordKeyZero hands one to the other",
			EnvKeyBytes, classKeyBytes)
	}
	// neither label is the whole of the other, in both directions
	for _, pair := range [][2]string{{envKeyLabel, mlsSecretLabel}, {mlsSecretLabel, envKeyLabel}} {
		if strings.HasPrefix(pair[0], pair[1]) {
			t.Errorf("%q is the whole of %q; two exporter labels at one epoch are two secrets and the label is all that separates them",
				pair[0], pair[1])
		}
	}
	// and the separation is LOAD BEARING rather than a fact about two strings: the same handle at
	// the same epoch answers different octets under the two, and a neighbouring label answers
	// different octets again -- so this case could see a drifted constant.
	fixture := newTestSession(t, "envelope-label")
	envKey, err := EnvKey(fixture.handle)
	if err != nil {
		t.Fatalf("EnvKey: %v", err)
	}
	if len(envKey) != EnvKeyBytes {
		t.Fatalf("EnvKey answered %d octets, want %d", len(envKey), EnvKeyBytes)
	}
	mlsSecret, err := fixture.handle.Export(mlsSecretLabel, nil, mlsSecretBytes)
	if err != nil {
		t.Fatalf("Export under the storage label: %v", err)
	}
	if bytes.Equal(envKey, mlsSecret) {
		t.Error("env_key[k] and mls_secret[k] are the same octets, so the device wrap's outer key is the ikm of the root it exists to deliver")
	}
	drifted, err := fixture.handle.Export(fromMaster+"X", nil, EnvKeyBytes)
	if err != nil {
		t.Fatalf("Export under a drifted label: %v", err)
	}
	if bytes.Equal(envKey, drifted) {
		t.Error("two exporter labels one character apart answer the same octets, so this case could not see a drifted constant even if it were pinned")
	}
}

// The one prefix relation this package's label rule cannot avoid on the wrap's side, with the
// argument that makes it safe, held in BOTH directions.
//
// It is the same shape recordkey_test.go's rec/v1/head exemption takes and for the same reason:
// the wire is normative and the rule is connect's own stricter one. What is different is that
// only one of this pair reaches the KDF as an argument -- the salt does, and the info label is the
// first seventeen octets of a nine-element info that WrapInfo assembles -- so recordkey_test.go's
// derived class contains one of them and this case is where the pair is judged at all.
var wrapLabelPrefixDisposition = map[string]string{
	"wrapInfoLabel|wrapSaltLabel": "MASTER section 7 fixes both literals and \"URmessage/v1/wrap\" is the whole of " +
		"\"URmessage/v1/wrap-salt\". It is safe for a reason about HKDF and not about care: the salt is " +
		"Extract's SALT ARGUMENT, which is the HMAC key of HMAC(salt, ikm), and the info label is the " +
		"head of Expand's INFO, which is HMAC(prk, info | 0x01) -- two different functions with the " +
		"value in two different positions, so no truncation of one produces the other's output. And " +
		"the bare label is never a complete info: WrapInfo always continues with LP(group_id). WHAT " +
		"WOULD REMOVE IT: a spec salt that is not an extension of the info label, which is msgrepo's " +
		"to choose and is reported rather than taken here",
}

func TestTheWrapKdfLabelsArePinnedAndTheirPrefixRelationIsDispositioned(t *testing.T) {
	const saltFromMaster = "URmessage/v1/wrap-salt"
	const infoFromMaster = "URmessage/v1/wrap"
	if wrapSaltLabel != saltFromMaster {
		t.Errorf("this package extracts under %q and MASTER section 7 fixes %q", wrapSaltLabel, saltFromMaster)
	}
	if wrapInfoLabel != infoFromMaster {
		t.Errorf("this package's wrap info leads with %q and MASTER section 7 fixes %q", wrapInfoLabel, infoFromMaster)
	}
	// the CLASS: every domain separation label this package reaches a kdf or an exporter with.
	// It is four names and they are named, because the two exporter labels are not constants any
	// derived reading of "reaches keyScheduleExpand" can find -- they go to a group handle.
	labels := map[string]string{
		"mlsSecretLabel": mlsSecretLabel,
		"envKeyLabel":    envKeyLabel,
		"wrapSaltLabel":  wrapSaltLabel,
		"wrapInfoLabel":  wrapInfoLabel,
	}
	names := slices.Sorted(maps2Keys(labels))
	exercised := map[string]bool{}
	for i, left := range names {
		for _, right := range names[i+1:] {
			if labels[left] == labels[right] {
				t.Errorf("%s and %s are the same label %q", left, right, labels[left])
			}
			if !strings.HasPrefix(labels[left], labels[right]) && !strings.HasPrefix(labels[right], labels[left]) {
				continue
			}
			pair := left + "|" + right
			if _, isDispositioned := wrapLabelPrefixDisposition[pair]; isDispositioned {
				exercised[pair] = true
				continue
			}
			t.Errorf("%s (%q) is the whole of %s (%q) and no row disposes of it; a truncation of the longer one is the shorter one",
				left, labels[left], right, labels[right])
		}
	}
	// AND THE OTHER DIRECTION: a disposition that is never reached is a claim nobody measured.
	for pair := range wrapLabelPrefixDisposition {
		if !exercised[pair] {
			t.Errorf("the prefix disposition %q was never reached, so it describes a pair this package no longer has; delete it", pair)
		}
	}
	// and the property the prefix rule stands in for, over the exempted pair: the two labels
	// produce different values in the positions they are actually used in.
	material := bytes.Repeat([]byte{0x6b}, 32)
	asSalt := keyScheduleExtract([]byte(wrapSaltLabel), material)
	asTruncatedSalt := keyScheduleExtract([]byte(wrapInfoLabel), material)
	asInfo := keyScheduleExpand(material, []byte(wrapInfoLabel), 32)
	for _, pair := range [][2][]byte{{asSalt, asTruncatedSalt}, {asSalt, asInfo}, {asTruncatedSalt, asInfo}} {
		if bytes.Equal(pair[0], pair[1]) {
			t.Error("two of the three derivations the exempted prefix pair can reach answer the same thirty two octets")
		}
	}
	// and the bare label is not an info this package can ever expand under, which is the half of
	// the disposition that is a property of the code rather than of HKDF
	info := WrapInfo(WrapEnvelope{FormatVersion: WrapFormatVersion}, nil, nil, XwingAlgId, nil, nil)
	if string(info) == wrapInfoLabel {
		t.Error("WrapInfo over empty inputs IS the bare label, so a truncation of the salt is an info this package expands under")
	}
	if !bytes.HasPrefix(info, []byte(wrapInfoLabel)) {
		t.Errorf("the wrap info does not lead with %q", wrapInfoLabel)
	}
}

// maps2Keys is a local spelling of maps.Keys so this file does not add an import for one call.
func maps2Keys(m map[string]string) func(func(string) bool) {
	return func(yield func(string) bool) {
		for key := range m {
			if !yield(key) {
				return
			}
		}
	}
}

// ---------------------------------------------------------------------------
// ledger ruling 37: env_key[n+1] before the merge is env_key[n+1] after it
// ---------------------------------------------------------------------------

// Property: the committer can compute the envelope key of the epoch its staged commit OPENS,
// before the delivery service has answered, and what it computes is what every member derives
// after applying the same commit.
//
// THIS IS WHAT MAKES RULING 37 BUILDABLE AND IT IS NOT A CONVENIENCE. The fan-out used to be
// published after the merge, so the wrap rows carried record epoch n+1 -- and item 246's epoch
// ceiling serves a reader standing at epoch n only rows with epoch <= n, so read_key[n+1] needed
// pq_secret[n+1] needed the wrap needed read_key[n+1]. The ruling breaks the cycle by submitting
// the wraps at epoch n, staged, still sealed under env_key[n+1]. If this property were false the
// ruling would be unimplementable and the fallback -- exempting wrap rows from the ceiling --
// re-opens a ruled item.
//
// It is built beside enginepending_test.go's TestThePendingReadsAnswerWhatTheMergeInstalls, which
// already holds exactly this for the STORAGE exporter. The storage label's passing case is not
// evidence for this one on its own -- PendingExport takes the label as an argument and could in
// principle answer a cached value for one label and a live one for another -- so the whole chain
// is re-run here under the envelope label, with the same control.
func TestEnvKeyReadBeforeTheMergeIsWhatTheMergeInstalls(t *testing.T) {
	chain := newCommitAddChain(t, "envelope-pending")

	// with nothing staged there is no epoch to answer for, which is what keeps a fan-out from
	// being built out of the epoch the group is already in
	if key, err := PendingEnvKey(chain.founded); !errors.Is(err, mls.ErrNoPendingCommit) {
		t.Fatalf("PendingEnvKey with nothing staged answered %d octets, %v; want ErrNoPendingCommit", len(key), err)
	}

	liveBefore, err := EnvKey(chain.founded)
	if err != nil {
		t.Fatalf("EnvKey before the commit: %v", err)
	}
	third := newTestEngine(t)
	keyPackage, err := third.engine.NewKeyPackage()
	if err != nil {
		t.Fatalf("NewKeyPackage: %v", err)
	}
	commit, _, _, err := chain.founded.CommitAdd([][]byte{keyPackage})
	if err != nil {
		t.Fatalf("CommitAdd: %v", err)
	}
	staged, err := PendingEnvKey(chain.founded)
	if err != nil {
		t.Fatalf("PendingEnvKey: %v", err)
	}
	// THE CONTROL, and it fires for its own reason: the staged value is not the live one, and the
	// live handle has not moved. Without it an implementation that answered the CURRENT epoch's
	// envelope key from both doors would pass every other clause of this case.
	if bytes.Equal(staged, liveBefore) {
		t.Fatal("PendingEnvKey answered the LIVE epoch's envelope key, so the read is off the wrong schedule and every clause below is vacuous")
	}
	if stillLive, err := EnvKey(chain.founded); err != nil || !bytes.Equal(stillLive, liveBefore) {
		t.Fatalf("the live handle moved while a commit was staged: %v", err)
	}

	// the receiver applies the same commit, the committer merges, and all three agree
	processed, err := chain.joined.Process(commit)
	if err != nil {
		t.Fatalf("the joiner's Process: %v", err)
	}
	if err := chain.joined.ApplyCommit(processed); err != nil {
		t.Fatalf("the joiner's ApplyCommit: %v", err)
	}
	if err := chain.founded.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit: %v", err)
	}
	merged, err := EnvKey(chain.founded)
	if err != nil {
		t.Fatalf("EnvKey after the merge: %v", err)
	}
	if !bytes.Equal(merged, staged) {
		t.Errorf("the committer sealed its fan-out under %x and entered an epoch whose envelope key is %x", staged, merged)
	}
	receiver, err := EnvKey(chain.joined)
	if err != nil {
		t.Fatalf("the receiver's EnvKey: %v", err)
	}
	if !bytes.Equal(receiver, staged) {
		t.Errorf("a receiver that applied the same commit derives %x and the committer sealed under %x; no member could open its own wrap",
			receiver, staged)
	}
	if key, err := PendingEnvKey(chain.founded); !errors.Is(err, mls.ErrNoPendingCommit) {
		t.Errorf("PendingEnvKey after the merge answered %d octets, %v; want ErrNoPendingCommit", len(key), err)
	}
}

// ---------------------------------------------------------------------------
// the door, through a real leaf keys extension
// ---------------------------------------------------------------------------

// wrapTestLeaf is one device's X-Wing half as the group publishes it: the seed the device keeps,
// the extension octets its leaf carries, and the public key parsed back out of those octets.
type wrapTestLeaf struct {
	seed      []byte
	extension []byte
	pub       *XwingPublicKey
	priv      *XwingPrivateKey
}

// newWrapTestLeaf builds one, THROUGH THE WIRE and not around it.
//
// The public key this seals to is the one that comes back out of ParseLeafKeysExtension over the
// encoded urmessage_leaf_keys body -- extension type 0xF002, alg 0x0014 -- rather than the one the
// key generator answered, because the path a fan-out actually takes is MemberAt's leafKeys octets
// and a test that sealed to the generator's own value could not see an encoder that dropped or
// reordered a byte.
//
// The private half is derived from the SEED, which is what s2-26 landed on the other side of this
// door: a device retains the thirty two octet seed behind the public half its leaf publishes, and
// derives the key through this package's XwingKeyGenFromSeed. Before that, device.go encoded only
// .Public() and no device could open a wrap addressed to its own leaf at all.
func newWrapTestLeaf(t *testing.T, fill byte) *wrapTestLeaf {
	t.Helper()
	seed := bytes.Repeat([]byte{fill}, XwingSeedSize)
	priv, err := XwingKeyGenFromSeed(seed)
	if err != nil {
		t.Fatalf("XwingKeyGenFromSeed: %v", err)
	}
	encoded, err := (&mls.LeafKeysExtension{
		AlgId:          mls.AlgIdXwing,
		DeviceXwingPub: priv.Public().Bytes(),
	}).Encode()
	if err != nil {
		t.Fatalf("encode urmessage_leaf_keys: %v", err)
	}
	if encoded.ExtensionType != mls.ExtensionTypeUrmessageLeafKeys {
		t.Fatalf("the extension is tagged %#04x, want urmessage_leaf_keys", uint16(encoded.ExtensionType))
	}
	parsed, err := mls.ParseLeafKeysExtension(encoded.ExtensionData)
	if err != nil {
		t.Fatalf("ParseLeafKeysExtension: %v", err)
	}
	pub, err := ParseXwingPublicKey(parsed.DeviceXwingPub)
	if err != nil {
		t.Fatalf("ParseXwingPublicKey over the published octets: %v", err)
	}
	// a second derivation of the same private half, so the seed really is what the device keeps
	fromSeed, err := XwingKeyGenFromSeed(seed)
	if err != nil {
		t.Fatalf("XwingKeyGenFromSeed a second time: %v", err)
	}
	if !bytes.Equal(fromSeed.Public().Bytes(), parsed.DeviceXwingPub) {
		t.Fatal("the key derived from the retained seed does not answer the public half the leaf published")
	}
	return &wrapTestLeaf{seed: seed, extension: encoded.ExtensionData, pub: pub, priv: fromSeed}
}

func wrapTestGroupId() []byte  { return bytes.Repeat([]byte{0x21}, 32) }
func wrapTestTargetId() []byte { return bytes.Repeat([]byte{0x71}, 16) }

// wrapTestAuthority stands for an opener whose OWN authority -- the epoch it is restoring and the
// record kind it asked for -- happens to be what some envelope says. It answers the three values
// in OpenWrapBody's own order, which is MASTER section 7's info order.
//
// IT IS A TEST HELPER AND IT IS NOT AN EXPORTED ONE, which is the whole point of OpenWrapBody's
// three expectation arguments: a production caller that built its expectation out of the body in
// front of it would have compared a value against itself, and this package gives it no door to do
// that through. Here the cases that use it are the ones whose subject is something else -- the
// KEM, the group binding, which octets the AEAD covers -- and every case whose subject IS the
// comparison writes its three values out as literals instead.
func wrapTestAuthority(envelope WrapEnvelope) (contentEpoch uint64, targetType uint8, payloadType uint8) {
	return envelope.ContentEpoch, envelope.TargetType, envelope.PayloadType
}

// wrapTestPayload is a payload of the shape MASTER section 7 puts inside aead_ct --
// secret | LP(identity_pub) | sig -- so the sizing this file asserts is the sizing MASTER
// publishes. The signature octets are fill: task 14 step 3 is blocked by open item M1-52 and
// nothing in this tree can sign a wrap.
func wrapTestPayload(secretFill byte) []byte {
	out := []byte{}
	out = append(out, bytes.Repeat([]byte{secretFill}, 32)...)
	out = append(out, 0x00, 0x00, 0x00, 0x20)
	out = append(out, bytes.Repeat([]byte{0x22}, 32)...)
	return append(out, bytes.Repeat([]byte{0x33}, 64)...)
}

// Property: a wrap sealed to a leaf's published X-Wing key opens under that leaf's own private
// half and under no other leaf's -- and the negative control fires in the same run, for its own
// reason.
//
// THE NEGATIVE CONTROL IS INLINE BECAUSE THE KEM MAKES IT NECESSARY. ML-KEM-768 rejects
// implicitly: a ciphertext not produced for this key decapsulates SUCCESSFULLY to a pseudorandom
// secret. So the second leaf's attempt reaches the KDF and the AEAD with thirty two perfectly well
// formed octets, and the only thing that separates it from the first leaf's attempt is the
// Poly1305 tag. A case that asserted only that the right leaf opens would be satisfied by a door
// that opened for everybody.
func TestAWrapOpensForItsTargetLeafAndForNoOther(t *testing.T) {
	target := newWrapTestLeaf(t, 0x01)
	other := newWrapTestLeaf(t, 0x02)
	if bytes.Equal(target.extension, other.extension) {
		t.Fatal("the two fixture leaves publish the same extension, so this case has one leaf in it")
	}
	envelope := WrapEnvelope{FormatVersion: WrapFormatVersion, TargetType: 0x01, PayloadType: 0x01, ContentEpoch: 4}
	payload := wrapTestPayload(0x11)
	body, err := SealWrapBody(rand.Reader, target.pub, envelope, wrapTestGroupId(), wrapTestTargetId(), payload)
	if err != nil {
		t.Fatalf("SealWrapBody: %v", err)
	}

	epoch, targetType, payloadType := wrapTestAuthority(envelope)
	gotEnvelope, gotPayload, err := OpenWrapBody(target.priv, wrapTestGroupId(), epoch,
		targetType, wrapTestTargetId(), payloadType, body)
	if err != nil {
		t.Fatalf("the target leaf could not open its own wrap: %v", err)
	}
	if gotEnvelope != envelope {
		t.Errorf("the envelope came back as %+v, want %+v", gotEnvelope, envelope)
	}
	if !bytes.Equal(gotPayload, payload) {
		t.Errorf("the payload came back as %d octets and went in as %d", len(gotPayload), len(payload))
	}

	// THE CONTROL: the other leaf's decapsulation SUCCEEDS and the open refuses anyway
	_, ctXwing, _, err := parseHybridCt(body[WrapEnvelopeBytes:])
	if err != nil {
		t.Fatalf("parse hybrid_ct: %v", err)
	}
	foreign, err := XwingDecapsulate(other.priv, ctXwing)
	if err != nil {
		t.Fatalf("the KEM refused a foreign ciphertext, so implicit rejection is not what this build does and this control is measuring something else: %v", err)
	}
	if len(foreign) != XwingSharedSize {
		t.Fatalf("a foreign decapsulation answered %d octets, want %d", len(foreign), XwingSharedSize)
	}
	if _, _, err := OpenWrapBody(other.priv, wrapTestGroupId(), epoch,
		targetType, wrapTestTargetId(), payloadType, body); !errors.Is(err, ErrWrapOpen) {
		t.Errorf("a second leaf's private half answered %v; want ErrWrapOpen", err)
	}
	// and the group and the target are bound too, which is what stops one group's wrap opening
	// in another and one member's opening at another's handle
	if _, _, err := OpenWrapBody(target.priv, bytes.Repeat([]byte{0x99}, 32), epoch,
		targetType, wrapTestTargetId(), payloadType, body); !errors.Is(err, ErrWrapOpen) {
		t.Errorf("a wrap opened under a different group_id: %v", err)
	}
	if _, _, err := OpenWrapBody(target.priv, wrapTestGroupId(), epoch,
		targetType, bytes.Repeat([]byte{0x99}, 16), payloadType, body); !errors.Is(err, ErrWrapOpen) {
		t.Errorf("a wrap opened under a different target_id: %v", err)
	}
}

// Property: a wrap sealed at one epoch does not open as a wrap of another, in all three of the
// places the epoch is bound -- and the FIRST of them is the one a reader would assume and is the
// one that needed a seam built for it.
//
// THE GENUINE CASE AND THE TAMPERED CASE ARE NOT THE SAME CASE, and this case used to hold only
// the second of them. u64(content_epoch) is one of wrap_key's nine info elements, so an opener
// handed a body whose envelope was EDITED on the wire derives a key the sealer never used and the
// AEAD refuses -- that is the tampered half, and it is real. But the key is derived from the
// envelope the body CARRIES, so a genuine wrap of another epoch is self-consistent: its key
// matches its own envelope and the AEAD opens it. Measured on this door before the expectation
// argument existed: a genuine wrap of content epoch 10 opened and returned its payload byte for
// byte. The headline of m1 task 14 property 4 is about that wrap, not about the edited one, and it
// is true here only because OpenWrapBody takes the epoch its opener is honouring, as a parameter
// with no default, and refuses anything else -- see OpenWrapBody.
//
// The THIRD is the outer seal: env_key[k] is an epoch's own exporter output, so the record ladder
// a wrap of epoch n+1 rides is not the ladder a wrap of epoch n rides, and the two are separated
// before any wrap body is reached. That half is the record layer's and not this door's.
func TestAWrapSealedAtOneEpochDoesNotOpenAtAnother(t *testing.T) {
	target := newWrapTestLeaf(t, 0x03)
	payload := wrapTestPayload(0x44)
	body, err := SealWrapBody(rand.Reader, target.pub,
		WrapEnvelope{FormatVersion: WrapFormatVersion, TargetType: 0x01, PayloadType: 0x01, ContentEpoch: 9},
		wrapTestGroupId(), wrapTestTargetId(), payload)
	if err != nil {
		t.Fatalf("SealWrapBody: %v", err)
	}
	// the three the opener states, written out rather than read off the body: this case's whole
	// subject is the comparison, and an expectation copied out of the envelope in front of it
	// would be a value compared against itself.
	const atNine, atEight uint64 = 9, 8
	// it opens at its own epoch, which is the control that makes the refusals below mean something
	if _, _, err := OpenWrapBody(target.priv, wrapTestGroupId(), atNine,
		0x01, wrapTestTargetId(), 0x01, body); err != nil {
		t.Fatalf("the wrap does not open at the epoch it was sealed at: %v", err)
	}
	// THE GENUINE HALF: the body is untouched and every octet in it is the sealer's. An opener
	// honouring epoch 8 must not be handed epoch 9's secret, and nothing in the KEM or the AEAD
	// can tell it so -- the refusal here is the opener's own authority and it is asserted BY NAME,
	// because an ErrWrapOpen here would mean the key moved and the key does not move.
	genuineEnvelope, genuinePayload, err := OpenWrapBody(target.priv, wrapTestGroupId(), atEight,
		0x01, wrapTestTargetId(), 0x01, body)
	if !errors.Is(err, ErrWrapEnvelopeMismatch) {
		t.Errorf("a GENUINE wrap of epoch 9 handed to an opener honouring epoch 8 answered %v; want ErrWrapEnvelopeMismatch", err)
	}
	if errors.Is(err, ErrWrapOpen) {
		t.Error("the genuine epoch 9 wrap was refused as a tag failure, so this case is measuring the AEAD and the AEAD cannot see this")
	}
	if genuinePayload != nil || genuineEnvelope != (WrapEnvelope{}) {
		t.Errorf("a refused wrap carried %d octets of payload and the envelope %+v out to its caller",
			len(genuinePayload), genuineEnvelope)
	}
	// and the refusal does not depend on the rest of the body: it is ahead of the KEM, so a body
	// whose hybrid_ct is destroyed is still refused as the wrong wrap rather than as bad framing.
	truncated := slices.Clone(body[:WrapEnvelopeBytes+4])
	if _, _, err := OpenWrapBody(target.priv, wrapTestGroupId(), atEight,
		0x01, wrapTestTargetId(), 0x01, truncated); !errors.Is(err, ErrWrapEnvelopeMismatch) {
		t.Errorf("a wrap of the wrong epoch whose hybrid_ct is four octets answered %v; want ErrWrapEnvelopeMismatch, which is what puts the comparison ahead of the KEM", err)
	}
	// THE TAMPERED HALF: the same edit made on the wire moves wrap_key, and this is the one the
	// AEAD convicts. The opener still honours epoch 8, so the comparison passes and the tag is
	// what refuses -- which is how this assertion stays a measurement of the key and not of the
	// comparison above it.
	restated := slices.Clone(body)
	edited, err := ParseWrapEnvelope(restated[:WrapEnvelopeBytes])
	if err != nil {
		t.Fatalf("ParseWrapEnvelope: %v", err)
	}
	edited.ContentEpoch = 8
	copy(restated[:WrapEnvelopeBytes], edited.Encode())
	if _, _, err := OpenWrapBody(target.priv, wrapTestGroupId(), atEight,
		0x01, wrapTestTargetId(), 0x01, restated); !errors.Is(err, ErrWrapOpen) {
		t.Errorf("a wrap whose content epoch was moved from 9 to 8 answered %v; want ErrWrapOpen", err)
	}

	// the outer half: two epochs' envelope keys start two ladders, so the record that carries a
	// wrap of epoch n+1 is not keyed under epoch n's root at any rung
	chain := newCommitAddChain(t, "envelope-epochs")
	before, err := EnvKey(chain.founded)
	if err != nil {
		t.Fatalf("EnvKey: %v", err)
	}
	third := newTestEngine(t)
	keyPackage, err := third.engine.NewKeyPackage()
	if err != nil {
		t.Fatalf("NewKeyPackage: %v", err)
	}
	if _, _, _, err := chain.founded.CommitAdd([][]byte{keyPackage}); err != nil {
		t.Fatalf("CommitAdd: %v", err)
	}
	if err := chain.founded.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit: %v", err)
	}
	after, err := EnvKey(chain.founded)
	if err != nil {
		t.Fatalf("EnvKey after the commit: %v", err)
	}
	if bytes.Equal(before, after) {
		t.Fatal("two epochs answered one envelope key, so this half is judging one epoch")
	}
	for _, leaf := range []uint32{0, 1, 0xFFFFFFFF} {
		if bytes.Equal(WrapRecordKeyZero(before, leaf), WrapRecordKeyZero(after, leaf)) {
			t.Errorf("leaf %d starts the same wrap ladder at both epochs", leaf)
		}
	}
}

// ---------------------------------------------------------------------------
// the measurement this door's honesty rests on
// ---------------------------------------------------------------------------

// Property: of the eleven octets a wrap envelope carries in the clear, exactly the ones MASTER
// section 7's info binds are refused when they are edited on the wire -- and the suite REPORTS
// which those are rather than asserting a fixed table.
//
// WHY IT IS A MEASUREMENT AND NOT AN ASSERTION. m1 task 14 property 9 states the shape: which
// octets can convict an attacker is a function of two sentences no document rules -- M1-54, what
// an opener does with an unrecognised wrap_format_version, and M1-55, whether an opener derives
// wrap_key from the envelope's CARRIED values or from its own. A fixed table here would
// presuppose one answer to each and would go red on a conforming implementation the day either is
// ruled the other way. So the set is measured, printed, and asserted only in the two directions
// that are true under every reading: every octet the info binds is REFUSED, and every octet it
// does not is REPORTED.
//
// WHAT IT MEASURES TODAY, and this is open item MG-7. This door derives wrap_key from the carried
// values, which is M1-55's first reading and the one MASTER's own rationale describes, and it
// refuses nothing on the version octet, which is M1-54 left unruled. Under that pair the killing
// set is ten of eleven: the content epoch, the target type and the payload type are in the info
// and the AEAD refuses an edit to any of them, and u8(wrap_format_version) is in NO element of
// info at all. The only construction in the corpus that ever covers it is the body signature,
// whose preimage MASTER extends by LP(wrap_envelope) for exactly this reason -- and that signature
// is task 14 step 3, blocked by M1-52. So today a wrap whose version octet was changed on the wire
// opens to the payload it carried, with no refusal anywhere, and that is printed rather than
// hidden behind a green round trip.
//
// THERE ARE TWO AUTHORITIES IN THIS DOOR AND THE CASE MEASURES THEM SEPARATELY, which is what the
// single column it used to print could not do. The AEAD convicts an octet only when the opener's
// own expectation MOVED WITH THE EDIT -- an opener that believes the edited value, which is the
// hardest case for the key and therefore the one worth measuring -- and the opener's comparison
// convicts an octet only when it did NOT. Running both columns is what separates "the key covers
// this octet" from "somebody happened to look at it", and their union's complement is the octet
// nothing in this door covers at all.
func TestTheEnvelopeOctetsTheWrapKeyBindsAreRefusedAndTheSuiteReportsTheRest(t *testing.T) {
	target := newWrapTestLeaf(t, 0x05)
	envelope := WrapEnvelope{FormatVersion: WrapFormatVersion, TargetType: 0x02, PayloadType: 0x03, ContentEpoch: 11}
	payload := wrapTestPayload(0x55)
	body, err := SealWrapBody(rand.Reader, target.pub, envelope, wrapTestGroupId(), wrapTestTargetId(), payload)
	if err != nil {
		t.Fatalf("SealWrapBody: %v", err)
	}
	sealedEpoch, sealedTargetType, sealedPayloadType := wrapTestAuthority(envelope)
	if _, _, err := OpenWrapBody(target.priv, wrapTestGroupId(), sealedEpoch,
		sealedTargetType, wrapTestTargetId(), sealedPayloadType, body); err != nil {
		t.Fatalf("the unedited wrap does not open, so every row below is measuring the wrong thing: %v", err)
	}
	refused := []int{}
	accepted := []int{}
	compared := []int{}
	for octet := 0; octet < WrapEnvelopeBytes; octet += 1 {
		edited := slices.Clone(body)
		edited[octet] ^= 0xFF
		moved, err := ParseWrapEnvelope(edited[:WrapEnvelopeBytes])
		if err != nil {
			t.Fatalf("the envelope with octet %d edited does not parse: %v", octet, err)
		}
		// COLUMN ONE, the key: the opener's own authority is moved to agree with the edit, so
		// the comparison passes and the only thing left that can refuse is the tag.
		movedEpoch, movedTargetType, movedPayloadType := wrapTestAuthority(moved)
		_, gotPayload, err := OpenWrapBody(target.priv, wrapTestGroupId(), movedEpoch,
			movedTargetType, wrapTestTargetId(), movedPayloadType, edited)
		switch {
		case err == nil:
			accepted = append(accepted, octet)
			// an accepted edit must at least deliver the SAME payload: an octet that
			// changed what came out while being accepted would be a third outcome this
			// reading does not have a name for
			if !bytes.Equal(gotPayload, payload) {
				t.Errorf("editing envelope octet %d was accepted AND changed the payload; that is neither of the two outcomes this reading separates", octet)
			}
		case errors.Is(err, ErrWrapOpen):
			refused = append(refused, octet)
		default:
			t.Errorf("editing envelope octet %d answered %v, which is neither the AEAD's refusal nor an acceptance; an octet that leaves the set for a third reason is a finding",
				octet, err)
		}
		// COLUMN TWO, the opener: the authority stays where the sealer put it, so an octet the
		// expectation covers is refused by name before the KEM is reached.
		switch _, _, err := OpenWrapBody(target.priv, wrapTestGroupId(), sealedEpoch,
			sealedTargetType, wrapTestTargetId(), sealedPayloadType, edited); {
		case errors.Is(err, ErrWrapEnvelopeMismatch):
			compared = append(compared, octet)
		case err == nil || errors.Is(err, ErrWrapOpen):
		default:
			t.Errorf("editing envelope octet %d answered %v against an unmoved authority, which is neither of this column's outcomes", octet, err)
		}
	}
	uncovered := []int{}
	for octet := 0; octet < WrapEnvelopeBytes; octet += 1 {
		if !slices.Contains(refused, octet) && !slices.Contains(compared, octet) {
			uncovered = append(uncovered, octet)
		}
	}
	t.Logf("wrap envelope, %d octets: the AEAD refuses %v and accepts %v when the opener's authority moves with the edit; the opener's own comparison refuses %v when it does not; %v is covered by neither",
		WrapEnvelopeBytes, refused, accepted, compared, uncovered)
	// THE HALF THAT IS TRUE UNDER EVERY READING: the ten octets MASTER's info binds are refused
	// BY THE KEY, measured against an opener that was fooled into agreeing with the edit. Octet 0
	// is the version and is the one the info does not reach.
	for _, octet := range []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10} {
		if !slices.Contains(refused, octet) {
			t.Errorf("envelope octet %d is bound into wrap_key by MASTER section 7's info and editing it was not refused; the key is not being derived from the envelope's carried values",
				octet)
		}
	}
	// AND THE REPORTED HALF, held against a written-down disposition rather than against a
	// preference. It is asserted in BOTH directions: a set that grew has a new unauthenticated
	// octet nobody named, and a set that shrank means an authority arrived -- which is what the
	// signature landing looks like, and which must delete this clause rather than pass quietly.
	if !slices.Equal(accepted, []int{0}) {
		t.Errorf("the accepted set is %v and the written-down disposition is exactly {0}, u8(wrap_format_version). A LARGER set is an envelope octet nothing authenticates that no document names; a SMALLER one means this door gained an authority over the version octet -- if that is task 14 step 3's signature landing, this clause and open item MG-7 come out together",
			accepted)
	}
	// AND THE SECOND COLUMN, against its own written-down disposition. The opener's comparison
	// covers exactly the ten octets the info covers -- not by coincidence: the three values
	// OpenWrapBody makes a caller state are the three envelope fields MASTER's info binds and
	// deliberately not the fourth, because ruling what an opener does with an unrecognised version
	// is M1-54's and not this package's.
	if !slices.Equal(compared, []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}) {
		t.Errorf("the opener's comparison refuses %v and the written-down disposition is exactly the ten info-bound octets. A SMALLER set is one of the three values the opener states that stopped being compared; a LARGER one means it gained the version octet, which is M1-54 being ruled here rather than in MASTER section 7",
			compared)
	}
	// and the complement of the two columns together, which is the sentence MG-7 carries: one
	// octet of the eleven is covered by NEITHER authority in this door.
	if !slices.Equal(uncovered, []int{0}) {
		t.Errorf("the octets no authority in this door covers are %v and the disposition is exactly {0}", uncovered)
	}
}

// Property: the opener NAMES every field of the envelope except the one octet no ruling reaches,
// and both complements are PRINTED and held against a written-down disposition.
//
// THE COMPLEMENT IS THE MEASUREMENT AND THE SIGNATURE IS WHERE IT IS DECIDABLE. The behavioural
// case above measures which octets each authority refuses, and it can only measure the fields that
// exist: a twelfth envelope octet added tomorrow would be carried, compared against nothing, and
// invisible there because no row would name it. This case reads WrapEnvelope's fields and
// OpenWrapBody's PARAMETER LIST out of the source and subtracts each from the other, so a field
// that arrives on the envelope without arriving on the door fails here on the day it lands.
//
// IT READS THE SIGNATURE AND NOT A STRUCT, AND THAT IS THE REPAIR RATHER THAN A RESTATEMENT. The
// three values used to arrive as one WrapExpectation argument, and this case read that type's
// fields. A struct's zero value is a complete value of it, so WrapExpectation{} stated nothing,
// compiled, and opened a genuine {0x00, 0x00, epoch 0} wrap -- measured -- which is a route no
// reading of that type could see, because from inside the type the three fields were all present.
// Read off the signature, "the caller stated it" and "the caller wrote it" are the same sentence:
// Go supplies no argument nobody wrote. See OpenWrapBody.
//
// IT FAILS IN BOTH DIRECTIONS, and the two failures mean opposite things. A complement LARGER than
// {FormatVersion} is an envelope field an opener cannot ask about -- a value on the wire that
// nothing in this door compares. A complement SMALLER than it means u8(wrap_format_version) became
// expressible, which is m1 open item M1-54 -- what an opener does with an unrecognised version --
// being decided in this package instead of in MASTER section 7, and it must arrive with the
// ruling, with the measurement above, and with MG-7's second half coming out.
func TestTheOpenerNamesEveryEnvelopeFieldButTheOneNoRulingReaches(t *testing.T) {
	_, sources := messagegroupProductionSources(t)
	envelope := messagegroupStructFields(sources, "WrapEnvelope")
	door := messagegroupFuncParams(sources, "OpenWrapBody")
	if len(envelope) != 4 {
		t.Fatalf("this reading found %d envelope fields (%v); the envelope MASTER section 7 fixes has four and this reading is not on it",
			len(envelope), envelope)
	}
	// the envelope fields the door's signature does not name
	uncovered := []string{}
	for name, kind := range envelope {
		stated, isStated := door[wrapLowerFirst(name)]
		if !isStated {
			uncovered = append(uncovered, name)
			continue
		}
		if stated != kind {
			t.Errorf("WrapEnvelope.%s is %s and OpenWrapBody's %s is %s; a comparison across two widths is a comparison that can be true of two different wire values",
				name, kind, wrapLowerFirst(name), stated)
		}
	}
	slices.Sort(uncovered)
	// AND THE OTHER COMPLEMENT, which is this case's positive control as well as its second
	// assertion: the door's parameters that name no envelope field at all. It cannot be empty --
	// a wrap is opened with a private half and over a body -- so a reading that had found no
	// signature reports {} here and fails, rather than reporting a clean {FormatVersion} above
	// for having read nothing. It is asserted against a written-down set rather than a count.
	beyond := []string{}
	for name := range door {
		if _, isEnvelopeField := envelope[wrapUpperFirst(name)]; !isEnvelopeField {
			beyond = append(beyond, name)
		}
	}
	slices.Sort(beyond)
	t.Logf("the envelope carries %d fields and OpenWrapBody takes %d parameters; envelope fields the door does not name %v, and door parameters that are no envelope field %v",
		len(envelope), len(door), uncovered, beyond)
	if !slices.Equal(uncovered, []string{"FormatVersion"}) {
		t.Errorf("the envelope fields no opener can state an expectation over are %v and the written-down disposition is exactly {FormatVersion}. A LARGER set is a wire value this door compares against nothing; a SMALLER one is m1 open item M1-54 being ruled in this package rather than in MASTER section 7, and it comes with the ruling or not at all",
			uncovered)
	}
	if !slices.Equal(beyond, []string{"body", "groupId", "priv", "targetId"}) {
		t.Errorf("OpenWrapBody's parameters that name no envelope field are %v and the written-down disposition is exactly {body, groupId, priv, targetId} -- the octets to open, the leaf's own private half, and the two of wrap_key's nine info elements that are not envelope fields. An EMPTY set means this reading did not find the door's signature and everything above it is vacuous; a LARGER one is a value this door takes that nothing in MASTER section 7's info names",
			beyond)
	}
}

// Property: the door's parameters arrive in the order wrap_key's info WRITES them, measured
// against the encoder rather than against a list copied out of MASTER section 7.
//
// WHY THIS IS A CASE AND NOT A COMMENT. OpenWrapBody's paragraph says its parameters are in
// MASTER section 7's info order, so that a call site reads as the line it is checked against and
// so that the two u8s are not adjacent -- two uint8 parameters side by side being two the
// compiler cannot tell apart. That is a claim about an ordering, and an ordering claim nothing
// reads is one the next edit moves. This case reads WrapInfo's own sequence of writes and the
// door's own parameter list, restricts each to what they have in common, and compares the two
// sequences. WrapInfo is where MASTER's order actually lives in this package -- it is the encoder
// the known answers reproduce bytewise against a second, independent transcription -- so deriving
// the order from it rather than writing it down here is what keeps this case from agreeing with
// itself.
func TestTheOpenersParametersArriveInTheOrderTheWrapKeyInfoWritesThem(t *testing.T) {
	_, sources := messagegroupProductionSources(t)
	info := wrapInfoWriteOrder(sources)
	door := messagegroupFuncParamOrder(sources, "OpenWrapBody")
	shared := []string{}
	for _, element := range info {
		if slices.Contains(door, element) {
			shared = append(shared, element)
		}
	}
	mirror := []string{}
	for _, parameter := range door {
		if slices.Contains(info, parameter) {
			mirror = append(mirror, parameter)
		}
	}
	t.Logf("wrap_key's info writes %v; OpenWrapBody takes %v; in common, the info writes %v and the door takes %v",
		info, door, shared, mirror)
	// THE ANTI-VACUITY CONTROL: an equality between two empty sequences is true of any order at
	// all, and two empty sequences are what a reading that had found neither function reports.
	if len(shared) == 0 {
		t.Fatalf("this reading found no element of wrap_key's info among the door's parameters -- info %v, door %v -- so the comparison below is between two empty sequences and holds of anything",
			info, door)
	}
	if !slices.Equal(shared, mirror) {
		t.Errorf("wrap_key's info writes %v and OpenWrapBody takes them %v. A call site is checked against MASTER section 7's line, and an order that is not that line is one a reader cannot check that way -- and it is this order that keeps u8(target_type) and u8(payload_type) apart, which is the only thing standing between two adjacent uint8 parameters and a transposition the compiler cannot see",
			shared, mirror)
	}
}

// Property: none of the three values the opener must state can arrive through something Go will
// fill in for a caller who did not.
//
// THIS IS THE FINDING ITSELF, HELD STRUCTURALLY. A struct passed by value has a zero value that is
// a complete value of the type, so a caller writing WrapExpectation{} -- or a partial literal
// naming one field -- stated nothing and the compiler supplied 0x00 for the two octets MG-7 says
// have no code point and 0 for the founding epoch. Measured before the repair: that call opened a
// genuine {0x00, 0x00, epoch 0} wrap and returned its payload byte for byte. Separate parameters
// have no such shape, because Go supplies no argument a caller did not write -- which is the only
// thing this case is about and is the only thing the door's paragraph claims.
//
// THE POSITIVE CONTROLS ARE IN THE SAME READING AND ARE TAKEN FROM THE SOURCE, not from memory.
// The two SEALING doors take a WrapEnvelope by value, deliberately -- it is the wire record being
// written rather than an authority being stated, and m1 task 14 property 9 requires in as many
// words that "the sealing side is reachable with an envelope the caller chooses", over all eleven
// octets including the one an opener may state no expectation over. So the reading that reports an
// empty set for the opener has to report those two by name, one exported and one not. An absence
// measured by a query that cannot produce a presence is not a measurement.
//
// AND THE SEALER'S ROW IS ASSERTED AND NOT ONLY PRINTED, in both directions: it is the written
// disposition that the two doors differ ON PURPOSE. A day that scalarises the sealer fails here
// and has to move property 9's seam with it or say why it need not.
func TestNoValueTheOpenerMustStateArrivesThroughAZeroValuableAggregate(t *testing.T) {
	_, sources := messagegroupProductionSources(t)
	for _, control := range []string{"SealWrapBody", "sealWrapBodyWith"} {
		carried := messagegroupZeroValuableStructParams(sources, control)
		if !maps.Equal(carried, map[string]string{"envelope": "WrapEnvelope"}) {
			t.Fatalf("the control reading answers %v for %s, and that door takes a WrapEnvelope by value for m1 task 14 property 9's reason. An EMPTY answer means this reading cannot see a struct parameter at all and the opener's empty set below would mean nothing; any OTHER answer means the sealing side's shape moved, which moves property 9's seam with it",
				carried, control)
		}
	}
	aggregates := messagegroupZeroValuableStructParams(sources, "OpenWrapBody")
	t.Logf("OpenWrapBody carries %v as a struct this package declares that a caller need not write out; the controls see WrapEnvelope on both sealing doors",
		aggregates)
	if len(aggregates) != 0 {
		t.Errorf("OpenWrapBody carries %v. A struct's zero value is a complete value of it and a variadic can be left off the call, so a caller can state that value by writing nothing -- which is how WrapExpectation{} opened a genuine {0x00, 0x00, epoch 0} wrap. The three under-determined values are parameters for that reason; see OpenWrapBody and open item MG-7",
			aggregates)
	}
}

// Property: each of the three values the opener states is compared, AT THE VALUES GO WOULD HAVE
// SUPPLIED -- and stating them is not the same as omitting them.
//
// WHY THE ZEROES ARE THE CASE WORTH WRITING. 0 is the founding epoch, and neither u8(target_type)
// nor u8(payload_type) has a code point in any document, so 0x00 is as plausible an assignment as
// any other -- MG-7 and MASTER section 7. A wrap whose envelope is {0x00, 0x00, epoch 0} is
// therefore a wrap this tree may really carry, and it is exactly the wrap a caller who stated
// nothing used to open. The repair does not forbid those values; it forbids reaching them by
// omission. So this case asserts BOTH halves: stated, they open the wrap they name, and each one
// moved off zero alone is refused by name.
func TestTheOpenerStatesTheThreeUnderDeterminedValuesRatherThanDefaultingThem(t *testing.T) {
	target := newWrapTestLeaf(t, 0x0B)
	envelope := WrapEnvelope{FormatVersion: WrapFormatVersion, TargetType: 0x00, PayloadType: 0x00, ContentEpoch: 0}
	payload := wrapTestPayload(0x66)
	body, err := SealWrapBody(rand.Reader, target.pub, envelope, wrapTestGroupId(), wrapTestTargetId(), payload)
	if err != nil {
		t.Fatalf("SealWrapBody: %v", err)
	}
	// STATED, it opens. An opener honouring the founding epoch and the two octets at 0x00 is a
	// legitimate opener and this door must not have made it unexpressible.
	got, gotPayload, err := OpenWrapBody(target.priv, wrapTestGroupId(), 0, 0x00, wrapTestTargetId(), 0x00, body)
	if err != nil {
		t.Fatalf("a wrap of {0x00, 0x00, epoch 0} does not open for an opener that states exactly that: %v", err)
	}
	if got != envelope || !bytes.Equal(gotPayload, payload) {
		t.Errorf("the wrap came back as %+v with %d octets of payload, and went in as %+v with %d",
			got, len(gotPayload), envelope, len(payload))
	}
	// AND EACH OF THE THREE MOVED OFF ZERO ALONE IS REFUSED, by name and with the field named in
	// the diagnostic -- which is what says all three are compared rather than one of them
	// happening to differ. Before the repair every one of these rows was reachable by a caller
	// who had written no expectation at all.
	for _, one := range []struct {
		name        string
		epoch       uint64
		targetType  uint8
		payloadType uint8
		names       string
	}{
		{name: "the content epoch", epoch: 1, names: "content epoch"},
		{name: "u8(target_type)", targetType: 0x01, names: "target_type"},
		{name: "u8(payload_type)", payloadType: 0x01, names: "payload_type"},
	} {
		_, refusedPayload, err := OpenWrapBody(target.priv, wrapTestGroupId(), one.epoch,
			one.targetType, wrapTestTargetId(), one.payloadType, body)
		if !errors.Is(err, ErrWrapEnvelopeMismatch) {
			t.Errorf("an opener whose %s alone is not the wrap's answered %v; want ErrWrapEnvelopeMismatch",
				one.name, err)
		}
		if !strings.Contains(fmt.Sprint(err), one.names) {
			t.Errorf("an opener whose %s alone is not the wrap's was refused with %q, which does not name %s; a diagnostic that names another field is a comparison reading another field",
				one.name, err, one.names)
		}
		if refusedPayload != nil {
			t.Errorf("an opener whose %s alone is not the wrap's was handed %d octets of payload",
				one.name, len(refusedPayload))
		}
	}
}

// messagegroupStructFields answers one named struct type's exported and unexported fields, as
// name -> type, out of this package's production source.
func messagegroupStructFields(sources []messagegroupSource, name string) map[string]string {
	fields := map[string]string{}
	for _, source := range sources {
		for _, declaration := range source.parsed.Decls {
			general, isGeneral := declaration.(*ast.GenDecl)
			if !isGeneral {
				continue
			}
			for _, spec := range general.Specs {
				typed, isTyped := spec.(*ast.TypeSpec)
				if !isTyped || typed.Name.Name != name {
					continue
				}
				structure, isStruct := typed.Type.(*ast.StructType)
				if !isStruct || structure.Fields == nil {
					continue
				}
				for _, field := range structure.Fields.List {
					for _, named := range field.Names {
						fields[named.Name] = typeExprName(field.Type)
					}
				}
			}
		}
	}
	return fields
}

// messagegroupFuncParams answers one named function's parameters as name -> type, out of this
// package's production source, with the type written the way the source writes it.
//
// It renders through go/types.ExprString and NOT through typeExprName above, which collapses
// []byte to byte and *XwingPrivateKey to XwingPrivateKey. Those two collapses are harmless where
// that helper is used and are exactly wrong here: the reading beside this one has to tell a value
// of a package type from a pointer to one.
func messagegroupFuncParams(sources []messagegroupSource, name string) map[string]string {
	params := map[string]string{}
	for _, source := range sources {
		for _, declaration := range source.parsed.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Name.Name != name || function.Type.Params == nil {
				continue
			}
			for _, field := range function.Type.Params.List {
				for _, named := range field.Names {
					params[named.Name] = types.ExprString(field.Type)
				}
			}
		}
	}
	return params
}

// messagegroupZeroValuableStructParams answers, as parameter name -> the type as the source writes
// it, the parameters of one function through which a caller can supply a value of a STRUCT this
// package declares WITHOUT WRITING ONE OUT.
//
// TWO SHAPES AND NOT ONE, and the second is why this reading is written as a walk over the type
// expression rather than as a string test. A struct taken BY VALUE has a zero value that is a
// complete value of it, so an empty or partial composite literal states nothing and compiles. A
// VARIADIC of one is weaker still: the argument can be left off the call altogether. A reading
// that looked only for a bare identifier would report a clean set for
// `want ...WrapExpectation` -- measured, as a mutant that survived this gate's first draft.
//
// WHAT IT DELIBERATELY DOES NOT FLAG, with the reason, because an over-broad gate is one a later
// commit works around. A POINTER to a struct: its zero value is nil, which is a distinguishable
// sentinel this door refuses by name rather than a statement it cannot tell from a real one. A
// SLICE, for the same reason. A DEFINED SCALAR -- type WrapEpoch uint64 -- which has no fields, so
// there is no literal that omits any of them and a caller still writes the value out. And a
// qualified type from another package, which this package did not declare and cannot judge.
func messagegroupZeroValuableStructParams(sources []messagegroupSource, name string) map[string]string {
	found := map[string]string{}
	for _, source := range sources {
		for _, declaration := range source.parsed.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Name.Name != name || function.Type.Params == nil {
				continue
			}
			for _, field := range function.Type.Params.List {
				carried := field.Type
				if variadic, isVariadic := carried.(*ast.Ellipsis); isVariadic {
					carried = variadic.Elt
				}
				named, isNamed := carried.(*ast.Ident)
				if !isNamed || len(messagegroupStructFields(sources, named.Name)) == 0 {
					continue
				}
				for _, parameter := range field.Names {
					found[parameter.Name] = types.ExprString(field.Type)
				}
			}
		}
	}
	return found
}

// messagegroupFuncParamOrder answers one named function's parameter names IN SOURCE ORDER, which
// is what messagegroupFuncParams' map cannot carry.
func messagegroupFuncParamOrder(sources []messagegroupSource, name string) []string {
	order := []string{}
	for _, source := range sources {
		for _, declaration := range source.parsed.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Name.Name != name || function.Type.Params == nil {
				continue
			}
			for _, field := range function.Type.Params.List {
				for _, parameter := range field.Names {
					order = append(order, parameter.Name)
				}
			}
		}
	}
	return order
}

// wrapInfoWriteOrder answers the names WrapInfo writes into MASTER section 7's info, in the order
// it writes them, read off its body.
//
// It reads the ENCODER and not a list, because the encoder is the thing the known answers hold to
// MASTER bytewise: testdata/envelope-wrap-kat.txt's H(info) is reproduced by a second
// transcription written from MASTER's block and sharing no code with WrapInfo, so an order that
// drifted here fails there first and in octets. A field written as envelope.TargetType is carried
// across as targetType, which is the one difference between a struct field and the parameter that
// states it. The raw label write has no name and is not an element.
func wrapInfoWriteOrder(sources []messagegroupSource) []string {
	written := []string{}
	for _, source := range sources {
		for _, declaration := range source.parsed.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Name.Name != "WrapInfo" || function.Body == nil {
				continue
			}
			for _, statement := range function.Body.List {
				expression, isExpression := statement.(*ast.ExprStmt)
				if !isExpression {
					continue
				}
				call, isCall := expression.X.(*ast.CallExpr)
				if !isCall || len(call.Args) != 1 {
					continue
				}
				if _, isWrite := call.Fun.(*ast.SelectorExpr); !isWrite {
					continue
				}
				switch argument := call.Args[0].(type) {
				case *ast.Ident:
					written = append(written, argument.Name)
				case *ast.SelectorExpr:
					written = append(written, wrapLowerFirst(argument.Sel.Name))
				}
			}
		}
	}
	return written
}

// wrapLowerFirst and wrapUpperFirst carry a name across the one difference between an exported
// struct field and the parameter that states it. They are a case change and nothing else: a
// mapping table of field to parameter would pass over a parameter renamed to something that means
// another field.
func wrapLowerFirst(name string) string {
	if name == "" {
		return name
	}
	return strings.ToLower(name[:1]) + name[1:]
}

func wrapUpperFirst(name string) string {
	if name == "" {
		return name
	}
	return strings.ToUpper(name[:1]) + name[1:]
}

// ---------------------------------------------------------------------------
// two records per target
// ---------------------------------------------------------------------------

// Property: the device wrap is TWO records at one target, and the two are separable by the one
// element of wrap_key's info that separates them.
//
// It was ONE record carrying both secrets until 2026-09-13, which is the shape every existing
// draft and the sizing arithmetic in three documents described, so it is the mistake a reader
// arrives holding. The split is what makes MASTER section 8.1's disappearing-message promise
// cryptographic: pq_secret rides a PERMANENT record and eph_root an EPH(5) one.
func TestTheDeviceWrapIsTwoBodiesAndTheirPayloadTypesMustDiffer(t *testing.T) {
	target := newWrapTestLeaf(t, 0x07)
	pqPayload := wrapTestPayload(0xAA)
	ephPayload := wrapTestPayload(0xBB)
	pqBody, ephBody, err := SealDeviceWraps(rand.Reader, target.pub, 12, 0x01,
		wrapTestGroupId(), wrapTestTargetId(), 0x01, pqPayload, 0x02, ephPayload)
	if err != nil {
		t.Fatalf("SealDeviceWraps: %v", err)
	}
	if bytes.Equal(pqBody, ephBody) {
		t.Fatal("the two device wraps of one leaf are the same octets")
	}
	for _, one := range []struct {
		name string
		body []byte
		want []byte
		kind uint8
	}{
		{name: "the pq_secret wrap", body: pqBody, want: pqPayload, kind: 0x01},
		{name: "the eph_root wrap", body: ephBody, want: ephPayload, kind: 0x02},
	} {
		envelope, payload, err := OpenWrapBody(target.priv, wrapTestGroupId(), 12,
			0x01, wrapTestTargetId(), one.kind, one.body)
		if err != nil {
			t.Fatalf("%s did not open: %v", one.name, err)
		}
		if envelope.PayloadType != one.kind {
			t.Errorf("%s carries payload_type %#02x, want %#02x", one.name, envelope.PayloadType, one.kind)
		}
		if envelope.ContentEpoch != 12 {
			t.Errorf("%s carries content epoch %d, want 12", one.name, envelope.ContentEpoch)
		}
		// THE ASSERTION A ROUND TRIP OVER ONE RECORD CANNOT MAKE: the two secrets are not
		// interchanged
		if !bytes.Equal(payload, one.want) {
			t.Errorf("%s delivered the other record's payload", one.name)
		}
	}
	// AND THE HALF A ROUND TRIP OVER EITHER BODY CANNOT SEE: the two are not interchangeable AT
	// THE DOOR. Both bodies are genuine, both are sealed to this leaf at this epoch, and both land
	// at ONE wrap_target_handle -- so before OpenWrapBody took an expectation, each of them opened
	// under the other's arguments and the payload_type was carried out to a caller who was under
	// no obligation to look at it. An opener honouring one kind must be refused the other's.
	for _, cross := range []struct {
		name string
		body []byte
		kind uint8
	}{
		{name: "the pq_secret wrap", body: pqBody, kind: 0x02},
		{name: "the eph_root wrap", body: ephBody, kind: 0x01},
	} {
		_, payload, err := OpenWrapBody(target.priv, wrapTestGroupId(), 12,
			0x01, wrapTestTargetId(), cross.kind, cross.body)
		if !errors.Is(err, ErrWrapEnvelopeMismatch) {
			t.Errorf("%s opened for an opener honouring payload_type %#02x and answered %v; want ErrWrapEnvelopeMismatch",
				cross.name, cross.kind, err)
		}
		if payload != nil {
			t.Errorf("%s handed %d octets of payload to an opener honouring the other kind", cross.name, len(payload))
		}
	}
	// and one payload_type used twice is refused by name, because it is the only element of
	// wrap_key's nine that separates two records landing at one wrap_target_handle
	if _, _, err := SealDeviceWraps(rand.Reader, target.pub, 12, 0x01,
		wrapTestGroupId(), wrapTestTargetId(), 0x01, pqPayload, 0x01, ephPayload); !errors.Is(err, ErrWrapPayloadTypeCollision) {
		t.Errorf("two device wraps under one payload_type answered %v; want ErrWrapPayloadTypeCollision", err)
	}
}

// Property: the wrap body's occupancy is the one MASTER section 8.2 publishes.
//
// 1,289 octets of wrap_body, 1,293 of the 4,096 rung once the record layer's LP32 body prefix is
// on it, and a 2,803-octet zero tail. Those numbers are a function of every length prefix in the
// grammar and of nothing else, so an implementation that reproduces them has agreed about all of
// them -- and one that does not has a framing disagreement no round trip against itself can see.
func TestTheWrapBodyOccupiesWhatMasterSectionEightTwoPublishes(t *testing.T) {
	target := newWrapTestLeaf(t, 0x09)
	body, err := SealWrapBody(rand.Reader, target.pub,
		WrapEnvelope{FormatVersion: WrapFormatVersion, TargetType: 0x01, PayloadType: 0x01, ContentEpoch: 1},
		wrapTestGroupId(), wrapTestTargetId(), wrapTestPayload(0x11))
	if err != nil {
		t.Fatalf("SealWrapBody: %v", err)
	}
	const fromMaster = 1289
	if len(body) != fromMaster {
		t.Errorf("a device wrap body is %d octets and MASTER section 8.2 publishes %d", len(body), fromMaster)
	}
	bucket, err := bucketForBody(len(body))
	if err != nil {
		t.Fatalf("bucketForBody: %v", err)
	}
	padded, err := padBody(bucket, body)
	if err != nil {
		t.Fatalf("padBody: %v", err)
	}
	occupancy := len(body) + lpPrefixBytes
	if occupancy != 1293 {
		t.Errorf("a device wrap occupies %d of its rung and MASTER section 8.2 publishes 1293", occupancy)
	}
	if len(padded) != 4096 {
		t.Errorf("the rung is %d octets and MASTER section 8.2's device wrap rung is 4096", len(padded))
	}
	if tail := len(padded) - occupancy; tail != 2803 {
		t.Errorf("the zero tail is %d octets and MASTER section 8.2 publishes 2803", tail)
	}
	// and the tail really is zeros, which is what the refusal task 14 property 10 owes will be
	// written over
	for i := occupancy; i < len(padded); i += 1 {
		if padded[i] != 0 {
			t.Fatalf("the pad octet at %d is %#02x and MASTER section 8.2's fill is zero", i, padded[i])
		}
	}
}

// ---------------------------------------------------------------------------
// what the door reaches, and what it erases
// ---------------------------------------------------------------------------

// Property 6's structural half: the device wrap's outer key is reached from the EXPORTER and from
// nothing that descends from a storage root.
//
// THE BEHAVIOURAL HALF CANNOT CARRY THIS ALONE and that is why the structural one exists. A wrap
// sealed under DeriveClassKeys(storage_root[k]) round trips perfectly between a sealer and an
// opener that both do it, and what it costs is not visible in any octet: storage_root[k] is the
// value the wrap exists to DELIVER, so the record would be openable only by a member that already
// had what was inside it, and a member removed by the commit that opened epoch k would keep its
// contribution to the key. That is the circularity MASTER section 8.2's ruling exists to remove,
// and an edge from this file into the key schedule's roots is exactly how it comes back.
func TestTheWrapDoorReachesTheExporterAndNoStorageRoot(t *testing.T) {
	_, sources := messagegroupProductionSources(t)
	var wrapSource *messagegroupSource
	for i := range sources {
		if strings.HasSuffix(sources[i].path, "wrap.go") {
			wrapSource = &sources[i]
		}
	}
	if wrapSource == nil {
		t.Fatal("this package has no wrap.go, so this gate read nothing")
	}
	reached := map[string]bool{}
	declared := []string{}
	for _, declaration := range wrapSource.parsed.Decls {
		function, isFunction := declaration.(*ast.FuncDecl)
		if !isFunction || function.Body == nil {
			continue
		}
		declared = append(declared, function.Name.Name)
		for _, callee := range keyScheduleCalleeNames(function.Body) {
			reached[callee] = true
		}
	}
	if len(declared) == 0 {
		t.Fatal("wrap.go declares no function with a body, so this gate judged an empty class")
	}
	// THE POSITIVE CONTROLS, in the same reading and for its own reason: this door certainly
	// reaches the ladder head and the exporter, so a reading that had stopped resolving callee
	// names would report the clean run a complete one reports.
	for _, wanted := range []string{"RecordKeyZero", "Export", "PendingExport", "XwingEncapsulate", "XwingDecapsulate"} {
		if !reached[wanted] {
			t.Fatalf("wrap.go's reading does not reach %s, so it is not resolving what it claims to: %v",
				wanted, slices.Sorted(maps2Keys(reachedNames(reached))))
		}
	}
	for _, forbidden := range []string{"StorageRoot", "DeriveClassKeys", "classKeyOnLoop", "classKeyOf"} {
		if reached[forbidden] {
			t.Errorf("wrap.go reaches %s; MASTER section 8.2's ruling puts env_key[k] at the head of this ladder precisely so that the device wrap is not sealed under a key descending from the root it delivers",
				forbidden)
		}
	}
}

func reachedNames(reached map[string]bool) map[string]string {
	out := map[string]string{}
	for name := range reached {
		out[name] = ""
	}
	return out
}

// Property: every value the door derives that is key material is handed to the eraser in the body
// that derived it.
//
// IT IS A SOURCE READING BECAUSE NO BEHAVIOURAL ONE EXISTS. The shared secret, the extraction's
// prk and the wrap key and nonce are locals that die with the call; nothing outside the function
// holds a header over them, so no test can look at them afterwards and no round trip changes if
// the erase is deleted. What the reading holds is the obligation: a local assigned out of the KEM
// or out of the KDF is ERASED in the body that derived it, or MOVED OUT of it.
//
// THE "MOVED OUT" HALF IS NOT A LOOPHOLE AND IT IS THE SAME ONE connect/mls's DROP SITE READING
// CARRIES. A body that hands its derivation to its caller has not dropped it; the obligation
// travels with the value, and the caller is then a member of this class in its own right.
// wrapKeyMaterial is exactly that shape -- it expands fifty six octets and returns the two halves
// of them -- and a gate that demanded an erase there would be demanding that a derivation blank
// the key it just answered, which is recordAeadMaterial's shape one file over.
//
// THE CLASS IS THE PACKAGE AND THE PRODUCERS ARE DERIVED, and both halves of that sentence are
// repairs of a measured hole rather than decoration. The version this replaces read ONE FILE --
// os.ReadFile("wrap.go") -- against a switch naming FIVE producers, each matched only when the
// callee was spelled as a bare identifier. xwing.go, the file that MAKES the shared secret this
// door erases, was therefore outside it twice over: the file was never opened, and every producer
// in it is a selector call (sha3.SumSHAKE256, mls.X25519DH, priv.mlkemPrivate.Decapsulate). It
// carried zero erases against six live-at-return secrets while this gate ran green, so the door
// was erasing one copy of secrets its own producer had already left lying about. Naming xwing.go
// beside wrap.go would have closed those six instances and left the next file in the same place,
// which is why the subject is now every production source of this package and the producers are
// read off what the package ITSELF erases.
//
// HOW A PRODUCER IS DERIVED, in two clauses and no list:
//
//  1. THE SEED, and it is the package's own erases read backwards. A callee is a producer at
//     result position i when some body of this package binds a local out of position i and hands
//     that local to zeroize. That is what tells XwingEncapsulate's SHARED SECRET at position 1
//     from its CIPHERTEXT at position 0 without anybody writing the positions down, and it reads
//     a selector callee by its trailing name, which is the convention zeroize_test.go's own
//     hand-off reading already keeps;
//  2. THE CLOSURE, because the obligation travels with the value. A declaration that binds a
//     local out of a producer and MOVES IT OUT at its own result position j is a producer at j,
//     to a fixed point -- which is how wrapKeyMaterial and recordAeadMaterial enter without a
//     row, and it is the same clause the "moved out" half of the obligation rests on.
//
// A BUILTIN IS NOT A PRODUCER, and that narrowing is asserted below rather than assumed: read
// without it, ratchet.go's `walking := append(...)` followed by `zeroize(walking)` makes append a
// producer and every assembled buffer in the package -- an encoded public key, a handle key, a
// replacement slice -- becomes an unerased derivation. The complement that narrowing removes is
// written down in wrapBuiltinsThatAreNotProducers and checked in both directions.
//
// WHAT THIS GATE STILL CANNOT SEE, stated because the next reader will need it: a producer NOBODY
// erases anywhere never enters the class, so this reading could not have caught xwing.go before
// the erases existed. That floor is a different mechanism and is held by
// TestEveryPrimitiveResultThisPackageBindsHasAWrittenDisposition in primitiveerase_test.go, which
// asks the opposite question -- what did the primitives produce -- and answers it from a written
// table rather than from the package's own habits.
func TestEveryKeyThisPackageDerivesIsErasedInTheBodyThatDerivedIt(t *testing.T) {
	// CONTROL ONE, over the DERIVATION: a package that seeds a producer, closes it over a
	// move-out, and offers a builtin and a non-producer callee to be left alone.
	derivationControl := "package control\n" +
		"func seeds() {\n\tc, y, _ := Encapsulate(r, p)\n\tzeroize(y)\n\t_ = c\n}\n" +
		"func closes() []byte {\n\t_, y, _ := Encapsulate(r, p)\n\treturn y\n}\n" +
		"func appends() {\n\tbuffer := append(a, b...)\n\tzeroize(buffer)\n}\n" +
		"func plain() {\n\tx := unrelated()\n\t_ = x\n}\n"
	derived := wrapProducerClassIn(t, "the producer control", derivationControl)
	wantDerived := []string{"Encapsulate@1", "closes@0"}
	if !slices.Equal(wrapProducerNames(derived), wantDerived) {
		t.Fatalf("the producer reading derived %v out of the control, want %v; it is not reading the seed off the package's own erase, not closing it over a move-out, or not holding the builtin narrowing",
			wrapProducerNames(derived), wantDerived)
	}
	// and the narrowing's complement, ASSERTED: without the builtin clause the control's append
	// would be a producer, and that is the whole of what the clause removes here.
	if removed := wrapBuiltinProducersIn(t, "the producer control", derivationControl); !slices.Equal(removed, []string{"append@0"}) {
		t.Fatalf("the builtin narrowing removed %v from the control's producer class, want [append@0]; a narrowing whose complement is empty is a clause that is not doing anything, and one whose complement is larger than this is removing coverage nobody wrote down",
			removed)
	}

	// CONTROL TWO, over the OBLIGATION, against a fixed producer set so the two halves fail
	// apart: erased, dropped, half erased, moved out, handed to a callee, installed in a value
	// this body builds, and installed in storage it was handed.
	fixed := map[wrapKeyProducer]string{
		{callee: "XwingEncapsulate", position: 1}:  "the control's KEM",
		{callee: "wrapKeyMaterial", position: 0}:   "the control's KDF, key half",
		{callee: "wrapKeyMaterial", position: 1}:   "the control's KDF, nonce half",
		{callee: "keyScheduleExpand", position: 0}: "the control's expansion",
	}
	const controlName = "the wrap erase control"
	control := []struct {
		name     string
		source   string
		wantHeld []string
	}{
		{name: "erased", source: "func probe() {\nx, y, _ := XwingEncapsulate(r, p)\nzeroize(y)\n_ = x\n}", wantHeld: nil},
		{name: "dropped", source: "func probe() {\nx, y, _ := XwingEncapsulate(r, p)\n_ = x\n_ = y\n}", wantHeld: []string{"y"}},
		{name: "half erased", source: "func probe() {\nk, n := wrapKeyMaterial(s, i)\nzeroize(k)\n_ = n\n}", wantHeld: []string{"n"}},
		{name: "moved out", source: "func probe() []byte {\nm := keyScheduleExpand(p, i, 56)\nreturn m[:32]\n}", wantHeld: nil},
		{name: "moved out whole", source: "func probe() ([]byte, []byte) {\nk, n := wrapKeyMaterial(s, i)\nreturn k, n\n}", wantHeld: nil},
		// THE ROW THE FIRST VERSION OF THIS READING GOT WRONG, kept as a control rather than
		// only fixed: a body that hands its derivation to a CALLEE inside its own return
		// statement has not moved it out, because the callee does not own it and the local is
		// still there to erase when the call comes back.
		{name: "handed to a callee", source: "func probe() ([]byte, error) {\nc, y, _ := XwingEncapsulate(r, p)\nreturn helper(c, y), nil\n}", wantHeld: []string{"y"}},
		// THE TWO SHAPES THE WIDENING ADDED, and they arrived with it because the package outside
		// wrap.go is full of them: NewSenderRatchet hands its walked record key to the ratchet it
		// is building, and installEpochOnLoop reads a storage root into the session's own field.
		// Both are the move-out clause read through a structure rather than through a return, and
		// a gate that called either a drop would be demanding that a constructor blank the key it
		// just installed.
		{name: "installed in a value this body builds", source: "func probe() *R {\nk, n := wrapKeyMaterial(s, i)\nzeroize(n)\nreturn &R{key: k}\n}", wantHeld: nil},
		{name: "installed in storage it was handed", source: "func (self *S) probe() {\nk, n := wrapKeyMaterial(s, i)\nzeroize(n)\nself.key = k\n}", wantHeld: nil},
	}
	for _, one := range control {
		held := wrapUnerasedDerivations(t, fixed, controlName, "package control\n"+one.source+"\n")
		if !slices.Equal(held, one.wantHeld) {
			t.Fatalf("the control %q reads as holding %v, want %v; the matcher is not separating an erased derivation from a dropped one, nor either from one moved out to the caller or installed in storage that outlives the frame",
				one.name, held, one.wantHeld)
		}
	}

	// and now the real source: every production file of this package, against the class this
	// package's own erases derive.
	_, sources := messagegroupProductionSources(t)
	producers := map[wrapKeyProducer]string{}
	builtinsStruck := map[string]bool{}
	for _, source := range sources {
		raw, err := os.ReadFile(source.path)
		if err != nil {
			t.Fatalf("read %s: %v", source.path, err)
		}
		for key, why := range wrapProducerClassIn(t, source.path, string(raw)) {
			if _, seen := producers[key]; !seen {
				producers[key] = why
			}
		}
		for _, struck := range wrapBuiltinProducersIn(t, source.path, string(raw)) {
			builtinsStruck[struck[:strings.Index(struck, "@")]] = true
		}
	}
	// THE NARROWING'S COMPLEMENT OVER THE REAL SOURCE, asserted in both directions against the
	// written table. A builtin this package starts erasing and nobody excused is a producer
	// silently struck from the class; a row that no longer strikes anything is a sentence
	// excusing nothing and hiding that it does.
	if struck := slices.Sorted(maps.Keys(builtinsStruck)); !slices.Equal(struck, slices.Sorted(maps2Keys(wrapBuiltinsThatAreNotProducers))) {
		t.Errorf("the builtin narrowing strikes %v from this package's producer class and wrapBuiltinsThatAreNotProducers excuses %v; the two must agree, because a builtin struck without a row is coverage removed by nobody and a row that strikes nothing is a sentence with no measurement under it",
			struck, slices.Sorted(maps2Keys(wrapBuiltinsThatAreNotProducers)))
	}
	// the positive controls on the real source, in the same query as the zero below: the class
	// reaches BOTH doors of the KEM and the wrap KDF, and a reading that had stopped reaching
	// them would report the same clean run a complete one reports.
	for _, wanted := range []wrapKeyProducer{
		{callee: "XwingEncapsulate", position: 1},
		{callee: "XwingDecapsulate", position: 0},
		{callee: "wrapKeyMaterial", position: 0},
		{callee: "wrapKeyMaterial", position: 1},
		{callee: "keyScheduleExtract", position: 0},
		{callee: "X25519DH", position: 0},
		{callee: "SumSHAKE256", position: 0},
	} {
		if _, isProducer := producers[wanted]; !isProducer {
			t.Fatalf("this package's source does not derive %s@%d as a key material producer, so the reading below cleared every body that binds one: %v",
				wanted.callee, wanted.position, wrapProducerNames(producers))
		}
	}
	t.Logf("%d key material producer position(s) derived: %v", len(producers), wrapProducerNames(producers))
	for _, source := range sources {
		raw, err := os.ReadFile(source.path)
		if err != nil {
			t.Fatalf("read %s: %v", source.path, err)
		}
		if held := wrapUnerasedDerivations(t, producers, source.path, string(raw)); len(held) != 0 {
			t.Errorf("%s derives %v out of a key material producer and neither hands them to zeroize in the same body, moves them out, nor installs them in storage that outlives the frame; a producer that leaves its own copy live makes every erase downstream of it an erase of one copy",
				source.path, held)
		}
	}
}

// wrapKeyProducer is one result position of one callee that this package's own source shows
// produces key material. The callee is its trailing name, so mls.X25519DH and a method
// Decapsulate are each one entry -- the same widening zeroize_test.go's hand-off reading takes,
// and for the same reason: reading a callee by its bare name can only put MORE call sites under
// the obligation, which is the direction a gate may be wrong in.
type wrapKeyProducer struct {
	callee   string
	position int
}

// The builtins a derivation may be bound from that are NOT key material producers however often
// the result is erased afterwards.
//
// One row, and it is load bearing. ratchet.go walks a ladder with `walking := append(...)` and
// erases each rung as it passes; read without this clause, append becomes a producer at position
// zero and every assembled buffer in the package -- xwing.go's encoded public key, session.go's
// replacement slices, engine.go's by-value proposal lists -- reads as an unerased derivation. The
// table is checked in both directions against the builtins the seed actually offers, so a row
// that stopped excusing anything is reported and a builtin that starts being erased needs one.
var wrapBuiltinsThatAreNotProducers = map[string]string{
	"append": "how this package ASSEMBLES octets rather than how it derives them. A body that " +
		"appends key material into a buffer and then erases the buffer is erasing its own scratch, " +
		"and the octets it appended are held to this obligation where they were derived, which is " +
		"upstream of the append. Treating it as a producer puts every encoding in the package -- " +
		"public keys, handle keys, wire bodies -- under an erase obligation nothing could meet",
	"make": "an ALLOCATION and not a derivation: what decides whether the octets are key material " +
		"is what fills the buffer afterwards, which this reading cannot see and does not claim to. " +
		"XwingGenerateKey's `seed := make(...)` followed by io.ReadFull and an erase is the one site " +
		"that offers it today, and the obligation there is held by a different mechanism -- " +
		"TestEveryBufferThisPackageFillsFromEntropyIsErasedOrMovedOut, which reads the FILL rather " +
		"than the allocation. Admitting make here would put every scratch buffer in the package " +
		"under this gate and would still not reach a buffer somebody allocated with a literal",
}

func wrapProducerNames(producers map[wrapKeyProducer]string) []string {
	names := []string{}
	for key := range producers {
		names = append(names, fmt.Sprintf("%s@%d", key.callee, key.position))
	}
	slices.Sort(names)
	return names
}

// wrapMovedOutName answers the identifier a returned expression hands to the caller, or "" when
// the expression is not one.
//
// A bare name, a slice of one and an index of one are all the same value travelling out. A CALL
// is not: its arguments belong to the body that made the call, and a body that passes its shared
// secret to a helper has not stopped owning it.
func wrapMovedOutName(expr ast.Expr) string {
	switch typed := expr.(type) {
	case *ast.Ident:
		return typed.Name
	case *ast.ParenExpr:
		return wrapMovedOutName(typed.X)
	case *ast.SliceExpr:
		return wrapMovedOutName(typed.X)
	case *ast.IndexExpr:
		return wrapMovedOutName(typed.X)
	case *ast.StarExpr:
		return wrapMovedOutName(typed.X)
	case *ast.UnaryExpr:
		return wrapMovedOutName(typed.X)
	}
	return ""
}

// mustParseMessagegroupSource parses one Go source text, for the readings that judge a control
// package and the real file through the same matcher.
func mustParseMessagegroupSource(t *testing.T, name string, source string) *ast.File {
	t.Helper()
	parsed, err := parser.ParseFile(token.NewFileSet(), name, source, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return parsed
}

// wrapCalleeName answers the trailing name of a callee: `zeroize`, `SumSHAKE256` for
// sha3.SumSHAKE256, `Decapsulate` for priv.mlkemPrivate.Decapsulate.
func wrapCalleeName(expr ast.Expr) string {
	switch typed := expr.(type) {
	case *ast.Ident:
		return typed.Name
	case *ast.SelectorExpr:
		return typed.Sel.Name
	case *ast.ParenExpr:
		return wrapCalleeName(typed.X)
	}
	return ""
}

// wrapDerivationBinding is one local this body bound out of one result position of one call.
type wrapDerivationBinding struct {
	name     string
	callee   string
	position int
}

func wrapDerivationBindingsIn(body *ast.BlockStmt) []wrapDerivationBinding {
	bindings := []wrapDerivationBinding{}
	ast.Inspect(body, func(node ast.Node) bool {
		assign, isAssign := node.(*ast.AssignStmt)
		if !isAssign || len(assign.Rhs) != 1 {
			return true
		}
		call, isCall := assign.Rhs[0].(*ast.CallExpr)
		if !isCall {
			return true
		}
		callee := wrapCalleeName(call.Fun)
		if callee == "" {
			return true
		}
		for position, left := range assign.Lhs {
			name, isName := left.(*ast.Ident)
			if !isName || name.Name == "_" {
				continue
			}
			bindings = append(bindings, wrapDerivationBinding{name.Name, callee, position})
		}
		return true
	})
	return bindings
}

// The names this body hands to the eraser, by bare name, which is the only shape a derivation is
// ever erased in.
func wrapErasedNamesIn(body *ast.BlockStmt) map[string]bool {
	erased := map[string]bool{}
	ast.Inspect(body, func(node ast.Node) bool {
		call, isCall := node.(*ast.CallExpr)
		if !isCall {
			return true
		}
		callee, isName := call.Fun.(*ast.Ident)
		if !isName || callee.Name != "zeroize" || len(call.Args) != 1 {
			return true
		}
		if argument, isArgument := call.Args[0].(*ast.Ident); isArgument {
			erased[argument.Name] = true
		}
		return true
	})
	return erased
}

// Every name this body MOVES OUT to its caller, and the result positions it leaves at. A slice or
// an index of a derivation counts as moved as well as the whole of it.
//
// IT DOES NOT DESCEND INTO A CALL'S ARGUMENTS, and that clause is here because the first version
// of this reading did. `return sealWrapBodyWith(..., shared, ...)` is not a body moving its shared
// secret out to its caller -- it is a body HANDING it to a callee that does not own it, and the
// local is still this body's to erase when the callee returns. Measured: with the descent in,
// deleting SealWrapBody's `defer zeroize(shared)` left this gate and every other case in the
// package green, which is the whole mutation this case exists to kill.
func wrapMovedOutIn(body *ast.BlockStmt) map[string][]int {
	moved := map[string][]int{}
	ast.Inspect(body, func(node ast.Node) bool {
		returned, isReturn := node.(*ast.ReturnStmt)
		if !isReturn {
			return true
		}
		for position, result := range returned.Results {
			if named := wrapMovedOutName(result); named != "" {
				moved[named] = append(moved[named], position)
			}
		}
		return true
	})
	return moved
}

// Every name this body reads INTO storage that outlives the frame: a field or an index of
// something, or an element of a value the body is BUILDING.
//
// It is the move-out clause read through a structure rather than through a return, and the
// package outside wrap.go is full of both shapes -- NewSenderRatchet hands its walked record key
// to the ratchet it constructs, installEpochOnLoop reads a storage root into the session's own
// field. A reading that called either a drop would be demanding that a constructor blank the key
// it just installed, which is the same demand connect/mls's drop reading refuses to make of a
// merge.
func wrapInstalledNamesIn(body *ast.BlockStmt) map[string]bool {
	installed := map[string]bool{}
	ast.Inspect(body, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.AssignStmt:
			for position, left := range typed.Lhs {
				switch left.(type) {
				case *ast.SelectorExpr, *ast.IndexExpr:
				default:
					continue
				}
				if position < len(typed.Rhs) {
					if name := wrapMovedOutName(typed.Rhs[position]); name != "" {
						installed[name] = true
					}
				}
			}
		case *ast.CompositeLit:
			for _, element := range typed.Elts {
				value := element
				if keyed, isKeyed := element.(*ast.KeyValueExpr); isKeyed {
					value = keyed.Value
				}
				if name := wrapMovedOutName(value); name != "" {
					installed[name] = true
				}
			}
		}
		return true
	})
	return installed
}

// The builtins the language supplies, so a derivation bound from one is told from a derivation
// bound from a function this tree wrote.
var wrapLanguageBuiltins = []string{
	"append", "cap", "clear", "close", "complex", "copy", "delete", "imag", "len",
	"make", "max", "min", "new", "panic", "print", "println", "real", "recover",
}

// wrapProducerClassIn derives, off ONE source text, every callee result position that this source
// shows produces key material: seeded on the erases it spells and closed over the move-outs, with
// the builtins struck.
func wrapProducerClassIn(t *testing.T, name string, source string) map[wrapKeyProducer]string {
	t.Helper()
	parsed := mustParseMessagegroupSource(t, name, source)
	producers := map[wrapKeyProducer]string{}
	bodies := []*ast.FuncDecl{}
	for _, declaration := range parsed.Decls {
		function, isFunction := declaration.(*ast.FuncDecl)
		if !isFunction || function.Body == nil {
			continue
		}
		bodies = append(bodies, function)
	}
	admit := func(key wrapKeyProducer, why string) bool {
		if slices.Contains(wrapLanguageBuiltins, key.callee) {
			return false
		}
		if _, seen := producers[key]; seen {
			return false
		}
		producers[key] = why
		return true
	}
	for _, function := range bodies {
		erased := wrapErasedNamesIn(function.Body)
		for _, binding := range wrapDerivationBindingsIn(function.Body) {
			if !erased[binding.name] {
				continue
			}
			admit(wrapKeyProducer{binding.callee, binding.position},
				name+": "+function.Name.Name+" erases "+binding.name)
		}
	}
	for grew := true; grew; {
		grew = false
		for _, function := range bodies {
			moved := wrapMovedOutIn(function.Body)
			for _, binding := range wrapDerivationBindingsIn(function.Body) {
				if _, isProducer := producers[wrapKeyProducer{binding.callee, binding.position}]; !isProducer {
					continue
				}
				for _, at := range moved[binding.name] {
					if admit(wrapKeyProducer{function.Name.Name, at},
						name+": "+function.Name.Name+" moves "+binding.name+" out") {
						grew = true
					}
				}
			}
		}
	}
	return producers
}

// wrapBuiltinProducersIn is the COMPLEMENT of the builtin narrowing: the producer positions the
// seed would have admitted if a builtin were a producer, and does not.
//
// It exists so the narrowing can be asserted rather than trusted. An empty answer over a source
// that erases an appended buffer is the tell that the clause has stopped doing anything.
func wrapBuiltinProducersIn(t *testing.T, name string, source string) []string {
	t.Helper()
	parsed := mustParseMessagegroupSource(t, name, source)
	removed := map[string]bool{}
	for _, declaration := range parsed.Decls {
		function, isFunction := declaration.(*ast.FuncDecl)
		if !isFunction || function.Body == nil {
			continue
		}
		erased := wrapErasedNamesIn(function.Body)
		for _, binding := range wrapDerivationBindingsIn(function.Body) {
			if !erased[binding.name] || !slices.Contains(wrapLanguageBuiltins, binding.callee) {
				continue
			}
			removed[fmt.Sprintf("%s@%d", binding.callee, binding.position)] = true
		}
	}
	return slices.Sorted(maps.Keys(removed))
}

// wrapUnerasedDerivations answers every identifier assigned out of a key material producer in some
// function body of the source and neither handed to zeroize, moved out, nor installed in storage
// that outlives the frame, in that same body.
//
// The producer class is derived and handed in rather than written here, which is what lets the
// obligation and the derivation fail apart in the controls above. XwingEncapsulate's FIRST result
// is the ciphertext and is deliberately not in the class -- it is destined for the wire -- and
// nobody had to say so: no body of this package erases it, so the seed never admits it.
func wrapUnerasedDerivations(t *testing.T, producers map[wrapKeyProducer]string,
	name string, source string) []string {

	t.Helper()
	parsed := mustParseMessagegroupSource(t, name, source)
	held := []string{}
	for _, declaration := range parsed.Decls {
		function, isFunction := declaration.(*ast.FuncDecl)
		if !isFunction || function.Body == nil {
			continue
		}
		erased := wrapErasedNamesIn(function.Body)
		moved := wrapMovedOutIn(function.Body)
		installed := wrapInstalledNamesIn(function.Body)
		for _, binding := range wrapDerivationBindingsIn(function.Body) {
			if _, isProducer := producers[wrapKeyProducer{binding.callee, binding.position}]; !isProducer {
				continue
			}
			if erased[binding.name] || len(moved[binding.name]) != 0 || installed[binding.name] {
				continue
			}
			held = append(held, binding.name)
		}
	}
	slices.Sort(held)
	return slices.Compact(held)
}

// Property: the door holds no X-Wing private key in a field, which is the written excuse
// connect/mls's erase class carries for XwingPrivateKey.
//
// THE EXCUSE IS A CLAIM ABOUT THIS PACKAGE'S SOURCE AND THIS IS WHERE ITS FIRST CLAUSE IS TRUE OR
// NOT. mls/staged_erase_test.go excuses the type as an ANSWER that "XwingGenerateKey and
// XwingKeyGenFromSeed build one per call and no production declaration holds one in a field", and
// this door takes the private half as an ARGUMENT rather than holding one so that the sentence
// stays true. A field here and the excuse has to change with the code, in the same commit.
//
// IT IS NOT THE FIRST PRODUCTION CONSUMER OF THE TYPE and an earlier version of this paragraph
// said it was. sdk 48ee76e -- the S2-26 commit this file's prose already cites -- landed
// urmessage.Device.DecapsulateToOwnLeaf, which builds an XwingPrivateKey out of the device's
// retained seed on every wrap it opens and decapsulates with it. That consumer is one repository
// over, it is why the type's residual is measured rather than hypothetical, and the excuse's
// SECOND clause is held next door in TestXwingPrivateKeyOffersNoWayToDropWhatItHolds.
func TestNoDeclarationOfThisPackageHoldsAnXwingPrivateKeyInAField(t *testing.T) {
	_, sources := messagegroupProductionSources(t)
	holders := []string{}
	structs := 0
	for _, source := range sources {
		for _, declaration := range source.parsed.Decls {
			general, isGeneral := declaration.(*ast.GenDecl)
			if !isGeneral {
				continue
			}
			for _, spec := range general.Specs {
				typed, isTyped := spec.(*ast.TypeSpec)
				if !isTyped {
					continue
				}
				structure, isStruct := typed.Type.(*ast.StructType)
				if !isStruct || structure.Fields == nil {
					continue
				}
				structs += 1
				for _, field := range structure.Fields.List {
					if !strings.Contains(typeExprName(field.Type), "XwingPrivateKey") {
						continue
					}
					for _, named := range field.Names {
						holders = append(holders, typed.Name.Name+"."+named.Name)
					}
				}
			}
		}
	}
	if structs == 0 {
		t.Fatal("this reading found no struct type in this package's production source, so it judged nothing")
	}
	// the positive control, in the same query: the reading DOES see the type where it is declared
	if !slices.Contains(theXwingPrivateKeyFieldNames(sources), "seed") {
		t.Fatal("this reading cannot see XwingPrivateKey's own fields, so its zero above is a zero over nothing")
	}
	if len(holders) != 0 {
		t.Errorf("%v hold an XwingPrivateKey in a field, and connect/mls's erase class excuses that type on the written ground that no production declaration does. The excuse is now false and must change with the code, in this commit: either the type declares a Zeroize that erases its seed and its two private halves, or it carries a row of its own",
			holders)
	}
	t.Logf("%d struct types read; no field of any of them holds an XwingPrivateKey", structs)
}

// Property: XwingPrivateKey offers NO WAY for anybody to drop what it holds -- it declares no
// erase and every field of it is unexported.
//
// THIS IS THE EXCUSE'S SECOND CLAUSE, AND IT IS HERE BECAUSE NOTHING MEASURED IT. connect/mls's
// erase class excused the type on two grounds; the first is held one case up, and the second read
// "the seed inside it is the caller's to keep or to drop", which was not true and which no test
// anywhere asked. A caller cannot drop it: there is no exported field to blank and no method to
// call. The excuse now says the weaker thing that IS true -- the erase the class would demand has
// nowhere to land -- and this case is what keeps the two sentences honest in both directions.
//
// IT GOES RED ON AN IMPROVEMENT, deliberately. The day somebody gives this type a Zeroize, or
// exports the seed so a holder can reach it, the erase HAS somewhere to land, the excuse in
// mls/staged_erase_test.go becomes a row about a type that declares an erase -- which that gate
// refuses outright -- and both have to be rewritten in the commit that makes the change. That is
// the same coupling TestNoDeclarationOfThisPackageHoldsAnXwingPrivateKeyInAField carries for the
// first clause, and it is the whole point of writing an excuse that a measurement can reach.
//
// WHAT THE RESIDUAL IS, since this case is where somebody will look for it: after xwing.go's
// erases, a dropped key leaves its own thirty-two octet seed field and the parsed
// *mlkem.DecapsulationKey768 and *ecdh.PrivateKey, neither of which is a []byte this tree holds a
// header over. The ninety-six octet expansion is no longer among them. sdk's own
// wrapSeedDerivedNotAliasedSites names the site that pays for it -- Device.DecapsulateToOwnLeaf
// re-expands on every wrap it opens -- and closing it is ledger item 243's.
func TestXwingPrivateKeyOffersNoWayToDropWhatItHolds(t *testing.T) {
	_, sources := messagegroupProductionSources(t)
	fields := theXwingPrivateKeyFieldNames(sources)
	// the positive control, in the same query as the two zeros below: the reading DOES see the
	// type, so neither zero is a zero over nothing.
	if !slices.Contains(fields, "seed") {
		t.Fatalf("this reading found the fields %v of XwingPrivateKey and seed is not among them, so it is not reading the type this case is about",
			fields)
	}
	exported := []string{}
	for _, field := range fields {
		if field != "" && strings.ToUpper(field[:1]) == field[:1] {
			exported = append(exported, field)
		}
	}
	if len(exported) != 0 {
		t.Errorf("XwingPrivateKey exports %v, so a holder outside this package CAN reach what it holds and connect/mls's erase class excuses the type on the written ground that the erase it would demand has nowhere to land. The excuse and this case must change together, in this commit",
			exported)
	}
	// and the methods it declares, read off the source rather than off a name, so a Zeroize
	// written under any other name is found too: any method of this type that hands one of its
	// own fields to this package's eraser.
	erasing := []string{}
	methods := 0
	for _, source := range sources {
		for _, declaration := range source.parsed.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Recv == nil || len(function.Recv.List) != 1 {
				continue
			}
			if typeExprName(function.Recv.List[0].Type) != "XwingPrivateKey" {
				continue
			}
			methods += 1
			if function.Body == nil {
				continue
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, isCall := node.(*ast.CallExpr)
				if !isCall {
					return true
				}
				callee, isName := call.Fun.(*ast.Ident)
				if !isName || callee.Name != "zeroize" {
					return true
				}
				erasing = append(erasing, function.Name.Name)
				return true
			})
		}
	}
	// the second positive control: the type declares methods, so "no method erases" is a
	// statement about methods this reading actually found.
	if methods == 0 {
		t.Fatal("this reading found no method of XwingPrivateKey at all; Seed and Public are declared on it, so a zero here means the receiver matcher has stopped matching and the verdict below is empty")
	}
	if len(erasing) != 0 {
		t.Errorf("%v erase storage of XwingPrivateKey, so the type DOES declare an erase; connect/mls's erase class excuses it as a type whose erase would have nowhere to land, and that gate refuses a row on a type that declares one. Both must change in this commit",
			slices.Compact(erasing))
	}
	t.Logf("%d method(s) of XwingPrivateKey read, %d of them erasing; fields %v, %d of them exported",
		methods, len(slices.Compact(erasing)), fields, len(exported))
}

func theXwingPrivateKeyFieldNames(sources []messagegroupSource) []string {
	names := []string{}
	for _, source := range sources {
		for _, declaration := range source.parsed.Decls {
			general, isGeneral := declaration.(*ast.GenDecl)
			if !isGeneral {
				continue
			}
			for _, spec := range general.Specs {
				typed, isTyped := spec.(*ast.TypeSpec)
				if !isTyped || typed.Name.Name != "XwingPrivateKey" {
					continue
				}
				structure, isStruct := typed.Type.(*ast.StructType)
				if !isStruct || structure.Fields == nil {
					continue
				}
				for _, field := range structure.Fields.List {
					for _, named := range field.Names {
						names = append(names, named.Name)
					}
				}
			}
		}
	}
	return names
}

func typeExprName(expr ast.Expr) string {
	switch typed := expr.(type) {
	case *ast.Ident:
		return typed.Name
	case *ast.StarExpr:
		return typeExprName(typed.X)
	case *ast.SelectorExpr:
		return typed.Sel.Name
	case *ast.ArrayType:
		return typeExprName(typed.Elt)
	case *ast.MapType:
		return typeExprName(typed.Key) + "|" + typeExprName(typed.Value)
	}
	return ""
}

// ---------------------------------------------------------------------------
// the known answers
// ---------------------------------------------------------------------------

// Property: this package reproduces testdata/envelope-wrap-kat.txt, and so does RFC 5869 written
// out by hand over an info this case assembles from MASTER section 7's field list.
//
// TWO DERIVATIONS THAT SHARE NO CODE, which is what makes the rows worth more than a snapshot of
// the implementation. WrapInfo is one encoder; the reader below is a second one, written from
// MASTER's block, and the KDF halves come from keyschedule_test.go's transcription of RFC 5869
// rather than from mls's provider. A file regenerated to agree with a broken implementation still
// has to agree with both.
func TestTheWrapDoorMatchesItsKnownAnswers(t *testing.T) {
	rows := readWrapKat(t)
	envKey := wrapKatHex(t, rows, "INPUT_ENV_KEY")
	shared := wrapKatHex(t, rows, "INPUT_SS")
	groupId := wrapKatHex(t, rows, "INPUT_GROUP_ID")
	targetId := wrapKatHex(t, rows, "INPUT_TARGET_ID")
	ctXwing := wrapKatFill(t, rows, "INPUT_CT_XWING_FILL", "INPUT_CT_XWING_LEN", "INPUT_CT_XWING_SHA256")
	targetPub := wrapKatFill(t, rows, "INPUT_TARGET_PUB_FILL", "INPUT_TARGET_PUB_LEN", "INPUT_TARGET_PUB_SHA256")
	envelope := WrapEnvelope{
		FormatVersion: byte(wrapKatHex(t, rows, "INPUT_ENVELOPE_VERSION")[0]),
		TargetType:    byte(wrapKatHex(t, rows, "INPUT_ENVELOPE_TARGET_TYPE")[0]),
		PayloadType:   byte(wrapKatHex(t, rows, "INPUT_ENVELOPE_PAYLOAD_TYPE")[0]),
		ContentEpoch:  uint64(wrapKatInt(t, rows, "INPUT_ENVELOPE_CONTENT_EPOCH")),
	}
	if got := hex.EncodeToString(envelope.Encode()); got != rows["INPUT_ENVELOPE_ENCODED"] {
		t.Errorf("the envelope encodes to %s and the vector gives %s", got, rows["INPUT_ENVELOPE_ENCODED"])
	}
	if len(envelope.Encode()) != WrapEnvelopeBytes || WrapEnvelopeBytes != 11 {
		t.Errorf("the envelope is %d octets and MASTER section 7 fixes eleven", len(envelope.Encode()))
	}

	// section 2: the ladder head off a known envelope key
	ladders := 0
	for _, row := range wrapKatRows(t, rows, "LADDER") {
		fields := strings.Fields(row)
		if len(fields) != 4 {
			t.Fatalf("a LADDER row has %d fields, want 4: %q", len(fields), row)
		}
		leaf, err := strconv.ParseUint(fields[0], 10, 32)
		if err != nil {
			t.Fatalf("a LADDER row's leaf index: %v", err)
		}
		rung := WrapRecordKeyZero(envKey, uint32(leaf))
		key, nonce := RecordAeadBody(rung)
		if hex.EncodeToString(rung) != fields[1] {
			t.Errorf("record_key[0] at leaf %d is %x and the vector gives %s", leaf, rung, fields[1])
		}
		if hex.EncodeToString(key) != fields[2] || hex.EncodeToString(nonce) != fields[3] {
			t.Errorf("the body aead at leaf %d is %x/%x and the vector gives %s/%s", leaf, key, nonce, fields[2], fields[3])
		}
		// the second derivation: RFC 5869 by hand, over the ladder's own labels
		reference := keyScheduleReferenceExpand(envKey,
			append([]byte(recordKeyZeroInfo), leafIndexLP(uint32(leaf))...), recordKeyBytes)
		if !bytes.Equal(reference, rung) {
			t.Errorf("RFC 5869 written out gives %x for leaf %d and this package gives %x", reference, leaf, rung)
		}
		ladders += 1
	}
	if ladders < 3 {
		t.Fatalf("the vector carries %d LADDER rows; a reading that found fewer than three is not reading the file", ladders)
	}

	// section 3: the wrap kdf and the sealed body
	info := WrapInfo(envelope, groupId, targetId, XwingAlgId, targetPub, ctXwing)
	if got := wrapKatInt(t, rows, "INFO_LEN"); len(info) != got {
		t.Errorf("the info is %d octets and the vector gives %d", len(info), got)
	}
	if got := sha256.Sum256(info); hex.EncodeToString(got[:]) != rows["INFO_SHA256"] {
		t.Errorf("H(info) is %x and the vector gives %s", got, rows["INFO_SHA256"])
	}
	// THE SECOND ENCODER, assembled here from MASTER section 7's field list rather than by
	// calling WrapInfo, so a transposition inside WrapInfo is visible
	if !bytes.Equal(info, wrapKatReferenceInfo(rows, envelope, groupId, targetId, targetPub, ctXwing)) {
		t.Error("WrapInfo and MASTER section 7's field list written out here produce different octets")
	}
	key, nonce := wrapKeyMaterial(shared, info)
	if hex.EncodeToString(key) != rows["WRAP_KEY"] {
		t.Errorf("wrap_key is %x and the vector gives %s", key, rows["WRAP_KEY"])
	}
	if hex.EncodeToString(nonce) != rows["WRAP_NONCE"] {
		t.Errorf("wrap_nonce is %x and the vector gives %s", nonce, rows["WRAP_NONCE"])
	}
	// and the second derivation of both halves, from RFC 5869 by hand
	referencePrk := keyScheduleReferenceExtract([]byte(rows["PRK_SALT"]), shared)
	referenceMaterial := keyScheduleReferenceExpand(referencePrk, info, wrapAeadMaterialBytes)
	if !bytes.Equal(referenceMaterial[:recordAeadKeyBytes], key) ||
		!bytes.Equal(referenceMaterial[recordAeadKeyBytes:], nonce) {
		t.Error("RFC 5869 written out over MASTER's salt does not reproduce this package's wrap_key | wrap_nonce")
	}
	if rows["PRK_SALT"] != wrapSaltLabel || rows["INFO_LABEL"] != wrapInfoLabel {
		t.Errorf("the vector's labels are %q and %q and this package's are %q and %q",
			rows["PRK_SALT"], rows["INFO_LABEL"], wrapSaltLabel, wrapInfoLabel)
	}
	if rows["ALG_ID"] != fmt.Sprintf("%04x", XwingAlgId) {
		t.Errorf("the vector's alg_id is %s and this package's is %04x", rows["ALG_ID"], XwingAlgId)
	}

	payload := wrapTestPayload(0x11)
	if got := wrapKatInt(t, rows, "INPUT_PAYLOAD_LEN"); len(payload) != got {
		t.Fatalf("the fixture payload is %d octets and the vector's is %d", len(payload), got)
	}
	if got := sha256.Sum256(payload); hex.EncodeToString(got[:]) != rows["INPUT_PAYLOAD_SHA256"] {
		t.Fatalf("the fixture payload is not the vector's")
	}
	body, err := sealWrapBodyWith(envelope, groupId, targetId, targetPub, ctXwing, shared, payload)
	if err != nil {
		t.Fatalf("sealWrapBodyWith over the vector's inputs: %v", err)
	}
	if len(body) != wrapKatInt(t, rows, "WRAP_BODY_LEN") {
		t.Errorf("the wrap body is %d octets and the vector gives %d", len(body), wrapKatInt(t, rows, "WRAP_BODY_LEN"))
	}
	if got := sha256.Sum256(body); hex.EncodeToString(got[:]) != rows["WRAP_BODY_SHA256"] {
		t.Errorf("H(wrap_body) is %x and the vector gives %s", got, rows["WRAP_BODY_SHA256"])
	}
	if got := len(body) + lpPrefixBytes; got != wrapKatInt(t, rows, "WRAP_BODY_OCCUPANCY") {
		t.Errorf("the occupancy is %d and the vector gives %d", got, wrapKatInt(t, rows, "WRAP_BODY_OCCUPANCY"))
	}
	if got := wrapKatInt(t, rows, "WRAP_BODY_RUNG") - wrapKatInt(t, rows, "WRAP_BODY_OCCUPANCY"); got != wrapKatInt(t, rows, "WRAP_BODY_ZERO_TAIL") {
		t.Errorf("the vector's own rung arithmetic gives a %d octet tail and its tail row says %d", got, wrapKatInt(t, rows, "WRAP_BODY_ZERO_TAIL"))
	}
	_, vectorCt, vectorAead, err := parseHybridCt(body[WrapEnvelopeBytes:])
	if err != nil {
		t.Fatalf("the vector's own body does not parse: %v", err)
	}
	if !bytes.Equal(vectorCt, ctXwing) {
		t.Error("the body's ct_xwing is not the vector's")
	}
	if len(vectorAead) != wrapKatInt(t, rows, "AEAD_CT_LEN") {
		t.Errorf("aead_ct is %d octets and the vector gives %d", len(vectorAead), wrapKatInt(t, rows, "AEAD_CT_LEN"))
	}
	if got := sha256.Sum256(vectorAead); hex.EncodeToString(got[:]) != rows["AEAD_CT_SHA256"] {
		t.Errorf("H(aead_ct) is %x and the vector gives %s", got, rows["AEAD_CT_SHA256"])
	}

	// section 4: the failing direction, one row per envelope octet
	octets := 0
	base := sha256.Sum256(info)
	for _, row := range wrapKatRows(t, rows, "ENVELOPE_OCTET") {
		fields := strings.Fields(row)
		if len(fields) != 3 {
			t.Fatalf("an ENVELOPE_OCTET row has %d fields, want 3: %q", len(fields), row)
		}
		index, err := strconv.Atoi(fields[0])
		if err != nil {
			t.Fatalf("an ENVELOPE_OCTET row's index: %v", err)
		}
		edited := envelope.Encode()
		edited[index] ^= 0xFF
		flipped, err := ParseWrapEnvelope(edited)
		if err != nil {
			t.Fatalf("parse the flipped envelope at %d: %v", index, err)
		}
		digest := sha256.Sum256(WrapInfo(flipped, groupId, targetId, XwingAlgId, targetPub, ctXwing))
		if hex.EncodeToString(digest[:]) != fields[2] {
			t.Errorf("flipping envelope octet %d gives H(info) %x and the vector gives %s", index, digest, fields[2])
		}
		want := "bound"
		if digest == base {
			want = "unbound"
		}
		if fields[1] != want {
			t.Errorf("envelope octet %d reads as %s and the vector calls it %s", index, want, fields[1])
		}
		octets += 1
	}
	if octets != WrapEnvelopeBytes {
		t.Fatalf("the vector carries %d ENVELOPE_OCTET rows and the envelope is %d octets", octets, WrapEnvelopeBytes)
	}

	// section 5: the OTHER failing direction, where nothing was edited at all. Each row is the
	// section 1 envelope with one field moved, and what it fixes is that such a wrap has a
	// perfectly good key of its own -- a different digest, not an absent one -- so no authority
	// inside the seal can refuse it and the opener's comparison is what must.
	genuine := 0
	for _, row := range wrapKatRows(t, rows, "GENUINE_OTHER") {
		fields := strings.Fields(row)
		if len(fields) != 3 {
			t.Fatalf("a GENUINE_OTHER row has %d fields, want 3: %q", len(fields), row)
		}
		encoded, err := hex.DecodeString(fields[1])
		if err != nil {
			t.Fatalf("a GENUINE_OTHER row's envelope: %v", err)
		}
		other, err := ParseWrapEnvelope(encoded)
		if err != nil {
			t.Fatalf("parse the %s row's envelope: %v", fields[0], err)
		}
		moved := []string{}
		if other.ContentEpoch != envelope.ContentEpoch {
			moved = append(moved, "content_epoch")
		}
		if other.TargetType != envelope.TargetType {
			moved = append(moved, "target_type")
		}
		if other.PayloadType != envelope.PayloadType {
			moved = append(moved, "payload_type")
		}
		if other.FormatVersion != envelope.FormatVersion {
			moved = append(moved, "wrap_format_version")
		}
		if !slices.Equal(moved, []string{fields[0]}) {
			t.Errorf("the %s row moves %v against section 1's envelope, and a row that moves anything else is not measuring the field it names", fields[0], moved)
		}
		digest := sha256.Sum256(WrapInfo(other, groupId, targetId, XwingAlgId, targetPub, ctXwing))
		if hex.EncodeToString(digest[:]) != fields[2] {
			t.Errorf("the %s row's H(info) is %x and the vector gives %s", fields[0], digest, fields[2])
		}
		// THE CHECK THE ROW EXISTS FOR: a different key, not a broken one. An implementation
		// whose digest here equalled INFO_SHA256 would have a field that reaches no key at all,
		// which is section 4's "unbound" verdict arriving where no octet was edited.
		if digest == base {
			t.Errorf("moving %s leaves H(info) at INFO_SHA256, so that field is in no element of wrap_key's info and belongs in section 4 as unbound", fields[0])
		}
		// and this package refuses such a wrap at the door, by the opener's own authority: the
		// three values the opener states stay where section 1 put them while the body carries
		// the moved envelope
		sectionOneEpoch, sectionOneTargetType, sectionOnePayloadType := wrapTestAuthority(envelope)
		if _, disagrees := wrapEnvelopeDisagreement(other, sectionOneEpoch,
			sectionOneTargetType, sectionOnePayloadType); !disagrees {
			t.Errorf("an opener honouring section 1's envelope finds no disagreement with the %s row, so nothing in this door separates a genuine wrap of another %s",
				fields[0], fields[0])
		}
		genuine += 1
	}
	if genuine != 2 {
		t.Fatalf("the vector carries %d GENUINE_OTHER rows; the two MASTER section 8.2 puts at one wrap_target_handle are the epoch and the payload kind", genuine)
	}
}

// wrapKatReferenceInfo is MASTER section 7's nine elements, written out here from the block and
// not by calling WrapInfo. LP is a fixed thirty two bit big endian length, which is the record
// layer's prefix and the one the spec's notation means.
func wrapKatReferenceInfo(rows map[string]string, envelope WrapEnvelope, groupId []byte,
	targetId []byte, targetPub []byte, ctXwing []byte) []byte {

	lp := func(out []byte, value []byte) []byte {
		length := uint32(len(value))
		out = append(out, byte(length>>24), byte(length>>16), byte(length>>8), byte(length))
		return append(out, value...)
	}
	u64 := func(out []byte, value uint64) []byte {
		for shift := 56; 0 <= shift; shift -= 8 {
			out = append(out, byte(value>>uint(shift)))
		}
		return out
	}
	out := []byte(rows["INFO_LABEL"])
	out = lp(out, groupId)
	out = u64(out, envelope.ContentEpoch)
	out = append(out, envelope.TargetType)
	out = lp(out, targetId)
	out = append(out, envelope.PayloadType)
	algId, _ := hex.DecodeString(rows["ALG_ID"])
	out = append(out, algId...)
	out = lp(out, targetPub)
	return lp(out, ctXwing)
}

// The suffix under which a repeated row name is collected, so one map can carry both the single
// valued rows and the tables.
const wrapKatMultiSuffix = "[]"

// The separator a repeated table's rows are joined under, which cannot occur in a row: this file
// is ascii text and a NUL in it would already have failed the read.
const wrapKatRowSeparator = "\x00"

// Property: no two OCTET STRING inputs of the known answers that are the same width carry the
// same octets.
//
// WHY THE PROPERTY AND NOT A REREADING OF THE PARAGRAPH. The file's own input paragraph says its
// inputs are ascending runs "so that a transposition of any two inputs is visible rather than
// symmetric", and for the three envelope octets that sentence was false for as long as it stood:
// u8(target_type) and u8(payload_type) were both 0x01 and MASTER section 7's info writes them four
// elements apart. Measured against a writer that swaps the two positions, the old inputs let it
// reproduce every PRIMARY row -- INFO_SHA256, WRAP_KEY, WRAP_NONCE, AEAD_CT_SHA256 and
// WRAP_BODY_SHA256 -- and it was caught only by the three rows of sections 4 and 5 that perturb
// one of the two octets. With the octets at 01 02 03 it is caught by all of them. A prose sentence
// checked by a reader is a sentence that gets re-checked by nobody, so the rule is asserted here
// instead.
//
// SAME WIDTH IS THE WHOLE OF THE PAIRING, because a transposition is only well formed between two
// values of one width -- swapping a thirty-two octet key with a sixteen octet target id does not
// produce a file, it produces a parse error. So the reading buckets by length and compares inside
// each bucket, and the bucket sizes are logged so a reading that had stopped finding the inputs
// reports a suspiciously empty set rather than a clean run.
func TestNoTwoKnownAnswerInputsOfTheSameWidthAreEqual(t *testing.T) {
	// the negative control first, through the same matcher: two equal values of one width must be
	// reported, and two equal values of DIFFERENT widths must not.
	collisions := wrapKatInputCollisions(map[string][]byte{
		"A": {0x01, 0x02}, "B": {0x01, 0x02}, "C": {0x01, 0x02, 0x03}, "D": {0x09, 0x09},
	})
	if want := []string{"A == B"}; !slices.Equal(collisions, want) {
		t.Fatalf("the control reads %v, want %v; the matcher is not comparing inside a width bucket, or it is comparing across widths",
			collisions, want)
	}

	rows := readWrapKat(t)
	inputs := map[string][]byte{}
	for name, value := range rows {
		if !strings.HasPrefix(name, "INPUT_") || strings.HasSuffix(name, "_LEN") {
			continue
		}
		// a DECIMAL count or epoch is not an octet string, and the two that are written in
		// decimal are told apart by parsing: every octet string in this file is lower case hex of
		// even length, which "7" is not.
		if name == "INPUT_ENVELOPE_CONTENT_EPOCH" {
			continue
		}
		octets, err := hex.DecodeString(value)
		if err != nil || len(octets) == 0 {
			continue
		}
		inputs[name] = octets
	}
	// the two long inputs are RULES rather than literals, so they are built the way the file says
	// to build them and compared as the octets they are.
	inputs["INPUT_CT_XWING"] = wrapKatFill(t, rows, "INPUT_CT_XWING_FILL", "INPUT_CT_XWING_LEN", "INPUT_CT_XWING_SHA256")
	inputs["INPUT_TARGET_PUB"] = wrapKatFill(t, rows, "INPUT_TARGET_PUB_FILL", "INPUT_TARGET_PUB_LEN", "INPUT_TARGET_PUB_SHA256")
	// the positive control, in the same query as the verdict: the three envelope octets are the
	// pair this case was written for, and a reading that had stopped seeing them would report the
	// same clean run a complete one reports.
	for _, wanted := range []string{
		"INPUT_ENVELOPE_VERSION", "INPUT_ENVELOPE_TARGET_TYPE", "INPUT_ENVELOPE_PAYLOAD_TYPE",
		"INPUT_ENV_KEY", "INPUT_SS", "INPUT_GROUP_ID", "INPUT_TARGET_ID",
	} {
		if _, isRead := inputs[wanted]; !isRead {
			t.Fatalf("this reading does not reach %s, so its verdict below is a verdict over %d input(s) that are not the ones the file is built from",
				wanted, len(inputs))
		}
	}
	widths := map[int][]string{}
	for name, octets := range inputs {
		widths[len(octets)] = append(widths[len(octets)], name)
	}
	for _, width := range slices.Sorted(maps.Keys(widths)) {
		t.Logf("%d octet(s): %v", width, slices.Sorted(slices.Values(widths[width])))
	}
	if collisions := wrapKatInputCollisions(inputs); len(collisions) != 0 {
		t.Errorf("%v are inputs of the same width carrying the same octets, so a transposition of the pair reproduces every answer below it and this file cannot see it. The inputs of this file are ascending runs precisely so that it can",
			collisions)
	}
}

// wrapKatInputCollisions answers every pair of inputs of one width carrying the same octets.
func wrapKatInputCollisions(inputs map[string][]byte) []string {
	names := slices.Sorted(maps.Keys(inputs))
	collisions := []string{}
	for i, one := range names {
		for _, other := range names[i+1:] {
			if len(inputs[one]) != len(inputs[other]) {
				continue
			}
			if bytes.Equal(inputs[one], inputs[other]) {
				collisions = append(collisions, one+" == "+other)
			}
		}
	}
	slices.Sort(collisions)
	return collisions
}

func readWrapKat(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile("testdata/envelope-wrap-kat.txt")
	if err != nil {
		t.Fatalf("read the wrap known answers: %v", err)
	}
	// CRLF is folded before anything is read, for the reason the eph window table's gate folds
	// it: core.autocrlf is true at system scope on the boxes that build this repo, and a digest
	// or a field split that cried wolf on a clean checkout would be deleted rather than fixed.
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	rows := map[string]string{}
	multi := map[string][]string{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, rest, found := strings.Cut(line, " ")
		if !found {
			t.Fatalf("a row of the wrap known answers has no value: %q", line)
		}
		rest = strings.TrimSpace(rest)
		switch name {
		case "LADDER", "ENVELOPE_OCTET", "GENUINE_OTHER":
			multi[name] = append(multi[name], rest)
		default:
			if _, isRepeat := rows[name]; isRepeat {
				t.Fatalf("the wrap known answers carry %s twice", name)
			}
			rows[name] = rest
		}
	}
	if len(rows) == 0 {
		t.Fatal("the wrap known answers parsed to no rows at all")
	}
	out := map[string]string{}
	for name, value := range rows {
		out[name] = value
	}
	for name, values := range multi {
		out[name+wrapKatMultiSuffix] = strings.Join(values, wrapKatRowSeparator)
	}
	return out
}

// wrapKatRows answers the rows of one repeated table, split back out of the single value the
// reader joined them into.
func wrapKatRows(t *testing.T, rows map[string]string, name string) []string {
	t.Helper()
	joined, found := rows[name+wrapKatMultiSuffix]
	if !found {
		t.Fatalf("the wrap known answers carry no %s rows", name)
	}
	return strings.Split(joined, wrapKatRowSeparator)
}

func wrapKatHex(t *testing.T, rows map[string]string, name string) []byte {
	t.Helper()
	value, found := rows[name]
	if !found {
		t.Fatalf("the wrap known answers carry no %s row", name)
	}
	decoded, err := hex.DecodeString(value)
	if err != nil {
		t.Fatalf("%s is not hex: %v", name, err)
	}
	return decoded
}

func wrapKatInt(t *testing.T, rows map[string]string, name string) int {
	t.Helper()
	value, found := rows[name]
	if !found {
		t.Fatalf("the wrap known answers carry no %s row", name)
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		t.Fatalf("%s is not a decimal integer: %v", name, err)
	}
	return parsed
}

func wrapKatFill(t *testing.T, rows map[string]string, fill string, length string, digest string) []byte {
	t.Helper()
	octet := wrapKatHex(t, rows, fill)
	if len(octet) != 1 {
		t.Fatalf("%s is %d octets, want one", fill, len(octet))
	}
	built := bytes.Repeat(octet, wrapKatInt(t, rows, length))
	got := sha256.Sum256(built)
	if hex.EncodeToString(got[:]) != rows[digest] {
		t.Fatalf("the octets %s and %s describe hash to %x and %s gives %s", fill, length, got, digest, rows[digest])
	}
	return built
}
