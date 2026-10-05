// The record layer's key schedule: the storage root every group key of an epoch hangs off, and
// the three retention class keys derived from it.
//
// Spec A section 5.3 is normative and its block is this file's contract:
//
//	storage_root[n] = HKDF-Extract(salt = mls_secret[n], ikm = pq_secret[n])   MASTER section 7
//
//	mls_secret[n] = MLS-Exporter("URmessage/v1/storage", "", 32)   RFC 9420 section 8.5
//
//	NOTE the argument order. crypto/hkdf's Extract takes (secret, salt) -- ikm FIRST.
//	This wrapper takes (salt, ikm), matching the spec text. Never call
//	crypto/hkdf's Extract directly anywhere in this package. See section 5.9.
//
// Guardrail G1 is that note, and it is the reason this file has a helper where a one line body
// would do. crypto/hkdf's Extract takes the input keying material first and the salt second,
// which is the reverse of the HKDF-Extract(salt, ikm) MASTER, RFC 9420 and RFC 9180 all write.
// Transposing them compiles, returns thirty two bytes, and passes every test that does not
// compare against an implementation written by somebody else -- both ends of this project would
// agree with themselves forever, and only a second client would ever find out.
//
// So the extraction is done in ONE unexported helper and that helper delegates to
// mls.CryptoProvider.Extract, which already takes the arguments in the spec's order, is already
// the tree's one reviewed swap, and is already held against RFC 5869's own table. That is open
// item M1-16's recommended shape and it is the cheaper of the two: the alternative was to call
// crypto/hkdf here and take an allow list entry in mls's forbidden gate, which is keyed by
// needle and would have excused Extract, Expand AND Key in this file -- and Key is the worst of
// the three to transpose, because the whole schedule it produces is internally consistent and
// wrong. Taking the delegation leaves the tree with exactly one direct extraction in the whole
// crypto surface and needs no widening of anything. If M1-16 is ever ruled the other way, the
// change is inside keyScheduleExtract and nowhere else.
//
// The expansion is one helper for the same reason at lower stakes: Expand has no salt argument
// and so carries no transposition, but a second spelling of it is a second place for a length or
// an info conversion to drift. Every derivation in this package that expands goes through it.
//
// ClassKeys carries three keys and its SHAPE is the second defence. Spec A section 5.3:
// "Eph is NOT here. eph_root is 32 B fresh CSPRNG at commit, never derived from storage_root.
// MASTER I4. Putting it in this struct would make the wrong thing the easy thing." An eph key
// derived from the storage root would be recoverable from it forever, which is the whole
// property the eph classes exist to not have, and the struct that has no field for it is what
// makes the correct thing the only thing in reach.
//
// The three labels are three constants and are never built by substituting a word into a shared
// stem. writeauth.go already sets that precedent for write/v1 and read/v1 and gives the
// argument: one construction is a single edit away from making two keys equal, and two
// retention classes sharing a key is a permanent record openable with a media key.
package messagegroup

import (
	"fmt"

	"github.com/urnetwork/connect/mls"
)

// The three retention class labels, raw ascii, expanded from the storage root at thirty two
// octets each.
//
// They are deliberately not the same length, and the separation does not rest on that. It rests
// on the bytes: all three disagree at index zero, which is inside the shortest of them, so no
// choice of anything that follows one label can turn it into another.
const (
	permClassInfo    = "perm/v1"
	durableClassInfo = "durable/v1"
	mediaClassInfo   = "media/v1"
)

// The width of a class key and of the storage root, in octets. MASTER section 7 gives both as
// thirty two, and the exporter that produces mls_secret is called for thirty two.
const classKeyBytes = 32

// The provider every derivation in this package runs on.
//
// It is one value for the process because it is stateless: mls's own comment on
// NewCryptoProvider says so in as many words -- it holds the suite parameters and an entropy
// source, and both are safe to share -- and nothing here draws entropy through it at all.
//
// The suite it is built for does not enter any derivation below. Extract and Expand are written
// against sha256 directly in mls's provider rather than selected from the suite parameters, and
// both registered suites name HKDF-SHA256; the guard for a third suite that did not is mls's
// own TestEverySuiteNamesTheHashTheProviderComputes, which reads the registry rather than this
// file. The suite is named here only because the constructor takes one.
//
// A failure here is an unregistered code point, which is a compile time constant of this file
// being wrong, so it stops the process at initialisation rather than handing every later
// derivation a nil provider.
var keyScheduleCrypto = mustKeyScheduleCrypto()

