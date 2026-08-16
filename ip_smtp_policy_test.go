package connect

import (
	"encoding/binary"
	"net"
	"testing"
	"time"
)

var smtpTestClientHello = []byte{
	0x16, 0x03, 0x01, 0x00, 0x40, // TLS handshake record, 64 bytes.
	0x01, 0x00, 0x00, 0x3c, // ClientHello, 60-byte body.
}

func smtpTestPath(sourcePort int, destinationPort int, sequence uint32) *IpPath {
	return &IpPath{
		Version:           4,
		Protocol:          IpProtocolTcp,
		SourceIp:          net.ParseIP("10.0.0.2"),
		SourcePort:        sourcePort,
		DestinationIp:     net.ParseIP("203.0.113.10"),
		DestinationPort:   destinationPort,
		SequenceNumber:    sequence,
		AckSequenceNumber: 9000,
		Ack:               true,
	}
}

func smtpTestSyn(sourcePort int, destinationPort int, sequence uint32) *IpPath {
	path := smtpTestPath(sourcePort, destinationPort, sequence)
	path.Ack = false
	path.Syn = true
	return path
}

func requireSmtpVerdict(t *testing.T, want smtpEgressVerdict, got smtpEgressVerdict) {
	t.Helper()
	if got != want {
		t.Fatalf("SMTP verdict = %d, want %d", got, want)
	}
}

func TestSmtpPortClassification(t *testing.T) {
	path := smtpTestPath(41000, smtpLocalPort, 1)
	if !smtpRoutesLocally(path) || !smtpNeedsOrderedSend(path) {
		t.Fatal("TCP/25 was not classified as the explicit local route")
	}
	if smtpNeedsEncryptionInspection(path) {
		t.Fatal("TCP/25 entered the encrypted SMTP inspector")
	}

	for _, port := range []int{smtpImplicitTlsPort, smtpStartTlsPort} {
		path.DestinationPort = port
		if smtpRoutesLocally(path) || !smtpNeedsEncryptionInspection(path) || !smtpNeedsOrderedSend(path) {
			t.Fatalf("TCP/%d SMTP classification is wrong", port)
		}
	}

	path.DestinationPort = 443
	if smtpRoutesLocally(path) || smtpNeedsEncryptionInspection(path) || smtpNeedsOrderedSend(path) {
		t.Fatal("HTTPS was classified as SMTP")
	}
}

func TestSmtp465RequiresFragmentedTlsClientHello(t *testing.T) {
	var guard smtpEgressGuard
	const sourcePort = 41001
	const synSequence = uint32(1000)

	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
		smtpTestSyn(sourcePort, smtpImplicitTlsPort, synSequence), nil,
	))
	first := smtpTestClientHello[:3]
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
		smtpTestPath(sourcePort, smtpImplicitTlsPort, synSequence+1), first,
	))
	// An exact retransmission is accepted without advancing the stream.
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
		smtpTestPath(sourcePort, smtpImplicitTlsPort, synSequence+1), first,
	))
	// An overlapping retransmission supplies the rest of the nine-byte prefix.
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
		smtpTestPath(sourcePort, smtpImplicitTlsPort, synSequence+2), smtpTestClientHello[1:],
	))
	// Once the prefix is verified, opaque TLS records are no longer inspected.
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
		smtpTestPath(sourcePort, smtpImplicitTlsPort, synSequence+10), []byte{0xff, 0x00, 0x7f},
	))
}

func TestSmtp465RejectsPlaintextAndLatchesFlow(t *testing.T) {
	var guard smtpEgressGuard
	const sourcePort = 41002
	const synSequence = uint32(2000)

	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
		smtpTestSyn(sourcePort, smtpImplicitTlsPort, synSequence), nil,
	))
	requireSmtpVerdict(t, smtpEgressReject, guard.inspect(
		smtpTestPath(sourcePort, smtpImplicitTlsPort, synSequence+1), []byte("EHLO plaintext.example\r\n"),
	))
	// A rejected connection cannot disguise a later segment as a new stream.
	requireSmtpVerdict(t, smtpEgressReject, guard.inspect(
		smtpTestPath(sourcePort, smtpImplicitTlsPort, synSequence+1), smtpTestClientHello,
	))
}

