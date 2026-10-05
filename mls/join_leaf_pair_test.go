// The joiner's own two halves, held against the leaf its key package published.
//
// The join door held the SIGNING half -- signaturePublicKeyOf against LeafNode.SignatureKey -- and
// installed the ENCRYPTION half against nothing. The reason written down for that, in the
// production comment at step 6 and in the round that reviewed it, was that "the provider has no
// private-to-public operation for HPKE". That is true of the CryptoProvider INTERFACE and is not a
// reason: signaturePublicKeyOf is itself a package level derivation deliberately outside the
// interface, with a comment giving that reason, and both registered suites are DHKEM(X25519), so
// the public half of the scalar a caller holds is one multiplication away.
//
// What the gap cost is not a join that fails. It is a join that SUCCEEDS: the member agrees on the
// epoch authenticator, carries application traffic for the rest of the epoch, and then decrypts
// nothing from the first commit that seals an update path to its leaf.
package mls

import (
	"bytes"
	"errors"
	"testing"
)

// TestJoinFromWelcomeRefusesALeafPrivateKeyThatIsNotTheLeafs is the door, over the two shapes a
// caller's keyring can be wrong in: a key that parses and belongs to another pair, and a key that
// does not parse at all.
//
// The fixture's own successful join is the live control -- testTwoMemberGroupNamed refuses to
// answer without one -- so a refusal here is about the one field each arm edits.
func TestJoinFromWelcomeRefusesALeafPrivateKeyThatIsNotTheLeafs(t *testing.T) {
	crypto := testCrypto(t)
	group, joined, _, _, material := testTwoMemberGroupNamed(t, crypto, "join-leaf-pair")
	defer group.Close()
	defer joined.Close()

	otherPriv, otherPub, err := crypto.DeriveKeyPair(crypto.Random(crypto.HashSize()))
	if err != nil {
		t.Fatalf("draw the second key pair this case joins with: %v", err)
	}
	if bytes.Equal(otherPub, material.keys.KeyPackage.LeafNode.EncryptionKey) {
		t.Fatal("the second key pair is the one the leaf publishes, so neither arm below observes anything")
	}

	edits := map[string]func(*JoinKeyMaterial){
		"another key pair's private half": func(keys *JoinKeyMaterial) {
			keys.EncryptPrivate = HpkePrivateKey(bytes.Clone(otherPriv))
		},
		// truncation reaches the same refusal through X25519PrivateKey's length gate rather than
		// through the comparison, which is what says the door answers one sentence for "this is not
		// the leaf's key" however the caller's keyring is wrong.
		"a private half one octet short": func(keys *JoinKeyMaterial) {
			keys.EncryptPrivate = HpkePrivateKey(bytes.Clone(material.keys.EncryptPrivate)[:x25519KeySize-1])
		},
		"no private half at all": func(keys *JoinKeyMaterial) {
			keys.EncryptPrivate = nil
		},
	}
	for name, edit := range edits {
		grafted, err := material.join(t, edit)
		if !errors.Is(err, errJoinerEncryptionKeyNotTheLeafs) {
			t.Errorf("JoinFromWelcome with %s = %v, want errJoinerEncryptionKeyNotTheLeafs", name, err)
		}
		if grafted != nil {
			t.Errorf("JoinFromWelcome with %s answered a group as well as an error", name)
			grafted.Close()
		}
	}
}

// TestAJoinerThatHeldTheWrongLeafKeyWouldDecryptNothing is why the refusal above is worth a door
// rather than a note, and it is the reading no round trip through the join can make.
//
// It runs the SAME material through the same commit twice: once with the leaf key the joiner
// published, which opens the next commit's update path, and once with the private half of another
// pair. The second is the member the missing check produced -- and it is built here directly out of
// the parts the door assembles, because the door now refuses to build it.
func TestAJoinerThatHeldTheWrongLeafKeyWouldDecryptNothing(t *testing.T) {
	crypto := testCrypto(t)
	group, joined, _, _, _ := testTwoMemberGroupNamed(t, crypto, "join-wrong-leaf-key")
	defer group.Close()
	defer joined.Close()

	// the control: with the leaf key it published, this member opens the committer's next update
	// path and enters the epoch that commit opens.
	result, err := group.CreateCommit(nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateCommit: %v", err)
	}
	processed, err := joined.ProcessMessage(result.Commit)
	if err != nil {
		t.Fatalf("the member holding its own leaf key could not process the commit: %v", err)
	}
	if err := joined.ApplyCommit(processed); err != nil {
		t.Fatalf("ApplyCommit: %v", err)
	}
	if err := group.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit: %v", err)
	}
	if !bytes.Equal(joined.EpochAuthenticator(), group.EpochAuthenticator()) {
		t.Fatal("the control member did not enter the epoch the commit opened")
	}

	// and the same member with another pair's private half in the same slot: the leaf it publishes
	// is unchanged, every octet the committer sealed is addressed to that leaf, and none of it
	// opens.
	otherPriv, _, err := crypto.DeriveKeyPair(crypto.Random(crypto.HashSize()))
	if err != nil {
		t.Fatalf("draw the second key pair: %v", err)
	}
	joined.stateLock.Lock()
	joined.ownPriv.EncryptionPriv = HpkePrivateKey(bytes.Clone(otherPriv))
	joined.stateLock.Unlock()

	next, err := group.CreateCommit(nil, nil, nil)
	if err != nil {
		t.Fatalf("the second CreateCommit: %v", err)
	}
	if _, err := joined.ProcessMessage(next.Commit); err == nil {
		t.Fatal("a member holding another pair's private half processed a commit sealed to its own published leaf key")
	}
}

