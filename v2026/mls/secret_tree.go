// the RFC 9420 section 9 secret tree: the per-sender key material of one epoch, and the
// deletions that make it forward secret.
//
// The tree is walked lazily and destructively. encryption_secret sits at the root, and a
// node secret is expanded into its two children and then erased, so the one value that
// could regenerate a whole subtree stops existing the moment any leaf under it has been
// reached. That erasure is the entire point of the section: every correctness question
// ("what is this leaf's secret?") is answered identically by an implementation that keeps
// every parent forever, and the only question that separates the two is what an attacker
// who takes the process a second later can still derive.
//
// Two things follow, worth stating because neither is obvious from the derivation alone.
// First, deletion is downward-closed but derivation is downward-only: from the secrets that
// remain, no expansion sequence reaches a secret that has been deleted, which is why "take
// the leaf, then erase the path above it" is enough. Second, taking a leaf twice is refused
// rather than answered, because answering it would mean the tree had kept a value it
// promised to destroy.
//
// The tree is always full. RFC 9420 section 7.7 makes a valid leaf count a power of two,
// and p3's Root refuses any other, so every parent reached here has both of its children
// inside the node array.
package mls

import (
	"fmt"
	"sync"
)

// SecretTree holds the node secrets of one epoch that no leaf has consumed yet.
//
// stateLock guards nodes, ratchets and erased, which are the mutable fields. crypto,
// leafCount, width, depth and root are written once by the constructor and read without it,
// which is what lets an exported method hold the lock and still read the count. Every
// unexported helper in this file assumes its caller holds stateLock; every exported entry
// point that touches the mutable state takes it. LeafCount deliberately does NOT take it, so
// a later exported method can report the count while holding it without deadlocking on its
// own accessor.
//
// erased is set by Zeroize and is checked before any derivation. It is state and not a
// convenience: once the secrets have been overwritten, every node secret and every ratchet
// secret is a run of Nh zero bytes, and a derivation from those is a value any party can
// compute. See ratchetFor.
type SecretTree struct {
	stateLock sync.Mutex
	crypto    CryptoProvider
	leafCount LeafCount
	width     uint32
	depth     uint32
	root      NodeIndex
	nodes     map[NodeIndex][]byte
	ratchets  map[ratchetKey]*ratchet
	erased    bool
}

// NewSecretTree seeds the tree with encryption_secret at the root.
//
// leafCount is a LeafCount and not a LeafIndex (registry override O-4). Both are uint32
// underneath, so the compiler cannot separate them at a call site that passes the wrong
// one, and the wrong one here is the off-by-one that makes the last member of the group
// unreachable while every other leaf still answers.
//
// Root is two-valued and its error is handled rather than shimmed away (convention C3). A
// shim that turned it into node zero would build a tree whose descent starts at a leaf and
// never terminates, and would accept a leaf count of zero and of three. Its sentinel is
// wrapped alongside this package's own, so a caller can ask either "is this a secret tree
// range failure" or "was the leaf count not a power of two" and get the true answer.
func NewSecretTree(crypto CryptoProvider, leafCount LeafCount, encryptionSecret []byte) (*SecretTree, error) {
	if crypto == nil {
		return nil, fmt.Errorf("%w: the secret tree derives every node secret through it", ErrNilCryptoProvider)
	}
	// measured redundant, and kept anyway: Root refuses zero with ErrLeafCountRange and this
	// constructor wraps that in the same ErrSecretTreeLeafOutOfRange, so deleting these three
	// lines leaves every test in this package passing. It stays for the message — "leaf count
	// is zero" names the caller's mistake and "leaf count out of range" names the library's
	// view of it — and because the check belongs to the precondition this function states.
	if leafCount == 0 {
		return nil, fmt.Errorf("%w: leaf count is zero", ErrSecretTreeLeafOutOfRange)
	}
	// the length is the provider's hash size and never a written down 32. Both registered
	// suites fix Nh at 32, so a literal here agrees with every vector in this tree and
	// silently disagrees with the five registered suites at Nh 48 and 64.
	if len(encryptionSecret) != crypto.HashSize() {
		return nil, fmt.Errorf("%w: encryption secret is %d bytes, want %d",
			ErrSecretLength, len(encryptionSecret), crypto.HashSize())
	}
	root, err := Root(leafCount)
	if err != nil {
		return nil, fmt.Errorf("%w: leaf count %d: %w", ErrSecretTreeLeafOutOfRange, leafCount, err)
	}
	width := NodeWidth(leafCount)
	if width == 0 {
		// unreachable behind Root, which refuses every count NodeWidth answers zero for.
		// It is kept because this constructor should enforce its own precondition rather
		// than inherit it from the function it happens to call first, and because a width
		// of zero would put every child of every parent outside the array.
		return nil, fmt.Errorf("%w: leaf count %d has no node array", ErrSecretTreeLeafOutOfRange, leafCount)
	}
	self := &SecretTree{
		crypto:    crypto,
		leafCount: leafCount,
		width:     width,
		depth:     TreeDepth(leafCount),
		root:      root,
		// the copy is deliberate: the tree erases what it holds, and erasing a caller's
		// slice in place would zero a secret the key schedule still owns.
		nodes: map[NodeIndex][]byte{root: append([]byte(nil), encryptionSecret...)},
		// created here rather than lazily: ratchetFor stores into it while holding the lock,
		// and a nil map would panic on the first sender rather than on a code path a test
		// happened to miss.
		ratchets: map[ratchetKey]*ratchet{},
	}
	return self, nil
}

// LeafCount is the number of leaves the tree was built for.
//
// No lock: leafCount is written once by the constructor. See the type's comment for why
// that is a requirement here rather than an optimisation.
func (self *SecretTree) LeafCount() LeafCount {
	return self.leafCount
}

// pathToLeaf returns the node indices from the root down to the leaf, inclusive.
//
// The array representation is in-order, so every index in a parent's left subtree is below
// the parent and every index in its right subtree is above it. That comparison is the whole
// descent rule and it needs no parent lookups; the children themselves come from p3's Left
// and Right, so the rule chooses the direction and tree math supplies the node.
//
// The loop is bounded by the tree's own depth rather than left to terminate on an argument
// about Level. It cannot run away for a validated index — Left and Right of a level k node
// are level k-1 — but a structural bound makes termination a property of the code, and the
// bound is derived from the leaf count, so a tree twice as deep walks exactly one step
// further.
//
// The caller holds stateLock, though this function reads no guarded state; it is documented
// that way so the whole unexported surface has one rule.
func (self *SecretTree) pathToLeaf(leaf LeafIndex) ([]NodeIndex, error) {
	if LeafCount(leaf) >= self.leafCount {
		return nil, fmt.Errorf("%w: leaf %d of %d", ErrSecretTreeLeafOutOfRange, leaf, self.leafCount)
	}
	target := leaf.NodeIndex()
	// LeafIndex.NodeIndex is total and wraps at 2^31, so this is what would catch a wrapped
	// index if the count check above ever stopped catching it first. Measured: behind that
	// check it is unreachable, and it is kept for the reason p3's DirectPath keeps a range
	// check Parent already makes — a function enforcing its own precondition rather than
	// inheriting it from the one it happens to call.
	if uint32(target) >= self.width {
		return nil, fmt.Errorf("%w: node %d of width %d", ErrSecretTreeLeafOutOfRange, target, self.width)
	}
	path := make([]NodeIndex, 0, self.depth+1)
	path = append(path, self.root)
	current := self.root
	for steps := uint32(0); current.Level() > 0; steps++ {
		if steps >= self.depth {
			return nil, fmt.Errorf("%w: descent from node %d ran past depth %d",
				ErrSecretTreeLeafOutOfRange, self.root, self.depth)
		}
		var next NodeIndex
		var err error
		if target < current {
			next, err = Left(current)
		} else {
			next, err = Right(current)
		}
		if err != nil {
			return nil, fmt.Errorf("%w: descent from node %d: %w", ErrSecretTreeLeafOutOfRange, current, err)
		}
		current = next
		path = append(path, current)
	}
	// measured redundant, and labelled here rather than defended silently: the loop leaves
	// current at a level 0 node, and for a full tree the comparison rule reaches the leaf it
	// was aimed at, so replacing this condition with a constant false leaves every test in
	// this package passing. What makes it redundant is asserted rather than argued --
	// TestSecretTreePathToLeafLandsOnTheTargetOfEveryLeafOfEveryWidth walks every leaf of
	// every swept width and holds the last node of the path to leaf.NodeIndex(). The check
	// stays because a descent that went wrong and a missing landing check are two separate
	// edits, and this is the one that turns the first into a refusal rather than into
	// another leaf's secret handed to the wrong sender.
	if current != target {
		return nil, fmt.Errorf("%w: descent reached node %d, want %d",
			ErrSecretTreeLeafOutOfRange, current, target)
	}
	return path, nil
}

