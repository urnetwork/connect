package protocol_test

// RULING 32: read_epoch joins §4.3.4's FetchAttestation and its signing preimage.
//
// §4.3.4 puts class_mask and heads_only in the preimage "so that a filtered fetch is
// not byte-indistinguishable from a withholding one". The epoch ceiling of §5.1.1
// made high_water_record_id relative to the reader's own epoch, which is a THIRD
// filter that section did not name — and measured through the server's own read path,
// a server clamping every reader to epoch 1 and asked at read_epoch = 3 under a valid
// req_auth answered a FetchResponse that proto.Equals the honest read_epoch = 1
// answer: 7 of 12 records, two entire epochs, withheld with no error and no hole, and
// the receiver's omission predicate answering "nothing omitted".
//
// THE SIGNATURE IS UNBUILT IN THIS REPOSITORY AND THESE TESTS SAY SO RATHER THAN
// IMPLYING OTHERWISE. TestNothingHereComputesTheAttestationPreimage is that claim
// written as a measurement with its own positive control, so a reader who believes
// this file is enforcing a signature is corrected by a test log rather than by a
// surprise. What this repository owns is the SHAPE — the proto and the transcribed
// preimage a second implementation builds from — and the gates below are about the
// shape agreeing with Spec B §4.3.4 and MASTER §9.4.

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/urnetwork/connect/protocol"
)

// The four domain-separation labels this file looks for, ASSEMBLED FROM TWO PIECES
// RATHER THAN WRITTEN WHOLE.
//
// TestNothingHereComputesTheAttestationPreimage walks every .go file under the
// repository root, and this file is one of them. A literal label here would be a hit
// on the test's own source, and an absence claim that convicts its own evidence is
// worth nothing. Splitting the constant keeps the needle out of the haystack. It is
// not obfuscation: the assembled values are logged in full by that test, so a reader
// of the output sees exactly what was searched for.
const urLabelPrefix = "URmessage/" + "v1/"

var (
	attestLabel    = urLabelPrefix + "attest"
	writeAuthLabel = urLabelPrefix + "write"
	reqAuthLabel   = urLabelPrefix + "req"
	epochKeysLabel = urLabelPrefix + "epochkeys"
)

