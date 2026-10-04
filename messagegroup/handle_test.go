package messagegroup

import (
	"encoding/binary"
	"errors"
	"fmt"
	"go/ast"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/connect/mls"
)

// The vectors, computed outside this tree with python's hmac and hashlib and checked here
// against keyschedule_test.go's RFC 5869 reference as well.
//
// THE DERIVATION, so a reader can re-derive every octet without running this package. The root
// is keyschedule_test.go's, whose own derivation is written out there:
//
//	storage_root[0]  = 62215de7bddcea7e2c4047ff6bb94f8d18262fc8b3f3648134bb7d44158ff84d
//
//	group_handle_key = HKDF-Expand(storage_root[0], "gh/v1", 32)
//	                 = 055505841d44bc6facf297fd79e5f6a56c56a2f98e3db503ce971c4747792b09
//
//	sender_handle(leaf) = HKDF-Expand(group_handle_key, "sh/v1" | 00000004 | u32(leaf), 16)
//	  leaf 0          -> 8f5de131289090faef3a7bcbf254bbc6   info 73682f76310000000400000000
//	  leaf 1          -> 42125b55b0fd5f50e76ca4265338d00b   info 73682f76310000000400000001
//	  leaf 7          -> 3cf7a0467604f64bfa2ec484db8345e8   info 73682f76310000000400000007
//	  leaf 0xffffffff -> 1c3e2e142a21e15a555e1904b440f94c   info 73682f763100000004ffffffff
//
//	wrap_target_handle(epoch, leaf) = HKDF-Expand(group_handle_key,
//	                                      "wt/v1" | u64(epoch) | u32(leaf), 16)
//	  (0, 0)          -> a5efcfa3de97c83f86b9fae0849f5ef6   info 77742f7631000000000000000000000000
//	  (1, 0)          -> 84257bd51e7d0cd53a1c5b5a77de205b   info 77742f7631000000000000000100000000
//	  (0, 1)          -> fd5e35e16341698af5cc69dca1223990   info 77742f7631000000000000000000000001
//	  (7, 3)          -> 9287ccbeb9e0b1fee02780aacd7cf3f4   info 77742f7631000000000000000700000003
//	  (2^64-1, 2^32-1)-> 2bb4e49e93e06a0b9a1844462d4cb388   info 77742f7631ffffffffffffffffffffffff
//
// Note the two info strings side by side: the sender handle's leaf index is LENGTH PREFIXED --
// 00 00 00 04 then the four octets -- and the wrap target's is not. That asymmetry is MASTER's
// and is open item M1-8; a vector for each is what makes it hold.
const groupHandleKeyKatHex = "055505841d44bc6facf297fd79e5f6a56c56a2f98e3db503ce971c4747792b09"

var senderHandleKat = map[uint32]string{
	0:          "8f5de131289090faef3a7bcbf254bbc6",
	1:          "42125b55b0fd5f50e76ca4265338d00b",
	7:          "3cf7a0467604f64bfa2ec484db8345e8",
	0xFFFFFFFF: "1c3e2e142a21e15a555e1904b440f94c",
}

var wrapTargetHandleKat = []struct {
	epoch uint64
	leaf  uint32
	hex   string
}{
	{epoch: 0, leaf: 0, hex: "a5efcfa3de97c83f86b9fae0849f5ef6"},
	{epoch: 1, leaf: 0, hex: "84257bd51e7d0cd53a1c5b5a77de205b"},
	{epoch: 0, leaf: 1, hex: "fd5e35e16341698af5cc69dca1223990"},
	{epoch: 7, leaf: 3, hex: "9287ccbeb9e0b1fee02780aacd7cf3f4"},
	{epoch: 0xFFFFFFFFFFFFFFFF, leaf: 0xFFFFFFFF, hex: "2bb4e49e93e06a0b9a1844462d4cb388"},
}

