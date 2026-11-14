// Real owned decoding checks the preflight allocation envelope and legacy
// extraction without adding a protocol subpackage import or a private API.
package connect

import (
	"bytes"
	"math"
	"testing"
)

// Borrow bodies while encoding one complete wire carrier; return ordinary Go
// scratch bytes. Only the caller's pooled carrier and actual decoder need return.
func receiveMemoryScanTestWire(path TransferPath, body []byte, legacy bool) []byte {
	wire := protoAppendTag(nil, 1, protoWireBytes)
	wire = protoAppendVarint(wire, uint64(sizeTransferPath(path)))
	wire = appendTransferPath(wire, path)
	if legacy {
		wrapper := protoAppendTag(nil, 2, protoWireBytes)
		wrapper = protoAppendVarint(wrapper, uint64(len(body)))
		wrapper = append(wrapper, body...)
		wire = protoAppendTag(wire, 2, protoWireBytes)
		wire = protoAppendVarint(wire, uint64(len(wrapper)))
		wire = append(wire, wrapper...)
	} else {
		wire = protoAppendTag(wire, 4, protoWireBytes)
		wire = protoAppendVarint(wire, uint64(len(body)))
		wire = append(wire, body...)
	}
	wire = protoAppendTag(wire, 7, protoWireVarint)
	wire = protoAppendVarint(wire, 2)
	wire = protoAppendTag(wire, 8, protoWireVarint)
	wire = protoAppendVarint(wire, 0)
	return wire
}

// Actual raw FromProvider frame geometry; small counts use full-MTU packets,
// while long valid Pack vectors keep TCP/IP headers and exceed16 frames.
func receiveMemoryScanTestBody(t *testing.T, count, size int) ([]byte, Id, Id) {
	t.Helper()
	messageId, sequenceId := NewId(), NewId()
	frames := (&SendPack{}).Frames
	for range count {
		packet := ipOosTcpPacketSequence(reliableDeliveryTestPath(4), tcpFlagAck, 7, bytes.Repeat([]byte{0x42}, size-Ipv4HeaderSizeWithoutExtensions-TcpHeaderSizeWithoutExtensions))
		frame, err := ipPacketFromProviderFrame(packet, 2)
		if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, frame)
	}
	pack := sendPackFrame{messageId: messageId, sequenceId: sequenceId, sequenceNumber: 9, head: true, frames: frames, logicalLane: 3, forceStream: true, companionContract: true}
	return pack.appendPack(nil), messageId, sequenceId
}

// The selected legacy body uses ProtoUnmarshal into an external Pack while
// bypassing the deprecated carrier wrapper; direct v2 uses its ordinary owner.
func receiveMemoryScanTestOwnedCost(t *testing.T, carrier []byte, scan receiveMemoryPackScan) ByteCount {
	t.Helper()
	var owner *decodedPackOwner
	if scan.legacy {
		owner = inboundDecodedPackOwners.take()
		external, ok := decodePack(nil)
		if !ok {
			owner.release()
			t.Fatal("empty protocol Pack constructor failed")
		}
		if err := ProtoUnmarshal(carrier[scan.packOffset:scan.packOffset+scan.packByteCount], external); err != nil {
			owner.release()
			t.Fatal(err)
		}
		pack := &owner.receivePack
		*pack = ReceivePack{Pack: external, decodedOwner: owner, TransferFrameBytes: MessagePoolShareReadOnly(carrier), MessageByteCount: MessageByteCount(external.Frames)}
	} else {
		var ok bool
		owner, ok = decodePackOwned(carrier[scan.packOffset : scan.packOffset+scan.packByteCount])
		if !ok {
			t.Fatal("preflight selected no actual owned Pack")
		}
		pack := &owner.receivePack
		*pack = ReceivePack{Pack: &owner.pack, decodedOwner: owner, TransferFrameBytes: MessagePoolShareReadOnly(carrier), MessageByteCount: MessageByteCount(owner.pack.Frames)}
	}
	cost := owner.receivePack.receiveQueueByteCount()
	owner.receivePack.messagePoolReturn()
	return cost
}

