package connect

// Role and generation policy of the ordered handshake inbox.
type sequenceHandshakeAction uint8

const (
	handshakeIgnore sequenceHandshakeAction = iota
	handshakeDeliver
	handshakeStart
)

// A responder can start or name an attempt only on a ClientHello. During a
// time-bounded attempt it may ignore an older retransmit until the attempt ends.
func selectSequenceHandshake(role sequenceTlsRole, hello, live, ordered bool, current, incoming Id) sequenceHandshakeAction {
	foreign := incoming != (Id{}) && current != incoming
	if role == sequenceTlsRoleServer && hello {
		if !live {
			return handshakeStart
		}
		if foreign && current != (Id{}) {
			if ordered && incoming.LessThan(current) {
				return handshakeIgnore
			}
			return handshakeStart
		}
		return handshakeDeliver
	}
	if !live || foreign {
		return handshakeIgnore
	}
	return handshakeDeliver
}

// Both inboxes apply the same lifetime check while the session lock protects the epoch.
func (self *tlsHandshakeEpoch) acceptsHandshakeWithLock() bool {
	return self != nil && !isClosed(self.handshakeDone) && !self.identityFailed && self.ctx.Err() == nil
}
