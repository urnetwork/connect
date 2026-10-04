package protocol_test

// THE GATE RULING 33 IS, AND THE MEASUREMENT IT WAS TAKEN ON.
//
// Item 244: the commit that removes a member is sealed at epoch n, is fetchable
// under read_key[n], and — under Spec B §5.4 as it was RULED — carries read_key[n+1]
// and write_key[n+1] in the clear inside a structure the server serves back verbatim.
// So a removed member ladders every future epoch forever. Reproduced against the
// message server twice: a fetch under a learned read key answered REASON_OK, and a
// forged write under a learned write key answered REASON_OK.
//
// Ruling 27 took the two keys out of the served attachment and replaced them with
// LP(H(epoch_keys)). Ruling 33 decided where they go instead, and it reversed the
// removal track's own step 3, which had said "connect/protocol: the two `Record`
// fields — use 15 and 16". The reason is a fact about this file that is checked below
// rather than recited: `Record` is the SERVER→CLIENT type in six places against two
// client→server carriers. Keys on `Record` would be six serve paths that each have to
// remember to clear them — item 244 re-opened once per path — where a request field
// is a type the response side structurally cannot carry.
//
// THIS FILE IS THE GATE THAT KEEPS THAT TRUE, AND THE PROPERTY IS ABOUT KEYS AND NOT
// ABOUT ONE TYPE NAME. It said otherwise until 2026-09-22, and the header sentence it
// said it in — "any later step that puts a key back on a served type fails here by
// name" — was false as written. Measured: `bytes write_key = 6; bytes read_key = 7;`
// added to FetchResponse, regenerated, whole package green. That is item 244's own
// defect — the epoch keys on the server→client fetch answer — re-created with every
// gate here passing, because nothing looked at a served field's NAME, and the one
// thing that looked at a served field's TYPE looked for a single simple name, reached
// only through the `oneof body` arms, and never through a map. FOUR REPAIRS:
//
//	1. the closure starts AT the envelopes, not at their oneof arms, so a non-oneof
//	   field of MessageServerResponse is inside the served set (it was in NEITHER set)
//	2. carriersOf descends into map values, so EpochKeyDelivery as a map value is a
//	   carrier and not the synthetic FooEntry the match used to compare against
//	3. every field of every served message is checked BY NAME against namesAKey
//	4. every field of every served message is checked BY TYPE, so a key one level out
//	   is caught without the carrier having to name it
//
// WHAT EACH REPAIR ACTUALLY CATCHES, MEASURED BY UNDOING IT — and it is not the tidy
// "four independent mechanisms" the list reads like, so the list does not say that.
// Two mutants: M1 is `bytes write_key = 6; bytes read_key = 7;` on FetchResponse, M2
// is `map<uint64, EpochKeyDelivery> epoch_keys = 4` on MessageServerResponse outside
// its oneof. Both regenerated with protoc 35.1 and run against the whole package.
//
//	M1 -> RED in the NAME check, and in the TYPE check through
//	      MessageServerResponse.fetch, whose type now declares a key. It is GREEN in
//	      both gates that look for the type EpochKeyDelivery, and that is the finding
//	      restated rather than a gap: a raw key is not an EpochKeyDelivery.
//	M2 -> RED in all four.
//
// Then, holding M2 and undoing one repair at a time, only repair 2 is individually
// necessary for the gate it belongs to. The other three still go red, for reasons
// worth knowing rather than worth assuming:
//
//	undo 1 -> TestNoServerToClientMessageCarriesTheEpochKeys is still red, but NOT on
//	          its ruling-33 assertion: the three envelopes fall back into `neither`,
//	          which is undispositioned, and the partition guard fires. That guard is
//	          what makes the reseeding non-revertible; the semantic assertion alone
//	          would have gone quiet.
//	undo 2 -> TestTheEpochKeysAreCarriedByExactlyTheTwoRequests goes GREEN. The IsMap
//	          branch is the whole of that catch.
//	undo 3 -> the NAME check is still red, through the COMPLEMENT and not the
//	          predicate: `epoch_keys` contains "key", the narrowed suffix no longer
//	          flags it, and an unlisted narrowing is an error. The enumeration of what
//	          a narrowing removed is load-bearing here, not commentary.
//	undo 4 -> the TYPE check is still red by ACCIDENT, and the accident is named so
//	          nobody banks on it: every synthetic map entry declares a field called
//	          `key`, which namesAKey matches, so the broken walk trips over the entry
//	          it should not have been looking at.
//
// EVERY SET BELOW IS DERIVED FROM THE COMPILED DESCRIPTOR, and the partition each
// derivation induces is PRINTED — what a narrowing removed is the thing a narrowed
// gate stops seeing, and an enumeration whose complement nobody looks at is an empty
// search that passes silently.
//
// AND THE SAME CLASS AGAIN, ONE COMMIT LATER, IN THIS FILE'S ONE UNPRINTED NARROWING.
//
// The four repairs above left keyNamedFields, keyTypedFields and carriersOf all
// iterating topLevelMessages — and declaresAKeyField is reached only through
// keyTypedFields — so a key on a NESTED type was covered by nothing but a complement
// loop in servedAndSubmitted
// — and that loop excused any simple name ending in "Entry". Reproduced, protoc 35.1,
// inside `message FetchResponse {`:
//
//	message ShimEntry   { KeyBagEntry bag = 1; }
//	message KeyBagEntry { bytes write_key = 1; bytes read_key = 2; }
//	ShimEntry shim = 20;
//
//	-> ok github.com/urnetwork/connect/protocol 0.379s
//
// Item 244's own defect for the second time in two commits, with all four key gates
// green. TWO MORE REPAIRS, and neither is a wider name match:
//
//	5. the key checks and carriersOf walk types AT ANY DEPTH — walkFrom descends by
//	   REFERENCE and by DECLARATION — instead of topLevelMessages
//	6. the exemption is md.IsMapEntry() and not a name suffix, with its complement
//	   printed and asserted against the map fields that produce it, both ways
//
// WHAT EACH REPAIR CATCHES, MEASURED AND NOT ASSUMED. Mutant M3 is the block above; M4
// is `message Extra { EpochKeyDelivery k = 1; } Extra extra = 20;` inside SubmitRequest,
// a THIRD carrier of the delivery type at depth 1 on the SUBMITTED side, where no served
// check looks at all.
//
//	M3 before repair 5  -> ok 0.379s, every gate green
//	M3 after            -> FAIL TestNoServedMessageCarriesAKeyByName (on
//	                       bringyour.FetchResponse.KeyBagEntry.write_key and .read_key)
//	                       FAIL TestNoServedMessageCarriesATypeThatDeclaresAKey
//	M4 with the descent in carriersOf undone and everything else repaired -> ok 0.404s
//	M4 after            -> FAIL TestTheEpochKeysAreCarriedByExactlyTheTwoRequests, and
//	                       that gate ALONE: the third carrier is on the submitted side
//
// So the descent in carriersOf is individually necessary for M4, measured by undoing it
// rather than by reasoning about it.
//
// WHAT NO MUTANT HERE SHOWS, said rather than left as an implication. (a) Repair 6 is
// not exercised by message.proto: the file declares no map field, so IsMapEntry and the
// old suffix agree on every type in it — which is why
// TestTheKeyCheckWalkDescendsAndSkipsOnlyMapEntries BUILDS a descriptor that has one,
// and the predicate is measured there or nowhere. (b) M3 is reached by walkFrom's
// REFERENCE descent; its DECLARATION descent is reached by no mutant, because a type
// nothing references is not on the wire and a mutant of it would be a finding about an
// unwritten line. It is kept anyway and the reason is WHEN: the only finding it can
// produce is a key-named field declared inside a served type, which ruling 33 forbids
// whether or not a field points at it yet, and catching that at the declaration is one
// commit earlier than catching it at the reference.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/urnetwork/connect/protocol"
)

// The name of the type the epoch keys travel in, and the name of the type they must
// never travel in. Simple names, because the test prints simple names.
const (
	keyDeliveryMessage = "EpochKeyDelivery"
	recordMessage      = "Record"
)

// messageFile is message.proto's compiled file descriptor, reached through a type in
// it rather than through the registry, so this walks the file this package was built
// from and not whatever else happens to be linked into the test binary.
func messageFile() protoreflect.FileDescriptor {
	return (*protocol.MessageServerRequest)(nil).ProtoReflect().Descriptor().ParentFile()
}

// topLevelMessages returns every message declared at the top level of message.proto,
// keyed by simple name.
func topLevelMessages(t *testing.T) map[string]protoreflect.MessageDescriptor {
	t.Helper()
	out := map[string]protoreflect.MessageDescriptor{}
	msgs := messageFile().Messages()
	for i := 0; i < msgs.Len(); i++ {
		md := msgs.Get(i)
		out[string(md.Name())] = md
	}
	if len(out) == 0 {
		t.Fatal("message.proto declares no top-level message; this walk is reading the wrong file descriptor")
	}
	return out
}

// envelopeDirections classifies the URmessage envelopes — the top-level messages that
// carry a `oneof body` — into the direction each one travels.
//
// DERIVED, NOT LISTED, in the same way isAuthenticatorField derives the authenticator
// class: an envelope is a top-level message with a non-synthetic `oneof body`, and it
// is client→server iff its name ends in "Request". Spec B §4.3 names exactly three —
// MessageServerRequest, MessageServerResponse and MessageServerPush — and
// TestTheEnvelopeDirectionDerivationFindsAllThree asserts the derivation finds those
// three and no others, so a fourth envelope added later cannot arrive unclassified
// and a rename cannot silently empty the server→client side.
func envelopeDirections(t *testing.T) (clientToServer, serverToClient []protoreflect.MessageDescriptor) {
	t.Helper()
	all := topLevelMessages(t)
	for _, name := range sortedKeys(all) {
		md := all[name]
		od := md.Oneofs().ByName("body")
		if od == nil || od.IsSynthetic() {
			continue
		}
		if strings.HasSuffix(name, "Request") {
			clientToServer = append(clientToServer, md)
		} else {
			serverToClient = append(serverToClient, md)
		}
	}
	return clientToServer, serverToClient
}

