// A held source-only audit of group prebinding versus exact-service history.
// This is not attribution for the captured public100 timing failure.
package connect

import (
	"bytes"
	"testing"
	"testing/synctest"
	"time"
)

// A retained donor needs completed bidirectional stats, not just a published
// SYNACK. Real bytes form four buckets at the unchanged production cadence;
// WindowStats excludes the latest two before publishing positive health.
func privateQualifyServiceDonorHealth(t *testing.T, harness *reraceHarness, path *IpPath, owner *multiClientChannel) {
	t.Helper()
	want := []byte("abc")
	for index := range want {
		harness.sleep(harness.multiClient.settings.StatsWindowBucketDuration + time.Nanosecond)
		if !harness.send(reraceSegment(path, uint32(index), want[index:index+1])) {
			t.Fatal("SDK_GROUP_DONOR_SETUP_WARM_SEND: actual donor bytes were not admitted")
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			harness.settle()
			var got []byte
			for _, packet := range harness.delivered(path) {
				parsed, payload, err := ParseIpPathWithPayload(packet.packet)
				if err != nil {
					t.Fatal(err)
				}
				if !parsed.Syn {
					got = append(got, payload...)
				}
			}
			if bytes.Equal(got, want[:index+1]) {
				break
			}
			if !time.Now().Before(deadline) {
				t.Fatalf("SDK_GROUP_DONOR_SETUP_WARM_ECHO: expected=%d received=%d", index+1, len(got))
			}
			time.Sleep(min(10*time.Millisecond, deadline.Sub(time.Now())))
		}
	}
	stats, err := owner.WindowStats()
	if err != nil {
		t.Fatal("SDK_GROUP_DONOR_SETUP_STATS: ", err)
	}
	sent, received := stats.EffectiveByteCount()
	t.Logf("donor health receipt: ipv%d completed_buckets=%d duration=%s effective_send=%d effective_receive=%d healthy=%t transport=%t warning=%t done=%t echoed_bytes=%d", path.Version, stats.bucketCount, stats.windowDuration, sent, received, owner.positiveAffinityHealth(), owner.hasActiveTransport(), owner.isWarning(), owner.IsDone(), len(want))
	if stats.bucketCount < 2 || sent <= 0 || received <= 0 || !owner.positiveAffinityHealth() || !owner.hasActiveTransport() || owner.isWarning() || owner.IsDone() {
		t.Fatal("SDK_GROUP_DONOR_SETUP_POSITIVE_HEALTH: retained donor did not qualify through actual stats and transport")
	}
}

