// The two per epoch keys the MESSAGE SERVER's two authenticators are taken under, handed out of a
// session as one value that carries its own erase.
//
// WHY A TYPE AND NOT TWO RETURN VALUES. A method answering (readKey, writeKey []byte, err error)
// hands a caller two live secrets under no name: nothing will ever erase them, and nothing can be
// asked to, because there is no receiver to hang a Destroy on. A type carries the erase obligation
// as a method a caller writes in a defer, which is the same answer ProvisionalEpoch gives to the
// same question one epoch over, and it is why this file exists rather than two more results on a
// signature in session.go.
//
// WHY THIS HOLDS COPIES WHERE ProvisionalEpoch HOLDS LIVE ARRAYS, which is a divergence from the
// sibling type and therefore owes a reason rather than a preference. A ProvisionalEpoch is built by
// one goroutine and read by that goroutine, and nothing else owns its arrays, so a live answer
// costs nothing. A GroupSession's write_key and read_key are written AND ERASED by the loop
// goroutine: installEpochOnLoop zeroizes both immediately before overwriting them at every
// AdvanceEpoch, and zeroizeOnLoop zeroizes both at Close. A value that held the session's own
// headers would be a window onto a field another goroutine writes, with no happens-before between
// the read and the write -- a data race in the exact place spec A section 3.6 built a command loop
// to make impossible. So the copy is derived from the concurrency contract.
//
// AND THE COPY COSTS SOMETHING, which is said here rather than left to be found: a value outlives
// the epoch it came from. A caller holding one across a commit is holding the PREVIOUS epoch's two
// keys, and the server resolves a write key for the current epoch plus a short lived predecessor
// and nothing older, so a submit under a stale one is refused rather than mis-attributed. Open
// item K1-5 records it.
//
// WHAT THE ACCESSORS HAND OUT IS THIS VALUE'S OWN ARRAY, live, and that is the ProvisionalEpoch
// shape kept on purpose. An accessor that answered a fresh copy per call would make Destroy
// useless: every read would leave a copy on the heap that no erase reaches, which is the orphan
// arm this type exists to close. The header a caller holds is therefore erased by the caller's own
// deferred Destroy, and that is the whole promise.
//
// WHERE THIS SITS AGAINST GUARDRAIL G6. G6 says epoch_secret is never returned by any exported
// symbol, and the line this file has to stay on the right side of is drawn several derivations
// further down than epoch_secret itself. What is handed out here is write_key[n] and read_key[n],
// each HKDF-Expand(storage_root[n], "write/v1" | "read/v1", 32) -- one expansion below
// storage_root, which is HKDF-Extract(mls_secret[n], pq_secret[n]), which is below the exporter
// output, which is below epoch_secret. STORAGE_ROOT IS NOT ON THIS TYPE and no accessor answers
// it: a caller handed the root could expand every class key, every record key ladder and
// group_handle_key from it, which is the whole client half of the record layer, whereas the two
// keys here are exactly the pair the message server authenticates on and can do nothing else with.
// The two the session already holds move; the parent they were expanded from does not.
package messagegroup

import "fmt"

// EpochKeys is one epoch's write_key and read_key, with the epoch number they belong to.
//
// It is NOT safe for concurrent use and is not meant to be: one caller opens it, spends it and
// destroys it. The session it came from is the value that is safe for concurrent use.
type EpochKeys struct {
	// Set by newEpochKeys and by nothing else, which is what tells a value that was MADE from
	// the zero value. It cannot be read off the two keys, because a destroyed value has nil in
	// both of them and the two cases owe different sentences.
	built bool
	// G10's flag, in the shape ProvisionalEpoch uses.
	destroyed bool
	// The epoch these two were derived for. It is answered rather than assumed because the whole
	// use of the value is telling two epochs apart: a submit under the previous epoch's write key
	// is refused by the server, and the number is what a caller compares before it spends one.
	epoch uint64
	// HKDF-Expand(storage_root[n], "read/v1", 32), which req_auth is macced under.
	readKey []byte
	// HKDF-Expand(storage_root[n], "write/v1", 32), which write_auth is macced under.
	writeKey []byte
}

