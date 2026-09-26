package connect

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"slices"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/v2026/protocol"
)

// The latency probe (DESIGNNOTES4.md, GEOMAP §2).
//
// A probe is an ordinary extender request with the probe service: it rides
// the same outer tls and the same `POST /` as a forward, so an observer sees
// nothing a forward does not show. The response is the ordinary one, and the
// client's rtt is the interval between writing the request and reading the
// response over the established carrier -- one round trip plus the extender's
// handling of the header.
//
// Any client may probe to RANK. A PROVIDER or an EXTENDER may also ATTEST: it
// names itself in the header -- a provider by its client id, an extender by
// its identity key -- the target answers with a fresh nonce, and the pinger
// sends back one frame carrying its rtt under its own signature. The target
// measures the interval from its response to that frame, accepts the claim
// only when it is at least that interval less a tolerance, and answers every
// claim with one verdict frame: a refusal with its reason, or an acceptance
// with the target's co-signature over the exact claim. The pinger keeps the
// pair and reports it (GEOMAP §2.5). Neither side alone controls the number:
// the pinger cannot claim less than the target saw, and the target cannot
// change what the pinger signed -- it can only refuse to vouch for it, and
// the refusal is itself on the record.
//
// A consumer client never identifies itself to an extender, which is the one
// party that sees its address (THREAT-MODEL §4). The provider attestor is
// installed by the provider role and by nothing else; the extender attestor
// by an extender's own peer pinger.

// The signature domains (GEOMAP §2.2, §2.3). The two pinger kinds sign under
// different domains, so a provider claim can never be read as an extender
// claim or the reverse, and the provider format already in the field is
// unchanged. The target co-signs under a third.
const (
	// what a provider signs, with its client id
	ExtenderProbeSignatureDomain = "ur-extender-probe-v1"
	// what an extender pinger signs, with its identity key in the place of
	// the client id
	ExtenderPeerProbeSignatureDomain = "ur-extender-peer-probe-v1"
	// what a target signs over a claim it accepted
	ExtenderProbeCosignDomain = "ur-extender-probe-cosign-v1"
)

// The nonce an extender issues to an attesting pinger: fresh random bytes
// per probe, held only for the life of that stream.
const ExtenderProbeNonceByteCount = 32

// The client id an attesting provider sends: the 16 bytes of an Id.
const ExtenderProbeClientIdByteCount = 16

// Which kind of pinger an attestor or an attestation names (GEOMAP §2.2). The
// values are the report's `pinger_kind`.
type ExtenderPingerKind string

const (
	ExtenderPingerKindProvider ExtenderPingerKind = "provider"
	ExtenderPingerKindExtender ExtenderPingerKind = "extender"
)

// The reasons of a verdict (GEOMAP §2.3). Wire values.
const (
	ExtenderProbeVerdictReasonOk               uint32 = 0
	ExtenderProbeVerdictReasonRttBelowObserved uint32 = 1
	ExtenderProbeVerdictReasonNonce            uint32 = 2
	ExtenderProbeVerdictReasonWrongExtender    uint32 = 3
	ExtenderProbeVerdictReasonUnknownPinger    uint32 = 4
	ExtenderProbeVerdictReasonBadSignature     uint32 = 5
	// Reserved for a target that admits a probe but not its attestation. The
	// extender in this module never sends it: a probe over its rate is
	// refused with the 403 every refusal is, before any nonce is issued.
	ExtenderProbeVerdictReasonRateLimited uint32 = 6
)

// What a pinger records of one probe (GEOMAP §2.3). The three named outcomes
// are the report's `outcome`.
type ExtenderPingOutcome string

