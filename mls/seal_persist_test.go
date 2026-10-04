// The persist a seal makes, held at EVERY site that seals.
//
// WHY THIS FILE EXISTS, and it is a fixture story rather than a code one. (*Group).sealAndRecordLocked
// persists before the ciphertext leaves, and its own comment calls that order load-bearing: "it runs
// before the caller is handed anything, and its failure is the call's ... hands out a message whose
// generation nothing has recorded, which is the defect above reached one step later".
//
// MEASURED, over the whole of ./mls/... and ./message/... rather than over a targeted run: with
// that persist rewritten as `_ = self.persist()`, 7474 of 7478 tests pass, and the four that fail
// are the case below and its three subtests. Nothing else in either package observes it -- before
// this file, nothing at all did.
//
// It had been observed once. TestAFailedPersistLeavesTheGroupAbleToProposeInTheEpochItMovedTo held a
// refusing store across CreateCommit and across ProposeUpdate, because the store was armed before
// both. The commit that made a seal persist moved `store.refusing = true` from BEFORE CreateCommit
// to after it and added `store.refusing = false` before the ProposeUpdate -- so that the case would
// go on reading as the merge-ordering case it is named for. That is the right decision for THAT
// case, which is about an epoch boundary and not about a seal; what was missing is the case the
// coverage moved off. This is it.
//
// THE CLASS IS DERIVED FROM THE SOURCE AND NOT LISTED. What has to be covered is every declaration
// that reaches sealAndRecordLocked, and this package has been wrong about a hand-written list of
// call sites often enough that the list is read out of the syntax tree instead. A fourth seal site
// added to group.go joins this fixture by existing, and until somebody drives it the gate fails
// naming the site it cannot reach -- rather than passing over three of four.
package mls

import (
	"errors"
	"go/ast"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/connect/mls/syntax"
)

// sealSiteCallers is every declaration of this package's non test source that calls
// sealAndRecordLocked, by the name of the declaration it is written in.
//
// The scan is over every file rather than over group.go, for packageLevelFunctions' stated reason:
// a gate that names a file goes on reporting a clean run while the thing it guards is written next
// door. The enclosing declaration is the unit rather than the call, because what a fixture can
// drive is a function.
func sealSiteCallers(t *testing.T) []string {
	t.Helper()
	found := []string{}
	for _, path := range packageSourcePaths(t) {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		parsed := mustParseSource(t, path)
		for _, declaration := range parsed.file.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Body == nil {
				continue
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, isCall := node.(*ast.CallExpr)
				if !isCall {
					return true
				}
				selector, isSelector := call.Fun.(*ast.SelectorExpr)
				if !isSelector || selector.Sel == nil || selector.Sel.Name != sealAndRecordName {
					return true
				}
				if !slices.Contains(found, function.Name.Name) {
					found = append(found, function.Name.Name)
				}
				return true
			})
		}
	}
	slices.Sort(found)
	if len(found) == 0 {
		t.Fatalf("no declaration of this package's non test source calls %s, so the table below covers nothing",
			sealAndRecordName)
	}
	return found
}

// sealAndRecordName is the one door every seal of this package records what it spent through. It is
// spelled once so the scan above and the failure messages below cannot drift apart.
const sealAndRecordName = "sealAndRecordLocked"

