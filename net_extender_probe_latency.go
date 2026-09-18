package connect

import (
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

	"github.com/urnetwork/connect/protocol"
)

// The latency probe (DESIGNNOTES4.md).
//
// A probe is an ordinary extender request with the probe service: it rides
// the same outer tls and the same `POST /` as a forward, so an observer sees
// nothing a forward does not show. The response is the ordinary one, and the
// client's rtt is the interval between writing the request and reading the
// response over the established carrier -- one round trip plus the extender's
// handling of the header.
//
// Any client may probe to RANK. Only a PROVIDER attests: it puts its client
// id in the header, the extender answers with a fresh nonce, and the provider
// sends back one frame carrying its rtt under its own client key signature.
// The extender measures the interval from its response to that frame and
// forwards the attestation to the operator only when the claim is at least
// that interval less a tolerance. Neither side alone controls the number: the
// provider cannot claim less than the extender saw, and the extender cannot
// change what the provider signed.
//
// A consumer client never identifies itself to an extender, which is the one
// party that sees its address (THREAT-MODEL §4). The attestor is installed by
// the provider role and by nothing else.

// Domain separator of the attestation signature (DESIGNNOTES4.md §2).
const ExtenderProbeSignatureDomain = "ur-extender-probe-v1"

// The nonce an extender issues to an attesting provider: fresh random bytes
// per probe, held only for the life of that stream.
const ExtenderProbeNonceByteCount = 32

// The client id an attesting provider sends: the 16 bytes of an Id.
const ExtenderProbeClientIdByteCount = 16

// ExtenderProbeAttestor is what a PROVIDER brings to a probe: its client id
// and its client key signature. A ranking client has none.
type ExtenderProbeAttestor struct {
	ClientId Id
	// Sign is the client key signature over the given bytes, which is
	// ClientKeyManager.Sign on a provider.
	Sign func(data []byte) []byte
}

// The outcome of one probe.
type ExtenderLatencyProbe struct {
	// The measured round trip.
	Rtt time.Duration
	// The extender's response, which names its identity key and carriers.
	Response *protocol.ExtenderResponse
	// Whether an attestation was sent. False for a ranking probe, and for an
	// attesting probe of an extender that issued no nonce, which one without
	// an identity or without a report path does.
	Attested bool
	// Why an attestation that was asked for was not sent, nil otherwise. The
	// measurement stands either way.
	AttestErr error
}