// closureFrom returns every message type reachable from these envelopes, THE
// ENVELOPES THEMSELVES INCLUDED. Reachability is what makes this a derived class:
// nesting a new type inside a served message pulls it in without anybody editing this
// file.
//
// IT STARTS AT THE ENVELOPE AND NOT AT `oneof body`, and that is a reproduced defect
// and not a tidy-up. Seeded with `od.Fields()` — the arms — the three envelopes' OWN
// non-oneof fields were in no closure at all: this file's own log printed
// `neither (4): [MessageServerFragment MessageServerPush MessageServerRequest
// MessageServerResponse]` and nobody read it as the hole it was. A
// `map<uint64, EpochKeyDelivery> epoch_keys = 4` added to MessageServerResponse
// OUTSIDE its oneof left every gate in this file green. An envelope is the most
// served thing here; it was the one thing the served set excluded.
func closureFrom(envelopes []protoreflect.MessageDescriptor) map[string]bool {
	out := map[string]bool{}
	var visit func(md protoreflect.MessageDescriptor)
	visit = func(md protoreflect.MessageDescriptor) {
		name := string(md.Name())
		if out[name] {
			return
		}
		out[name] = true
		fields := md.Fields()
		for i := 0; i < fields.Len(); i++ {
			f := fields.Get(i)
			if sub := f.Message(); sub != nil {
				visit(sub)
			}
			if f.IsMap() {
				if sub := f.MapValue().Message(); sub != nil {
					visit(sub)
				}
			}
		}
	}
	for _, envelope := range envelopes {
		visit(envelope)
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k, in := range m {
		if in {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// allMessageTypes returns every message message.proto declares AT ANY DEPTH, keyed by
// FULL name. topLevelMessages is the file's outermost layer; this is the file.
func allMessageTypes(t *testing.T) map[string]protoreflect.MessageDescriptor {
	t.Helper()
	out := map[string]protoreflect.MessageDescriptor{}
	var visit func(msgs protoreflect.MessageDescriptors)
	visit = func(msgs protoreflect.MessageDescriptors) {
		for i := 0; i < msgs.Len(); i++ {
			md := msgs.Get(i)
			out[string(md.FullName())] = md
			visit(md.Messages())
		}
	}
	visit(messageFile().Messages())
	if len(out) == 0 {
		t.Fatal("message.proto declares no message at all; this walk is reading the wrong file descriptor")
	}
	return out
}

// isTopLevel answers from the name rather than from pointer identity: a top-level
// message's full name has the file's package as its parent, a nested one has its
// enclosing message.
func isTopLevel(md protoreflect.MessageDescriptor) bool {
	return md.FullName().Parent() == messageFile().Package()
}

// typeWalk is what a set of root messages expands to for the key checks below: every
// message type reachable from a root BY REFERENCE and every message type DECLARED inside
// any of those, recursively — partitioned into the types the checks WALK and the
// synthetic map entries they SKIP, with the map fields that produced those entries kept
// so the skip can be asserted from both ends.
//
// KEYED BY FULL NAME, and the map entries are the reason: protoc names a synthetic entry
// after its FIELD, so two messages that each declare a map field called `foo` declare two
// different types both simply named `FooEntry`. A walk keyed on simple names would hold
// one of them and drop the other with nothing to notice.
//
// DECLARATION AS WELL AS REFERENCE, because a type declared inside a served message and
// not yet referenced by any field is one line away from being served and is invisible to
// a reachability walk.
type typeWalk struct {
	checked    map[string]protoreflect.MessageDescriptor
	mapEntries map[string]protoreflect.MessageDescriptor
	mapFields  map[string]string // map field full name -> its synthetic entry's full name
}

// walkFrom is the walk, and THE ONE NARROWING IN IT IS md.IsMapEntry().
//
// A synthetic map entry declares fields literally named `key` and `value`, so checking
// one would make every map field a finding about protoc rather than about this file. The
// predicate is the descriptor's own answer — true of synthetic entries and of nothing
// else — and NOT `strings.HasSuffix(name, "Entry")`, which is what stood here until
// 2026-09-22 and which the header records riding straight through. A skipped entry's
// VALUE type is still reached, by the IsMap branch below.
func walkFrom(roots []protoreflect.MessageDescriptor) typeWalk {
	w := typeWalk{
		checked:    map[string]protoreflect.MessageDescriptor{},
		mapEntries: map[string]protoreflect.MessageDescriptor{},
		mapFields:  map[string]string{},
	}
	seen := map[string]bool{}
	var visit func(md protoreflect.MessageDescriptor)
	visit = func(md protoreflect.MessageDescriptor) {
		full := string(md.FullName())
		if seen[full] {
			return
		}
		seen[full] = true
		if md.IsMapEntry() {
			w.mapEntries[full] = md
		} else {
			w.checked[full] = md
		}
		fields := md.Fields()
		for i := 0; i < fields.Len(); i++ {
			f := fields.Get(i)
			if sub := f.Message(); sub != nil {
				if f.IsMap() {
					w.mapFields[string(f.FullName())] = string(sub.FullName())
				}
				visit(sub)
			}
			if f.IsMap() {
				if sub := f.MapValue().Message(); sub != nil {
					visit(sub)
				}
			}
		}
		nested := md.Messages()
		for i := 0; i < nested.Len(); i++ {
			visit(nested.Get(i))
		}
	}
	for _, root := range roots {
		visit(root)
	}
	return w
}

// walkOfNames expands these TOP-LEVEL message names into the walk above.
func walkOfNames(t *testing.T, names map[string]bool) typeWalk {
	t.Helper()
	all := topLevelMessages(t)
	roots := []protoreflect.MessageDescriptor{}
	for _, name := range sortedKeys(all) {
		if names[name] {
			roots = append(roots, all[name])
		}
	}
	if len(roots) == 0 {
		t.Fatal("no root message was named, so the walk below covers nothing and every check over " +
			"it is a search across an empty set")
	}
	return walkFrom(roots)
}

// TestTheEnvelopeDirectionDerivationFindsAllThree is the floor under every set in
// this file. Without it, a rename that emptied the server→client side would make the
// ruling-33 gate below pass over nothing at all, which is the way a gate stops
// working without failing.
func TestTheEnvelopeDirectionDerivationFindsAllThree(t *testing.T) {
	clientToServer, serverToClient := envelopeDirections(t)
	got := map[string]string{}
	for _, md := range clientToServer {
		got[string(md.Name())] = "client→server"
	}
	for _, md := range serverToClient {
		got[string(md.Name())] = "server→client"
	}
	want := map[string]string{
		"MessageServerRequest":  "client→server",
		"MessageServerResponse": "server→client",
		"MessageServerPush":     "server→client",
	}
	for name, direction := range want {
		if got[name] != direction {
			t.Errorf("the derivation classifies %s as %q; Spec B §4.3 makes it %q",
				name, got[name], direction)
		}
	}
	for name, direction := range got {
		if _, known := want[name]; !known {
			t.Errorf("%s carries a `oneof body`, so this file treats it as a URmessage envelope and "+
				"classified it %q by its name alone. Spec B §4.3 names three envelopes. Decide which "+
				"direction this one travels and write it down here, because the ruling-33 gate is "+
				"only as wide as this classification.", name, direction)
		}
	}
	if len(serverToClient) == 0 {
		t.Fatal("no envelope was classified server→client, so the gate below is asserting something about an empty set")
	}
	t.Logf("envelope directions: %v", got)
}

// unreachedFromAnyEnvelope is the `neither` partition, WRITTEN DOWN WITH A
// DISPOSITION EACH, because a top-level message no envelope reaches is a message this
// file's reachability argument does not cover, and an uncovered message is exactly
// where a key would go to be missed.
//
// There is one, and it is treated as SERVED: a fragment travels back from the server
// on every response large enough to need one.
var unreachedFromAnyEnvelope = map[string]string{
	"MessageServerFragment": "the transport fragment, bound as frame type 1003 in frame.proto and not " +
		"as an arm of any envelope. It travels BOTH directions and carries an envelope's octets " +
		"opaquely in `part`, so it is SERVED and the key checks below cover it.",
}

// servedAndSubmitted partitions message.proto's top-level messages by which envelopes
// reach them, and answers with the set the key checks are about.
//
// THE SERVED SET IS "NOT SUBMITTED-ONLY" AND NOT "SERVED-ONLY". A type in `both`
// (Record) is served. A type in `neither` is not PROVEN unserved — nothing reached it,
// which is an absence of evidence — so each is dispositioned by name above and treated
// as served. Only a type reachable from the client→server envelope and from nothing
// else is a place a key may legitimately sit.
func servedAndSubmitted(t *testing.T) (served, submitted, servedSet map[string]bool) {
	t.Helper()
	clientToServer, serverToClient := envelopeDirections(t)
	served = closureFrom(serverToClient)
	submitted = closureFrom(clientToServer)

	both := map[string]bool{}
	servedOnly := map[string]bool{}
	submittedOnly := map[string]bool{}
	neither := map[string]bool{}
	for _, name := range sortedKeys(topLevelMessages(t)) {
		switch {
		case served[name] && submitted[name]:
			both[name] = true
		case served[name]:
			servedOnly[name] = true
		case submitted[name]:
			submittedOnly[name] = true
		default:
			neither[name] = true
		}
	}
	t.Logf("served only    (%2d): %v", len(servedOnly), sortedSet(servedOnly))
	t.Logf("submitted only (%2d): %v", len(submittedOnly), sortedSet(submittedOnly))
	t.Logf("both           (%2d): %v", len(both), sortedSet(both))
	t.Logf("neither        (%2d): %v", len(neither), sortedSet(neither))

	// THE `neither` PARTITION IS ASSERTED IN BOTH DIRECTIONS. An unexpected member is a
	// message whose direction nobody decided; a missing one means the disposition above
	// is dead text describing a file that has moved.
	for name := range neither {
		if _, dispositioned := unreachedFromAnyEnvelope[name]; !dispositioned {
			t.Errorf("%s is reachable from no envelope, so nothing here knows which direction it "+
				"travels and it lands in the served set by default rather than by decision. Write "+
				"its disposition into unreachedFromAnyEnvelope.", name)
		}
	}
	for name := range unreachedFromAnyEnvelope {
		if !neither[name] {
			t.Errorf("unreachedFromAnyEnvelope dispositions %s, but an envelope now reaches it or it "+
				"is gone. A dead disposition is a sentence about this file that has stopped being true.",
				name)
		}
	}

	servedSet = map[string]bool{}
	for name := range servedOnly {
		servedSet[name] = true
	}
	for name := range both {
		servedSet[name] = true
	}
	for name := range neither {
		servedSet[name] = true
	}
	if len(servedSet) == 0 {
		t.Fatal("the served set is empty, so every key check below is a search over nothing")
	}
	// the inline positive control: Record IS served, which is the fact ruling 33 was
	// taken on. If this fails, the walk is not reaching the served records and every
	// refusal below is vacuous.
	if !servedSet[recordMessage] {
		t.Fatalf("%s is not in the served set, so the walk is not reaching the served records and "+
			"the refusals below are vacuous", recordMessage)
	}
	// and the complement of "top level", WHICH IS NO LONGER AN EXEMPTION AND IS STILL
	// ASSERTED. This loop used to require every member of the served closure to be a
	// top-level message and excused anything whose simple name ended in "Entry" — and
	// since all three key checks iterated topLevelMessages, that one loop was the WHOLE
	// of the coverage of nested types. It excused nothing that exists while a KeyBagEntry
	// two levels down inside FetchResponse carried write_key and read_key through every
	// gate green. The checks now walk nested types directly, so the suffix is gone.
	//
	// WHAT REPLACED IT WAS A t.Logf, AND THAT WAS THE DEFECT. For one commit this
	// partition was printed and nothing asserted it, on the reasoning that the walk below
	// now covers what the guard used to. It does — for a nested type whose fields are
	// NAMED like keys. A nested served type carrying the two keys under any other field
	// names (`message NextEpochBag { bytes wk = 1; bytes rk = 2; }` on FetchResponse) went
	// green here and was RED under the guard it replaced, and no mutant in that commit's
	// table could see the loss, because every one of them used key names.
	//
	// So: PRINTED IS NOT THE PROPERTY. A narrowing is held by being asserted in BOTH
	// directions against a written-down disposition — a member with no entry is a refusal,
	// and an entry nothing needs is a refusal — which is how every other narrowing in this
	// file is held, and is why the map below is here and is empty.
	nested := []string{}
	for _, name := range sortedSet(served) {
		if _, top := topLevelMessages(t)[name]; !top {
			nested = append(nested, name)
		}
	}
	t.Logf("served closure members that are NOT top-level (%d): %v — dispositioned: %d",
		len(nested), nested, len(nestedServedTypes))
	for _, name := range nested {
		why, dispositioned := nestedServedTypes[name]
		if !dispositioned {
			t.Errorf("%s is in the served closure and is not a top-level message, and no entry in "+
				"nestedServedTypes says why that is allowed. A nested served type is how item 244's "+
				"own shape rides back in — the next epoch's two keys, under field names no check "+
				"recognises. Give it an entry with the reason, or take it off the served side.", name)
			continue
		}
		t.Logf("  nested served type %s is allowed: %s", name, why)
	}
	for name := range nestedServedTypes {
		if !containsString(nested, name) {
			t.Errorf("nestedServedTypes says %s is an allowed nested served type and the walk does "+
				"not find it there. A disposition nothing needs is a disposition nobody reads, and "+
				"it is how this map stops describing the file.", name)
		}
	}
	return served, submitted, servedSet
}

// The nested types the served closure is allowed to reach, name -> why. EMPTY, and the
// emptiness is the point: message.proto declares every message at the top level today, so
// the served closure holds no nested type at all and both halves of the check above are
// refusals over nothing. The day that changes, the day says so in a FAILURE rather than in
// a log line -- and whoever adds the first one writes down here why a type the server hands
// back may be reached only through another type, which is the question the suffix exemption
// used to answer with a naming convention.
var nestedServedTypes = map[string]string{}

func containsString(names []string, want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}
	return false
}

// THE RULING 33 GATE: NO SERVER→CLIENT MESSAGE TRANSITIVELY CARRIES AN EpochKeyDelivery.
//
// The set is the transitive closure of every server→client envelope, so a key
// delivery nested three types deep inside a FetchResponse fails here exactly as one
// nested directly on it does — and one hung on the ENVELOPE itself fails too, which
// it did not until the closure was reseeded.
//
// The partition is printed in full — served only, submitted only, both, neither —
// because that is the only way to see that the closure reached anything. A gate that
// searched an empty set for EpochKeyDelivery would pass, and would go on passing for
// as long as it took somebody to put the keys back.
func TestNoServerToClientMessageCarriesTheEpochKeys(t *testing.T) {
	served, submitted, servedSet := servedAndSubmitted(t)

	if served[keyDeliveryMessage] || servedSet[keyDeliveryMessage] {
		t.Errorf("%s is reachable from a server→client envelope. Ruling 33: the epoch keys travel on "+
			"the REQUEST messages and never on anything the server hands back — a served key field is "+
			"item 244 re-opened, once for every serve path that forgets to clear it.", keyDeliveryMessage)
	}
	if !submitted[keyDeliveryMessage] {
		t.Errorf("%s is not reachable from a client→server envelope either, so it is now on no path at "+
			"all and the keys have nowhere to ride", keyDeliveryMessage)
	}
}

// namesAKey is the served-side field-name predicate: the one already applied to
// Record, NARROWED — with the narrowing printed by the test that uses it.
//
// `strings.Contains(name, "key")` is what Record is checked with, and Record carries
// nothing that would trip it falsely. Over the whole served set it does trip:
// Capabilities.read_key_window_seconds is a DURATION, named after the key whose
// retention it bounds. So the predicate is the suffix and not the substring, and
// TestNoServedMessageCarriesAKeyByName prints every served field name that contains
// "key" and that this predicate did NOT flag, asserted in both directions — because a
// narrowing whose complement nobody enumerates is how a gate quietly stops covering
// the thing it is named for.
func namesAKey(name string) bool {
	return name == "key" || strings.HasSuffix(name, "_key") || strings.HasSuffix(name, "_keys")
}

// keyNameExemptions are the fields namesAKey flags that are NOT key material, each
// with its argument, and each ASSERTED USED below.
//
// ONE LIST FOR BOTH CHECKS. The name check partitions its findings against it and is
// where "used" is measured; declaresAKeyField consults it too, so a type whose only
// key-named field is an exempt one does not make every carrier of that type a
// finding. Two lists would drift, and the drift would be silent in the direction that
// matters: an exemption taken on one check and not the other.
var keyNameExemptions = map[string]string{
	"bringyour.HelloResponse.server_keys": "repeated ServerKey — the fleet's Ed25519 PUBLIC signing " +
		"keys and their certification chain (`pub`, `sig_by_previous`, `sig_by_root`). Publishing " +
		"them is the point of the field: a client cannot verify a server signature without them.",
	"bringyour.CapabilityChange.server_keys": "the same set, re-served on rotation. Same argument.",
}

// keyNamedFields returns the fields of every message in this walk that namesAKey flags,
// and, as the complement, the ones whose name merely CONTAINS "key" and that it did not.
//
// IT TAKES THE WALK AND NOT A SET OF TOP-LEVEL NAMES. Until 2026-09-22 it iterated
// topLevelMessages, so a key on a NESTED type was outside it — reproduced, and recorded
// in the header with its protoc output.
func keyNamedFields(t *testing.T, w typeWalk) (flagged, narrowedAway map[string]string) {
	t.Helper()
	flagged = map[string]string{}
	narrowedAway = map[string]string{}
	for _, message := range sortedKeys(w.checked) {
		fields := w.checked[message].Fields()
		for i := 0; i < fields.Len(); i++ {
			f := fields.Get(i)
			name := string(f.Name())
			switch {
			case namesAKey(name):
				flagged[string(f.FullName())] = message
			case strings.Contains(name, "key"):
				narrowedAway[string(f.FullName())] = message
			}
		}
	}
	return flagged, narrowedAway
}

// NO SERVED MESSAGE CARRIES A FIELD THAT NAMES A KEY.
//
// This is the check that was missing, and item 244's own defect is what goes through
// where it is not: `bytes write_key = 6; bytes read_key = 7;` on FetchResponse is the
// served fetch answer handing out the next epoch's keys, in raw octets, naming
// itself — and every other gate in this file passed on it. A key does not have to be
// an EpochKeyDelivery to be a key.
func TestNoServedMessageCarriesAKeyByName(t *testing.T) {
	_, submitted, servedSet := servedAndSubmitted(t)

	w := walkOfNames(t, servedSet)
	t.Logf("the key checks walk %d served types (%d of them nested) and skip %d synthetic map entries",
		len(w.checked), len(w.checked)-countTopLevel(w.checked), len(w.mapEntries))
	flagged, narrowedAway := keyNamedFields(t, w)
	t.Logf("served fields whose name names a key (%d): %v", len(flagged), sortedKeys(flagged))
	t.Logf("served fields containing \"key\" that the SUFFIX predicate narrowed away (%d): %v",
		len(narrowedAway), sortedKeys(narrowedAway))

	// THE COMPLEMENT, ASSERTED IN BOTH DIRECTIONS. Every field the narrowing removed is
	// written down with its reason, so a new one has to be dispositioned rather than
	// silently skipped, and a stale one means this list describes a file that has moved.
	narrowedAwayOnPurpose := map[string]string{
		"bringyour.Capabilities.read_key_window_seconds": "a DURATION — how long this server retains " +
			"each installed read key (§5.3's ninety-day window). It names the key whose retention it " +
			"bounds and carries none.",
	}
	for full := range narrowedAway {
		if _, known := narrowedAwayOnPurpose[full]; !known {
			t.Errorf("%s contains \"key\" and namesAKey did not flag it, so this gate is not looking "+
				"at it. Either it is key material and the predicate is too narrow, or it is not and it "+
				"belongs in narrowedAwayOnPurpose with the argument.", full)
		}
	}
	for full := range narrowedAwayOnPurpose {
		if _, still := narrowedAway[full]; !still {
			t.Errorf("narrowedAwayOnPurpose excuses %s and nothing needs the excuse any more — it was "+
				"renamed, removed, or is now flagged. A dead exemption is a narrowing nobody is "+
				"measuring.", full)
		}
	}

	usedExemption := map[string]bool{}
	for _, full := range sortedKeys(flagged) {
		if _, exempt := keyNameExemptions[full]; exempt {
			usedExemption[full] = true
			continue
		}
		t.Errorf("%s is a field of %s — a type a server→client envelope reaches, or one DECLARED "+
			"inside such a type and one field reference away from being reached — and its name names "+
			"a key. Item 244 IS this shape: the served commit carrying read_key[n+1] and "+
			"write_key[n+1]. Ruling 33 is that nothing served carries one, and a key declared inside "+
			"a served type is not exempt from it for as long as no field happens to point at it. If "+
			"it is not key material, exempt it in keyNameExemptions with the argument; do not rename "+
			"it past the predicate.",
			full, flagged[full])
	}
	for full := range keyNameExemptions {
		if !usedExemption[full] {
			t.Errorf("keyNameExemptions exempts %s and nothing needed the exemption. Either the "+
				"field moved out of the served set or it is gone; either way this is a hole written "+
				"into the gate for a reason that has stopped applying.", full)
		}
	}
	t.Logf("exemptions used (%d of %d): %v", len(usedExemption), len(keyNameExemptions),
		sortedKeys(usedExemption))

	// THE FAILING DIRECTION, IN THE SAME QUERY. The identical predicate over the
	// submitted-only set must find the deliveries, or the refusal above is a predicate
	// that matches nothing rather than a file that carries nothing.
	submittedOnly := map[string]bool{}
	for name := range submitted {
		if !servedSet[name] {
			submittedOnly[name] = true
		}
	}
	control, _ := keyNamedFields(t, walkOfNames(t, submittedOnly))
	t.Logf("CONTROL — submitted-only fields that name a key (%d): %v", len(control), sortedKeys(control))
	for _, want := range []string{
		"bringyour.SubmitRequest.epoch_keys",
		"bringyour.CreateGroupRequest.epoch_keys",
		"bringyour.CreateGroupRequest.bootstrap_write_key",
	} {
		if _, found := control[want]; !found {
			t.Errorf("the control did not find %s, so namesAKey is matching nothing and the "+
				"served-side refusal above proves nothing", want)
		}
	}
}

// declaresAKeyField answers whether this message type itself declares a field that
// names a key — the TYPE-level half of the same property.
// It consults keyNameExemptions for the reason given there: HelloResponse declares
// `server_keys`, so without the exemption every envelope arm that carries a
// HelloResponse would be reported as reaching key material, and the finding would be
// the fleet's public signing keys.
func declaresAKeyField(md protoreflect.MessageDescriptor) []string {
	out := []string{}
	fields := md.Fields()
	for i := 0; i < fields.Len(); i++ {
		f := fields.Get(i)
		if _, exempt := keyNameExemptions[string(f.FullName())]; exempt {
			continue
		}
		if name := string(f.Name()); namesAKey(name) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// keyTypedFields returns the fields of every message in this walk whose MESSAGE TYPE
// declares a key, descending into map values.
//
// IT TAKES THE WALK, for the reason keyNamedFields gives.
func keyTypedFields(t *testing.T, w typeWalk) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, message := range sortedKeys(w.checked) {
		fields := w.checked[message].Fields()
		for i := 0; i < fields.Len(); i++ {
			f := fields.Get(i)
			sub := f.Message()
			// MAP VALUES ARE DESCENDED INTO, and that is a reproduced defect and not a
			// completeness flourish: `map<uint64, EpochKeyDelivery> epoch_keys = 4` is a
			// field whose f.Message() is the SYNTHETIC EpochKeysEntry, so a match that does
			// not call f.IsMap() compares against the wrong descriptor and never fires.
			if f.IsMap() {
				sub = f.MapValue().Message()
			}
			if sub == nil {
				continue
			}
			if declared := declaresAKeyField(sub); len(declared) > 0 {
				out[string(f.FullName())] = declared
			}
		}
	}
	return out
}

// NO SERVED MESSAGE CARRIES A FIELD WHOSE TYPE DECLARES A KEY.
//
// The name check above catches a key the served message names itself. This one
// catches a key ONE LEVEL OUT — a served field of some type that declares the keys,
// under a field name that says nothing — which is what an economy-minded later step
// reaches for once the name check is in the way.
func TestNoServedMessageCarriesATypeThatDeclaresAKey(t *testing.T) {
	_, submitted, servedSet := servedAndSubmitted(t)

	// the positive control, stated before the walk: the predicate flags the delivery
	// type. Without it the walk below could be running a predicate that is false of
	// everything in the file.
	delivery := topLevelMessages(t)[keyDeliveryMessage]
	if delivery == nil {
		t.Fatalf("message.proto declares no %s", keyDeliveryMessage)
	}
	declared := declaresAKeyField(delivery)
	if len(declared) == 0 {
		t.Fatalf("%s declares no field namesAKey flags, so the walk below cannot fire on the one type "+
			"it exists for. Its fields ARE the keys; if they were renamed, widen the predicate.",
			keyDeliveryMessage)
	}
	t.Logf("CONTROL — %s declares key fields %v", keyDeliveryMessage, declared)

	carried := keyTypedFields(t, walkOfNames(t, servedSet))
	t.Logf("served fields whose type declares a key (%d): %v", len(carried), sortedKeys(carried))
	for _, full := range sortedKeys(carried) {
		t.Errorf("%s has a type that declares %v. A served field does not have to NAME a key to carry "+
			"one: ruling 33 is that nothing the server hands back reaches key material at any depth, "+
			"and the walk that found this covers types DECLARED inside a served message as well as "+
			"types it references. Put the keys on a request.", full, carried[full])
	}

	// THE FAILING DIRECTION, IN THE SAME QUERY: the identical walk over the submitted
	// side must find the two carriers ruling 33 names.
	submittedOnly := map[string]bool{}
	for name := range submitted {
		if !servedSet[name] {
			submittedOnly[name] = true
		}
	}
	found := keyTypedFields(t, walkOfNames(t, submittedOnly))
	t.Logf("CONTROL — submitted-only fields whose type declares a key (%d): %v", len(found), sortedKeys(found))
	for _, want := range []string{
		"bringyour.SubmitRequest.epoch_keys",
		"bringyour.CreateGroupRequest.epoch_keys",
	} {
		if _, ok := found[want]; !ok {
			t.Errorf("the control did not find %s, so this walk finds nothing anywhere and the "+
				"served-side refusal above proves nothing", want)
		}
	}
}

// carriersOf returns every message message.proto declares — AT ANY DEPTH — with a field
// whose type is this message, keyed by the carrier's FULL name, with the field name each
// one carries it under.
//
// AT ANY DEPTH, for the reason the key checks now walk nested types. A carrier nested
// inside a request is a third place the keys can be forgotten, and a walk over top-level
// messages answers "exactly the two requests" for a file that has three. Measured, with
// the top-level walk still in place: `message Extra { EpochKeyDelivery k = 1; }` nested
// inside SubmitRequest with an `Extra extra = 20;` beside it left this gate green.
//
// MAP VALUES COUNT, for the reason keyTypedFields gives: without the IsMap branch
// this answered "no carrier" for a message that is carried as a map value, measured
// with the mutant green. SYNTHETIC MAP ENTRIES ARE NOT THEMSELVES CARRIERS: the entry of
// `map<k, EpochKeyDelivery>` has a `value` field of that type, and reporting the entry
// would name the type protoc wrote instead of the field a person did — which is the
// carrying message the IsMap branch already reports.
func carriersOf(t *testing.T, typeName string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	all := allMessageTypes(t)
	for _, name := range sortedKeys(all) {
		md := all[name]
		if md.IsMapEntry() {
			continue
		}
		fields := md.Fields()
		for i := 0; i < fields.Len(); i++ {
			f := fields.Get(i)
			sub := f.Message()
			if f.IsMap() {
				sub = f.MapValue().Message()
			}
			if sub != nil && string(sub.Name()) == typeName {
				out[name] = append(out[name], string(f.Name()))
			}
		}
	}
	return out
}

// countTopLevel is the printed half of the walk's coverage: how many of these types are
// message.proto's outermost layer, so the nested count is visible as the difference.
func countTopLevel(types map[string]protoreflect.MessageDescriptor) int {
	n := 0
	for _, md := range types {
		if isTopLevel(md) {
			n++
		}
	}
	return n
}

// The key delivery is carried by exactly the two request messages ruling 33 names,
// and the SAME QUERY prints Record's carriers as the control.
//
// Record's carriers are the control rather than a second test because they are the
// measurement the ruling rests on — six served, two submitted — and a query that
// found two carriers for one type and none for the other would be a query that had
// stopped reading the file, not a file that had changed.
func TestTheEpochKeysAreCarriedByExactlyTheTwoRequests(t *testing.T) {
	delivery := carriersOf(t, keyDeliveryMessage)
	records := carriersOf(t, recordMessage)

	t.Logf("%s carriers: %v", keyDeliveryMessage, delivery)
	t.Logf("%s carriers (the control): %v", recordMessage, records)

	if len(records) == 0 {
		t.Fatalf("the control found no carrier of %s at all, so this query is reading nothing", recordMessage)
	}

	// FULL NAMES, because carriersOf now walks every depth: a carrier nested inside one of
	// these two would be a different type with the same tail, and ruling 33 names the
	// top-level requests and not whatever is declared inside them.
	wantDelivery := map[string]string{
		"bringyour.SubmitRequest":      "epoch_keys",
		"bringyour.CreateGroupRequest": "epoch_keys",
	}
	for name, field := range wantDelivery {
		fields, carried := delivery[name]
		if !carried {
			t.Errorf("%s does not carry a %s; ruling 33 puts one on it", name, keyDeliveryMessage)
			continue
		}
		if len(fields) != 1 || fields[0] != field {
			t.Errorf("%s carries %s as %v, want exactly [%s]", name, keyDeliveryMessage, fields, field)
		}
	}
	for name := range delivery {
		if _, expected := wantDelivery[name]; !expected {
			t.Errorf("%s carries a %s and ruling 33 gives it to two messages only: SubmitRequest and "+
				"CreateGroupRequest. A third carrier is a third place the keys can be forgotten.",
				name, keyDeliveryMessage)
		}
	}

	// and the ruling's own measurement, re-derived here rather than taken from the
	// ledger: Record is the server→client type in six places against two client→server
	// carriers, which is why a key pair on it would have been six serve paths.
	//
	// SERVED-NESS IS TAKEN FROM THE WALK AND NOT FROM servedSet, because carriersOf keys
	// by full name and can now answer with a nested type. servedSet is top-level simple
	// names; a nested carrier looked up in it would miss and be counted as submitted,
	// which is the ratio silently answering the wrong question.
	_, _, servedSet := servedAndSubmitted(t)
	servedTypes := walkOfNames(t, servedSet).checked
	servedFields, submittedFields := 0, 0
	for name, fields := range records {
		if _, isServed := servedTypes[name]; isServed {
			servedFields += len(fields)
		} else {
			submittedFields += len(fields)
		}
	}
	if servedFields != 6 || submittedFields != 2 {
		t.Errorf("Record is a field of %d server→client messages and %d client→server ones; item 248 "+
			"measured six and two, and the ratio is the whole argument for ruling 33. If the file "+
			"really changed, re-argue the ruling rather than re-typing these numbers.",
			servedFields, submittedFields)
	}
	t.Logf("Record appears in %d served fields and %d submitted fields", servedFields, submittedFields)
}

// Record gains nothing, and field 14 stays out of reach.
//
// The keys half is covered transitively above; this states it directly at the type it
// is about, and adds the number. Spec B §4.3.3 declares `uint64 eph_window = 14` on
// this message and this file does not carry it — the eph window landed in the record
// CODEC at format version 0x02 and never in the protobuf projection — so 14 is
// spoken for by a field that has not arrived, not free.
func TestRecordCarriesNoKeysAndLeavesFourteenAlone(t *testing.T) {
	record := topLevelMessages(t)[recordMessage]
	if record == nil {
		t.Fatalf("message.proto declares no %s", recordMessage)
	}
	fields := record.Fields()
	numbers := map[int]string{}
	for i := 0; i < fields.Len(); i++ {
		f := fields.Get(i)
		numbers[int(f.Number())] = string(f.Name())
		if sub := f.Message(); sub != nil && string(sub.Name()) == keyDeliveryMessage {
			t.Errorf("%s.%s is an %s. Ruling 33: not on Record, ever — and the projection contract "+
				"refuses it anyway, because the server checks every field below record_bytes against "+
				"ParseRecord(record_bytes) and a key is by construction not implied by a projection "+
				"of those octets.", recordMessage, f.Name(), keyDeliveryMessage)
		}
		if name := string(f.Name()); strings.Contains(name, "key") {
			t.Errorf("%s.%s names a key. Whatever it carries, the whole of ruling 33 is that this "+
				"type carries none.", recordMessage, name)
		}
	}
	if len(numbers) == 0 {
		t.Fatalf("%s declares no field at all, so this walk read nothing", recordMessage)
	}
	if name, taken := numbers[14]; taken {
		t.Errorf("%s.%s is field 14. Spec B §4.3.3 gives 14 to eph_window on this message; taking it "+
			"for anything else puts a different field at 14 from the one every reader of that section "+
			"expects, and a projection field is compared against a parse.", recordMessage, name)
	}
	// the inline positive control: 13 IS taken, by record_id, so "14 is free" is a
	// statement about this walk having read the field numbers rather than none
	if numbers[13] != "record_id" {
		t.Errorf("%s field 13 is %q, want record_id; the number walk is not reading this message",
			recordMessage, numbers[13])
	}
	t.Logf("%s fields by number: %v", recordMessage, numbers)
}

// THE ALIGNMENT RULE, ASSERTED ON BOTH HALVES IT HAS.
//
// The shape half is the descriptor's: `epoch_keys` is repeated, of the delivery type,
// on the same message as the repeated `records` it aligns with. A singular field
// cannot be positionally aligned with a list, so the cardinality IS the rule's first
// clause.
//
// The rule half is the source's, and it is asserted over message.proto's own octets
// because that is where it lives: nothing in a descriptor can say what an entry means
// for a record that is not a commit.
//
// AND THE CLAUSE LIST NOW PINS WHAT THE RULE RESTS ON. The three clauses — the length,
// the entry-against-a-non-commit refusal and the commit-with-no-entry refusal — admit
// NO value at all for a batch mixing a commit with an ordinary record: length 0 leaves
// the commit with no entry, length n puts an entry against a non-commit, and any other
// length fails the length clause. They are satisfiable only because Spec B §4.3.3
// restricts a batch containing a commit to exactly one record. This file said the
// opposite — "the batch rule is Spec B's to relax and this alignment is not" — and
// this gate pinned that sentence. A rule that becomes unsatisfiable the moment its
// claimed independence is exercised is not general, so what the clauses below pin is
// the DEPENDENCY and its price, and the next reader relaxing §4.3.3 finds the wire
// cost written down instead of an assurance that there is none.
func TestTheEpochKeyDeliveriesAlignWithTheRecordsTheySubmit(t *testing.T) {
	submit := topLevelMessages(t)["SubmitRequest"]
	if submit == nil {
		t.Fatal("message.proto declares no SubmitRequest")
	}
	records := submit.Fields().ByName("records")
	deliveries := submit.Fields().ByName("epoch_keys")
	if records == nil || deliveries == nil {
		t.Fatalf("SubmitRequest has records=%v and epoch_keys=%v; the alignment is between those two",
			records != nil, deliveries != nil)
	}
	if records.Cardinality() != protoreflect.Repeated {
		t.Errorf("SubmitRequest.records is %v, want repeated", records.Cardinality())
	}
	if deliveries.Cardinality() != protoreflect.Repeated {
		t.Errorf("SubmitRequest.epoch_keys is %v; a singular field cannot be POSITIONALLY ALIGNED with "+
			"a repeated one, so this cardinality is the first clause of the rule and not a style choice",
			deliveries.Cardinality())
	}
	if sub := deliveries.Message(); sub == nil || string(sub.Name()) != keyDeliveryMessage {
		t.Errorf("SubmitRequest.epoch_keys has type %v, want %s", sub, keyDeliveryMessage)
	}
	// the pattern being followed, in the same test: SubmitResponse.results is the
	// repeated field this alignment is copied from, and if IT is not repeated then the
	// precedent this rule cites does not exist
	response := topLevelMessages(t)["SubmitResponse"]
	if response == nil {
		t.Fatal("message.proto declares no SubmitResponse")
	}
	if results := response.Fields().ByName("results"); results == nil || results.Cardinality() != protoreflect.Repeated {
		t.Errorf("SubmitResponse.results is %v; it is the pattern SubmitRequest.epoch_keys follows", results)
	}

	// THE CLAUSE LIST GREW WHEN THE RULE STOPPED BEING KIND-FREE, and the growth is the
	// repair rather than a decoration on it. The clause this list used to pin read "and so
	// is a commit with no entry" — kind-free, and therefore a rule that refuses every
	// conforming kind 0x0001 commit for as long as Spec B §5.4's acceptance window is open.
	// Deleting that clause without replacing it would leave this gate weaker than it was, so
	// the one sentence is replaced by SEVEN: which kind the no-entry refusal is about, the
	// opposite refusal that exists only during the window, the admission that the kind-free
	// form was the AFTER form, and the two dates that bound it. A gate that pinned the
	// corrected sentence and not its boundary would go on passing over a comment that had
	// quietly reverted to a rule true on one side of a date only.
	block := flatten(t, submitRequestSource(t), "while Spec B §5.4's acceptance window is open")
	for _, clause := range []struct {
		what   string
		phrase string
	}{
		{"the alignment", "POSITIONALLY ALIGNED"},
		{"the field it aligns with", "`records`"},
		{"the length rule", "EMPTY, OR EXACTLY AS LONG AS `records`"},
		{"what an entry against a non-commit means", "is_commit = 0 IS REASON_REJECTED"},
		{"that the commit arm turns on the attachment kind", "THE RULE IS KEYED ON THE ATTACHMENT KIND"},
		{"which kind a commit with no entry is refused for", "a kind 0x0005 commit with NO entry → REASON_REJECTED"},
		{"the refusal that exists only while the window is open", "a kind 0x0001 commit WITH an entry → REASON_REJECTED"},
		{"that the kind-free form was the one for after the window", "THAT FORM IS TRUE ONLY AFTER THE WINDOW CLOSES"},
		{"the date the window opened", "from 2026-09-22 a server accepts EITHER kind on a commit"},
		{"the date it closes and the earlier bound on it", "from 2026-11-03 — OR the day the Remove arm first ships, WHICHEVER IS EARLIER"},
		{"the batch rule the clauses rest on", "A batch containing a commit MUST contain exactly one record."},
		{"the empty case", "EMPTY when `records` carries no commit"},
		{"the second empty case the window adds", "when `records` is a single kind 0x0001 commit"},
		{"the one-entry case", "EXACTLY ONE ENTRY when `records` is a single kind 0x0005 commit"},
		{"that a mixed batch has no valid value at all", "UNSATISFIABLE"},
		{"which kind that unsatisfiability is scoped to", "for a mixed batch carrying a 0x0005 commit"},
		{"what relaxing the batch rule would cost", "would need explicit presence"},
	} {
		if !strings.Contains(block, clause.phrase) {
			t.Errorf("SubmitRequest's declaration does not state %s. The phrase %q is gone, and this "+
				"rule lives nowhere else: no descriptor can carry it, and the server that enforces it "+
				"is in another repository reading this file.", clause.what, clause.phrase)
		}
	}
}

// THE SECOND CARRIER'S PRESENCE RULE. Symmetry, because the hazard is symmetric.
//
// CreateGroupRequest.epoch_keys had no presence rule anywhere and no gate. The hazard
// SubmitRequest documents at length — "a commit whose keys never arrived is an epoch
// the server would install without having been handed what opens it" — applies
// verbatim to the create-group commit that installs epoch 1, and was written for
// neither this carrier nor covered by any test: the alignment gate above reads
// SubmitRequest's block alone. Nothing downstream supplied it either. Measured, with
// the control inline in the same query: `GetEpochKeys` occurs in 0 non-generated Go
// files in connect, 0 in sdk and 0 in msgrepo, against `GetRecords` at 0 / 1 / 13; and
// Spec B §4.3.2 still declares this message with fields 1..3 and no delivery at all.
//
// It is NOT the same rule as SubmitRequest's, which is why writing it down mattered
// rather than citing the other one: this field is SINGULAR, so absence is
// representable and distinguishable, and the two refusals — an absent delivery and a
// present-but-zero delivery — are different refusals for different reasons.
func TestTheCreateGroupEpochKeysCarryAPresenceRule(t *testing.T) {
	create := topLevelMessages(t)["CreateGroupRequest"]
	if create == nil {
		t.Fatal("message.proto declares no CreateGroupRequest")
	}
	deliveries := create.Fields().ByName("epoch_keys")
	if deliveries == nil {
		t.Fatal("CreateGroupRequest has no epoch_keys; ruling 33 puts one on it")
	}
	// the shape the rule is written against: singular, so GetEpochKeys() can answer nil
	// and "absent" is a state the server can name. On SubmitRequest the same rule would
	// be unstatable, which is why that carrier refuses on alignment instead.
	if deliveries.Cardinality() == protoreflect.Repeated {
		t.Error("CreateGroupRequest.epoch_keys is repeated; the presence rule is written for a " +
			"SINGULAR field, whose absence a server can tell from a zero value. If this becomes a " +
			"list it needs SubmitRequest's alignment rule instead, and this one stops meaning anything.")
	}
	if sub := deliveries.Message(); sub == nil || string(sub.Name()) != keyDeliveryMessage {
		t.Errorf("CreateGroupRequest.epoch_keys has type %v, want %s", sub, keyDeliveryMessage)
	}
	// the inline control for "this request's record is always a commit": the record the
	// delivery is for is a SINGULAR Record named initial_commit. If it were a list, the
	// required-presence rule would be the wrong rule.
	commit := create.Fields().ByName("initial_commit")
	if commit == nil || commit.Cardinality() == protoreflect.Repeated {
		t.Errorf("CreateGroupRequest.initial_commit is %v; the presence rule says this request carries "+
			"exactly one record and that it is always a commit, and that is why epoch_keys is "+
			"REQUIRED here and merely aligned on SubmitRequest", commit)
	}

	// THE SAME REPAIR AS SubmitRequest's, on the carrier Spec B §4.3.2 names alongside it:
	// this block's rule was UNCONDITIONAL ("PRESENCE: REQUIRED"), which is the form that
	// becomes true when §5.4's window closes and is false for every conforming kind 0x0001
	// founding commit while it is open. The one clause that pinned the unconditional wording
	// is replaced by FIVE — the kind each arm is keyed on, the admission that the old form
	// was the after form, and both dates — so the gate is stronger where it was narrowed.
	block := flatten(t, createGroupRequestSource(t), "AN ABSENT epoch_keys IS REASON_REJECTED")
	for _, clause := range []struct {
		what   string
		phrase string
	}{
		{"that presence turns on the attachment kind", "PRESENCE IS KEYED ON THE ATTACHMENT KIND"},
		{"which kind requires the delivery", "REQUIRED under kind 0x0005"},
		{"what an absent delivery means", "AN ABSENT epoch_keys IS REASON_REJECTED"},
		{"which kind forbids the delivery", "MUST BE ABSENT under kind 0x0001, where PRESENT IS REASON_REJECTED"},
		{"that the unconditional form was the one for after the window", "THAT FORM IS TRUE ONLY AFTER THE WINDOW CLOSES"},
		{"the date the window opened", "from 2026-09-22 this server takes either kind on the founding commit"},
		{"the date it closes and the earlier bound on it", "from 2026-11-03 — OR the day the Remove arm first ships, WHICHEVER IS EARLIER"},
		{"the hazard it shares with SubmitRequest", "an epoch the server would install without having been handed what opens it"},
		{"that absence is representable here and is not on the other carrier", "AND HERE ABSENCE IS REPRESENTABLE"},
		{"that a zero delivery is not an absence", "is two EMPTY KEYS and not an absence"},
	} {
		if !strings.Contains(block, clause.phrase) {
			t.Errorf("CreateGroupRequest's epoch_keys does not state %s. The phrase %q is gone. This "+
				"carrier had NO presence rule and NO gate until 2026-09-22, and the server that "+
				"enforces it is in another repository reading this file.", clause.what, clause.phrase)
		}
	}
}

// protoSource returns message.proto's text with line endings normalised.
//
// The .proto and not the .pb.go, because protoc-gen-go does not carry every source
// comment into the compiled descriptor this package links, and because the .proto is
// the artefact a second implementation reads. The file sits beside this test.
//
// LINE ENDINGS NORMALISED BEFORE ANY MATCH. .gitattributes pins *.proto to eol=lf
// precisely because core.autocrlf=true is set at system scope on the machines that
// build this tree, and this repository has already lost 84 source anchors to a
// checkout that wrote CRLF under a gate matching LF. A gate that depends on that pin
// holding is a gate with a second failure mode; this one does not.
func protoSource(t *testing.T) string {
	t.Helper()
	path := filepath.Join(".", "message.proto")
	bs, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("message.proto is unreadable at %s: %v", path, err)
	}
	return strings.ReplaceAll(string(bs), "\r\n", "\n")
}

// messageBlock slices message.proto from a message's declaration to the brace that
// closes it, and REFUSES AN ANCHOR IT DOES NOT FIND EXACTLY ONCE — a second
// occurrence means the anchor is also matching a comment somewhere, and a clause
// search over the wrong region is an anchor that has silently stopped anchoring.
func messageBlock(t *testing.T, name string) string {
	t.Helper()
	src := protoSource(t)
	anchor := "message " + name + " {"
	if n := strings.Count(src, anchor); n != 1 {
		t.Fatalf("message.proto contains %q %d times, want exactly 1; the slice below would be "+
			"reading an arbitrary one of them", anchor, n)
	}
	open := strings.Index(src, anchor)
	end := strings.Index(src[open:], "\n}")
	if end < 0 {
		t.Fatalf("message %s is not closed by a brace at column 0 in message.proto", name)
	}
	return src[open : open+end]
}

// flatten strips message.proto's comment prefixes and collapses runs of whitespace,
// so a clause is searched for as the SENTENCE it is rather than as the line wrapping
// it happens to have today.
//
// It WIDENS the old anchors rather than narrowing them: a deleted clause is still red
// and a clause that only moved across a line break no longer is. The previous form of
// this file matched one clause as "a commit\n    // with no entry", which a reflow
// changing nothing would have failed — and a gate that fires on formatting teaches
// its readers to edit the gate.
//
// Its own control is inside it: the flattened text must have lost the comment markers
// and got shorter, or the stripping did nothing and every phrase is being matched
// against the raw block by accident.
// `probe` is the per-call positive control and is required: a phrase that the RAW
// block splits across a line break and that the flattened block must carry whole. It
// makes the flattening load-bearing rather than decorative — without it a helper that
// silently returned its input would leave every clause below matching by luck.
func flatten(t *testing.T, block string, probe string) string {
	out := strings.Join(strings.Fields(strings.ReplaceAll(block, "//", " ")), " ")
	if strings.Contains(out, "//") {
		t.Fatalf("flatten left comment markers in place, so it is not stripping what it says it "+
			"strips: %q", out[:min(200, len(out))])
	}
	if len(out) >= len(block) {
		t.Fatalf("flatten returned %d octets from a %d-octet block; it removed nothing, so the clause "+
			"matches are running against the raw source and this helper is decoration",
			len(out), len(block))
	}
	if strings.Contains(block, probe) {
		t.Fatalf("the probe %q is already contiguous in the RAW block, so it does not show that "+
			"flattening did anything. Pick a phrase this file wraps across a line.", probe)
	}
	if !strings.Contains(out, probe) {
		t.Fatalf("the probe %q is in neither the raw block nor the flattened one, so flatten is not "+
			"joining the lines it claims to join and every clause below is being searched for in "+
			"text that does not read as sentences", probe)
	}
	return out
}

// submitRequestSource returns message.proto's SubmitRequest declaration.
func submitRequestSource(t *testing.T) string {
	t.Helper()
	block := messageBlock(t, "SubmitRequest")
	// the positive control for the read itself: the declaration line the rule is about
	// has to be in what was sliced out, or the phrases are being searched for in the
	// wrong region of the file
	if !strings.Contains(block, "repeated EpochKeyDelivery epoch_keys = 3;") {
		t.Fatalf("the SubmitRequest block read from message.proto does not contain the epoch_keys "+
			"declaration; the slice is wrong. It is %d octets and begins %q",
			len(block), block[:min(120, len(block))])
	}
	return block
}

// createGroupRequestSource returns message.proto's CreateGroupRequest declaration.
func createGroupRequestSource(t *testing.T) string {
	t.Helper()
	block := messageBlock(t, "CreateGroupRequest")
	if !strings.Contains(block, "EpochKeyDelivery epoch_keys = 4;") {
		t.Fatalf("the CreateGroupRequest block read from message.proto does not contain the epoch_keys "+
			"declaration; the slice is wrong. It is %d octets and begins %q",
			len(block), block[:min(120, len(block))])
	}
	return block
}

// controlFile is the POSITIVE CONTROL FOR THE WALK, BUILT, because message.proto cannot
// supply one: it declares no nested message and no map field, so over this file the
// descent is exercised by nothing and md.IsMapEntry() is false of everything. Two
// mechanisms tested against a file that cannot make either of them fire is an empty
// search reported as a clean bill, and this file's own header is the record of what an
// unmeasured narrowing costs.
//
// It assembles, with no .proto and no codegen:
//
//	message Outer {
//	    message Inner { bytes write_key = 1; }
//	    Inner inner = 1;
//	    map<uint64, Inner> bag = 2;
//	}
//
// `Inner` is nested and is NOT a map entry; `BagEntry` is the entry protoc would
// synthesise for `bag` and IS one. The SAME walkFrom and the SAME namesAKey run over it.
func controlFile(t *testing.T) protoreflect.FileDescriptor {
	t.Helper()
	optional := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
	repeated := descriptorpb.FieldDescriptorProto_LABEL_REPEATED
	bytesKind := descriptorpb.FieldDescriptorProto_TYPE_BYTES
	uint64Kind := descriptorpb.FieldDescriptorProto_TYPE_UINT64
	messageKind := descriptorpb.FieldDescriptorProto_TYPE_MESSAGE
	const inner = ".keydeliverygatecontrol.Outer.Inner"
	const entry = ".keydeliverygatecontrol.Outer.BagEntry"
	fd := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("keydelivery_gate_control.proto"),
		Package: proto.String("keydeliverygatecontrol"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("Outer"),
			NestedType: []*descriptorpb.DescriptorProto{
				{
					Name: proto.String("Inner"),
					Field: []*descriptorpb.FieldDescriptorProto{{
						Name: proto.String("write_key"), Number: proto.Int32(1),
						Label: &optional, Type: &bytesKind, JsonName: proto.String("writeKey"),
					}},
				},
				{
					Name:    proto.String("BagEntry"),
					Options: &descriptorpb.MessageOptions{MapEntry: proto.Bool(true)},
					Field: []*descriptorpb.FieldDescriptorProto{
						{
							Name: proto.String("key"), Number: proto.Int32(1),
							Label: &optional, Type: &uint64Kind, JsonName: proto.String("key"),
						},
						{
							Name: proto.String("value"), Number: proto.Int32(2),
							Label: &optional, Type: &messageKind, TypeName: proto.String(inner),
							JsonName: proto.String("value"),
						},
					},
				},
			},
			Field: []*descriptorpb.FieldDescriptorProto{
				{
					Name: proto.String("inner"), Number: proto.Int32(1),
					Label: &optional, Type: &messageKind, TypeName: proto.String(inner),
					JsonName: proto.String("inner"),
				},
				{
					Name: proto.String("bag"), Number: proto.Int32(2),
					Label: &repeated, Type: &messageKind, TypeName: proto.String(entry),
					JsonName: proto.String("bag"),
				},
			},
		}},
	}
	file, err := protodesc.NewFile(fd, nil)
	if err != nil {
		t.Fatalf("the control descriptor does not build, so there is no control: %v", err)
	}
	return file
}

// THE SCOPE OF THE KEY CHECKS, PRINTED, AND THE ONE NARROWING IN IT ASSERTED FROM BOTH
// ENDS.
//
// Until 2026-09-22 keyNamedFields, keyTypedFields and declaresAKeyField's caller all
// iterated topLevelMessages, and the only thing standing between a NESTED type and those
// three checks was one loop in servedAndSubmitted that excused any simple name ending in
// "Entry". Reproduced, protoc 35.1, inside `message FetchResponse {`:
//
//	message ShimEntry   { KeyBagEntry bag = 1; }
//	message KeyBagEntry { bytes write_key = 1; bytes read_key = 2; }
//	ShimEntry shim = 20;
//
//	-> ok github.com/urnetwork/connect/protocol 0.379s
//
// That is item 244's own defect again — the next epoch's write and read key, under those
// exact names, on the served fetch answer — with all four key gates green. ISOLATED IN
// THE SAME QUERY: renaming the two containers to Shim/KeyBag and changing nothing else
// reddens all four, so the "Entry" suffix at depth >= 2 was the whole of the escape.
//
// AND THE EXEMPTION EXCUSED NOTHING THAT EXISTS, which is why it was invisible. The
// counts it was narrowing over are printed below rather than recited. The repair is that
// the checks walk nested types directly and the exemption is md.IsMapEntry() — the
// descriptor's own answer, true of synthetic entries and of nothing else — with its
// complement asserted against the map fields that produce it.
func TestTheKeyCheckWalkDescendsAndSkipsOnlyMapEntries(t *testing.T) {
	all := allMessageTypes(t)
	nestedInFile, entriesInFile, suffixInFile := []string{}, []string{}, []string{}
	for _, full := range sortedKeys(all) {
		md := all[full]
		if !isTopLevel(md) {
			nestedInFile = append(nestedInFile, full)
		}
		if md.IsMapEntry() {
			entriesInFile = append(entriesInFile, full)
		}
		if strings.HasSuffix(string(md.Name()), "Entry") {
			suffixInFile = append(suffixInFile, full)
		}
	}
	t.Logf("message.proto declares %d message types at all depths: %d nested, %d map entries, "+
		"%d with a simple name ending in \"Entry\" (the exemption that stood here until 2026-09-22)",
		len(all), len(nestedInFile), len(entriesInFile), len(suffixInFile))
	t.Logf("nested (%d): %v", len(nestedInFile), nestedInFile)
	t.Logf("map entries (%d): %v", len(entriesInFile), entriesInFile)
	t.Logf("simple name ends in \"Entry\" (%d): %v", len(suffixInFile), suffixInFile)
	if len(all) != len(topLevelMessages(t))+len(nestedInFile) {
		t.Fatalf("the depth walk found %d types, %d of them nested, against %d top-level; the three "+
			"do not add up, so allMessageTypes and topLevelMessages are not reading the same file",
			len(all), len(nestedInFile), len(topLevelMessages(t)))
	}

	// THE SKIP, FROM BOTH ENDS. Every type the walk skipped is the synthetic entry of a
	// map field the same walk found, and every map field the walk found has its entry in
	// the skipped set. Today both sides are EMPTY, and that is printed rather than left
	// implicit: an exemption whose complement nobody enumerates is exactly the shape the
	// "Entry" suffix had for as long as it stood.
	_, _, servedSet := servedAndSubmitted(t)
	w := walkOfNames(t, servedSet)
	t.Logf("the served walk checks %d types (%d top-level, %d nested) and skips %d synthetic map "+
		"entries, produced by %d map fields: %v",
		len(w.checked), countTopLevel(w.checked), len(w.checked)-countTopLevel(w.checked),
		len(w.mapEntries), len(w.mapFields), sortedKeys(w.mapFields))
	byEntry := map[string]string{}
	for field, produced := range w.mapFields {
		byEntry[produced] = field
	}
	for skipped := range w.mapEntries {
		if _, produced := byEntry[skipped]; !produced {
			t.Errorf("%s was skipped as a map entry and no map field in the same walk produces it, "+
				"so the skip is excusing a type nobody asked it to excuse", skipped)
		}
	}
	for produced, field := range byEntry {
		if _, skipped := w.mapEntries[produced]; !skipped {
			t.Errorf("%s is the synthetic entry of the map field %s and the walk did not skip it, so "+
				"the key checks are about to report protoc's own `key` field as key material",
				produced, field)
		}
	}

	// THE POSITIVE CONTROL, and it is the whole reason this test is not a pair of zeroes.
	// The same walkFrom and the same keyNamedFields, over a descriptor that HAS a nested
	// message and HAS a map field.
	outer := controlFile(t).Messages().Get(0)
	cw := walkFrom([]protoreflect.MessageDescriptor{outer})
	const (
		controlOuter = "keydeliverygatecontrol.Outer"
		controlInner = "keydeliverygatecontrol.Outer.Inner"
		controlEntry = "keydeliverygatecontrol.Outer.BagEntry"
		controlField = "keydeliverygatecontrol.Outer.bag"
		controlKey   = "keydeliverygatecontrol.Outer.Inner.write_key"
	)
	t.Logf("CONTROL — the built walk checks %v and skips %v",
		sortedKeys(cw.checked), sortedKeys(cw.mapEntries))
	for _, want := range []string{controlOuter, controlInner} {
		if _, ok := cw.checked[want]; !ok {
			t.Errorf("control: the walk does not check %s, so it is not descending and the served "+
				"walk above covers only what it happens to find at the top level", want)
		}
	}
	if _, ok := cw.checked[controlEntry]; ok {
		t.Errorf("control: %s is a synthetic map entry and the walk checked it; every entry declares "+
			"a field literally named `key`, so this would make every map field a finding about protoc",
			controlEntry)
	}
	if _, ok := cw.mapEntries[controlEntry]; !ok {
		t.Errorf("control: %s was not recognised as a map entry, so md.IsMapEntry() answers false for "+
			"a real one and the skip above is a predicate that fires on nothing", controlEntry)
	}
	if got := cw.mapFields[controlField]; got != controlEntry {
		t.Errorf("control: the walk records the map field %s as producing %q, want %s; the both-ends "+
			"assertion above compares against that map", controlField, got, controlEntry)
	}
	if nested := outer.Messages().ByName("Inner"); nested == nil || nested.IsMapEntry() {
		t.Error("control: the nested message Inner reports IsMapEntry() true, so the predicate does " +
			"not separate a nested message from a synthetic entry and the skip is not a narrowing " +
			"but a hole")
	}

	// AND END TO END: namesAKey, run over the built walk by the same helper the served
	// side uses, finds the key on the NESTED type. That is the escape of 2026-09-22
	// caught by the repaired mechanism, in-process and with no proto to regenerate.
	flagged, _ := keyNamedFields(t, cw)
	t.Logf("CONTROL — the key-name check over the built walk flags %v", sortedKeys(flagged))
	if _, found := flagged[controlKey]; !found {
		t.Errorf("control: keyNamedFields did not flag %s. That field is a `write_key` on a type "+
			"nested inside the message the walk was rooted at, which is exactly what rode through "+
			"this gate until 2026-09-22; if it is not found here, the repair is not in the code path "+
			"the served side uses.", controlKey)
	}
}

// ── the served closure's OPAQUE fields ──────────────────────────────────────────────

// THE SECOND NET, AND WHY namesAKey COULD NOT BE THE ONLY ONE.
//
// Every key check above this line keys on a NAME — the field's own (namesAKey) or the
// names a field's TYPE declares (declaresAKeyField). Both are narrowings, both have their
// complements written down and asserted, and both are blind in exactly the same place: a
// field whose name says nothing. Reproduced on 2026-09-23, protoc 35.1, inside
// `message FetchResponse`:
//
//	bytes wk = 20;
//	bytes rk = 21;
//
//	-> ok github.com/urnetwork/connect/protocol 0.404s
//
// That is item 244's own defect on the served fetch answer — the next epoch's write and
// read key, in raw octets, handed to a removed member forever — with every gate in this
// file green. `wk` contains no "key", so it is outside namesAKey AND outside
// narrowedAwayOnPurpose, which only ever enumerates the names that DO contain "key": a
// narrowing whose complement is itself narrowed by the same predicate is a narrowing
// nobody is measuring. The NESTED form of the same mutant IS caught, by nestedServedTypes,
// which is the repair of the commit before this one; one level up it walked past
// everything.
//
// SO THE SERVED CLOSURE'S OPAQUE FIELDS ARE ENUMERATED FROM THE DESCRIPTOR AND EACH ONE IS
// DISPOSITIONED BY NAME, in both directions: an opaque field with no entry in
// servedOpaqueFields is a refusal, and an entry the walk no longer finds is a refusal. A
// NAME is then no longer what stands between a served message and the epoch keys — a
// DECISION is, written down beside the field, and adding an opaque field to anything the
// server hands back costs whoever adds it one sentence saying what is in it.
//
// It is a SECOND net and not a replacement. namesAKey keeps catching `read_key` on a
// uint64, on a message type, on anything at all, which no enumeration of opaque fields
// would see; this one catches `wk`, which no name predicate will ever see. Neither
// subsumes the other and both are asserted.

// opaqueServedFields returns every field in this walk whose value is a string of octets
// this file gives no structure to — `bytes`, `string`, their repeated forms, and a map
// field whose VALUE is one of the two — keyed by full name, with the spelling of the
// carrier as the value.
//
// STRING IS IN THE CLASS AND NOT IN THE COMPLEMENT. A proto3 `string` is UTF-8 validated,
// so 32 raw key octets cannot simply be assigned to one — but base64 or hex can, and the
// re-encoding is four lines. The class costs five more entries in the table below over
// this file, which is cheap enough that excluding it would be a narrowing taken for
// convenience rather than for a reason.
//
// MAP VALUES ARE READ THROUGH f.MapValue(), for the reason keyTypedFields gives at length:
// a map field's own Kind() is the synthetic entry's, so a check that asked f.Kind() alone
// would answer "message" for `map<uint64, bytes>` and never fire.
//
// The second return is THE COMPLEMENT, BY KIND: every field the same walk saw that this
// narrowing removed, gathered under the spelling that removed it, so "which fields is this
// gate not looking at" is a value this file prints and asserts rather than a property a
// reader has to infer from the predicate.
func opaqueServedFields(t *testing.T, w typeWalk) (opaque map[string]string, complementByKind map[string][]string) {
	t.Helper()
	isOpaque := func(k protoreflect.Kind) bool {
		return k == protoreflect.BytesKind || k == protoreflect.StringKind
	}
	opaque = map[string]string{}
	complementByKind = map[string][]string{}
	for _, message := range sortedKeys(w.checked) {
		fields := w.checked[message].Fields()
		for i := 0; i < fields.Len(); i++ {
			f := fields.Get(i)
			full := string(f.FullName())
			switch {
			case f.IsMap():
				spelling := fmt.Sprintf("map<%s, %s>", f.MapKey().Kind(), f.MapValue().Kind())
				if isOpaque(f.MapValue().Kind()) {
					opaque[full] = spelling
				} else {
					complementByKind[spelling] = append(complementByKind[spelling], full)
				}
			case isOpaque(f.Kind()):
				if f.IsList() {
					opaque[full] = "repeated " + f.Kind().String()
				} else {
					opaque[full] = f.Kind().String()
				}
			default:
				spelling := f.Kind().String()
				if f.IsList() {
					spelling = "repeated " + spelling
				}
				complementByKind[spelling] = append(complementByKind[spelling], full)
			}
		}
	}
	return opaque, complementByKind
}

// EVERY OPAQUE FIELD THE SERVER HANDS BACK, name -> what is in it.
//
// This is a DISPOSITION and not an exemption list: the entries here are not holes punched
// in a gate, they are the answers to the question the gate asks. `bytes` is the shape epoch
// key material has in this protocol — EpochKeyDelivery.write_key and .read_key are `bytes`,
// spec A §5.11's write_key and read_key are 32 raw octets — so the rule is that a
// server→client field of that shape says what it carries, in one sentence, next to its
// name.
//
// EACH SENTENCE IS TAKEN FROM message.proto OR FROM THE SPEC SECTION IT CITES, not invented
// here. Where the file says nothing about a field, the entry says what the file's shape
// says and no more.
//
// Both halves are asserted by the test below. An undispositioned field is a refusal —
// including one added under a name no predicate in this file recognises, which is the hole
// this map closes. A dispositioned field the walk no longer finds is also a refusal,
// because a sentence about a field that has moved is a sentence nobody is reading.
var servedOpaqueFields = map[string]string{
	// ── the record, and the projections of it the server indexes ──
	"bringyour.Record.record_bytes": "the canonical connect/message encoding of the whole " +
		"record, AUTHORITATIVE for every other field of Record: ct_head, ct_body, " +
		"server_attachment and write_auth. Ciphertext and macs. The attachment inside it is " +
		"where item 244 lived, and kind 0x0005 is the repair — LP(H(epoch_keys)) in place of " +
		"the two keys, with the keys themselves on the request (ruling 33).",
	"bringyour.Record.sender_handle": "16 B. A server-indexed PROJECTION of record_bytes, " +
		"which the server verifies equals the corresponding field of ParseRecord(record_bytes). " +
		"Record's own comment argues why no key can be a projection: a key is by construction " +
		"not implied by the record's octets.",
	"bringyour.Record.body_hash":          "32 B. A projection of record_bytes, verified equal by the server. A hash.",
	"bringyour.Record.blob_id":            "32 B, present iff size_bucket == 5. A projection of record_bytes, verified equal.",
	"bringyour.Record.wrap_target_handle": "16 B, from the server_attachment WrapTag, absent otherwise. A projection, verified equal.",
	"bringyour.Record.recovery_handle":    "16 B, from the server_attachment RecoveryTag, absent otherwise. A projection, verified equal.",

	// ── group ids: the routing identifier the client itself named ──
	"bringyour.Backpressure.group_id":    "the group this pause is about — the id the client itself named when it subscribed.",
	"bringyour.GroupRecords.group_id":    "the group these recovery-fetch records belong to. §4.3.7 scopes that arm by the recovery_handle.",
	"bringyour.RecordPush.group_id":      "the group these pushed records belong to.",
	"bringyour.SubscriptionAck.group_id": "the group being acknowledged — the id carried by the Subscription that asked.",
	"bringyour.TransientPush.group_id":   "the group these transient records belong to.",
	"bringyour.FetchAttestation.group_id": "the group the attestation is over, and the second " +
		"length-prefixed term of §4.3.4's preimage. An echo of FetchRequest.group_id.",

	// ── the fetch attestation ──
	"bringyour.FetchAttestation.server_id": "the fleet server's identity, the first " +
		"length-prefixed term of §4.3.4's preimage. Matches HelloResponse.server_id.",
	"bringyour.FetchAttestation.sig": "Ed25519 over §4.3.4's preimage, transcribed in this " +
		"file beside the field. A SIGNATURE, not a key — and nothing in this repository " +
		"computes or verifies it, which TestNothingHereComputesTheAttestationPreimage states " +
		"as a measurement with its own controls.",

	// ── the fleet's public signing keys and the handshake ──
	"bringyour.ServerKey.pub": "Ed25519 PUBLIC, 32 B. Publishing it is the point of the type: " +
		"a client cannot verify a server signature without it. This is the same set " +
		"keyNameExemptions exempts under HelloResponse.server_keys, one level down.",
	"bringyour.ServerKey.sig_by_previous": "Ed25519 by the OUTGOING key over this key's " +
		"certification body — a signature in the rotation chain.",
	"bringyour.ServerKey.sig_by_root":   "Ed25519 by the FLEET ROOT key over the same. A signature.",
	"bringyour.HelloResponse.server_id": "16 B, stable per fleet, as the field's own comment says.",
	"bringyour.HelloResponse.server_nonce": "32 B, as the field's own comment says. A " +
		"server-chosen nonce for this connection; it is 32 octets and it is NOT secret — it is " +
		"published to whoever connects.",

	// ── key transparency gossip: roots and one signature ──
	"bringyour.KtGossip.root_hash":    "the current signed-tree-head root hash of the key transparency log.",
	"bringyour.KtGossip.prev_root":    "the previous STH's root hash, which is how a client chains two gossips.",
	"bringyour.KtGossip.history_root": "the log's history root. A hash.",
	"bringyour.KtGossip.sth_sig":      "the signature over the signed tree head. A signature, verified under ServerKey.pub.",

	// ── blobs ──
	"bringyour.BlobEndpoint.tls_spki_sha256": "repeated: SHA-256 of the blob endpoint's TLS " +
		"SubjectPublicKeyInfo, current plus one announced successor, as the field's own comment " +
		"says. A PIN over a certificate that is already public to anyone who opens the connection.",
	"bringyour.BlobGrantResponse.grant_token": "an opaque bearer capability, §8.2. It IS a " +
		"secret — and it is a BLOB capability: server-minted, scoped to one blob, and expiring " +
		"at the expires_ms beside it. It is not epoch key material and it opens no record.",
	"bringyour.BlobGrantResponse.chunk_mask": "which chunks the server already holds, for " +
		"resume, as the field's own comment says. A bitmask.",

	// ── rendezvous ──
	"bringyour.RendezvousPush.rendezvous_id": "the mailbox this push is about — the id the caller registered.",
	"bringyour.RendezvousDeposit.deposit_ct": "the deposited CIPHERTEXT, exactly " +
		"Capabilities.rendezvous_deposit_bytes wide. Opaque to the server, which is the point of " +
		"the arm; it is handed back to the collector who holds the key, and this file carries no key for it.",
	"bringyour.RendezvousOpenResponse.card_xwing_pub": "an X-Wing PUBLIC key. Publishing it is " +
		"what opening a rendezvous card IS: the depositor encrypts to it.",

	// ── transport framing ──
	"bringyour.MessageServerFragment.part": "one slice of a fragmented envelope's own octets, " +
		"reassembled by index and count. Transport framing — whatever the envelope carried, " +
		"which is every other type in this walk.",

	// ── the five `string` fields ──
	"bringyour.BlobEndpoint.host":                 "the blob endpoint's hostname. A host.",
	"bringyour.BlobEndpoint.path_prefix":          "the path prefix blob URLs are built under.",
	"bringyour.BlobGrantResponse.path":            "the path this grant is for, under that prefix.",
	"bringyour.Capabilities.operator_host":        "the operator's hostname, as advertised.",
	"bringyour.Capabilities.hosting_jurisdiction": "the jurisdiction the operator declares it hosts in.",
}

// THE KINDS THAT ARE NOT OPAQUE, dispositioned as a CLASS.
//
// The narrowing above is "the field's value is a string of octets or characters this file
// gives no structure to", and the complement is every other proto kind. It is dispositioned
// by kind rather than by field because the argument is the same sentence for every field of
// a kind and repeating it 41 times would be noise nobody reads — but it is asserted in BOTH
// directions all the same: a kind that turns up in the served closure and is not written
// down here is a refusal, and a kind written down that the walk no longer finds is a
// refusal. The class narrowing cannot widen quietly.
//
// WHAT THIS CLASS DOES NOT CLAIM, and the limit is stated because the alternative is a
// sentence no measurement supports. It does not claim key material cannot be SPELLED in
// these kinds. Four uint64s are 32 octets. What it claims is narrower and is the whole of
// what is claimed anywhere in this file: none of these kinds is the SHAPE the epoch keys
// have in this protocol, so a key smuggled through one would have to be split and
// reassembled at both ends — which is a change to a server's and a client's code, not a
// field added to a proto, and it is the class of defect the SDK's dataflow gate and
// connect/message's width checks are the answer to. namesAKey and declaresAKeyField still
// run over every field of every one of these kinds.
var servedFieldKindsThatAreNotOpaque = map[string]string{
	"message": "a message-typed field is not a leaf and is not narrowed away at all: walkFrom " +
		"DESCENDS into it, so its own opaque fields are in the enumeration above under their " +
		"own names. This entry is here because the kind appears in the complement, and what it " +
		"records is that the complement is a leaf classification, not a boundary of the walk.",
	"repeated message": "the same, through the repeated form. FetchResponse.records is here, " +
		"and Record's six opaque fields are dispositioned above.",
	"uint64": "a 64 bit unsigned integer: record ids, epochs, cursors, millisecond clocks. " +
		"Not an octet string.",
	"uint32":          "a 32 bit unsigned integer: byte and record ceilings, ttl seconds, class masks, indices.",
	"repeated uint64": "a list of 64 bit integers — FetchAttestation.record_ids, the ids §4.3.4 attests to.",
	"repeated uint32": "a list of 32 bit integers — the eph and size bucket edges Capabilities advertises.",
	"bool":            "one bit.",
	"enum":            "a closed set of named integers — the Reason codes this file declares.",
}

// EVERY OPAQUE FIELD THE SERVED CLOSURE CARRIES IS DISPOSITIONED, AND THE NARROWING THAT
// DECIDED WHICH FIELDS THOSE ARE IS ASSERTED FROM BOTH ENDS.
func TestEveryOpaqueFieldTheServerServesIsDispositioned(t *testing.T) {
	_, submitted, servedSet := servedAndSubmitted(t)
	w := walkOfNames(t, servedSet)

	opaque, complementByKind := opaqueServedFields(t, w)
	narrowedAwayCount := 0
	for _, fields := range complementByKind {
		narrowedAwayCount += len(fields)
	}
	t.Logf("the served walk checks %d types; %d of their fields are opaque and %d fields across "+
		"%d proto kinds were narrowed away", len(w.checked), len(opaque), narrowedAwayCount,
		len(complementByKind))
	for _, full := range sortedKeys(opaque) {
		t.Logf("  OPAQUE %-16s %s", opaque[full], full)
	}
	for _, kind := range sortedKeys(complementByKind) {
		t.Logf("  NARROWED AWAY — %d fields of kind %s: %v", len(complementByKind[kind]), kind,
			complementByKind[kind])
	}
	if len(opaque) == 0 {
		t.Fatal("the served closure carries no opaque field at all, so this gate is a walk over an " +
			"empty set and every refusal below is vacuous")
	}

	// THE DISPOSITION, BOTH DIRECTIONS.
	for _, full := range sortedKeys(opaque) {
		if _, dispositioned := servedOpaqueFields[full]; !dispositioned {
			t.Errorf("%s is a %s field of a type the server hands back, and no entry in "+
				"servedOpaqueFields says what is in it. An opaque field on a served message is the "+
				"shape item 244 IS: `bytes wk = 20; bytes rk = 21;` on FetchResponse is the next "+
				"epoch's two keys handed to a removed member forever, under names no name predicate "+
				"in this file recognises — measured green against every other gate here on "+
				"2026-09-23. Write down what this field carries, or take it off the served side.",
				full, opaque[full])
		}
	}
	for full := range servedOpaqueFields {
		if _, still := opaque[full]; !still {
			t.Errorf("servedOpaqueFields says what %s carries and the walk does not find it among the "+
				"served closure's opaque fields any more — it was renamed, retyped, or is gone. A "+
				"disposition nothing needs is a sentence about this file that has stopped being true.",
				full)
		}
	}

	// THE CLASS NARROWING, BOTH DIRECTIONS.
	for kind := range complementByKind {
		if _, dispositioned := servedFieldKindsThatAreNotOpaque[kind]; !dispositioned {
			t.Errorf("the served closure carries %d fields of kind %s and "+
				"servedFieldKindsThatAreNotOpaque does not say why that kind is outside this gate. A "+
				"narrowing whose complement nobody enumerates is how a gate stops covering the thing "+
				"it is named for — which is what namesAKey's own complement did by only ever "+
				"enumerating names that already contained \"key\".", len(complementByKind[kind]), kind)
		}
	}
	for kind := range servedFieldKindsThatAreNotOpaque {
		if _, still := complementByKind[kind]; !still {
			t.Errorf("servedFieldKindsThatAreNotOpaque disposes of kind %s and the served closure has "+
				"no field of that kind any more, so this entry narrows nothing", kind)
		}
	}

	// THE FAILING DIRECTION, IN THE SAME QUERY — part one: the identical enumerator over the
	// submitted-only side must find the two keys ruling 33 put on the request. If it does
	// not, this enumerator answers "no opaque fields anywhere" and the refusals above are a
	// walk over an empty set rather than a file that carries nothing undispositioned.
	submittedOnly := map[string]bool{}
	for name := range submitted {
		if !servedSet[name] {
			submittedOnly[name] = true
		}
	}
	control, _ := opaqueServedFields(t, walkOfNames(t, submittedOnly))
	t.Logf("CONTROL — submitted-only opaque fields (%d): %v", len(control), sortedKeys(control))
	for _, want := range []string{
		"bringyour.EpochKeyDelivery.write_key",
		"bringyour.EpochKeyDelivery.read_key",
	} {
		if _, found := control[want]; !found {
			t.Errorf("the control did not find %s, which is the epoch key itself and is `bytes`. This "+
				"enumerator is then not finding opaque fields and the refusals above prove nothing.",
				want)
		}
	}

	// AND PART TWO, WHICH IS THE WHOLE REASON THIS GATE EXISTS: the same enumerator beside
	// namesAKey, over one built descriptor, showing that this net catches the field the name
	// net cannot see. `wk` is what walked past every gate in this file on 2026-09-23.
	outer := opaqueControlFile(t).Messages().Get(0)
	cw := walkFrom([]protoreflect.MessageDescriptor{outer})
	builtOpaque, builtComplement := opaqueServedFields(t, cw)
	builtNamed, _ := keyNamedFields(t, cw)
	t.Logf("CONTROL — over the built descriptor, the opaque net flags %v and the name net flags %v",
		sortedKeys(builtOpaque), sortedKeys(builtNamed))
	const (
		controlUnnamedKey = "opaquegatecontrol.Served.wk"
		controlNamedKey   = "opaquegatecontrol.Served.write_key"
		controlNotOpaque  = "opaquegatecontrol.Served.record_id"
	)
	if _, found := builtOpaque[controlUnnamedKey]; !found {
		t.Errorf("control: the opaque net did not flag %s. That is a `bytes` field on a served "+
			"message under a name no predicate in this file recognises — the exact mutant that was "+
			"green on 2026-09-23 — so if it is not found here the repair is not in the code path the "+
			"served side uses.", controlUnnamedKey)
	}
	if _, found := builtNamed[controlUnnamedKey]; found {
		t.Errorf("control: namesAKey flagged %s. It does not contain \"key\", so if it is flagged the "+
			"name predicate has changed and the two nets are no longer the two different nets this "+
			"test compares.", controlUnnamedKey)
	}
	if _, found := builtNamed[controlNamedKey]; !found {
		t.Errorf("control: namesAKey did not flag %s, so the name net is matching nothing in the same "+
			"run and the comparison above says nothing", controlNamedKey)
	}
	if _, found := builtOpaque[controlNamedKey]; !found {
		t.Errorf("control: the opaque net did not flag %s, which is `bytes`; the two nets are "+
			"supposed to overlap on a field that is both", controlNamedKey)
	}
	if !containsString(builtComplement["uint64"], controlNotOpaque) {
		t.Errorf("control: %s is a uint64 and the opaque net's complement does not hold it under "+
			"\"uint64\"; the complement asserted over the real file above is then built by something "+
			"other than the kind it names", controlNotOpaque)
	}
	// AND THE MAP BRANCH, which the real file cannot exercise because it declares no map
	// field. Without this the IsMap arm above is a mechanism no run measures — and reading
	// f.Kind() instead of f.MapValue().Kind() is a shipped defect in this file's history, not
	// a hypothetical.
	const controlMapKey = "opaquegatecontrol.Served.bag"
	if spelling := builtOpaque[controlMapKey]; spelling != "map<uint64, bytes>" {
		t.Errorf("control: the opaque net answers %q for %s, want \"map<uint64, bytes>\". A map field's "+
			"own Kind() is its synthetic entry's, so a check that asked f.Kind() alone would file this "+
			"under \"message\" and never look at the octets in it.", spelling, controlMapKey)
	}
}

// opaqueControlFile is the POSITIVE CONTROL FOR THE OPAQUE NET, BUILT, and it is built for
// the same reason controlFile is: message.proto cannot supply the comparison. The thing to
// be shown is that the opaque net catches an opaque field the NAME net does not, and
// message.proto has no such field — because if it had one, this gate would be red.
//
// It assembles, with no .proto and no codegen:
//
//	message Served {
//	    bytes  wk                 = 1;   // opaque, name says nothing -> opaque net only
//	    bytes  write_key          = 2;   // opaque, name says key     -> both nets
//	    uint64 record_id          = 3;   // not opaque                -> neither, and in the complement
//	    map<uint64, bytes> bag    = 4;   // opaque THROUGH f.MapValue()
//	}
//
// THE MAP FIELD IS HERE BECAUSE message.proto HAS NO MAP FIELD AT ALL. The IsMap branch of
// opaqueServedFields exists because a map field's own Kind() is the synthetic entry's — the
// defect keyTypedFields records having shipped — and over the real file that branch is
// exercised by nothing. Two mechanisms tested against a file that cannot make one of them
// fire is the empty search this file's own header is the record of.
func opaqueControlFile(t *testing.T) protoreflect.FileDescriptor {
	t.Helper()
	optional := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
	repeated := descriptorpb.FieldDescriptorProto_LABEL_REPEATED
	bytesKind := descriptorpb.FieldDescriptorProto_TYPE_BYTES
	uint64Kind := descriptorpb.FieldDescriptorProto_TYPE_UINT64
	messageKind := descriptorpb.FieldDescriptorProto_TYPE_MESSAGE
	const bagEntry = ".opaquegatecontrol.Served.BagEntry"
	fd := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("opaque_gate_control.proto"),
		Package: proto.String("opaquegatecontrol"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("Served"),
			NestedType: []*descriptorpb.DescriptorProto{{
				Name:    proto.String("BagEntry"),
				Options: &descriptorpb.MessageOptions{MapEntry: proto.Bool(true)},
				Field: []*descriptorpb.FieldDescriptorProto{
					{
						Name: proto.String("key"), Number: proto.Int32(1),
						Label: &optional, Type: &uint64Kind, JsonName: proto.String("key"),
					},
					{
						Name: proto.String("value"), Number: proto.Int32(2),
						Label: &optional, Type: &bytesKind, JsonName: proto.String("value"),
					},
				},
			}},
			Field: []*descriptorpb.FieldDescriptorProto{
				{
					Name: proto.String("wk"), Number: proto.Int32(1),
					Label: &optional, Type: &bytesKind, JsonName: proto.String("wk"),
				},
				{
					Name: proto.String("write_key"), Number: proto.Int32(2),
					Label: &optional, Type: &bytesKind, JsonName: proto.String("writeKey"),
				},
				{
					Name: proto.String("record_id"), Number: proto.Int32(3),
					Label: &optional, Type: &uint64Kind, JsonName: proto.String("recordId"),
				},
				{
					Name: proto.String("bag"), Number: proto.Int32(4),
					Label: &repeated, Type: &messageKind, TypeName: proto.String(bagEntry),
					JsonName: proto.String("bag"),
				},
			},
		}},
	}
	file, err := protodesc.NewFile(fd, nil)
	if err != nil {
		t.Fatalf("the opaque net's control descriptor does not build, so there is no control: %v", err)
	}
	return file
}
