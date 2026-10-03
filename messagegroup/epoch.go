// pq_secret, and the provisional epoch state guardrail G10 destroys.
//
// TWO THINGS LIVE HERE AND THEY ARE HERE BECAUSE THEY DIE TOGETHER. The first is the sampler for
// pq_secret[n], which MASTER section 7 specifies in five words -- "32 B CSPRNG at commit" -- and
// for which spec A section 5 declares no function, no type, no file and no section (open item
// M1-3). The second is the value spec A section 5.12 step 1 orders discarded on any rejection of
// a commit submission, which no document gives a home either (open item M1-20). They share a file
// because the sampler's output is the first field of that value and because the whole of G10 is
// the sentence joining them: "the provisional epoch state is a value that ClearPendingCommit
// destroys; there is no path that reads it afterwards."
//
// THE SAMPLER'S SIGNATURE IS THE DEFENCE AND NOT ITS BODY. It takes an io.Reader and nothing
// else: no group, no epoch, no storage root, no class key. A pq_secret derived from anything
// durable would compile, round trip, agree between two clients, pass every behavioural test in
// this package -- and forfeit the post quantum property in silence, because storage_root =
// HKDF-Extract(salt = mls_secret, ikm = pq_secret) is only PQ hard when the ikm arrived under
// X-Wing rather than out of the classical handshake the attacker already harvested. That is the
// same defect class as a derived eph_root, and it gets the same defence: the parameter list is
// refused as a SIGNATURE, by a gate reading this declaration, rather than as a behaviour no test
// can observe. epoch_test.go's half A holds it; half B walks this function's call graph and
// requires that it reach the reader it was handed and reach no derivation at all.
//
// WHAT THE SAMPLER DOES NOT DO IS DELIVER. pq_secret[n] reaches the other devices inside a device
// wrap (spec A section 5.10 E1, MASTER section 8.2), and after the 2026-09-13 ruling that is a
// PERMANENT class record of its own with eph_root[n] riding a second EPH(5) one. None of that is
// here. A 32 octet CSPRNG draw is not an ambiguity; its delivery is, and its delivery is open item
// M1-1's, which is task 14's file and not this one's.
//
// THE PROVISIONAL VALUE HOLDS WHAT SECTION 5.12 STEP 1 LISTS AND ITS DESTRUCTOR CALLS
// ClearPendingCommit FROM INSIDE ITSELF. Step 1 orders the committer to discard "TreeKEM path
// secrets, storage_root[n+1], write_key[n+1], eph_root[n+1], pq_secret[n+1] and every X-Wing wrap
// it built" -- six things, of which connect/mls knows exactly one. The landed ClearPendingCommit
// (mls/group.go:2475) erases the staged MLS epoch and nothing else; the other five had no home in
// this package at all. So the mls call is INSIDE this value's destructor rather than beside it,
// because two erasures a caller has to remember to make in pairs is one erasure a caller will make
// alone -- and a half destroyed epoch is worse than an undestroyed one, since the surviving half
// still looks like a value somebody may use.
//
// AND ONE VALUE IS DELIBERATELY NOT PART OF IT: env_key[k]. Spec A section 5.11 seals every device
// wrap under env_key[k] = MLS-Exporter("URmessage/v1/envelope", "", 32) and makes caching that key
// a normative obligation, because (*Group).Export reads the CURRENT schedule and connect has no
// ExportAt -- measured, no ExportAt is DECLARED anywhere in connect, and every occurrence of the
// name is prose arguing about it: this sentence, the one epoch_test.go repeats it in, and, since
// item 241 and m1 task 14, pastepoch.go's and wrap.go's, which both say why there is no such
// declaration to write -- so env_key[k] is computable off the LIVE handle only while the group
// stands at epoch k. (*Amended here on the commit that added the fourth occurrence: this clause
// read "the only occurrences of the name are this sentence and the one epoch_test.go repeats it
// in", which was true when written and false from the commit that landed pastepoch.go. It is
// corrected rather than deleted because the MEASUREMENT it carries -- no declaration -- is the
// load-bearing half and is still exactly true.*) It is therefore not provisional
// committer state at all: it belongs to an epoch that may already be OPEN, and destroying it
// because some LATER commit was rejected would discard the only route into that open epoch's
// storage_root that does not go back through LoadGroup. This type declares no field able to hold one, and epoch_test.go holds the shape as
// well as the behaviour, because the shape is what stops the behaviour being re-broken by somebody
// who finds it convenient to keep the two together.
package messagegroup