// takeLeafSecret derives and removes the node secret of one leaf, expanding and then
// erasing every ancestor still held along the way.
//
// The caller owns the returned slice and is expected to erase it once both ratchet roots
// exist; this function does not erase it, because the value has to survive the return.
//
// The erasure ORDER is load-bearing and is the one thing here that no correctness test of
// the taken leaf can see: both children are written before the parent is zeroized. Zeroizing
// between the two derives the right child from a run of zeros — a value any party can
// compute with no knowledge of the group — and every leaf under it stays self-consistent, so
// only a comparison against an independent derivation notices.
//
// The caller holds stateLock.
//
// The noinline directive is here because this package's erase-helper gate derives its class
// as "every function that writes through storage outliving the call", and the writes into
// self.nodes put this function in it. Two honest notes about that. The gate is right that
// this function erases -- zeroizeSecret is called from here and the deletion is this
// function's job -- and it is over-broad about why: a map store is a runtime call with
// observable effects, not a store a compiler may delete, so the directive protects the
// zeroize inside zeroizeSecret (which carries its own) rather than anything written here.
// It costs nothing either way; a body this size is far past the inliner's budget.
//
//go:noinline
func (self *SecretTree) takeLeafSecret(leaf LeafIndex) ([]byte, error) {
	// an erased tree still HOLDS a node secret for every unconsumed leaf; every byte of it
	// is zero. Expanding that would hand back a leaf secret an attacker can derive without
	// knowing anything about the group, so the whole surface that reaches node secrets
	// refuses once Zeroize has run, and this is the deepest point of it.
	if self.erased {
		return nil, fmt.Errorf("%w: leaf %d", ErrEpochErased, leaf)
	}
	path, err := self.pathToLeaf(leaf)
	if err != nil {
		return nil, err
	}
	// the deepest node still held, not the first.
	//
	// Held nodes form an antichain: expanding a node deletes it and stores both of its
	// children, so no two nodes still held ever sit on one root-leaf path. That means the
	// two rules name the same node and no sequence of calls on this type can tell them
	// apart, which is why the invariant is asserted rather than left as an argument here --
	// TestSecretTreeHeldNodesNeverShareARootLeafPath walks every take order of every swept
	// width and holds it.
	//
	// The rule is the deepest one anyway, because that is the safe answer if the invariant
	// ever broke. Starting from a shallower ancestor would re-derive and OVERWRITE a subtree
	// from a secret this tree has already promised to destroy, replacing live node secrets
	// with ones an attacker holding the erased ancestor can compute;
	// TestSecretTreeTakesTheDeepestHeldAncestorAndNotTheShallowest plants exactly that state
	// and holds the answer to the deeper node.
	deepest := -1
	for i, node := range path {
		if _, ok := self.nodes[node]; ok {
			deepest = i
		}
	}
	if deepest < 0 {
		return nil, fmt.Errorf("%w: leaf %d", ErrSecretTreeConsumed, leaf)
	}
	nh := self.crypto.HashSize()
	for i := deepest; i < len(path)-1; i++ {
		parent := path[i]
		parentSecret := self.nodes[parent]
		left, err := Left(parent)
		if err != nil {
			return nil, fmt.Errorf("%w: left child of node %d: %w", ErrSecretTreeLeafOutOfRange, parent, err)
		}
		right, err := Right(parent)
		if err != nil {
			return nil, fmt.Errorf("%w: right child of node %d: %w", ErrSecretTreeLeafOutOfRange, parent, err)
		}
		// measured redundant for a full tree, and labelled: section 7.7 makes a valid leaf
		// count a power of two, Root refuses every other, and
		// TestSecretTreeEveryParentOnEveryPathHasBothChildrenInsideTheNodeArray holds that
		// for every swept width, so this cannot fire.
		//
		// It is a REFUSAL and not the two ifs that stood here, because the two ifs failed
		// SILENTLY in the direction that matters: a child store skipped leaves every leaf
		// under that child answering ErrSecretTreeConsumed, which tells the caller forward
		// secrecy did its job when what actually happened is that the tree dropped a
		// subtree. A width that OVERSTATES the node array -- the direction no guard of this
		// shape can see at all -- is caught before any leaf is taken, by
		// TestSecretTreeCachedGeometryIsDerivedFromTheLeafCount.
		if uint32(left) >= self.width || uint32(right) >= self.width {
			return nil, fmt.Errorf("%w: children %d and %d of node %d lie outside a node array of width %d",
				ErrSecretTreeLeafOutOfRange, left, right, parent, self.width)
		}
		// ExpandWithLabel returns a fresh slice, so the parent can be zeroized after both
		// stores without touching either child.
		self.nodes[left] = self.crypto.ExpandWithLabel(parentSecret, "tree", []byte("left"), nh)
		self.nodes[right] = self.crypto.ExpandWithLabel(parentSecret, "tree", []byte("right"), nh)
		zeroizeSecret(parentSecret)
		delete(self.nodes, parent)
	}
	target := path[len(path)-1]
	secret, ok := self.nodes[target]
	if !ok {
		// measured redundant, and reachable only by breaking an invariant: the loop above
		// stores both children of every node it expands and its last iteration expands
		// path[len(path)-2], so the target is held whenever the loop ran -- and when it did
		// not run, deepest already named the target. Reclassifying this return to an
		// unrelated sentinel left every test in this package passing, which is why it now
		// wraps one of its own: errSecretTreeDescentDidNotStoreTheTarget says an invariant
		// broke rather than "this leaf was already taken", and
		// TestSecretTreeASecondTakeIsAlwaysTheConsumedRefusalAndNeverTheInvariantOne holds
		// every legitimate second take to the OTHER return.
		return nil, fmt.Errorf("%w: leaf %d: %w", ErrSecretTreeConsumed, leaf,
			errSecretTreeDescentDidNotStoreTheTarget)
	}
	delete(self.nodes, target)
	return secret, nil
}

// ---------------------------------------------------------------------------
// the per-sender hash ratchets: RFC 9420 section 9.1
// ---------------------------------------------------------------------------

// RatchetType selects a leaf's handshake or application ratchet. The two are separate
// expansions of one leaf secret so a handshake message and an application message can
// never share an AEAD key and nonce.
//
// The zero value is deliberately not one of them. A ratchet type arrives from a decoded
// ContentType one layer up, and a zero that silently meant "handshake" would route an
// application message onto the handshake ratchet -- which is not a decode failure, it is
// two different message streams drawing from one keystream.
type RatchetType uint8

const (
	RatchetHandshake RatchetType = iota + 1
	RatchetApplication
)

// ratchetKey names one ratchet: one leaf, one type. Both halves are in the key because the
// whole safety argument of this file is that no two senders and no two message streams
// ever reach the same key and nonce pair, and a table keyed on less than this would hand
// one ratchet to two of them.
type ratchetKey struct {
	leaf LeafIndex
	kind RatchetType
}

// generationKeys is one generation's AEAD key and nonce.
type generationKeys struct {
	key   []byte
	nonce []byte
}

// ratchet is one leaf's hash ratchet for one RatchetType.
//
// head is the next generation this ratchet will produce, and secret is the ratchet secret
// that generation will be derived from, so the pair always describes the SAME point on the
// chain. Nothing may advance one without the other: a head that moved on its own would
// re-derive a generation number under a secret that has already moved, and a secret that
// moved on its own would put two generations on one number.
//
// exhausted is what stops head from wrapping at 2^32. A wrap is not a wasted message, it is
// generation numbers being reused on the wire while the keys behind them have moved on, and
// the receiver's own consumed check then reads the reused number as a replay. There is no
// successor past 2^32-1, so the honest answer is a refusal and a rekey.
type ratchet struct {
	crypto    CryptoProvider
	secret    []byte
	head      uint32
	exhausted bool
	window    map[uint32]*generationKeys
}

// newRatchet builds one ratchet over this tree's own provider, taking ownership of the root
// secret: it is erased in place by the first step, so the caller must not keep it or pass a
// slice it still reads.
//
// The provider is read off the receiver rather than taken as a parameter. Two reasons, and
// the second is the one that decided it. A ratchet built over a provider that is not the
// one the tree derived its leaf secret with would derive at another suite's widths from
// this suite's secret. And the class this package holds every construction handed a
// provider to -- routed through it, reads Nh off it, draws exactly what it uses, leaves its
// input alone -- is about DERIVATIONS; a struct literal that stores what it is given
// answers none of those questions, and would have to be excused from all six.
func (self *SecretTree) newRatchet(rootSecret []byte) *ratchet {
	return &ratchet{
		crypto: self.crypto,
		secret: rootSecret,
		head:   0,
		window: map[uint32]*generationKeys{},
	}
}

// step derives the head generation's key and nonce, replaces the ratchet secret with its
// successor, erases the old secret and advances the head.
//
// The three derivations all read the CURRENT secret, before it is replaced, and each binds
// the generation number into the KDF context. That binding is what makes a repeated key and
// nonce pair require two independent defects rather than one: the secret advancing and the
// generation number advancing would both have to stop.
//
// The erasure is the forward secrecy, and it is the half no correctness test can see. A
// ratchet that derives its successor and keeps the predecessor answers every "what is
// generation n's key" question identically, and hands anyone who takes the process a second
// later every generation from that point back to the epoch's start.
//
// The three lengths are read off the provider. Both registered suites fix Nn at 12 and one
// of them fixes Nk at 32, which is also Nh and also the literal a body would have written
// down, so inside the registry a read and a constant are the same number.
//
// The noinline directive is here for the reason takeLeafSecret carries one: this function
// is in the erase-helper class, and the directive keeps the store inside zeroizeSecret
// across a call boundary the compiler cannot see through.
//
//go:noinline
func (self *ratchet) step() (uint32, *generationKeys, error) {
	if self.exhausted {
		return 0, nil, fmt.Errorf("%w: generation %d was the last", ErrRatchetExhausted, self.head)
	}
	generation := self.head
	keys := self.keysAt(self.secret, generation)
	next := self.chainSecretAfter(self.secret, generation)
	zeroizeSecret(self.secret)
	self.secret = next
	if generation == ^uint32(0) {
		// the counter is not allowed to wrap. head stays where it is so a later classify
		// still classifies every generation below it as consumed rather than as future.
		self.exhausted = true
	} else {
		self.head = generation + 1
	}
	return generation, keys, nil
}

