// Immediate identities are reconstructible without retaining per-packet IDs.
package connect

import (
	"crypto/sha256"
	"encoding/binary"
)

// Only opted-in reliable originals use this domain. Ordinary NewId semantics
// are unchanged. This binds identity, not authentication: source/session and
// contract authority remain mandatory before any delivery or duplicate ACK.
func immediateMessageId(sourceId Id, sequenceId Id, sequenceNumber uint64) Id {
	const domain = "urnetwork/reliable-immediate-message/v1"
	var body [len(domain) + 2*len(Id{}) + 8]byte
	copy(body[:], domain)
	offset := len(domain)
	copy(body[offset:], sourceId[:])
	offset += len(sourceId)
	copy(body[offset:], sequenceId[:])
	offset += len(sequenceId)
	binary.BigEndian.PutUint64(body[offset:], sequenceNumber)
	hash := sha256.Sum256(body[:])
	var messageId Id
	copy(messageId[:6], sequenceId[:6])
	copy(messageId[6:], hash[:10])
	return messageId
}