// The epoch zero storage root every vector above hangs off.
func handleKatRoot() []byte {
	mlsSecret, pqSecret := keyScheduleKatInputs()
	return StorageRoot(mlsSecret, pqSecret)
}

// Property 1, and the KAT for all three: three distinct labels, three distinct outputs, at
// thirty two, sixteen and sixteen octets, each pinned against an independent expansion.
func TestTheThreeHandleDerivationsAreDistinctAndPinned(t *testing.T) {
	root := handleKatRoot()
	key := GroupHandleKey(root)
	want := mustKeyScheduleHex(t, groupHandleKeyKatHex)
	if reference := keyScheduleReferenceExpand(root, []byte(groupHandleKeyInfo), groupHandleKeyBytes); string(reference) != string(want) {
		t.Fatalf("RFC 5869 written out in keyschedule_test.go gives %x for %q and the pinned vector is %x", reference, groupHandleKeyInfo, want)
	}
	if string(key) != string(want) {
		t.Errorf("GroupHandleKey(storage_root[0]) = %x, want %x", key, want)
	}
	if len(key) != groupHandleKeyBytes {
		t.Errorf("the group handle key is %d octets, want %d", len(key), groupHandleKeyBytes)
	}
	for leaf, pinned := range senderHandleKat {
		got := SenderHandle(key, leaf)
		if got != [16]byte(mustKeyScheduleHex(t, pinned)) {
			t.Errorf("SenderHandle(gh, %d) = %x, want %s", leaf, got, pinned)
		}
	}
	for _, pinned := range wrapTargetHandleKat {
		got := WrapTargetHandle(key, pinned.epoch, pinned.leaf)
		if got != [16]byte(mustKeyScheduleHex(t, pinned.hex)) {
			t.Errorf("WrapTargetHandle(gh, %d, %d) = %x, want %s", pinned.epoch, pinned.leaf, got, pinned.hex)
		}
	}
	// the three labels are three distinct constants and no one of them is a prefix of another
	labels := []string{groupHandleKeyInfo, senderHandleInfo, wrapTargetHandleInfo}
	for i := range labels {
		for j := range labels {
			if i == j {
				continue
			}
			shorter := min(len(labels[i]), len(labels[j]))
			if labels[i][:shorter] == labels[j][:shorter] {
				t.Errorf("%q and %q agree over the whole of the shorter one", labels[i], labels[j])
			}
		}
	}
	// and three distinct outputs from the same key, which is what the labels buy
	sender := SenderHandle(key, 3)
	wrap := WrapTargetHandle(key, 0, 3)
	if sender == wrap {
		t.Error("the sender handle and the wrap target handle of one leaf are the same sixteen octets, so their two labels separate nothing")
	}
	if string(key[:handleBytes]) == string(sender[:]) || string(key[:handleBytes]) == string(wrap[:]) {
		t.Error("a handle is the first sixteen octets of the group handle key, so it is a truncation rather than a derivation")
	}
}

// Property 1's other half, and the in-scope reading of the epoch zero obligation: the group
// handle key is a function of the root it is handed.
//
// The obligation itself -- that the root be epoch ZERO's and be persisted for the life of the
// group -- is a property of the CALLER, which task 10 writes; there is no site inside this
// function that could get it wrong, because the root arrives as an argument. What is holdable
// here is the consequence that makes the caller's mistake fatal rather than survivable: two
// epochs' roots give two different keys, so every handle in the group moves, no member can
// compute another's, and every write is refused by a server that cannot resolve the sender.
func TestTheGroupHandleKeyIsAFunctionOfTheRootItIsGiven(t *testing.T) {
	epochZero := handleKatRoot()
	mlsSecret, pqSecret := keyScheduleKatInputs()
	// a plausible second epoch: the same members, one commit later, a different pq contribution
	epochOne := StorageRoot(mlsSecret, append(slices.Clone(pqSecret[:31]), 0xFF))
	if string(epochZero) == string(epochOne) {
		t.Fatal("the two epoch roots are equal, so this reading cannot see a key derived from the wrong one")
	}
	zeroKey := GroupHandleKey(epochZero)
	oneKey := GroupHandleKey(epochOne)
	if string(zeroKey) == string(oneKey) {
		t.Fatal("GroupHandleKey does not depend on the root it is handed, so a caller reaching for the current epoch's root instead of epoch zero's would be indistinguishable from a correct one")
	}
	for leaf := uint32(0); leaf < 4; leaf++ {
		if SenderHandle(zeroKey, leaf) == SenderHandle(oneKey, leaf) {
			t.Errorf("leaf %d has the same sender handle under two epochs' group handle keys", leaf)
		}
	}
}

