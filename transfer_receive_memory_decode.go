// Owned decoding prepays copied payload roots before materialization. The scan
// selects the canonical effective body without allocating ignored outer values.
// It does not authenticate a new contract or grant Head authority.
package connect

import "github.com/urnetwork/connect/protocol"

// Borrows carrier; a successful Pack decoder owns copied roots, while the
// caller continues to own carrier until its normal ReceivePack handoff. The
// pressure result distinguishes safe refusal from malformed/authenticated data.
func (self *Client) unmarshalReceiveMemoryFrame(carrier []byte, decoded *decodedTransferFrame, decodePath bool,
	expectedPath TransferPath, decrypted bool, role sequenceTlsRole, companion bool) (decodedOk bool, pressure bool) {
	if !self.settings.ReceiveBufferSettings.ownsDecodedReceiveMemory() {
		return unmarshalOwnedTransferFrame(carrier, decoded, decodePath), false
	}
	scan, status := scanReceiveMemoryPack(carrier, expectedPath)
	if status == receiveMemoryPackScanInvalid || decodePath && !scan.pathMatches {
		return false, false
	}
	decoded.release()
	*decoded = decodedTransferFrame{}
	if decodePath {
		decoded.pathIds = [3]Id{scan.path.DestinationId, scan.path.SourceId, scan.path.StreamId}
		decoded.path = protocol.TransferPath{DestinationId: decoded.pathIds[0][:], SourceId: decoded.pathIds[1][:], StreamId: decoded.pathIds[2][:]}
		decoded.frame.TransferPath = &decoded.path
	}
	if scan.sessionRoleSet {
		decoded.sessionRole = protocol.SequenceRole(scan.sessionRole)
		decoded.frame.SessionRole = &decoded.sessionRole
	}
	if scan.sessionCompanionSet {
		decoded.companion = scan.sessionCompanion
		decoded.frame.SessionCompanion = &decoded.companion
	}
	if scan.encrypted {
		decoded.frame.EncryptedTransferFrame = carrier[scan.encryptedOffset : scan.encryptedOffset+scan.encryptedByteCount]
		return true, false
	}
	if scan.ack {
		if scan.ackLegacy {
			if err := ProtoUnmarshal(carrier[scan.ackOffset:scan.ackOffset+scan.ackByteCount], &decoded.ack); err != nil {
				return false, false
			}
		} else if scan.ackNeedsMerge {
			cursor := receiveMemoryWireCursor{bytes: carrier}
			for cursor.offset < len(carrier) {
				field, ok := cursor.next()
				if !ok {
					return false, false
				}
				if field.number == 5 && !decodeAckOwned(field.bytes, decoded) {
					return false, false
				}
			}
		} else if !decodeAckOwned(carrier[scan.ackOffset:scan.ackOffset+scan.ackByteCount], decoded) {
			return false, false
		}
		decoded.frame.Ack = &decoded.ack
		return true, false
	}
	if status != receiveMemoryPackScanReady && status != receiveMemoryPackScanCompatibility {
		return false, false
	}
	if !decrypted {
		role, companion = sequenceTlsRoleServer, false
		if scan.sessionRoleSet {
			if senderRole, ok := sequenceTlsRoleFromProtobuf(protocol.SequenceRole(scan.sessionRole)); ok {
				role = senderRole.complement()
			}
		}
		if scan.sessionCompanionSet {
			companion = scan.sessionCompanion
		}
	}
	var sequence *ReceiveSequence
	// EC can select another actual local role after its payload is parsed.
	// Such a plaintext original gets fresh bytes, never a guessed prefix loan.
	if scan.loanSafe && !scan.nack && (decrypted || !scan.hasEncryptedControl) {
		key := receiveSequenceId{Source: expectedPath.SourceMask(), SequenceId: scan.sequenceId,
			EncryptionRole: role, EncryptionCompanion: companion, LogicalLane: scan.logicalLane}
		buffer := self.receiveBuffer
		buffer.mutex.Lock()
		candidate := buffer.receiveSequences[key]
		if candidate != nil && candidate.transferKey.ForceStream == scan.forceStream &&
			candidate.transferKey.CompanionContract == scan.companionContract &&
			candidate.transferKey.LogicalLane == scan.logicalLane && candidate.ctx.Err() == nil {
			sequence = candidate
		}
		buffer.mutex.Unlock()
	}
	owner := self.receiveBuffer.takeReceiveMemoryOwner(sequence, scan.messageId, scan.sequenceNumber, scan.retainedByteCount)
	repairCopy := false
	if owner == nil {
		owner = self.receiveBuffer.takeReceiveHeldHeadRepairOwner(sequence, scan, carrier)
		repairCopy = owner != nil
	}
	if owner == nil {
		owner = self.receiveBuffer.takeReceiveHeadRecoveryOwnerInSession(sequence, scan, decrypted)
	}
	if owner == nil {
		if sequence != nil {
			sequence.requestReceiveMemoryPressure(scan, decrypted)
		}
		self.recordReceivePackHandoffDrop(scan.messageByteCount)
		return false, true
	}
	// Cleanup custody is established before either decoder can allocate/fail.
	decoded.packOwner = owner
	body := carrier[scan.packOffset : scan.packOffset+scan.packByteCount]
	if repairCopy {
		if !decodeReceiveHeldHeadRepair(body, scan, owner) {
			return false, false
		}
		owner.receivePack.Pack = &owner.pack
		decoded.frame.Pack = &owner.pack
	} else if scan.legacy {
		owner.receivePack.Pack = &protocol.Pack{}
		if err := ProtoUnmarshal(body, owner.receivePack.Pack); err != nil {
			return false, false
		}
		decoded.frame.Pack = owner.receivePack.Pack
	} else {
		if _, ok := decodePackOwned(body, owner); !ok {
			return false, false
		}
		owner.receivePack.Pack = &owner.pack
		decoded.frame.Pack = &owner.pack
	}
	// The preflight identity is the only authority allowed to borrow a loan.
	// A mismatch is a scanner defect, never permission to bind another owner.
	pack := decoded.frame.Pack
	messageId, messageErr := IdFromBytes(pack.MessageId)
	sequenceId, sequenceErr := IdFromBytes(pack.SequenceId)
	if messageErr != nil || sequenceErr != nil || messageId != scan.messageId || sequenceId != scan.sequenceId ||
		pack.SequenceNumber != scan.sequenceNumber || pack.Nack != scan.nack || pack.LogicalLane != scan.logicalLane ||
		pack.ForceStream != scan.forceStream || pack.CompanionContract != scan.companionContract {
		return false, false
	}
	return true, false
}