const (
	// No attestation was made: a ranking probe, a target without an identity
	// or one that predates the kind of pinger asking, since neither issues a
	// nonce. Nothing is reported for it.
	ExtenderPingUnattested ExtenderPingOutcome = ""
	// The target accepted, and its co-signature verifies under its key.
	ExtenderPingCosigned ExtenderPingOutcome = "cosigned"
	// The target refused, with its reason. An acceptance whose co-signature
	// does not verify is recorded here too: it is not an acceptance the
	// pinger can show anyone, and a target must not escape its refusal rate
	// by accepting with a signature nobody can check. Its reason is the 0 the
	// target sent, which no honest refusal carries.
	ExtenderPingRejected ExtenderPingOutcome = "rejected"
	// No verdict arrived: a close, a timeout, an unreadable frame, or a
	// target that predates the verdict. Kept apart from a refusal so a flaky
	// path is not read as a refusing extender.
	ExtenderPingUnknown ExtenderPingOutcome = "unknown"
)

// ExtenderProbeAttestor is what an attesting pinger brings to a probe:
// exactly one identity -- a PROVIDER's client id or an EXTENDER's identity
// key -- and that identity's signature. A ranking client has none. Build one
// with NewExtenderProbeProviderAttestor or NewExtenderProbeExtenderAttestor.
type ExtenderProbeAttestor struct {
	// The provider's client id, zero for an extender.
	ClientId Id
	// The extender's 32 byte identity key, empty for a provider.
	ExtenderPublicKey []byte
	// Sign is the identity's signature over the given bytes:
	// ClientKeyManager.Sign on a provider, the identity key on an extender.
	Sign func(data []byte) []byte
}

// The attestor of a provider, which signs with its client key.
func NewExtenderProbeProviderAttestor(clientId Id, sign func(data []byte) []byte) *ExtenderProbeAttestor {
	return &ExtenderProbeAttestor{
		ClientId: clientId,
		Sign:     sign,
	}
}

// The attestor of an extender, which signs with its identity key -- the same
// key that signs its challenge responses and that its record publishes.
func NewExtenderProbeExtenderAttestor(publicKey []byte, sign func(data []byte) []byte) *ExtenderProbeAttestor {
	return &ExtenderProbeAttestor{
		ExtenderPublicKey: slices.Clone(publicKey),
		Sign:              sign,
	}
}

// The Sign of an extender attestor over its identity key. The same key signs the extender's certificate authority, its
// challenge responses and its co-signatures, so this signs the peer probe
// signing bytes and nothing else: anything not under that domain gets no
// signature, which fails the attestation rather than the key.
func NewExtenderPeerProbeSigner(privateKey ed25519.PrivateKey) func(data []byte) []byte {
	return func(data []byte) []byte {
		if len(privateKey) != ed25519.PrivateKeySize || !bytes.HasPrefix(data, []byte(ExtenderPeerProbeSignatureDomain)) {
			return nil
		}
		return ed25519.Sign(privateKey, data)
	}
}

// The pinger the attestor identifies, empty when it names both identities,
// neither, or a key of the wrong size: no probe carries such an attestor.
func (self *ExtenderProbeAttestor) Kind() ExtenderPingerKind {
	if self == nil {
		return ""
	}
	hasClientId := self.ClientId != (Id{})
	hasPublicKey := 0 < len(self.ExtenderPublicKey)
	switch {
	case hasClientId && !hasPublicKey:
		return ExtenderPingerKindProvider
	case !hasClientId && len(self.ExtenderPublicKey) == ed25519.PublicKeySize:
		return ExtenderPingerKindExtender
	default:
		return ""
	}
}

// The outcome of one probe.
type ExtenderLatencyProbe struct {
	// The measured round trip. It stands whatever became of the attestation.
	Rtt time.Duration
	// The extender's response, which names its identity key and carriers.
	Response *protocol.ExtenderResponse
	// Whether an attestation was sent. False for a ranking probe, and for an
	// attesting probe of an extender that issued no nonce.
	Attested bool
	// Why an attestation that was asked for was not sent, nil otherwise. The
	// measurement stands either way.
	AttestErr error
	// The signed claim that was sent, nil unless Attested. It is what the
	// pinger reports.
	Attestation *protocol.ExtenderProbeAttestation
	// The target's verdict, nil when none was read.
	Verdict *protocol.ExtenderProbeVerdict
	// Why no verdict was read, nil otherwise.
	VerdictErr error
	// Whether the verdict accepts and its co-signature verifies under the
	// target's key.
	Cosigned bool
	// What the pinger records: cosigned, rejected or unknown once attested,
	// unattested otherwise.
	Outcome ExtenderPingOutcome
	// The verdict's reason, 0 without a verdict.
	Reason uint32
	// How many extenders the probe crossed before the one that answered it
	// (GEOMAP §2.9): 0 for a direct probe. A probe answered for by a chain
	// end other than the extender dialed crossed at least that one, so it is
	// never below 1 then, whatever the response says.
	HopCount uint32
	// The identity key of the chain end that answered a relayed probe, which
	// is the key the claim names and the co-signature is under; empty for a
	// direct probe.
	ChainEndPublicKey []byte
}

