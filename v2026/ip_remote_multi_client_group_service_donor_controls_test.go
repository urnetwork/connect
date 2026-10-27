// Selector-state controls complement the real provider refusal regression.
// Declared history inputs are never described as observed provider failures.
package connect

import (
	"bytes"
	"net"
	"testing"
	"testing/synctest"
	"time"
)

// Uses the existing INITIAL adoption fixture with a retained answered donor.
func privateServiceDonorParent(t *testing.T) (*RemoteUserNatMultiClient, *multiClientChannelUpdate, *multiClientChannel) {
	t.Helper()
	parent := affinityTimeTestParent(t)
	old, donor := affinityTimeTestDonor(t, parent, 4)
	old.receivedInbound.Store(true)
	return parent, old, donor
}

// The existing cache writer owns its ordinary key and timestamp semantics.
// Only expiry controls replace the timestamp with a declared boundary input.
func privateServiceDonorHistory(parent *RemoteUserNatMultiClient, client *multiClientChannel, path *IpPath, age time.Duration) {
	parent.stateLock.Lock()
	defer parent.stateLock.Unlock()
	parent.recordDestinationServiceFailureWithLock(client, path)
	if age != 0 {
		key, _ := destinationServiceFailureKeyFor(path, client)
		entry := parent.destinationServiceFailures[key]
		entry.time = time.Now().Add(-age)
		parent.destinationServiceFailures[key] = entry
	}
}

// Missing exact-service history preserves ordinary quality affinity.
func TestPrivateGroupDonorUnknownServiceKeepsHealthyAffinity(t *testing.T) {
	parent, old, donor := privateServiceDonorParent(t)
	fresh, _, selected := parent.sendUpdate(affinityTimeTestPath(4, 44001), flowPin{})
	if selected != donor || fresh == old || fresh.client.loadSelection() != old.client.loadSelection() || old.client.Load() != donor {
		t.Fatal("unknown service history discarded a healthy affinity donor")
	}
}

// Failure of another destination is not global provider failure evidence.
func TestPrivateGroupDonorOtherDestinationKeepsHealthyAffinity(t *testing.T) {
	parent, old, donor := privateServiceDonorParent(t)
	other := affinityTimeTestPath(4, 44001)
	other.DestinationIp = net.ParseIP("203.0.113.28")
	privateServiceDonorHistory(parent, donor, other, 0)
	fresh, _, selected := parent.sendUpdate(affinityTimeTestPath(4, 44001), flowPin{})
	if selected != donor || fresh.client.loadSelection() != old.client.loadSelection() || old.client.Load() != donor {
		t.Fatal("another destination's cache entry vetoed ordinary affinity")
	}
}

// A port-specific refusal must not turn into an address-wide donor ban.
func TestPrivateGroupDonorOtherPortKeepsHealthyAffinity(t *testing.T) {
	parent, old, donor := privateServiceDonorParent(t)
	other := affinityTimeTestPath(4, 44001)
	other.DestinationPort = 8443
	privateServiceDonorHistory(parent, donor, other, 0)
	fresh, _, selected := parent.sendUpdate(affinityTimeTestPath(4, 44001), flowPin{})
	if selected != donor || fresh.client.loadSelection() != old.client.loadSelection() || old.client.Load() != donor {
		t.Fatal("another port's cache entry vetoed ordinary affinity")
	}
}

// Equality at the existing TTL is already expired and supplies no veto.
func TestPrivateGroupDonorExpiredFailureKeepsHealthyAffinity(t *testing.T) {
	parent, old, donor := privateServiceDonorParent(t)
	path := affinityTimeTestPath(4, 44001)
	privateServiceDonorHistory(parent, donor, path, destinationServiceFailureTtl)
	fresh, _, selected := parent.sendUpdate(path, flowPin{})
	if selected != donor || fresh.client.loadSelection() != old.client.loadSelection() || old.client.Load() != donor {
		t.Fatal("expired service history vetoed ordinary affinity")
	}
}

// Public pin semantics remain explicit authority over ordinary new-flow bias.
func TestPrivateGroupDonorKnownFailureKeepsExplicitPin(t *testing.T) {
	parent, old, donor := privateServiceDonorParent(t)
	path := affinityTimeTestPath(4, 44001)
	privateServiceDonorHistory(parent, donor, path, 0)
	fresh, _, selected := parent.sendUpdate(path, flowPin{site: true})
	if fresh == nil || !fresh.pinned || selected != donor || old.client.Load() != donor {
		t.Fatal("ordinary service guard changed explicit pin affinity")
	}
}

// UDP retains its existing inheritance behavior even with exact UDP history.
// This component directly exercises INITIAL adoption, not TCP donor grouping.
func TestPrivateGroupDonorUdpKeepsExistingAdoption(t *testing.T) {
	parent, old, donor := privateServiceDonorParent(t)
	path := affinityTimeTestPath(4, 44001)
	path.Protocol, path.Syn, path.SequenceNumber = IpProtocolUdp, false, 0
	privateServiceDonorHistory(parent, donor, path, 0)
	fresh := newMultiClientChannelUpdate(parent.ctx, path)
	defer fresh.Close()
	parent.stateLock.Lock()
	adopted := parent.adoptAffinityClientWithLock(fresh, donor, old.client.loadSelection())
	parent.stateLock.Unlock()
	if !adopted || fresh.client.Load() != donor || old.client.Load() != donor {
		t.Fatal("TCP-only service guard changed UDP adoption")
	}
}