import (
	"fmt"
	"io"

	"github.com/urnetwork/connect/mls"
)

// PqSecretBytes is the width MASTER section 7 fixes for pq_secret[n].
//
// It is the ikm of storage_root's extraction, so it is not a tunable: a shorter draw still
// extracts to thirty two octets and still round trips, which is the whole reason the width is a
// constant here rather than a caller's argument.
const PqSecretBytes = 32

// NewPqSecret draws pq_secret[n] out of random and out of nothing else.
//
// The value is the draw ITSELF and is not expanded, whitened or mixed on the way out. That is
// MASTER section 7's "32 B CSPRNG" read literally, and it is worth stating because an expansion
// here would be invisible: HKDF-Expand of a good draw is still a good secret, both clients would
// still agree, and the test that says the output equals what the source supplied is the only thing
// that can tell the two apart.
//
// A nil reader is REFUSED and never filled in from the process source. That is the position
// mls.X25519GenerateKey argues at length and which xwing.go took second: a key drawn from
// crypto/rand behind the caller's back is a GOOD key, every behavioural test passes, and every
// randomness parameter above it silently becomes decoration. It answers mls's own sentinel rather
// than declaring a second name for one condition, so a caller holding either package matches the
// refusal with one errors.Is. Without the guard io.ReadFull dereferences the nil interface and this
// takes the caller's process down instead of its call.
//
// A short read is returned UNWRAPPED, for the reason xwing.go gives: a failing randomness source is
// not a width problem and must not be reported as one. What matters is that it is returned at all
// -- a fallback onto a second source when the caller's runs dry is the same substitution as the nil
// case with the opposite symptom, and in a key encapsulation it is the worse one.
func NewPqSecret(random io.Reader) ([]byte, error) {
	if random == nil {
		return nil, mls.ErrNilRandomSource
	}
	secret := make([]byte, PqSecretBytes)
	if _, err := io.ReadFull(random, secret); err != nil {
		return nil, err
	}
	return secret, nil
}

