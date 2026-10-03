// Delayed DNS answers must survive the provider race's comparison window.
package connect

import (
	"bytes"
	"context"
	"net"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
	"golang.org/x/net/dns/dnsmessage"
)

// Local grouped-send fixture; only provider admission and return timing are
// controlled. Routing, race completion, delivery, and pool ownership are real.
type dnsRaceTest struct {
	parent      *RemoteUserNatMultiClient
	update      *multiClientChannelUpdate
	path        *IpPath
	clients     []*multiClientChannel
	cancelFirst context.CancelFunc
	response    []byte
	delivered   [][]byte
}

// Called inside virtual time after priming the process-owned pool outside it.
func newDnsRaceTest(t *testing.T) *dnsRaceTest {
	t.Helper()
	parent, update, closeParent := groupTestParent(t, DisableSecurityPolicy())
	t.Cleanup(closeParent)
	path := &IpPath{
		Version:         4,
		Protocol:        IpProtocolUdp,
		SourceIp:        net.IPv4(192, 0, 2, 2).To4(),
		SourcePort:      45001,
		DestinationIp:   net.IPv4(198, 51, 100, 53).To4(),
		DestinationPort: 53,
	}
	parent.ip4PathUpdates = map[Ip4Path]*multiClientChannelUpdate{path.ToIp4Path(): update}
	parent.flowUpdates = map[*multiClientChannelUpdate]bool{update: true}
	update.activityTime = time.Now()
	question := dnsmessage.Question{
		Name: dnsmessage.MustNewName("race-delay.example."),
		Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET,
	}
	query := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: 7, RecursionDesired: true},
		Questions: []dnsmessage.Question{question},
	}
	queryBytes, err := query.Pack()
	if err != nil {
		t.Fatal(err)
	}
	answer := query
	answer.Response = true
	answer.Answers = []dnsmessage.Resource{{
		Header: dnsmessage.ResourceHeader{Name: question.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 60},
		Body:   &dnsmessage.AResource{A: [4]byte{203, 0, 113, 9}},
	}}
	answerBytes, err := answer.Pack()
	if err != nil {
		t.Fatal(err)
	}
	f := &dnsRaceTest{parent: parent, update: update, path: path, response: ipOosUdpPacket(path.Reverse(), answerBytes)}
	parent.SetReceivePacketCallback(func(_ TransferPath, _ protocol.ProvideMode, _ *IpPath, packet []byte) {
		f.delivered = append(f.delivered, bytes.Clone(packet))
	})
	var sent atomic.Int32
	for range 2 {
		client := &multiClientChannel{
			ctx: parent.ctx, settings: parent.settings,
			sendGroupForTest: func(group *parsedPacketGroup, _ time.Duration, _ bool) (bool, error) {
				sent.Add(1)
				for _, packet := range group.packets {
					MessagePoolReturn(packet.packet)
				}
				return true, nil
			},
		}
		f.clients = append(f.clients, client)
	}
	f.clients[0].ctx, f.cancelFirst = context.WithCancel(parent.ctx)
	t.Cleanup(f.cancelFirst)
	parent.groupRaceCandidatesForTest = func(*parsedPacketGroup) []*multiClientChannel { return f.clients }
	packet := MessagePoolCopy(ipOosUdpPacket(path, queryBytes))
	if !parent.sendPacketGroup(SourceId(NewId()), protocol.ProvideMode_Network, requireGroupTestPacketGroup(t, packet), 0) {
		MessagePoolReturn(packet)
		t.Fatal("DNS query was not admitted")
	}
	synctest.Wait()
	if sent.Load() != 2 {
		t.Fatalf("query admissions = %d, want two candidate providers", sent.Load())
	}
	return f
}

// Advances the fake clock after every admission and timer owner has parked.
func (self *dnsRaceTest) elapseWindow() {
	time.Sleep(self.parent.settings.MultiRaceSetOnResponseTimeout + time.Second)
	synctest.Wait()
}

// Borrows the same answer through the ordinary post-accounting receive path.
func (self *dnsRaceTest) answer(client *multiClientChannel) {
	self.parent.clientReceivePacketResolve(client, TransferPath{}, protocol.ProvideMode_Network, self.path, self.response, tcpControlObservation{})
	synctest.Wait()
}