// ProbeExtenderLatency measures one round trip to one extender carrier and,
// with an attestor, attests it and reads the target's verdict (DESIGNNOTES4.md
// §3, GEOMAP §2.3).
//
// extenderConfig names the carrier exactly as a feed dial does; its PublicKey,
// when set, is required on the outer leaf and in the response, and is the key
// the co-signature must verify under -- unless the extender dialed is an
// NLayer front that relayed the probe, when the claim names and the verdict
// is verified under the chain end's key the response carries (GEOMAP §2.9).
// The round trip is header to response either way, which through a chain is
// the whole chain. The stream is closed before returning; nothing is
// forwarded over it.
func ProbeExtenderLatency(
	ctx context.Context,
	connectSettings *ConnectSettings,
	extenderConfig *ExtenderConfig,
	attestor *ExtenderProbeAttestor,
) (*ExtenderLatencyProbe, error) {
	if extenderConfig == nil || !extenderConfig.Ip.IsValid() {
		return nil, fmt.Errorf("extender address is not valid")
	}

	probeCtx, probeCancel := probeContext(ctx, connectSettings)
	defer probeCancel()

	// an attestor that does not name exactly one identity is carried by no
	// probe; the probe still ranks, and says why it did not attest
	var attestErr error
	pingerKind := attestor.Kind()
	if attestor != nil && pingerKind == "" {
		attestErr = fmt.Errorf("extender probe attestor must name exactly one identity")
		attestor = nil
	}

	roundTrip := &ExtenderRoundTrip{}
	extenderDial := &ExtenderDial{
		Service:   ExtenderServiceProbe,
		RoundTrip: roundTrip,
	}
	switch pingerKind {
	case ExtenderPingerKindProvider:
		extenderDial.ProbeClientId = attestor.ClientId.Bytes()
	case ExtenderPingerKindExtender:
		extenderDial.ProbeExtenderPublicKey = slices.Clone(attestor.ExtenderPublicKey)
	}
	conn, response, err := DialExtender(probeCtx, connectSettings, extenderConfig, extenderDial)
	if err != nil {
		// ownership transfers for every non-nil result, including a rejected one
		if conn != nil {
			conn.Close()
		}
		return nil, err
	}
	defer conn.Close()

	if response == nil {
		return nil, fmt.Errorf("extender sent no response")
	}
	if 0 < len(extenderConfig.PublicKey) && !slices.Equal(response.PublicKey, extenderConfig.PublicKey) {
		return nil, fmt.Errorf("extender published another identity key")
	}
	rtt := roundTrip.Rtt()
	if rtt <= 0 {
		return nil, fmt.Errorf("extender probe measured no round trip")
	}
	probe := &ExtenderLatencyProbe{
		Rtt:       rtt,
		Response:  response,
		AttestErr: attestErr,
		HopCount:  response.HopCount,
	}
	// The key the claim names and the verdict is verified under: the key the
	// outer leaf was checked against when the record named one, else the one
	// the response published -- or, for a probe an NLayer extender relayed,
	// the key of the chain end it names, which is the extender that issued
	// the nonce and judges the claim (GEOMAP §2.9). The front's own key is
	// still what the pin checked, above.
	targetPublicKey := extenderConfig.PublicKey
	if len(targetPublicKey) == 0 {
		targetPublicKey = response.PublicKey
	}
	if 0 < len(response.ChainEndPublicKey) && !bytes.Equal(response.ChainEndPublicKey, response.PublicKey) {
		probe.ChainEndPublicKey = slices.Clone(response.ChainEndPublicKey)
		targetPublicKey = probe.ChainEndPublicKey
		// a relay cannot pass for a direct ping by the count it copies back
		probe.HopCount = max(1, response.HopCount)
	}
	if attestor == nil || len(response.ProbeNonce) == 0 {
		return probe, nil
	}

	// the extender issued a nonce, so it will read exactly one frame, judge
	// the claim against what it observed, and answer with one verdict
	attestation := &protocol.ExtenderProbeAttestation{
		ExtenderPublicKey: slices.Clone(targetPublicKey),
		ProbeNonce:        slices.Clone(response.ProbeNonce),
		RttMs:             extenderProbeRttMs(rtt),
		TimestampMs:       uint64(time.Now().UnixMilli()),
	}
	switch pingerKind {
	case ExtenderPingerKindProvider:
		attestation.ProbeClientId = attestor.ClientId.Bytes()
	case ExtenderPingerKindExtender:
		attestation.PingerExtenderPublicKey = slices.Clone(attestor.ExtenderPublicKey)
	}
	if err := SignExtenderProbeAttestation(attestor, attestation); err != nil {
		probe.AttestErr = err
		return probe, nil
	}
	frameBytes, err := ExtenderProbeAttestationFrame(attestation)
	if err != nil {
		probe.AttestErr = err
		return probe, nil
	}
	if err := withConnWritePhaseDeadline(probeCtx, conn, connectSettings.ConnectTimeout, func() error {
		_, err := conn.Write(frameBytes)
		return err
	}); err != nil {
		probe.AttestErr = err
		return probe, nil
	}
	probe.Attested = true
	probe.Attestation = attestation

	// The target writes its verdict only once it has consumed the frame, so
	// reading it is also what keeps this side's close from overtaking the
	// frame: on the quic carriers a connection close discards what the peer
	// has not read yet. The wait is bounded by the probe budget, and however
	// it ends the measurement stands; only the outcome depends on it.
	var verdict *protocol.ExtenderProbeVerdict
	readErr := withConnReadPhaseDeadline(probeCtx, conn, connectSettings.ConnectTimeout, func() error {
		var err error
		verdict, err = ReadExtenderProbeVerdictFrame(conn)
		return err
	})
	if verdict == nil {
		if readErr == nil {
			readErr = fmt.Errorf("extender sent no verdict")
		}
		probe.VerdictErr = readErr
		probe.Outcome = ExtenderPingUnknown
		return probe, nil
	}
	probe.Verdict = verdict
	probe.Reason = verdict.Reason
	// the key the claim names, whose holder is the only one that can
	// co-sign it
	if VerifyExtenderProbeVerdict(targetPublicKey, attestation, verdict) {
		probe.Cosigned = true
		probe.Outcome = ExtenderPingCosigned
	} else {
		probe.Outcome = ExtenderPingRejected
	}
	return probe, nil
}