// The provider, or a stopped process. Split out so the var above has no function literal in it
// and reads as what it is.
func mustKeyScheduleCrypto() mls.CryptoProvider {
	crypto, err := mls.NewCryptoProvider(mls.CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		panic(fmt.Errorf("messagegroup: the key schedule's crypto provider names an unregistered suite: %w", err))
	}
	return crypto
}

// The ONE extraction of this package, and guardrail G1's single reviewed call site.
//
// The argument names are the spec's and the delegation preserves them: mls's Extract is declared
// Extract(salt, ikm) precisely so that a call reading the way the spec reads is the correct
// call. Nothing else in this package may extract, and keyschedule_test.go derives that class off
// the syntax tree rather than trusting this sentence.
func keyScheduleExtract(salt []byte, ikm []byte) []byte {
	return keyScheduleCrypto.Extract(salt, ikm)
}

// The ONE expansion of this package.
//
// It exists so that a length, an info conversion or a provider is never spelled twice, and so
// that every derivation this package publishes can be read as one line naming its own label.
// mls's Expand panics on a length outside the KDF's range rather than returning a short key,
// which is the answer this layer wants too: every length below is a compile time constant of
// this file.
func keyScheduleExpand(prk []byte, info []byte, length int) []byte {
	return keyScheduleCrypto.Expand(prk, info, length)
}

// StorageRoot derives storage_root[n], the root every other key of an epoch is expanded from.
//
// The first argument is the SALT and the second is the input keying material, in that order,
// because that is the order MASTER section 7 and spec A section 5.3 write and because the whole
// of guardrail G1 is that the library writes them the other way round. mls_secret comes from
// the group's MLS exporter and pq_secret from the epoch's post quantum contribution; a
// transposition here produces a root that is thirty two well formed octets, that both ends of
// one implementation agree on, and that no second implementation ever reproduces.
//
// There is no refusal and no error: it is a pure function of two secrets its caller has already
// obtained, and the one thing that could go wrong -- an aged out epoch that cannot export -- is
// reported by mls's exporter one level above this signature.
func StorageRoot(mlsSecret []byte, pqSecret []byte) []byte {
	return keyScheduleExtract(mlsSecret, pqSecret)
}

// ClassKeys holds one key per retention class that a record can be written under.
//
// There are three and there is no fourth. The eph classes are absent on purpose and the absence
// is the point -- MASTER I4 and spec A section 5.3 -- because eph_root is fresh entropy at each
// commit and is never a function of the storage root. A field here for it would make deriving
// the wrong thing the shortest path to a compiling program.
type ClassKeys struct {
	// Permanent records: kept until the group deletes them.
	Perm []byte
	// Durable records: the class an ordinary message is written under, and the class CP3b's
	// one message travels in.
	Durable []byte
	// Media records: the class a blob's inline metadata is written under.
	Media []byte
}

// Zeroize erases all three keys in place.
//
// It is owed because this type is storage rather than an answer: a session holds one for the
// epoch it is in and drops it at the next commit, and the octets it drops are what forward
// secrecy is about. Every field is erased by name rather than by a loop over a slice of them,
// so a fourth field added without an erase beside it leaves this method visibly short.
//
// The noinline directive is this package's erase helper class, and this method is a member of
// it through the HAND-OFF rather than through a write it spells: the three arrays outlive the
// call, and zeroize is where the stores are. connect/mls settled that class boundary first and
// argued it at length -- "a body that hands the caller's array to an eraser has erased it just
// as surely as one that spells the loop, and in this package that is how erasure is nearly
// always written" -- and this method was outside the copy of that rule this package shipped
// until zeroize_test.go's gate was re-derived from the property rather than from the one loop
// this package happens to spell.
//
//go:noinline
func (self *ClassKeys) Zeroize() {
	if self == nil {
		return
	}
	zeroize(self.Perm)
	zeroize(self.Durable)
	zeroize(self.Media)
}