// newEpochKeys copies the two keys out of whatever held them.
//
// IT COPIES, and the copy is the point of the constructor rather than a detail of it: the one
// caller is GroupSession.EpochKeys, running on the loop goroutine, and what it passes are the
// session's own fields -- the two arrays the next AdvanceEpoch zeroizes in place. A constructor
// that took the headers would hand its caller a window onto them.
//
// It is unexported because there is exactly one legal source for these octets, and a caller that
// could build one from octets of its own would be building a value that says "the session held
// this" about octets no session ever held.
func newEpochKeys(epoch uint64, readKey []byte, writeKey []byte) *EpochKeys {
	return &EpochKeys{
		built:    true,
		epoch:    epoch,
		readKey:  append([]byte(nil), readKey...),
		writeKey: append([]byte(nil), writeKey...),
	}
}

// Epoch is the epoch number the door was opened at.
func (self *EpochKeys) Epoch() (uint64, error) {
	if err := self.unusable("the epoch"); err != nil {
		return 0, err
	}
	return self.epoch, nil
}

// ReadKey is read_key[n], which every request this member macs under req_auth is authenticated by.
func (self *EpochKeys) ReadKey() ([]byte, error) {
	if err := self.unusable("read_key"); err != nil {
		return nil, err
	}
	return self.readKey, nil
}

// WriteKey is write_key[n], which every record this member submits is authenticated by.
func (self *EpochKeys) WriteKey() ([]byte, error) {
	if err := self.unusable("write_key"); err != nil {
		return nil, err
	}
	return self.writeKey, nil
}

// unusable is the one door check every accessor above runs, and it answers TWO conditions because
// both of them are "this value has nothing to tell you".
//
// The first is the ZERO VALUE. newEpochKeys is the only constructor, so a value holding nothing was
// never made by one -- var keys EpochKeys, or a deferred Destroy written above a construction that
// then failed -- and without this arm ReadKey on such a value answers a nil key and NO error, which
// is a caller maccing a request under nothing and reading the refusal as a server problem. The
// second is G10's: the destructor has run. One sentinel covers both because a caller's question is
// the same in both cases and it matches it with one errors.Is. This is ProvisionalEpoch.unusable's
// reasoning applied to the sibling type rather than restated as a new decision.
func (self *EpochKeys) unusable(what string) error {
	if !self.built {
		return fmt.Errorf("%w: %s of a zero valued EpochKeys, which GroupSession.EpochKeys did not make",
			ErrEpochKeysDestroyed, what)
	}
	if self.destroyed {
		return fmt.Errorf("%w: %s of the epoch keys of epoch %d", ErrEpochKeysDestroyed, what, self.epoch)
	}
	return nil
}

// Destroy erases both keys in place and shuts every door above.
//
// It is idempotent, and it is safe on the zero value, for the reason ProvisionalEpoch.Destroy is:
// a destructor is the one method a caller writes in a defer ABOVE the construction it is
// destroying, so a destructor that panicked there would take the process down on the cleanup path
// of the failure it was cleaning up.
//
// The flag is set BEFORE the erase, which is fail closed, and it is what makes the second call a
// no-op rather than a second walk over two nil slices.
//
// The noinline directive is this package's erase helper class, reached through the hand off to
// zeroize: the stores land in arrays that outlive this call -- the caller still holds the headers
// every accessor answered -- and a compiler that inlined this body could prove those writes dead.
// zeroize.go's own comment is the argument and zeroize_test.go derives the class off the source.
//
//go:noinline
func (self *EpochKeys) Destroy() {
	if self.destroyed {
		return
	}
	self.destroyed = true
	zeroize(self.readKey)
	zeroize(self.writeKey)
	self.readKey = nil
	self.writeKey = nil
}
