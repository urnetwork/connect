package connect

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	mathrand "math/rand"
	"os"
	"sync"
	"testing"
	"time"

	// "google.golang.org/protobuf/proto"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/connect/protocol"
)

// encryptionMode selects how the SendSequence <-> ReceiveSequence TLS
// encryption is configured for a test. It is used to run the same scenario
// under both unencrypted and encrypted settings.
type encryptionMode int

const (
	encryptionModeOff encryptionMode = iota
	// encryptionModeOn: both sides Encrypt=true,
	// EncryptAllowUnwrappedFallback=false. The handshake must succeed for
	// any application data to flow (the SendSequence gates app packs on
	// session readiness).
	encryptionModeOn
	// encryptionModeOnAllowFallback: both sides Encrypt=true,
	// EncryptAllowUnwrappedFallback=true. The handshake is still expected
	// to succeed under normal conditions, but app packs are allowed to flow
	// in parallel during the handshake (gating off) and the sender will opt
	// out gracefully if the handshake fails.
	encryptionModeOnAllowFallback
	// encryptionModeFallback exercises the opt-out path: encryption is enabled
	// on the sender but the handshake is expected to time out, so the sender
	// falls back to plaintext.
	encryptionModeFallback
)

func TestSendReceiveSenderReset(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping testing in short mode")
	}
	runSendReceiveSenderReset(t, encryptionModeOff)
}

func TestSendReceiveSenderResetEncrypted(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping testing in short mode")
	}
	runSendReceiveSenderReset(t, encryptionModeOn)
}

func TestSendReceiveSenderResetEncryptedAllowFallback(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping testing in short mode")
	}
	runSendReceiveSenderReset(t, encryptionModeOnAllowFallback)
}