// DeriveClassKeys expands the three retention class keys from a storage root.
//
// Each is HKDF-Expand(storage_root, label, 32) under its own label, and the three labels are
// three separate constants for the reason the file comment gives.
func DeriveClassKeys(storageRoot []byte) *ClassKeys {
	refuseWrongWidthStorageRoot(storageRoot)
	return &ClassKeys{
		Perm:    keyScheduleExpand(storageRoot, []byte(permClassInfo), classKeyBytes),
		Durable: keyScheduleExpand(storageRoot, []byte(durableClassInfo), classKeyBytes),
		Media:   keyScheduleExpand(storageRoot, []byte(mediaClassInfo), classKeyBytes),
	}
}

// ---------------------------------------------------------------------------
// the record key ladder, spec A section 5.3 and MASTER section 8.1
// ---------------------------------------------------------------------------

// The four labels of the ladder, raw ascii, four constants and not one stem with a word
// substituted into it.
//
// The last two are the pair this rule exists for. "rec/v1/head" and "rec/v1/body" share an
// eleven character prefix and differ in their final four octets, which makes them the most
// concatenation prone pair in the whole schedule: a single construction with the tail
// substituted is one edit away from handing the head and the body one key, and a record whose
// two ciphertexts are sealed under one key and one nonce is a record whose Poly1305 one time
// key an attacker recovers. The first two are separated by their first octet, which is inside
// the shorter of them, so nothing following one can turn it into the other.
// The head bind label is the FIFTH and arrived with MASTER section 8.4.2 v2 on 2026-09-17. It
// stays at v1 because it names a derivation off the v1 record ladder -- the rung is unchanged and
// the ladder is unchanged; what is new is a fourth thing expanded from a rung that already produces
// three. It shares the "rec/v1/" stem with the two above and is separated from both by its length
// and by its ninth octet, and like them it is a whole constant rather than a stem with a word
// appended.
const (
	recordKeyZeroInfo  = "sender/v1"
	recordKeyNextInfo  = "ratchet/v1"
	recordAeadHeadInfo = "rec/v1/head"
	recordAeadBodyInfo = "rec/v1/body"
	recordHeadBindInfo = "rec/v1/head-bind"
)

// The width of one rung of the ladder, and the width of the material one rung expands into.
//
// The fifty six is DERIVED and never written. MASTER section 8.1's block gives 56 for
// key_head | nonce_head, and 56 is 32 + 24 -- the aead's key size and the extended nonce
// XChaCha20-Poly1305 takes. Writing the number down would put this file's opinion of the
// primitive beside the primitive: a suite change would silently truncate the nonce by twelve
// octets and every record would still round trip against itself. Written as the sum, the same
// change is a compile error at recordaead.go, which is where the primitive is.
const (
	recordKeyBytes          = 32
	recordAeadMaterialBytes = recordAeadKeyBytes + recordAeadNonceBytes
)

// RecordKeyZero derives record_key[0], the head of one sender's ladder for one retention class.
//
//	record_key[0] = HKDF-Expand(class_key, "sender/v1" | LP(leaf_index), 32)
//
// The leaf goes through leafLabelledInfo and so through leafIndexLP, which is the one reading
// of LP(leaf_index) this package has: sender_handle is derived from the same shape one file
// over and the two must not be able to drift apart. Open item M1-8 is the ruling on what LP of
// an integer means, and when it lands it is a single edit inside that helper.
//
// The class key is what separates one sender's ladders from each other -- section 5.5 sizes the
// skipped key window per (sender_handle, retention class) for exactly this reason -- so a wrong
// width one is refused rather than expanded. There is no error to return in the signature spec A
// section 5.3 publishes, so the refusal is a panic carrying the sentinel, which is the shape
// SenderHandle and writeauth.go's computing half already use: nothing here is reachable from the
// network, the class key is this member's own derivation, and a ladder built on a truncated key
// is a ladder no peer reproduces.
func RecordKeyZero(classKey []byte, leaf uint32) []byte {
	refuseWrongWidthClassKey(classKey)
	return keyScheduleExpand(classKey, leafLabelledInfo(recordKeyZeroInfo, leaf), recordKeyBytes)
}

