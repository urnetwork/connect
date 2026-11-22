// Bounded borrowed wire inspection for one verified stream grant. The ordinary
// Transfer decryptor, signed current contract and native sequence still own
// their checks. A forwarded application route cannot bootstrap a new grant.
package connect

import (
	"bytes"
	"crypto/sha256"

	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/encoding/protowire"
)

// Application kinds routed to the peer. Control frames and future unclassified
// kinds stay outside this enrolled data and delivery-receipt carrier.
func streamForwardingApplicationKind(kind protocol.MessageType) bool {
	switch kind {
	case protocol.MessageType_TestSimpleMessage,
		protocol.MessageType_IpIpPacketToProvider,
		protocol.MessageType_IpIpPacketFromProvider,
		protocol.MessageType_Subprotocol,
		protocol.MessageType_MessageMessageServerRequest,
		protocol.MessageType_MessageMessageServerResponse,
		protocol.MessageType_MessageMessageServerPush,
		protocol.MessageType_MessageMessageServerFragment:
		return true
	default:
		return false
	}
}

// Borrows the existing wire scanner's final Frame body without decoding or
// copying its application bytes.
func streamForwardingFrameBody(raw []byte) (protocol.MessageType, []byte, bool) {
	kind, size, offset, ok := receiveMemoryScanFrame(raw, 0)
	if !ok {
		return 0, nil, false
	}
	return protocol.MessageType(kind), raw[offset : offset+size], true
}

// Allows a reliable retry to repeat the exact signed work already enrolled.
// The ordinary contract registration still verifies its authentication.
func (self *streamForwardingGrant) repeatsGrant(frame []byte) bool {
	kind, contract, ok := streamForwardingFrameBody(frame)
	if !ok || kind != protocol.MessageType_TransferContract {
		return false
	}
	cursor := receiveMemoryWireCursor{bytes: contract}
	var stored []byte
	for cursor.offset < len(contract) {
		field, ok := cursor.next()
		if !ok {
			return false
		}
		if field.number == 1 {
			if field.wireType != protowire.BytesType || stored != nil {
				return false
			}
			stored = field.bytes
		}
	}
	return len(stored) != 0 && sha256.Sum256(stored) == self.contractDigest
}

// The forward Pack must contain peer data entirely inside the already enrolled
// sequence and grant. A reverse Ack addresses that same sender sequence, where
// normal admission checks whether its message was sent and remains pending.
func (self *streamForwardingGrant) permitsBody(class byte, body []byte) bool {
	pack := class == streamForwardingPack
	cursor := receiveMemoryWireCursor{bytes: body}
	var messageId, sequenceId, applicationFrame bool
	for cursor.offset < len(body) {
		field, ok := cursor.next()
		if !ok {
			return false
		}
		switch field.number {
		case 1:
			if messageId || field.wireType != protowire.BytesType || len(field.bytes) != len(Id{}) ||
				[16]byte(field.bytes) == (Id{}) {
				return false
			}
			messageId = true
		case 2:
			if sequenceId || field.wireType != protowire.BytesType || !bytes.Equal(field.bytes, self.sequenceId[:]) {
				return false
			}
			sequenceId = true
		case 5:
			if field.wireType != protowire.BytesType {
				return false
			}
			if pack {
				kind, _, valid := streamForwardingFrameBody(field.bytes)
				if !valid || !streamForwardingApplicationKind(kind) {
					return false
				}
				applicationFrame = true
			} else if !bytes.Equal(field.bytes, self.contractId[:]) {
				// A missing-contract request applies only to this grant.
				return false
			}
		case 7:
			if pack && (field.wireType != protowire.BytesType || !self.repeatsGrant(field.bytes)) {
				return false
			}
		case 9:
			if pack && (field.wireType != protowire.BytesType || !bytes.Equal(field.bytes, self.contractId[:])) {
				return false
			}
		case 13:
			if pack && (field.wireType != protowire.VarintType || field.value != 0) {
				return false
			}
		}
	}
	return messageId && sequenceId && (!pack || applicationFrame)
}

// A valid wrap gets its inner application/ack classification immediately after
// the existing endpoint decryption succeeds. Plain and legacy forms can be
// completely scoped here before any generic peer or receive-sequence lookup.
func (self *streamForwardingAssociation) frameClass(wire []byte, expected TransferPath) byte {
	class, _ := self.classifyFrame(wire, expected, nil)
	return class
}

func (self *streamForwardingAssociation) classifyFrame(
	wire []byte,
	expected TransferPath,
	chargedContractId *Id,
) (byte, *streamForwardingReciprocal) {
	path, present, ok := parseFilteredTransferPath(wire)
	if !ok || !present || path != expected {
		return 0, nil
	}
	cursor := receiveMemoryWireCursor{bytes: wire}
	var class byte
	var reciprocal *streamForwardingReciprocal
	for cursor.offset < len(wire) {
		field, ok := cursor.next()
		if !ok {
			return 0, nil
		}
		if field.number != 2 && field.number != 4 && field.number != 5 && field.number != 6 {
			continue
		}
		if class != 0 || field.wireType != protowire.BytesType || len(field.bytes) == 0 {
			return 0, nil
		}
		body := field.bytes
		switch field.number {
		case 2:
			kind, legacyBody, valid := streamForwardingFrameBody(body)
			if !valid {
				return 0, nil
			}
			body = legacyBody
			switch kind {
			case protocol.MessageType_TransferPack:
				class = streamForwardingPack
			case protocol.MessageType_TransferAck:
				class = streamForwardingAck
			default:
				return 0, nil
			}
		case 4:
			class = streamForwardingPack
		case 5:
			class = streamForwardingAck
		case 6:
			class = streamForwardingWrap
			continue
		}
		grant := &self.grant
		if (class == streamForwardingPack) != (path.SourceId == grant.path.SourceId) {
			reciprocal = self.reciprocal.Load()
			if !reciprocal.live() {
				return 0, nil
			}
			grant = &reciprocal.grant
		}
		if chargedContractId != nil && *chargedContractId != grant.contractId ||
			!grant.permitsBody(class, body) {
			return 0, nil
		}
	}
	return class, reciprocal
}