// keysAt and chainSecretAfter are the two halves of RFC 9420 section 9.1's derivation, each
// written ONCE.
//
// THEY ARE FUNCTIONS BECAUSE THERE ARE NOW TWO WALKS OVER THIS CHAIN and not one. step advances
// this ratchet through them and peekFor walks a SCRATCH copy through chainSecretAfter without
// advancing anything, so the same three expansions are reached from two places. Spelled out twice,
// a peek and a commit could come to derive different keys for one generation -- a transposed
// label, an argument order, a length taken from a constant on one side and off the provider on the
// other -- and the failure would arrive as an AEAD tag that does not verify, which this package
// reports as tampering.
//
// They take the secret they read as an ARGUMENT rather than off the receiver, which is what lets
// the peek hand over a copy and get scratch behaviour without either function knowing which caller
// it has. The generation is bound into all three expansions: that binding is what makes a repeated
// key and nonce pair require two independent defects rather than one, the secret advancing and the
// generation number advancing would both have to stop.
//
// The three lengths are read off the provider. Both registered suites fix Nn at 12 and one of them
// fixes Nk at 32, which is also Nh and also the literal a body would have written down, so inside
// the registry a read and a constant are the same number.
//
// Neither erases anything, which is why neither carries a noinline directive: the storage they
// read is the caller's, and the caller is the one that knows whether it is a scratch copy to erase
// or the ratchet's own.
func (self *ratchet) keysAt(secret []byte, generation uint32) *generationKeys {
	return &generationKeys{
		key:   self.crypto.DeriveTreeSecret(secret, "key", generation, self.crypto.KeySize()),
		nonce: self.crypto.DeriveTreeSecret(secret, "nonce", generation, self.crypto.NonceSize()),
	}
}

func (self *ratchet) chainSecretAfter(secret []byte, generation uint32) []byte {
	return self.crypto.DeriveTreeSecret(secret, "secret", generation, self.crypto.HashSize())
}

// peekFor returns the keys for one generation and MOVES NOTHING.
//
// IT IS THE FORM AN UNAUTHENTICATED GENERATION IS READ THROUGH, and that is the whole reason it
// exists in this shape. The generation number arrives inside a PrivateMessage's sender data, which
// is sealed under the epoch's sender_data_secret -- a secret EVERY member of the group holds. So at
// the moment this ratchet is asked for a key, the only thing established about the number is that a
// member chose it, and "a member chose it" is exactly the threat model section 9 has to survive:
// RFC 9420 derives the whole secret tree from encryption_secret, so any member can compute any
// OTHER member's leaf and hand this function any generation it likes at it.
//
// THE PROPERTY, IN THE TERMS IT HAS TO BE HELD IN: no input an unauthenticated party controls may
// advance or prune another member's receiving ratchet. A peek that FINDS a key is fine -- finding
// is what the caller asked for. A peek that MUTATES is the defect, and the mutation is not the
// obvious one. Nothing here erases anything; what the eager form did was ADVANCE the head, and
// every generation below a head is classified consumed, so a head pushed to 1025 answers
// ErrRatchetGenerationConsumed for the true sender's next 1,025 messages at that receiver, forever.
// MEASURED on the shape this replaces: one forged header at head+MaxGenerationSkip cost the victim
// its next message, two cost it 1,025 of them, and three forged headers at ONE record position cost
// it four of its next four -- pre-emptively, before the victim had written anything, and
// repeatably, because the refusal is taken below every caller that would otherwise have consumed a
// position of its own. The eviction is the second half of the same mutation: the skipped run the
// walk retained pushed the tree past MaxRetainedWindowKeys, and the next call's pruneRetained
// zeroized the victim's genuinely retained generations to make room for the attacker's.
//
// SO THE WALK IS OVER A COPY. self.secret, self.head and self.window are not written by anything
// below, and the scratch chain is erased as the walk passes it, so a peek that is never committed
// has cost this ratchet nothing but cpu -- and the cpu is the same MaxGenerationSkip expansions the
// eager form already granted for the price of one header, so nothing new is bought either.
// (*ratchet).commitFor is the other half, and it is reached only after the content AEAD has opened
// and the sender's signature has verified.
//
// IT IS THE SHAPE connect/messagegroup ALREADY SHIPS ONE LAYER UP. (*ReceiverRatchet).PeekFor walks
// a copy of the record ladder and (*ReceiverRatchet).Commit applies it once the record has opened,
// for the same reason and against the same attacker. The two receiving ratchets a URmessage record
// passes through now hold one discipline rather than two, which is what makes the sentence "a
// refused record moves no receiver ratchet" true of both of them.
//
// THE TWO REFUSALS ARE classify's, so the peek and the commit cannot come to disagree about which
// generations exist. A retained generation is answered from the window and COPIED, because the
// window has to go on holding it until the message it belongs to has actually opened.
//
// The noinline directive is the erase-helper class's: the walk erases every scratch rung it passes
// and those stores are dead in the compiler's reading.
//
//go:noinline
func (self *ratchet) peekFor(generation uint32) (*generationKeys, error) {
	if keys, ok := self.window[generation]; ok {
		return &generationKeys{
			key:   append([]byte(nil), keys.key...),
			nonce: append([]byte(nil), keys.nonce...),
		}, nil
	}
	if err := self.classify(generation); err != nil {
		return nil, err
	}
	// THE SCRATCH ERASE BELOW DEFENDS NOTHING THIS SUITE CAN SEE, measured: delete both
	// zeroizeSecret calls and ./mls/, ./message/ and ./messagegroup/ stay green. Nothing in the
	// process can reach these slices once this call returns, so no runtime assertion tells a walk
	// that erased from one that dropped. It stays because the run of chain secrets between the head
	// and a generation an UNAUTHENTICATED party named is exactly what a peek must not leave in the
	// heap, and the gate over that class reads the SOURCE:
	// TestNoDeclarationReachingTheSecretTreeStoragePutsItBeyondTheCall. UNOBSERVED.md carries it.
	walking := append([]byte(nil), self.secret...)
	for at := self.head; at < generation; at += 1 {
		next := self.chainSecretAfter(walking, at)
		zeroizeSecret(walking)
		walking = next
	}
	keys := self.keysAt(walking, generation)
	zeroizeSecret(walking)
	return keys, nil
}

// classify decides whether a generation is answerable AT ALL, and writes nothing.
//
// It is one function and not two copies because the peek and the commit have to admit exactly the
// same set: a commit that refused what the peek served would refuse a message that had already
// authenticated, and a commit that served what the peek refused would be a second, laxer rule
// reached by anyone who could get a message past the first.
//
// THE ORDER OF THE TWO ARMS IS NOT INTERCHANGEABLE. A generation below the head is consumed, which
// is a fact about this receiver; a generation far above it is a bound, which is a fact about what a
// sender is allowed to ask for. Reversing them would report an old generation as "too far ahead"
// whenever the head had run past the bound. The exhausted arm is the second half of the first: head
// does not advance past 2^32-1, so the last generation of an epoch is the one value "below the
// head" cannot classify -- without it that single generation comes back as ErrRatchetExhausted
// while every other generation of the same epoch comes back as ErrRatchetGenerationConsumed, and a
// caller asking "is this a replay" has to get one answer across the whole range of the counter.
// Exhaustion stays the SENDER's sentinel: nextSenderKeyLocked reaches step directly and still
// answers ErrRatchetExhausted, which is the fact that party needs -- there is no next generation,
// rekey.
//
// THE BOUND REFUSES AND CATCHES UP NO MORE, and that is a deliberate loss of behaviour rather than
// an omission. A refusal here used to advance the head by MaxGenerationSkip first, so that a
// receiver restored behind a busy peer resynchronised over a handful of refused messages instead of
// going deaf to that peer for the epoch. That resynchronisation was driven by a generation number
// nobody had authenticated, which is the same sentence as "any member can advance any other
// member's head by MaxGenerationSkip for the price of one header" -- the resynchronisation and the
// deletion channel were ONE MECHANISM READ FROM TWO ENDS. Its own disclosure argued it granted
// nothing new, and that argument was true only while the ACCEPTED path advanced the head as well.
// The accepted path no longer does, so the catch-up became the only grant left and its premise was
// gone with it. What a lagging receiver does instead is stated at MaxGenerationSkip's declaration
// and measured by TestARestoredMemberIsDeafToAPeerThatMovedPastTheSkipBound.
//
// The caller holds stateLock.
func (self *ratchet) classify(generation uint32) error {
	if generation < self.head || (self.exhausted && generation == self.head) {
		return fmt.Errorf("%w: generation %d, head %d", ErrRatchetGenerationConsumed, generation, self.head)
	}
	// generation is at or above head here, so the subtraction cannot wrap.
	if generation-self.head > MaxGenerationSkip {
		return fmt.Errorf("%w: generation %d, head %d, bound %d",
			ErrRatchetGenerationTooFarAhead, generation, self.head, MaxGenerationSkip)
	}
	return nil
}