// The rtt as the attestation carries it: whole milliseconds, rounded UP, so
// rounding can never make a claim fall short of what the extender observed.
func extenderProbeRttMs(rtt time.Duration) uint32 {
	if rtt <= 0 {
		return 0
	}
	// the cap is checked before the round up, which would overflow near the
	// top of the range
	if time.Duration(math.MaxUint32)*time.Millisecond <= rtt {
		return math.MaxUint32
	}
	return uint32((rtt + time.Millisecond - 1) / time.Millisecond)
}

// NewExtenderProbeNonce draws the nonce an extender issues to an attesting
// pinger.
func NewExtenderProbeNonce() ([]byte, error) {
	nonce := make([]byte, ExtenderProbeNonceByteCount)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return nonce, nil
}

// The pinger an attestation names by which identity field is present: a
// provider with the client id alone, an extender with the pinger key alone.
// Empty with both or neither, which is malformed. Sizes are judged by the
// signing bytes, not here.
func ExtenderProbeAttestationPingerKind(attestation *protocol.ExtenderProbeAttestation) ExtenderPingerKind {
	if attestation == nil {
		return ""
	}
	hasClientId := 0 < len(attestation.ProbeClientId)
	hasPingerKey := 0 < len(attestation.PingerExtenderPublicKey)
	switch {
	case hasClientId && !hasPingerKey:
		return ExtenderPingerKindProvider
	case !hasClientId && hasPingerKey:
		return ExtenderPingerKindExtender
	default:
		return ""
	}
}