// Canonical and deprecated valid wrappers keep their own funded payload models.
func TestReceiveMemoryScanCanonicalAndLegacyOwnedBounds(t *testing.T) {
	assertMessagePoolOwnership(t)
	path := TransferPath{SourceId: NewId(), DestinationId: NewId(), StreamId: NewId()}
	for _, legacy := range []bool{false, true} {
		for _, count := range []int{0, 1, 2, 3, 8, 9, 17, 257} {
			size := 1280
			if count > 8 {
				size = 40
			}
			body, messageId, sequenceId := receiveMemoryScanTestBody(t, count, size)
			func() {
				carrier := MessagePoolCopy(receiveMemoryScanTestWire(path, body, legacy))
				defer MessagePoolReturn(carrier)
				scan, status := scanReceiveMemoryPack(carrier, path)
				if status != receiveMemoryPackScanReady || !scan.loanSafe || scan.path != path || scan.messageId != messageId || scan.sequenceId != sequenceId || scan.sequenceNumber != 9 || scan.logicalLane != 3 ||
					!scan.head || !scan.forceStream || !scan.companionContract || scan.nack || scan.legacy != legacy || scan.frameCount != count || scan.messageByteCount != ByteCount(count*size) ||
					!scan.sessionRoleSet || scan.sessionRole != 2 || !scan.sessionCompanionSet || scan.sessionCompanion {
					t.Fatalf("actual scan identity/geometry changed: legacy=%t count=%d scan=%+v status=%d", legacy, count, scan, status)
				}
				cost := receiveMemoryScanTestOwnedCost(t, carrier, scan)
				if cost > scan.retainedByteCount {
					t.Fatalf("predecode bound underprices actual owned storage: count=%d actual=%d bound=%d", count, cost, scan.retainedByteCount)
				}
				if count == 8 && !legacy && (cost != 30544 || scan.retainedByteCount != 30544 || scan.metadataByteCount != 1872) {
					t.Fatalf("ordinary8-frame envelope changed: actual=%d scan=%+v", cost, scan)
				}
				if allocated := testing.AllocsPerRun(100, func() {
					result, status := scanReceiveMemoryPack(carrier, path)
					if status != receiveMemoryPackScanReady || result.messageId != messageId {
						t.Fatal("scan changed without allocation")
					}
				}); allocated != 0 {
					t.Fatalf("preflight allocated %v objects", allocated)
				}
			}()
		}
	}
}

