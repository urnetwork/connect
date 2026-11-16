// Provider answers acquit their own exact source without erasing failed peers.
package connect

import (
	"bytes"
	"testing"
	"testing/synctest"
)

// Both real sources admit the same native opening before B's explicit answer.
// B's successful retry must not turn A's failed upstream dial into health.
func TestDialFailureRetryAnswerPreservesFailedProviderConnectOwner(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, version := range []int{4, 6} {
		synctest.Test(t, func(t *testing.T) {
			harness := newReraceHarness(t, []reraceExitSpec{
				{name: "a", tier: 0, answer: reraceAlways(reraceSignal)},
				{name: "b", tier: 2, answer: reraceAlways(reraceSilent)},
			}, nil)
			a, b := harness.channel("a"), harness.channel("b")
			path := reraceFlow(version, 11, 47100+version)
			if !harness.syn(path) {
				t.Fatal("the original native SYN was not admitted")
			}
			if len(harness.exit("a").syns(path)) != 1 || len(harness.exit("b").syns(path)) != 1 ||
				harness.metrics().DialFailuresIntercepted != 1 || len(harness.delivered(path)) != 0 {
				t.Fatal("the actual failed dial and automatic retry did not reach both silent native openings")
			}
			update := harness.update(path)
			if update == nil || update.receivedInbound.Load() {
				t.Fatal("the unanswered flow lost its canonical owner or gained a response")
			}
			aKey, aValid := a.affinitySourceKey()
			bKey, bValid := b.affinitySourceKey()
			if !aValid || !bValid || aKey == bKey {
				t.Fatal("the actual providers did not retain distinct authenticated source identities")
			}
			var aOwner, bOwner *connectSynOwner
			func() {
				update.stateLock.Lock()
				defer update.stateLock.Unlock()
				for owner := update.connectSynOwners; owner != nil; owner = owner.flowNext {
					switch owner.sourceKey {
					case aKey:
						if aOwner != nil {
							t.Fatal("A retained duplicate source owners")
						}
						aOwner = owner
					case bKey:
						if bOwner != nil {
							t.Fatal("B retained duplicate source owners")
						}
						bOwner = owner
					default:
						t.Fatal("the flow retained an unexpected provider source")
					}
				}
			}()
			if aOwner == nil || bOwner == nil || aOwner == bOwner || aOwner.retired.Load() || bOwner.retired.Load() ||
				aOwner.quota != a.connectSynQuota.Load() || bOwner.quota != b.connectSynQuota.Load() {
				t.Fatal("the two genuine SYN admissions did not retain their own live source owners")
			}
			aBefore, err := a.WindowStats()
			if err != nil {
				t.Fatal(err)
			}
			bBefore, err := b.WindowStats()
			if err != nil {
				t.Fatal(err)
			}
			if aBefore.firstUnansweredSendSynTime.IsZero() || bBefore.firstUnansweredSendSynTime.IsZero() ||
				aBefore.sendSynCount != 1 || aBefore.receiveSynCount != 0 || aBefore.receiveAckCount != 0 ||
				bBefore.sendSynCount != 1 || bBefore.receiveSynCount != 0 || bBefore.receiveAckCount != 0 {
				t.Fatal("the real unanswered provider openings did not each retain their own connect age")
			}

			// The ordinary provider Client carries this exact SYN-ACK through
			// authentication, ordered receive, generation validation and commit.
			harness.inject("b", reraceSynAck(path))
			if _, ok := harness.synAck(path); !ok || harness.boundTo(path) != b || !update.receivedInbound.Load() {
				t.Fatal("B's real answer did not establish and deliver the canonical opening")
			}
			aAfter, err := a.WindowStats()
			if err != nil {
				t.Fatal(err)
			}
			bAfter, err := b.WindowStats()
			if err != nil {
				t.Fatal(err)
			}
			if aOwner.retired.Load() || !aAfter.firstUnansweredSendSynTime.Equal(aBefore.firstUnansweredSendSynTime) ||
				aAfter.receiveSynCount != 0 || aAfter.receiveAckCount != 0 {
				t.Fatal("B's successful retry retired A's exact failed-provider owner or connect age")
			}
			if !bOwner.retired.Load() || !bAfter.firstUnansweredSendSynTime.IsZero() ||
				bAfter.receiveSynCount != 1 || bAfter.receiveAckCount != 1 {
				t.Fatal("B's own native answer did not acquit its exact provider opening")
			}
			func() {
				update.stateLock.Lock()
				defer update.stateLock.Unlock()
				if update.connectSynOwners != aOwner || aOwner.flowNext != nil {
					t.Fatal("the answered flow did not preserve only the original failed-provider owner")
				}
			}()
			payload := []byte("synthetic healthy retry payload")
			if !harness.send(reraceSegment(path, 0, payload)) {
				t.Fatal("the healthy retry refused native data")
			}
			packets := harness.delivered(path)
			if len(packets) != 2 {
				t.Fatal("the healthy retry did not deliver exactly its SYN-ACK and data response")
			}
			_, receivedPayload, err := ParseIpPathWithPayload(packets[1].packet)
			if err != nil || !bytes.Equal(receivedPayload, payload) || b.IsDone() {
				t.Fatal("preserving failed-provider evidence interrupted the healthy sibling")
			}
			// Generic flow closure relinquishes local custody; it supplies no
			// authenticated answer from the still-unanswered provider A.
			update.Close()
			closed, err := a.WindowStats()
			if err != nil || aOwner.retired.Load() ||
				!closed.firstUnansweredSendSynTime.Equal(aBefore.firstUnansweredSendSynTime) || b.IsDone() {
				t.Fatal("later local flow cleanup acquitted the failed provider or ended its healthy sibling")
			}
		})
	}
}
