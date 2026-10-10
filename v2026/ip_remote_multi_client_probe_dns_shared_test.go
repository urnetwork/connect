package connect

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func TestProbeSharedDnsReplyIdentity(t *testing.T) {
	for _, version := range []int{4, 6} {
		t.Run(fmt.Sprintf("ip%d", version), func(t *testing.T) {
			parent, client, forwarded := probeTestParent(t)
			resolver := net.ParseIP("1.1.1.1")
			if version == 6 {
				resolver = net.ParseIP("2606:4700:4700::1111")
			}
			names := []string{"first.example", "second.example", "third.example"}
			anchor, ok := parent.registerProbeFlow(client, probeResolverTarget(resolver, names[0]), names...)
			if !ok {
				t.Fatal("register shared resolver")
			}
			defer parent.unregisterProbeFlows([]*probeFlow{anchor})
			questions := make([]*probeFlow, len(names))
			for i := range questions {
				questions[i] = anchor.dnsQueries[uint16(anchor.synSequence)+uint16(i)]
				if questions[i] == nil || questions[i].ipPath != anchor.ipPath || questions[i].target.QueryName != names[i] {
					t.Fatal("question lost its name/id/shared path")
				}
			}
			reply := func(source *multiClientChannel, path *IpPath, id uint16, name string, last byte) {
				payload := dnsTestAnswer(t, id, name, 0x8180, []dnsTestRecord{dnsTestARecord(net.IPv4(203, 0, 113, last))})
				packet := ipOosUdpPacket(path.Reverse(), payload)
				ingress, err := ParseIpPath(packet)
				if err != nil {
					t.Fatal(err)
				}
				parent.clientReceivePacket(source, TransferPath{}, 0, TransportTypeUnknown, ingress, packet)
			}
			// Unknown transaction, wrong provider and wrong resolver are not
			// evidence for any pending sibling.
			reply(client, anchor.ipPath, uint16(anchor.synSequence)+3, names[0], 10)
			reply(probeTestChannel(parent.settings), anchor.ipPath, uint16(questions[0].synSequence), names[0], 10)
			wrongPath := *anchor.ipPath
			wrongPath.DestinationPort = 54
			reply(client, &wrongPath, uint16(questions[0].synSequence), names[0], 10)
			for _, q := range questions {
				select {
				case <-q.done:
					t.Fatal("unmatched reply completed a question")
				default:
				}
			}
			// Reverse order and a duplicate with different bytes must preserve
			// each question's first exact answer, not consume the whole tuple.
			for i := len(questions) - 1; i >= 0; i-- {
				q := questions[i]
				reply(client, anchor.ipPath, uint16(q.synSequence), names[i], byte(20+i))
				reply(client, anchor.ipPath, uint16(q.synSequence), names[i], 99)
				if !q.answered.Load() {
					t.Fatal("matching resolver answer did not complete question")
				}
				ips, valid := parseDnsAResponse(q.answer, uint16(q.synSequence))
				if !valid || len(ips) != 1 || !ips[0].Equal(net.IPv4(203, 0, 113, byte(20+i))) {
					t.Fatal("duplicate replaced a question's first response")
				}
			}
			parent.unregisterProbeFlows([]*probeFlow{anchor})
			// Cleanup is identity guarded and late replies are still consumed.
			fresh, ok := parent.registerProbeFlow(client, probeResolverTarget(resolver, names[0]), names...)
			if !ok {
				t.Fatal("register next pass")
			}
			defer parent.unregisterProbeFlows([]*probeFlow{fresh})
			parent.unregisterProbeFlows([]*probeFlow{anchor})
			reply(client, anchor.ipPath, uint16(questions[0].synSequence), names[0], 77)
			for _, q := range fresh.dnsQueries {
				if q.answered.Load() {
					t.Fatal("late previous-pass reply completed the next pass")
				}
			}
			if len(*forwarded) != 0 || parent.providerQualified(client.probeDestination()) || client.dialFailureCount() != 0 {
				t.Fatal("DNS demultiplexing forwarded probe bytes or manufactured a verdict")
			}
		})
	}
}