// Whole-body scanning cannot claim first-seen identities before later Frames
// or scalar fields have been validated against the actual decoder semantics.
func TestReceiveMemoryScanReorderedAndRepeatedFields(t *testing.T) {
	assertMessagePoolOwnership(t)
	path := TransferPath{SourceId: NewId(), DestinationId: NewId()}
	body, messageId, sequenceId := receiveMemoryScanTestBody(t, 8, 1280)
	canonical := receiveMemoryScanTestWire(path, body, false)
	scan, status := scanReceiveMemoryPack(canonical, path)
	if status != receiveMemoryPackScanReady {
		t.Fatal("base scan failed")
	}
	ids := appendIdField(nil, 1, messageId)
	ids = appendIdField(ids, 2, sequenceId)
	reordered := append([]byte{}, body[len(ids):]...)
	reordered = append(reordered, ids...)
	result, status := scanReceiveMemoryPack(receiveMemoryScanTestWire(path, reordered, false), path)
	if status != receiveMemoryPackScanReady || result.messageId != messageId || result.sequenceId != sequenceId {
		t.Fatal("valid IDs after Frames were rejected or misidentified")
	}
	other := NewId()
	duplicateId := appendIdField(append([]byte{}, body...), 1, other)
	duplicateSeq := protoAppendTag(append([]byte{}, body...), 3, protoWireVarint)
	duplicateSeq = protoAppendVarint(duplicateSeq, 123)
	duplicateLane := protoAppendTag(append([]byte{}, body...), 12, protoWireVarint)
	duplicateLane = protoAppendVarint(duplicateLane, 7)
	frame := protoAppendTag(nil, 2, protoWireBytes)
	frame = protoAppendVarint(frame, 1)
	frame = append(frame, 1)
	repeatedBytes := protoAppendTag(append([]byte{}, frame...), 2, protoWireBytes)
	repeatedBytes = protoAppendVarint(repeatedBytes, 1)
	repeatedBytes = append(repeatedBytes, 2)
	repeatedFrame := protoAppendTag(append([]byte{}, ids...), 5, protoWireBytes)
	repeatedFrame = protoAppendVarint(repeatedFrame, uint64(len(repeatedBytes)))
	repeatedFrame = append(repeatedFrame, repeatedBytes...)
	contract := protoAppendTag(append([]byte{}, body...), 7, protoWireBytes)
	contract = protoAppendVarint(contract, uint64(len(frame)))
	contract = append(contract, frame...)
	repeatedContract := protoAppendTag(append([]byte{}, contract...), 7, protoWireBytes)
	repeatedContract = protoAppendVarint(repeatedContract, uint64(len(frame)))
	repeatedContract = append(repeatedContract, frame...)
	duplicateOuter := protoAppendTag(append([]byte{}, canonical...), 4, protoWireBytes)
	duplicateOuter = protoAppendVarint(duplicateOuter, uint64(len(ids)))
	duplicateOuter = append(duplicateOuter, ids...)
	mixedAck := protoAppendTag(append([]byte{}, canonical...), 5, protoWireBytes)
	mixedAck = protoAppendVarint(mixedAck, 0)
	secondPath := protoAppendTag(append([]byte{}, canonical...), 1, protoWireBytes)
	secondPath = protoAppendVarint(secondPath, uint64(sizeTransferPath(path)))
	secondPath = appendTransferPath(secondPath, path)
	for _, wire := range [][]byte{
		receiveMemoryScanTestWire(path, duplicateId, false), receiveMemoryScanTestWire(path, duplicateSeq, false), receiveMemoryScanTestWire(path, duplicateLane, false),
		receiveMemoryScanTestWire(path, repeatedFrame, false), receiveMemoryScanTestWire(path, repeatedContract, false),
	} {
		result, status := scanReceiveMemoryPack(wire, path)
		if status != receiveMemoryPackScanReady || result.loanSafe {
			t.Fatal("valid repeated fields were rejected or borrowed predecessor credit")
		}
		owner, ok := decodePackOwned(wire[result.packOffset : result.packOffset+result.packByteCount])
		if !ok {
			t.Fatal("valid repeated-field preflight did not match owned decode")
		}
		decodedId, err := IdFromBytes(owner.pack.MessageId)
		if err != nil || decodedId != result.messageId || owner.pack.SequenceNumber != result.sequenceNumber || owner.pack.LogicalLane != result.logicalLane {
			owner.release()
			t.Fatal("preflight used first identity/scalar rather than effective final value")
		}
		owner.release()
	}
	if result, status := scanReceiveMemoryPack(receiveMemoryScanTestWire(path, duplicateId, false), path); status != receiveMemoryPackScanReady || result.messageId != other {
		t.Fatal("last Pack identity was not selected")
	}
	for _, wire := range [][]byte{duplicateOuter, append(append([]byte{}, duplicateOuter[len(canonical):]...), canonical...), secondPath} {
		result, status := scanReceiveMemoryPack(wire, path)
		if status != receiveMemoryPackScanCompatibility || result.loanSafe || !result.compatibility {
			t.Fatal("valid repeated outer was rejected or obtained predecessor credit")
		}
	}
	if _, status := scanReceiveMemoryPack(mixedAck, path); status != receiveMemoryPackScanInvalid {
		t.Fatal("effective Pack plus ACK was accepted")
	}
	wrong := path
	wrong.SourceId = NewId()
	if _, status := scanReceiveMemoryPack(canonical, wrong); status != receiveMemoryPackScanInvalid {
		t.Fatal("wrong source path obtained a memory loan")
	}
	for end := 0; end < scan.packOffset+scan.packByteCount; end++ {
		if _, status := scanReceiveMemoryPack(canonical[:end], path); status == receiveMemoryPackScanReady {
			t.Fatal("truncated carrier obtained a ready loan")
		}
	}
	if _, ok := receiveMemoryScanAdd(math.MaxInt64, 1); ok {
		t.Fatal("overflow bound wrapped")
	}
	if _, ok := receiveMemoryScanAdd(1, -1); ok {
		t.Fatal("negative bound accepted")
	}
	if _, ok := receiveMemoryScanMetadata(-1); ok {
		t.Fatal("negative frame metadata accepted")
	}
}

