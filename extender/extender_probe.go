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

// The latency probe service (DESIGNNOTES4.md, GEOMAP §2).
//
// A probe is answered exactly like any other request -- the same response
// frame, with a nonce added for an attesting pinger -- and then the stream is
// read for at most one attestation frame, answered with one verdict frame and
// closed. The extender's part is the gate: it timestamps its response,
// timestamps the frame, and accepts the pinger's signed claim only when the
// claim is at least the interval it observed, less a tolerance. It cannot
// alter the number, which is under the pinger's signature. It can accept,
// with its own co-signature over the exact claim, or refuse, with a reason,
// and either way the verdict goes back to the pinger, who reports it
// (GEOMAP §2.5). Nothing leaves this extender for the operator from here.
//
// A provider pinger is checked for the shape of its signature only: this
// extender holds no provider keys, and the operator verifies it. An extender
// pinger is checked in full, since its key is its identity: the signature
// under that key, and that the key is an active record this extender knows,
// which the operator's root key vouched for (GEOMAP §2.4).
//
// The nonce is fresh random bytes per probe, held on the stack for the life
// of the stream, so there is no table to fill and nothing to replay across
// streams. A probe over the per-source rate is refused before any of this,
// because a probe is cheap by design and would otherwise be a lever.

// The map cap of the probe limiter, the same as the dns forwarder's.
const extenderProbeMaxSourceCount = 4096

// The state of one probe request between the header and the stream.
type extenderProbe struct {
	// the attesting provider's client id, nil unless a provider asked
	clientId []byte
	// the attesting extender's identity key, nil unless an extender asked
	pingerPublicKey []byte
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

// Admits one probe request, whether this extender answers it or relays it to
// the end of its chain (GEOMAP §2.9): the per-source rate, and a header that
// names at most one pinger, since exactly one identity signs a claim.
func (self *ExtenderServer) admitProbe(header *protocol.ExtenderHeader, remoteAddress string) error {
	source := connectionSourceAddress(remoteAddress)
	if !self.probeLimiter.admit(
		source,
		time.Now(),
		self.settings.ProbeMaxRatePerSource,
		self.settings.ProbeMaxBurstPerSource,
	) {
		return fmt.Errorf("%s is over its probe rate", source)
	}
	if 0 < len(header.ProbeClientId) && 0 < len(header.ProbeExtenderPublicKey) {
		return fmt.Errorf("a probe names both a provider and an extender")
	}
	return nil
}

// Admits one probe request this extender answers itself and decides whether
// it gets a nonce (DESIGNNOTES4.md §2, GEOMAP §2.2). A nonce is issued when
// the header names one well formed pinger -- a 16 byte client id or a 32 byte
// extender key -- and this extender has an identity to bind the claim to.
// Every claim that follows is answered with a verdict, so a pinger never signs
// for nothing.
func (self *ExtenderServer) beginProbe(header *protocol.ExtenderHeader, remoteAddress string) (*extenderProbe, error) {
	if err := self.admitProbe(header, remoteAddress); err != nil {
		return nil, err
	}
	probe := &extenderProbe{}
	if len(self.PublicKey()) != ed25519.PublicKeySize {
		return probe, nil
	}
	switch {
	case len(header.ProbeClientId) == connect.ExtenderProbeClientIdByteCount:
		probe.clientId = slices.Clone(header.ProbeClientId)
	case len(header.ProbeExtenderPublicKey) == ed25519.PublicKeySize:
		probe.pingerPublicKey = slices.Clone(header.ProbeExtenderPublicKey)
	default:
		// no pinger, or one of the wrong size: a ranking probe
		return probe, nil
	}
	nonce, err := connect.NewExtenderProbeNonce()
	if err != nil {
		return nil, err
	}
	probe.nonce = nonce
	return probe, nil
}

// Serves the stream after the response of a probe: reads the one attestation
// frame an attesting pinger sends, judges it against the interval observed
// and answers with the verdict (DESIGNNOTES4.md §3, GEOMAP §2.3). A ranking
// probe has no nonce, and its stream simply closes. The response has been
// flushed when this is called, so the interval starts here.
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
	// the observation ends at the frame, before any of the judging below
	// can add to it
	observed := time.Since(responseTime)
	verdict := self.judgeProbeAttestation(probe, attestation, observed)

	// Every claim is answered, accepted or refused, so the pinger can tell a
	// refusal from a lost connection (GEOMAP §2.3). A provider built before
	// the verdict closes its side after the frame without reading, so a
	// failed write here is expected, costs nothing, and changes nothing.
	verdictFrameBytes, err := connect.ExtenderProbeVerdictFrame(verdict)
	if err != nil {
		self.reportError("probe verdict", err)
		return
	}
	if err := clientConn.SetWriteDeadline(time.Now().Add(self.settings.ProbeAttestationTimeout)); err != nil {
		self.reportError("probe verdict", err)
		return
	}
	if _, err := clientConn.Write(verdictFrameBytes); err != nil {
		self.reportError("probe verdict", err)
	}
}