func runSendReceiveSenderReset(t *testing.T, encMode encryptionMode) {
	// in this case two senders with the same client_id send after each other
	// The receiver should be able to reset using the new sequence_id

	// timeout between receives or acks
	// receive timeout. Large enough to absorb -race instrumentation overhead,
	// which can slow per-message processing by 5-10x.
	timeout := 5 * time.Minute
	// number of messages
	n := 1024
	stress := os.Getenv("CONNECT_TRANSFER_STRESS") != ""
	if stress {
		n = 16 * 1024
	}

	contractCount := 1
	// random delay / loss; the encrypted scenarios use a tighter conditioner
	// because TLS records still need to be delivered eventually for the
	// session to complete the handshake.
	var conditionerDelay time.Duration
	var conditionerLoss float32
	switch encMode {
	case encryptionModeOff:
		if stress {
			conditionerDelay = 5 * time.Second
			conditionerLoss = 0.5
		} else {
			conditionerDelay = 200 * time.Millisecond
			conditionerLoss = 0.1
		}
	case encryptionModeOn, encryptionModeOnAllowFallback:
		if stress {
			conditionerDelay = 200 * time.Millisecond
			conditionerLoss = 0.1
		} else {
			conditionerDelay = 100 * time.Millisecond
			conditionerLoss = 0.05
		}
		contractCount = 2
	case encryptionModeFallback:
		// no loss/delay so the opt-out and follow-on plaintext frames flow;
		// the handshake is forced to fail by configuring a tiny timeout
		// in `applyTestEncryptionSettings`.
		conditionerDelay = 0
		conditionerLoss = 0
		contractCount = 2
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	aClientId := NewId()
	bClientId := NewId()

	aSend := make(chan []byte)
	bSend := make(chan []byte)

	aConditioner, bReceive := newConditioner(ctx, aSend)
	bConditioner, aReceive := newConditioner(ctx, bSend)

	aConditioner.update(func() {
		aConditioner.randomDelay = conditionerDelay
		aConditioner.lossProbability = conditionerLoss
	})

	bConditioner.update(func() {
		bConditioner.randomDelay = conditionerDelay
		bConditioner.lossProbability = conditionerLoss
	})

	aSendTransport := NewSendGatewayTransport()
	aReceiveTransport := NewReceiveGatewayTransport()

	bSendTransport := NewSendGatewayTransport()
	bReceiveTransport := NewReceiveGatewayTransport()

	provideModes := map[protocol.ProvideMode]bool{
		protocol.ProvideMode_Network: true,
	}

	clientSettingsA := DefaultClientSettings()
	clientSettingsA.SendBufferSettings.SequenceBufferSize = 0
	clientSettingsA.SendBufferSettings.AckBufferSize = 0
	clientSettingsA.SendBufferSettings.AckTimeout = 300 * time.Second
	clientSettingsA.SendBufferSettings.IdleTimeout = 300 * time.Second
	clientSettingsA.ReceiveBufferSettings.SequenceBufferSize = 0
	clientSettingsA.ReceiveBufferSettings.GapTimeout = 300 * time.Second
	clientSettingsA.ReceiveBufferSettings.IdleTimeout = 300 * time.Second
	// clientSettingsA.ReceiveBufferSettings.AckBufferSize = 0
	clientSettingsA.ForwardBufferSettings.SequenceBufferSize = 0
	clientSettingsA.ForwardBufferSettings.IdleTimeout = 300 * time.Second
	clientSettingsA.ContractManagerSettings.LegacyCreateContract = true
	applyTestEncryptionSettings(clientSettingsA, encMode)
	a := NewClient(ctx, aClientId, NewNoContractClientOob(), clientSettingsA)
	aRouteManager := a.RouteManager()
	aContractManager := a.ContractManager()
	// aRouteManager := NewRouteManager(a)
	// aContractManager := NewContractManagerWithDefaults(a)
	defer a.Cancel()
	// a.Setup(aRouteManager, aContractManager)
	// go a.Run()

	aRouteManager.UpdateTransport(aSendTransport, []Route{aSend})
	aRouteManager.UpdateTransport(aReceiveTransport, []Route{aReceive})

	aContractManager.SetProvideModes(provideModes)

	clientSettingsB := DefaultClientSettings()
	clientSettingsB.SendBufferSettings.SequenceBufferSize = 0
	clientSettingsB.SendBufferSettings.AckBufferSize = 0
	clientSettingsB.SendBufferSettings.AckTimeout = 300 * time.Second
	clientSettingsB.SendBufferSettings.IdleTimeout = 300 * time.Second
	clientSettingsB.ReceiveBufferSettings.SequenceBufferSize = 0
	clientSettingsB.ReceiveBufferSettings.GapTimeout = 300 * time.Second
	clientSettingsB.ReceiveBufferSettings.IdleTimeout = 300 * time.Second
	// clientSettingsB.ReceiveBufferSettings.AckBufferSize = 0
	clientSettingsB.ForwardBufferSettings.SequenceBufferSize = 0
	clientSettingsB.ForwardBufferSettings.IdleTimeout = 300 * time.Second
	clientSettingsB.ContractManagerSettings.LegacyCreateContract = true
	applyTestEncryptionSettings(clientSettingsB, encMode)
	b := NewClient(ctx, bClientId, NewNoContractClientOob(), clientSettingsB)
	bRouteManager := b.RouteManager()
	bContractManager := b.ContractManager()
	// bRouteManager := NewRouteManager(b)
	// bContractManager := NewContractManagerWithDefaults(b)
	defer b.Cancel()
	// b.Setup(bRouteManager, bContractManager)
	// go b.Run()

	bRouteManager.UpdateTransport(bSendTransport, []Route{bSend})
	bRouteManager.UpdateTransport(bReceiveTransport, []Route{bReceive})

	bContractManager.SetProvideModes(provideModes)

	acks := make(chan error)
	receives := make(chan *protocol.SimpleMessage)

	b.AddReceiveCallback(func(source TransferPath, frames []*protocol.Frame, peer Peer) {
		for _, frame := range frames {
			m, err := FromFrame(frame)
			if err != nil {
				panic(err)
			}
			switch v := m.(type) {
			case *protocol.SimpleMessage:
				receives <- v
			}
		}
	})

	var ackCount int
	var waitingAckCount int
	var receiveCount int
	var waitingReceiveCount int
	var receiveMessages map[string]bool

	for range contractCount {
		err := aContractManager.HandleControlFrame(
			ContractKey{
				Destination: DestinationId(bClientId),
			},
			requireContractResult(
				protocol.ProvideMode_Network,
				bContractManager.RequireProvideSecretKey(protocol.ProvideMode_Network),
				aClientId,
				bClientId,
			),
		)
		assert.Equal(t, err, nil)
	}
	// aReceive <- requireTransferFrameBytes(
	// 	requireContractResultInitialPack(
	// 		protocol.ProvideMode_Network,
	// 		bContractManager.RequireProvideSecretKey(protocol.ProvideMode_Network),
	// 		aClientId,
	// 		bClientId,
	// 	),
	// 	ControlId,
	// 	aClientId,
	// )

	go func() {
		for i := 0; i < n; i += 1 {
			message := &protocol.SimpleMessage{
				Content: fmt.Sprintf("hi %d", i),
			}
			frame, err := ToFrame(message, DefaultProtocolVersion)
			if err != nil {
				panic(err)
			}
			success := a.Send(frame, DestinationId(bClientId), func(err error) {
				acks <- err
			})
			assert.Equal(t, success, true)
		}
	}()

	ackCount = 0
	waitingAckCount = -1
	receiveCount = 0
	waitingReceiveCount = -1
	receiveMessages = map[string]bool{}
	for receiveCount < n || ackCount < n {
		if receiveCount < n && waitingReceiveCount < receiveCount {
			fmt.Printf("[0] waiting for %d/%d\n", receiveCount+1, n)
			waitingReceiveCount = receiveCount
		} else if ackCount < n && waitingAckCount < ackCount {
			fmt.Printf("[0] waiting for ack %d/%d\n", ackCount+1, n)
		}

		select {
		case <-ctx.Done():
			return
		case message := <-receives:
			receiveMessages[message.Content] = true
			assert.Equal(t, fmt.Sprintf("hi %d", receiveCount), message.Content)
			receiveCount += 1
		case err := <-acks:
			assert.Equal(t, err, nil)
			ackCount += 1
		case <-time.After(timeout):
			t.Fatal("Timeout.")
		}
	}
	for i := 0; i < n; i += 1 {
		message := fmt.Sprintf("hi %d", i)
		found := receiveMessages[message]
		assert.Equal(t, found, true)
	}

	assert.Equal(t, n, len(receiveMessages))
	assert.Equal(t, n, ackCount)

	a.Cancel()
	aRouteManager.RemoveTransport(aSendTransport)
	aRouteManager.RemoveTransport(aReceiveTransport)

	select {
	case <-time.After(1 * time.Second):
	}

	a2 := NewClient(ctx, aClientId, NewNoContractClientOob(), clientSettingsA)
	// a2 := NewClientWithDefaults(ctx, aClientId, NewNoContractClientOob())
	a2RouteManager := a2.RouteManager()
	a2ContractManager := a2.ContractManager()
	// a2RouteManager := NewRouteManager(a2)
	// a2ContractManager := NewContractManagerWithDefaults(a2)
	// a2.Setup(a2RouteManager, a2ContractManager)
	defer a2.Cancel()
	// go a2.Run()

	a2RouteManager.UpdateTransport(aSendTransport, []Route{aSend})
	a2RouteManager.UpdateTransport(aReceiveTransport, []Route{aReceive})

	a2ContractManager.SetProvideModes(provideModes)

	for range contractCount {
		err := a2ContractManager.HandleControlFrame(
			ContractKey{
				Destination: DestinationId(bClientId),
			},
			requireContractResult(
				protocol.ProvideMode_Network,
				bContractManager.RequireProvideSecretKey(protocol.ProvideMode_Network),
				aClientId,
				bClientId,
			),
		)
		assert.Equal(t, err, nil)
	}
	// aReceive <- requireTransferFrameBytes(
	// 	requireContractResultInitialPack(
	// 		protocol.ProvideMode_Network,
	// 		bContractManager.RequireProvideSecretKey(protocol.ProvideMode_Network),
	// 		aClientId,
	// 		bClientId,
	// 	),
	// 	ControlId,
	// 	aClientId,
	// )

	select {
	case message := <-receives:
		// an older message was delivered
		assert.Equal(t, message, nil)
	default:
	}

	go func() {
		for i := 0; i < n; i += 1 {
			message := &protocol.SimpleMessage{
				Content: fmt.Sprintf("hi %d", i),
			}
			frame, err := ToFrame(message, DefaultProtocolVersion)
			if err != nil {
				panic(err)
			}
			success := a2.Send(frame, DestinationId(bClientId), func(err error) {
				acks <- err
			})
			assert.Equal(t, success, true)
		}
	}()

	ackCount = 0
	waitingAckCount = -1
	receiveCount = 0
	waitingReceiveCount = -1
	receiveMessages = map[string]bool{}
	for receiveCount < n || ackCount < n {
		if receiveCount < n && waitingReceiveCount < receiveCount {
			fmt.Printf("[1] waiting for %d/%d\n", receiveCount+1, n)
			waitingReceiveCount = receiveCount
		} else if ackCount < n && waitingAckCount < ackCount {
			fmt.Printf("[1] waiting for ack %d/%d\n", ackCount+1, n)
		}

		select {
		case <-ctx.Done():
			return
		case message := <-receives:
			receiveMessages[message.Content] = true
			assert.Equal(t, fmt.Sprintf("hi %d", receiveCount), message.Content)
			receiveCount += 1
		case err := <-acks:
			assert.Equal(t, err, nil)
			ackCount += 1
		case <-time.After(timeout):
			t.Fatal("Timeout.")
		}
	}
	for i := 0; i < n; i += 1 {
		message := fmt.Sprintf("hi %d", i)
		found := receiveMessages[message]
		assert.Equal(t, found, true)
	}

	fmt.Printf("[2] done\n")

	assert.Equal(t, n, len(receiveMessages))
	assert.Equal(t, n, ackCount)

	a2.Cancel()
	b.Cancel()
	cancel()
}

func createContractResult(
	provideMode protocol.ProvideMode,
	provideSecretKey []byte,
	sourceId Id,
	destinationId Id,
) (*protocol.Frame, error) {
	contractId := NewId()
	contractByteCount := 8 * 1024 * 1024 * 1024

	storedContract := &protocol.StoredContract{
		ContractId:        contractId.Bytes(),
		TransferByteCount: uint64(contractByteCount),
		SourceId:          sourceId.Bytes(),
		DestinationId:     destinationId.Bytes(),
	}
	storedContractBytes, err := ProtoMarshal(storedContract)
	if err != nil {
		return nil, err
	}
	defer MessagePoolReturn(storedContractBytes)
	storedContractHmac := SignStoredContract(DefaultContractManagerSettings(), provideSecretKey, storedContractBytes)

	message := &protocol.CreateContractResult{
		Contract: &protocol.Contract{
			StoredContractBytes: storedContractBytes,
			StoredContractHmac:  storedContractHmac,
			ProvideMode:         provideMode,
		},
	}

	return ToFrame(message, DefaultProtocolVersion)
	// if err != nil {
	// 	return nil, err
	// }
	// defer MessagePoolReturn(frame.MessageBytes)

	// messageId := NewId()
	// sequenceId := NewId()
	// pack := &protocol.Pack{
	// 	MessageId:      messageId.Bytes(),
	// 	SequenceId:     sequenceId.Bytes(),
	// 	SequenceNumber: 0,
	// 	Head:           true,
	// 	Frames:         []*protocol.Frame{frame},
	// }

	// return ToFrame(pack, DefaultProtocolVersion)
}

func requireContractResult(
	provideMode protocol.ProvideMode,
	provideSecretKey []byte,
	sourceId Id,
	destinationId Id,
) *protocol.Frame {
	frame, err := createContractResult(provideMode, provideSecretKey, sourceId, destinationId)
	if err != nil {
		panic(err)
	}
	return frame
}

func createTransferFrameBytes(frame *protocol.Frame, sourceId Id, destinationId Id) ([]byte, error) {
	transferFrame := &protocol.TransferFrame{
		TransferPath: &protocol.TransferPath{
			SourceId:      sourceId.Bytes(),
			DestinationId: destinationId.Bytes(),
			// StreamId: DirectStreamId.Bytes(),
		},
		Frame: frame,
	}

	return ProtoMarshal(transferFrame)
}

func requireTransferFrameBytes(frame *protocol.Frame, sourceId Id, destinationId Id) []byte {
	b, err := createTransferFrameBytes(frame, sourceId, destinationId)
	if err != nil {
		panic(err)
	}

	var filteredTransferFrame protocol.FilteredTransferFrame
	if err := ProtoUnmarshal(b, &filteredTransferFrame); err != nil {
		panic(err)
	}
	sourceId_, err := IdFromBytes(filteredTransferFrame.TransferPath.SourceId)
	if err != nil {
		panic(err)
	}
	destinationId_, err := IdFromBytes(filteredTransferFrame.TransferPath.DestinationId)
	if err != nil {
		panic(err)
	}

	if sourceId != sourceId_ {
		panic(fmt.Errorf("%s <> %s", sourceId, sourceId_))
	}

	if destinationId != destinationId_ {
		panic(fmt.Errorf("%s <> %s", destinationId, destinationId_))
	}

	return b
}

type conditioner struct {
	ctx             context.Context
	fixedDelay      time.Duration
	randomDelay     time.Duration
	hold            bool
	inversionWindow time.Duration
	invertFraction  float32
	lossProbability float32
	monitor         *Monitor
	mutex           sync.Mutex
}

func newConditioner(ctx context.Context, in chan []byte) (*conditioner, chan []byte) {
	c := &conditioner{
		ctx:             ctx,
		fixedDelay:      0,
		randomDelay:     0,
		lossProbability: 0,
		monitor:         NewMonitor(),
	}
	out := make(chan []byte)
	go c.run(in, out)
	return c, out
}

func (self *conditioner) update(callback func()) {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	callback()
	self.monitor.NotifyAll()
}

func (self *conditioner) calcLoss() bool {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	return mathrand.Float32() < self.lossProbability
}

func (self *conditioner) calcDelay() time.Duration {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	delay := self.fixedDelay
	if 0 < self.randomDelay {
		delay += time.Duration(mathrand.Intn(int(self.randomDelay)))
	}
	return delay
}

func (self *conditioner) run(in chan []byte, out chan []byte) {
	// defer close(out)

	for {
		select {
		case <-self.ctx.Done():
			return
		case <-self.monitor.NotifyChannel():
			continue
		case b, ok := <-in:
			if !ok {
				return
			}

			if self.calcLoss() {
				continue
			}

			delay := self.calcDelay()

			if delay <= 0 {
				select {
				case <-self.ctx.Done():
					return
				case out <- b:
				}
			} else {
				go func() {
					select {
					case <-self.ctx.Done():
						return
					case <-time.After(delay):
					}

					select {
					case <-self.ctx.Done():
						return
					case out <- b:
					}
				}()
			}
		}
	}
}

// applyTestEncryptionSettings configures the per-client encryption settings.
// In the new design, encryption is a binary property of the per-peer
// `EncryptionSessionManager` session: cipher set → all traffic encrypted;
// cipher nil → all traffic plaintext. There is no per-frame fallback flag.
func applyTestEncryptionSettings(clientSettings *ClientSettings, encMode encryptionMode) {
	switch encMode {
	case encryptionModeOff:
		clientSettings.EncryptionSettings.Encrypt = false
	case encryptionModeOn, encryptionModeOnAllowFallback:
		clientSettings.EncryptionSettings.Encrypt = true
		clientSettings.EncryptionSettings.TlsTimeout = 60 * time.Second
	case encryptionModeFallback:
		// sender enables encryption with a tight timeout but the receiver
		// has encryption disabled, so the handshake never completes; the
		// session stays in the cipher-nil state and all traffic flows in
		// plaintext.
		clientSettings.EncryptionSettings.Encrypt = true
		clientSettings.EncryptionSettings.TlsTimeout = 50 * time.Millisecond
	}
}

// TestSendReceiveEncryptedFallback exercises the opt-out path. The sender's
// TLS handshake is forced to time out (lossy conditioner) and the receiver
// accepts plaintext fallback.
func TestSendReceiveEncryptedFallback(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping testing in short mode")
	}
	runSendReceiveSenderReset(t, encryptionModeFallback)
}