// RecordKeyNext derives record_key[i+1] from record_key[i].
//
//	record_key[i+1] = HKDF-Expand(record_key[i], "ratchet/v1", 32)
//
// It does not erase its input. The forward secrecy of the ladder is the CALLER's erasure of the
// rung it has finished with -- SenderRatchet.Next is where that happens and where section 5.5
// puts it -- because this function is also how a receiver walks forward over rungs it must
// RETAIN, and an erasure here would blank the skipped key window as it filled it.
func RecordKeyNext(recordKey []byte) []byte {
	refuseWrongWidthRecordKey(recordKey)
	return keyScheduleExpand(recordKey, []byte(recordKeyNextInfo), recordKeyBytes)
}

// RecordAeadHead derives the key and nonce ct_head is sealed under.
//
//	key_head | nonce_head = HKDF-Expand(record_key[i], "rec/v1/head", 56)
//
// THE ARGUMENT IS THE RECORD'S OWN LADDER, AT THE RECORD'S OWN POSITION, FOR EVERY CLASS. Ruled
// 2026-09-13 (ledger items 152 and 128, spec A revision A-25), and it REVERSES the ruling of
// 2026-09-07. ct_head is keyed under the record's own class key exactly as ct_body is, so head
// and body take ONE ladder at ONE position and are separated only by their HKDF labels,
// "rec/v1/head" against "rec/v1/body" -- which is what MASTER invariant I7's "distinct keys and
// distinct AADs" has always meant and is why one position is safe.
//
// WHAT THIS COMMENT SAID UNTIL THEN, because a reversal that erases what it reverses leaves the
// next reader unable to reconstruct it, and because what it said is the reading a builder would
// otherwise transcribe. It called this a contradiction the function does not resolve: MASTER
// section 8.1 said one line after the ladder that ct_head is always under the durable class since
// it is always retained, while spec A section 5.3 gave this function and RecordAeadBody the same
// record_key[i]; for a DURABLE record the two coincide, and for a PERMANENT, MEDIA or EPH record
// they were two rungs of two different class ladders, with WHICH rung each half takes left to
// m1 open item M1-6 -- ruled 2026-09-07, reversed 2026-09-13.
// That reading is GONE. Its premise -- the head is always retained -- is false
// for exactly one class and it is the class the question was about: spec B section 7.2 sets
// ct_head = NULL for EPH(1..5) at prune_after.
//
// THE SIGNATURE STILL CANNOT EXPRESS IT AND THE CALL SITE STILL BINDS IT. This function takes a
// thirty two octet secret and so does RecordAeadBody, so nothing here can check that the two were
// handed the same rung of the right ladder; SealRecord and OpenRecord are where that is decided
// and seal.go's decision (a) is where it is argued. What a caller can now get wrong in silence is
// the INVERSE of what it was: passing the durable ladder's record_key to this function for a
// non-DURABLE record is a working program that seals an EPH header under a key nothing destroys.
//
// The class blind counter is untouched and is still needed. Ruling A1 of 2026-09-07 -- ledger
// items 143 and 169 -- makes i = stream_index in every ladder over one counter per
// (group_id, sender_handle), and its load bearing case is the device wrap's classless shared
// root, which this reversal does not reach.
func RecordAeadHead(recordKey []byte) (key []byte, nonce []byte) {
	return recordAeadMaterial(recordKey, recordAeadHeadInfo)
}

// RecordAeadBody derives the key and nonce ct_body is sealed under.
//
//	key_body | nonce_body = HKDF-Expand(record_key[i], "rec/v1/body", 56)
//
// It is a call to the same helper under the OTHER label and not a call to RecordAeadHead: the
// whole separation between a record's two ciphertexts is that the two labels are two constants,
// and a body that reached the head's derivation would be a body one edit away from sealing both
// halves of a record under one key and one nonce.
func RecordAeadBody(recordKey []byte) (key []byte, nonce []byte) {
	return recordAeadMaterial(recordKey, recordAeadBodyInfo)
}