// commitFor applies the movement peekFor described, once the message at that generation has
// AUTHENTICATED, and it is where every write on the receiving path lives.
//
// It is the only thing in this package that advances a RECEIVING head. A caller that peeks and
// never commits has cost this ratchet nothing, which is the whole of what the split buys.
//
// The retained generation is handed over and DROPPED, so a second request for it is refused: a
// window that hands the same generation out twice is a window that survives a replay. The walking
// case retains every generation it passes and NOT the target -- the target is the one generation
// this very call consumes, so storing it and deleting it again is the same state reached by two
// more writes. That is the only observable difference from the split this replaces, and it removes
// the RatchetWindowSize+1 peak the old peek's own disclosure had to carry.
//
// receiverKey reaches it directly, because that door has no second call in which a caller says the
// message opened; CommitMessageKey reaches it after OpenPrivateMessage has verified the signature.
//
// The steps guard is kept for the reason the loop it replaces carried one: the bound in classify
// makes at most MaxGenerationSkip+1 steps reach any generation it admits, but a head that stopped
// advancing would turn this into a hang, and a hang a peer reaches by choosing a generation number
// is the denial of service the bound exists to prevent. Measured on the shape this replaces: with
// step leaving the head where it is, three tests of this package stop failing and start TIMING OUT,
// which is the worse of the two.
//
// The caller holds stateLock.
//
// The noinline directive is the erase-helper class's: prune erases through storage that outlives
// this call.
//
//go:noinline
func (self *ratchet) commitFor(generation uint32) (*generationKeys, error) {
	if keys, ok := self.window[generation]; ok {
		delete(self.window, generation)
		return keys, nil
	}
	if err := self.classify(generation); err != nil {
		return nil, err
	}
	for steps := uint32(0); ; steps += 1 {
		if steps > MaxGenerationSkip {
			return nil, fmt.Errorf("%w: generation %d, head %d, bound %d: the ratchet stopped advancing",
				ErrRatchetGenerationTooFarAhead, generation, self.head, MaxGenerationSkip)
		}
		stepped, keys, err := self.step()
		if err != nil {
			return nil, err
		}
		if stepped == generation {
			return keys, nil
		}
		self.window[stepped] = keys
		self.prune()
	}
}

// eraseKey zeroizes one retained generation and drops it.
//
// Total by design: erasing a generation that was never derived is a no-op, because the
// framing layer calls it on paths that never found a key. It must never be the thing that
// creates one.
//
// It is the single erase site for a window entry -- evictOldest chooses WHICH generation goes
// and then comes here -- because the erasure is the half a caller omits silently. A bare
// delete leaves live AEAD keys wherever the allocator puts them next, nothing the tree still
// reaches can see the difference, and this package has already measured that shape passing
// every test it had.
//
// The noinline directive is this package's erase-helper class, and this function is a member
// of that class through the HAND-OFF rather than through a write it spells: the window map
// outlives the call, the entry read out of it with a comma ok read is that same storage, and
// zeroizeSecret is where the stores are. TestEveryEraseHelperCarriesTheNoInlineDirective
// derives the class transitively for exactly this reason. Until it did, this sentence named an
// enforcement that was not there: deleting the directive from the line below was invisible to
// all 526 tests of this package.
//
//go:noinline
func (self *ratchet) eraseKey(generation uint32) {
	keys, ok := self.window[generation]
	if !ok {
		return
	}
	zeroizeSecret(keys.key)
	zeroizeSecret(keys.nonce)
	delete(self.window, generation)
}

// evictOldest drops the oldest retained generation, erasing its key material in place.
//
// The oldest is what goes, because a skipped generation that has not arrived yet grows less
// likely to arrive the older it gets.
//
// It is one helper rather than a loop written out in each of the two callers, because the
// ERASURE is the half a caller omits silently: a bare delete leaves live AEAD keys wherever
// the allocator puts them next, nothing the tree still reaches can see the difference, and
// this package has already measured that shape passing every test it had. Both the per
// ratchet bound and the tree wide one evict through here, so the erasure cannot be present
// on one path and missing on the other -- and both reach it through eraseKey, so the third
// way an entry leaves the window cannot be present on one path and missing there either.
//
// The noinline directive is carried here by the same convention, and this function is the one
// place that convention runs ahead of the gate: the erasure it performs is eraseKey's, reached
// through a method call on the same receiver, and
// TestEveryEraseHelperCarriesTheNoInlineDirective follows the hand-off through ARGUMENTS only.
// Counting receivers would close the class over every exported method of this type that erases
// anything anywhere, MessageKey and receiverKey included, and what separates those from an
// erase helper is intent, which no matcher reads. So the line is drawn where a name is handed
// over, and this paragraph says which side of it this function is on rather than claiming a
// membership it does not have.
//
//go:noinline
func (self *ratchet) evictOldest() {
	if len(self.window) == 0 {
		return
	}
	oldest := ^uint32(0)
	for generation := range self.window {
		if generation < oldest {
			oldest = generation
		}
	}
	// the erase itself is eraseKey's, so the two ways an entry leaves the window -- evicted
	// because the bound was reached, erased because its message was opened -- zeroize
	// through one body. Choosing WHICH generation goes is all this function does.
	self.eraseKey(oldest)
}

// prune evicts the oldest retained generations once THIS ratchet is over its own bound.
//
// It is a bound on MEMORY, and the party who decides how much of it gets used is whoever
// sends the messages. Without eviction a sender that skips generations forever grows a
// receiver's heap forever; with eviction at one an ordinary out of order delivery loses
// every message but the newest.
//
// This bound is per ratchet, which is the whole of what it can promise: the number of
// ratchets is not the receiver's choice. SecretTree.pruneRetained is the other half.
func (self *ratchet) prune() {
	for len(self.window) > RatchetWindowSize {
		self.evictOldest()
	}
}

// ratchetKeyLess orders two ratchet keys.
//
// It exists so the eviction below breaks a tie between two equally full windows by the key
// rather than by go's randomised map iteration. A bound that reached a different ratchet on
// every run would be a bound whose behaviour cannot be stated, let alone tested.
func ratchetKeyLess(left ratchetKey, right ratchetKey) bool {
	if left.leaf != right.leaf {
		return left.leaf < right.leaf
	}
	return left.kind < right.kind
}

// pruneRetained holds the skipped generation keys retained across EVERY ratchet of this tree
// to one bound.
//
// RatchetWindowSize bounds one ratchet, and the number of ratchets is not this receiver's
// choice. receiverKey reaches ratchetFor -- and so takeLeafSecret -- before any generation
// check and before any AEAD, because in section 9 the AEAD tag is the authentication and the
// key has to exist before it can be checked. So a peer that picks leaf indices and generation
// numbers out of the air materialises every leaf's two ratchets and fills every one of their
// windows, and nothing it sent had to be authentic. Measured on the shape this replaces: a 64
// leaf tree, four forged headers per ratchet, 131072 retained generation keys and 17 MB of
// heap -- and the figure is LINEAR IN THE GROUP SIZE, so a 1024 leaf group reaches roughly
// 270 MB from roughly 4096 unauthenticated headers. A bound multiplied by a number the
// attacker chooses is not a bound.
//
// What goes is the oldest generation OF THE FULLEST RATCHET, and the choice of ratchet is the
// half that matters. Evicting the globally oldest generation instead would let one flooding
// sender push out the handful of keys an honest out of order sender is holding, which turns a
// memory bound into a way to drop other members' messages; taking from the largest holder
// puts the pressure on whoever created it.
//
// The caller holds stateLock.
func (self *SecretTree) pruneRetained() {
	for {
		retained := 0
		var fullest *ratchet
		var fullestKey ratchetKey
		for key, r := range self.ratchets {
			retained += len(r.window)
			if fullest == nil || len(r.window) > len(fullest.window) ||
				(len(r.window) == len(fullest.window) && ratchetKeyLess(key, fullestKey)) {
				fullest, fullestKey = r, key
			}
		}
		if retained <= MaxRetainedWindowKeys {
			return
		}
		if fullest == nil || len(fullest.window) == 0 {
			// unreachable: retained is the sum of the window lengths, so a total over the
			// bound means some window is non empty and the fullest is one of those. It is a
			// return and not an assertion because the alternative for a loop whose only exit
			// is the bound would be to spin forever on a state it cannot fix.
			return
		}
		fullest.evictOldest()
	}
}

// zeroize clears the ratchet secret and every retained window entry.
//
//go:noinline
func (self *ratchet) zeroize() {
	zeroizeSecret(self.secret)
	for generation, keys := range self.window {
		zeroizeSecret(keys.key)
		zeroizeSecret(keys.nonce)
		delete(self.window, generation)
	}
}