// ExtenderProbeAttestationSigningBytes is what the pinger signs: the domain of
// its kind and every field but the signature, each of fixed width, so the
// encoding is unambiguous without depending on how the message serializes
// (GEOMAP §2.2):
//
//	provider: "ur-extender-probe-v1"      || client id (16) || target key (32) || nonce (32) || rtt_ms (4 BE) || timestamp_ms (8 BE)
//	extender: "ur-extender-peer-probe-v1" || pinger key (32) || target key (32) || nonce (32) || rtt_ms (4 BE) || timestamp_ms (8 BE)
//
// An attestation that names both pinger identities or neither, or a field of
// the wrong size, is refused here, on both the signing and the verifying side.
func ExtenderProbeAttestationSigningBytes(attestation *protocol.ExtenderProbeAttestation) ([]byte, error) {
	if attestation == nil {
		return nil, fmt.Errorf("extender probe attestation is missing")
	}
	var domain string
	var pingerIdentity []byte
	switch ExtenderProbeAttestationPingerKind(attestation) {
	case ExtenderPingerKindProvider:
		if len(attestation.ProbeClientId) != ExtenderProbeClientIdByteCount {
			return nil, fmt.Errorf(
				"extender probe client id is %d bytes, expected %d",
				len(attestation.ProbeClientId),
				ExtenderProbeClientIdByteCount,
			)
		}
		domain = ExtenderProbeSignatureDomain
		pingerIdentity = attestation.ProbeClientId
	case ExtenderPingerKindExtender:
		if len(attestation.PingerExtenderPublicKey) != ed25519.PublicKeySize {
			return nil, fmt.Errorf(
				"extender probe pinger key is %d bytes, expected %d",
				len(attestation.PingerExtenderPublicKey),
				ed25519.PublicKeySize,
			)
		}
		domain = ExtenderPeerProbeSignatureDomain
		pingerIdentity = attestation.PingerExtenderPublicKey
	default:
		return nil, fmt.Errorf("extender probe attestation must name exactly one pinger")
	}
	if len(attestation.ExtenderPublicKey) != ed25519.PublicKeySize {
		return nil, fmt.Errorf(
			"extender probe public key is %d bytes, expected %d",
			len(attestation.ExtenderPublicKey),
			ed25519.PublicKeySize,
		)
	}
	if len(attestation.ProbeNonce) != ExtenderProbeNonceByteCount {
		return nil, fmt.Errorf(
			"extender probe nonce is %d bytes, expected %d",
			len(attestation.ProbeNonce),
			ExtenderProbeNonceByteCount,
		)
	}
	signingBytes := make(
		[]byte,
		0,
		len(domain)+len(pingerIdentity)+ed25519.PublicKeySize+ExtenderProbeNonceByteCount+4+8,
	)
	signingBytes = append(signingBytes, []byte(domain)...)
	signingBytes = append(signingBytes, pingerIdentity...)
	signingBytes = append(signingBytes, attestation.ExtenderPublicKey...)
	signingBytes = append(signingBytes, attestation.ProbeNonce...)
	signingBytes = binary.BigEndian.AppendUint32(signingBytes, attestation.RttMs)
	signingBytes = binary.BigEndian.AppendUint64(signingBytes, attestation.TimestampMs)
	return signingBytes, nil
}