// The existing adoption seam declares history between ranking and final store.
// It writes under the already-held parent lock; no nested self-locking query.
func TestPrivateGroupDonorFinalAdoptionRechecksCurrentHistory(t *testing.T) {
	parent, old, donor := privateServiceDonorParent(t)
	path := affinityTimeTestPath(4, 44001)
	visited := false
	parent.beforeAffinityAdoptionForTest = func(client *multiClientChannel) {
		visited = true
		parent.recordDestinationServiceFailureWithLock(client, path)
	}
	fresh, _, selected := parent.sendUpdate(path, flowPin{})
	if !visited || selected != nil || fresh.client.Load() != nil || old.client.Load() != donor {
		t.Fatal("current service failure was ignored at final adoption")
	}
}

// An eligible older donor in the same group outranks a newer failed donor.
// Virtual time only distinguishes creation ordering; it waits for no event.
func TestPrivateGroupDonorOlderUnfailedCandidateStaysAvailable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		parent, old, first := privateServiceDonorParent(t)
		time.Sleep(time.Millisecond)
		newer, _, inherited := parent.sendUpdate(affinityTimeTestPath(4, 44001), flowPin{})
		if inherited != first {
			t.Fatal("second original group member did not inherit its healthy donor")
		}
		second := &multiClientChannel{ctx: parent.ctx, settings: parent.settings, healthy: true, args: &multiClientChannelArgs{DestinationStats: DestinationStats{IpFamily: IpFamilyDualstack}}}
		newer.selectAffinityClient(second)
		parent.bindClientFlow(newer, second)
		newer.receivedInbound.Store(true)
		path := affinityTimeTestPath(4, 44002)
		privateServiceDonorHistory(parent, second, path, 0)
		time.Sleep(time.Millisecond)
		fresh, _, selected := parent.sendUpdate(path, flowPin{})
		if selected != first || fresh.client.loadSelection() != old.client.loadSelection() || old.client.Load() != first || newer.client.Load() != second {
			t.Fatal("newer failed donor hid the group's older eligible donor")
		}
	})
}

// Declining affinity still reaches the ordinary all-failed availability field.
// The actual first-window provider can answer without changing old ownership.
func TestPrivateGroupDonorAllFailedStillUsesOrdinaryFallback(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		harness := newPrivateCrossWindowHarness(t, []privateCrossWindowExitSpec{
			{exitSpec: reraceExitSpec{name: "quality"}, windowType: WindowTypeQuality},
			{exitSpec: reraceExitSpec{name: "speed"}, windowType: WindowTypeSpeed},
		}, nil)
		donorPath := privateCrossWindowFlow(4, 27, 45700)
		start := time.Now()
		if !harness.syn(donorPath) || !harness.waitForSynAck(donorPath, start.Add(5*time.Second)) {
			t.Fatal("positive original donor response missing")
		}
		old := harness.update(donorPath)
		quality := privateCrossWindowChannel(t, harness, "quality")
		if harness.boundTo(donorPath) != quality || !old.receivedInbound.Load() {
			t.Fatal("original donor owner not established")
		}
		path := privateCrossWindowFlow(4, 27, 45701)
		for _, name := range []string{"quality", "speed"} {
			privateServiceDonorHistory(harness.multiClient, privateCrossWindowChannel(t, harness, name), path, 0)
		}
		start = time.Now()
		if !harness.syn(path) || !harness.waitForSynAck(path, start.Add(5*time.Second)) || harness.boundTo(path) != quality || harness.boundTo(donorPath) != quality || quality.IsDone() {
			t.Fatal("all-failed history became a routing dead end or moved the original donor")
		}
		if len(harness.exit("speed").syns(path)) != 0 {
			t.Fatal("all-failed fallback changed the original first-window order")
		}
	})
}

// Existing inbound traffic keeps its exact flow and owner after service history
// changes. The new guard runs only during INITIAL fresh-flow adoption.
func TestPrivateGroupDonorFailureDoesNotMoveEstablishedData(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		harness := newPrivateCrossWindowHarness(t, []privateCrossWindowExitSpec{
			{exitSpec: reraceExitSpec{name: "quality"}, windowType: WindowTypeQuality},
			{exitSpec: reraceExitSpec{name: "speed"}, windowType: WindowTypeSpeed},
		}, nil)
		path := privateCrossWindowFlow(4, 28, 45800)
		start := time.Now()
		if !harness.syn(path) || !harness.waitForSynAck(path, start.Add(5*time.Second)) {
			t.Fatal("original public flow did not receive its positive response")
		}
		old := harness.update(path)
		quality := privateCrossWindowChannel(t, harness, "quality")
		privateServiceDonorHistory(harness.multiClient, quality, path, 0)
		body := []byte("synthetic established body")
		if !harness.send(reraceSegment(path, 0, body)) {
			t.Fatal("existing owner refused its unchanged body")
		}
		var received []byte
		deadline := time.Now().Add(5 * time.Second)
		for {
			harness.settle()
			received = received[:0]
			for _, packet := range harness.delivered(path) {
				parsed, payload, err := ParseIpPathWithPayload(packet.packet)
				if err != nil {
					t.Fatal(err)
				}
				if !parsed.Syn {
					received = append(received, payload...)
				}
			}
			if bytes.Equal(body, received) || !time.Now().Before(deadline) {
				break
			}
			time.Sleep(min(10*time.Millisecond, deadline.Sub(time.Now())))
		}
		if !bytes.Equal(body, received) || harness.update(path) != old || harness.boundTo(path) != quality || old.IsDone() || len(harness.exit("speed").received(path)) != 0 {
			t.Fatal("new service history changed an established exact body or owner")
		}
	})
}