// ProvisionalEpoch is everything a committer holds for epoch n+1 before the server has accepted its
// commit, and which spec A section 5.12 step 1 orders discarded the moment the server has not.
//
// EVERY FIELD IS ONE ITEM OF STEP 1'S LIST, and there is no field that is not. epoch_test.go pins
// the field set as a WHOLE rather than banning names, which is the only reading under which a
// cached env_key added here fails on the commit that adds it whatever it is called.
//
// IT TAKES OWNERSHIP OF THE SLICES IT IS GIVEN AND DOES NOT COPY THEM, which is the opposite of
// what most constructors in this package do and is deliberate. zeroize.go's own comment states the
// discipline that makes best effort erasure worth anything -- "derive a secret into a slice, pass
// that slice, do not copy it" -- and a constructor that copied would leave the committer's original
// arrays live after Destroy had run, which is a destructor that destroys a duplicate. For the same
// reason the accessors hand back the LIVE slice rather than a copy: task 15's fan out builds the
// device wraps out of these bytes, and the whole of G10 is that those bytes stop existing together.
// That sentence is HELD and is not a comment -- epoch_test.go's
// TestEveryAccessorOfAProvisionalEpochHandsBackTheLiveSliceAndNotACopy derives the class of slice
// answering accessors off the type and requires each one's answer to be dead after Destroy, because
// an accessor that copied would hand task 15 a buffer this destructor never reaches and section
// 5.12 step 2's "MUST NOT be reused" would be quietly satisfiable again.
//
// THE ZERO VALUE IS NOT ONE OF THESE. NewProvisionalEpoch is the only thing that makes one, and a
// value that did not come from it holds no handle: every accessor refuses it for that reason rather
// than answering the zeros it happens to hold, and Destroy is a no-op on it rather than a nil
// dereference on the caller's cleanup path.
//
// It is NOT safe for concurrent use and is not meant to be. A commit is built on one goroutine --
// GroupSession's, per spec A section 3.6 -- and a lock here would buy nothing except the appearance
// of one.
type ProvisionalEpoch struct {
	// the MLS surface whose staged epoch is the "TreeKEM path secrets" half of step 1's list.
	// Destroy calls ClearPendingCommit on it, from inside, so the two halves cannot be
	// destroyed apart. It is the handle and not a cached exporter output: env_key[k] belongs to
	// an epoch that may already be open and is not this value's to destroy.
	handle GroupHandle
	// n+1, the epoch this state was built FOR and which it may never reach. It is held so a
	// caller holding two of these cannot confuse them, and so a fan out publishing under it does
	// not have to ask the handle -- whose own epoch is still n.
	epoch uint64
	// storage_root[n+1]: step 1, and the root every class key of the unborn epoch hangs off.
	storageRoot []byte
	// write_key[n+1]: step 1. The server installs it from the commit's attachment, so a
	// committer whose commit lost holds the write key of an epoch the server keyed to somebody
	// else's commit.
	writeKey []byte
	// eph_root[n+1]: step 1, and the value MASTER section 8.1's disappearing message promise is
	// about. Since the 2026-09-13 ruling it rides a second record from pq_secret, at a different
	// retention class, which changes nothing here and changes everything about how a half erase
	// looks: erasing one and leaving the other leaves half an epoch's delivery material alive,
	// and the surviving half is the one section 8.1 promised would become undecryptable.
	ephRoot []byte
	// pq_secret[n+1]: step 1, and step 2's "MUST NOT be reused" is the reason the accessor
	// refuses after Destroy rather than merely returning zeros. It was encapsulated to a ratchet
	// tree that no longer exists; carrying it into the real epoch n+1 binds one PQ secret across
	// two distinct epochs and breaks MASTER section 7's per epoch independence.
	pqSecret []byte
	// "and every X-Wing wrap it built": step 1's last clause. They are sealed bytes rather than
	// secrets, and they are erased anyway, because a wrap built to a tree that no longer exists
	// names a wrap_target_handle of an epoch nobody entered and can only ever mislead a reader.
	//
	// It is WRITE ONCE, which is InstallWraps's whole shape and is not a style choice: a second
	// install over a live set would drop the first fan out's wraps on the floor unerased, and
	// connect/mls's TestEveryPathThatDropsHeldKeyMaterialErasesItFirst derives that hazard off
	// this package's source and refuses it. It is the discipline task 6 gives stream_index for
	// the same reason -- a value that may be written twice is a value that will be.
	wraps [][]byte
	// G10's "there is no path that reads it afterwards", as a value rather than as a sentence.
	// Every accessor is closed off this flag, and task 15 property 6 derives the class of in
	// package readers off the syntax tree and holds each of them to it -- which it can do there
	// and could not do here, because at this commit that class is empty.
	destroyed bool
}

// NewProvisionalEpoch takes the four secrets of section 5.12 step 1 and the handle whose staged
// commit completes them.
//
// Every width is checked here rather than at the first expansion that meets one, because these
// values arrive from four different derivations and a thirty one octet write_key produces a MAC
// that verifies against itself and against nothing else.
//
// The wraps are not a parameter: a committer builds them AFTER it has this value, so they arrive
// through InstallWraps. Handing an empty slice in the constructor and installing later is the same
// thing with one more argument to get wrong -- and it would make the field non nil, which is the
// value InstallWraps's write once refusal reads.
func NewProvisionalEpoch(handle GroupHandle, epoch uint64,
	storageRoot []byte, writeKey []byte, ephRoot []byte, pqSecret []byte) (*ProvisionalEpoch, error) {

	if handle == nil {
		return nil, fmt.Errorf("%w: the destructor clears its staged commit from inside", ErrNilGroupHandle)
	}
	for _, named := range []struct {
		what  string
		value []byte
	}{
		{what: "storage_root", value: storageRoot},
		{what: "write_key", value: writeKey},
		{what: "eph_root", value: ephRoot},
		{what: "pq_secret", value: pqSecret},
	} {
		if len(named.value) != PqSecretBytes {
			return nil, fmt.Errorf("%w: %s is %d octets and section 5.12 step 1's values are %d",
				ErrProvisionalEpochValue, named.what, len(named.value), PqSecretBytes)
		}
	}
	return &ProvisionalEpoch{
		handle:      handle,
		epoch:       epoch,
		storageRoot: storageRoot,
		writeKey:    writeKey,
		ephRoot:     ephRoot,
		pqSecret:    pqSecret,
	}, nil
}