func TestSmtp465RejectsMalformedClientHelloPrefixes(t *testing.T) {
	tests := map[string][]byte{
		"application data record": {0x17, 0x03, 0x03, 0x00, 0x40, 0x01, 0x00, 0x00, 0x3c},
		"non TLS version":         {0x16, 0x02, 0x00, 0x00, 0x40, 0x01, 0x00, 0x00, 0x3c},
		"SSLv3 version":           {0x16, 0x03, 0x00, 0x00, 0x40, 0x01, 0x00, 0x00, 0x3c},
		"oversized record":        {0x16, 0x03, 0x03, 0x40, 0x01, 0x01, 0x00, 0x00, 0x3c},
		"server hello":            {0x16, 0x03, 0x03, 0x00, 0x40, 0x02, 0x00, 0x00, 0x3c},
		"short client hello":      {0x16, 0x03, 0x03, 0x00, 0x40, 0x01, 0x00, 0x00, 0x28},
	}
	for name, prefix := range tests {
		t.Run(name, func(t *testing.T) {
			var guard smtpEgressGuard
			requireSmtpVerdict(t, smtpEgressReject, guard.inspect(
				smtpTestPath(41005, smtpImplicitTlsPort, 3600), prefix,
			))
		})
	}
}

func TestSmtp465AcceptsLargeFirstTlsSegmentWithoutRetainingPayload(t *testing.T) {
	var guard smtpEgressGuard
	payload := append(append([]byte{}, smtpTestClientHello...), make([]byte, 4096)...)
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
		smtpTestPath(41003, smtpImplicitTlsPort, 3000), payload,
	))

	guard.stateLock.Lock()
	defer guard.stateLock.Unlock()
	for _, flow := range guard.flows {
		if !flow.secure || len(flow.stream) != 0 {
			t.Fatalf("verified 465 flow state: secure=%t retained bytes=%d", flow.secure, len(flow.stream))
		}
	}
}

func TestSmtpSecureFlowTreatsLaterSequenceSpaceAsOpaque(t *testing.T) {
	var guard smtpEgressGuard
	const sequence = uint32(3500)
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
		smtpTestPath(41004, smtpImplicitTlsPort, sequence), smtpTestClientHello,
	))

	// Exact retransmissions and opaque data after the validated prefix remain
	// valid once the flow is marked secure.
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
		smtpTestPath(41004, smtpImplicitTlsPort, sequence), smtpTestClientHello,
	))
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
		smtpTestPath(41004, smtpImplicitTlsPort, sequence+uint32(len(smtpTestClientHello))), []byte{0x17, 0x03, 0x03},
	))
}

func TestSmtpSecureFlowAllowsOpaqueDataPastHalfSequenceSpace(t *testing.T) {
	var guard smtpEgressGuard
	const sequence = uint32(3700)
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
		smtpTestPath(41006, smtpImplicitTlsPort, sequence), smtpTestClientHello,
	))

	// Once TLS is established, a forward offset at the TCP half-sequence-space
	// boundary is opaque application data rather than a segment from before the
	// negotiation prefix.
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
		smtpTestPath(41006, smtpImplicitTlsPort, sequence+(uint32(1)<<31)),
		[]byte{0x17, 0x03, 0x03},
	))
}

func TestSmtpSecureFlowAllowsOpaqueDataAfterFullSequenceWrap(t *testing.T) {
	var guard smtpEgressGuard
	const sequence = uint32(3800)
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
		smtpTestPath(41007, smtpImplicitTlsPort, sequence), smtpTestClientHello,
	))

	// The same uint32 value represents baseSequence + 2^32 after one complete
	// sequence-space wrap. It must not alias the discarded negotiation prefix.
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
		smtpTestPath(41007, smtpImplicitTlsPort, sequence),
		[]byte{0x17, 0x03, 0x03},
	))
}