// Property 2: the sender handle depends on the leaf and on nothing else, and no two leaves this
// group can reach collide.
//
// The range is derived from connect/mls's own lifecycle ceilings rather than picked: a group
// holds at most MaxGroupMembers identities and each at most MaxDeviceLeavesPerIdentity device
// leaves, so the largest leaf index a v1 group can reach is their product.
func TestSenderHandleSeparatesEveryLeafThisGroupCanReach(t *testing.T) {
	key := GroupHandleKey(handleKatRoot())
	reach := mls.MaxGroupMembers * mls.MaxDeviceLeavesPerIdentity
	if reach < 4000 {
		t.Fatalf("the derived reach is %d leaves; connect/mls gives %d members times %d device leaves and a reach this small is a ceiling that stopped deriving",
			reach, mls.MaxGroupMembers, mls.MaxDeviceLeavesPerIdentity)
	}
	seen := make(map[[16]byte]uint32, reach)
	for leaf := uint32(0); int(leaf) < reach; leaf++ {
		handle := SenderHandle(key, leaf)
		if previous, collided := seen[handle]; collided {
			t.Fatalf("leaves %d and %d share the sender handle %x", previous, leaf, handle)
		}
		seen[handle] = leaf
	}
	// and the extremes, which are outside the reach and must still separate
	for _, leaf := range []uint32{0xFFFFFFFE, 0xFFFFFFFF} {
		handle := SenderHandle(key, leaf)
		if previous, collided := seen[handle]; collided {
			t.Errorf("leaf %d collides with leaf %d", leaf, previous)
		}
		seen[handle] = leaf
	}
	// on nothing else: the same key and leaf give the same handle every time, and a different
	// key gives a different one
	for leaf := uint32(0); leaf < 8; leaf++ {
		if SenderHandle(key, leaf) != SenderHandle(key, leaf) {
			t.Fatalf("SenderHandle is not a function of its arguments at leaf %d", leaf)
		}
	}
	other := GroupHandleKey(append(slices.Clone(handleKatRoot()[:31]), 0x00))
	if SenderHandle(key, 0) == SenderHandle(other, 0) {
		t.Error("the sender handle does not depend on the group handle key")
	}
}