// sealSiteDoor is one derived seal site and the exported call a fixture reaches it with.
//
// THE HEADER THAT STOOD HERE NAMED THE OTHER ASSERTION'S PROPERTY, and it is corrected rather than
// deleted because a reader would have trusted it. It said that "the call returned an error" is
// satisfied by a build that persisted afterwards and by one that never persisted at all, and that
// the `handedBack` half is what tells those apart. It is not: over a REFUSING store a build that
// never persisted at all has nothing to refuse it, so it answers a message and a NIL error, and the
// errors.Is check below is precisely what separates the two -- which is what this file's own
// measurement at the top records, four failures out of 7478 with the persist rewritten as
// `_ = self.persist()`, every one of them on that check.
//
// WHAT `handedBack` OBSERVES IS THE DOORS' DROP AND NOT sealAndRecordLocked'S OWN, and this is the
// third header written over this field. Both of the first two named a property that is not asserted
// here, so this one says what the field reads rather than what a reader would like it to mean:
// `drive` returns whether (*Group).ProposeUpdate, (*Group).CreateCommit or (*Group).Protect handed
// its caller a value, and each of those three is `return nil, err` written out at the call. That is
// a real property and it is three places it can be lost -- a door that returned its half-built
// proposal beside the refusal would put octets on the wire under a generation the persisted state
// does not carry -- and it is what this field measures.
//
// sealAndRecordLocked'S OWN DROP IS A DIFFERENT PROPERTY AND IT IS ASSERTED SOMEWHERE ELSE. The
// build its comment names and rejects -- "answer the ciphertext and report the store's refusal some
// other way" -- is invisible to every assertion in this file, because the three doors go on
// dropping what it hands them. Measured over the whole of ./mls/... and ./message/... with that
// function's `return nil, err` rewritten as `return private, err`: 7,488 pass and ONE fails, and
// the one is TestSealAndRecordDropsTheCiphertextWhoseGenerationItCouldNotRecord -- so before that
// case was written, nothing in either package saw it. It sees it by calling the function rather
// than a door, and it exists because this header claimed the property twice while nothing held
// it.
//
// AND IT IS THE CONTROL'S VACUITY GUARD AS WELL, in the other direction: a door that answered
// nothing over a store that is not refusing would satisfy the refusal assertions below without ever
// having sealed anything, so the control requires the message to come back before the refusing run
// requires it not to.
type sealSiteDoor struct {
	site  string
	door  string
	drive func(t *testing.T, group *Group) (handedBack bool, err error)
}

// sealSiteDoors is one entry per derived site. It is NOT the class -- sealSiteCallers is -- and the
// gate below compares the two in both directions, so an entry naming a site this package no longer
// has fails here as loudly as a site nothing drives.
func sealSiteDoors() []sealSiteDoor {
	return []sealSiteDoor{
		{
			site: "propose",
			door: "(*Group).ProposeUpdate",
			drive: func(t *testing.T, group *Group) (bool, error) {
				proposal, err := group.ProposeUpdate()
				return proposal != nil, err
			},
		},
		{
			site: "CreateCommit",
			door: "(*Group).CreateCommit",
			drive: func(t *testing.T, group *Group) (bool, error) {
				result, err := group.CreateCommit(nil, nil, nil)
				return result != nil, err
			},
		},
		{
			site: "Protect",
			door: "(*Group).Protect",
			drive: func(t *testing.T, group *Group) (bool, error) {
				sealed, err := group.Protect(nil, []byte("a message whose generation nothing recorded"))
				return sealed != nil, err
			},
		},
	}
}

// sealSiteConsumedTotal is how many generations this member's leaf has spent across ALL of its
// ratchets, summed rather than read per kind.
//
// Summed because the kind a door draws from is the door's business: a proposal and a commit spend
// the handshake ratchet and an application message spends the other, and a table that named the
// kind per door would be a second enumeration of something the code already decides. What every
// site has in common is that a seal costs exactly one generation of this leaf, and that is what is
// counted.
func sealSiteConsumedTotal(t *testing.T, group *Group) uint64 {
	t.Helper()
	entries, err := group.secretTree.SenderRatchets(group.OwnLeafIndex())
	if err != nil {
		t.Fatalf("SenderRatchets: %v", err)
	}
	total := uint64(0)
	for _, entry := range entries {
		total += entry.Consumed
	}
	return total
}

