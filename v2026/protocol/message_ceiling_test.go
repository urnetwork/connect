package protocol_test

// WHAT A SERVED high_water_record_id MEANS, ONCE THERE IS A CEILING UNDER IT.
//
// F0 (ledger item 246) made the read path serve only records at or below the
// request's own authenticated `read_epoch`. That changed the meaning of a field this
// file had already shipped: `FetchResponse.high_water_record_id` stopped being the
// group's maximum and became the maximum the reader may see. The code moved and the
// comment did not. It read "the group's max at read time" until 2026-09-22 — false
// since F0, and contradicted seventeen lines below it by a comment the SAME commit
// added, which says §5.1.1's ceiling "made high_water_record_id relative to the
// reader's own epoch".
//
// That is the class ledger item 248 records as closed on the specification side and
// left open here: "Spec B's own high_water_record_id line had been made false by the
// code and disclosed rather than amended". This file is the wire contract a second
// implementation reads, and it was the copy nobody amended.
//
// THE PROPERTY, AND IT IS NOT "THESE TWO COMMENTS MATCH". Every field named
// high_water_record_id in message.proto must SAY WHICH SIDE OF THE CEILING IT IS ON,
// and the answer is not the same for all of them — which is exactly why a blanket
// sentence would have been worse than none. Spec B §5.1.1 enumerates the arms and
// three answers come out of it:
//
//	Fetch          — bounded. §4.3.4's high water is the group's max at or below read_epoch.
//	Subscribe      — INHERITS it. §4.3.5: RecordPush's is ceiling-relative "exactly as §4.3.4's is".
//	WrapFetch      — NOT bounded, and applying it there would break the arm. Carries no high water.
//	RecoveryFetch  — "outside it entirely": authorized by the Ed25519 recovery proof and not by
//	                 req_auth, so the restorer holds no read key and NAMES NO EPOCH. Its answer
//	                 type, GroupRecords, therefore carries an ABSOLUTE high water.
//	GroupStatus    — "serves no records, so the rule as written does not reach it". ABSOLUTE, and
//	                 what the two absolute counters leak is OPEN as ledger item 248.
//
// So the guess a reader would make — that the ceiling is inherited everywhere — is
// wrong for two of the five, and which one a field is depends on how its arm is
// AUTHORIZED rather than on what it returns. A gate that demanded one sentence
// everywhere would have written the wrong rule into three of the five blocks.
//
// THE SET IS DERIVED FROM THE DESCRIPTOR and asserted against the dispositions in
// BOTH DIRECTIONS, so a sixth carrier of this field cannot arrive undispositioned and
// a disposition cannot outlive the field it describes.

import (
	"strings"
	"testing"
)

// highWaterField is the field name this gate is about. One constant, so the
// derivation below and the slicing control cannot drift apart.
const highWaterField = "high_water_record_id"

type ceilingDisposition struct {
	// ceilingRelative is which side of §5.1.1 this field is on. It drives the
	// cross-check: a bounded field must not claim to be absolute and an absolute one
	// must not claim to be bounded, so pasting the wrong block's sentence is red.
	ceilingRelative bool
	// mustSay is the phrase the declaration has to carry. Each one SPANS A LINE BREAK
	// in message.proto, which makes it the positive control for the flattening as well
	// as the anchor — see flatten's probe argument.
	mustSay string
	why     string
}

var highWaterDispositions = map[string]ceilingDisposition{
	"FetchResponse": {
		ceilingRelative: true,
		mustSay:         "AT OR BELOW read_epoch — the epoch ceiling of §5.1.1",
		why:             "Spec B §4.3.4 and §5.1.1: Fetch is the arm the ceiling was written for.",
	},
	"FetchAttestation": {
		ceilingRelative: true,
		mustSay:         "the attested copy of FetchResponse.high_water_record_id above, and CEILING-RELATIVE",
		why: "the same number, signed. Spec B C-4 compares attestations only within an identical " +
			"(class_mask, heads_only, read_epoch) filter BECAUSE the high water is ceiling-relative.",
	},
	"RecordPush": {
		ceilingRelative: true,
		mustSay:         "applies to this arm too: a subscription serves no record above the read_epoch",
		why:             "Spec B §4.3.5 and §5.1.1: a subscription is a streaming Fetch and INHERITS the rule.",
	},
	"GroupRecords": {
		ceilingRelative: false,
		mustSay:         "ABSOLUTE, and NOT ceiling-relative, unlike §4.3.4's and §4.3.5's",
		why: "its one carrier is RecoveryFetchResponse.groups, and §5.1.1 puts RecoveryFetch " +
			"\"outside it entirely\": §4.3.7 authorizes it by the Ed25519 recovery proof, so the " +
			"caller holds no read key and names no epoch for a ceiling to compare against.",
	},
	"GroupStatusResponse": {
		ceilingRelative: false,
		mustSay:         "ABSOLUTE, and NOT bounded by §5.1.1's epoch ceiling — this arm",
		why: "§5.1.1: GroupStatus serves no records, so the rule as written does not reach it. " +
			"What the absolute counters leak is OPEN as ledger item 248 and is not this file's to close.",
	},
}