// Property 3: the wrap target handle depends on the epoch AND the leaf, and the snapshot's leaf
// index is computed rather than special cased.
func TestWrapTargetHandleDependsOnBothTheEpochAndTheLeaf(t *testing.T) {
	key := GroupHandleKey(handleKatRoot())
	seen := map[[16]byte]string{}
	for epoch := uint64(0); epoch < 24; epoch++ {
		for leaf := uint32(0); leaf < 24; leaf++ {
			handle := WrapTargetHandle(key, epoch, leaf)
			at := fmt.Sprintf("epoch %d leaf %d", epoch, leaf)
			if previous, collided := seen[handle]; collided {
				t.Fatalf("%s and %s share the wrap target handle %x", previous, at, handle)
			}
			seen[handle] = at
		}
	}
	// the same leaf at two epochs is two targets, which is what stops the server following one
	// device across a group's life
	if WrapTargetHandle(key, 0, 5) == WrapTargetHandle(key, 1, 5) {
		t.Error("the wrap target handle does not depend on the epoch, so one device keeps one address forever")
	}
	if WrapTargetHandle(key, 5, 0) == WrapTargetHandle(key, 5, 1) {
		t.Error("the wrap target handle does not depend on the leaf")
	}
	// the snapshot's leaf index is an ordinary value to this derivation: it is what the pinned
	// vector for (2^64-1, 2^32-1) says, and it equals the independent expansion rather than any
	// constant this file could have branched to.
	const snapshotLeaf = uint32(0xFFFFFFFF)
	for _, epoch := range []uint64{0, 1, 9} {
		info := append([]byte(wrapTargetHandleInfo), binary.BigEndian.AppendUint64(nil, epoch)...)
		info = binary.BigEndian.AppendUint32(info, snapshotLeaf)
		want := keyScheduleReferenceExpand(key, info, handleBytes)
		if got := WrapTargetHandle(key, epoch, snapshotLeaf); got != [16]byte(want) {
			t.Errorf("WrapTargetHandle(gh, %d, 0x%08x) = %x and the independent expansion of the same info gives %x; the snapshot's leaf index is special cased here",
				epoch, snapshotLeaf, got, want)
		}
	}
}