// ProbeExtenderLatency measures one round trip to one extender carrier and,
// with an attestor, attests it (DESIGNNOTES4.md §3).
//
// extenderConfig names the carrier exactly as a feed dial does; its PublicKey,
// when set, is required on the outer leaf and in the response. The stream is
// closed before returning; nothing is forwarded over it.
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

	roundTrip := &ExtenderRoundTrip{}
	extenderDial := &ExtenderDial{
		Service:   ExtenderServiceProbe,
		RoundTrip: roundTrip,
	}
	if attestor != nil {
		extenderDial.ProbeClientId = attestor.ClientId.Bytes()
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
		Rtt:      rtt,
		Response: response,
	}
	if attestor == nil || len(response.ProbeNonce) == 0 {
		return probe, nil
	}

	// the extender issued a nonce, so it will read exactly one frame and
	// judge the claim against what it observed
	attestation := &protocol.ExtenderProbeAttestation{
		ProbeClientId:     attestor.ClientId.Bytes(),
		ExtenderPublicKey: slices.Clone(response.PublicKey),
		ProbeNonce:        slices.Clone(response.ProbeNonce),
		RttMs:             extenderProbeRttMs(rtt),
		TimestampMs:       uint64(time.Now().UnixMilli()),
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
	// The extender closes the stream once it has read the frame. Waiting for
	// that keeps this side's close from overtaking the frame: on the quic
	// carriers a connection close discards what the peer has not consumed
	// yet, and the frame would be lost exactly when the extender is busy.
	// The wait is bounded by the probe budget; what the extender decided is
	// never sent back, so the outcome of the read does not matter.
	withConnReadPhaseDeadline(probeCtx, conn, connectSettings.ConnectTimeout, func() error {
		_, err := io.Copy(io.Discard, io.LimitReader(conn, 1))
		return err
	})
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
// provider.
func NewExtenderProbeNonce() ([]byte, error) {
	nonce := make([]byte, ExtenderProbeNonceByteCount)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return nonce, nil
}

// ExtenderProbeAttestationSigningBytes is what the provider signs: the domain
// separator and every field but the signature, each of fixed width, so the
// encoding is unambiguous without depending on how the message serializes. A
// field of the wrong size is refused here, on both the signing and the
// verifying side.
func ExtenderProbeAttestationSigningBytes(attestation *protocol.ExtenderProbeAttestation) ([]byte, error) {
	if attestation == nil {
		return nil, fmt.Errorf("extender probe attestation is missing")
	}
	if len(attestation.ProbeClientId) != ExtenderProbeClientIdByteCount {
		return nil, fmt.Errorf(
			"extender probe client id is %d bytes, expected %d",
			len(attestation.ProbeClientId),
			ExtenderProbeClientIdByteCount,
		)
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
		len(ExtenderProbeSignatureDomain)+ExtenderProbeClientIdByteCount+ed25519.PublicKeySize+ExtenderProbeNonceByteCount+4+8,
	)
	signingBytes = append(signingBytes, []byte(ExtenderProbeSignatureDomain)...)
	signingBytes = append(signingBytes, attestation.ProbeClientId...)
	signingBytes = append(signingBytes, attestation.ExtenderPublicKey...)
	signingBytes = append(signingBytes, attestation.ProbeNonce...)
	signingBytes = binary.BigEndian.AppendUint32(signingBytes, attestation.RttMs)
	signingBytes = binary.BigEndian.AppendUint64(signingBytes, attestation.TimestampMs)
	return signingBytes, nil
}

// SignExtenderProbeAttestation fills the signature under the attestor's key.
func SignExtenderProbeAttestation(
	attestor *ExtenderProbeAttestor,
	attestation *protocol.ExtenderProbeAttestation,
) error {
	if attestor == nil || attestor.Sign == nil {
		return fmt.Errorf("extender probe attestation has no attestor")
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

// VerifyExtenderProbeAttestation checks the signature under the provider's
// client key. Any malformed input verifies as false.
func VerifyExtenderProbeAttestation(
	providerPublicKey ed25519.PublicKey,
	attestation *protocol.ExtenderProbeAttestation,
) bool {
	signingBytes, err := ExtenderProbeAttestationSigningBytes(attestation)
	if err != nil {
		return false
	}
	return VerifyClientKeySignature(providerPublicKey, signingBytes, attestation.Signature)
}

// The attestation frame: the 4-byte big-endian length prefix every extender
// frame uses, bounded by the header ceiling.
func ExtenderProbeAttestationFrame(attestation *protocol.ExtenderProbeAttestation) ([]byte, error) {
	attestationBytes, err := proto.Marshal(attestation)
	if err != nil {
		return nil, err
	}
	if ExtenderMaxHeaderByteCount < len(attestationBytes) {
		return nil, fmt.Errorf(
			"extender probe attestation is %d bytes, at most %d",
			len(attestationBytes),
			ExtenderMaxHeaderByteCount,
		)
	}
	frameBytes := make([]byte, 4+len(attestationBytes))
	binary.BigEndian.PutUint32(frameBytes[0:4], uint32(len(attestationBytes)))
	copy(frameBytes[4:], attestationBytes)
	return frameBytes, nil
}

// Reads exactly one attestation frame.
func ReadExtenderProbeAttestationFrame(reader io.Reader) (*protocol.ExtenderProbeAttestation, error) {
	lengthBytes := make([]byte, 4)
	if _, err := io.ReadFull(reader, lengthBytes); err != nil {
		return nil, err
	}
	attestationByteCount := int(binary.BigEndian.Uint32(lengthBytes))
	if attestationByteCount == 0 || ExtenderMaxHeaderByteCount < attestationByteCount {
		return nil, fmt.Errorf(
			"extender probe attestation is %d bytes, at most %d",
			attestationByteCount,
			ExtenderMaxHeaderByteCount,
		)
	}
	attestationBytes := make([]byte, attestationByteCount)
	if _, err := io.ReadFull(reader, attestationBytes); err != nil {
		return nil, err
	}
	attestation := &protocol.ExtenderProbeAttestation{}
	if err := proto.Unmarshal(attestationBytes, attestation); err != nil {
		return nil, err
	}
	return attestation, nil
}