// TestSealAndRecordDropsTheCiphertextWhoseGenerationItCouldNotRecord is sealAndRecordLocked's own
// half of the sentence, asserted at the frame that makes it.
//
// WHY IT IS NOT THE GATE BELOW. That case drives the three exported doors, and every one of them
// writes `return nil, err` over whatever the seal answered -- so a sealAndRecordLocked that handed
// back the ciphertext ALONGSIDE the store's refusal is dropped by its callers and observed by
// nothing. Measured: with its refusal arm rewritten as `return private, err`, ./mls/... and
// ./message/... come back 7,488 passing and one failing, and the one is this case. The gate below
// passes under that rewrite, at all three of its doors. The function's own comment states the drop
// as the reason the
// order is safe -- "a refusal here costs the generation and nothing else: the ciphertext is dropped
// inside this function, so nothing under that generation ever reaches a peer" -- so the frame that
// makes the claim is the frame it is asserted at.
//
// THE CONTROL IS THE SAME CALL OVER A STORE THAT IS NOT REFUSING, for the reason the gate below
// gives at each door: a call that answered nothing for a reason of its own would satisfy the
// refusal assertion perfectly and observe nothing.
func TestSealAndRecordDropsTheCiphertextWhoseGenerationItCouldNotRecord(t *testing.T) {
	crypto := testCrypto(t)
	owner := testIdentity(t, crypto, "owner")
	cfg := testGroupConfig(t, crypto, owner, "seal-and-record-drop")
	store := &refusingPutStore{testStore: newTestStore()}
	cfg.Store = store
	group, err := NewGroup(cfg, owner.SigPriv, BasicCredential(owner.IdentityPub))
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}
	defer group.Close()

	private, err := sealOneMessageThroughTheRecordingDoor(t, group)
	if err != nil || private == nil {
		t.Fatalf("sealAndRecordLocked over a store that is not refusing answered (%v, %v); this call is not reachable in this fixture, so the refusal below would observe nothing",
			private, err)
	}

	// armed after the group exists and after the control, for the gate's reason: NewGroup
	// persists, so a store refusing from the start refuses the constructor.
	store.refusing = true
	before := sealSiteConsumedTotal(t, group)
	private, err = sealOneMessageThroughTheRecordingDoor(t, group)
	if !errors.Is(err, errTheStoreRefusedThisWrite) {
		t.Fatalf("sealAndRecordLocked over a refusing store = %v, want the store's own refusal", err)
	}
	if private != nil {
		t.Fatalf("sealAndRecordLocked answered a *PrivateMessage ALONGSIDE the store's refusal; those octets are sealed under generation %d and the persisted state does not carry it, so a restore of this member draws that generation again and two plaintexts go out under one key and nonce",
			before)
	}
	if after := sealSiteConsumedTotal(t, group); after != before+1 {
		t.Fatalf("the refused seal moved this leaf from %d consumed generation(s) to %d, want %d: the drop above is being read over a call that never sealed",
			before, after, before+1)
	}
}

// sealOneMessageThroughTheRecordingDoor calls sealAndRecordLocked the way the three doors do.
//
// It builds the authenticated content the way (*Group).Protect does and takes stateLock the way
// every door does, because that function's contract is "the caller holds stateLock" and a fixture
// that ignored it would be measuring a call this package never makes.
func sealOneMessageThroughTheRecordingDoor(t *testing.T, group *Group) (*PrivateMessage, error) {
	t.Helper()
	group.stateLock.Lock()
	defer group.stateLock.Unlock()
	groupContext, err := syntax.Marshal(group.context)
	if err != nil {
		t.Fatalf("marshal group context: %v", err)
	}
	authenticated, err := SignAuthenticatedContent(group.crypto, group.signer,
		WireFormatPrivateMessage, &FramedContent{
			GroupId:         cloneBytes(group.context.GroupId),
			Epoch:           group.context.Epoch,
			Sender:          Sender{SenderType: SenderTypeMember, LeafIndex: group.ownLeaf},
			ContentType:     ContentTypeApplication,
			ApplicationData: []byte("a message whose generation nothing recorded"),
		}, groupContext)
	if err != nil {
		t.Fatalf("SignAuthenticatedContent: %v", err)
	}
	return group.sealAndRecordLocked(authenticated)
}