// TestEveryHighWaterSaysWhichSideOfTheCeilingItIsOn is the gate item 248's class asks
// for: the two documents cannot drift again on this field, because the field's meaning
// is now written at every declaration of it and the set of declarations is derived.
func TestEveryHighWaterSaysWhichSideOfTheCeilingItIsOn(t *testing.T) {
	all := topLevelMessages(t)

	carriers := map[string]bool{}
	for _, name := range sortedKeys(all) {
		if all[name].Fields().ByName(highWaterField) != nil {
			carriers[name] = true
		}
	}
	t.Logf("messages carrying %s (%d): %v", highWaterField, len(carriers), sortedSet(carriers))

	if len(carriers) == 0 {
		t.Fatalf("no message in message.proto declares %s, so this gate is a search over nothing",
			highWaterField)
	}
	// the inline positive control: the arm the ceiling was written FOR is in the derived
	// set. If it is not, the derivation is reading something other than this file.
	if !carriers["FetchResponse"] {
		t.Fatalf("FetchResponse does not carry %s; the derivation is not reading message.proto",
			highWaterField)
	}

	// BOTH DIRECTIONS. An undispositioned carrier is a field whose meaning nobody
	// decided after F0; a disposition with no carrier is a sentence about a file that
	// has moved.
	for name := range carriers {
		if _, dispositioned := highWaterDispositions[name]; !dispositioned {
			t.Errorf("%s carries %s and nothing here says whether it is bounded by §5.1.1's epoch "+
				"ceiling. That answer is NOT the same for every arm — it follows from how the arm is "+
				"AUTHORIZED — so it has to be decided and written at the field, not inherited by "+
				"assumption.", name, highWaterField)
		}
	}
	for name := range highWaterDispositions {
		if !carriers[name] {
			t.Errorf("highWaterDispositions describes %s.%s and that field is gone or renamed",
				name, highWaterField)
		}
	}

	// and the two classes are BOTH non-empty, printed as the partition they are. A gate
	// whose every entry had the same answer would be a gate that could be satisfied by
	// one sentence pasted five times, which is the failure this whole file exists to
	// prevent.
	bounded := map[string]bool{}
	absolute := map[string]bool{}
	for name, disposition := range highWaterDispositions {
		if disposition.ceilingRelative {
			bounded[name] = true
		} else {
			absolute[name] = true
		}
	}
	t.Logf("ceiling-relative (%d): %v", len(bounded), sortedSet(bounded))
	t.Logf("absolute         (%d): %v", len(absolute), sortedSet(absolute))
	if len(bounded) == 0 || len(absolute) == 0 {
		t.Errorf("the dispositions put every high water on one side of the ceiling (%d bounded, %d "+
			"absolute). Spec B §5.1.1 names arms on both sides; if that has really changed, re-read "+
			"that section rather than deleting the side that emptied.", len(bounded), len(absolute))
	}

	for _, name := range sortedKeys(highWaterDispositions) {
		disposition := highWaterDispositions[name]
		if !carriers[name] {
			continue
		}
		raw := messageBlock(t, name)
		// the positive control for the slice: the declaration this is about is inside what
		// was sliced out, or the phrase below is being searched for in the wrong region
		if !strings.Contains(raw, "uint64 high_water_record_id") {
			t.Errorf("the %s block read from message.proto does not declare %s; the slice is wrong. "+
				"It is %d octets.", name, highWaterField, len(raw))
			continue
		}
		block := flatten(t, raw, disposition.mustSay)
		if !strings.Contains(block, disposition.mustSay) {
			t.Errorf("%s.%s does not say %q. %s", name, highWaterField, disposition.mustSay, disposition.why)
		}
		// THE CROSS-CHECK: the wrong side's marker must be absent. Without it the gate is
		// satisfied by a block that says both things, which is what a copy-paste from the
		// neighbouring message produces.
		wrongSide := "ABSOLUTE"
		if !disposition.ceilingRelative {
			wrongSide = "AT OR BELOW read_epoch"
		}
		if strings.Contains(block, wrongSide) {
			t.Errorf("%s.%s is dispositioned ceilingRelative=%v and its declaration also carries %q, "+
				"the marker of the other side. One of the two is a sentence copied from a message "+
				"this one does not behave like. %s",
				name, highWaterField, disposition.ceilingRelative, wrongSide, disposition.why)
		}
	}
}

// AND THE CLAUSE THAT COMES WITH THE CEILING ON THE ONE ARM THAT HAS A `complete`.
//
// Spec B §5.1.1, third consequence: "`complete` is false for the limit and never for
// the ceiling. A page the ceiling ends is complete." It is not decoration beside the
// high water — it is the half a client acts on. A reader that has ingested the record
// at its own ceiling, told `complete = false`, re-asks from the cursor it holds, is
// served nothing, and raises a no-progress error against a server that did exactly
// what it was asked. Spec B amended its own §4.3.4 block with this clause on the same
// day it amended the high water; this file took neither until now.
func TestTheFetchCompleteFlagSaysTheCeilingDoesNotClearIt(t *testing.T) {
	fetch := topLevelMessages(t)["FetchResponse"]
	if fetch == nil {
		t.Fatal("message.proto declares no FetchResponse")
	}
	if complete := fetch.Fields().ByName("complete"); complete == nil {
		t.Fatal("FetchResponse has no `complete`; the clause below is about that field")
	}
	block := flatten(t, messageBlock(t, "FetchResponse"),
		"NOT false for the ceiling: a page the ceiling ends is complete")
	for _, clause := range []struct {
		what   string
		phrase string
	}{
		{"what does make it false", "false when truncated by limit OR by max_response_bytes"},
		{"that the ceiling does not", "NOT false for the ceiling: a page the ceiling ends is complete"},
		{"why that matters to a client", "raise a no-progress error"},
	} {
		if !strings.Contains(block, clause.phrase) {
			t.Errorf("FetchResponse.complete does not state %s. The phrase %q is gone. Spec B §5.1.1 "+
				"makes this normative and this file is the copy a second implementation reads.",
				clause.what, clause.phrase)
		}
	}
}