func TestSmtp587AllowsNegotiationThenRequiresClientHello(t *testing.T) {
	var guard smtpEgressGuard
	const sourcePort = 42001
	const synSequence = uint32(4000)
	sequence := synSequence + 1

	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
		smtpTestSyn(sourcePort, smtpStartTlsPort, synSequence), nil,
	))
	firstEhlo := []byte("eh")
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
		smtpTestPath(sourcePort, smtpStartTlsPort, sequence), firstEhlo,
	))
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
		smtpTestPath(sourcePort, smtpStartTlsPort, sequence), firstEhlo,
	))
	sequence += uint32(len(firstEhlo))
	restEhlo := []byte("lo client.example\r\n")
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
		smtpTestPath(sourcePort, smtpStartTlsPort, sequence), restEhlo,
	))
	sequence += uint32(len(restEhlo))

	startTls := []byte("STARTTLS\r\n")
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
		smtpTestPath(sourcePort, smtpStartTlsPort, sequence), startTls,
	))
	sequence += uint32(len(startTls))

	firstTls := smtpTestClientHello[:4]
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
		smtpTestPath(sourcePort, smtpStartTlsPort, sequence), firstTls,
	))
	// Overlap two verified bytes while completing the ClientHello prefix.
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
		smtpTestPath(sourcePort, smtpStartTlsPort, sequence+2), smtpTestClientHello[2:],
	))
	sequence += uint32(len(smtpTestClientHello))

	// AUTH is opaque TLS application data after the ClientHello, not plaintext
	// SMTP, and therefore passes the now-secure connection marker.
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
		smtpTestPath(sourcePort, smtpStartTlsPort, sequence), []byte("AUTH PLAIN encrypted"),
	))
}

func TestSmtp587AcceptsStartTlsSplitAcrossTcpSegments(t *testing.T) {
	var guard smtpEgressGuard
	const sourcePort = 42002
	sequence := uint32(4500)
	for _, fragment := range [][]byte{
		[]byte("EHLO client.example\r\nSTA"),
		[]byte("RT"),
		[]byte("TLS\r"),
		[]byte("\n"),
		smtpTestClientHello[:2],
		smtpTestClientHello[2:],
	} {
		requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
			smtpTestPath(sourcePort, smtpStartTlsPort, sequence), fragment,
		))
		sequence += uint32(len(fragment))
	}
	// The split command and ClientHello must leave the flow in the secure,
	// opaque phase rather than continuing to parse TLS records as SMTP text.
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
		smtpTestPath(sourcePort, smtpStartTlsPort, sequence), []byte{0x17, 0x03, 0x03},
	))
}

func TestSmtp587RejectsTransactionCommandsBeforeStartTls(t *testing.T) {
	commands := []string{
		"AUTH PLAIN secret\r\n",
		"MAIL FROM:<sender@example.com>\r\n",
		"RCPT TO:<recipient@example.com>\r\n",
		"DATA\r\n",
		"NOOP secret\r\n",
		"VRFY user\r\n",
	}
	for index, command := range commands {
		t.Run(command[:4], func(t *testing.T) {
			var guard smtpEgressGuard
			port := 43000 + index
			requireSmtpVerdict(t, smtpEgressReject, guard.inspect(
				smtpTestPath(port, smtpStartTlsPort, 5000), []byte(command),
			))
		})
	}
}

func TestSmtp587RejectsFragmentedTransactionCommandAtFirstDisallowedPrefix(t *testing.T) {
	commands := []string{"AUTH", "MAIL", "RCPT", "DATA"}
	for index, command := range commands {
		t.Run(command, func(t *testing.T) {
			var guard smtpEgressGuard
			// None of the permitted pre-TLS negotiation commands starts with these
			// bytes, so a segmented transaction command must fail closed before a
			// later segment can carry credentials or message data.
			requireSmtpVerdict(t, smtpEgressReject, guard.inspect(
				smtpTestPath(43500+index, smtpStartTlsPort, 5500),
				[]byte(command[:1]),
			))
		})
	}
}

func TestSmtp587RejectsPlaintextAfterStartTls(t *testing.T) {
	var guard smtpEgressGuard
	negotiation := []byte("EHLO client.example\r\nSTARTTLS\r\n")
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
		smtpTestPath(44001, smtpStartTlsPort, 6000), negotiation,
	))
	requireSmtpVerdict(t, smtpEgressReject, guard.inspect(
		smtpTestPath(44001, smtpStartTlsPort, 6000+uint32(len(negotiation))), []byte("AUTH PLAIN secret\r\n"),
	))
}

