// Common public native inputs report allocation cost beside actual useful
// work. Covered-data feedback is a recovery/input-cost case, not a speed score.
package connect

import (
	"bytes"
	"io"
	"os"
	"testing"
	"testing/synctest"
	"time"
	"unsafe"
)

// One isolated child measures both healthy equal-work controls and the same
// pathological input whose admitted work differs before and after the fix.
func TestNativeMixedAckAllocationCensus(t *testing.T) {
	if os.Getenv(allocationCountRootEnv) == "" {
		output, err := runAllocationCountChild(t, t.Name(), "")
		t.Logf("%s", output)
		if err != nil {
			t.Fatal(err)
		}
		return
	}
	assertMessagePoolOwnership(t)
	for _, mode := range []string{"healthy-all-acks-equal-work", "healthy-fresh-data-acks-equal-work", "covered-data-feedback-input-cost"} {
		checkNativeMixedAckAllocationCase(t, mode)
	}
}

// Constructors and public calls are shared verbatim by canonical and candidate
// overlays; counts and completed socket bytes disclose their actual work.
func checkNativeMixedAckAllocationCase(t *testing.T, mode string) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		fixture := newNativeDuplicateAckFixture(t, false)
		fixture.consumer.stateLock.Lock()
		base := fixture.consumer.base
		fixture.consumer.stateLock.Unlock()
		upload := []byte("data")
		data := groupRecoveryPacket(fixture.initial+1, base, fixture.window, tcpFlagAck, upload)
		sequence := fixture.initial + 1 + uint32(len(upload))
		ack := groupRecoveryPacket(sequence, base, fixture.window, tcpFlagAck, nil)
		type readResult struct {
			valid bool
			err   error
		}
		uploaded, uploadDone := make(chan readResult, 1), make(chan struct{})
		defer func() { fixture.close(); <-uploadDone }()
		go func() {
			defer close(uploadDone)
			payload := make([]byte, len(upload))
			_, err := io.ReadFull(fixture.origin, payload)
			uploaded <- readResult{valid: bytes.Equal(payload, upload), err: err}
		}()
		if !fixture.offer(data) {
			t.Fatal("allocation census initial upload was refused")
		}
		synctest.Wait()
		select {
		case result := <-uploaded:
			if result.err != nil || !result.valid {
				t.Fatalf("allocation census actual upload: valid=%t err=%v", result.valid, result.err)
			}
		default:
			t.Fatal("allocation census initial upload did not drain")
		}
		var freshResults chan readResult
		var freshDone chan struct{}
		if mode == "healthy-fresh-data-acks-equal-work" {
			freshResults, freshDone = make(chan readResult, 1), make(chan struct{})
			defer func() { fixture.close(); <-freshDone }()
			go func() {
				defer close(freshDone)
				payload := make([]byte, 16*4)
				expected := bytes.Repeat([]byte("next"), 16)
				for {
					_, err := io.ReadFull(fixture.origin, payload)
					result := readResult{valid: bytes.Equal(payload, expected), err: err}
					select {
					case freshResults <- result:
					case <-fixture.ctx.Done():
						return
					}
					if err != nil {
						return
					}
				}
			}()
		}
		// AllocsPerRun uses one processor. One complete offered group must fit
		// before a peer ACK runs; otherwise the intentional raw-suffix capacity
		// refusal measures less useful work than this census promises.
		budgetTemplates := make([][]byte, 32)
		for index := range budgetTemplates {
			budgetTemplates[index] = ack
			if mode == "healthy-fresh-data-acks-equal-work" && index%2 == 0 {
				budgetTemplates[index] = groupRecoveryPacket(sequence+uint32(index*2), base, fixture.window, tcpFlagAck, []byte("next"))
			} else if mode == "covered-data-feedback-input-cost" && index < len(budgetTemplates)-1 {
				budgetTemplates[index] = data
			}
		}
		source := func() *SendSequence {
			fixture.sender.sendBuffer.mutex.Lock()
			defer fixture.sender.sendBuffer.mutex.Unlock()
			var source *SendSequence
			for id, candidate := range fixture.sender.sendBuffer.sendSequences {
				if id.Destination == fixture.receiver.ClientId() {
					if source != nil {
						t.Fatal("allocation census has more than one actual sender sequence")
					}
					source = candidate
				}
			}
			if source == nil {
				t.Fatal("allocation census has no actual sender sequence")
			}
			return source
		}()
		flightCost := nativeMixedAckFlightRetainedCost(t, source, budgetTemplates)
		budget := fixture.sender.settings.SendBufferSettings.ResendQueueBudget
		if budget == nil || budget.UsedByteCount() != 0 {
			t.Fatal("allocation census calibration requires an empty owned sender budget")
		}
		budget.SetTotalByteCount(flightCost.total)
		synctest.Wait()
		providerPackets := watchNativePureAckProvider(t, fixture)
		beforeProvider, beforeTerminals := providerPackets.Load(), fixture.senderTerminals.Load()
		beforeDrops, before := fixture.parent.TcpCollapseDropCount(), time.Now()
		offers, acceptedTotal, socketBytes := 0, 0, 0
		allocations := testing.AllocsPerRun(100, func() {
			templates := make([][]byte, 32)
			switch mode {
			case "healthy-all-acks-equal-work":
				for index := range templates {
					templates[index] = ack
				}
			case "healthy-fresh-data-acks-equal-work":
				for index := 0; index < len(templates); index += 2 {
					templates[index] = groupRecoveryPacket(sequence, base, fixture.window, tcpFlagAck, []byte("next"))
					sequence += 4
					templates[index+1] = groupRecoveryPacket(sequence, base, fixture.window, tcpFlagAck, nil)
				}
			case "covered-data-feedback-input-cost":
				for index := range templates[:31] {
					templates[index] = data
				}
				templates[31] = ack
			default:
				t.Fatalf("unknown allocation case %q", mode)
			}
			packets := make([][]byte, len(templates))
			membership := make([]bool, len(templates))
			for index, template := range templates {
				packets[index] = MessagePoolCopy(template)
			}
			count := fixture.parent.SendNativePacketBatchWithResults(SourceId(fixture.sender.ClientId()), fixture.parent.provideMode,
				packets, 5*time.Second, membership)
			actual := 0
			for index, admitted := range membership {
				if admitted {
					actual++
					if mode == "covered-data-feedback-input-cost" && index != len(membership)-1 {
						t.Fatal("allocation recovery case admitted covered data")
					}
				}
			}
			if actual != count || mode != "covered-data-feedback-input-cost" && count != len(templates) ||
				mode == "covered-data-feedback-input-cost" && count != 0 && count != 1 {
				t.Fatalf("allocation case changed input membership: mode=%s count=%d membership=%v", mode, count, membership)
			}
			synctest.Wait()
			if freshResults != nil {
				select {
				case result := <-freshResults:
					if result.err != nil || !result.valid {
						t.Fatalf("allocation census fresh socket bytes: valid=%t err=%v", result.valid, result.err)
					}
					socketBytes += 16 * 4
				default:
					t.Fatal("healthy equal-work census did not complete its socket write")
				}
			}
			offers++
			acceptedTotal += count
		})
		queued, queuedBytes, _ := fixture.sender.ResendQueueSize(fixture.receiver.ClientId(), MultiHopId{}, false, false)
		dispatched := providerPackets.Load() - beforeProvider
		if dispatched != int64(acceptedTotal) || queued != 0 || queuedBytes != 0 || time.Since(before) != 0 {
			t.Fatalf("allocation census lost actual useful work: mode=%s offers=%d accepted=%d provider=%d transfer=%d/%d elapsed=%s",
				mode, offers, acceptedTotal, dispatched, queued, queuedBytes, time.Since(before))
		}
		t.Logf("NATIVE_MIXED_ACK_ALLOCATION_CENSUS mode=%s offers_including_warmup=%d input_packets_per_offer=32 accepted_packets=%d provider_dispatches=%d socket_bytes=%d source_terminals=%d collapse_drops=%d allocations_per_offer=%g parsed_group_bytes=%d admission_bytes=%d sender_budget_limit=%d full_flight_chunks=%d full_flight_metadata=%d sender_budget_after=%d elapsed=%s",
			mode, offers, acceptedTotal, dispatched, socketBytes, fixture.senderTerminals.Load()-beforeTerminals,
			fixture.parent.TcpCollapseDropCount()-beforeDrops, allocations, unsafe.Sizeof(parsedPacketGroup{}),
			unsafe.Sizeof(ipPacketGroupAdmission{}), budget.TotalByteCount(), len(flightCost.chunks), flightCost.metadata,
			budget.UsedByteCount(), time.Since(before))
	})
}