// Judges one claim into its verdict: the gate, and on acceptance this
// extender's co-signature over the exact claim, the pinger's signature
// included.
func (self *ExtenderServer) judgeProbeAttestation(
	probe *extenderProbe,
	attestation *protocol.ExtenderProbeAttestation,
	observed time.Duration,
) *protocol.ExtenderProbeVerdict {
	reason, err := self.gateProbeAttestation(probe, attestation, observed)
	if err != nil {
		self.reportError("probe gate", err)
		return &protocol.ExtenderProbeVerdict{
			Reason: reason,
		}
	}
	cosignature, err := connect.SignExtenderProbeVerdict(self.SignProbeCosign, attestation)
	if err != nil {
		// The gate has checked every field the co-signature commits to, and
		// no nonce is issued without an identity, so this cannot happen short
		// of a broken identity. The empty verdict it leaves is refused by the
		// frame, and the pinger records no verdict rather than a refusal the
		// extender never made.
		self.reportError("probe cosign", err)
		return &protocol.ExtenderProbeVerdict{}
	}
	return &protocol.ExtenderProbeVerdict{
		Accepted:    true,
		Reason:      connect.ExtenderProbeVerdictReasonOk,
		Cosignature: cosignature,
	}
}

// The gate (DESIGNNOTES4.md §3, GEOMAP §2.4), in order, each refusal with its
// verdict reason. The claim must echo this probe's nonce (nonce) and name this
// extender's key (wrong extender); it must name the pinger the header named
// and no other identity (unknown pinger). An extender pinger's signature must
// verify under its key (bad signature), and the key must be an active record
// this extender knows and not its own -- with no verifier, no extender is
// known (unknown pinger). A provider's signature need only have the right
// size (bad signature): the operator verifies it, and this extender holds no
// provider key to. Last, the claim must be no lower than the observed
// interval less the tolerance (rtt below observed): a pinger cannot claim to
// be closer than it was seen to be.
func (self *ExtenderServer) gateProbeAttestation(
	probe *extenderProbe,
	attestation *protocol.ExtenderProbeAttestation,
	observed time.Duration,
) (uint32, error) {
	if subtle.ConstantTimeCompare(attestation.ProbeNonce, probe.nonce) != 1 {
		return connect.ExtenderProbeVerdictReasonNonce, fmt.Errorf("the attestation does not echo the nonce")
	}
	if !slices.Equal(attestation.ExtenderPublicKey, self.PublicKey()) {
		return connect.ExtenderProbeVerdictReasonWrongExtender, fmt.Errorf("the attestation names another extender")
	}
	switch {
	case probe.clientId != nil:
		if !slices.Equal(attestation.ProbeClientId, probe.clientId) || 0 < len(attestation.PingerExtenderPublicKey) {
			return connect.ExtenderProbeVerdictReasonUnknownPinger, fmt.Errorf("the attestation names another client id")
		}
		if len(attestation.Signature) != ed25519.SignatureSize {
			return connect.ExtenderProbeVerdictReasonBadSignature, fmt.Errorf(
				"the attestation signature is %d bytes, expected %d",
				len(attestation.Signature),
				ed25519.SignatureSize,
			)
		}
	case probe.pingerPublicKey != nil:
		if !slices.Equal(attestation.PingerExtenderPublicKey, probe.pingerPublicKey) || 0 < len(attestation.ProbeClientId) {
			return connect.ExtenderProbeVerdictReasonUnknownPinger, fmt.Errorf("the attestation names another pinger extender")
		}
		if !connect.VerifyExtenderProbeAttestation(probe.pingerPublicKey, attestation) {
			return connect.ExtenderProbeVerdictReasonBadSignature, fmt.Errorf("the attestation signature does not verify under the pinger key")
		}
		if slices.Equal(probe.pingerPublicKey, self.PublicKey()) {
			return connect.ExtenderProbeVerdictReasonUnknownPinger, fmt.Errorf("the pinger extender is this extender")
		}
		if self.settings.ProbePeerVerifier == nil || !self.settings.ProbePeerVerifier(probe.pingerPublicKey) {
			return connect.ExtenderProbeVerdictReasonUnknownPinger, fmt.Errorf("the pinger extender is not an active peer")
		}
	default:
		return connect.ExtenderProbeVerdictReasonUnknownPinger, fmt.Errorf("the probe named no pinger")
	}
	claimed := time.Duration(attestation.RttMs) * time.Millisecond
	if claimed+self.settings.ProbeRttTolerance < observed {
		return connect.ExtenderProbeVerdictReasonRttBelowObserved, fmt.Errorf(
			"the claimed rtt %s is below the observed %s",
			claimed,
			observed,
		)
	}
	return connect.ExtenderProbeVerdictReasonOk, nil
}