// TestVerifyPeerCertificateAgainstContract covers the sender-side TLS
// certificate verification against the contract's PEM commitment.
func TestVerifyPeerCertificateAgainstContract(t *testing.T) {
	cert, _ := generateSequenceTlsCertificate()
	leaf := cert.Leaf
	leafPem := pemEncodeCertificate(leaf.Raw)

	// no commitment in the contract -> verification is skipped, no error
	ok, err := verifyPeerCertificateAgainstContract([]*x509.Certificate{leaf}, nil)
	assert.Equal(t, err, nil)
	assert.Equal(t, ok, true)

	// matching commitment -> success
	ok, err = verifyPeerCertificateAgainstContract([]*x509.Certificate{leaf}, [][]byte{leafPem})
	assert.Equal(t, err, nil)
	assert.Equal(t, ok, true)

	// mismatched commitment -> failure
	otherCert, _ := generateSequenceTlsCertificate()
	otherLeafPem := pemEncodeCertificate(otherCert.Leaf.Raw)
	ok, err = verifyPeerCertificateAgainstContract([]*x509.Certificate{leaf}, [][]byte{otherLeafPem})
	assert.NotEqual(t, err, nil)
	assert.Equal(t, ok, false)

	// peer presented no cert but contract has a commitment -> failure
	ok, err = verifyPeerCertificateAgainstContract(nil, [][]byte{leafPem})
	assert.NotEqual(t, err, nil)
	assert.Equal(t, ok, false)
}

