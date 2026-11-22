package connect

import (
	"bytes"
	"crypto/ed25519"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/urnetwork/connect/v2026/protocol"
)

func newTestLagKey(t *testing.T) (ed25519.PrivateKey, ed25519.PublicKey) {
	t.Helper()
	seed, err := NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	privateKey, err := ExtenderPrivateKeyFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	return privateKey, privateKey.Public().(ed25519.PublicKey)
}

func newTestLagTicketBody(extenderPublicKey ed25519.PublicKey, now time.Time) *protocol.LagTicketBody {
	return &protocol.LagTicketBody{
		TicketId:          bytes.Repeat([]byte{0x11}, 16),
		ClientId:          bytes.Repeat([]byte{0x22}, 16),
		ExtenderPublicKey: extenderPublicKey,
		NetworkHost:       "host.example",
		IssueTimeMs:       uint64(now.UnixMilli()),
		ExpireTimeMs:      uint64(now.Add(2 * time.Minute).UnixMilli()),
		TrackId:           bytes.Repeat([]byte{0x33}, 16),
		TrackPosition:     3,
		PingIntervalMs:    250,
		DownloadMs:        5000,
		UploadMs:          5000,
		MaxStreams:        4,
		MaxDownloadBytes:  2 << 30,
		MaxUploadBytes:    2 << 30,
	}
}

func newTestLagReportBody(extenderPublicKey ed25519.PublicKey, now time.Time) *protocol.LagReportBody {
	return &protocol.LagReportBody{
		TicketId:              bytes.Repeat([]byte{0x11}, 16),
		ExtenderPublicKey:     extenderPublicKey,
		TrackId:               bytes.Repeat([]byte{0x33}, 16),
		TrackPosition:         3,
		SessionStartTimeMs:    uint64(now.Add(-90 * time.Second).UnixMilli()),
		WindowStartTimeMs:     uint64(now.Add(-time.Minute).UnixMilli()),
		PingOffsetMs:          []uint32{0, 250, 500, 750},
		PingMicros:            []uint32{23410, 22980, 0, 61200},
		DownloadStartMs:       25000,
		DownloadMs:            5000,
		UploadStartMs:         30000,
		UploadMs:              5000,
		IntervalMs:            250,
		DownloadReceivedBytes: []uint64{1 << 20, 3 << 20},
		DownloadSentBytes:     []uint64{2 << 20, 3 << 20},
		UploadBytes:           []uint64{1 << 19, 1 << 20},
		DownloadStreams:       4,
		UploadStreams:         4,
		UploadVerified:        true,
		EndReason:             protocol.LagEndReason_LAG_END_COMPLETE,
	}
}

// A ticket round-trips through the root signature and its text form, names
// the signing key, and verifies under any accepted root key (M0, M3).
func TestLagTicketRoundTrips(t *testing.T) {
	now := time.Now()
	rootPrivateKey, rootPublicKey := newTestLagKey(t)
	_, otherPublicKey := newTestLagKey(t)
	_, extenderPublicKey := newTestLagKey(t)
	body := newTestLagTicketBody(extenderPublicKey, now)

	ticket, err := SignLagTicket(body, rootPrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ticket.RootKeyId, ExtenderKeyId(rootPublicKey)) {
		t.Fatal("the ticket does not name the signing key")
	}
	if !ed25519.Verify(rootPublicKey, append([]byte("ur-lag-ticket-v1"), ticket.Body...), ticket.RootSignature) {
		t.Fatal("the signature is not over the ticket domain and the body")
	}

	text, err := EncodeLagTicket(ticket)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(text, "=+/ \n") {
		t.Fatalf("ticket text %q is not unpadded base64url", text)
	}
	decodedTicket, err := DecodeLagTicket(text)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(decodedTicket, ticket) {
		t.Fatal("the ticket did not round trip through its text form")
	}

	for _, rootPublicKeys := range [][]ed25519.PublicKey{
		{rootPublicKey},
		{otherPublicKey, rootPublicKey},
		{rootPublicKey, otherPublicKey},
		// a malformed key is skipped, not fatal
		{ed25519.PublicKey{1, 2, 3}, rootPublicKey},
	} {
		verifiedBody, err := VerifyLagTicket(decodedTicket, rootPublicKeys, now)
		if err != nil {
			t.Fatal(err)
		}
		if !proto.Equal(verifiedBody, body) {
			t.Fatal("the verified body is not the signed body")
		}
	}

	// the key id is a hint only: a wrong one costs a try, never a refusal
	hintless := proto.Clone(ticket).(*protocol.LagTicket)
	hintless.RootKeyId = ExtenderKeyId(otherPublicKey)
	if _, err := VerifyLagTicket(hintless, []ed25519.PublicKey{otherPublicKey, rootPublicKey}, now); err != nil {
		t.Fatalf("a wrong key id refused the ticket: %v", err)
	}
	hintless.RootKeyId = nil
	if _, err := VerifyLagTicket(hintless, []ed25519.PublicKey{rootPublicKey}, now); err != nil {
		t.Fatalf("a missing key id refused the ticket: %v", err)
	}
}

