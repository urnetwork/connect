// Allocation-free preflight for one complete owned plaintext TransferFrame.
// Facts are copied values and body offsets; no borrowed slice escapes. The
// caller still authenticates the peer/session and checks delivery semantics.
package connect

import (
	"crypto/sha256"
	"math"

	"google.golang.org/protobuf/encoding/protowire"

	"github.com/urnetwork/connect/v2026/protocol"
)

type receiveMemoryPackScanStatus uint8

const (
	receiveMemoryPackScanInvalid receiveMemoryPackScanStatus = iota
	receiveMemoryPackScanNotPack
	receiveMemoryPackScanReady
	receiveMemoryPackScanCompatibility
)

// Offsets select effective bodies without allocating discarded outer values.
// Legacy Pack uses a funded external ProtoUnmarshal graph; normal v2 keeps its
// inline owned decoder. ACK repeats may require merging the selected category.
type receiveMemoryPackScan struct {
	path                  TransferPath
	loanSafe              bool
	compatibility         bool
	routingPath           TransferPath
	pathMatches           bool
	messageId             Id
	messageIdSet          bool
	sequenceId            Id
	sequenceIdSet         bool
	contractId            Id
	contractIdSet         bool
	contractFrameSet      bool
	contractFrameType     int32
	contractProof         [32]byte
	sequenceNumber        uint64
	logicalLane           uint32
	head                  bool
	nack                  bool
	forceStream           bool
	companionContract     bool
	contractAhead         bool
	sessionRole           int32
	sessionRoleSet        bool
	sessionCompanion      bool
	sessionCompanionSet   bool
	legacy                bool
	encrypted             bool
	encryptedOffset       int
	encryptedByteCount    int
	ack                   bool
	ackOffset             int
	ackByteCount          int
	ackLegacy             bool
	ackNeedsMerge         bool
	hasEncryptedControl   bool
	packOffset            int
	packByteCount         int
	frameCount            int
	protocolFrameCount    int
	consumerFrameCount    int
	providerFrameCount    int
	providerTcpFrameCount int
	messageByteCount      ByteCount
	payloadRootByteCount  ByteCount
	metadataByteCount     ByteCount
	retainedByteCount     ByteCount
}

// These local views borrow only for the current scan; neither is published.
type receiveMemoryWireCursor struct {
	bytes  []byte
	offset int
	base   int
}

type receiveMemoryWireField struct {
	number     protowire.Number
	wireType   protowire.Type
	value      uint64
	bytes      []byte
	bodyOffset int
}

// Reuses protobuf's checked, allocation-free wire consumers. Its unknown-group
// recursion limit is the same one used by the actual owned decoder; no new
// count, size or nesting policy is inferred from the H1 batching defaults.
func (self *receiveMemoryWireCursor) next() (receiveMemoryWireField, bool) {
	field := receiveMemoryWireField{}
	if self.offset >= len(self.bytes) {
		return field, false
	}
	rest := self.bytes[self.offset:]
	number, wireType, n := protowire.ConsumeTag(rest)
	if n < 0 {
		return field, false
	}
	self.offset += n
	rest = self.bytes[self.offset:]
	field.number, field.wireType = number, wireType
	switch wireType {
	case protowire.VarintType:
		value, n := protowire.ConsumeVarint(rest)
		if n < 0 {
			return field, false
		}
		field.value = value
		self.offset += n
	case protowire.BytesType:
		value, n := protowire.ConsumeBytes(rest)
		if n < 0 {
			return field, false
		}
		field.bytes = value
		field.bodyOffset = self.base + self.offset + n - len(value)
		self.offset += n
	default:
		n := protowire.ConsumeFieldValue(number, wireType, rest)
		if n < 0 {
			return field, false
		}
		self.offset += n
	}
	return field, true
}

// Accumulate rather than saturating: a failed bound must never become a loan.
func receiveMemoryScanAdd(total, value ByteCount) (ByteCount, bool) {
	if total < 0 || value < 0 || value > math.MaxInt64-total {
		return 0, false
	}
	return total + value, true
}

