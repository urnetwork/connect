// A replacement timeout cannot reintroduce an unreadable contract head while
// the old cipher remains available to application data.
package connect

import (
	"context"
	"testing"

	"github.com/urnetwork/connect/protocol"
)

// Completes the replacement with an explicit error, without a clock race or
// worker. The last successful epoch deliberately remains the application key.
func failContractReplacementForTest(session *peerEncryptionSession) {
	handshakeDone := make(chan struct{})
	establishmentDone := make(chan struct{})
	close(handshakeDone)
	close(establishmentDone)
	session.stateLock.Lock()
	defer session.stateLock.Unlock()
	session.epoch = &tlsHandshakeEpoch{
		handshakeDone:     handshakeDone,
		establishmentDone: establishmentDone,
		handshakeErr:      context.DeadlineExceeded,
	}
}

// A blocked opener or an ahead announcement may first reach the writer after
// the handshake deadline. Both still have to let the peer read the contract.
func TestContractControlNewAfterFailedRekey(t *testing.T) {
	for _, mode := range []EncryptionMode{EncryptionModeOpportunistic, EncryptionModeRequired} {
		for _, ahead := range []bool{false, true} {
			sequence, session, route := newContractRekeyWriteFixture(t)
			session.settings.Mode = mode
			failContractReplacementForTest(session)
			if ahead {
				contract := newContractAheadTestContract(t, sequence.client, sequence.destination)
				sequence.sendContractAheadAnnouncement(contract, nil)
			} else {
				sequence.sendWithSetContract(nil, nil, true, true, false)
			}
			requireContractRekeyWireWrapped(t, route, false)
			item := sequence.resendQueue.PeekFirst()
			if item == nil || !item.forceUnwrapped {
				t.Fatalf("failed replacement lost contract bootstrap pin: mode=%v ahead=%t", mode, ahead)
			}
			if session.Cipher() == nil {
				t.Fatal("contract recovery discarded the retained application cipher")
			}
		}
	}
}

// A contract first written under a working cipher can need recovery only
// after the replacement has failed. The pin must survive subsequent success.
func TestContractControlResendAfterFailedRekey(t *testing.T) {
	for _, ahead := range []bool{false, true} {
		sequence, session, route := newContractRekeyWriteFixture(t)
		if ahead {
			contract := newContractAheadTestContract(t, sequence.client, sequence.destination)
			sequence.sendContractAheadAnnouncement(contract, nil)
		} else {
			sequence.sendWithSetContract(nil, nil, true, true, false)
		}
		requireContractRekeyWireWrapped(t, route, true)
		item := sequence.resendQueue.PeekFirst()
		if item == nil || item.forceUnwrapped {
			t.Fatalf("working contract was not retained wrapped: ahead=%t", ahead)
		}
		failContractReplacementForTest(session)
		path := sendTransferPath(sequence.client.ClientId(), DestinationId(sequence.destination))
		if _, err := sequence.writeMaybeWrappedBytes(item.transferFrameBytes, path, item.forceUnwrapped, item, true, false); err != nil {
			t.Fatal(err)
		}
		requireContractRekeyWireWrapped(t, route, false)
		if !item.forceUnwrapped {
			t.Fatal("failed replacement did not pin its retained contract")
		}
		session.stateLock.Lock()
		session.epoch = session.establishedEpoch
		session.stateLock.Unlock()
		if _, err := sequence.writeMaybeWrappedBytes(item.transferFrameBytes, path, item.forceUnwrapped, item, true, false); err != nil {
			t.Fatal(err)
		}
		requireContractRekeyWireWrapped(t, route, false)
	}
}

// Failed replacement recovery is limited to control-only frames. Empty and
// nonempty application bodies keep encryption on both their first and resend.
func TestContractDataHeadRetainsCipherAfterFailedRekey(t *testing.T) {
	for _, body := range []string{"", "synthetic application bytes"} {
		for _, mode := range []EncryptionMode{EncryptionModeOpportunistic, EncryptionModeRequired} {
			sequence, session, route := newContractRekeyWriteFixture(t)
			session.settings.Mode = mode
			failContractReplacementForTest(session)
			frame := requiredGateFrame(t, body)
			sequence.sendWithSetContract([]*protocol.Frame{frame}, nil, true, true, false)
			requireContractRekeyWireWrapped(t, route, true)
			item := sequence.resendQueue.PeekFirst()
			if item == nil || item.forceUnwrapped || item.contractControl {
				t.Fatalf("application data received control permission: mode=%v empty=%t", mode, body == "")
			}
			path := sendTransferPath(sequence.client.ClientId(), DestinationId(sequence.destination))
			if _, err := sequence.writeMaybeWrappedBytes(item.transferFrameBytes, path, item.forceUnwrapped, item, true, false); err != nil {
				t.Fatal(err)
			}
			requireContractRekeyWireWrapped(t, route, true)
		}
	}
}
