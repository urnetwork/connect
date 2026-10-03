package connect

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type checkedTestPeerPinStore struct {
	*testPeerClientKeyPinStore
	readErr   error
	commitErr error
	commits   int
}

func (self *checkedTestPeerPinStore) GetPeerClientKeyPinChecked(peer Id) (ClientKeyPin, bool, error) {
	if self.readErr != nil {
		return ClientKeyPin{}, false, self.readErr
	}
	pin, ok := self.GetPeerClientKeyPin(peer)
	return pin, ok, nil
}

func (self *checkedTestPeerPinStore) CommitPeerClientKeyPin(peer Id, pin ClientKeyPin) error {
	self.commits++
	if self.commitErr != nil {
		return self.commitErr
	}
	self.SetPeerClientKeyPin(peer, pin)
	self.SetSignedHistorySeen()
	return nil
}

func TestSignedIdentityCheckedStoreFailureIsLocalAndTerminal(t *testing.T) {
	for _, phase := range []string{"read", "capacity", "io"} {
		t.Run(phase, func(t *testing.T) {
			store := &checkedTestPeerPinStore{testPeerClientKeyPinStore: newTestPeerClientKeyPinStore()}
			failure := errors.New(phase + strings.Repeat("x", 1024))
			if phase == "read" {
				store.readErr = failure
			} else {
				store.commitErr = failure
			}
			session, done := newTestKeyHistorySession(t, store, nil)
			defer done()
			events := make(chan *EncryptionEvent, 2)
			remove := session.manager.AddEncryptionEventCallback(func(event *EncryptionEvent) { events <- event })
			defer remove()
			armKeyHistoryGate(session)
			session.applyPeerClientKeyHistory(goldenHeadPublicKey(t), goldenClientKeyHistory())
			if keyHistoryState(session) != clientKeyHistoryStoreUnavailable || session.Cipher() != nil || session.KeyIdentityRejected() {
				t.Fatal("local store failure exposed Cipher or accused the peer")
			}
			if store.setPinCalls != 0 || store.SignedHistorySeen() {
				t.Fatal("failed commit changed pin or latch")
			}
			select {
			case event := <-events:
				if event.Type != EncryptionEventKeyIdentityStoreUnavailable || len(event.Reason) > 256 {
					t.Fatalf("wrong/unbounded event: %+v", event)
				}
			default:
				t.Fatal("local storage failure was not observable")
			}
			session.openKeyHistoryGate(clientKeyHistoryVerified, "")
			if keyHistoryState(session) != clientKeyHistoryStoreUnavailable || session.Cipher() != nil {
				t.Fatal("terminal local failure reopened")
			}
		})
	}
}

func TestSignedIdentityCheckedStoreCommitAndFetchPolicy(t *testing.T) {
	store := &checkedTestPeerPinStore{testPeerClientKeyPinStore: newTestPeerClientKeyPinStore()}
	session, done := newTestKeyHistorySession(t, store, nil)
	defer done()
	armKeyHistoryGate(session)
	session.applyPeerClientKeyHistory(goldenHeadPublicKey(t), goldenClientKeyHistory())
	if keyHistoryState(session) != clientKeyHistoryVerified || store.commits != 1 || !store.SignedHistorySeen() {
		t.Fatal("successful checked commit did not open the gate")
	}
	for _, healthy := range []bool{true, false} {
		store := &checkedTestPeerPinStore{testPeerClientKeyPinStore: newTestPeerClientKeyPinStore()}
		if !healthy {
			store.readErr = errors.New("store closed")
		}
		session, done := newTestKeyHistorySession(t, store, func(context.Context) ([][]byte, error) { return nil, errors.New("evidence unavailable") })
		armKeyHistoryGate(session)
		session.resolvePeerClientKeyHistory(goldenHeadPublicKey(t))
		deadline := time.Now().Add(time.Second)
		for keyHistoryState(session) == clientKeyHistoryPending && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		want := clientKeyHistoryVerified
		if !healthy {
			want = clientKeyHistoryStoreUnavailable
		}
		if got := keyHistoryState(session); got != want {
			t.Fatalf("healthy=%t state=%v want=%v", healthy, got, want)
		}
		done()
	}
}