// Match MessagePoolCopy/MessagePoolRootByteCount without initializing a pool.
// Empty Frame bytes allocate no root. The class table is guarded against the
// actual owned decoder in the scanner tests, including the unpooled tail.
func receiveMemoryScanCopyRoot(byteCount int) ByteCount {
	if byteCount <= 0 {
		return 0
	}
	switch {
	case byteCount <= 256:
		return 256
	case byteCount <= 2048:
		return 2048
	case byteCount <= 4096:
		return 4096
	case byteCount <= 8192:
		return 8192
	case byteCount <= 12288:
		return 12288
	default:
		return ByteCount(byteCount)
	}
}

// The ordinary inline pair and eight-frame case keep the existing exact1872
// envelope. Beyond eight, bound both current/growing pointer arrays rather
// than guessing a toolchain-specific append capacity. Growth is at most2x;
// two allocation-page roundings add at most2048 eight-byte slots. Overflow
// frames retain the existing conservative128-byte allowance each.
func receiveMemoryScanMetadata(frameCount int) (ByteCount, bool) {
	if frameCount < 0 {
		return 0, false
	}
	total := decodedPackOwnerQueueByteCount
	if frameCount <= decodedPackOwnerInlineFrames {
		return total, true
	}
	overflow := ByteCount(frameCount - decodedPackOwnerInlineFrames)
	if overflow > math.MaxInt64/128 {
		return 0, false
	}
	var ok bool
	total, ok = receiveMemoryScanAdd(total, overflow*128)
	if !ok {
		return 0, false
	}
	var pointers ByteCount
	if frameCount <= 8 {
		pointers = ByteCount(decodedPackOwnerInlineFrames)
		for pointers < ByteCount(frameCount) {
			pointers *= 2
		}
	} else {
		if ByteCount(frameCount) > (math.MaxInt64-2048)/4 {
			return 0, false
		}
		pointers = 4*ByteCount(frameCount) + 2048
	}
	if pointers > (math.MaxInt64-16)/8 {
		return 0, false
	}
	return receiveMemoryScanAdd(total, 8*pointers+16)
}

// Final path IDs follow protobuf last-value behavior. The mask also lets the
// outer scan reproduce routing's cross-message merge without retaining bytes.
func receiveMemoryScanPath(body []byte) (TransferPath, uint8, bool, bool) {
	var path TransferPath
	var seen, valid uint8
	safe := true
	cursor := receiveMemoryWireCursor{bytes: body}
	for cursor.offset < len(body) {
		field, ok := cursor.next()
		if !ok {
			return TransferPath{}, 0, false, false
		}
		switch field.number {
		case 1, 2, 3:
			if field.wireType != protowire.BytesType {
				return TransferPath{}, 0, false, false
			}
			bit := uint8(1) << uint(field.number-1)
			if seen&bit != 0 {
				safe = false
			}
			seen |= bit
			valid &^= bit
			if len(field.bytes) == len(Id{}) {
				valid |= bit
			}
			switch field.number {
			case 1:
				path.DestinationId = Id{}
				copy(path.DestinationId[:], field.bytes)
			case 2:
				path.SourceId = Id{}
				copy(path.SourceId[:], field.bytes)
			case 3:
				path.StreamId = Id{}
				copy(path.StreamId[:], field.bytes)
			}
		}
	}
	return path, seen, safe, valid == seen
}