// A unique encrypted outer must reach the existing canonical unwrap. It never
// obtains a Pack loan; a plaintext inner is scanned again after authentication.
func TestReceiveMemoryScanEncryptedAndAckBypass(t *testing.T) {
	path := TransferPath{SourceId: NewId(), DestinationId: NewId()}
	for _, kind := range []int{5, 6} {
		wire := protoAppendTag(nil, 1, protoWireBytes)
		wire = protoAppendVarint(wire, uint64(sizeTransferPath(path)))
		wire = appendTransferPath(wire, path)
		body := []byte{1, 2, 3}
		if kind == 5 {
			body = appendIdField(nil, 1, NewId())
			body = appendIdField(body, 2, NewId())
		}
		wire = protoAppendTag(wire, kind, protoWireBytes)
		wire = protoAppendVarint(wire, uint64(len(body)))
		wire = append(wire, body...)
		scan, status := scanReceiveMemoryPack(wire, path)
		if status != receiveMemoryPackScanNotPack || scan.encrypted != (kind == 6) || scan.ack != (kind == 5) || scan.retainedByteCount != 0 {
			t.Fatal("non-Pack outer incorrectly obtained/rejected a loan")
		}
		mixed := protoAppendTag(append([]byte{}, wire...), 4, protoWireBytes)
		mixed = protoAppendVarint(mixed, 0)
		if result, status := scanReceiveMemoryPack(mixed, path); kind == 6 {
			if status != receiveMemoryPackScanNotPack || !result.encrypted || !result.compatibility {
				t.Fatal("nonempty encrypted outer lost precedence")
			}
		} else if status != receiveMemoryPackScanInvalid {
			t.Fatal("effective Pack plus ACK was accepted")
		}
	}
}

// Old v1 payload wrappers use the same owned Frame decoder, rather than a
// protobuf-GC Pack fallback. Both the nested bytes and the complete root count.
func TestReceiveMemoryScanLegacyPayloadAndCarrierClass(t *testing.T) {
	assertMessagePoolOwnership(t)
	path := TransferPath{SourceId: NewId(), DestinationId: NewId()}
	frames := (&SendPack{}).Frames
	for range 8 {
		packet := ipOosTcpPacketSequence(reliableDeliveryTestPath(4), tcpFlagAck, 1, bytes.Repeat([]byte{0x42}, 1240))
		frame, err := ipPacketFromProviderFrame(packet, 1)
		if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, frame)
	}
	expected := MessageByteCount(frames)
	pack := sendPackFrame{messageId: NewId(), sequenceId: NewId(), frames: frames}
	body := pack.appendPack(nil)
	for _, frame := range frames {
		returnDecodedFrameMessageBytes(frame)
	}
	carrier := MessagePoolCopy(receiveMemoryScanTestWire(path, body, true))
	scan, status := scanReceiveMemoryPack(carrier, path)
	if status != receiveMemoryPackScanReady || scan.messageByteCount != expected || scan.frameCount != 8 || !scan.legacy {
		MessagePoolReturn(carrier)
		t.Fatal("legacy payload wrappers were mispriced or rejected")
	}
	cost := receiveMemoryScanTestOwnedCost(t, carrier, scan)
	if scan.retainedByteCount < cost || scan.metadataByteCount <= 1024 {
		MessagePoolReturn(carrier)
		t.Fatal("legacy external protocol graph escaped its envelope")
	}
	MessagePoolReturn(carrier)
	body, _, _ = receiveMemoryScanTestBody(t, 1, 40)
	wire := receiveMemoryScanTestWire(path, body, false)
	large := MessagePoolGet(9000)
	large = large[:len(wire)]
	copy(large, wire)
	defer MessagePoolReturn(large)
	scan, status = scanReceiveMemoryPack(large, path)
	if status != receiveMemoryPackScanReady || scan.retainedByteCount < MessagePoolRootByteCount(large)+256+1024 || receiveMemoryScanTestOwnedCost(t, large, scan) > scan.retainedByteCount {
		t.Fatal("actual larger retained carrier root escaped preflight pricing")
	}
}