func TestSmtp587BoundsNegotiationBuffer(t *testing.T) {
	var guard smtpEgressGuard
	sequence := uint32(6500)
	line := []byte("EHLO client.example\r\n")
	buffered := 0
	for buffered+len(line) <= smtpMaxNegotiationBytes {
		requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
			smtpTestPath(44002, smtpStartTlsPort, sequence), line,
		))
		sequence += uint32(len(line))
		buffered += len(line)
	}
	requireSmtpVerdict(t, smtpEgressReject, guard.inspect(
		smtpTestPath(44002, smtpStartTlsPort, sequence), line,
	))

	guard.stateLock.Lock()
	defer guard.stateLock.Unlock()
	for _, flow := range guard.flows {
		if smtpMaxNegotiationBytes < len(flow.stream) {
			t.Fatalf("587 negotiation buffer retained %d bytes, max %d", len(flow.stream), smtpMaxNegotiationBytes)
		}
	}
}

func TestSmtpGuardRejectsGapsAndConflictingRetransmissions(t *testing.T) {
	t.Run("gap", func(t *testing.T) {
		var guard smtpEgressGuard
		requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
			smtpTestPath(45001, smtpStartTlsPort, 7000), []byte("EH"),
		))
		requireSmtpVerdict(t, smtpEgressReject, guard.inspect(
			smtpTestPath(45001, smtpStartTlsPort, 7003), []byte("LO client\r\n"),
		))
	})

	t.Run("conflicting overlap", func(t *testing.T) {
		var guard smtpEgressGuard
		requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
			smtpTestPath(45002, smtpStartTlsPort, 8000), []byte("EH"),
		))
		requireSmtpVerdict(t, smtpEgressReject, guard.inspect(
			smtpTestPath(45002, smtpStartTlsPort, 8000), []byte("EX"),
		))
	})
}

func TestSmtpGuardFreshSynReplacesTupleState(t *testing.T) {
	var guard smtpEgressGuard
	const sourcePort = 46001
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
		smtpTestPath(sourcePort, smtpImplicitTlsPort, 9000), smtpTestClientHello,
	))
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
		smtpTestSyn(sourcePort, smtpImplicitTlsPort, 10000), nil,
	))
	requireSmtpVerdict(t, smtpEgressReject, guard.inspect(
		smtpTestPath(sourcePort, smtpImplicitTlsPort, 10001), []byte("plaintext"),
	))
}

func TestSmtpGuardRstClearsTupleState(t *testing.T) {
	var guard smtpEgressGuard
	const sourcePort = 46003
	const sequence = uint32(10500)

	requireSmtpVerdict(t, smtpEgressReject, guard.inspect(
		smtpTestPath(sourcePort, smtpImplicitTlsPort, sequence), []byte("plaintext"),
	))
	rstPath := smtpTestPath(sourcePort, smtpImplicitTlsPort, sequence)
	rstPath.Rst = true
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(rstPath, nil))
	// Reusing the tuple after teardown must start from empty state rather than
	// inherit the prior connection's latched rejection.
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
		smtpTestPath(sourcePort, smtpImplicitTlsPort, sequence), smtpTestClientHello,
	))
}

func TestSmtpGuardFinClearsTupleState(t *testing.T) {
	var guard smtpEgressGuard
	const sequence = uint32(10700)

	var path IpPath
	payload, err := parseIpPathWithPayloadBorrowed(
		smtpTestTcp4Packet(byte(tcpFlagAck|tcpFlagPsh), sequence, 9000, smtpTestClientHello),
		&path,
	)
	if err != nil {
		t.Fatal(err)
	}
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(&path, payload))

	payload, err = parseIpPathWithPayloadBorrowed(
		smtpTestTcp4Packet(
			byte(tcpFlagAck|tcpFlagFin),
			sequence+uint32(len(smtpTestClientHello)),
			9000,
			nil,
		),
		&path,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !path.Fin {
		t.Fatal("TCP FIN was not exposed on the SMTP inspection path")
	}
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(&path, payload))

	guard.stateLock.Lock()
	defer guard.stateLock.Unlock()
	if len(guard.flows) != 0 {
		t.Fatalf("SMTP flow table retained %d entries after FIN, want 0", len(guard.flows))
	}
}

func TestSmtpGuardNamespacesProviderFlowsBySource(t *testing.T) {
	var guard smtpEgressGuard
	firstSource := NewId()
	secondSource := NewId()
	path := smtpTestPath(46002, smtpImplicitTlsPort, 11000)

	// The first remote client poisons only its own exact tuple.
	requireSmtpVerdict(t, smtpEgressReject, guard.inspectForOwner(
		firstSource,
		path,
		[]byte("plaintext"),
	))
	// A different authenticated client may legitimately reuse the same tunnel
	// address, ports, and sequence number without inheriting that rejection.
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspectForOwner(
		secondSource,
		path,
		smtpTestClientHello,
	))
}