// TestFetchAttestationCarriesTheReadEpochItWasServedUnder is the shape half: the
// field exists, at the number Spec B §4.3.4 and MASTER §9.4 both give it, and it
// survives the wire.
//
// The round trip is asserted through the whole envelope rather than on the
// attestation alone, because that is the path it travels: a field that is dropped by
// a nested marshal is dropped exactly where nobody looks.
func TestFetchAttestationCarriesTheReadEpochItWasServedUnder(t *testing.T) {
	md := (*protocol.FetchAttestation)(nil).ProtoReflect().Descriptor()
	readEpoch := md.Fields().ByName("read_epoch")
	if readEpoch == nil {
		t.Fatal("FetchAttestation has no read_epoch. Ruling 32 puts the ceiling this answer was " +
			"served under inside the attested field list, because without it a short-ceiling " +
			"withholding is byte-identical to the honest answer at that ceiling.")
	}
	if readEpoch.Number() != 11 {
		t.Errorf("FetchAttestation.read_epoch is field %d; Spec B §4.3.4 and MASTER §9.4 both number "+
			"it 11 — not 10, because `sig` landed at 10 and a landed field number is never "+
			"renumbered, and not 14, because 14 is the read_epoch slot on the REQUEST messages "+
			"whose numbers are inside canonical_request_bytes", readEpoch.Number())
	}
	if readEpoch.Kind() != protoreflect.Uint64Kind || readEpoch.Cardinality() != protoreflect.Optional {
		t.Errorf("FetchAttestation.read_epoch is %v %v, want a singular uint64 — the preimage takes it "+
			"as u64(read_epoch)", readEpoch.Cardinality(), readEpoch.Kind())
	}
	if sig := md.Fields().ByName("sig"); sig == nil || sig.Number() != 10 {
		t.Errorf("FetchAttestation.sig is %v; it landed at 10 and stays there", sig)
	}

	envelope := &protocol.MessageServerResponse{
		RequestId: 7,
		Reason:    protocol.Reason_REASON_OK,
		Body: &protocol.MessageServerResponse_Fetch{
			Fetch: &protocol.FetchResponse{
				HighWaterRecordId: 5,
				Complete:          true,
				Attestation: &protocol.FetchAttestation{
					GroupId:           []byte("group-id-thirty-two-octets-long!"),
					SinceRecordId:     1,
					UntilRecordId:     5,
					RecordIds:         []uint64{2, 3, 4, 5},
					HighWaterRecordId: 5,
					ServerTimeMs:      1758499200000,
					ServerId:          []byte("server-id-16-b!!"),
					ClassMask:         0b101,
					HeadsOnly:         true,
					ReadEpoch:         3,
					Sig:               []byte("not a signature; nothing here signs"),
				},
			},
		},
	}
	bs, err := proto.Marshal(envelope)
	if err != nil {
		t.Fatalf("the envelope does not marshal: %v", err)
	}
	var back protocol.MessageServerResponse
	if err := proto.Unmarshal(bs, &back); err != nil {
		t.Fatalf("the envelope does not unmarshal: %v", err)
	}
	got := back.GetFetch().GetAttestation()
	if got == nil {
		t.Fatal("the attestation did not survive the round trip at all")
	}
	if got.GetReadEpoch() != 3 {
		t.Errorf("read_epoch came back %d, want 3", got.GetReadEpoch())
	}
	if !proto.Equal(envelope, &back) {
		t.Error("the envelope does not round-trip unchanged")
	}

	// AND IT IS ON THE WIRE, not merely in the struct. Two attestations differing in
	// read_epoch alone must not marshal to the same octets — which is the property that
	// would fail if the field were dropped from the descriptor but left on the Go type,
	// and the only one that makes signing it worth anything. The identical pair is the
	// inline control: without it, an encoder that produced different bytes every time
	// would pass this.
	clamped := proto.Clone(envelope).(*protocol.MessageServerResponse)
	clamped.GetFetch().GetAttestation().ReadEpoch = 1
	clampedBytes, err := proto.Marshal(clamped)
	if err != nil {
		t.Fatalf("the clamped envelope does not marshal: %v", err)
	}
	if string(clampedBytes) == string(bs) {
		t.Error("an answer served at read_epoch 1 and one served at read_epoch 3 marshal to the same " +
			"octets, which is ruling 32's whole measurement re-created: the ceiling is invisible")
	}
	same := proto.Clone(envelope).(*protocol.MessageServerResponse)
	sameBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(same)
	if err != nil {
		t.Fatalf("the control does not marshal: %v", err)
	}
	control, err := proto.MarshalOptions{Deterministic: true}.Marshal(envelope)
	if err != nil {
		t.Fatalf("the control does not marshal: %v", err)
	}
	if string(sameBytes) != string(control) {
		t.Error("control: two identical envelopes marshal to different octets, so the comparison above " +
			"proves nothing about read_epoch")
	}
}

// attestationPreimageSource returns the §4.3.4 preimage block as message.proto
// transcribes it: the comment on FetchAttestation.sig.
//
// The .proto and not the .pb.go, because the .proto is the artefact a second
// implementation reads and this block is the ONLY statement of the preimage that
// exists in this repository — there is no Go function to compare it against.
func attestationPreimageSource(t *testing.T) string {
	t.Helper()
	bs, err := os.ReadFile(filepath.Join(".", "message.proto"))
	if err != nil {
		t.Fatalf("message.proto is unreadable: %v", err)
	}
	// LINE ENDINGS NORMALISED BEFORE ANY MATCH. .gitattributes pins *.proto to eol=lf
	// precisely because core.autocrlf=true is set at system scope on the machines that
	// build this tree, and this repository has already lost 84 source anchors to a
	// checkout that wrote CRLF under a gate matching LF. A gate that depends on that
	// pin holding is a gate with a second failure mode; this one does not.
	src := strings.ReplaceAll(string(bs), "\r\n", "\n")
	open := strings.Index(src, "message FetchAttestation {")
	if open < 0 {
		t.Fatal("message.proto has no `message FetchAttestation {`; this test is reading the wrong file")
	}
	end := strings.Index(src[open:], "\n}")
	if end < 0 {
		t.Fatal("message FetchAttestation is not closed in message.proto")
	}
	block := src[open : open+end]

	// NARROWED TO THE PREIMAGE ITSELF, from its label to its last term, and not left as
	// the whole message. Every field name also appears in this message's own field
	// DECLARATIONS, so a coverage check over the whole block would answer "covered" for
	// a field that is declared and not signed — which is precisely the defect ruling 32
	// corrected, and a gate that cannot see it is a gate that would have let it through.
	// Both ends are positive controls for the slice.
	label := strings.Index(block, attestLabel)
	if label < 0 {
		t.Fatalf("the FetchAttestation block read from message.proto carries no preimage label, so the "+
			"slice is wrong. It is %d octets.", len(block))
	}
	const lastTerm = "u64(server_time_ms)"
	tail := strings.Index(block[label:], lastTerm)
	if tail < 0 {
		t.Fatalf("the preimage in message.proto does not end at %s; §4.3.4 and MASTER §9.4 both close "+
			"it with that term, so either the transcription moved or this slice is wrong", lastTerm)
	}
	preimage := block[label : label+tail+len(lastTerm)]
	if strings.Contains(preimage, "= 11;") || strings.Contains(preimage, "= 10;") {
		t.Fatalf("the preimage slice reached a field declaration, so it is wider than the preimage and "+
			"the coverage check below would pass on declarations rather than on signed terms: %q", preimage)
	}
	return preimage
}