// TestEverySealSiteRefusesWhenTheStoreRefusesToRecordWhatItSpent is the gate.
//
// THE CONTROL RUNS FIRST AT EVERY DOOR, on a group whose store is not refusing, and it is not
// decoration: without it, a door that had started refusing for some reason of its own -- a closed
// group, a missing proposal, an argument this fixture stopped building correctly -- would satisfy
// the refusal assertion below perfectly, and this gate would report that the persist is observed at
// a door where nothing is.
//
// AND THE GENERATION IS COUNTED ACROSS THE REFUSAL. sealAndRecordLocked's comment says what a
// refusal costs -- "the generation and nothing else: the ciphertext is dropped inside this
// function" -- and that sentence is the difference between this order and the other one. If the
// persist ran BEFORE the seal, the ratchet would not have moved and the store would be right about
// the position; it runs after, so the honest record is that the generation is spent and the write
// that would have recorded it failed. Counting it is what says the refusal happened where the
// comment says it does rather than in front of the seal.
func TestEverySealSiteRefusesWhenTheStoreRefusesToRecordWhatItSpent(t *testing.T) {
	crypto := testCrypto(t)
	doors := sealSiteDoors()
	sites := []string{}
	for _, one := range doors {
		sites = append(sites, one.site)
	}
	slices.Sort(sites)
	derived := sealSiteCallers(t)
	if !slices.Equal(sites, derived) {
		t.Fatalf("this package's source reaches %s from %v and this fixture drives %v; a seal site nothing drives is a site where a persist that stopped running is observed by nothing, which is the measured shape this file exists for",
			sealAndRecordName, derived, sites)
	}
	t.Logf("%d seal site(s) derived from the source: %v", len(derived), derived)

	for _, one := range doors {
		t.Run(one.site, func(t *testing.T) {
			// the control, on a group of its own so the state it leaves behind is not the state
			// the refusing run is driven over.
			owner := testIdentity(t, crypto, "owner")
			cfg := testGroupConfig(t, crypto, owner, "seal-control-"+one.site)
			control := &refusingPutStore{testStore: newTestStore()}
			cfg.Store = control
			green, err := NewGroup(cfg, owner.SigPriv, BasicCredential(owner.IdentityPub))
			if err != nil {
				t.Fatalf("NewGroup: %v", err)
			}
			defer green.Close()
			handedBack, err := one.drive(t, green)
			if err != nil {
				t.Fatalf("%s over a store that is not refusing = %v; this door is not reachable in this fixture, so the refusal below would observe nothing",
					one.door, err)
			}
			if !handedBack {
				t.Fatalf("%s answered nothing and no error over a store that is not refusing", one.door)
			}

			subject := testIdentity(t, crypto, "owner")
			subjectCfg := testGroupConfig(t, crypto, subject, "seal-subject-"+one.site)
			store := &refusingPutStore{testStore: newTestStore()}
			subjectCfg.Store = store
			group, err := NewGroup(subjectCfg, subject.SigPriv, BasicCredential(subject.IdentityPub))
			if err != nil {
				t.Fatalf("NewGroup: %v", err)
			}
			defer group.Close()
			// armed AFTER the group exists, because NewGroup persists: a store refusing from the
			// start refuses the constructor and this case never reaches the door it is about.
			before := sealSiteConsumedTotal(t, group)
			store.refusing = true
			handedBack, err = one.drive(t, group)
			if !errors.Is(err, errTheStoreRefusedThisWrite) {
				t.Fatalf("%s over a refusing store = %v, want the store's own refusal: this seal handed out a message whose generation nothing has recorded, and a restore of this member then draws it again",
					one.door, err)
			}
			if handedBack {
				t.Fatalf("%s answered a message ALONGSIDE the store's refusal; those octets are on the wire under a generation the persisted state does not carry",
					one.door)
			}
			after := sealSiteConsumedTotal(t, group)
			if after != before+1 {
				t.Fatalf("%s over a refusing store moved this leaf from %d consumed generation(s) to %d, want %d: the persist is recording something other than a seal that has already happened",
					one.door, before, after, before+1)
			}
		})
	}
}