// A real retained contract/proof root is additional storage, not just the
// frame payload sum or inline contract Frame metadata.
func TestReceiveMemoryScanContractRootBound(t *testing.T) {
	assertMessagePoolOwnership(t)
	path := TransferPath{SourceId: NewId(), DestinationId: NewId()}
	body, _, _ := receiveMemoryScanTestBody(t, 8, 1280)
	proof := protoAppendTag(nil, 2, protoWireBytes)
	proof = protoAppendVarint(proof, 7000)
	proof = append(proof, bytes.Repeat([]byte{0x24}, 7000)...)
	body = protoAppendTag(body, 7, protoWireBytes)
	body = protoAppendVarint(body, uint64(len(proof)))
	body = append(body, proof...)
	for _, legacy := range []bool{false, true} {
		carrier := MessagePoolCopy(receiveMemoryScanTestWire(path, body, legacy))
		scan, status := scanReceiveMemoryPack(carrier, path)
		if status != receiveMemoryPackScanReady || scan.frameCount != 8 || scan.payloadRootByteCount != 8*2048+8192 || receiveMemoryScanTestOwnedCost(t, carrier, scan) > scan.retainedByteCount {
			MessagePoolReturn(carrier)
			t.Fatal("contract root escaped predecode envelope")
		}
		MessagePoolReturn(carrier)
	}
}

// Replaced data/proof roots have a funded peak bound even when final bytes
// are small. Fresh funding is mandatory; duplicate semantics remain last-wins.
func TestReceiveMemoryScanRepeatedPayloadAndProofPeak(t *testing.T) {
	assertMessagePoolOwnership(t)
	path := TransferPath{SourceId: NewId(), DestinationId: NewId()}
	ids := appendIdField(nil, 1, NewId())
	ids = appendIdField(ids, 2, NewId())
	frame := protoAppendTag(nil, 2, protoWireBytes)
	frame = protoAppendVarint(frame, 6000)
	frame = append(frame, bytes.Repeat([]byte{1}, 6000)...)
	frame = protoAppendTag(frame, 2, protoWireBytes)
	frame = protoAppendVarint(frame, 1)
	frame = append(frame, 2)
	for _, field := range []int{5, 7} {
		body := protoAppendTag(append([]byte{}, ids...), field, protoWireBytes)
		body = protoAppendVarint(body, uint64(len(frame)))
		body = append(body, frame...)
		carrier := MessagePoolCopy(receiveMemoryScanTestWire(path, body, false))
		scan, status := scanReceiveMemoryPack(carrier, path)
		if status != receiveMemoryPackScanReady || scan.loanSafe || scan.payloadRootByteCount < 8192+256 || receiveMemoryScanTestOwnedCost(t, carrier, scan) > scan.retainedByteCount {
			MessagePoolReturn(carrier)
			t.Fatal("replaced payload root peak escaped fresh-funding bound")
		}
		MessagePoolReturn(carrier)
	}
}