// headCommit is MASTER section 8.4.2 v2's fourth term of the aad_mls preimage: the KEYED
// commitment to a record's head plaintext that the sender's own MLS signature then covers.
//
//	head_bind_key = HKDF-Expand(record_key[i], "rec/v1/head-bind", 32)
//	head_commit   = HMAC-SHA-256(head_bind_key, head_plain)
//
// WHAT IT CLOSES, which is a measured attack and not a tidiness. ct_head is sealed under the same
// record_key[i] EVERY member derives, and before v2 no field of the inner frame covered what the
// head said. So a member could lift another member's GENUINE body -- frame, signature and all --
// and re-issue it at the SAME position under a head of its own writing: R1 passed because the frame
// really was that member's, R2 passed because the position really was that record's, and the record
// opened to the true sender's plaintext under an attacker's head. The head is not decoration: it
// carries sent_at, which is what a conversation is ordered by. And the substitute was ACCEPTED, so
// it spent the rung and the genuine record at that index then could not open at all. That is open
// item MG-6 and ledger item 204, and this is the derivation that closes both.
//
// IT IS KEYED AND NEVER A BARE H(head_plain), AND THE REASON IS A LEAK RATHER THAN A PREFERENCE.
// AAD_body is public to the server and aad_mls travels IN THE CLEAR as the frame's
// authenticated_data, so an unkeyed commitment to a nine octet head whose only variable is a
// millisecond timestamp is a few million guesses -- which would hand the SERVER a confirmable
// sent_at, the one clock value the record layer deliberately keeps inside an AEAD. record_key[i] is
// the key because both sides hold it at the right moment and no non-member ever does.
//
// AND THAT CLAUSE DEFENDS NOTHING ANY REFUSAL CAN SEE, which is said here rather than left to be
// discovered: replacing the HMAC with a bare SHA-256 of head_plain changes no refusal anywhere and
// turns nothing red. MASTER section 8.4.3's mutation (f) names that as the expected answer. The
// clause is held by this paragraph and by the KAT beside it, not by a case.
//
// IT IS NOT CIRCULAR, and that is what separates it from AAD_head, which cannot be bound in any
// form. The sealer is handed head_plain as an argument and frames the body AFTER the rung exists
// and BEFORE ct_head is sealed; the opener opens ct_head ABOVE the point where it unframes the
// body. Both sides therefore hold both inputs at the moment they need the digest. AAD_head contains
// body_hash = H(ct_body) and ct_body is sealed over the very frame the aad would sit in, which is a
// cycle rather than an ordering.
//
// head_plain is passed through with NO length prefix, no padding, no re-encoding and no
// canonicalisation: it is the same array that is sealed into ct_head, of whatever length including
// zero. A second encoding here would be a second opinion about what the head is.
func headCommit(recordKey []byte, headPlain []byte) [32]byte {
	refuseWrongWidthRecordKey(recordKey)
	bindKey := keyScheduleExpand(recordKey, []byte(recordHeadBindInfo), recordKeyBytes)
	defer zeroize(bindKey)
	return [32]byte(keyScheduleCrypto.Mac(bindKey, headPlain))
}

// The one expansion the two aead derivations share, so the split of the fifty six octets is
// stated once.
//
// The key is the FIRST thirty two octets and the nonce is the last twenty four, which is the
// order MASTER section 8.1 writes them in -- key_head | nonce_head -- and a transposition
// produces two values of the right widths that seal and open against themselves and against
// nothing else.
//
// Both halves are cut with their capacity pinned to their own length. Without that an append to
// the key would write into the nonce's octets, which is a defect that shows up as a record that
// does not open on the OTHER side of a wire and never here.
func recordAeadMaterial(recordKey []byte, info string) (key []byte, nonce []byte) {
	refuseWrongWidthRecordKey(recordKey)
	material := keyScheduleExpand(recordKey, []byte(info), recordAeadMaterialBytes)
	return material[:recordAeadKeyBytes:recordAeadKeyBytes], material[recordAeadKeyBytes:recordAeadMaterialBytes:recordAeadMaterialBytes]
}

// The class key width refusal, in one place so the ladder's head cannot disagree with a later
// caller about what a class key is.
func refuseWrongWidthClassKey(classKey []byte) {
	if len(classKey) != classKeyBytes {
		panic(fmt.Errorf("%w: %d octets, want %d", ErrClassKeyLength, len(classKey), classKeyBytes))
	}
}

// The record key width refusal, made by every function that takes a rung of the ladder.
//
// It is checked and not assumed even though every rung this package produces is thirty two
// octets by construction: the exported signatures take a []byte from a caller, and a short one
// expands to a well formed key and a well formed nonce that no peer computes.
func refuseWrongWidthRecordKey(recordKey []byte) {
	if len(recordKey) != recordKeyBytes {
		panic(fmt.Errorf("%w: %d octets, want %d", ErrRecordKeyLength, len(recordKey), recordKeyBytes))
	}
}