func pemEncodeCertificate(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// TestMinimumMessageLenLimitFitsWorstCaseHandshake verifies that
// `ClientSettings.MinimumMessageLenLimit()` is at least as large as
// the actual upper bound the per-peer encryption handshake can
// produce on the wire. The contract is: any framer / transport
// receive-cap configured to `>= MinimumMessageLenLimit()` must be
// able to deliver the largest single `EncryptedControl{Handshake}`
// Pack the runtime ever produces. If this invariant slips (e.g.,
// the post-quantum key share grows, or someone adds a field to the
// outer wraps), the runtime would silently deadlock the handshake.
//
// This test exercises just the math: it asserts the limit is
// generous enough to cover the documented worst-case in the
// method's comment block, with margin for ASN.1 size jitter and
// protobuf field-tag drift. It is intentionally a coarse-grained
// check; the integration tests under `server/connect` verify the
// end-to-end behavior.
func TestMinimumMessageLenLimitFitsWorstCaseHandshake(t *testing.T) {
	settings := DefaultClientSettings()
	limit := settings.MinimumMessageLenLimit()

	// Documented worst-case sizing from the comment on
	// `MinimumMessageLenLimit`: TLS 1.3 server flight with the
	// post-quantum hybrid key share + mTLS CertificateRequest +
	// ephemeral ECDSA P-256 cert is observed at ~1947 bytes. Round
	// to a conservative 2 KiB for "actual raw handshake bytes."
	const observedHandshakeRawBytes = ByteCount(2 * 1024)

	// Protobuf wrap overhead (EncryptedControl + Frame + Pack +
	// TransferFrame): documented at ~200 bytes, with ample slop.
	const protobufWrapOverhead = ByteCount(300)

	worstCaseWireBytes := observedHandshakeRawBytes + protobufWrapOverhead
	if limit < worstCaseWireBytes {
		t.Fatalf(
			"MinimumMessageLenLimit %d < worst-case handshake wire bytes %d (TLS %d + wrap %d)",
			limit, worstCaseWireBytes, observedHandshakeRawBytes, protobufWrapOverhead,
		)
	}

	// And the limit must not be absurdly large either — that would
	// indicate someone forgot to read the comment. A few MiB is
	// a sane upper bound for "this is a per-message handshake
	// payload cap."
	const sanityUpperBound = ByteCount(4 * 1024 * 1024)
	if sanityUpperBound < limit {
		t.Fatalf("MinimumMessageLenLimit %d > sanity upper bound %d; review the value", limit, sanityUpperBound)
	}
}

// FIXME TestAckTimeout