func TestSmtpGuardCapsProviderSynEntriesPerOwner(t *testing.T) {
	var guard smtpEgressGuard
	ownerId := NewId()
	for index := 0; index < smtpMaxOwnerFlowCount+1; index++ {
		requireSmtpVerdict(t, smtpEgressAllow, guard.inspectForOwner(
			ownerId,
			smtpTestSyn(18000+index, smtpImplicitTlsPort, uint32(index+1)),
			nil,
		))
	}

	guard.stateLock.Lock()
	defer guard.stateLock.Unlock()
	ownerFlowCount := 0
	for key := range guard.flows {
		if key.ownerId == ownerId {
			ownerFlowCount += 1
		}
	}
	if ownerFlowCount != smtpMaxOwnerFlowCount {
		t.Fatalf("provider owner retained %d SYN entries, want %d", ownerFlowCount, smtpMaxOwnerFlowCount)
	}
}

func TestSmtpGuardOwnerCapContainsFakeTlsFlood(t *testing.T) {
	var guard smtpEgressGuard
	protectedOwnerId := NewId()
	noisyOwnerId := NewId()
	protectedPath := smtpTestPath(19000, smtpImplicitTlsPort, 1000)
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspectForOwner(
		protectedOwnerId,
		protectedPath,
		smtpTestClientHello,
	))
	protectedKey, ok := smtpFlowKeyForOwnerPath(protectedOwnerId, protectedPath)
	if !ok {
		t.Fatal("could not build protected SMTP flow key")
	}

	// Reproduce the reported 2,048-packet fill: SYN followed by the minimal
	// nine-byte prefix that makes an entry look like established TLS. Without
	// an owner cap, the protected flow is the oldest secure eviction candidate.
	for index := 0; index < smtpMaxFlowCount; index++ {
		sourcePort := 20000 + index
		sequence := uint32(20000 + index*16)
		requireSmtpVerdict(t, smtpEgressAllow, guard.inspectForOwner(
			noisyOwnerId,
			smtpTestSyn(sourcePort, smtpImplicitTlsPort, sequence),
			nil,
		))
		requireSmtpVerdict(t, smtpEgressAllow, guard.inspectForOwner(
			noisyOwnerId,
			smtpTestPath(sourcePort, smtpImplicitTlsPort, sequence+1),
			smtpTestClientHello[:smtpTlsClientHelloPrefixBytes],
		))
	}

	guard.stateLock.Lock()
	defer guard.stateLock.Unlock()
	protectedFlow, ok := guard.flows[protectedKey]
	if !ok || !protectedFlow.secure {
		t.Fatal("one provider owner evicted another owner's established TLS flow")
	}
	noisyFlowCount := 0
	for key := range guard.flows {
		if key.ownerId == noisyOwnerId {
			noisyFlowCount += 1
		}
	}
	if noisyFlowCount != smtpMaxOwnerFlowCount {
		t.Fatalf("noisy owner retained %d fake-TLS entries, want %d", noisyFlowCount, smtpMaxOwnerFlowCount)
	}
	if len(guard.flows) != smtpMaxOwnerFlowCount+1 {
		t.Fatalf("provider table retained %d flows, want %d", len(guard.flows), smtpMaxOwnerFlowCount+1)
	}
}

func TestSmtpGuardIdleTimeoutExpiresSecureTuple(t *testing.T) {
	now := time.Unix(1700000000, 0)
	guard := smtpEgressGuard{
		timeNow: func() time.Time {
			return now
		},
	}
	ownerId := NewId()
	path := smtpTestPath(22000, smtpImplicitTlsPort, 30000)
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspectForOwner(
		ownerId,
		path,
		smtpTestClientHello,
	))

	now = now.Add(smtpFlowIdleTimeout)
	// The identical tuple must no longer inherit the old secure marker. With
	// no fresh ClientHello, its first post-timeout payload fails closed.
	requireSmtpVerdict(t, smtpEgressReject, guard.inspectForOwner(
		ownerId,
		path,
		[]byte("plaintext after idle timeout"),
	))
}