// Final Frame values follow the owned decoder's last-wins behavior. Every
// replacement root contributes to a conservative peak bound; duplicate fields
// disable predecessor loans but do not make valid protobuf inadmissible.
func receiveMemoryScanFrameBound(body []byte, base int) (messageType int32, messageBytes int, messageOffset int, rootBound ByteCount, loanSafe bool, ok bool) {
	loanSafe = true
	var seen uint8
	cursor := receiveMemoryWireCursor{bytes: body, base: base}
	for cursor.offset < len(body) {
		field, valid := cursor.next()
		if !valid {
			return 0, 0, 0, 0, false, false
		}
		switch field.number {
		case 1, 2, 3:
			bit := uint8(1) << uint(field.number-1)
			if seen&bit != 0 {
				loanSafe = false
			}
			seen |= bit
			if field.number == 2 {
				if field.wireType != protowire.BytesType {
					return 0, 0, 0, 0, false, false
				}
				messageBytes, messageOffset = len(field.bytes), field.bodyOffset
				var added bool
				rootBound, added = receiveMemoryScanAdd(rootBound, receiveMemoryScanCopyRoot(messageBytes))
				if !added {
					return 0, 0, 0, 0, false, false
				}
			} else {
				if field.wireType != protowire.VarintType {
					return 0, 0, 0, 0, false, false
				}
				if field.number == 1 {
					messageType = int32(field.value)
				}
			}
		}
	}
	return messageType, messageBytes, messageOffset, rootBound, loanSafe, true
}

// A borrowed legacy wrapper needs only its final type/body offset; the bound
// helper separately supplies replacement storage and loan eligibility.
func receiveMemoryScanFrame(body []byte, base int) (messageType int32, messageBytes int, messageOffset int, ok bool) {
	messageType, messageBytes, messageOffset, _, _, ok = receiveMemoryScanFrameBound(body, base)
	return
}

// Raw provider classification borrows the validated final frame payload.
// Wrapped packets keep ordinary admission; inspecting them would allocate
// before the decode owner was funded. Duplicate frame scalars never reach a
// recovery loan because the caller also requires scan.loanSafe.
func receiveMemoryScanRawProviderProtocol(body []byte, packet []byte) IpProtocol {
	raw := false
	cursor := receiveMemoryWireCursor{bytes: body}
	for cursor.offset < len(body) {
		field, ok := cursor.next()
		if !ok {
			return IpProtocolUnknown
		}
		if field.number == 3 {
			raw = field.wireType == protowire.VarintType && field.value != 0
		}
	}
	if !raw {
		return IpProtocolUnknown
	}
	return receiveProviderHeadRecoveryProtocol(packet)
}

// Known Tag scalars follow last-value behavior; no payload allocation occurs.
func receiveMemoryScanTag(body []byte) bool {
	cursor := receiveMemoryWireCursor{bytes: body}
	for cursor.offset < len(body) {
		field, ok := cursor.next()
		if !ok {
			return false
		}
		if field.number == 1 && field.wireType != protowire.VarintType {
			return false
		}
	}
	return true
}

// Legacy protobuf retains unknown bytes. A conservative two-array growth
// bound is paid separately from payload copies; empty unknown storage is free.
func receiveMemoryScanUnknown(body []byte, known protowire.Number) (ByteCount, bool) {
	cursor := receiveMemoryWireCursor{bytes: body}
	var size ByteCount
	for cursor.offset < len(body) {
		before := cursor.offset
		field, ok := cursor.next()
		if !ok {
			return 0, false
		}
		if field.number < 1 || known < field.number {
			size, ok = receiveMemoryScanAdd(size, ByteCount(cursor.offset-before))
			if !ok {
				return 0, false
			}
		}
	}
	if size == 0 {
		return 0, true
	}
	if size <= 12288 {
		cost := receiveMemoryScanCopyRoot(int(size))
		if cost > math.MaxInt64/4 {
			return 0, false
		}
		return 4 * cost, true
	}
	if size > (math.MaxInt64-16384)/4 {
		return 0, false
	}
	return 4*size + 16384, true
}