// One no-contract, raw-v2 flight owns only the existing lazy range metadata
// and each transport-bounded retained item. These setup-only slices borrow
// template packet bytes; any legacy wrapper is returned before failure.
type nativeMixedAckFlightCost struct {
	total    ByteCount
	metadata ByteCount
	chunks   []ByteCount
	ends     []int
}

// Borrows templates and source. Uses the serializer's own chunk, pool-class
// and owner-envelope helpers before the allocation measurement starts.
func nativeMixedAckFlightRetainedCost(t *testing.T, source *SendSequence, templates [][]byte) nativeMixedAckFlightCost {
	t.Helper()
	settings := source.sendBufferSettings
	policy := source.transferFlightPolicy()
	if policy.h1Only || source.sendContract != nil || source.preparedContractByteCount.Load() != 0 ||
		!source.client.ContractManager().SendNoContract(source.destination) || !source.resendQueue.lifetimeBudget {
		t.Fatal("allocation flight calibration requires the actual no-contract, retained, non-H1 source")
	}
	pack := SendPack{logicalGroup: true}
	pack.pinGroupChunkLimits(policy)
	defer func() {
		for _, frame := range pack.Frames {
			if !frame.Raw {
				MessagePoolReturn(frame.MessageBytes)
			}
		}
	}()
	for _, template := range templates {
		frame, err := ipPacketToProviderFrame(template, settings.ProtocolVersion)
		if err != nil {
			t.Fatal(err)
		}
		pack.Frames = append(pack.Frames, frame)
		if !frame.Raw {
			t.Fatal("allocation flight calibration requires the fixture's raw-v2 path")
		}
	}
	group := parsedPacketGroup{packets: make([]parsedPacket, len(templates))}
	group.completionFlags.Store(groupCompletionTracked)
	cost := nativeMixedAckFlightCost{metadata: group.sendGroupMemoryByteCount()}
	cost.total = cost.metadata
	maxFrames, maxBytes := pack.groupChunkLimits()
	for start := 0; start < len(pack.Frames); {
		end := nextSendGroupChunkEndWithLimits(pack.Frames, start, maxFrames, maxBytes)
		bytes := (&sendItem{}).retainedMemoryByteCount(
			source.retainedSendFrameByteCount(MessageByteCount(pack.Frames[start:end]), end-start),
			end-start, settings.ProtocolVersion < 2)
		cost.chunks = append(cost.chunks, bytes)
		cost.ends = append(cost.ends, end)
		cost.total += bytes
		start = end
	}
	return cost
}