// The drift test's flags (lag.dev/DRIFTTEST.md E4) round-trip through the
// root signature and the ticket's text form, each alone and together, on the
// latency-only caps its tickets carry (E3). A ticket without them reads both
// as false, and neither can be set or cleared on a signed ticket.
func TestLagTicketDriftFlagsRoundTrip(t *testing.T) {
	now := time.Now()
	rootPrivateKey, rootPublicKey := newTestLagKey(t)
	_, extenderPublicKey := newTestLagKey(t)
	rootPublicKeys := []ed25519.PublicKey{rootPublicKey}

	for _, flags := range []struct{ reportPromptly, noReport bool }{
		{false, false},
		{true, false},
		{false, true},
		{true, true},
	} {
		body := newTestLagTicketBody(extenderPublicKey, now)
		body.DownloadMs = 0
		body.UploadMs = 0
		body.MaxStreams = 0
		body.MaxDownloadBytes = 0
		body.MaxUploadBytes = 0
		body.ReportPromptly = flags.reportPromptly
		body.NoReport = flags.noReport

		ticket, err := SignLagTicket(body, rootPrivateKey)
		if err != nil {
			t.Fatal(err)
		}
		text, err := EncodeLagTicket(ticket)
		if err != nil {
			t.Fatal(err)
		}
		decodedTicket, err := DecodeLagTicket(text)
		if err != nil {
			t.Fatal(err)
		}
		verifiedBody, err := VerifyLagTicket(decodedTicket, rootPublicKeys, now)
		if err != nil {
			t.Fatal(err)
		}
		if verifiedBody.ReportPromptly != flags.reportPromptly || verifiedBody.NoReport != flags.noReport {
			t.Fatalf("flags %+v came back as ReportPromptly %t, NoReport %t", flags, verifiedBody.ReportPromptly, verifiedBody.NoReport)
		}
		if !proto.Equal(verifiedBody, body) {
			t.Fatalf("flags %+v: the verified body is not the signed body", flags)
		}

		// a holder can't add NoReport to a real ticket, or drop it from a demo's
		for _, flip := range []func(*protocol.LagTicketBody){
			func(b *protocol.LagTicketBody) { b.ReportPromptly = !b.ReportPromptly },
			func(b *protocol.LagTicketBody) { b.NoReport = !b.NoReport },
		} {
			flippedBody := proto.Clone(body).(*protocol.LagTicketBody)
			flip(flippedBody)
			flippedBodyBytes, err := proto.Marshal(flippedBody)
			if err != nil {
				t.Fatal(err)
			}
			flipped := &protocol.LagTicket{Body: flippedBodyBytes, RootSignature: ticket.RootSignature, RootKeyId: ticket.RootKeyId}
			if _, err := VerifyLagTicket(flipped, rootPublicKeys, now); err == nil {
				t.Fatalf("flags %+v: a ticket verified with a flag changed after signing", flags)
			}
		}
	}
}

// LagTicketBody as an extender built before the drift test's flags knows it.
func oldLagTicketBodyDescriptor(t *testing.T) protoreflect.MessageDescriptor {
	t.Helper()
	fileProto := protodesc.ToFileDescriptorProto(protocol.File_lag_proto)
	found := false
	for _, messageProto := range fileProto.MessageType {
		if messageProto.GetName() != "LagTicketBody" {
			continue
		}
		found = true
		var fields []*descriptorpb.FieldDescriptorProto
		for _, field := range messageProto.Field {
			if field.GetNumber() < 15 {
				fields = append(fields, field)
			}
		}
		messageProto.Field = fields
	}
	if !found {
		t.Fatal("LagTicketBody is missing from lag.proto")
	}
	file, err := protodesc.NewFile(fileProto, nil)
	if err != nil {
		t.Fatal(err)
	}
	return file.Messages().ByName("LagTicketBody")
}