// refuseUnknownRatchetType is the one door onto "is this a ratchet type this build has".
//
// ONE DOOR AND NOT TWO ENUMERATIONS. ratchetFor asks it because a kind it does not recognise has
// no root to store; RestoreSenderRatchets asks it because a persisted vector is a corrupted store's
// choice and its entries are installed one at a time. A second copy of the same two names is how a
// kind added to this file joins one of the two readings and not the other, and the reading it
// missed then decides that a well formed value is unknown -- or, worse, that an unknown one is
// well formed.
//
// The sentinel is ErrSecretTreeLeafOutOfRange for the reason it was already ratchetFor's: a caller
// asking for a ratchet names a leaf and a kind, and this package answers "there is no such ratchet
// in this tree" under one value however the pair failed. errRatchetTypeHasNoRoot is what tells the
// two apart for a test, and it belongs to the arm below rather than to this one.
func refuseUnknownRatchetType(kind RatchetType) error {
	if kind != RatchetHandshake && kind != RatchetApplication {
		return fmt.Errorf("%w: unknown ratchet type %d", ErrSecretTreeLeafOutOfRange, kind)
	}
	return nil
}

// ratchetFor returns the leaf's ratchet, creating BOTH of a leaf's ratchets together so the
// leaf node secret is taken from the tree exactly once and erased immediately. Taking it
// twice is refused by the tree, so creating them one at a time would make the second kind
// unreachable for every leaf.
//
// The erased check is first, and not after the type check or after the cache lookup. An
// erased tree holds Nh zero bytes where each node secret was, and expanding a run of zeros
// is not a weak derivation, it is a PUBLIC one: any party can compute it knowing nothing
// about the group. So an epoch past its window refuses rather than answers, which is the
// same call ErrEpochErased's own comment makes for the key schedule.
//
// The caller holds stateLock; this function does not take it.
//
// The noinline directive is the erase-helper class's, for the leaf secret erased below.
//
//go:noinline
func (self *SecretTree) ratchetFor(leaf LeafIndex, kind RatchetType) (*ratchet, error) {
	if self.erased {
		return nil, fmt.Errorf("%w: leaf %d", ErrEpochErased, leaf)
	}
	if err := refuseUnknownRatchetType(kind); err != nil {
		return nil, err
	}
	key := ratchetKey{leaf: leaf, kind: kind}
	if existing, ok := self.ratchets[key]; ok {
		return existing, nil
	}
	leafSecret, err := self.takeLeafSecret(leaf)
	if err != nil {
		return nil, err
	}
	nh := self.crypto.HashSize()
	self.ratchets[ratchetKey{leaf: leaf, kind: RatchetHandshake}] =
		self.newRatchet(self.crypto.ExpandWithLabel(leafSecret, "handshake", nil, nh))
	self.ratchets[ratchetKey{leaf: leaf, kind: RatchetApplication}] =
		self.newRatchet(self.crypto.ExpandWithLabel(leafSecret, "application", nil, nh))
	// the leaf secret has produced both roots and is now the one value that could regenerate
	// either of them, so it stops existing here.
	zeroizeSecret(leafSecret)
	created, ok := self.ratchets[key]
	if !ok {
		// measured redundant behind the type check at the top, which admits only the two
		// kinds the two stores above write. It is a REFUSAL rather than the bare map read
		// that stood here, because a bare read answers a NIL RATCHET AND A NIL ERROR for any
		// kind that check ever stopped catching, and the caller then steps a nil ratchet.
		// Measured: with the type check replaced by a constant false, the bare read makes
		// the whole test binary panic on a nil dereference, so the run reports one aborted
		// test instead of the test written for exactly that mutation --
		// TestRatchetForRefusesAnUnknownRatchetType never gets to run. A refusal here is what
		// makes that mutation observable rather than fatal.
		return nil, fmt.Errorf("%w: ratchet type %d has no root for leaf %d: %w",
			ErrSecretTreeLeafOutOfRange, kind, leaf, errRatchetTypeHasNoRoot)
	}
	return created, nil
}

const (
	// MaxGenerationSkip bounds how far ahead of the current head a receiver will derive in one
	// call. A generation number is attacker supplied, and without a bound a single uint32 buys
	// four billion KDF calls for the price of one forged header.
	//
	// IT IS A BOUND ON WORK AND ON NOTHING ELSE, and that sentence is the correction this
	// declaration exists to carry. A refusal past this bound used to advance the head by exactly
	// this much before returning, so that a receiver behind a busy peer closed the gap by
	// MaxGenerationSkip-1 per message it lost instead of going deaf to that peer for the epoch.
	// The argument for it was that the work a generation number buys is this constant whether the
	// number is inside the bound or outside it -- true, and a statement about CPU. What it was
	// silent about is EVICTION: every generation below a head is consumed, so a walk driven by a
	// number nobody authenticated made another member's messages permanently unopenable, one
	// header at a time and repeatably. A bound on the work is not a bound on the damage.
	//
	// SO NOTHING WALKS ON A REFUSAL ANY MORE, and the receive path is two phase instead:
	// (*ratchet).peekFor derives over a scratch copy and writes nothing, and (*ratchet).commitFor
	// carries every write and is reached only after a signature has verified.
	//
	// WHAT A LAGGING RECEIVER DOES NOW, stated here because it is the cost. A receiver whose head
	// is more than this many generations behind one sender is refused with
	// ErrRatchetGenerationTooFarAhead and stays refused for every later generation that sender
	// reaches in this epoch; the epoch's next commit rebuilds the secret tree and every receiving
	// head starts at 0 again. That arrangement is reached by a RESTORE and not by ordinary
	// delivery -- the persisted group state carries a member's own sender position and nothing
	// about where its receiving ratchets for its peers stood -- so the durable repair is to
	// persist those positions, which is (*Group).LoadGroup's disclosure and not this constant's.
	// Out of order delivery and lost messages INSIDE the bound are untouched: the window retains
	// every generation an accepted walk passed, exactly as before.
	// TestAReceiverPastTheSkipBoundStaysWhereItIs and
	// TestARestoredMemberIsDeafToAPeerThatMovedPastTheSkipBound measure both halves.
	MaxGenerationSkip uint32 = 1024

	// RatchetWindowSize bounds the skipped keys retained for out of order receipt BY ONE
	// RATCHET. A sender that skips more than this many produces a visible gap, which is the
	// same trade spec A section 5.5 makes for records.
	RatchetWindowSize int = 1024

	// MaxRetainedWindowKeys bounds the skipped keys retained by the WHOLE TREE, across every
	// ratchet it holds, so a receiver's retained key memory is a constant rather than a
	// multiple of the group size -- and the group size is a number the other members choose.
	// See SecretTree.pruneRetained for what a peer buys without it, measured.
	//
	// It is RatchetWindowSize itself and not a multiple of it, so a tree of any size retains
	// at most what a single ratchet could already cost and adding senders adds no memory at
	// all. What keeps that from starving an honest out of order sender is WHICH entry goes:
	// the eviction always lands on the fullest window, so a member holding a handful of
	// skipped keys is never the one paying for a member holding a thousand.
	//
	// It is derived from RatchetWindowSize rather than written down again, so the two cannot
	// drift into a state where the per ratchet bound is the larger of the pair and the tree
	// wide one is the only bound that ever fires.
	MaxRetainedWindowKeys int = RatchetWindowSize
)

// NextSenderKey returns the next generation's key and nonce for our own leaf, and advances
// that leaf's ratchet past it. There is no way to ask for the same generation twice, which
// is what makes this the encrypt path: a caller that dropped the answer and called again
// gets the NEXT generation rather than the one it lost.
func (self *SecretTree) NextSenderKey(leaf LeafIndex, kind RatchetType) (generation uint32, key []byte, nonce []byte, err error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.nextSenderKeyLocked(leaf, kind)
}

// nextSenderKeyLocked is the encrypt path itself, split out so NextSenderKey and
// NextMessageKey are one derivation reached by two names rather than two derivations that
// can drift. It has to be split: this type's lock is not reentrant, so the ContentType
// keyed wrapper cannot simply call the RatchetType keyed method, and a wrapper that took no
// lock at all would be an exported path to guarded state without one.
//
// The caller holds stateLock; this function does not take it.
func (self *SecretTree) nextSenderKeyLocked(leaf LeafIndex, kind RatchetType) (generation uint32, key []byte, nonce []byte, err error) {
	r, err := self.ratchetFor(leaf, kind)
	if err != nil {
		return 0, nil, nil, err
	}
	generation, keys, err := r.step()
	if err != nil {
		return 0, nil, nil, err
	}
	return generation, keys.key, keys.nonce, nil
}

// SenderGeneration is the next generation this leaf's ratchet will hand out.
//
// It creates the ratchet if it does not exist yet, which is why it can fail: asking a
// consumed leaf where its ratchet stands is the same question as asking for its secret.
func (self *SecretTree) SenderGeneration(leaf LeafIndex, kind RatchetType) (uint32, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	r, err := self.ratchetFor(leaf, kind)
	if err != nil {
		return 0, err
	}
	return r.head, nil
}

