// Native metadata refreshes keep the actual failed source eligible for a
// later published provider, without issuing a third native SYN.
package connect

import (
	"bytes"
	"encoding/binary"
	"slices"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/urnetwork/connect/protocol"
)

// Explicit timestamps and identification use ordinary complete checksummed
// wire packets; no production comparator or new lineage field is needed here.
func synMetadataTestPacket(path *IpPath, timestamp uint32, identification uint16, payload []byte) []byte {
	const headerBytes = TcpHeaderSizeWithoutExtensions + 12
	packet, tcp := ipTransportPacket(path, ipProtocolNumberTcp, headerBytes+len(payload))
	binary.BigEndian.PutUint16(tcp[0:2], uint16(path.SourcePort))
	binary.BigEndian.PutUint16(tcp[2:4], uint16(path.DestinationPort))
	binary.BigEndian.PutUint32(tcp[4:8], path.SequenceNumber)
	tcp[12], tcp[13] = byte(headerBytes/4)<<4, tcpFlagSyn
	binary.BigEndian.PutUint16(tcp[14:16], 65535)
	copy(tcp[20:24], []byte{1, 1, 8, 10})
	binary.BigEndian.PutUint32(tcp[24:28], timestamp)
	copy(tcp[headerBytes:], payload)
	binary.BigEndian.PutUint16(tcp[16:18], ipPathTransportChecksum(path, ipProtocolNumberTcp, tcp))
	if path.Version == 4 {
		binary.BigEndian.PutUint16(packet[4:6], identification)
		writeIpv4HeaderChecksum(packet[:int(packet[0]&15)*4])
	}
	return packet
}