// The legacy proto object graph is additional to the embedded owner. Frame
// and ID replacements are conservatively summed; message merges cannot make
// a final contract/tag larger than all its observed component allocations.
func receiveMemoryScanPackBody(body []byte, base int, scan *receiveMemoryPackScan, legacy bool) bool {
	var seen uint16
	messageValid, sequenceValid, contractValid := false, false, true
	var legacyExtra ByteCount
	cursor := receiveMemoryWireCursor{bytes: body, base: base}
	for cursor.offset < len(body) {
		field, ok := cursor.next()
		if !ok {
			return false
		}
		if field.number >= 1 && field.number <= 14 && field.number != 5 {
			bit := uint16(1) << uint(field.number-1)
			if seen&bit != 0 {
				scan.loanSafe = false
			}
			seen |= bit
		}
		switch field.number {
		case 1, 2, 9:
			if field.wireType != protowire.BytesType {
				return false
			}
			valid := len(field.bytes) == len(Id{})
			if !legacy && !valid {
				return false
			}
			if legacy {
				legacyExtra, ok = receiveMemoryScanAdd(legacyExtra, receiveMemoryScanCopyRoot(len(field.bytes)))
				if !ok {
					return false
				}
			}
			switch field.number {
			case 1:
				scan.messageId = Id{}
				copy(scan.messageId[:], field.bytes)
				messageValid = valid
			case 2:
				scan.sequenceId = Id{}
				copy(scan.sequenceId[:], field.bytes)
				sequenceValid = valid
			case 9:
				scan.contractId = Id{}
				copy(scan.contractId[:], field.bytes)
				scan.contractIdSet = true
				contractValid = valid
			}
		case 3, 4, 6, 10, 11, 12, 13, 14:
			if field.wireType != protowire.VarintType {
				return false
			}
			switch field.number {
			case 3:
				scan.sequenceNumber = field.value
			case 4:
				scan.head = field.value != 0
			case 6:
				scan.nack = field.value != 0
			case 10:
				scan.forceStream = field.value != 0
			case 11:
				scan.companionContract = field.value != 0
			case 12:
				scan.logicalLane = uint32(field.value)
			case 13:
				scan.contractAhead = field.value != 0
			}
		case 5, 7:
			if field.wireType != protowire.BytesType {
				return false
			}
			if field.number == 7 {
				scan.contractFrameSet = true
			}
			kind, size, offset, roots, safe, valid := receiveMemoryScanFrameBound(field.bytes, field.bodyOffset)
			if !valid {
				return false
			}
			if field.number == 7 {
				scan.contractFrameType = kind
				if size == 0 {
					scan.contractProof = sha256.Sum256(nil)
				} else {
					start := offset - field.bodyOffset
					scan.contractProof = sha256.Sum256(field.bytes[start : start+size])
				}
			}
			if !safe {
				scan.loanSafe = false
			}
			scan.payloadRootByteCount, ok = receiveMemoryScanAdd(scan.payloadRootByteCount, roots)
			if !ok {
				return false
			}
			if field.number == 5 {
				if scan.frameCount == math.MaxInt {
					return false
				}
				scan.frameCount++
				if protocolOnlyWireFrame(field.bytes) {
					scan.protocolFrameCount++
				}
				if kind == int32(protocol.MessageType_IpIpPacketFromProvider) && size > 0 {
					scan.consumerFrameCount++
				}
				if kind == int32(protocol.MessageType_IpIpPacketToProvider) && size > 0 {
					start := offset - field.bodyOffset
					packetProtocol := receiveMemoryScanRawProviderProtocol(field.bytes, field.bytes[start:start+size])
					if packetProtocol != IpProtocolUnknown {
						scan.providerFrameCount++
					}
					if packetProtocol == IpProtocolTcp {
						scan.providerTcpFrameCount++
					}
				}
				scan.messageByteCount, ok = receiveMemoryScanAdd(scan.messageByteCount, ByteCount(size))
				if !ok {
					return false
				}
				// TransferEncryptedControl's established wire enum is23.
				if kind == 23 {
					scan.hasEncryptedControl = true
				}
			}
			if legacy {
				unknown, valid := receiveMemoryScanUnknown(field.bytes, 3)
				if !valid {
					return false
				}
				legacyExtra, ok = receiveMemoryScanAdd(legacyExtra, unknown)
				if !ok {
					return false
				}
				if field.number == 7 {
					legacyExtra, ok = receiveMemoryScanAdd(legacyExtra, 128)
					if !ok {
						return false
					}
				}
			}
		case 8:
			if field.wireType != protowire.BytesType || !receiveMemoryScanTag(field.bytes) {
				return false
			}
			if legacy {
				unknown, valid := receiveMemoryScanUnknown(field.bytes, 1)
				if !valid {
					return false
				}
				legacyExtra, ok = receiveMemoryScanAdd(legacyExtra, unknown)
				if !ok {
					return false
				}
				legacyExtra, ok = receiveMemoryScanAdd(legacyExtra, 128)
				if !ok {
					return false
				}
			}
		}
	}
	scan.messageIdSet = seen&1 != 0 && messageValid
	scan.sequenceIdSet = seen&2 != 0 && sequenceValid
	if !contractValid {
		return false
	}
	metadata, ok := receiveMemoryScanMetadata(scan.frameCount)
	if !ok {
		return false
	}
	if legacy {
		// The inline owner still exists, but every legacy Frame is external.
		count := ByteCount(min(scan.frameCount, decodedPackOwnerInlineFrames))
		metadata, ok = receiveMemoryScanAdd(metadata, count*128)
		if !ok {
			return false
		}
		metadata, ok = receiveMemoryScanAdd(metadata, 512)
		if !ok {
			return false
		}
		unknown, valid := receiveMemoryScanUnknown(body, 13)
		if !valid {
			return false
		}
		legacyExtra, ok = receiveMemoryScanAdd(legacyExtra, unknown)
		if !ok {
			return false
		}
		metadata, ok = receiveMemoryScanAdd(metadata, legacyExtra)
		if !ok {
			return false
		}
	}
	scan.metadataByteCount = metadata
	scan.retainedByteCount, ok = receiveMemoryScanAdd(metadata, scan.payloadRootByteCount)
	return ok
}