// SenderRatchets is where this leaf's OWN ratchets stand, as the persisted epoch state carries
// them: one entry per ratchet of that leaf THAT ALREADY EXISTS, in ascending RatchetType order.
//
// EXPORTED AND ANSWERING AN UNEXPORTED TYPE, which is a shape worth explaining rather than
// leaving to be noticed. This type's lock discipline is that an exported entry point takes
// stateLock and an unexported helper assumes its caller holds it -- and the caller here is
// (*Group).marshalState, which holds the GROUP's lock and not this one. So the door has to be an
// exported one. What keeps it from being a new exported way to reach key material is the return
// type: senderRatchetEntry is unexported, so no caller outside this package can read a Secret out
// of one, build one, or do anything with the answer except hand it back to RestoreSenderRatchets
// on a tree it built itself.
//
// IT CREATES NOTHING, which is the whole difference between this and SenderGeneration. That one
// reaches ratchetFor in order to answer, and ratchetFor takes the leaf's node secret out of the
// tree -- so a persist asking it would consume this leaf's secret on behalf of a member that has
// never sent, and would then record a position of zero for two ratchets the live tree had not
// built either. A leaf with no ratchet has sent nothing under this epoch, and the ABSENCE is the
// honest record of that.
//
// THE KINDS ARE READ OFF THE TABLE AND NOT LISTED HERE. What has to be recorded is every ratchet
// this leaf is drawing from, so the class is derived from the ratchets the tree holds rather than
// from the two RatchetType constants a reader remembers; a third kind added to this file joins
// the persisted state by existing, rather than on the day somebody notices this function.
//
// SORTED, and pathSecretsOf's two reasons hold unchanged: a map has no order and these octets go
// to a disk, so two persists of one unchanged epoch must not answer different octets; and the sort
// is written out rather than imported, because this package's constant-time gate derives its
// banned comparator class out of the imports each production file makes. It is
// sortSenderRatchetEntries and not two nested loops written here, so the ordering can be held to a
// descending input on every run rather than only on the rounds the map's randomisation supplies
// one -- see that function for what the difference was measured to be.
//
// NO SECRET IS COPIED, which is marshalState's rule for every field of the blob this fills. Each
// Secret here is the live ratchet's own storage, and that is safe for exactly as long as the
// caller encodes before anything can step the ratchet again: step ZEROIZES the secret it replaces,
// so a blob built here and marshalled after a later send would persist a run of zeros -- a ratchet
// secret every party in the world can compute. (*Group).persist is that caller, it holds the
// group's own lock, and it runs in the same statement list as the seal it is recording.
func (self *SecretTree) SenderRatchets(leaf LeafIndex) ([]senderRatchetEntry, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	// AN ERASED EPOCH REFUSES RATHER THAN ANSWERING, which is this type's rule everywhere and here
	// it is the difference between a refused persist and a persisted run of zeros. Zeroize
	// overwrites every ratchet secret in place and leaves the ratchets in the table, so without
	// this the vector below is one zero secret per ratchet -- and a state written from it restores
	// a member sending under a ratchet every party in the world can compute.
	if err := self.refuseIfErased(leaf); err != nil {
		return nil, err
	}
	out := []senderRatchetEntry{}
	for key, r := range self.ratchets {
		if key.leaf != leaf {
			continue
		}
		out = append(out, senderRatchetEntry{
			Kind: key.kind, Consumed: r.consumed(), Secret: r.secret})
	}
	sortSenderRatchetEntries(out)
	return out, nil
}

// sortSenderRatchetEntries puts a vector into the strictly increasing RatchetType order that
// RestoreSenderRatchets is the only reader of.
//
// IT IS A NAMED FUNCTION SO THE ORDER CAN BE HELD WITHOUT WAITING FOR THE RUNTIME TO COOPERATE.
// Its caller ranges a map, so a case that can reach the sort only through SenderRatchets is handed
// a DESCENDING input on the rounds go's iteration randomisation happens to produce one and on no
// others -- measured over the two entries a leaf of this build holds, 600 rounds in 5000. A case
// written over that door has to ask hundreds of times, guard its own vacuity, and still assert a
// probability rather than a property. TestSenderRatchetsAnswersOneOrderWhateverTheMapHandsBack is
// still that case, because the door is what ships; this function is how the ordering itself is
// held to a descending input on every run.
//
// It sorts IN PLACE rather than answering a vector, so there is no second allocation of a slice
// whose elements alias live ratchet secrets, and no way for a caller to sort a copy and persist
// the original.
//
// The sort is written out rather than imported, for SenderRatchets' stated reason: this package's
// constant-time gate derives its banned comparator class out of the imports each production file
// makes, and slices.SortFunc arrives beside slices.Equal and slices.Compare.
func sortSenderRatchetEntries(entries []senderRatchetEntry) {
	for i := 1; i < len(entries); i += 1 {
		for j := i; 0 < j && entries[j].Kind < entries[j-1].Kind; j -= 1 {
			entries[j], entries[j-1] = entries[j-1], entries[j]
		}
	}
}

// RestoreSenderRatchets puts this leaf's own ratchets back where the persisted state left them.
//
// Exported for SenderRatchets' reason and on the same terms: the caller is (*Group).LoadGroup,
// which holds no lock of this type, and the argument is a vector of an unexported element type, so
// the only value anything outside this package could pass is one this package answered.
//
// WITHOUT IT A RESTORED MEMBER SENDS UNDER A GENERATION IT HAS ALREADY USED. A fresh SecretTree
// starts every ratchet at generation 0, so a member restored into an epoch it has already spoken
// in draws 0 again: each peer answers ErrRatchetGenerationConsumed and drops the message -- and
// the head differs per peer, so the recovery is not even uniform -- while two different plaintexts
// of that epoch have been sealed under one (key, base nonce) pair for this leaf and generation.
// The only thing between that and an AEAD nonce collision is the 32 bit reuse_guard.
//
// THE POSITION IS INSTALLED RATHER THAN REPLAYED. Stepping a fresh ratchet forward head times
// would reach the same secret, and it would also make a restore cost one KDF call per message this
// member has ever sent in the epoch -- a number a corrupted store chooses, up to 2^32. The
// persisted secret is the ratchet's CURRENT one, so this is O(1), and it adds no secret to the
// blob that was not already derivable from it: the same state carries the restore secret, and the
// encryption secret expanded out of that derives every leaf's every ratchet from generation 0.
//
// AND IT REFUSES WHAT THE WRITE SIDE REFUSES, which is the half this read had missing. SenderRatchets
// opens with refuseIfErased, and its own comment says why: Zeroize overwrites every ratchet secret
// in place and leaves the ratchets in the table, so a persist taken from an erased epoch writes one
// run of Nh zeros per ratchet, and "a state written from an erased tree restores a member sending
// under a ratchet every party in the world can compute". This function checked the LENGTH of each
// secret and nothing else. Measured: two right-length all-zero secrets restored with a nil error,
// and the very next NextSenderKey handed out a key expanded from a public constant. A read that
// admits what its own write refuses is a door only for the callers that came through the door.
//
// The refusal is ErrEpochErased and not a new sentinel, because it is not a new condition: it is
// the state SenderRatchets names, reached from a store rather than from this process. The check is
// an OR over the octets rather than a comparison against a zero array, which costs nothing and is
// the shape this package's comparator gate cannot read either way -- see constant_time_test.go's
// own statement of that limit.
//
// EVERY ENTRY IS CHECKED BEFORE ANY RATCHET IS BUILT. ratchetFor takes the leaf's node secret to
// build its two ratchets and there is no way to put it back, so a refusal discovered halfway would
// leave a tree that can never answer for this leaf again.
//
// THAT SENTENCE WAS FALSE UNTIL THE KIND JOINED THE LOOP, and it is worth recording rather than
// quietly fixing. The loop checked the secret's length, the consumed count and the ordering, and
// the one field it did not check was the one ratchetFor refuses: an entry naming a Kind this build
// does not have passed the whole validation loop and was refused on the second pass -- AFTER the
// first entry's ratchetFor had taken the leaf node secret out of the tree and installed a restored
// secret over one of the two ratchets it built. The refusal reached the caller either way, because
// LoadGroup drops and erases everything it holds on any refusal, so nothing shipped wrong; what
// was wrong was this paragraph, which claimed a property the code below did not have.
// refuseUnknownRatchetType is the door both passes now ask.
//
// BOTH OF THE LEAF'S RATCHETS ARE MATERIALISED by the first ratchetFor here, which is that
// function's own rule and is what makes a SHORT record safe: an entry vector holding fewer than the
// leaf's ratchets leaves the rest standing at generation 0, derived from the leaf secret, which is
// where a tree that had never stepped them had them.
//
// A ONE ENTRY RECORD CANNOT ARISE FROM THIS BUILD, and the sentence that stood here said it could
// -- "a member that sent application messages and no handshake message persists one entry". It
// does not: ratchetFor creates BOTH of a leaf's ratchets on the first call for either kind, so a
// leaf that has sent anything at all holds two ratchets and SenderRatchets answers two entries,
// and a leaf that has sent nothing holds none and it answers zero. The short case is reachable
// only from a store that truncated the vector or from a future kind, which is exactly why the
// handling is stated as a rule rather than removed.
func (self *SecretTree) RestoreSenderRatchets(leaf LeafIndex, entries []senderRatchetEntry) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.erased {
		return fmt.Errorf("%w: leaf %d", ErrEpochErased, leaf)
	}
	nh := self.crypto.HashSize()
	for i, entry := range entries {
		if len(entry.Secret) != nh {
			return fmt.Errorf("%w: entry %d carries a %d byte ratchet secret, want %d",
				ErrSecretLength, i, len(entry.Secret), nh)
		}
		if err := refuseUnknownRatchetType(entry.Kind); err != nil {
			return fmt.Errorf("entry %d: %w", i, err)
		}
		// the erased epoch's own value, refused here rather than expanded from. See this
		// function's comment: SenderRatchets will not WRITE this and a store can still hand it
		// back, and what comes out the other side of installing it is a sender whose every
		// generation is derivable by anybody.
		var octets byte
		for _, b := range entry.Secret {
			octets |= b
		}
		if octets == 0 {
			return fmt.Errorf("%w: entry %d carries a ratchet secret of %d zero octets, which is what an erased epoch holds and what every party in the world can compute",
				ErrEpochErased, i, len(entry.Secret))
		}
		if _, _, err := generationsConsumed(entry.Consumed); err != nil {
			return fmt.Errorf("%w: entry %d: %w", errGroupStateSenderRatchet, i, err)
		}
		// STRICTLY INCREASING, which is the shape SenderRatchets writes and therefore the only
		// shape this build can have produced. It refuses a reordered vector and, more importantly,
		// a REPEATED kind: two entries naming one ratchet would install the second OVER the first,
		// so the restored member would stand wherever the entry that happened to come last says --
		// and when that is the earlier of the two, what has been discarded is the record of the
		// generations this member has already sent under.
		if 0 < i && entry.Kind <= entries[i-1].Kind {
			return fmt.Errorf("%w: entry %d names ratchet type %d after type %d",
				errGroupStateSenderRatchetOrder, i, entry.Kind, entries[i-1].Kind)
		}
	}
	for _, entry := range entries {
		head, exhausted, err := generationsConsumed(entry.Consumed)
		if err != nil {
			return fmt.Errorf("%w: %w", errGroupStateSenderRatchet, err)
		}
		r, err := self.ratchetFor(leaf, entry.Kind)
		if err != nil {
			return err
		}
		// the freshly derived root is ERASED rather than dropped. It is the value generation 0 of
		// this ratchet comes out of, this epoch has already spent generations past it, and after
		// the assignment below nothing in this process can reach it to erase it.
		zeroizeSecret(r.secret)
		r.secret = append([]byte(nil), entry.Secret...)
		r.head = head
		r.exhausted = exhausted
	}
	return nil
}

