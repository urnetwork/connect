// Authenticated readiness controls for an enrolled pair of stream endpoints.
// The physical receive handler borrows the wire buffer. A newly encoded control
// belongs to the caller until its bounded physical send takes ownership.
package connect

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"sync"
	"time"
)

const (
	p2pForwardProbeVersion   = byte(2)
	p2pForwardProbeByteCount = p2pStreamProbeByteCount + sha256.Size
)

// These directional control keys come from a verified endpoint enrollment.
// They are separate from the keys dedicated to forwarded transfers and acks.
type p2pStreamReadinessKeys struct {
	transcript               [sha256.Size]byte
	sendKey                  [sha256.Size]byte
	receiveKey               [sha256.Size]byte
	expirationTimeUnixMillis int64
	destination              bool
}

// One logical stream incarnation selects authenticated readiness before it can
// publish a nonadjacent writer. Its owner cancels this context on retirement.
type p2pStreamReadiness struct {
	ctx         context.Context
	installOnce sync.Once
	installed   chan struct{}
	keys        p2pStreamReadinessKeys
}

// Creates a pending enrollment. It admits no readiness traffic until installed.
func newP2pStreamReadiness(ctx context.Context) *p2pStreamReadiness {
	return &p2pStreamReadiness{ctx: ctx, installed: make(chan struct{})}
}

// Installs the verified keys after the owner's authenticated receive path is
// available. Repeat delivery of the same acceptance is idempotent.
func (self *p2pStreamReadiness) install(keys p2pStreamReadinessKeys) bool {
	var zero [sha256.Size]byte
	if self.ctx.Err() != nil || keys.expirationTimeUnixMillis <= time.Now().UnixMilli() ||
		keys.transcript == zero || keys.sendKey == zero ||
		keys.receiveKey == zero || keys.sendKey == keys.receiveKey {
		return false
	}
	self.installOnce.Do(func() {
		self.keys = keys
		close(self.installed)
	})
	return self.keys == keys
}

// Reads the immutable key set after the matching installation or returns nil.
func (self *p2pStreamReadiness) activeKeys() *p2pStreamReadinessKeys {
	if self.ctx.Err() == nil {
		select {
		case <-self.installed:
			if self.keys.expirationTimeUnixMillis > time.Now().UnixMilli() {
				return &self.keys
			}
		default:
		}
	}
	return nil
}

// The ordinary direct peer uses the established stream challenge. Forwarded
// endpoint challenges use a fresh full-width cryptographic nonce.
func (self *p2pStreamProbe) challengeNonce() Id {
	if self.forwardReadiness == nil {
		return NewId()
	}
	var nonce Id
	rand.Read(nonce[:])
	return nonce
}

// Only the fixed-size reserved control header reaches this operation. The
// transcript and role-specific key bind its purpose, stream, kind and nonce.
func (self *p2pStreamReadinessKeys) tag(key *[sha256.Size]byte, header []byte, out []byte) []byte {
	mac := hmac.New(sha256.New, key[:])
	mac.Write([]byte("urnetwork-forward-readiness-v2"))
	mac.Write(self.transcript[:])
	mac.Write(header)
	return mac.Sum(out)
}

// Returns an owned control buffer. Direct endpoints retain the original wire.
func encodeP2pReadinessProbe(keys *p2pStreamReadinessKeys, messageType byte, streamId Id, nonce Id) []byte {
	if keys == nil {
		return encodeP2pStreamProbe(messageType, streamId, nonce)
	}
	message := MessagePoolGet(p2pForwardProbeByteCount)
	writeP2pStreamProbeHeader(message, messageType, streamId, nonce)
	message[len(p2pStreamProbePrefix)-1] = p2pForwardProbeVersion
	keys.tag(&keys.sendKey, message[:p2pStreamProbeByteCount], message[:p2pStreamProbeByteCount])
	return message
}

// Borrows one exact authenticated control. Nothing from the peer allocates or
// selects a key or starts enrollment. The caller already holds the local keys.
func (self *p2pStreamReadinessKeys) verifies(message []byte) bool {
	var tag [sha256.Size]byte
	return len(message) == p2pForwardProbeByteCount &&
		hmac.Equal(message[p2pStreamProbeByteCount:], self.tag(&self.receiveKey, message[:p2pStreamProbeByteCount], tag[:0]))
}