func TestSmtpGuardIdleReaperKeepsRecentlyActiveFlow(t *testing.T) {
	now := time.Unix(1700000000, 0)
	guard := smtpEgressGuard{
		timeNow: func() time.Time {
			return now
		},
	}
	ownerId := NewId()
	stalePath := smtpTestSyn(22001, smtpImplicitTlsPort, 31000)
	activePath := smtpTestSyn(22002, smtpImplicitTlsPort, 32000)
	triggerPath := smtpTestSyn(22003, smtpImplicitTlsPort, 33000)
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspectForOwner(ownerId, stalePath, nil))

	now = now.Add(smtpFlowIdleTimeout / 2)
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspectForOwner(ownerId, activePath, nil))

	now = now.Add(smtpFlowIdleTimeout / 2)
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspectForOwner(ownerId, triggerPath, nil))

	staleKey, staleOk := smtpFlowKeyForOwnerPath(ownerId, stalePath)
	activeKey, activeOk := smtpFlowKeyForOwnerPath(ownerId, activePath)
	triggerKey, triggerOk := smtpFlowKeyForOwnerPath(ownerId, triggerPath)
	if !staleOk || !activeOk || !triggerOk {
		t.Fatal("could not build SMTP idle-reaper flow keys")
	}
	guard.stateLock.Lock()
	defer guard.stateLock.Unlock()
	if _, ok := guard.flows[staleKey]; ok {
		t.Fatal("idle SMTP flow survived its deterministic timeout")
	}
	if _, ok := guard.flows[activeKey]; !ok {
		t.Fatal("recently active SMTP flow was reaped")
	}
	if _, ok := guard.flows[triggerKey]; !ok {
		t.Fatal("flow that triggered SMTP idle reaping was not retained")
	}
}

func TestSmtpGuardBoundsFlowTable(t *testing.T) {
	var guard smtpEgressGuard
	for index := 0; index < smtpMaxFlowCount+1; index++ {
		requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
			smtpTestSyn(10000+index, smtpImplicitTlsPort, uint32(index+1)), nil,
		))
	}
	guard.stateLock.Lock()
	defer guard.stateLock.Unlock()
	if len(guard.flows) != smtpMaxFlowCount {
		t.Fatalf("SMTP flow table size = %d, want %d", len(guard.flows), smtpMaxFlowCount)
	}
}

func TestSmtpGuardEvictsNegotiatingFlowBeforeSecureFlow(t *testing.T) {
	guard := smtpEgressGuard{
		flows: make(map[smtpFlowKey]*smtpFlowState, smtpMaxFlowCount),
	}
	for index := 0; index < smtpMaxFlowCount-1; index++ {
		key := smtpFlowKey{sourcePort: uint16(index + 1)}
		guard.flows[key] = &smtpFlowState{secure: true, lastUsed: 1}
	}
	negotiatingKey := smtpFlowKey{sourcePort: uint16(smtpMaxFlowCount)}
	guard.flows[negotiatingKey] = &smtpFlowState{lastUsed: 2}
	newKey := smtpFlowKey{sourcePort: uint16(smtpMaxFlowCount + 1)}

	guard.stateLock.Lock()
	guard.newFlowWithLock(newKey, smtpImplicitTlsPort, time.Unix(1, 0))
	guard.stateLock.Unlock()

	if _, ok := guard.flows[negotiatingKey]; ok {
		t.Fatal("negotiating SMTP flow survived eviction ahead of established TLS flows")
	}
	if _, ok := guard.flows[newKey]; !ok {
		t.Fatal("new SMTP flow was not added after eviction")
	}
	if len(guard.flows) != smtpMaxFlowCount {
		t.Fatalf("SMTP flow table size = %d, want %d", len(guard.flows), smtpMaxFlowCount)
	}
}