// Explicit one-byte TFO originals preserve the DATA-credit publication premise.
// The same leaf compiles against the unchanged production baseline. Its
// failure is delayed until every real client/provider and credit has joined.
func testNativeSynMetadataPublication(t *testing.T, version int, mode string) {
	partial := mode == "partial"
	t.Helper()
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		cause := ""
		t.Cleanup(func() {
			if !t.Failed() {
				t.Log("SYN_METADATA_PUBLICATION_CLEANUP_JOINED")
				if cause != "" {
					t.Fatal(cause)
				}
			}
		})
		root := NewTransferMemoryBudget(8 * 1024 * 1024)
		budget := NewTransferMemoryBudgetWithParent(root.TotalByteCount(), root)
		t.Cleanup(func() {
			for _, ledger := range []*TransferMemoryBudget{budget, root} {
				reserved, released := ledger.Counts()
				if ledger.UsedByteCount() != 0 || reserved != released {
					t.Errorf("joined metadata publication retained bytes: used=%d reserved=%d released=%d", ledger.UsedByteCount(), reserved, released)
				}
			}
		})
		var original, current *tcpSynRetry
		var originalCredit *transferRetainedControlCredit
		var oldPacket, latestPacket, paidPacket []byte
		var history, standing, raceCount, raceLimit, raceSlots int
		var priorRefusalPresent bool
		var priorRemote Id
		var originalProvider *multiClientChannel
		var initialHistory []Id
		var beforeSignals uint64
		var otherHistoryPresent bool
		wantedRefusals := 4
		if partial {
			wantedRefusals = 1
		}
		wire := make(chan []byte, 4)
		configure := func(settings *ClientSettings) {
			settings.SendBufferSettings.ResendQueueBudget = budget
			settings.SendBufferSettings.ResendQueueRetainedByteAccounting = true
		}
		var silence atomic.Bool
		options := &synPublicationPacketOptions{
			silentRepeat: &silence,
			packet: func(path *IpPath, ordinal int) []byte {
				// The maintained partial fixture offers initial / just-before-age /
				// exact-age. Only the first retry changes metadata; the third is exact.
				generation := min(ordinal, 2)
				return synMetadataTestPacket(path, uint32(100+generation), uint16(200+generation), []byte{0x64})
			},
			afterRetry: func(f *synPublicationTestFixture) {
				latestPacket = synMetadataTestPacket(f.path, 102, 202, []byte{0x64})
				f.update.stateLock.Lock()
				current = f.update.synRetry
				valid := current != nil && current.credit != nil && !protocolClassPureTcpControl(current.packet) && !current.credit.noBudget && current.credit.budget == budget && !current.credit.released.Load() &&
					current.source == f.h.source && current.sequence == f.path.SequenceNumber && current.validWithLock(f.update) &&
					(bytes.Equal(current.packet, oldPacket) || bytes.Equal(current.packet, latestPacket)) &&
					!f.update.receivedInbound.Load() && f.update.client.Load() == nil
				if current != nil {
					history = len(current.refusedRemoteIds)
					paidPacket = bytes.Clone(current.packet)
					priorRefusalPresent = f.update.synRetryRemoteTailRefusedWithLock(&multiClientChannel{args: &multiClientChannelArgs{Destination: RequireMultiHopId(priorRemote)}})
					otherHistoryPresent = true
					for _, remote := range initialHistory {
						if remote != priorRemote && !slices.Contains(current.refusedRemoteIds, remote) {
							otherHistoryPresent = false
						}
					}
				}
				standing = len(f.update.dialFailureSignaledClients)
				if race := f.update.race; race != nil {
					raceCount, raceLimit = len(race.clientStates), race.clientLimit
					raceSlots = f.h.multiClient.synRetryRaceSlotsWithLock(f.update, nil)
				}
				pending := current != nil && current.pendingConflictingControlWithLock(f.update)
				serial, references := uint64(0), uint32(0)
				if current != nil {
					serial, references = current.serial, current.references
				}
				soleSilent := f.update.race != nil && f.update.race.clientStates[originalProvider] != nil
				f.update.stateLock.Unlock()
				renewedSignals := f.h.metrics().DialFailuresIntercepted - beforeSignals
				t.Logf("SYN_METADATA_PAID_CENSUS mode=%s same_owner=%t serial=%d original_serial=%d paid_latest=%t old_credit_released=%t standing=%d history=%d prior_remote_refused=%t race_states=%d race_limit=%d free_slots=%d pending=%t refs=%d renewed_signals=%d other_history=%t", mode, current == original, serial, original.serial, bytes.Equal(paidPacket, latestPacket), originalCredit.released.Load(), standing, history, priorRefusalPresent, raceCount, raceLimit, raceSlots, pending, references, renewedSignals, otherHistoryPresent)
				if !valid || standing != wantedRefusals {
					t.Fatal("native metadata offers lost actual paid source custody or prior standing failures")
				}
				if partial && (raceCount != 3 || raceLimit != 4 || raceSlots != 1) {
					t.Fatal("partial source lost its unchanged three-candidate race and genuine free slot")
				}
				if mode == "selective" && (raceCount != 1 || raceLimit != 4 || raceSlots != 3 || !soleSilent || renewedSignals != 3 || !otherHistoryPresent) {
					t.Fatal("selective response control did not withdraw exactly three real candidates")
				}
			},
		}
		f := newSynPublicationPacketFixture(t, version, partial, configure, options, func(f *synPublicationTestFixture) *reraceExit {
			// This constructor edge precedes both retry offers and the held listing.
			// Earlier refusals remain real; no fresh provider failure requalifies them.
			original, originalCredit = f.owner, f.owner.credit
			oldPacket = bytes.Clone(f.owner.packet)
			if len(f.owner.refusedRemoteIds) != wantedRefusals || f.owner.credit.budget != budget {
				t.Fatal("metadata control lacks its original paid provider refusal history")
			}
			originalProvider = f.h.channel("a")
			priorRemote = originalProvider.Destination().Tail()
			initialHistory = slices.Clone(f.owner.refusedRemoteIds)
			beforeSignals = f.h.metrics().DialFailuresIntercepted
			silence.Store(mode != "fresh")
			exit := f.h.addExit(reraceExitSpec{name: "late", clientSettings: configure})
			exit.provider.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
				for _, frame := range frames {
					if frame.MessageType != protocol.MessageType_IpIpPacketToProvider {
						continue
					}
					packet, err := ipPacketToProviderBytes(frame)
					if err != nil {
						continue
					}
					path, err := ParseIpPath(packet)
					if err == nil && reraceSameFlow(path, f.path) && path.Syn && !path.Ack {
						wire <- bytes.Clone(packet)
					}
				}
			})
			return exit
		})
		beforeNative := f.nativeOffers.Load()
		f.resume()
		synctest.Wait()
		_, answered := f.h.synAck(f.path)
		t.Logf("SYN_METADATA_PUBLICATION_CENSUS version=%d partial=%t standing=%d retained_refusals=%d race_states=%d race_limit=%d free_slots=%d native_before=%d native_after=%d answered=%t late_syns=%d", version, partial, standing, history, raceCount, raceLimit, raceSlots, beforeNative, f.nativeOffers.Load(), answered, len(f.lateExit.syns(f.path)))
		if f.nativeOffers.Load() != beforeNative {
			t.Fatal("publication control introduced another native retry")
		}
		if !answered {
			t.Fatal("actual paid metadata cache did not recover through the available publication slot")
		}
		f.requireAnswer(t)
		synRetryControlReleased(t, current)
		select {
		case actual := <-wire:
			if !bytes.Equal(actual, paidPacket) {
				t.Fatal("published retry did not send the exact actually paid snapshot")
			}
		default:
			t.Fatal("real late provider did not receive the actual paid cache image")
		}
		if len(wire) != 0 {
			t.Fatal("publication duplicated physical SYN ownership")
		}
		if history != wantedRefusals || !priorRefusalPresent {
			if mode != "selective" || history != 3 || priorRefusalPresent || !bytes.Equal(paidPacket, latestPacket) {
				t.Fatal("metadata history changed outside the exact one-withheld-refusal ordering")
			}
			cause = "SYN_METADATA_REMOTE_REFUSAL_LOST: paid timestamp successor forgot a prior validated remote refusal when that provider sent no fresh refusal"
		}
	})
}

// One provider withholds only its repeated refusal. Three real new refusals
// vacate ordinary race slots while the fourth original identity must survive.
func TestNativeSynMetadataRefreshPreservesPriorRefusalIpv4(t *testing.T) {
	testNativeSynMetadataPublication(t, 4, "selective")
}

// IPv6 exercises the same actual history cut without an identification field.
func TestNativeSynMetadataRefreshPreservesPriorRefusalIpv6(t *testing.T) {
	testNativeSynMetadataPublication(t, 6, "selective")
}

// Repeated fresh failures independently requalify a replacement cache. The
// original first probe passed for this ordering, so it remains a positive control.
func TestNativeSynMetadataRefreshFreshRefusalsRequalify(t *testing.T) {
	testNativeSynMetadataPublication(t, 4, "fresh")
}

// An already-owned partial race may collapse both offered retries. Publication
// must use its actual paid old image, without pretending offered bytes were cached.
func TestNativeSynMetadataCollapsedRetryPublishesActuallyPaidImage(t *testing.T) {
	testNativeSynMetadataPublication(t, 4, "partial")
}