// Property 4: every derivation that binds a leaf index agrees about what LP(leaf_index) means,
// because the ones that length prefix it route through the one helper and the one that does not
// says so.
//
// THE CLASS IS DERIVED FROM THE PROPERTY AND NOT FROM A PARAMETER NAME, and that is this gate's
// history rather than a preference. The version this replaces required a uint32 parameter whose
// NAME contained "leaf". Measured on this package's own source: an exported
// RecordKeyZero(classKey []byte, index uint32) taking the OPPOSITE, minimal encoding reading of
// LP -- with its own inline WriteOpaqueLP, five octets where the helper produces eight, so the two
// derivations disagree on the wire -- landed with every gate in the tree green, because it spelled
// its parameter "index". Task 5's record_key[0] was the next commit and was the second wire
// visible consumer of that unruled reading.
//
// So the class is: every production declaration of this package that takes a uint32 AND reaches
// the key schedule, transitively through this package's own calls. A leaf index is the only
// uint32 any derivation here binds, the reachability is what says "this number goes into a key",
// and neither half can be satisfied by choosing a name.
//
// The table is held in BOTH directions, and it is a table rather than an assertion because
// MASTER writes the members differently: sender_handle length prefixes the index and
// wrap_target_handle writes it raw. A gate that demanded one reading of both would be a gate
// against the spec. What it demands instead is that every member declare which reading it takes
// and that the length prefixing members share one implementation of it, so open item M1-8's
// ruling is one edit.
var handleLeafIndexReadings = map[string]string{
	"RecordKeyZero": "LP -- MASTER section 8.1 writes record_key[0] = HKDF-Expand(class_key, " +
		"\"sender/v1\" | LP(leaf_index), 32), the same shape as sender_handle and through the same helper",
	"NewSenderRatchet": "LP -- it binds the leaf only through RecordKeyZero, so its reading is that " +
		"one, and a second spelling here would be a ladder head no peer reproduces",
	"NewReceiverRatchet": "LP -- it binds the leaf only through RecordKeyZero, and a receiver whose " +
		"ladder head disagreed with the sender's would open nothing at all",
	"SenderHandle": "LP -- MASTER section 8 writes sender_handle as HKDF-Expand(group_handle_key, " +
		"\"sh/v1\" | LP(leaf_index), 16), and this is the one place in the project where LP wraps an integer",
	"WrapTargetHandle": "raw -- section 5.11 writes wrap_target_handle with u32(leaf_index) and no length " +
		"prefix at all, which is the asymmetry open item M1-8 is about",
	"TrackSender": "LP -- it binds the leaf only through SenderHandle and NewReceiverRatchet, so its " +
		"reading is theirs, and a receiver installed under a handle the sender does not compute is a ladder nothing routes to",
	"trackSenderOnLoop": "LP -- TrackSender's body, and the same reading for the same reason",
	"TrackSenderAt": "LP -- TrackSender for a prior epoch, ledger item 241; it binds the leaf only " +
		"through SenderHandle and NewReceiverRatchet exactly as TrackSender does, and a prior epoch's " +
		"ladder installed under a handle the sender does not compute is a ladder nothing routes to",
	"trackSenderAtOnLoop": "LP -- TrackSenderAt's body, and the same reading for the same reason",
	"refuseFrameBindingsOnLoop": "LP -- it binds the leaf only through SenderHandle, because MASTER " +
		"section 8.4.3's R1 is the comparison of the handle the frame's signer expands to against the " +
		"handle the record carries, and a second spelling here would refuse every honest record",
	// THE THIRD READING, AND IT IS NEITHER. It is written out rather than left off because the
	// class above is derived from reachability -- takes a uint32 AND reaches the key schedule --
	// and item 242's R4 is the first member of it that takes a leaf index and puts it into no
	// derivation whatever. Leaving the row off would have been the only way to say that, and an
	// absent row is what this gate exists to refuse.
	"RoleAt": "neither -- ledger item 242's R4 reads the identity and role standing at a leaf of " +
		"ONE EPOCH'S ratchet tree, so the number is a tree position and a map key: it is compared " +
		"against a member's LeafIndex and used to look an entry up, and no octet of it is written " +
		"into any expansion. It is in this class because it ROUTES THROUGH scheduleForOnLoop, whose " +
		"prior-epoch arm rebuilds that epoch's class keys from a storage root -- reachability, which " +
		"is the property, and not a binding. Two declarations carry the name, the seam's projection " +
		"and the session's door, and the reading is the same for both",
	"roleAtOnLoop": "neither -- RoleAt's body, and the same reading for the same reason",
	"WrapRecordKeyZero": "LP -- MASTER section 8.2's ruling of 2026-09-13 puts env_key[k] where the " +
		"class key stands at the head of the device wrap's ladder and changes NOTHING below the " +
		"root: record_key[0] = HKDF-Expand(env_key[k], \"sender/v1\" | LP(leaf_index), 32) is " +
		"RecordKeyZero's own line under a different first argument. It is in this class by reaching " +
		"RecordKeyZero and it shares the one helper through it, which is the point -- a wrap ladder " +
		"that spelled LP(leaf_index) for itself would be a second reading of the same octets in the " +
		"one place where the handle a peer computes has to agree with it",
}