// SignExtenderProbeAttestation fills the signature under the attestor's key.
// The attestation must name the attestor itself: a pinger never signs a claim
// for another identity.
func SignExtenderProbeAttestation(
	attestor *ExtenderProbeAttestor,
	attestation *protocol.ExtenderProbeAttestation,
) error {
	if attestor == nil || attestor.Sign == nil {
		return fmt.Errorf("extender probe attestation has no attestor")
	}
	switch attestor.Kind() {
	case ExtenderPingerKindProvider:
		if ExtenderProbeAttestationPingerKind(attestation) != ExtenderPingerKindProvider ||
			!bytes.Equal(attestation.ProbeClientId, attestor.ClientId.Bytes()) {
			return fmt.Errorf("extender probe attestation does not name the attesting provider")
		}
	case ExtenderPingerKindExtender:
		if ExtenderProbeAttestationPingerKind(attestation) != ExtenderPingerKindExtender ||
			!bytes.Equal(attestation.PingerExtenderPublicKey, attestor.ExtenderPublicKey) {
			return fmt.Errorf("extender probe attestation does not name the attesting extender")
		}
	default:
		return fmt.Errorf("extender probe attestor must name exactly one identity")
	}
	signingBytes, err := ExtenderProbeAttestationSigningBytes(attestation)
	if err != nil {
		return err
	}
	signature := attestor.Sign(signingBytes)
	if len(signature) != ed25519.SignatureSize {
		return fmt.Errorf(
			"extender probe attestor produced a %d byte signature, expected %d",
			len(signature),
			ed25519.SignatureSize,
		)
	}
	attestation.Signature = signature
	return nil
}

// VerifyExtenderProbeAttestation checks the pinger's signature: under the
// provider's client key for a provider claim, and under the named key for an
// extender claim, which must be the key given -- a claim is never verified
// under a key it does not name. Any malformed input verifies as false.
func VerifyExtenderProbeAttestation(
	pingerPublicKey ed25519.PublicKey,
	attestation *protocol.ExtenderProbeAttestation,
) bool {
	signingBytes, err := ExtenderProbeAttestationSigningBytes(attestation)
	if err != nil {
		return false
	}
	if ExtenderProbeAttestationPingerKind(attestation) == ExtenderPingerKindExtender &&
		!bytes.Equal(attestation.PingerExtenderPublicKey, pingerPublicKey) {
		return false
	}
	return VerifyClientKeySignature(pingerPublicKey, signingBytes, attestation.Signature)
}

// What a target co-signs over a claim it accepted (GEOMAP §2.3): the cosign domain, the claim's signing bytes with their own
// domain, and the pinger's signature. Committing to the signature as well as
// the fields makes the claim and the co-signature one object, so nobody can
// pair a co-signature with another number or with another signature over the
// same one.
func ExtenderProbeCosignBytes(attestation *protocol.ExtenderProbeAttestation) ([]byte, error) {
	signingBytes, err := ExtenderProbeAttestationSigningBytes(attestation)
	if err != nil {
		return nil, err
	}
	if len(attestation.Signature) != ed25519.SignatureSize {
		return nil, fmt.Errorf(
			"extender probe attestation signature is %d bytes, expected %d",
			len(attestation.Signature),
			ed25519.SignatureSize,
		)
	}
	cosignBytes := make([]byte, 0, len(ExtenderProbeCosignDomain)+len(signingBytes)+ed25519.SignatureSize)
	cosignBytes = append(cosignBytes, []byte(ExtenderProbeCosignDomain)...)
	cosignBytes = append(cosignBytes, signingBytes...)
	cosignBytes = append(cosignBytes, attestation.Signature...)
	return cosignBytes, nil
}

// A target's co-signature over a claim it accepted, by `sign`, which is the
// target's identity key.
func SignExtenderProbeVerdict(
	sign func(data []byte) []byte,
	attestation *protocol.ExtenderProbeAttestation,
) ([]byte, error) {
	if sign == nil {
		return nil, fmt.Errorf("extender probe verdict has no signer")
	}
	cosignBytes, err := ExtenderProbeCosignBytes(attestation)
	if err != nil {
		return nil, err
	}
	cosignature := sign(cosignBytes)
	if len(cosignature) != ed25519.SignatureSize {
		return nil, fmt.Errorf(
			"extender probe verdict signer produced a %d byte signature, expected %d",
			len(cosignature),
			ed25519.SignatureSize,
		)
	}
	return cosignature, nil
}