// Epoch is n+1, the epoch this state was built for.
func (self *ProvisionalEpoch) Epoch() (uint64, error) {
	if err := self.unusable("epoch"); err != nil {
		return 0, err
	}
	return self.epoch, nil
}

// StorageRoot is storage_root[n+1], live rather than copied.
func (self *ProvisionalEpoch) StorageRoot() ([]byte, error) {
	if err := self.unusable("storage_root"); err != nil {
		return nil, err
	}
	return self.storageRoot, nil
}

// WriteKey is write_key[n+1], live rather than copied.
func (self *ProvisionalEpoch) WriteKey() ([]byte, error) {
	if err := self.unusable("write_key"); err != nil {
		return nil, err
	}
	return self.writeKey, nil
}

// EphRoot is eph_root[n+1], live rather than copied.
func (self *ProvisionalEpoch) EphRoot() ([]byte, error) {
	if err := self.unusable("eph_root"); err != nil {
		return nil, err
	}
	return self.ephRoot, nil
}

// PqSecret is pq_secret[n+1], live rather than copied.
//
// This is the accessor G10 is named for. Section 5.12 step 2 forbids reusing this value across a
// lost commit in so many words, and the refusal below is what leaves a retry loop that tried to
// unable to: after Destroy there is no value to reuse and no door to ask for one through.
func (self *ProvisionalEpoch) PqSecret() ([]byte, error) {
	if err := self.unusable("pq_secret"); err != nil {
		return nil, err
	}
	return self.pqSecret, nil
}

// Wraps is every X-Wing wrap built for this unborn epoch so far.
func (self *ProvisionalEpoch) Wraps() ([][]byte, error) {
	if err := self.unusable("the X-Wing wraps"); err != nil {
		return nil, err
	}
	return self.wraps, nil
}

// InstallWraps hands over the X-Wing wraps the committer built for this unborn epoch, so that the
// destructor reaches them. It may be called once.
//
// WRITE ONCE, AND THE REFUSAL IS THE POINT. Section 5.11's fan out builds one set of wraps per
// epoch -- two device-wrap records per active device leaf plus the snapshot -- and a second install
// over a live set would drop the first set with nothing erasing it. That is the hazard
// connect/mls's TestEveryPathThatDropsHeldKeyMaterialErasesItFirst derives off this package's
// source, and it is a real one rather than a gate's opinion: the abandoned set still points at
// every wrap it held, and this type's whole promise is that those bytes stop existing together.
//
// WHAT IT COSTS, said rather than left to be found. A committer that dies part way through building
// its wraps has installed none, so the destructor reaches none of the ones it had built and they go
// out of scope unerased. The alternative -- appending each wrap as it is built -- trades that for a
// drop site on every call, which is the worse half of the same problem, and the caller that holds
// half a fan out is holding it in its own frame either way. Open item M1-20 records the choice.
//
// The slices are taken by reference and not copied, for the reason the constructor takes the four
// secrets that way.
func (self *ProvisionalEpoch) InstallWraps(wraps [][]byte) error {
	if err := self.unusable("the X-Wing wraps"); err != nil {
		return err
	}
	if len(wraps) == 0 {
		return fmt.Errorf("%w: an empty set is not an install", ErrProvisionalEpochWraps)
	}
	if self.wraps != nil {
		return fmt.Errorf("%w: %d are already installed for epoch %d", ErrProvisionalEpochWraps, len(self.wraps), self.epoch)
	}
	self.wraps = wraps
	return nil
}