func TestEveryLeafIndexDerivationDeclaresItsReadingAndSharesOneHelper(t *testing.T) {
	_, sources := messagegroupProductionSources(t)
	reachesTheKdf := recordKeyKdfReachingFunctions(sources)
	reachesTheHelper := messagegroupFunctionsReaching(sources, []string{"leafIndexLP"})
	members := []string{}
	callsHelper := map[string]bool{}
	for _, source := range sources {
		for _, declaration := range source.parsed.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Body == nil {
				continue
			}
			name := function.Name.Name
			if !handleTakesAUint32(function) || !reachesTheKdf[name] {
				continue
			}
			members = append(members, name)
			callsHelper[name] = reachesTheHelper[name]
		}
	}
	slices.Sort(members)
	if len(members) == 0 {
		t.Fatal("no production function of this package binds a uint32 into a derivation, so this gate held nothing to one reading of LP(leaf_index)")
	}
	for _, name := range members {
		reading, hasRow := handleLeafIndexReadings[name]
		if !hasRow {
			t.Errorf("%s expands under a leaf index and handleLeafIndexReadings has no row for it; open item M1-8 is wire visible and every member of this class owes a stated reading",
				name)
			continue
		}
		wantsLP := strings.HasPrefix(reading, "LP")
		if wantsLP && !callsHelper[name] {
			t.Errorf("%s is declared to length prefix its leaf index and does not call leafIndexLP; a second spelling of LP(leaf_index) is how the two derivations come to disagree", name)
		}
		if !wantsLP && callsHelper[name] {
			t.Errorf("%s is declared to write its leaf index raw and calls leafIndexLP", name)
		}
	}
	for name := range handleLeafIndexReadings {
		if !slices.Contains(members, name) {
			t.Errorf("handleLeafIndexReadings has a row for %s, which no longer expands under a leaf index; a row that outlived its call site reads as coverage",
				name)
		}
	}
	// and the helper is the ONLY place a length prefix is written, and the only place besides the
	// one declared raw reading where a uint32 becomes octets at all. Both halves are read off the
	// CALLS -- a length prefix written, a thirty two bit encoding spelled -- rather than off an
	// argument name, because a second reading arrives with whatever names its author chooses.
	// THE CLASS HERE IS "a length prefix over an INDEX" AND NOT "a length prefix", and the
	// narrowing is recorded rather than made quietly. It used to be every declaration calling
	// WriteOpaqueLP, which was the class derived from the one INSTANCE that existed when it was
	// written -- and the moment task 11's body padder landed, a function that length prefixes a
	// MESSAGE BODY failed a gate whose stated property is about LP(leaf_index). Deriving from
	// the property instead: a member of this class writes a length prefix AND has an integer in
	// reach to write it over, read off the calls and the signature rather than off a name. A
	// padder over a []byte is not in it; a second reading of LP(leaf_index) under any name is.
	prefixingAnIndex, prefixingAnything, encoding := []string{}, []string{}, []string{}
	for _, source := range sources {
		for _, declaration := range source.parsed.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Body == nil {
				continue
			}
			writesLP := handleWritesALengthPrefix(function.Body)
			if writesLP {
				prefixingAnything = append(prefixingAnything, function.Name.Name)
			}
			if writesLP && (handleEncodesAUint32(function.Body) || handleTakesAUint32(function)) {
				prefixingAnIndex = append(prefixingAnIndex, function.Name.Name)
			}
			if handleEncodesAUint32(function.Body) {
				encoding = append(encoding, function.Name.Name)
			}
		}
	}
	slices.Sort(prefixingAnIndex)
	slices.Sort(prefixingAnything)
	slices.Sort(encoding)
	if len(prefixingAnything) == 0 {
		t.Fatal("nothing in this package writes a length prefix at all, so the matcher this half rests on has stopped matching and the whole half is reporting clean having read nothing")
	}
	if !slices.Equal(prefixingAnIndex, []string{"leafIndexLP"}) {
		t.Errorf("a length prefix is written over an index in %v; there is one reading of LP(leaf_index) in this package and it lives in leafIndexLP so that M1-8's ruling is a single edit",
			prefixingAnIndex)
	}
	if len(encoding) == 0 {
		t.Fatal("nothing in this package encodes a uint32, so this half of the gate read nothing")
	}
	for _, name := range encoding {
		if name == "leafIndexLP" {
			continue
		}
		reading, hasRow := handleLeafIndexReadings[name]
		if !hasRow || !strings.HasPrefix(reading, "raw") {
			t.Errorf("%s turns a uint32 into octets itself and is not the one helper that reads LP(leaf_index); only a member declared to write its index RAW may spell its own encoding",
				name)
		}
	}
	// the reading itself: eight octets, the length 00 00 00 04 then the index
	for _, leaf := range []uint32{0, 1, 7, 0xFFFFFFFF} {
		got := leafIndexLP(leaf)
		want := append([]byte{0x00, 0x00, 0x00, 0x04}, binary.BigEndian.AppendUint32(nil, leaf)...)
		if string(got) != string(want) {
			t.Errorf("leafIndexLP(%d) = %x, want %x", leaf, got, want)
		}
	}
}