// Final legacy body selection preserves generated protobuf field merging and
// final-ID validation, while earlier direct bodies are still checked first.
func TestReceiveMemoryScanOuterPrecedenceAndLegacyFinalIds(t *testing.T) {
	assertMessagePoolOwnership(t)
	path := TransferPath{SourceId: NewId(), DestinationId: NewId()}
	direct, _, _ := receiveMemoryScanTestBody(t, 1, 40)
	legacy, messageId, _ := receiveMemoryScanTestBody(t, 3, 40)
	wrapper := protoAppendTag(nil, 2, protoWireBytes)
	wrapper = protoAppendVarint(wrapper, uint64(len(legacy)))
	wrapper = append(wrapper, legacy...)
	wire := receiveMemoryScanTestWire(path, direct, false)
	wire = protoAppendTag(wire, 2, protoWireBytes)
	wire = protoAppendVarint(wire, uint64(len(wrapper)))
	wire = append(wire, wrapper...)
	result, status := scanReceiveMemoryPack(wire, path)
	if status != receiveMemoryPackScanCompatibility || !result.legacy || result.messageId != messageId || result.frameCount != 3 || result.loanSafe {
		t.Fatal("last legacy Pack did not override direct Pack category")
	}
	if receiveMemoryScanTestOwnedCost(t, wire, result) > result.retainedByteCount {
		t.Fatal("ignored-body compatibility funding underpriced selected legacy")
	}
	bad := protoAppendTag(nil, 4, protoWireBytes)
	bad = protoAppendVarint(bad, 1)
	bad = append(bad, 0)
	if _, status := scanReceiveMemoryPack(append(bad, wire...), path); status != receiveMemoryPackScanInvalid {
		t.Fatal("malformed ignored direct body was silently bypassed")
	}
	finalId := NewId()
	legacy = protoAppendTag(legacy, 1, protoWireBytes)
	legacy = protoAppendVarint(legacy, 1)
	legacy = append(legacy, 7)
	legacy = appendIdField(legacy, 1, finalId)
	wire = receiveMemoryScanTestWire(path, legacy, true)
	result, status = scanReceiveMemoryPack(wire, path)
	if status != receiveMemoryPackScanReady || result.messageId != finalId || result.loanSafe {
		t.Fatal("valid final legacy ID was rejected because a superseded value had a different width")
	}
	external, ok := decodePack(nil)
	if !ok {
		t.Fatal("protocol constructor failed")
	}
	if err := ProtoUnmarshal(wire[result.packOffset:result.packOffset+result.packByteCount], external); err != nil {
		t.Fatal(err)
	}
	id, err := IdFromBytes(external.MessageId)
	if err != nil || id != result.messageId {
		t.Fatal("legacy final-ID preflight differs from ProtoUnmarshal")
	}
	returnDecodedPackMessageBytes(external)
	// An empty encrypted field is not an encrypted carrier in Client.run.
	wire = receiveMemoryScanTestWire(path, direct, false)
	wire = protoAppendTag(wire, 6, protoWireBytes)
	wire = protoAppendVarint(wire, 0)
	result, status = scanReceiveMemoryPack(wire, path)
	if status != receiveMemoryPackScanCompatibility || result.encrypted || result.frameCount != 1 {
		t.Fatal("empty encryption field suppressed a valid plaintext Pack")
	}
}