func TestSmtpGuardTupleReplacementAtCapacityDoesNotEvictAnotherFlow(t *testing.T) {
	var guard smtpEgressGuard
	const firstSourcePort = 12000
	for index := 0; index < smtpMaxFlowCount; index++ {
		requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
			smtpTestPath(firstSourcePort+index, smtpImplicitTlsPort, uint32(index+1)),
			smtpTestClientHello,
		))
	}

	firstPath := smtpTestPath(firstSourcePort, smtpImplicitTlsPort, 1)
	firstKey, ok := smtpFlowKeyForOwnerPath(Id{}, firstPath)
	if !ok {
		t.Fatal("could not build the first SMTP flow key")
	}
	lastSourcePort := firstSourcePort + smtpMaxFlowCount - 1
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
		smtpTestSyn(lastSourcePort, smtpImplicitTlsPort, 50000),
		nil,
	))

	guard.stateLock.Lock()
	defer guard.stateLock.Unlock()
	if len(guard.flows) != smtpMaxFlowCount {
		t.Fatalf("SMTP tuple replacement left %d flows, want %d", len(guard.flows), smtpMaxFlowCount)
	}
	if _, ok := guard.flows[firstKey]; !ok {
		t.Fatal("SMTP tuple replacement evicted an unrelated established flow")
	}
}

func smtpTestTcp4Packet(flags byte, sequence uint32, ack uint32, payload []byte) []byte {
	return smtpTestTcp4PacketToPort(smtpImplicitTlsPort, flags, sequence, ack, payload)
}

func smtpTestTcp4PacketToPort(destinationPort int, flags byte, sequence uint32, ack uint32, payload []byte) []byte {
	sourceIp := net.IPv4(10, 0, 0, 2).To4()
	destinationIp := net.IPv4(203, 0, 113, 10).To4()
	packet := make([]byte, Ipv4HeaderSizeWithoutExtensions+TcpHeaderSizeWithoutExtensions+len(payload))
	writeIpv4Header(packet, ipProtocolNumberTcp, sourceIp, destinationIp)
	tcp := packet[Ipv4HeaderSizeWithoutExtensions:]
	binary.BigEndian.PutUint16(tcp[0:2], 47001)
	binary.BigEndian.PutUint16(tcp[2:4], uint16(destinationPort))
	binary.BigEndian.PutUint32(tcp[4:8], sequence)
	binary.BigEndian.PutUint32(tcp[8:12], ack)
	tcp[12] = byte(TcpHeaderSizeWithoutExtensions/4) << 4
	tcp[13] = flags
	binary.BigEndian.PutUint16(tcp[14:16], 65535)
	copy(tcp[TcpHeaderSizeWithoutExtensions:], payload)
	binary.BigEndian.PutUint16(tcp[16:18], transportChecksum(
		ipProtocolNumberTcp,
		sourceIp,
		destinationIp,
		tcp,
	))
	return packet
}

func TestTcpRstForSmtpPolicyReject(t *testing.T) {
	packet := smtpTestTcp4Packet(byte(tcpFlagAck|tcpFlagPsh), 12000, 34000, []byte("plaintext"))
	reset := tcpRstForPolicyReject(packet)
	if reset == nil {
		t.Fatal("SMTP policy rejection did not build a TCP reset")
	}
	defer MessagePoolReturn(reset)

	ipProtocol, sourceIp, destinationIp, transport, ok := parseIpv4(reset)
	if !ok || ipProtocol != ipProtocolNumberTcp {
		t.Fatal("SMTP policy reset is not valid IPv4/TCP")
	}
	var tcp parsedTcp
	if !parseTcpPacket(sourceIp, destinationIp, transport, &tcp) {
		t.Fatal("could not parse SMTP policy reset")
	}
	if !tcp.rst || tcp.ack || tcp.seq != 34000 {
		t.Fatalf("reset flags/sequence = rst:%t ack:%t seq:%d", tcp.rst, tcp.ack, tcp.seq)
	}
	if !sourceIp.Equal(net.IPv4(203, 0, 113, 10)) || !destinationIp.Equal(net.IPv4(10, 0, 0, 2)) {
		t.Fatalf("reset addresses were not reversed: %s -> %s", sourceIp, destinationIp)
	}
	if tcp.sourcePort != smtpImplicitTlsPort || tcp.destinationPort != 47001 {
		t.Fatalf("reset ports = %d -> %d", tcp.sourcePort, tcp.destinationPort)
	}
	if checksum := transportChecksum(ipProtocolNumberTcp, sourceIp, destinationIp, transport); checksum != 0 {
		t.Fatalf("reset TCP checksum verification = %#x, want 0", checksum)
	}

	rstPacket := smtpTestTcp4Packet(byte(tcpFlagRst), 1, 0, nil)
	if secondReset := tcpRstForPolicyReject(rstPacket); secondReset != nil {
		MessagePoolReturn(secondReset)
		t.Fatal("policy reset builder answered a reset with another reset")
	}
}