// Three real fresh tuples share one destination: a retained established donor,
// an observed refused connect, then the new SYN whose selection is audited.
func privateTestGroupDonorServiceFailure(t *testing.T, version int) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		const donorPort, refusedPort, freshPort = 45600, 45601, 45602
		firstRoute := make(chan string, 1)
		observe := func(name string) func(*IpPath) reraceAnswer {
			return func(path *IpPath) reraceAnswer {
				if path.SourcePort == freshPort {
					select {
					case firstRoute <- name:
					default:
					}
				}
				if name == "quality" && path.SourcePort != donorPort {
					return reraceSignal
				}
				return reraceAccept
			}
		}
		harness := newPrivateCrossWindowHarness(t, []privateCrossWindowExitSpec{
			{exitSpec: reraceExitSpec{name: "quality", answer: observe("quality")}, windowType: WindowTypeQuality},
			{exitSpec: reraceExitSpec{name: "speed", answer: observe("speed")}, windowType: WindowTypeSpeed},
		}, nil)
		if !harness.multiClient.settings.DestinationAffinity || harness.multiClient.reliabilitySettings().AffinityMaxTime <= 0 {
			t.Fatal("ordinary destination-group affinity was not enabled")
		}
		quality := privateCrossWindowChannel(t, harness, "quality")
		speed := privateCrossWindowChannel(t, harness, "speed")
		donorPath := privateCrossWindowFlow(version, 26, donorPort)
		start := time.Now()
		if !harness.syn(donorPath) || !harness.waitForSynAck(donorPath, start.Add(5*time.Second)) || harness.boundTo(donorPath) != quality {
			t.Fatal("retained donor did not establish through the original quality owner")
		}
		donor := harness.update(donorPath)
		if donor == nil {
			t.Fatal("SDK_GROUP_DONOR_SETUP_UPDATE: original donor update missing")
		}
		t.Logf("donor initial receipt: ipv%d inbound=%t pinned=%t healthy=%t", version, donor.receivedInbound.Load(), donor.pinned, quality.positiveAffinityHealth())
		if !donor.receivedInbound.Load() {
			t.Fatal("SDK_GROUP_DONOR_SETUP_INBOUND: original SYNACK did not establish inbound ownership")
		}
		if donor.pinned {
			t.Fatal("SDK_GROUP_DONOR_SETUP_PIN: original ordinary donor was pinned")
		}
		privateQualifyServiceDonorHealth(t, harness, donorPath, quality)

		refused := make(chan struct{}, 1)
		harness.multiClient.stateLock.Lock()
		harness.multiClient.beforeDialFailureStandingForTest = func(update *multiClientChannelUpdate) {
			if update.ipPath.SourcePort == refusedPort {
				select {
				case refused <- struct{}{}:
				default:
				}
			}
		}
		harness.multiClient.stateLock.Unlock()
		refusedPath := privateCrossWindowFlow(version, 26, refusedPort)
		if !harness.syn(refusedPath) {
			t.Fatal("actual refused tuple was not admitted")
		}
		select {
		case <-refused:
		case <-time.After(5 * time.Second):
			t.Fatal("positive quality refusal never reached the consumer")
		}
		harness.settle()
		privateCrossWindowAssertFailed(t, harness, refusedPath, "quality")
		t.Logf("donor after-refusal receipt: ipv%d owner_retained=%t inbound=%t healthy=%t warning=%t done=%t transport=%t", version, harness.boundTo(donorPath) == quality, donor.receivedInbound.Load(), quality.positiveAffinityHealth(), quality.isWarning(), quality.IsDone(), quality.hasActiveTransport())
		if harness.boundTo(donorPath) != quality || !donor.receivedInbound.Load() || quality.isWarning() || quality.IsDone() || !quality.positiveAffinityHealth() {
			t.Fatal("exact-service refusal changed the retained established donor's global health or owner")
		}
		if speed.isWarning() || speed.IsDone() || !speed.supportsIpVersion(version) {
			t.Fatal("ordinary unfailed speed alternative was not available")
		}
		freshPath := privateCrossWindowFlow(version, 26, freshPort)
		if !harness.multiClient.destinationServiceFailuresExhausted([]*multiClientChannel{quality}, freshPath) || harness.multiClient.destinationServiceFailuresExhausted([]*multiClientChannel{speed}, freshPath) {
			t.Fatal("exact fresh service did not preserve quality-only standing failure memory")
		}
		t.Logf("group donor audit premises: ipv%d established_quality_donor=true globally_healthy=true exact_service_failed=true speed_unfailed=true fresh_unpinned_tuple=true", version)
		start = time.Now()
		if !harness.syn(freshPath) {
			t.Fatal("fresh source SYN was not admitted to an available owner")
		}
		var first string
		select {
		case first = <-firstRoute:
		case <-time.After(5 * time.Second):
			t.Fatal("positive first-provider callback was not observed")
		}
		if first != "speed" {
			t.Fatalf("SDK_GROUP_DONOR_REUSED_KNOWN_FAILED_SERVICE: ipv%d first=%s quality_syns=%d speed_syns=%d retained_owner=%t", version, first, len(harness.exit("quality").syns(freshPath)), len(harness.exit("speed").syns(freshPath)), harness.boundTo(donorPath) == quality)
		}
		if !harness.waitForSynAck(freshPath, start.Add(5*time.Second)) || harness.boundTo(freshPath) != speed || harness.boundTo(donorPath) != quality || donor.IsDone() {
			t.Fatal("fresh recovery changed the original established donor or missed the unchanged response bound")
		}
	})
}

// IPv4 reaches the actual group-adoption path through a public source SYN.
func TestPrivateGroupDonorKnownServiceFailureUsesHealthySpeedIp4(t *testing.T) {
	assertMessagePoolOwnership(t)
	privateTestGroupDonorServiceFailure(t, 4)
}

// IPv6 has a separate root so an IPv4 failure cannot hide its prebinding path.
func TestPrivateGroupDonorKnownServiceFailureUsesHealthySpeedIp6(t *testing.T) {
	assertMessagePoolOwnership(t)
	privateTestGroupDonorServiceFailure(t, 6)
}