// Validate ignored direct ACKs as the owned decoder would before unwrap. The
// count also funds a conservative growing eviction slice if a compatibility
// caller uses the original full outer decoder instead of skipping its bodies.
func receiveMemoryScanAck(body []byte) (ByteCount, bool) {
	cursor := receiveMemoryWireCursor{bytes: body}
	var count ByteCount
	for cursor.offset < len(body) {
		field, ok := cursor.next()
		if !ok {
			return 0, false
		}
		switch field.number {
		case 1, 2, 5:
			if field.wireType != protowire.BytesType || len(field.bytes) != len(Id{}) {
				return 0, false
			}
		case 3, 6, 7, 8, 10, 11, 12:
			if field.wireType != protowire.VarintType {
				return 0, false
			}
		case 4:
			if field.wireType != protowire.BytesType || !receiveMemoryScanTag(field.bytes) {
				return 0, false
			}
		case 9:
			if field.wireType != protowire.BytesType {
				return 0, false
			}
			rest := field.bytes
			for len(rest) != 0 {
				_, n := protowire.ConsumeVarint(rest)
				if n < 0 {
					return 0, false
				}
				rest = rest[n:]
				if count == math.MaxInt64 {
					return 0, false
				}
				count++
			}
		}
	}
	if count == 0 {
		return 1024, true
	}
	if count > (math.MaxInt64-17408)/32 {
		return 0, false
	}
	return count*32 + 17408, true
}