// Checks application delivery and the provider ownership used by teardown.
func (self *dnsRaceTest) requireAnswer(t *testing.T, client *multiClientChannel) {
	t.Helper()
	if len(self.delivered) != 1 || !bytes.Equal(self.delivered[0], self.response) {
		t.Fatalf("delivered DNS answers = %d, want the candidate's delayed answer without another query", len(self.delivered))
	}
	if self.update.client.Load() != client || !self.update.receivedInbound.Load() || !self.parent.clientUpdates[client][self.update] {
		t.Fatal("delayed DNS answer did not establish and register its responding provider")
	}
}

// The resolver still owns its socket after the two-second comparison window.
// Virtual time forces the first answer to arrive later without scheduler races
// or a second query that could reopen the route before the answer arrives.
func TestMultiClientDnsAnswerAfterRaceWindow(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newDnsRaceTest(t)
		f.elapseWindow()
		f.answer(f.clients[1])
		f.requireAnswer(t, f.clients[1])
	})
}

// A matching IP tuple from a channel never offered the query is insufficient.
func TestMultiClientDnsRaceRejectsUnselectedProvider(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newDnsRaceTest(t)
		f.elapseWindow()
		f.answer(&multiClientChannel{ctx: f.parent.ctx, settings: f.parent.settings})
		if len(f.delivered) != 0 || f.update.client.Load() != nil {
			t.Fatal("an unselected provider claimed the unanswered flow")
		}
		f.answer(f.clients[1])
		f.requireAnswer(t, f.clients[1])
	})
}

// Provider cancellation must not turn retained identity into a new binding.
func TestMultiClientDnsRaceRejectsCanceledProvider(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newDnsRaceTest(t)
		f.elapseWindow()
		f.cancelFirst()
		f.answer(f.clients[0])
		if len(f.delivered) != 0 || f.update.client.Load() != nil {
			t.Fatal("a canceled provider claimed the unanswered flow")
		}
		f.answer(f.clients[1])
		f.requireAnswer(t, f.clients[1])
	})
}

// Once one reply wins, other candidates cannot append a second DNS answer.
func TestMultiClientDnsRaceWinnerRejectsLateLoser(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newDnsRaceTest(t)
		f.elapseWindow()
		f.answer(f.clients[1])
		f.answer(f.clients[0])
		f.requireAnswer(t, f.clients[1])
	})
}

// Cancellation can precede Close; no late packet may resurrect that generation.
func TestMultiClientDnsRaceCanceledFlowCannotCommit(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newDnsRaceTest(t)
		f.elapseWindow()
		f.update.cancel()
		f.answer(f.clients[1])
		if len(f.delivered) != 0 || f.update.client.Load() != nil || len(f.parent.clientUpdates) != 0 {
			t.Fatal("a late reply resurrected the canceled flow")
		}
		f.update.Close()
		if f.update.race != nil {
			t.Fatal("Close retained unanswered provider candidates")
		}
	})
}

// The existing shared reaper owns silent candidates; no new per-flow waiter
// or timer is needed after the comparison window has elapsed.
func TestMultiClientDnsRaceReaperReleasesSilentCandidates(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newDnsRaceTest(t)
		f.elapseWindow()
		race := f.update.race
		if race == nil || f.update.client.Load() != nil {
			t.Fatal("silence must retain the offered candidates without choosing one")
		}
		retired, _, _ := f.parent.detachIdleFlows(time.Now().Add(f.parent.settings.SequenceIdleTimeout))
		f.parent.finishRetiredFlows(retired)
		synctest.Wait()
		if len(retired) != 1 || f.update.race != nil || len(race.clientStates) != 0 || len(f.parent.ip4PathUpdates) != 0 || len(f.parent.flowUpdates) != 0 {
			t.Fatal("shared reaper retained the expired flow or its candidate identities")
		}
		if !f.update.IsDone() || race.ctx.Err() == nil {
			t.Fatal("reaper did not cancel both flow and race lifetimes")
		}
	})
}