// TestEveryRegisteredSuiteNamesTheKemThisDerivationAssumes is what makes hpkePublicKeyOf sound over
// the registry rather than over the suite the fixtures happen to run.
//
// It reads no ciphersuite argument -- there is none to read, for signaturePublicKeyOf's reason --
// so what it assumes is that every registered suite is DHKEM(X25519) with a 32 octet scalar. The
// assumption is asserted here rather than glossed, and it is the twin of
// TestEverySuiteNamesTheSignatureSchemeTheProviderComputes for the encryption half.
func TestEveryRegisteredSuiteNamesTheKemThisDerivationAssumes(t *testing.T) {
	suites := Suites()
	if len(suites) == 0 {
		t.Fatal("the registry names no suite, so this holds nothing to the assumption")
	}
	for _, suite := range suites {
		params, err := LookupSuite(suite)
		if err != nil {
			t.Fatalf("LookupSuite(%#04x): %v", uint16(suite), err)
		}
		if params.KemId != HpkeKemX25519HkdfSha256 {
			t.Errorf("suite %#04x names kem %#04x; hpkePublicKeyOf multiplies the base point of x25519 and would answer another curve's key for it",
				uint16(suite), uint16(params.KemId))
		}
		if params.Nsk != x25519KeySize || params.Npk != x25519KeySize {
			t.Errorf("suite %#04x names Nsk %d and Npk %d, and hpkePublicKeyOf gates both at %d",
				uint16(suite), params.Nsk, params.Npk, x25519KeySize)
		}
	}
}

// TestHpkePublicKeyOfAnswersTheHalfTheKeyPairConstructorsAnswer holds the derivation against the two
// constructors that hand out a private half at all, over the suites above.
//
// AGAINST THE CONSTRUCTORS AND NOT AGAINST A VECTOR, deliberately: what a caller of this function
// has is a key one of them produced, so the property is that the two agree. A vector would say the
// same thing about the curve and nothing about the pairing.
func TestHpkePublicKeyOfAnswersTheHalfTheKeyPairConstructorsAnswer(t *testing.T) {
	for _, suite := range Suites() {
		params, err := LookupSuite(suite)
		if err != nil {
			t.Fatalf("LookupSuite(%#04x): %v", uint16(suite), err)
		}
		crypto, err := NewCryptoProvider(suite)
		if err != nil {
			t.Fatalf("NewCryptoProvider(%#04x): %v", uint16(suite), err)
		}
		ikm := crypto.Random(crypto.HashSize())
		derivedPriv, derivedPub, err := HpkeDeriveKeyPair(params, ikm)
		if err != nil {
			t.Fatalf("HpkeDeriveKeyPair(%#04x): %v", uint16(suite), err)
		}
		recovered, err := hpkePublicKeyOf(derivedPriv)
		if err != nil {
			t.Fatalf("hpkePublicKeyOf over a key HpkeDeriveKeyPair drew (%#04x): %v", uint16(suite), err)
		}
		if !bytes.Equal(recovered, derivedPub) {
			t.Errorf("suite %#04x: hpkePublicKeyOf answered %x and HpkeDeriveKeyPair answered %x",
				uint16(suite), recovered, derivedPub)
		}
		providerPriv, providerPub, err := crypto.DeriveKeyPair(ikm)
		if err != nil {
			t.Fatalf("CryptoProvider.DeriveKeyPair(%#04x): %v", uint16(suite), err)
		}
		recovered, err = hpkePublicKeyOf(providerPriv)
		if err != nil {
			t.Fatalf("hpkePublicKeyOf over a key the provider drew (%#04x): %v", uint16(suite), err)
		}
		if !bytes.Equal(recovered, providerPub) {
			t.Errorf("suite %#04x: hpkePublicKeyOf answered %x and the provider answered %x",
				uint16(suite), recovered, providerPub)
		}
		// and a scalar of the wrong width is a refusal rather than a key, which is the arm the join
		// door's length gate stands on.
		if _, err := hpkePublicKeyOf(HpkePrivateKey(derivedPriv[:x25519KeySize-1])); !errors.Is(err, ErrBadKeyLength) {
			t.Errorf("suite %#04x: hpkePublicKeyOf over a short scalar = %v, want ErrBadKeyLength", uint16(suite), err)
		}
	}
}
