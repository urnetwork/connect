package connect

// Sequence TLS certificate verification belongs to one handshake epoch: the
// epoch whose cipher seals the frame. A background rekey must have its own
// peer certificate checked against the contract commitment before its cipher
// seals, and a promotion that races a send must never let one epoch's check
// stand in for another epoch's. Every epoch here completes a real TLS 1.3
// handshake with the production sequence configs, so verification reads the
// certificate the handshake actually observed.

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/tls"
	"encoding/pem"
	"net"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Returns a fresh synthetic sequence certificate and its PEM block, the form
// a contract commits to.
func newCertEpochCertificate(t *testing.T) (tls.Certificate, []byte) {
	t.Helper()
	certificate, err := generateSequenceTlsCertificate()
	if err != nil {
		t.Fatal(err)
	}
	certificatePem := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Certificate[0]})
	return certificate, certificatePem
}

// Completes a TLS handshake over an in-memory pipe in which the peer presents
// peerCertificate, and returns the local client end. Its ConnectionState
// carries that certificate as the peer leaf, as a completed epoch's does.
func newCertEpochTlsConn(t *testing.T, peerCertificate tls.Certificate) *tls.Conn {
	t.Helper()
	localCertificate, _ := newCertEpochCertificate(t)
	clientConfig := DefaultSequenceClientTlsConfig()
	clientConfig.Certificates = []tls.Certificate{localCertificate}
	serverConfig, err := DefaultSequenceServerTlsConfig()
	if err != nil {
		t.Fatal(err)
	}
	serverConfig.Certificates = []tls.Certificate{peerCertificate}
	localConn, peerConn := net.Pipe()
	// closing the pipe rather than the TLS ends skips a close_notify that
	// nothing reads
	t.Cleanup(func() {
		localConn.Close()
		peerConn.Close()
	})
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	client := tls.Client(localConn, clientConfig)
	server := tls.Server(peerConn, serverConfig)
	serverErr := make(chan error, 1)
	go func() {
		serverErr <- server.HandshakeContext(ctx)
	}()
	if err := client.HandshakeContext(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
	return client
}

// Builds a handshake epoch whose completed TLS connection observed
// peerCertificate. Each epoch has its own AEAD key, so a sealed frame names
// the epoch that sealed it. A rekey epoch has finished TLS but is still
// waiting for its identity proof; any other epoch is identity-verified.
func newCertEpoch(t *testing.T, peerCertificate tls.Certificate, keyByte byte, rekey bool) *tlsHandshakeEpoch {
	t.Helper()
	block, err := aes.NewCipher(bytes.Repeat([]byte{keyByte}, 32))
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	e := &tlsHandshakeEpoch{
		ctx:                  ctx,
		cancel:               cancel,
		epochId:              NewId(),
		tlsConn:              newCertEpochTlsConn(t, peerCertificate),
		handshakeDone:        make(chan struct{}),
		establishmentDone:    make(chan struct{}),
		derivedTlsCipher:     &sequenceCipher{aead: aead},
		peerIdentityVerified: !rekey,
	}
	close(e.handshakeDone)
	if !rekey {
		close(e.establishmentDone)
	}
	return e
}

// Installs the trusted set and the epochs. established seals; current is the
// newest epoch, a rekey in flight when it differs from established.
func setCertEpochSessionForTest(
	session *peerEncryptionSession,
	trustedPems [][]byte,
	established *tlsHandshakeEpoch,
	current *tlsHandshakeEpoch,
) {
	session.stateLock.Lock()
	defer session.stateLock.Unlock()
	session.trustedPeerCertPems = map[string]bool{}
	for _, trustedPem := range trustedPems {
		session.trustedPeerCertPems[string(trustedPem)] = true
	}
	session.establishedEpoch = established
	session.epoch = current
}

// Completes a rekey as a verified identity proof does: e becomes the current
// and established epoch, and the epoch it replaces stays for decrypt only.
func promoteCertEpochForTest(session *peerEncryptionSession, e *tlsHandshakeEpoch) {
	session.stateLock.Lock()
	defer session.stateLock.Unlock()
	session.epoch = e
	e.peerIdentityVerified = true
	session.markEstablishedWithLock(e)
	if !isClosed(e.establishmentDone) {
		close(e.establishmentDone)
	}
}

// Reads an epoch's own verification under the session lock.
func certEpochVerified(session *peerEncryptionSession, e *tlsHandshakeEpoch) bool {
	session.stateLock.Lock()
	defer session.stateLock.Unlock()
	return e.certVerified
}

// Takes the frame a synchronous write committed to the route, if any, and
// returns the epoch whose cipher sealed it, or nil when nothing was written.
// Fails on plaintext, on a frame none of epochs opens, and on a frame sealed
// by an epoch whose own certificate was never verified (reported without
// stopping, so the test can show what that seal leads to).
func takeCertEpochWire(
	t *testing.T,
	session *peerEncryptionSession,
	route chan []byte,
	epochs ...*tlsHandshakeEpoch,
) *tlsHandshakeEpoch {
	t.Helper()
	var message []byte
	select {
	case message = <-route:
	default:
		return nil
	}
	defer MessagePoolReturn(message)
	var outer protocol.TransferFrame
	if err := ProtoUnmarshal(message, &outer); err != nil {
		t.Fatal(err)
	}
	if len(outer.EncryptedTransferFrame) == 0 {
		t.Fatal("send with an established cipher wrote plaintext")
	}
	for _, e := range epochs {
		plaintext, err := e.derivedTlsCipher.Open(outer.EncryptedTransferFrame)
		if err != nil {
			continue
		}
		MessagePoolReturn(plaintext)
		if !certEpochVerified(session, e) {
			t.Errorf("frame sealed by epoch %s whose own peer certificate was never verified", e.epochId)
		}
		return e
	}
	t.Fatal("frame sealed by a cipher of no known epoch")
	return nil
}

// Resends the retained item through the real wrap path.
func resendCertEpochItem(t *testing.T, sequence *SendSequence) error {
	t.Helper()
	item := sequence.resendQueue.PeekFirst()
	if item == nil || item.forceUnwrapped || item.contractControl {
		t.Fatal("no retained application item to resend")
	}
	path := sendTransferPath(sequence.client.ClientId(), DestinationId(sequence.destination))
	_, err := sequence.writeMaybeWrappedBytes(item.transferFrameBytes, path, item.forceUnwrapped, item, true, false)
	return err
}

// A rekey that completes after the established epoch's certificate matched,
// but before that match is recorded, must not inherit it. Marking the newly
// established epoch at that point would let its cipher seal without its own
// certificate ever being compared to the commitment.
func TestCertVerificationMarksOnlyCheckedEpoch(t *testing.T) {
	sequence, session, route := newContractRekeyWriteFixture(t)
	trusted, trustedPem := newCertEpochCertificate(t)
	untrusted, _ := newCertEpochCertificate(t)
	e1 := newCertEpoch(t, trusted, 1, false)
	e2 := newCertEpoch(t, untrusted, 2, true)
	setCertEpochSessionForTest(session, [][]byte{trustedPem}, e1, e2)
	marks := 0
	session.beforeCertVerifiedMarkForTest = func() {
		marks += 1
		if marks == 1 {
			promoteCertEpochForTest(session, e2)
		}
	}

	sequence.sendWithSetContract([]*protocol.Frame{requiredGateFrame(t, "checked epoch")}, nil, true, true, false)
	if marks != 1 {
		t.Fatalf("send recorded %d certificate matches, want 1", marks)
	}
	if sealer := takeCertEpochWire(t, session, route, e1, e2); sealer != e1 {
		t.Fatal("first send was not sealed by the epoch it verified")
	}
	if !certEpochVerified(session, e1) || certEpochVerified(session, e2) {
		t.Errorf(
			"certificate match recorded on the wrong epoch: checked=%t promoted=%t",
			certEpochVerified(session, e1), certEpochVerified(session, e2),
		)
	}

	// the promoted epoch presents an uncommitted certificate: refused, unsent
	if err := resendCertEpochItem(t, sequence); err == nil {
		t.Error("promoted epoch sealed without its own certificate checked")
	}
	if sealer := takeCertEpochWire(t, session, route, e1, e2); sealer != nil {
		t.Error("refused send reached the wire")
	}
}

// A background rekey keeps the verified established cipher sealing. Once the
// rekey is promoted, the next send checks the new epoch's own certificate: a
// rotated certificate the peer committed to passes and seals with the new
// cipher, and an uncommitted one fails the send with nothing written.
func TestRekeyPromotionReverifiesPeerCertificate(t *testing.T) {
	for _, rekeyCommitted := range []bool{true, false} {
		sequence, session, route := newContractRekeyWriteFixture(t)
		trusted, trustedPem := newCertEpochCertificate(t)
		rotated, rotatedPem := newCertEpochCertificate(t)
		uncommitted, _ := newCertEpochCertificate(t)
		rekeyCertificate := uncommitted
		if rekeyCommitted {
			rekeyCertificate = rotated
		}
		e1 := newCertEpoch(t, trusted, 1, false)
		e2 := newCertEpoch(t, rekeyCertificate, 2, true)
		setCertEpochSessionForTest(session, [][]byte{trustedPem, rotatedPem}, e1, e1)

		sequence.sendWithSetContract([]*protocol.Frame{requiredGateFrame(t, "rekey")}, nil, true, true, false)
		if sealer := takeCertEpochWire(t, session, route, e1, e2); sealer != e1 {
			t.Fatalf("committed=%t: first send was not sealed by the established epoch", rekeyCommitted)
		}

		// the rekey starts; the established epoch keeps sealing through it
		session.stateLock.Lock()
		session.epoch = e2
		session.stateLock.Unlock()
		if err := resendCertEpochItem(t, sequence); err != nil {
			t.Fatalf("committed=%t: send during the rekey: %v", rekeyCommitted, err)
		}
		if sealer := takeCertEpochWire(t, session, route, e1, e2); sealer != e1 {
			t.Fatalf("committed=%t: send during the rekey was not sealed by the established epoch", rekeyCommitted)
		}
		if certEpochVerified(session, e2) {
			t.Errorf("committed=%t: in-flight rekey epoch inherited the established verification", rekeyCommitted)
		}

		promoteCertEpochForTest(session, e2)
		err := resendCertEpochItem(t, sequence)
		sealer := takeCertEpochWire(t, session, route, e1, e2)
		if rekeyCommitted {
			if err != nil || sealer != e2 || !certEpochVerified(session, e2) {
				t.Fatalf("rotated committed certificate: err=%v sealedByRekey=%t", err, sealer == e2)
			}
		} else {
			if err == nil || sealer != nil || certEpochVerified(session, e2) {
				t.Fatalf(
					"uncommitted rekey certificate: err=%v written=%t verified=%t",
					err, sealer != nil, certEpochVerified(session, e2),
				)
			}
		}
	}
}

// A promotion between taking the sealing cipher and verifying must not pair
// the newer epoch's verification with the older epoch's cipher. Here the
// established epoch's peer passed its identity proof but presents a
// certificate the contract never committed to; its cipher must never seal.
func TestSealingCipherBelongsToVerifiedEpoch(t *testing.T) {
	sequence, session, route := newContractRekeyWriteFixture(t)
	trusted, trustedPem := newCertEpochCertificate(t)
	uncommitted, _ := newCertEpochCertificate(t)
	e1 := newCertEpoch(t, uncommitted, 1, false)
	e2 := newCertEpoch(t, trusted, 2, true)
	setCertEpochSessionForTest(session, [][]byte{trustedPem}, e1, e2)
	taken := 0
	session.afterSealingEpochForTest = func() {
		taken += 1
		if taken == 1 {
			promoteCertEpochForTest(session, e2)
		}
	}

	// the first write takes e1, then races the promotion; whatever it writes
	// must be sealed by an epoch whose own certificate matched
	sequence.sendWithSetContract([]*protocol.Frame{requiredGateFrame(t, "sealing epoch")}, nil, true, true, false)
	if taken != 1 {
		t.Fatalf("send took the sealing epoch %d times, want 1", taken)
	}
	takeCertEpochWire(t, session, route, e1, e2)

	// recovery seals with the promoted epoch, whose certificate matches
	if err := resendCertEpochItem(t, sequence); err != nil {
		t.Fatal(err)
	}
	if sealer := takeCertEpochWire(t, session, route, e1, e2); sealer != e2 {
		t.Fatal("resend after promotion was not sealed by the promoted epoch")
	}
	if certEpochVerified(session, e1) {
		t.Fatal("uncommitted certificate was recorded as verified")
	}
}

// The no-acknowledgement fast path seals without the loop's verification, so
// it may write only when the epoch whose cipher it took is itself verified. A
// promotion between taking the cipher and reading the verification must not
// let the promoted epoch's verification admit the older epoch's cipher. Once
// the promoted epoch seals, the fast path writes again.
func TestNoAckFastPathSealsOnlyWithVerifiedEpoch(t *testing.T) {
	assertMessagePoolOwnership(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := newNoAckBudgetHarness(t, ctx)
	session := &peerEncryptionSession{
		client:   h.client,
		settings: &EncryptionSettings{Mode: EncryptionModeRequired},
	}
	h.sequence.session = session
	trusted, trustedPem := newCertEpochCertificate(t)
	uncommitted, _ := newCertEpochCertificate(t)
	e1 := newCertEpoch(t, uncommitted, 1, false)
	e2 := newCertEpoch(t, trusted, 2, true)
	setCertEpochSessionForTest(session, [][]byte{trustedPem}, e1, e2)
	taken := 0
	session.afterSealingEpochForTest = func() {
		taken += 1
		if taken == 1 {
			// the rekey completes, and another sequence's loop verifies its
			// certificate before this caller reads the verification
			promoteCertEpochForTest(session, e2)
			session.stateLock.Lock()
			e2.certVerified = true
			session.stateLock.Unlock()
		}
	}

	// returns the epoch that sealed the write, or nil when it fell back
	write := func() *tlsHandshakeEpoch {
		frame := &protocol.Frame{MessageType: protocol.MessageType_TransferExchangeSignals, MessageBytes: MessagePoolGet(40)}
		clear(frame.MessageBytes)
		pack := &SendPack{TransferOptions: TransferOptions{Ack: false}, Frame: frame, Ctx: ctx, Destination: h.destinationId}
		written := h.sequence.writeNoAckFastPath(h.sequence.noAckFastPath.Load(), pack)
		sealer := takeCertEpochWire(t, session, h.route, e1, e2)
		if !written {
			pack.disposeUnsentGroup(context.Canceled)
		}
		if written != (sealer != nil) {
			t.Fatalf("fast path written=%t but wire frame present=%t", written, sealer != nil)
		}
		return sealer
	}
	write()
	if taken != 1 {
		t.Fatalf("fast path took the sealing epoch %d times, want 1", taken)
	}
	if sealer := write(); sealer != e2 {
		t.Fatal("fast path did not resume under the verified promoted epoch")
	}
}
