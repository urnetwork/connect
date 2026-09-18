package extender

import (
	"context"
	"crypto/ed25519"
	"crypto/subtle"
	"fmt"
	"net"
	"slices"
	"sync"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// The latency probe service (DESIGNNOTES4.md).
//
// A probe is answered exactly like any other request -- the same response
// frame, with a nonce added for an attesting provider -- and then the stream
// is read for at most one attestation frame and closed. The extender's part
// is the gate: it timestamps its response, timestamps the frame, and forwards
// the provider's signed claim only when the claim is at least the interval it
// observed, less a tolerance. It cannot alter the number, which is under the
// provider's signature; it can only accept, refuse or drop.
//
// The nonce is fresh random bytes per probe, held on the stack for the life
// of the stream, so there is no table to fill and nothing to replay across
// streams. A probe over the per-source rate is refused before any of this,
// because a probe is cheap by design and would otherwise be a lever.

// The map cap of the probe limiter, the same as the dns forwarder's.
const extenderProbeMaxSourceCount = 4096

// The state of one probe request between the header and the stream.
type extenderProbe struct {
	// the attesting provider's client id, nil for a ranking probe
	clientId []byte
	// the nonce issued in the response, nil when none was
	nonce []byte
}

func (self *extenderProbe) nonceBytes() []byte {
	if self == nil {
		return nil
	}
	return self.nonce
}

// The per-source admission of probes: one token bucket per address, pruned
// when the map grows past the cap.
type extenderProbeLimiter struct {
	stateLock     sync.Mutex
	sourceBuckets map[string]*tokenBucket
}

func newExtenderProbeLimiter() *extenderProbeLimiter {
	return &extenderProbeLimiter{
		sourceBuckets: map[string]*tokenBucket{},
	}
}

// Reports whether one more probe from the source is within its rate, taking
// the token when it is.
func (self *extenderProbeLimiter) admit(source string, now time.Time, ratePerSecond float64, burst int) bool {
	if ratePerSecond <= 0 || burst <= 0 {
		return true
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	bucket := self.sourceBuckets[source]
	if bucket == nil {
		bucket = &tokenBucket{}
		self.sourceBuckets[source] = bucket
	}
	available := bucket.refill(now, ratePerSecond, float64(burst))
	if available {
		bucket.take()
	}
	if extenderProbeMaxSourceCount < len(self.sourceBuckets) {
		// a source with a full bucket is indistinguishable from one never
		// seen, so a flood of addresses cannot grow the map without bound
		for otherSource, otherBucket := range self.sourceBuckets {
			if otherSource == source {
				continue
			}
			if otherBucket.refill(now, ratePerSecond, float64(burst)) && float64(burst) <= otherBucket.tokens {
				delete(self.sourceBuckets, otherSource)
			}
		}
	}
	return available
}

// Admits one probe request and decides whether it gets a nonce
// (DESIGNNOTES4.md §2). A nonce is issued only when the header names a well
// formed client id, this extender has an identity to bind the claim to, and
// there is a handler to receive what passes: a provider is never asked to
// sign for nothing.
func (self *ExtenderServer) beginProbe(header *protocol.ExtenderHeader, remoteAddress string) (*extenderProbe, error) {
	source := connectionSourceAddress(remoteAddress)
	if !self.probeLimiter.admit(
		source,
		time.Now(),
		self.settings.ProbeMaxRatePerSource,
		self.settings.ProbeMaxBurstPerSource,
	) {
		return nil, fmt.Errorf("%s is over its probe rate", source)
	}
	probe := &extenderProbe{}
	if len(header.ProbeClientId) != connect.ExtenderProbeClientIdByteCount {
		return probe, nil
	}
	if self.settings.ProbeAttestationHandler == nil {
		return probe, nil
	}
	if len(self.PublicKey()) != ed25519.PublicKeySize {
		return probe, nil
	}
	nonce, err := connect.NewExtenderProbeNonce()
	if err != nil {
		return nil, err
	}
	probe.clientId = slices.Clone(header.ProbeClientId)
	probe.nonce = nonce
	return probe, nil
}

// Serves the stream after the response of a probe: reads the one attestation
// frame an attesting provider sends, gates it against the interval observed
// and hands it on (DESIGNNOTES4.md §3). A ranking probe has no nonce, and its
// stream simply closes. The response has been flushed when this is called,
// so the interval starts here.
func (self *ExtenderServer) serveProbe(ctx context.Context, clientConn net.Conn, probe *extenderProbe) {
	if probe == nil || probe.nonce == nil {
		return
	}
	responseTime := time.Now()
	if err := clientConn.SetReadDeadline(responseTime.Add(self.settings.ProbeAttestationTimeout)); err != nil {
		self.reportError("probe attestation", err)
		return
	}
	attestation, err := connect.ReadExtenderProbeAttestationFrame(clientConn)
	if err != nil {
		self.reportError("probe attestation", err)
		return
	}
	observed := time.Since(responseTime)
	if err := self.gateProbeAttestation(probe, attestation, observed); err != nil {
		self.reportError("probe gate", err)
		return
	}
	self.settings.ProbeAttestationHandler(attestation)
}

// The gate (DESIGNNOTES4.md §3). The claim must echo this probe's nonce and
// client id, name this extender's key, carry a signature of the right size --
// the operator verifies it, this extender has no provider key to -- and be
// no lower than the observed interval less the tolerance. A provider cannot
// claim to be closer than it was seen to be.
func (self *ExtenderServer) gateProbeAttestation(
	probe *extenderProbe,
	attestation *protocol.ExtenderProbeAttestation,
	observed time.Duration,
) error {
	if subtle.ConstantTimeCompare(attestation.ProbeNonce, probe.nonce) != 1 {
		return fmt.Errorf("the attestation does not echo the nonce")
	}
	if !slices.Equal(attestation.ProbeClientId, probe.clientId) {
		return fmt.Errorf("the attestation names another client id")
	}
	if !slices.Equal(attestation.ExtenderPublicKey, self.PublicKey()) {
		return fmt.Errorf("the attestation names another extender")
	}
	if len(attestation.Signature) != ed25519.SignatureSize {
		return fmt.Errorf("the attestation signature is %d bytes, expected %d", len(attestation.Signature), ed25519.SignatureSize)
	}
	claimed := time.Duration(attestation.RttMs) * time.Millisecond
	if claimed+self.settings.ProbeRttTolerance < observed {
		return fmt.Errorf("the claimed rtt %s is below the observed %s", claimed, observed)
	}
	return nil
}