// Whether the verdict accepts the claim and carries the target's co-signature
// over exactly it. The key must be the one
// the claim names: a co-signature counts only from the extender the pinger
// measured. A refusal never verifies, whatever it carries, and any malformed
// input verifies as false.
func VerifyExtenderProbeVerdict(
	targetPublicKey ed25519.PublicKey,
	attestation *protocol.ExtenderProbeAttestation,
	verdict *protocol.ExtenderProbeVerdict,
) bool {
	if verdict == nil || !verdict.Accepted || attestation == nil {
		return false
	}
	if len(targetPublicKey) != ed25519.PublicKeySize ||
		!bytes.Equal(targetPublicKey, attestation.ExtenderPublicKey) {
		return false
	}
	cosignBytes, err := ExtenderProbeCosignBytes(attestation)
	if err != nil {
		return false
	}
	return VerifyClientKeySignature(targetPublicKey, cosignBytes, verdict.Cosignature)
}

// The attestation frame: the 4-byte big-endian length prefix every extender
// frame uses, bounded by the header ceiling.
func ExtenderProbeAttestationFrame(attestation *protocol.ExtenderProbeAttestation) ([]byte, error) {
	return extenderProbeFrame(attestation, "attestation")
}

// Reads exactly one attestation frame.
func ReadExtenderProbeAttestationFrame(reader io.Reader) (*protocol.ExtenderProbeAttestation, error) {
	attestation := &protocol.ExtenderProbeAttestation{}
	if err := readExtenderProbeFrame(reader, attestation, "attestation"); err != nil {
		return nil, err
	}
	return attestation, nil
}

// The verdict frame, framed exactly as the attestation is.
func ExtenderProbeVerdictFrame(verdict *protocol.ExtenderProbeVerdict) ([]byte, error) {
	return extenderProbeFrame(verdict, "verdict")
}

// Reads exactly one verdict frame. An empty frame is refused: no verdict the
// target sends is empty, since an acceptance carries its flag and a refusal
// its reason.
func ReadExtenderProbeVerdictFrame(reader io.Reader) (*protocol.ExtenderProbeVerdict, error) {
	verdict := &protocol.ExtenderProbeVerdict{}
	if err := readExtenderProbeFrame(reader, verdict, "verdict"); err != nil {
		return nil, err
	}
	return verdict, nil
}

// One probe frame of either kind: the length prefix, then the message. An
// empty message is refused on the way out exactly as on the way in, so a
// frame is never written that no reader accepts.
func extenderProbeFrame(message proto.Message, name string) ([]byte, error) {
	messageBytes, err := proto.Marshal(message)
	if err != nil {
		return nil, err
	}
	if len(messageBytes) == 0 {
		return nil, fmt.Errorf("extender probe %s is empty", name)
	}
	if ExtenderMaxHeaderByteCount < len(messageBytes) {
		return nil, fmt.Errorf(
			"extender probe %s is %d bytes, at most %d",
			name,
			len(messageBytes),
			ExtenderMaxHeaderByteCount,
		)
	}
	frameBytes := make([]byte, 4+len(messageBytes))
	binary.BigEndian.PutUint32(frameBytes[0:4], uint32(len(messageBytes)))
	copy(frameBytes[4:], messageBytes)
	return frameBytes, nil
}

// Reads one probe frame into the message. The length is checked against the
// ceiling before anything is allocated for it.
func readExtenderProbeFrame(reader io.Reader, message proto.Message, name string) error {
	lengthBytes := make([]byte, 4)
	if _, err := io.ReadFull(reader, lengthBytes); err != nil {
		return err
	}
	messageByteCount := int(binary.BigEndian.Uint32(lengthBytes))
	if messageByteCount == 0 || ExtenderMaxHeaderByteCount < messageByteCount {
		return fmt.Errorf(
			"extender probe %s is %d bytes, at most %d",
			name,
			messageByteCount,
			ExtenderMaxHeaderByteCount,
		)
	}
	messageBytes := make([]byte, messageByteCount)
	if _, err := io.ReadFull(reader, messageBytes); err != nil {
		return err
	}
	return proto.Unmarshal(messageBytes, message)
}