func TestProbeSharedDnsDeadlineAndCancellation(t *testing.T) {
	for _, terminal := range []string{"deadline", "parent_cancel", "channel_cancel", "dial_failure", "send_refused"} {
		t.Run(terminal, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				parent, client, _ := probeTestParent(t)
				parentCtx, cancelParent := context.WithCancel(context.Background())
				clientCtx, cancelClient := context.WithCancel(parentCtx)
				defer cancelParent()
				defer cancelClient()
				parent.ctx, client.ctx = parentCtx, clientCtx
				if terminal == "send_refused" {
					client.stalled.Store(false)
				}
				start := time.Now()
				done := make(chan struct{})
				go func() {
					defer close(done)
					resolved, answered := parent.probeResolveNames(client, net.IPv4(1, 1, 1, 1), []string{"one.example", "two.example", "three.example"}, 4*time.Second)
					if len(resolved) != 0 || answered {
						t.Error("silent resolver produced a positive answer")
					}
				}()
				synctest.Wait()
				switch terminal {
				case "parent_cancel":
					cancelParent()
				case "channel_cancel":
					cancelClient()
				case "dial_failure":
					parent.stateLock.Lock()
					var anchor *probeFlow
					for _, update := range parent.ip4PathUpdates {
						anchor = update.probe
					}
					parent.stateLock.Unlock()
					if anchor == nil || !parent.probeDialFailure(client, anchor.ipPath) {
						t.Fatal("missing shared resolver flow")
					}
				}
				<-done
				wantDuration := time.Duration(0)
				if terminal == "deadline" {
					wantDuration = 4 * time.Second
				}
				if time.Since(start) != wantDuration || len(parent.ip4PathUpdates) != 0 || len(parent.ip6PathUpdates) != 0 {
					t.Errorf("terminal=%s duration=%s map4=%d map6=%d", terminal, time.Since(start), len(parent.ip4PathUpdates), len(parent.ip6PathUpdates))
				}
				wantSent := uint64(3)
				if terminal == "send_refused" {
					wantSent = 0
				}
				if parent.reliabilityMetrics.probesSent.Load() != wantSent || parent.reliabilityMetrics.probesAnswered.Load() != 0 || client.dialFailureCount() != 0 {
					t.Fatal("terminal changed question counts or convicted the provider")
				}
			})
		})
	}
}

func TestProbeSharedDnsConcurrentCompletionCleanup(t *testing.T) {
	parent, client, _ := probeTestParent(t)
	names := []string{"first.example", "second.example"}
	anchor, ok := parent.registerProbeFlow(client, probeResolverTarget(net.IPv4(1, 1, 1, 1), names[0]), names...)
	if !ok {
		t.Fatal("register resolver")
	}
	payload := dnsTestAnswer(t, uint16(anchor.synSequence), names[0], 0x8180, nil)
	packet := ipOosUdpPacket(anchor.ipPath.Reverse(), payload)
	path, err := ParseIpPath(packet)
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for i := 0; i < 24; i++ {
		workers.Add(1)
		go func(i int) {
			defer workers.Done()
			switch i % 3 {
			case 0:
				parent.clientReceiveProbePacket(client, path, packet)
			case 1:
				parent.probeDialFailure(client, anchor.ipPath)
			case 2:
				parent.unregisterProbeFlows([]*probeFlow{anchor})
			}
		}(i)
	}
	workers.Wait()
	if len(parent.ip4PathUpdates) != 0 {
		t.Fatal("concurrent cleanup left shared resolver registered")
	}
	for _, q := range anchor.dnsQueries {
		if q.answered.Load() && (len(q.answer) < 2 || binary.BigEndian.Uint16(q.answer) != uint16(q.synSequence)) {
			t.Fatal("concurrent answer publication mixed identities")
		}
	}
}