// An older extender admits a ticket that carries the flags, and reads every
// field it knows as signed: the signature is over the body's bytes, and the
// flags decode as unknown fields it ignores. Such an extender reports a
// prompt ticket's session within 10 s, and a demo's anyway
// (lag.dev/DRIFTTEST.md E4, T1).
func TestLagTicketDriftFlagsOlderExtender(t *testing.T) {
	now := time.Now()
	rootPrivateKey, rootPublicKey := newTestLagKey(t)
	_, extenderPublicKey := newTestLagKey(t)
	oldDescriptor := oldLagTicketBodyDescriptor(t)
	if oldDescriptor.Fields().Len() != 14 {
		t.Fatalf("old descriptor has %d fields, want the 14 before the flags", oldDescriptor.Fields().Len())
	}
	oldType := dynamicpb.NewMessageType(oldDescriptor)

	body := newTestLagTicketBody(extenderPublicKey, now)
	body.ReportPromptly = true
	body.NoReport = true
	ticket, err := SignLagTicket(body, rootPrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	// the signature check comes before any decoding, so it is the same for an
	// older extender
	if _, err := VerifyLagTicket(ticket, []ed25519.PublicKey{rootPublicKey}, now); err != nil {
		t.Fatal(err)
	}
	oldBody := oldType.New()
	if err := proto.Unmarshal(ticket.Body, oldBody.Interface()); err != nil {
		t.Fatalf("an older extender cannot decode a ticket with the flags: %v", err)
	}
	if len(oldBody.GetUnknown()) == 0 {
		t.Fatal("the older extender did not carry the flags as unknown fields")
	}
	oldBody.SetUnknown(nil)

	unflaggedBody := proto.Clone(body).(*protocol.LagTicketBody)
	unflaggedBody.ReportPromptly = false
	unflaggedBody.NoReport = false
	unflaggedBodyBytes, err := proto.Marshal(unflaggedBody)
	if err != nil {
		t.Fatal(err)
	}
	expectedOldBody := oldType.New()
	if err := proto.Unmarshal(unflaggedBodyBytes, expectedOldBody.Interface()); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(oldBody.Interface(), expectedOldBody.Interface()) {
		t.Fatal("the older extender read the ticket's other fields differently")
	}
}

// A ticket is valid strictly before its expiry, and never without one.
func TestLagTicketExpiry(t *testing.T) {
	now := time.Now()
	rootPrivateKey, rootPublicKey := newTestLagKey(t)
	_, extenderPublicKey := newTestLagKey(t)
	body := newTestLagTicketBody(extenderPublicKey, now)
	ticket, err := SignLagTicket(body, rootPrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	rootPublicKeys := []ed25519.PublicKey{rootPublicKey}
	expireTime := time.UnixMilli(int64(body.ExpireTimeMs))

	if _, err := VerifyLagTicket(ticket, rootPublicKeys, expireTime.Add(-time.Millisecond)); err != nil {
		t.Fatalf("a ticket was refused a millisecond before its expiry: %v", err)
	}
	for _, at := range []time.Time{expireTime, expireTime.Add(time.Millisecond), expireTime.Add(time.Hour)} {
		if _, err := VerifyLagTicket(ticket, rootPublicKeys, at); err == nil {
			t.Fatalf("a ticket verified %v after its expiry", at.Sub(expireTime))
		}
	}

	body.ExpireTimeMs = 0
	foreverTicket, err := SignLagTicket(body, rootPrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyLagTicket(foreverTicket, rootPublicKeys, now); err == nil {
		t.Fatal("a ticket without an expiry verified")
	}
}

// A ticket verifies only under a root key, its own domain and its exact body.
func TestLagTicketRefusals(t *testing.T) {
	now := time.Now()
	rootPrivateKey, rootPublicKey := newTestLagKey(t)
	otherPrivateKey, otherPublicKey := newTestLagKey(t)
	_, extenderPublicKey := newTestLagKey(t)
	body := newTestLagTicketBody(extenderPublicKey, now)
	ticket, err := SignLagTicket(body, rootPrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	rootPublicKeys := []ed25519.PublicKey{rootPublicKey}

	if _, err := VerifyLagTicket(ticket, []ed25519.PublicKey{otherPublicKey}, now); err == nil {
		t.Fatal("a ticket verified under a key that did not sign it")
	}
	if _, err := VerifyLagTicket(ticket, nil, now); err == nil {
		t.Fatal("a ticket verified with no root keys")
	}
	if _, err := VerifyLagTicket(nil, rootPublicKeys, now); err == nil {
		t.Fatal("a missing ticket verified")
	}

	// signed by another key that claims the root's key id
	forged, err := SignLagTicket(body, otherPrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	forged.RootKeyId = ticket.RootKeyId
	if _, err := VerifyLagTicket(forged, rootPublicKeys, now); err == nil {
		t.Fatal("a ticket signed by another key verified")
	}

	mutatedBody := proto.Clone(body).(*protocol.LagTicketBody)
	mutatedBody.MaxDownloadBytes += 1
	mutatedBodyBytes, err := proto.Marshal(mutatedBody)
	if err != nil {
		t.Fatal(err)
	}
	mutated := &protocol.LagTicket{Body: mutatedBodyBytes, RootSignature: ticket.RootSignature, RootKeyId: ticket.RootKeyId}
	if _, err := VerifyLagTicket(mutated, rootPublicKeys, now); err == nil {
		t.Fatal("a mutated ticket body verified")
	}
	truncated := &protocol.LagTicket{Body: ticket.Body, RootSignature: ticket.RootSignature[:len(ticket.RootSignature)-1], RootKeyId: ticket.RootKeyId}
	if _, err := VerifyLagTicket(truncated, rootPublicKeys, now); err == nil {
		t.Fatal("a truncated ticket signature verified")
	}

	// a root signature under another domain never admits a test, and a ticket
	// signature is never a record's
	for _, domain := range []string{ExtenderRecordSignatureDomain, ExtenderRevocationSignatureDomain, LagReportSignatureDomain, ""} {
		otherDomain := &protocol.LagTicket{
			Body:          ticket.Body,
			RootSignature: ed25519.Sign(rootPrivateKey, append([]byte(domain), ticket.Body...)),
			RootKeyId:     ticket.RootKeyId,
		}
		if _, err := VerifyLagTicket(otherDomain, rootPublicKeys, now); err == nil {
			t.Fatalf("a root signature under %q verified as a ticket", domain)
		}
	}
	asRecord := &protocol.ExtenderRecord{Body: ticket.Body, RootSignature: ticket.RootSignature, RootKeyId: ticket.RootKeyId}
	if _, err := NewExtenderRootKeySet(rootPublicKey).VerifyRecord(asRecord); err == nil {
		t.Fatal("a ticket signature verified as a record")
	}

	if _, err := SignLagTicket(nil, rootPrivateKey); err == nil {
		t.Fatal("a missing ticket body was signed")
	}
	if _, err := SignLagTicket(body, rootPrivateKey[:ed25519.SeedSize]); err == nil {
		t.Fatal("a ticket was signed with a malformed key")
	}
}

// Only the unpadded base64url of a ticket parses.
func TestLagTicketDecodeRefusals(t *testing.T) {
	// fixed keys and time, so the text is the same on every run
	rootPrivateKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x5a}, ed25519.SeedSize))
	extenderPublicKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0xa5}, ed25519.SeedSize)).Public().(ed25519.PublicKey)
	ticket, err := SignLagTicket(newTestLagTicketBody(extenderPublicKey, time.UnixMilli(1760000000000)), rootPrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	// a length that padding would extend, which one of three lengths is
	var text string
	for i := 0; ; i++ {
		text, err = EncodeLagTicket(ticket)
		if err != nil {
			t.Fatal(err)
		}
		if len(text)%4 != 0 {
			break
		}
		if i == 2 {
			t.Fatalf("ticket text %q is padded", text)
		}
		ticket.RootKeyId = append(ticket.RootKeyId, 0)
	}
	if !strings.ContainsAny(text, "-_") {
		t.Fatalf("ticket text %q exercises no base64url character", text)
	}
	if _, err := DecodeLagTicket(text); err != nil {
		t.Fatal(err)
	}
	refused := []string{
		"",
		text + "=",
		text + strings.Repeat("=", (4-len(text)%4)%4),
		strings.NewReplacer("-", "+", "_", "/").Replace(text),
		" " + text,
		text[:len(text)-1] + "!",
		// valid base64url of bytes that are no protobuf
		"_____w",
	}
	for _, other := range refused {
		if other == text {
			continue
		}
		if _, err := DecodeLagTicket(other); err == nil {
			t.Fatalf("ticket text %q was decoded", other)
		}
	}
	if _, err := EncodeLagTicket(nil); err == nil {
		t.Fatal("a missing ticket was encoded")
	}
}