// TestTheAttestationPreimageCoversEveryAttestedField is the preimage half, and the
// field set is DERIVED from the descriptor rather than listed: every field of
// FetchAttestation except `sig` itself is named in the preimage, because §4.5 calls
// this an explicit named field list and MASTER §9.4 requires both sides to agree on
// it byte for byte. A field added to the message and forgotten in the preimage is the
// exact defect this catches — and it is the defect ruling 32 corrected, one field
// earlier, when the ceiling landed and the preimage did not move.
//
// The complement is printed: `sig` is the one field excluded, because a signature
// cannot be inside its own preimage.
func TestTheAttestationPreimageCoversEveryAttestedField(t *testing.T) {
	block := attestationPreimageSource(t)
	fields := (*protocol.FetchAttestation)(nil).ProtoReflect().Descriptor().Fields()

	covered := []string{}
	excluded := []string{}
	for i := 0; i < fields.Len(); i++ {
		name := string(fields.Get(i).Name())
		if name == "sig" {
			excluded = append(excluded, name)
			continue
		}
		covered = append(covered, name)
		// record_ids is written in the preimage as u32(count) ‖ u64(record_id[0]) ‖ …,
		// which is the singular spelling of the same field
		needle := name
		if name == "record_ids" {
			needle = "record_id[0]"
		}
		if !strings.Contains(block, needle) {
			t.Errorf("FetchAttestation.%s is a field of the attestation and %q appears nowhere in the "+
				"§4.3.4 preimage this file transcribes. MASTER §9.4 makes the preimage an explicit "+
				"named field list that both sides must agree on byte for byte, so a field outside it "+
				"is a value the server can choose freely and still be believed — which is exactly "+
				"what the ceiling was until ruling 32.", name, needle)
		}
	}
	sort.Strings(covered)
	t.Logf("attested fields covered by the preimage (%d): %v", len(covered), covered)
	t.Logf("complement, excluded from the preimage (%d): %v — a signature is not inside its own preimage",
		len(excluded), excluded)

	if len(covered) != 10 {
		t.Errorf("the preimage covers %d fields besides sig. Spec B §4.5 counts TEN attested fields and "+
			"names them: server_id, group_id, since_record_id, until_record_id, high_water_record_id, "+
			"class_mask, heads_only, read_epoch, record_ids[] and server_time_ms. That list read "+
			"\"nine\" against eight until 2026-09-22, so recount it in §4.3.4 before changing this "+
			"number here.", len(covered))
	}
	if len(excluded) != 1 {
		t.Errorf("%d fields were excluded from the preimage check, want exactly sig: %v", len(excluded), excluded)
	}
	// and the ruling itself, named: the term is in the block, in the position §4.3.4
	// puts it, after u8(heads_only) and before u32(count)
	heads := strings.Index(block, "u8(heads_only)")
	epoch := strings.Index(block, "u64(read_epoch)")
	count := strings.Index(block, "u32(count)")
	if epoch < 0 {
		t.Fatal("u64(read_epoch) is not in the §4.3.4 preimage this file transcribes; ruling 32 puts " +
			"it there, and without it a server applying a shorter ceiling than the request named is " +
			"byte-indistinguishable from the honest answer at that ceiling")
	}
	if !(heads >= 0 && heads < epoch && epoch < count) {
		t.Errorf("the preimage orders heads_only at %d, read_epoch at %d and count at %d; Spec B §4.3.4 "+
			"and MASTER §9.4 put read_epoch between them, and a preimage in a different order is a "+
			"different preimage", heads, epoch, count)
	}
}