// consumed is how many generations this ratchet has handed out, which is the ONE number a
// persisted state needs in order to put it back.
//
// One number and not the pair, because the pair has a state it must never be in. head is the next
// generation and exhausted says the counter has stopped, so (head 5, exhausted true) describes
// nothing this type can reach -- and a blob that could encode it would be a blob a corrupted store
// could hand back as "this ratchet is finished" over a ratchet with four billion generations left,
// or the other way about. A count has no such pair to disagree with itself.
//
// The count runs one past the counter: a ratchet parked on 2^32-1 with exhausted set has handed
// out that generation as well, so it has consumed 2^32 of them, which is why this is a uint64.
func (self *ratchet) consumed() uint64 {
	if self.exhausted {
		return uint64(1) << 32
	}
	return uint64(self.head)
}

// generationsConsumed is consumed read backwards: the (head, exhausted) pair a persisted count
// describes, or a refusal for a count no ratchet can have reached.
//
// The refusal is not a formality. A count above 2^32 narrowed into a uint32 head would put the
// ratchet somewhere in the middle of the epoch it claims to have finished, which is a restored
// member sending under generations it has already used -- the defect this whole field exists to
// close, reached through the arithmetic instead of through the missing field.
func generationsConsumed(count uint64) (head uint32, exhausted bool, err error) {
	switch {
	case count < uint64(1)<<32:
		return uint32(count), false, nil
	case count == uint64(1)<<32:
		return ^uint32(0), true, nil
	}
	return 0, false, fmt.Errorf("%w: %d generations consumed, and a ratchet has %d",
		ErrRatchetExhausted, count, uint64(1)<<32)
}

// Zeroize clears every secret the tree still holds and refuses every later derivation.
// Called when the epoch leaves PastEpochWindow.
//
// The flag is the load bearing half. Zeroizing without it leaves a tree whose node secrets
// and ratchet secrets are all Nh zero bytes and whose methods all still answer, and the
// answers would be keys derived from a value every party in the world can compute, handed
// back with no error. Erasing and refusing are one operation here for that reason.
//
// The noinline directive is this package's erase-helper class. Every store this method makes
// is inside zeroizeSecret and inside the ratchet's own zeroize, reached with storage of this
// object handed over as an argument, which is what makes it a member of the class
// TestEveryEraseHelperCarriesTheNoInlineDirective derives -- the same membership the gate's
// own comment named as the shape it exists for, and did not hold until the class was closed
// under the hand-off.
//
//go:noinline
func (self *SecretTree) Zeroize() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for _, secret := range self.nodes {
		zeroizeSecret(secret)
	}
	for _, r := range self.ratchets {
		r.zeroize()
	}
	self.erased = true
}

// ---------------------------------------------------------------------------
// the MessageKeySource surface the framing plan calls: RFC 9420 section 9.1
// ---------------------------------------------------------------------------

// ratchetTypeOf maps the wire ContentType a PrivateMessage header carries to the ratchet it
// draws from. RFC 9420 section 9.1: application content uses the application ratchet, and
// proposals and commits share the handshake one.
//
// It exists so no caller of the framing layer has to remember that mapping. A caller that got
// it wrong would encrypt a commit under an application key that the receiver looks up in the
// other ratchet, and the failure would arrive as a bad AEAD tag -- indistinguishable from
// tampering, and therefore diagnosed as an attack rather than as the bug it is.
//
// Anything outside the three is refused rather than defaulted. RFC 9420 reserves 0, and a
// default arm would route every unregistered code point onto the handshake ratchet: a peer
// could then draw generation 0 of a real ratchet by sending a content type nobody has
// defined, and every later handshake message of that leaf would come back consumed.
func ratchetTypeOf(contentType ContentType) (RatchetType, error) {
	switch contentType {
	case ContentTypeApplication:
		return RatchetApplication, nil
	case ContentTypeProposal, ContentTypeCommit:
		return RatchetHandshake, nil
	}
	return 0, fmt.Errorf("%w: content type %d", ErrUnknownContentType, contentType)
}

// refuseIfErased answers the epoch's own state, and is asked BEFORE any argument of the call
// is looked at.
//
// The order is ratchetFor's, for ratchetFor's reason, and it is not a style choice. An erased
// tree holds KDF.Nh zero bytes where every secret was, and expanding a run of zeros is not a
// weak derivation but a PUBLIC one, so what a caller needs to hear is "this epoch is gone" --
// a fact it acts on by going to the current epoch -- and not "your content type is wrong",
// which sends it back to re-read a header that was fine. The wrappers below reached
// ratchetTypeOf first and answered the second; the gate that caught it sweeps every exported
// method of this type with ZERO arguments, which is exactly the shape that arrives at an
// argument check before it arrives at the epoch.
//
// The caller holds stateLock; this function does not take it.
func (self *SecretTree) refuseIfErased(leaf LeafIndex) error {
	if self.erased {
		return fmt.Errorf("%w: leaf %d", ErrEpochErased, leaf)
	}
	return nil
}

// NextMessageKey is the encrypt half of the framing layer's message key source: the next
// generation's key and nonce for our own leaf, keyed on the ContentType the header carries.
//
// It is NextSenderKey under the framing layer's own key, and it consumes for the same reason
// that one does: there is no way to ask for the same generation twice, so a caller that
// dropped the answer and called again gets the next generation rather than the one it lost.
func (self *SecretTree) NextMessageKey(contentType ContentType, leaf LeafIndex) (key []byte, nonce []byte, generation uint32, err error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if err := self.refuseIfErased(leaf); err != nil {
		return nil, nil, 0, err
	}
	kind, err := ratchetTypeOf(contentType)
	if err != nil {
		return nil, nil, 0, err
	}
	generation, key, nonce, err = self.nextSenderKeyLocked(leaf, kind)
	if err != nil {
		return nil, nil, 0, err
	}
	return key, nonce, generation, nil
}