// Whether a declaration takes a uint32 parameter, whatever it is called.
//
// The NAME is deliberately not read. A leaf index is the only uint32 any derivation in this
// package binds, and the previous version of this predicate -- which required the name to contain
// "leaf" -- let a second, contradicting reading of LP(leaf_index) into production under the name
// "index" with every gate in the tree green.
func handleTakesAUint32(function *ast.FuncDecl) bool {
	if function.Type.Params == nil {
		return false
	}
	for _, parameter := range function.Type.Params.List {
		identifier, isIdentifier := parameter.Type.(*ast.Ident)
		if isIdentifier && identifier.Name == "uint32" && len(parameter.Names) != 0 {
			return true
		}
	}
	return false
}

// Whether a body writes a length prefix at all.
//
// WriteOpaqueLP is mls/syntax's one LP implementation and is deliberately not WriteOpaque, mls's
// varint: codec.go's header and mls/syntax/encode.go both say the two are never interchangeable.
// Any call to it outside leafIndexLP is a second place LP is spelled.
func handleWritesALengthPrefix(body ast.Node) bool {
	return slices.Contains(keyScheduleCalleeNames(body), "WriteOpaqueLP")
}

// Whether a body turns a uint32 into octets.
//
// Derived from the SHAPE of the callee name rather than from a list of the three functions this
// tree happens to use: anything ending in Uint32 is a thirty two bit encoding or decoding,
// whichever package it comes from, so binary.BigEndian.AppendUint32, binary.BigEndian.PutUint32
// and the syntax writer's WriteUint32 are all read, and so is one this module has not got yet.
func handleEncodesAUint32(body ast.Node) bool {
	for _, callee := range keyScheduleCalleeNames(body) {
		if strings.HasSuffix(callee, "Uint32") {
			return true
		}
	}
	return false
}

// Property 5: a group handle key that is not thirty two octets is refused by both handles, with
// the sentinel as the panic value so a caller that recovers can name what it caught.
func TestBothHandlesRefuseAGroupHandleKeyOfTheWrongWidth(t *testing.T) {
	for _, width := range []int{0, 1, 15, 16, 31, 33, 64} {
		key := make([]byte, width)
		for _, refusal := range []struct {
			name string
			call func()
		}{
			{name: "SenderHandle", call: func() { SenderHandle(key, 3) }},
			{name: "WrapTargetHandle", call: func() { WrapTargetHandle(key, 1, 3) }},
		} {
			caught := handleRecoveredFrom(refusal.call)
			if caught == nil {
				t.Errorf("%s accepted a %d octet group handle key; a handle derived from a truncated key is a well formed handle no other member computes",
					refusal.name, width)
				continue
			}
			if !errors.Is(caught, ErrGroupHandleKeyLength) {
				t.Errorf("%s refused a %d octet group handle key with %v, want ErrGroupHandleKeyLength", refusal.name, width, caught)
			}
		}
	}
	// and the correct width is not refused
	key := GroupHandleKey(handleKatRoot())
	if caught := handleRecoveredFrom(func() { SenderHandle(key, 3) }); caught != nil {
		t.Errorf("SenderHandle refused a thirty two octet key with %v", caught)
	}
	if caught := handleRecoveredFrom(func() { WrapTargetHandle(key, 1, 3) }); caught != nil {
		t.Errorf("WrapTargetHandle refused a thirty two octet key with %v", caught)
	}
}

// The error one call panicked with, or nil.
func handleRecoveredFrom(call func()) (caught error) {
	defer func() {
		recovered := recover()
		if recovered == nil {
			return
		}
		if asError, isError := recovered.(error); isError {
			caught = asError
			return
		}
		caught = errors.New("messagegroup: panicked with a non error value")
	}()
	call()
	return nil
}