// TestNothingHereComputesTheAttestationPreimage states the honest position as a
// measurement instead of a sentence.
//
// The §4.3.4 attestation label is in no Go file of this repository. The inline
// positive controls, in the same walk, are the write_auth and req_auth labels — the
// two preimages this repository DOES build, in message/writeauth.go — and the
// epoch_keys label in message/attachment.go. A walk that found nothing anywhere would
// pass an absence claim while reading no files at all; those three are what make the
// zero mean something. The four labels are assembled above rather than written whole,
// for the reason given there.
//
// protocol/message.pb.go is exempt and the exemption is PRINTED rather than assumed:
// protoc-gen-go carries only the first line of a field's trailing comment onto the
// generated struct, so the label does not in fact reach it today, and an exemption
// nobody can see being unused is a hole.
//
// If somebody builds the signer or the verifier here, this test fails and the file
// comment above stops being true at the same moment — which is the point.
func TestNothingHereComputesTheAttestationPreimage(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("cannot resolve the repository root: %v", err)
	}
	labels := map[string][]string{
		attestLabel:    nil,
		writeAuthLabel: nil,
		reqAuthLabel:   nil,
		epochKeysLabel: nil,
	}
	walked := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == ".git" || name == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		bs, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		walked++
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		rel = filepath.ToSlash(rel)
		for label := range labels {
			if strings.Contains(string(bs), label) {
				labels[label] = append(labels[label], rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("the walk failed: %v", err)
	}
	if walked == 0 {
		t.Fatal("walked no Go file at all, so every absence below is an absence of reading")
	}
	for _, label := range []string{attestLabel, writeAuthLabel, reqAuthLabel, epochKeysLabel} {
		t.Logf("%-24s %v", label, labels[label])
	}

	// the controls first: if these are empty the walk is wrong and the absence below
	// says nothing
	for _, control := range []struct{ label, want string }{
		{writeAuthLabel, "message/writeauth.go"},
		{reqAuthLabel, "message/writeauth.go"},
		{epochKeysLabel, "message/attachment.go"},
	} {
		found := false
		for _, file := range labels[control.label] {
			if file == control.want {
				found = true
			}
		}
		if !found {
			t.Fatalf("control: %q is not in %s, so this walk is not reading the files it claims to and "+
				"the absence of %q below proves nothing", control.label, control.want, attestLabel)
		}
	}

	const exempt = "protocol/message.pb.go"
	exemptUsed := false
	for _, file := range labels[attestLabel] {
		if file == exempt {
			// the .proto comment, if protoc-gen-go ever carries the whole of it. It is a
			// transcription and not an implementation, and it is what the test above checks.
			exemptUsed = true
			continue
		}
		t.Errorf("%s carries the attestation label. If the §4.3.4 signature is now built in this "+
			"repository, the field list, the preimage order and the Ed25519 key custody of MASTER "+
			"§9.4 all become this repository's to hold — and the comments in message.proto that say "+
			"nothing here computes it are now false.", file)
	}
	t.Logf("walked %d Go files; the §4.3.4 signature is UNBUILT here and "+
		"Capabilities.attestation_supported is the flag that says so on the wire", walked)
	t.Logf("the %s exemption was %s", exempt, map[bool]string{true: "USED", false: "not used — protoc-gen-go carries only the first line of the trailing comment"}[exemptUsed])
}

// keylessComparableFields is the set of attestation fields a caller can check with NO
// KEY AT ALL, DERIVED and not listed.
//
// A keyless check is a comparison against a value the caller itself sent, so the set is
// exactly the intersection of FetchRequest's field names with FetchAttestation's.
// Everything outside it — until_record_id, record_ids, high_water_record_id,
// server_time_ms, server_id — is a value the caller holds no independent copy of and can
// only believe, and `sig` is the thing that is unbuilt. The intersection is printed by
// the test that uses it, because it is the whole scope of what "checkable without the
// signature" can possibly mean.
func keylessComparableFields(t *testing.T) []string {
	t.Helper()
	request := (*protocol.FetchRequest)(nil).ProtoReflect().Descriptor().Fields()
	attestation := (*protocol.FetchAttestation)(nil).ProtoReflect().Descriptor().Fields()
	sent := map[string]bool{}
	for i := 0; i < request.Len(); i++ {
		sent[string(request.Get(i).Name())] = true
	}
	out := []string{}
	for i := 0; i < attestation.Len(); i++ {
		if name := string(attestation.Get(i).Name()); sent[name] {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	if len(out) == 0 {
		t.Fatal("FetchRequest and FetchAttestation share no field name, so there is no keyless " +
			"comparison at all and the measurement below is a statement about nothing")
	}
	return out
}

// attestationDiff names the fields on which two attestations disagree. The field list is
// the descriptor's and not a hand-written one, so a field added to the message joins the
// comparison without anybody editing this file.
func attestationDiff(a, b *protocol.FetchAttestation) []string {
	fields := a.ProtoReflect().Descriptor().Fields()
	out := []string{}
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		if !a.ProtoReflect().Get(fd).Equal(b.ProtoReflect().Get(fd)) {
			out = append(out, string(fd.Name()))
		}
	}
	sort.Strings(out)
	return out
}

// WHAT THE KEYLESS COMPARISON CATCHES AND WHAT IT DOES NOT — MEASURED, because the
// paragraph at FetchAttestation.read_epoch claims both halves and a comment is not a
// measurement.
//
// This file said the other thing until 2026-09-22. The paragraph claimed the keyless
// comparison caught "exactly the withholding measured above", and a commit hand-off
// repeated that to the sdk owner as the argument for building the check. It is false,
// and the reason is one line long: `read_epoch` on the RESPONSE is a value the SERVER
// chooses, and ruling 32's adversary is the server. A clamping server names the ceiling
// the caller asked for and serves the shorter page under it.
//
// Ruling 32's own scenario, at the shapes this repository owns: a caller authenticated
// at read_epoch 3; an honest answer of 12 records with a ceiling-relative high water of
// 12; and a server clamping every reader to epoch 1, which has 5 records to serve. The
// two clamping servers differ only in what they put in this field:
//
//	the TRUTHFUL clamp names read_epoch = 1 -> refused, and by that field ALONE
//	the LYING clamp names read_epoch = 3    -> accepted, with 7 records and two whole
//	                                          epochs withheld
//
// The truthful clamp is the inline positive control: without it, "the lying clamp is
// accepted" would be a comparison that accepts everything. Neither result is an argument
// against ruling 32 — Spec B §5.1.1 puts read_epoch in the PREIMAGE for exactly this
// reason — they are an argument against the sentence that read as though the preimage
// were optional.
func TestTheKeylessCheckRefusesATruthfulClampAndNotALyingOne(t *testing.T) {
	const asked = 3
	group := []byte("group-id-thirty-two-octets-long!")

	// served is an answer of n records with the ceiling-relative high water that implies,
	// naming `ceiling` as the epoch it was served under. Everything a caller cannot check
	// independently moves with n; everything it can check is held identical on purpose,
	// because the question is which of those separates the three answers.
	served := func(n int, ceiling uint64) *protocol.FetchAttestation {
		ids := make([]uint64, 0, n)
		for i := 1; i <= n; i++ {
			ids = append(ids, uint64(i))
		}
		return &protocol.FetchAttestation{
			GroupId:           group,
			SinceRecordId:     0,
			UntilRecordId:     uint64(n),
			RecordIds:         ids,
			HighWaterRecordId: uint64(n),
			ServerTimeMs:      1758499200000,
			ServerId:          []byte("server-id-16-b!!"),
			ClassMask:         0,
			HeadsOnly:         false,
			ReadEpoch:         ceiling,
		}
	}
	honest := served(12, asked)
	truthful := served(5, 1)
	lying := served(5, asked)

	request := &protocol.FetchRequest{GroupId: group, SinceRecordId: 0, ReadEpoch: asked}
	keyless := keylessComparableFields(t)
	t.Logf("fields a caller can compare with NO KEY (%d): %v — the intersection of FetchRequest's "+
		"field names with FetchAttestation's", len(keyless), keyless)
	if len(keyless) < 2 {
		t.Fatalf("only %v is comparable without a key; the partition below is not a partition", keyless)
	}

	// the keyless check itself, over the derived set: every shared field of the
	// attestation compared against the same-named field of the request the caller sent.
	refusesOn := func(a *protocol.FetchAttestation) []string {
		bad := []string{}
		am, rm := a.ProtoReflect(), request.ProtoReflect()
		for _, name := range keyless {
			af := am.Descriptor().Fields().ByName(protoreflect.Name(name))
			rf := rm.Descriptor().Fields().ByName(protoreflect.Name(name))
			if af == nil || rf == nil {
				t.Fatalf("%q is in the derived intersection and is missing from one of the two "+
					"messages, so the derivation and the lookup disagree", name)
			}
			if !am.Get(af).Equal(rm.Get(rf)) {
				bad = append(bad, name)
			}
		}
		return bad
	}

	if bad := refusesOn(honest); len(bad) != 0 {
		t.Errorf("CONTROL: the honest answer at the ceiling the caller asked for is refused on %v. "+
			"A check that refuses the honest answer is not a check.", bad)
	}
	if bad := refusesOn(truthful); len(bad) != 1 || bad[0] != "read_epoch" {
		t.Errorf("CONTROL: the truthful clamp — an answer that says it was served at ceiling 1 to a "+
			"caller that authenticated at ceiling %d — is refused on %v, want exactly [read_epoch]. "+
			"If it is refused on nothing, the comparison accepts everything and the result below "+
			"proves nothing; if on more, something other than the ceiling is doing the work.",
			asked, bad)
	}
	if bad := refusesOn(lying); len(bad) != 0 {
		t.Errorf("the lying clamp is refused on %v. If the keyless comparison has become able to "+
			"catch a server that names the ceiling it was asked for and serves less, the paragraph "+
			"at FetchAttestation.read_epoch now UNDERstates what this field buys, and it should be "+
			"rewritten to whatever made that true.", bad)
	}
	t.Logf("keyless verdicts — honest: %v, truthful clamp: %v, lying clamp: %v",
		refusesOn(honest), refusesOn(truthful), refusesOn(lying))

	// AND WHAT WAS ACCEPTED IS A WITHHOLDING, not a smaller honest page: the lying clamp
	// serves strictly fewer records and names a strictly smaller high water, which is the
	// reader's only omission detector.
	if len(lying.GetRecordIds()) >= len(honest.GetRecordIds()) ||
		lying.GetHighWaterRecordId() >= honest.GetHighWaterRecordId() {
		t.Fatalf("the clamping answer serves %d records with high water %d against the honest %d and "+
			"%d; it withholds nothing and this measurement is about nothing",
			len(lying.GetRecordIds()), lying.GetHighWaterRecordId(),
			len(honest.GetRecordIds()), honest.GetHighWaterRecordId())
	}
	t.Logf("the ACCEPTED answer withholds %d of %d records and names high water %d against %d",
		len(honest.GetRecordIds())-len(lying.GetRecordIds()), len(honest.GetRecordIds()),
		lying.GetHighWaterRecordId(), honest.GetHighWaterRecordId())

	// THE PARTITION, IN BOTH DIRECTIONS, which is the property the three verdicts above
	// are instances of: the fields that separate the honest answer from the LYING one are
	// DISJOINT from the fields a caller can check without a key, and the fields that
	// separate it from the TRUTHFUL one meet that set in exactly `read_epoch`.
	lyingDiff := attestationDiff(honest, lying)
	truthfulDiff := attestationDiff(honest, truthful)
	t.Logf("honest vs lying clamp differ on %v; honest vs truthful clamp differ on %v",
		lyingDiff, truthfulDiff)
	if len(lyingDiff) == 0 {
		t.Fatal("the honest answer and the lying clamp are identical in every field, so the two " +
			"servers are not doing different things and the disjointness below is vacuous")
	}
	comparable := map[string]bool{}
	for _, name := range keyless {
		comparable[name] = true
	}
	for _, name := range lyingDiff {
		if comparable[name] {
			t.Errorf("the honest answer and the lying clamp differ on %q, which a caller CAN compare "+
				"without a key. That would make ruling 32's withholding refusable unsigned, and the "+
				"paragraph at FetchAttestation.read_epoch says it is not.", name)
		}
	}
	met := []string{}
	for _, name := range truthfulDiff {
		if comparable[name] {
			met = append(met, name)
		}
	}
	if len(met) != 1 || met[0] != "read_epoch" {
		t.Errorf("the truthful clamp is separated from the honest answer, among the keyless fields, "+
			"by %v; want exactly [read_epoch]. That intersection IS the keyless half of ruling 32, "+
			"and if it is empty the check catches nothing at all.", met)
	}
}

// THE KEYLESS HALF OF THE CEILING, SAID OUT LOUD AT THE FIELD — AND ITS LIMIT SAID IN
// THE SAME BREATH.
//
// Ruling 32 put read_epoch in the attestation and in the signing preimage, and that
// preimage is unbuilt here — true, and measured by
// TestNothingHereComputesTheAttestationPreimage. Read alone, that invites the conclusion
// that the field buys nothing until §9.4's fleet key exists, and that is wrong: the
// caller holds its own read_epoch and can compare. But the correction was itself
// overstated here until 2026-09-22 — the block claimed the keyless comparison caught
// "exactly the withholding measured above" — and
// TestTheKeylessCheckRefusesATruthfulClampAndNotALyingOne measures that it does not. So
// the clause list below pins BOTH halves, because either half read alone is an
// instruction to build the wrong thing.
//
// WHAT IT STILL DOES NOT SAY, deliberately: anything about what any particular consumer
// does today. A comment in this repository asserting the state of another repository's
// code is the stale-disclosure class item 248 exists to catch — it would be true on the
// day it was written and false on the day somebody acted on it. The shape property is
// permanent; the survey belongs in the commit that measured it.
func TestTheReadEpochSaysWhatTheKeylessCheckCatchesAndWhatItDoesNot(t *testing.T) {
	md := (*protocol.FetchAttestation)(nil).ProtoReflect().Descriptor()
	if md.Fields().ByName("read_epoch") == nil {
		t.Fatal("FetchAttestation has no read_epoch; the clauses below are about that field")
	}
	// the shape the sentence rests on, checked rather than recited: the REQUEST carries
	// the value the answer is compared against. Without that field the comparison the
	// clauses describe would be impossible and the sentence would be false.
	request := (*protocol.FetchRequest)(nil).ProtoReflect().Descriptor()
	asked := request.Fields().ByName("read_epoch")
	if asked == nil {
		t.Fatal("FetchRequest has no read_epoch, so a caller holds no ceiling to compare the " +
			"attestation's against and the keyless check below is available to nobody")
	}
	if asked.Number() != 14 {
		t.Errorf("FetchRequest.read_epoch is field %d; the comment says 14, and §4.3.8 reserves 14 "+
			"on the request messages", asked.Number())
	}

	block := flatten(t, messageBlock(t, "FetchAttestation"),
		"AND THIS FIELD IS CHECKABLE WITHOUT THE SIGNATURE, BUT WHAT THE KEYLESS")
	for _, clause := range []struct {
		what   string
		phrase string
	}{
		{"that the field does not wait for the signature", "AND THIS FIELD IS CHECKABLE WITHOUT THE SIGNATURE,"},
		{"that the keyless check is narrower than the withholding", "BUT WHAT THE KEYLESS CHECK CATCHES IS NARROWER THAN THE WITHHOLDING ABOVE"},
		{"the terms it is like", "`group_id` and `since_record_id` above are already comparable with"},
		{"where the caller's own copy comes from", "FetchRequest.read_epoch is field 14 of that"},
		{"which server the keyless check actually refuses", "TRUTHFULLY names a ceiling below the one the caller asked for is refusable TODAY"},
		{"that it does NOT refuse the one ruling 32 measured", "IT DOES NOT CATCH THE WITHHOLDING MEASURED ABOVE."},
		{"why not — the field is the server's to choose", "This field is SERVER-CHOSEN"},
		{"the measurement, by name", "TestTheKeylessCheckRefusesATruthfulClampAndNotALyingOne"},
		{"Spec B §5.1.1 agreeing in its own words", "byte-indistinguishable from an honest one unless `read_epoch` is in the attestation preimage"},
		{"what the signature actually adds", "SO WHAT THE SIGNATURE ADDS IS THE BINDING"},
		{"that the C-4 mechanism is cited and not measured here", "CITED HERE AND NOT MEASURED"},
	} {
		if !strings.Contains(block, clause.phrase) {
			t.Errorf("FetchAttestation.read_epoch does not state %s. The phrase %q is gone. Either "+
				"half of this paragraph read alone is an instruction to build the wrong thing: "+
				"without the first a reader leaves the free comparison unbuilt, and without the "+
				"second a reader builds it believing it closes ruling 32's withholding, which "+
				"TestTheKeylessCheckRefusesATruthfulClampAndNotALyingOne measures that it does not.",
				clause.what, clause.phrase)
		}
	}
}