// Borrows one whole carrier. Final facts preserve the actual outer precedence:
// nonempty encryption wins; final legacy Frame overrides its own body category;
// an effective Pack plus ACK is malformed. Repeated/mixed valid outer forms
// use Compatibility/fresh funding rather than becoming wire rejections.
func scanReceiveMemoryPack(carrier []byte, expectedPath TransferPath) (receiveMemoryPackScan, receiveMemoryPackScanStatus) {
	header := receiveMemoryPackScan{loanSafe: true}
	cursor := receiveMemoryWireCursor{bytes: carrier}
	var counts [9]int
	var direct receiveMemoryPackScan
	var directCost, ackCost, wrapperCost ByteCount
	directPresent, ackPresent, legacyPresent := false, false, false
	legacyKind := int32(-1)
	legacyOffset, legacySize := 0, 0
	legacySafe := true
	for cursor.offset < len(carrier) {
		field, ok := cursor.next()
		if !ok {
			return receiveMemoryPackScan{}, receiveMemoryPackScanInvalid
		}
		if field.number >= 1 && field.number <= 8 {
			counts[field.number]++
			if counts[field.number] > 1 {
				header.loanSafe = false
				header.compatibility = true
			}
		}
		switch field.number {
		case 1:
			if field.wireType != protowire.BytesType {
				return receiveMemoryPackScan{}, receiveMemoryPackScanInvalid
			}
			path, mask, safe, valid := receiveMemoryScanPath(field.bytes)
			if !valid {
				return receiveMemoryPackScan{}, receiveMemoryPackScanInvalid
			}
			header.path = path
			if mask&1 != 0 {
				header.routingPath.DestinationId = path.DestinationId
			}
			if mask&2 != 0 {
				header.routingPath.SourceId = path.SourceId
			}
			if mask&4 != 0 {
				header.routingPath.StreamId = path.StreamId
			}
			if !safe {
				header.loanSafe = false
				header.compatibility = true
			}
		case 2:
			if field.wireType != protowire.BytesType {
				return receiveMemoryPackScan{}, receiveMemoryPackScanInvalid
			}
			kind, size, offset, roots, safe, valid := receiveMemoryScanFrameBound(field.bytes, field.bodyOffset)
			if !valid {
				return receiveMemoryPackScan{}, receiveMemoryPackScanInvalid
			}
			wrapperCost, ok = receiveMemoryScanAdd(wrapperCost, roots)
			if !ok {
				return receiveMemoryPackScan{}, receiveMemoryPackScanInvalid
			}
			legacyKind, legacySize, legacyOffset = kind, size, offset
			legacySafe = safe
			legacyPresent = true
		case 4:
			if field.wireType != protowire.BytesType {
				return receiveMemoryPackScan{}, receiveMemoryPackScanInvalid
			}
			candidate := receiveMemoryPackScan{loanSafe: true, packOffset: field.bodyOffset, packByteCount: len(field.bytes)}
			if !receiveMemoryScanPackBody(field.bytes, field.bodyOffset, &candidate, false) {
				return receiveMemoryPackScan{}, receiveMemoryPackScanInvalid
			}
			directCost, ok = receiveMemoryScanAdd(directCost, candidate.retainedByteCount)
			if !ok {
				return receiveMemoryPackScan{}, receiveMemoryPackScanInvalid
			}
			direct = candidate
			directPresent = true
		case 5:
			if field.wireType != protowire.BytesType {
				return receiveMemoryPackScan{}, receiveMemoryPackScanInvalid
			}
			cost, valid := receiveMemoryScanAck(field.bytes)
			if !valid {
				return receiveMemoryPackScan{}, receiveMemoryPackScanInvalid
			}
			ackCost, ok = receiveMemoryScanAdd(ackCost, cost)
			if !ok {
				return receiveMemoryPackScan{}, receiveMemoryPackScanInvalid
			}
			header.ackOffset, header.ackByteCount = field.bodyOffset, len(field.bytes)
			ackPresent = true
		case 6:
			if field.wireType != protowire.BytesType {
				return receiveMemoryPackScan{}, receiveMemoryPackScanInvalid
			}
			header.encryptedOffset, header.encryptedByteCount = field.bodyOffset, len(field.bytes)
		case 3, 7, 8:
			if field.wireType != protowire.VarintType {
				return receiveMemoryPackScan{}, receiveMemoryPackScanInvalid
			}
			switch field.number {
			case 7:
				header.sessionRole = int32(field.value)
				header.sessionRoleSet = true
			case 8:
				header.sessionCompanion = field.value != 0
				header.sessionCompanionSet = true
			}
		}
	}
	if counts[1] == 0 || header.routingPath != expectedPath {
		return receiveMemoryPackScan{}, receiveMemoryPackScanInvalid
	}
	header.pathMatches = header.path == expectedPath
	if !header.pathMatches {
		header.loanSafe = false
		header.compatibility = true
	}
	categories := 0
	for _, field := range []int{2, 4, 5} {
		if counts[field] != 0 {
			categories++
		}
	}
	if categories > 1 || counts[6] != 0 && categories != 0 {
		header.compatibility = true
		header.loanSafe = false
	}
	if header.encryptedByteCount != 0 {
		header.encrypted = true
		header.loanSafe = false
		if categories != 0 {
			cost, ok := receiveMemoryScanAdd(directCost, ackCost)
			if !ok {
				return receiveMemoryPackScan{}, receiveMemoryPackScanInvalid
			}
			cost, ok = receiveMemoryScanAdd(cost, wrapperCost)
			if !ok {
				return receiveMemoryPackScan{}, receiveMemoryPackScanInvalid
			}
			header.retainedByteCount, ok = receiveMemoryScanAdd(MessagePoolRootByteCount(carrier), cost)
			if !ok {
				return receiveMemoryPackScan{}, receiveMemoryPackScanInvalid
			}
		}
		return header, receiveMemoryPackScanNotPack
	}
	selected := direct
	if legacyPresent {
		switch legacyKind {
		case 0:
			selected = receiveMemoryPackScan{loanSafe: legacySafe, legacy: true, packOffset: legacyOffset, packByteCount: legacySize}
			if !receiveMemoryScanPackBody(carrier[legacyOffset:legacyOffset+legacySize], legacyOffset, &selected, true) {
				return receiveMemoryPackScan{}, receiveMemoryPackScanInvalid
			}
			directPresent = true
		case 1:
			ackPresent = true
			header.ackLegacy = true
			header.legacy = true
			header.ackOffset, header.ackByteCount = legacyOffset, legacySize
		default:
			return receiveMemoryPackScan{}, receiveMemoryPackScanInvalid
		}
	}
	if directPresent == ackPresent {
		return receiveMemoryPackScan{}, receiveMemoryPackScanInvalid
	}
	if ackPresent {
		header.ack = true
		header.ackNeedsMerge = !header.ackLegacy && counts[5] > 1
		return header, receiveMemoryPackScanNotPack
	}
	if !selected.messageIdSet || !selected.sequenceIdSet {
		return receiveMemoryPackScan{}, receiveMemoryPackScanInvalid
	}
	selected.path, selected.routingPath, selected.pathMatches = header.path, header.routingPath, header.pathMatches
	selected.sessionRole, selected.sessionRoleSet = header.sessionRole, header.sessionRoleSet
	selected.sessionCompanion, selected.sessionCompanionSet = header.sessionCompanion, header.sessionCompanionSet
	selected.ackOffset, selected.ackByteCount, selected.ackLegacy = header.ackOffset, header.ackByteCount, header.ackLegacy
	selected.encryptedOffset, selected.encryptedByteCount = header.encryptedOffset, header.encryptedByteCount
	selected.compatibility = header.compatibility
	selected.loanSafe = selected.loanSafe && header.loanSafe
	// Ignored bodies are not decoded by final selection. This conservative
	// bound also covers an implementation temporarily using the old full outer
	// decoder, including replaced wrapper bytes and earlier direct owners.
	extra := directCost
	if !selected.legacy {
		extra -= direct.retainedByteCount
	}
	wrapperExtra := wrapperCost
	if legacyPresent {
		wrapperExtra -= receiveMemoryScanCopyRoot(legacySize)
	}
	if wrapperExtra < 0 || extra < 0 {
		return receiveMemoryPackScan{}, receiveMemoryPackScanInvalid
	}
	extra, ok := receiveMemoryScanAdd(extra, wrapperExtra)
	if !ok {
		return receiveMemoryPackScan{}, receiveMemoryPackScanInvalid
	}
	selected.retainedByteCount, ok = receiveMemoryScanAdd(selected.retainedByteCount, extra)
	if !ok {
		return receiveMemoryPackScan{}, receiveMemoryPackScanInvalid
	}
	selected.retainedByteCount, ok = receiveMemoryScanAdd(selected.retainedByteCount, MessagePoolRootByteCount(carrier))
	if !ok {
		return receiveMemoryPackScan{}, receiveMemoryPackScanInvalid
	}
	if selected.compatibility {
		selected.loanSafe = false
		return selected, receiveMemoryPackScanCompatibility
	}
	return selected, receiveMemoryPackScanReady
}