// Destroyed answers G10's own question, and is the only accessor that still answers after Destroy.
//
// It exists so that an in package reader -- which reaches the fields directly and around every
// refusal above -- has a flag to check, and so that task 15 property 6's derived class of such
// readers has something to be held to.
func (self *ProvisionalEpoch) Destroyed() bool {
	return self.destroyed
}

// Destroy is section 5.12 step 1, entire.
//
// It erases the four secrets and every wrap, drops the slices, and calls ClearPendingCommit on the
// handle -- the TreeKEM path secrets half of step 1's list, which is the half connect/mls owns and
// the only half any document had a name for. The mls call is INSIDE here rather than beside it at
// the call site because a caller that has to remember two erasures will one day make one.
//
// It is idempotent: a second call is the same state as the first and must not clear a commit the
// group staged AFTER this state was destroyed, which is exactly what step 5's retry stages. It is
// also safe on the zero value, which has no handle to clear a commit on: a destructor is the one
// method a caller writes in a defer before the thing it destroys exists, so a destructor that
// panicked there would take the process down on the cleanup path of a failure it was cleaning up.
//
// The flag is set BEFORE anything else happens here, which is fail closed. What HOLDS that
// ordering is one observation and it is worth saying which: the only point this body can be
// interrupted at is the call out to foreign code at the end of it, since zeroize is this package's
// own leaf and cannot fail, and by then all four secrets are already zeros -- so
// epoch_test.go's TestAProvisionalEpochIsAlreadyRefusingWhenItCallsIntoTheGroupHandle fails a
// handle that finds this value still answering. Moving the assignment between two zeroize calls is
// not observable from anywhere and is not claimed to be.
//
// The noinline directive is this package's erase helper class, reached through the hand off to
// zeroize: the stores it makes are into arrays that outlive this call, and a compiler that inlined
// it could prove those writes dead.
//
//go:noinline
func (self *ProvisionalEpoch) Destroy() {
	if self.destroyed {
		return
	}
	self.destroyed = true
	zeroize(self.storageRoot)
	zeroize(self.writeKey)
	zeroize(self.ephRoot)
	zeroize(self.pqSecret)
	for _, wrap := range self.wraps {
		zeroize(wrap)
	}
	self.storageRoot = nil
	self.writeKey = nil
	self.ephRoot = nil
	self.pqSecret = nil
	self.wraps = nil
	if self.handle != nil {
		self.handle.ClearPendingCommit()
	}
}

// unusable is the one door check every accessor above runs, and it answers two conditions rather
// than one because both of them are "this value has nothing to tell you".
//
// The first is G10's: the destructor has run. The second is the ZERO VALUE, and it is here because
// the alternative was worse than it looks. NewProvisionalEpoch refuses a nil handle, so a value
// holding none was never constructed -- `var value ProvisionalEpoch`, or a deferred Destroy written
// above a construction that then failed -- and every field of it is the zero one. Without this
// check PqSecret on such a value answers a nil secret and NO error, which is precisely the shape
// ErrProvisionalEpochDestroyed's own doc comment names: a caller that seals under the zeros it was
// handed rather than stopping at a refusal. One sentinel covers both because a caller's question is
// the same in both cases and it matches it with one errors.Is.
func (self *ProvisionalEpoch) unusable(what string) error {
	if self.handle == nil {
		return fmt.Errorf("%w: %s of a zero valued provisional epoch, which NewProvisionalEpoch did not make",
			ErrProvisionalEpochDestroyed, what)
	}
	if self.destroyed {
		return self.refuse(what)
	}
	return nil
}

// refuse is G10's typed refusal, with the field named so a caller reading a log knows which door it
// tried rather than only that a door was shut.
func (self *ProvisionalEpoch) refuse(what string) error {
	return fmt.Errorf("%w: %s of the provisional epoch %d", ErrProvisionalEpochDestroyed, what, self.epoch)
}