// Sparse repeated paths can merge for routing but reset in the actual inner
// decoder. Publish both facts; never treat that ambiguity as predecessor credit.
func TestReceiveMemoryScanPathMergeAndEncryptedControl(t *testing.T) {
	source, destination := NewId(), NewId()
	expected := TransferPath{SourceId: source, DestinationId: destination}
	body, _, _ := receiveMemoryScanTestBody(t, 1, 40)
	wire := receiveMemoryScanTestWire(TransferPath{SourceId: source}, body, false)
	path := TransferPath{DestinationId: destination}
	wire = protoAppendTag(wire, 1, protoWireBytes)
	wire = protoAppendVarint(wire, uint64(sizeTransferPath(path)))
	wire = appendTransferPath(wire, path)
	result, status := scanReceiveMemoryPack(wire, expected)
	if status != receiveMemoryPackScanCompatibility || result.routingPath != expected || result.path != path || result.pathMatches || result.loanSafe {
		t.Fatal("ambiguous routing/inner path semantics were hidden")
	}
	ids := appendIdField(nil, 1, NewId())
	ids = appendIdField(ids, 2, NewId())
	frame := protoAppendTag(nil, 1, protoWireVarint)
	frame = protoAppendVarint(frame, 23)
	withEc := protoAppendTag(append([]byte{}, ids...), 5, protoWireBytes)
	withEc = protoAppendVarint(withEc, uint64(len(frame)))
	withEc = append(withEc, frame...)
	result, status = scanReceiveMemoryPack(receiveMemoryScanTestWire(expected, withEc, false), expected)
	if status != receiveMemoryPackScanReady || !result.hasEncryptedControl {
		t.Fatal("effective encrypted control was not exposed before role selection")
	}
	frame = protoAppendTag(frame, 1, protoWireVarint)
	frame = protoAppendVarint(frame, 16)
	withoutEc := protoAppendTag(append([]byte{}, ids...), 5, protoWireBytes)
	withoutEc = protoAppendVarint(withoutEc, uint64(len(frame)))
	withoutEc = append(withoutEc, frame...)
	result, status = scanReceiveMemoryPack(receiveMemoryScanTestWire(expected, withoutEc, false), expected)
	if status != receiveMemoryPackScanReady || result.hasEncryptedControl || result.loanSafe {
		t.Fatal("superseded encrypted-control type was treated as effective")
	}
}

// Sparse repeated direct ACK messages merge in the existing owned decoder.
// The selected-body caller must retain the whole category, not just last bytes.
func TestReceiveMemoryScanRepeatedAckMergeFlag(t *testing.T) {
	path := TransferPath{SourceId: NewId(), DestinationId: NewId()}
	first := appendIdField(nil, 1, NewId())
	first = appendIdField(first, 2, NewId())
	second := protoAppendTag(nil, 3, protoWireVarint)
	second = protoAppendVarint(second, 1)
	wire := protoAppendTag(nil, 1, protoWireBytes)
	wire = protoAppendVarint(wire, uint64(sizeTransferPath(path)))
	wire = appendTransferPath(wire, path)
	for _, body := range [][]byte{first, second} {
		wire = protoAppendTag(wire, 5, protoWireBytes)
		wire = protoAppendVarint(wire, uint64(len(body)))
		wire = append(wire, body...)
	}
	scan, status := scanReceiveMemoryPack(wire, path)
	if status != receiveMemoryPackScanNotPack || !scan.ack || !scan.ackNeedsMerge || scan.ackLegacy || !scan.compatibility {
		t.Fatal("sparse direct ACK merge was reduced to last body")
	}
	wrapper := protoAppendTag(nil, 1, protoWireVarint)
	wrapper = protoAppendVarint(wrapper, 1)
	wrapper = protoAppendTag(wrapper, 2, protoWireBytes)
	wrapper = protoAppendVarint(wrapper, 0)
	wire = protoAppendTag(wire, 2, protoWireBytes)
	wire = protoAppendVarint(wire, uint64(len(wrapper)))
	wire = append(wire, wrapper...)
	scan, status = scanReceiveMemoryPack(wire, path)
	if status != receiveMemoryPackScanNotPack || !scan.ack || !scan.legacy || !scan.ackLegacy || scan.ackNeedsMerge || scan.ackByteCount != 0 {
		t.Fatal("empty selected legacy ACK lost body presence or failed to override direct category")
	}
}