// MessageKey is the decrypt half, and it is a PEEK: it neither consumes the generation nor moves
// the ratchet in any other way. CommitMessageKey is the second half, and the pair is "look up,
// open, AUTHENTICATE, commit".
//
// WHAT THE SPLIT IS FOR, stated in the terms the defect it closes has to be stated in: no input an
// unauthenticated party controls may advance or prune another member's receiving ratchet. The leaf
// and the generation this method is handed come out of a PrivateMessage's sender data, which is
// sealed under the epoch's sender_data_secret -- a secret every member of the group holds -- so
// both are values any member CHOOSES. An earlier shape of this method looked the generation up
// through a ratcheting peek, which advanced the head to the generation asked for, retained the
// skipped run and pruned. None of that is an erase and all of it is destruction: every generation
// below a head is classified consumed, so one header naming another member's leaf at head+1024 made
// that member's next message permanently unopenable at this receiver, two made 1,025 of them
// unopenable, and the retained run pushed the tree past MaxRetainedWindowKeys so that the next
// call's pruneRetained zeroized the victim's genuine skipped keys as well. It was pre-emptive --
// the victim need never have written anything -- and repeatable at one position, because the
// refusal that follows is taken below every layer that would otherwise have spent a position of
// its own. See (*ratchet).peekFor for the measurements and for what an honest lagging receiver now
// does instead.
//
// NOTHING HERE IS A BOUND ON THE DAMAGE, AND THAT IS THE LESSON THE OLD COMMENTARY CARRIED AND
// MISSED. This file argued at length that what a peer buys with one generation number is bounded
// by MaxGenerationSkip, and that argument was correct about CPU and about memory and said nothing
// about eviction. A bound on the WORK is not a bound on the DAMAGE. The repair is not a smaller
// bound, it is that the work leaves no trace at all unless the message authenticates.
//
// THE ANSWER IS STORAGE NOTHING ELSE NAMES. A retained generation is copied out of the window on
// the way past, and a walked one is derived into fresh slices, so no key this method hands back is
// reachable from the lock guarded map afterwards. That is not caution: the window deliberately
// goes on holding a retained entry until the message it belongs to has opened, so handing the
// entry itself out would put the caller's key bytes under three later writers, each of which
// zeroizes IN PLACE -- CommitMessageKey for the same generation, pruneRetained evicting it, and
// Zeroize at the end of the epoch. Measured on the shape this replaces: two lookups of one
// generation were handed ONE array, so an erase by either holder turned the other's key into Nk
// zero bytes it had already been told were good; and a holder of generation 0 watched its key go to
// zero when a later lookup pushed the tree past MaxRetainedWindowKeys. Both come back with a nil
// error, which is the same defect the ordering below exists to prevent, reached from the other end.
// This type is built for concurrent callers -- see the lock discipline gate -- so the aliasing is
// also a write to key bytes another goroutine is reading, outside stateLock and outside anything
// the race detector could attribute to a caller.
//
// IT NO LONGER APPLIES THE TREE WIDE RETAINED BOUND, and the reason is that it no longer retains
// anything. The bound exists because a peer that picks leaf indices and generation numbers out of
// the air used to materialise every leaf's two ratchets AND fill every one of their windows from
// headers that never had to be authentic -- a number multiplied by the group size rather than a
// bound. A peek retains nothing, so the only thing an unauthenticated header still grows here is
// one ratchet per (leaf, kind) through ratchetFor, which is bounded by the tree. What retains is
// CommitMessageKey and receiverKey, and those two apply the bound where the retention is.
// TestAPeekRetainsNothingAcrossTheWholeTree is that, measured, rather than this
// paragraph.
func (self *SecretTree) MessageKey(contentType ContentType, leaf LeafIndex, generation uint32) (key []byte, nonce []byte, err error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if err := self.refuseIfErased(leaf); err != nil {
		return nil, nil, err
	}
	kind, err := ratchetTypeOf(contentType)
	if err != nil {
		return nil, nil, err
	}
	r, err := self.ratchetFor(leaf, kind)
	if err != nil {
		return nil, nil, err
	}
	keys, err := r.peekFor(generation)
	if err != nil {
		return nil, nil, err
	}
	return keys.key, keys.nonce, nil
}

// CommitMessageKey is the second half of the two phase read: it applies the movement MessageKey's
// peek described, and it is reached ONLY after the content AEAD has opened and the sender's
// signature has verified.
//
// IT IS WHERE EVERY RECEIVING-SIDE WRITE LIVES. The head advances to generation+1, every
// generation the walk passed is retained for out of order delivery, the per ratchet window and the
// tree wide retained bound are applied, and the generation just used stops existing -- which is
// the forward secrecy erase the replay guard is built on, reached at the one moment the caller can
// honestly say the message was real.
//
// IT REPLACES EraseMessageKey ON THE OPEN PATH AND SUBSUMES IT. The erase alone was never enough:
// an erase says "this generation is spent" and says nothing about where the head stands, and it was
// the HEAD that a forged header moved. A commit states both, at one moment, under one lock.
// EraseMessageKey stays for the seal path, where NextMessageKey has already consumed and there is
// nothing to walk.
//
// A REFUSAL HERE IS A REPLAY AND IS ANSWERED AS ONE. classify admits exactly what the peek admitted,
// so the only way a commit refuses a message whose peek succeeded is that something else committed
// that generation in between -- a concurrent open of the same message, which this type supports by
// construction. ErrRatchetGenerationConsumed is the honest answer and OpenPrivateMessage returns it
// rather than accepting the message twice.
//
// The keys it takes back are ERASED rather than dropped. The caller already holds its own copy from
// the peek; this one is the generation's last live copy inside the tree, and leaving it to the
// allocator is the erase this method exists to perform, omitted silently.
//
// The noinline directive is this package's erase-helper class, joined through the hand-off: the
// names below go to zeroizeSecret, and the stores are dead in the compiler's reading.
//
//go:noinline
func (self *SecretTree) CommitMessageKey(contentType ContentType, leaf LeafIndex, generation uint32) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if err := self.refuseIfErased(leaf); err != nil {
		return err
	}
	kind, err := ratchetTypeOf(contentType)
	if err != nil {
		return err
	}
	r, err := self.ratchetFor(leaf, kind)
	if err != nil {
		return err
	}
	keys, err := r.commitFor(generation)
	// the tree wide bound is applied whether or not the commit was served, for receiverKey's
	// reason: a walk that fails partway through has retained what it passed just the same, so a
	// bound applied only on the success path is one a peer walks around by always failing.
	self.pruneRetained()
	if err != nil {
		return err
	}
	// AND THIS ERASE DEFENDS NOTHING THIS SUITE CAN SEE EITHER, measured the same way: with both
	// lines deleted all three package trees stay green, because the storage is unreachable from the
	// tree the moment this returns and no case can read it back. It stays for eraseKey's reason --
	// the erasure is the half a caller omits silently -- and UNOBSERVED.md names it.
	zeroizeSecret(keys.key)
	zeroizeSecret(keys.nonce)
	return nil
}

// EraseMessageKey is the forward secrecy erase the framing layer's replay guard is built on:
// once a message at this generation has been opened, its key stops existing. It is what makes
// MessageKey's non consuming lookup safe -- the pair is "look up, open, erase", and the erase
// is the step that turns a repeatable lookup back into a single use key.
//
// Total by design. It is called on paths that never derived a key, so an unknown content
// type, a leaf outside the tree and a generation nobody asked for are all no-ops. None of
// them builds a ratchet: the table is read directly rather than through ratchetFor, because
// ratchetFor CREATES, and an erase that created would take a leaf secret out of the tree --
// destroying it, since taking it is what erases it -- on behalf of a message that failed to
// open.
//
// It carries no refuseIfErased of its own, and does not need one: Zeroize erases every
// window entry of every ratchet and DELETES it, so an erased tree holds nothing this could
// find and the map read below already answers no. It has nothing to report through either
// way -- being unable to report is what "total" means here.
func (self *SecretTree) EraseMessageKey(contentType ContentType, leaf LeafIndex, generation uint32) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	kind, err := ratchetTypeOf(contentType)
	if err != nil {
		return
	}
	r, ok := self.ratchets[ratchetKey{leaf: leaf, kind: kind}]
	if !ok {
		return
	}
	r.eraseKey(generation)
}

// SenderDataKeyNonce derives the AEAD key and nonce protecting a PrivateMessage's sender
// data, per RFC 9420 section 6.3.2:
//
//	ciphertext_sample = ciphertext[0..KDF.Nh-1]   (all of it when it is shorter)
//	sender_data_key   = ExpandWithLabel(sender_data_secret, "key",   ciphertext_sample, AEAD.Nk)
//	sender_data_nonce = ExpandWithLabel(sender_data_secret, "nonce", ciphertext_sample, AEAD.Nn)
//
// The sample is what stops one sender_data_secret from producing one key and nonce for every
// message of the epoch. That secret does not ratchet -- every member holds the same one for
// the whole epoch -- so without the sample every sender_data header would be sealed under a
// single key and nonce pair, which is a keystream reused across every message anyone sends.
//
// Two things about the sample are values rather than failures, which is why they are stated
// here and pinned against the published corpus rather than left to a length check. A sample
// taken at the wrong length, or from the wrong offset, is still real ciphertext, so it derives
// a well formed key of exactly the right width that simply does not open anything -- and
// against a peer that made the same mistake it interoperates. And a ciphertext shorter than
// Nh is used WHOLE rather than padded: padding would make two different short ciphertexts
// sample identically, which is the reuse the sample exists to prevent, reintroduced at the
// short end.
//
// Every length is read off the provider. Both registered suites fix Nn at 12 and one of them
// fixes Nk at 32, which is also Nh, so inside this registry a written down number and a read
// of the provider are indistinguishable -- which is what the synthetic suite in this
// construction's own width test exists to separate.
func SenderDataKeyNonce(crypto CryptoProvider, senderDataSecret []byte, ciphertext []byte) (key []byte, nonce []byte, err error) {
	if crypto == nil {
		return nil, nil, fmt.Errorf("%w: the sender data key and nonce are two expansions through it", ErrNilCryptoProvider)
	}
	nh := crypto.HashSize()
	if len(senderDataSecret) != nh {
		return nil, nil, fmt.Errorf("%w: sender data secret is %d bytes, want %d",
			ErrSecretLength, len(senderDataSecret), nh)
	}
	sample := ciphertext
	if len(sample) > nh {
		sample = sample[:nh]
	}
	key = crypto.ExpandWithLabel(senderDataSecret, "key", sample, crypto.KeySize())
	nonce = crypto.ExpandWithLabel(senderDataSecret, "nonce", sample, crypto.NonceSize())
	return key, nonce, nil
}