// A report round-trips through the extender's signature, and verifies only
// under that extender's key, its own domain, its exact body and a body that
// names the signer (M0, M8, A7).
func TestLagReportRoundTripsAndRefusals(t *testing.T) {
	now := time.Now()
	identityPrivateKey, identityPublicKey := newTestLagKey(t)
	otherPrivateKey, otherPublicKey := newTestLagKey(t)
	body := newTestLagReportBody(identityPublicKey, now)

	report, err := SignLagReport(body, identityPrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(identityPublicKey, append([]byte("ur-lag-report-v1"), report.Body...), report.Signature) {
		t.Fatal("the signature is not over the report domain and the body")
	}
	verifiedBody, err := VerifyLagReport(report, identityPublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(verifiedBody, body) {
		t.Fatal("the verified body is not the signed body")
	}
	reportBytes, err := proto.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	wireReport := &protocol.LagReport{}
	if err := proto.Unmarshal(reportBytes, wireReport); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyLagReport(wireReport, identityPublicKey); err != nil {
		t.Fatalf("a report did not survive the wire: %v", err)
	}

	if _, err := VerifyLagReport(report, otherPublicKey); err == nil {
		t.Fatal("a report verified under a key that did not sign it")
	}
	if _, err := VerifyLagReport(report, otherPublicKey[:16]); err == nil {
		t.Fatal("a report verified under a malformed key")
	}
	if _, err := VerifyLagReport(nil, identityPublicKey); err == nil {
		t.Fatal("a missing report verified")
	}

	mutatedBody := proto.Clone(body).(*protocol.LagReportBody)
	mutatedBody.UploadBytes[0] += 1
	mutatedBodyBytes, err := proto.Marshal(mutatedBody)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyLagReport(&protocol.LagReport{Body: mutatedBodyBytes, Signature: report.Signature}, identityPublicKey); err == nil {
		t.Fatal("a mutated report body verified")
	}
	if _, err := VerifyLagReport(&protocol.LagReport{Body: report.Body, Signature: report.Signature[1:]}, identityPublicKey); err == nil {
		t.Fatal("a truncated report signature verified")
	}

	for _, domain := range []string{LagTicketSignatureDomain, ExtenderRecordSignatureDomain, ExtenderChallengeSignatureDomain, ""} {
		otherDomain := &protocol.LagReport{
			Body:      report.Body,
			Signature: ed25519.Sign(identityPrivateKey, append([]byte(domain), report.Body...)),
		}
		if _, err := VerifyLagReport(otherDomain, identityPublicKey); err == nil {
			t.Fatalf("an identity signature under %q verified as a report", domain)
		}
	}

	// a well signed report that names another extender
	for _, named := range [][]byte{otherPublicKey, nil} {
		misnamedBody := proto.Clone(body).(*protocol.LagReportBody)
		misnamedBody.ExtenderPublicKey = named
		misnamed, err := SignLagReport(misnamedBody, identityPrivateKey)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := VerifyLagReport(misnamed, identityPublicKey); err == nil {
			t.Fatalf("a report naming %x verified under another extender's key", named)
		}
	}
	// and the other extender's own report of it verifies under its own key only
	otherReport, err := SignLagReport(newTestLagReportBody(otherPublicKey, now), otherPrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyLagReport(otherReport, otherPublicKey); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyLagReport(otherReport, identityPublicKey); err == nil {
		t.Fatal("another extender's report verified under this extender's key")
	}

	if _, err := SignLagReport(nil, identityPrivateKey); err == nil {
		t.Fatal("a missing report body was signed")
	}
	if _, err := SignLagReport(body, identityPrivateKey[:ed25519.SeedSize]); err == nil {
		t.Fatal("a report was signed with a malformed key")
	}
}
