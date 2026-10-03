// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

//go:build !js

package sctp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"math"
	"net"
	"strconv"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

// These tests drive real native admission, packetization, ACK processing and
// reassembly without sockets or a write loop. They characterize the three
// service-window states observed in the legacy P2P probe diagnostic; they do
// not add a priority bypass or promise service when SCTP has no send credit.
type serviceWindowFixture struct {
	t         *testing.T
	a         *Association
	s         *Stream
	peer      net.Conn
	receive   *reassemblyQueue
	expected  [][]byte
	delivered [][]byte
	sentTSNs  map[uint32]bool
}

type serviceWindowWriteResult struct {
	n   int
	err error
}

type serviceWindowWrite struct {
	done     chan serviceWindowWriteResult
	payload  []byte
	expected []byte
}

type serviceWindowChunk struct {
	tsn   uint32
	bytes int
}

func newServiceWindowFixture(t *testing.T, interleaving, unordered bool) *serviceWindowFixture {
	t.Helper()
	left, right := net.Pipe()
	a := createTestAssociation(t, Config{MTU: 1191, BlockWrite: true, NetConn: left})
	a.setState(established)
	a.initialTSN, a.myNextTSN, a.minTSN2MeasureRTT = 100, 100, 100
	a.cumulativeTSNAckPoint, a.advancedPeerTSNAckPoint = 99, 99
	a.peerInterleaving, a.peerIForwardTSN, a.peerForwardTSN = interleaving, interleaving, true
	require.NoError(t, a.updateInterleavingState())
	a.setRWND(512 * 1024)
	a.ssthresh = 4 * a.MTU()
	s, err := a.OpenStream(1, PayloadTypeWebRTCBinary)
	require.NoError(t, err)
	s.SetReliabilityParams(unordered, ReliabilityTypeReliable, 0)
	return &serviceWindowFixture{
		t: t, a: a, s: s, peer: right, receive: newReassemblyQueue(1, 0), sentTSNs: map[uint32]bool{},
	}
}

func (f *serviceWindowFixture) close() {
	// createTestAssociation starts the timer loop but not the read/write loops.
	// Public Close waits for the absent read loop. Wake native admission before
	// internal close, and cancel the stream timer while still inside the bubble.
	f.a.lock.Lock()
	f.a.setState(closed)
	f.a.unblockPendingWrites()
	f.a.lock.Unlock()
	_ = f.s.SetWriteDeadline(time.Time{})
	_ = f.a.close()
	_ = f.peer.Close()
	synctest.Wait()
}

func serviceWindowPayload(n int, marker byte) []byte {
	payload := make([]byte, n)
	for i := range payload {
		payload[i] = marker + byte(i%251)
	}
	return payload
}

func (f *serviceWindowFixture) write(payload []byte) {
	f.t.Helper()
	f.a.lock.Lock()
	pending := f.a.writePending
	f.a.lock.Unlock()
	require.False(f.t, pending, "synchronous test write would wait for previous pending DATA")
	want := bytes.Clone(payload)
	n, err := f.s.WriteSCTP(payload, PayloadTypeWebRTCBinary)
	require.NoError(f.t, err)
	require.Equal(f.t, len(payload), n)
	f.expected = append(f.expected, want)
	// The caller may reuse its buffer immediately after admission. Reassembly
	// below must still recover the original bytes, not this mutation.
	clear(payload)
}

func (f *serviceWindowFixture) startWrite(payload []byte) serviceWindowWrite {
	f.t.Helper()
	w := serviceWindowWrite{
		done: make(chan serviceWindowWriteResult, 1), payload: payload, expected: bytes.Clone(payload),
	}
	go func() {
		n, err := f.s.WriteSCTP(payload, PayloadTypeWebRTCBinary)
		w.done <- serviceWindowWriteResult{n: n, err: err}
	}()
	return w
}

func (f *serviceWindowFixture) blocked(w serviceWindowWrite) {
	f.t.Helper()
	synctest.Wait()
	select {
	case result := <-w.done:
		f.t.Fatalf("write returned before native pending DATA drained: n=%d err=%v", result.n, result.err)
	default:
	}
}

func (f *serviceWindowFixture) finish(w serviceWindowWrite, wantErr error) {
	f.t.Helper()
	synctest.Wait()
	select {
	case result := <-w.done:
		if wantErr != nil {
			require.ErrorIs(f.t, result.err, wantErr)
			require.Zero(f.t, result.n)
			return
		}
		require.NoError(f.t, result.err)
		require.Equal(f.t, len(w.expected), result.n)
		f.expected = append(f.expected, w.expected)
		clear(w.payload)
	default:
		f.t.Fatal("write remained blocked after native writable notification")
	}
}

func (f *serviceWindowFixture) pop() []serviceWindowChunk {
	budget, consumed := int64(math.MaxInt64), false
	return f.popWithBudget(&budget, &consumed)
}

func (f *serviceWindowFixture) popWithBudget(budget *int64, consumed *bool) []serviceWindowChunk {
	f.t.Helper()
	f.a.lock.Lock()
	defer f.a.lock.Unlock()
	chunks, resets := f.a.popPendingDataChunksToSend(budget, consumed)
	require.Empty(f.t, resets)
	result := make([]serviceWindowChunk, 0, len(chunks))
	for _, c := range chunks {
		require.False(f.t, f.sentTSNs[c.tsn], "original DATA was admitted twice")
		f.sentTSNs[c.tsn] = true
		result = append(result, serviceWindowChunk{tsn: c.tsn, bytes: len(c.userData)})
		// Cross a real serialization ownership boundary. Gap ACK processing
		// clears sender userData; it must not erase a retained receiver fragment.
		raw, err := c.marshal()
		require.NoError(f.t, err)
		received := &chunkPayloadData{}
		require.NoError(f.t, received.unmarshal(raw))
		_, err = f.receive.pushWithError(received)
		require.NoError(f.t, err)
	}
	for {
		buffer := make([]byte, 65536)
		n, ppi, err := f.receive.read(buffer)
		if err == errTryAgain {
			break
		}
		require.NoError(f.t, err)
		require.Equal(f.t, PayloadTypeWebRTCBinary, ppi)
		f.delivered = append(f.delivered, bytes.Clone(buffer[:n]))
	}
	return result
}

func (f *serviceWindowFixture) ack(cumulative, rwnd uint32, gaps ...gapAckBlock) error {
	f.a.lock.Lock()
	defer f.a.lock.Unlock()
	return f.a.handleSack(&chunkSelectiveAck{
		cumulativeTSNAck: cumulative, advertisedReceiverWindowCredit: rwnd, gapAckBlocks: gaps,
	})
}

func (f *serviceWindowFixture) state(pending, inflight int, buffered uint64) {
	f.t.Helper()
	f.a.lock.Lock()
	gotPending, gotInflight := f.a.pendingQueue.getNumBytes(), f.a.inflightQueue.getNumBytes()
	f.a.lock.Unlock()
	require.Equal(f.t, pending, gotPending, "native pending bytes")
	require.Equal(f.t, inflight, gotInflight, "native inflight bytes")
	require.Equal(f.t, buffered, f.s.BufferedAmount(), "stream buffered ownership")
}

func (f *serviceWindowFixture) finishAll() {
	f.t.Helper()
	f.a.lock.Lock()
	lastTSN := f.a.myNextTSN - 1
	f.a.lock.Unlock()
	require.NoError(f.t, f.ack(lastTSN, 512*1024))
	f.state(0, 0, 0)
	require.Zero(f.t, f.receive.getNumBytes())
	require.Len(f.t, f.delivered, len(f.expected), "missing or duplicate user messages")
	for i := range f.expected {
		require.Equal(f.t, len(f.expected[i]), len(f.delivered[i]), "message %d length", i)
		require.Equal(f.t, sha256.Sum256(f.expected[i]), sha256.Sum256(f.delivered[i]), "message %d content/order", i)
	}
	require.Equal(f.t, sha256.Sum256(bytes.Join(f.expected, nil)), sha256.Sum256(bytes.Join(f.delivered, nil)))
}

func TestAssociationServiceWindowResidualCredit(t *testing.T) {
	for _, queuedBulk := range []bool{false, true} {
		name := "small_write_uses_available_credit"
		if queuedBulk {
			name = "previous_bulk_head_blocks_same_credit"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newServiceWindowFixture(t, true, true)
				defer f.close()
				f.a.setCWND(4925)
				f.write(serviceWindowPayload(4092, 1))
				prime := f.pop()
				require.Len(t, prime, 4)
				f.state(0, 4092, 4092)
				require.Equal(t, uint32(833), f.a.CWND()-4092)
				if !queuedBulk {
					f.write(serviceWindowPayload(230, 2))
					require.Equal(t, []serviceWindowChunk{{tsn: 104, bytes: 230}}, f.pop())
					f.finishAll()
					return
				}
				f.write(serviceWindowPayload(1204, 2))
				require.Empty(t, f.pop())
				w := f.startWrite(serviceWindowPayload(230, 3))
				f.blocked(w)
				f.state(1204, 4092, 4092+1204+230)
				start := time.Now()
				// Simulated time only. There is no SACK or native writer loop.
				time.Sleep(6400 * time.Millisecond)
				f.blocked(w)
				require.Equal(t, 6400*time.Millisecond, time.Since(start))
				require.Empty(t, f.pop(), "positive residual credit does not bypass the pending head")
				require.NoError(t, f.ack(prime[len(prime)-1].tsn, 512*1024))
				require.Equal(t, []serviceWindowChunk{{tsn: 104, bytes: 1156}, {tsn: 105, bytes: 48}}, f.pop())
				f.finish(w, nil)
				f.state(230, 1204, 1434)
				require.Equal(t, []serviceWindowChunk{{tsn: 106, bytes: 230}}, f.pop())
				f.finishAll()
			})
		})
	}
}

func TestAssociationServiceWindowRTOAndSelectiveCredit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newServiceWindowFixture(t, true, true)
		defer f.close()
		f.a.setCWND(12198)
		f.write(serviceWindowPayload(11862, 1))
		prime := f.pop()
		require.Len(t, prime, 11)
		f.write(serviceWindowPayload(1204, 2))
		require.Empty(t, f.pop())
		w := f.startWrite(serviceWindowPayload(230, 3))
		f.blocked(w)
		f.a.onRetransmissionTimeout(timerT3RTX, 1)
		require.Equal(t, uint32(1191), f.a.CWND())
		f.state(1204, 11862, 11862+1204+230)
		require.Empty(t, f.pop(), "RTO credit must not be bypassed for small traffic")
		// A gap-only ACK frees six full fragments but leaves flight above cwnd.
		gap := gapAckBlock{start: 2, end: 7}
		require.NoError(t, f.ack(99, 512*1024, gap))
		f.state(1204, 4926, 4926+1204+230)
		require.Empty(t, f.pop())
		f.blocked(w)
		// Duplicate/old SACKs must not release bytes twice; invalid ranges must
		// not partially consume the otherwise valid prefix.
		require.NoError(t, f.ack(99, 512*1024, gap))
		require.NoError(t, f.ack(98, 512*1024))
		require.ErrorIs(t, f.ack(99, 512*1024, gapAckBlock{start: 2, end: 12}), ErrTSNRequestNotExist)
		f.state(1204, 4926, 4926+1204+230)
		require.NoError(t, f.ack(prime[len(prime)-1].tsn, 512*1024))
		require.Len(t, f.pop(), 2)
		f.finish(w, nil)
		require.Len(t, f.pop(), 1)
		f.finishAll()
	})
}

func TestAssociationServiceWindowFragmentTail(t *testing.T) {
	for _, interleaving := range []bool{false, true} {
		for _, size := range []int{1156, 1191, 1192, 1206} {
			name := "data"
			if interleaving {
				name = "i_data"
			}
			t.Run(name+"/"+strconv.Itoa(size), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					f := newServiceWindowFixture(t, interleaving, true)
					defer f.close()
					f.a.setCWND(1191)
					f.write(serviceWindowPayload(size, 1))
					first := f.pop()
					require.NotEmpty(t, first)
					if size <= 1191 {
						f.state(0, size, uint64(size))
						f.write(serviceWindowPayload(230, 2))
						require.NoError(t, f.ack(first[len(first)-1].tsn, 512*1024))
						require.Len(t, f.pop(), 1)
					} else {
						maxPayload := int(maxPayloadSizeForMTU(1191, interleaving))
						require.Equal(t, []serviceWindowChunk{{tsn: 100, bytes: maxPayload}}, first)
						f.state(size-maxPayload, maxPayload, uint64(size))
						w := f.startWrite(serviceWindowPayload(230, 2))
						f.blocked(w)
						require.Empty(t, f.pop())
						require.NoError(t, f.ack(100, 512*1024))
						require.Equal(t, []serviceWindowChunk{{tsn: 101, bytes: size - maxPayload}}, f.pop())
						f.finish(w, nil)
						require.Len(t, f.pop(), 1)
					}
					f.finishAll()
				})
			})
		}
	}
}

func TestAssociationServiceWindowZeroReceiverWindow(t *testing.T) {
	for _, inflight := range []bool{false, true} {
		name := "one_chunk_probe_when_empty"
		if inflight {
			name = "no_new_data_with_outstanding_flight"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newServiceWindowFixture(t, true, true)
				defer f.close()
				f.a.setCWND(4096)
				if inflight {
					f.write(serviceWindowPayload(100, 1))
					require.Len(t, f.pop(), 1)
				}
				f.a.setRWND(0)
				f.write(serviceWindowPayload(230, 2))
				if inflight {
					require.Empty(t, f.pop())
					w := f.startWrite(serviceWindowPayload(40, 3))
					f.blocked(w)
					require.NoError(t, f.ack(100, 4096))
					require.Len(t, f.pop(), 1)
					f.finish(w, nil)
					require.Len(t, f.pop(), 1)
				} else {
					require.Equal(t, []serviceWindowChunk{{tsn: 100, bytes: 230}}, f.pop())
					require.Zero(t, f.a.RWND())
					f.write(serviceWindowPayload(40, 3))
					require.Empty(t, f.pop(), "zero-window exception permits one outstanding chunk, not unlimited data")
					require.NoError(t, f.ack(100, 0))
					require.Equal(t, []serviceWindowChunk{{tsn: 101, bytes: 40}}, f.pop())
				}
				f.finishAll()
			})
		})
	}
}

func TestAssociationServiceWindowTLRBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newServiceWindowFixture(t, true, true)
		defer f.close()
		f.a.setCWND(4096)
		f.a.tlrActive = true
		f.write(serviceWindowPayload(230, 1))
		budget, consumed := int64(0), true
		require.Empty(t, f.popWithBudget(&budget, &consumed))
		f.state(230, 0, 230)
		w := f.startWrite(serviceWindowPayload(40, 2))
		f.blocked(w)
		// The existing TLR rule allows the first packet of a fresh burst even
		// with a zero budget, but not a second packet in that exhausted burst.
		consumed = false
		require.Len(t, f.popWithBudget(&budget, &consumed), 1)
		require.True(t, consumed)
		require.Zero(t, budget)
		f.finish(w, nil)
		require.Empty(t, f.popWithBudget(&budget, &consumed))
		f.state(40, 230, 270)
		consumed = false
		require.Len(t, f.popWithBudget(&budget, &consumed), 1)
		f.finishAll()
	})
}

func TestAssociationServiceWindowDeadlineRollback(t *testing.T) {
	for _, tc := range []struct {
		name         string
		interleaving bool
		unordered    bool
	}{
		{name: "ordered_data"},
		{name: "unordered_data", unordered: true},
		{name: "ordered_i_data", interleaving: true},
		{name: "unordered_i_data", interleaving: true, unordered: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newServiceWindowFixture(t, tc.interleaving, tc.unordered)
				defer f.close()
				f.a.setCWND(1191)
				f.write(serviceWindowPayload(1206, 1))
				first := f.pop()
				require.Len(t, first, 1)
				ssn, orderedMID, unorderedMID := f.s.sequenceNumber, f.s.nextOrderedMID, f.s.nextUnorderedMID
				require.NoError(t, f.s.SetWriteDeadline(time.Now().Add(15*time.Second)))
				w := f.startWrite(serviceWindowPayload(230, 2))
				f.blocked(w)
				clear(w.payload) // packetize has copied it before blocking
				f.state(1206-first[0].bytes, first[0].bytes, 1436)
				time.Sleep(15*time.Second - time.Nanosecond)
				f.blocked(w)
				time.Sleep(time.Nanosecond)
				f.finish(w, context.DeadlineExceeded)
				require.Equal(t, ssn, f.s.sequenceNumber)
				require.Equal(t, orderedMID, f.s.nextOrderedMID)
				require.Equal(t, unorderedMID, f.s.nextUnorderedMID)
				f.state(1206-first[0].bytes, first[0].bytes, 1206)
				require.NoError(t, f.s.SetWriteDeadline(time.Time{}))
				require.NoError(t, f.ack(100, 512*1024))
				require.Len(t, f.pop(), 1)
				f.write(serviceWindowPayload(230, 2))
				require.Len(t, f.pop(), 1)
				f.finishAll()
			})
		})
	}
}

func TestAssociationServiceWindowCanceledAdmission(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newServiceWindowFixture(t, true, true)
		defer f.close()
		f.a.setCWND(1191)
		f.write(serviceWindowPayload(1206, 1))
		first := f.pop()
		require.Len(t, first, 1)
		// Test the association's context cancellation separately from Stream's
		// deadline/sequence rollback. This candidate is never stream-owned.
		candidate := &chunkPayloadData{
			streamIdentifier: 1, unordered: true, beginningFragment: true, endingFragment: true,
			iData: true, messageIdentifier: 1, payloadType: PayloadTypeWebRTCBinary,
			userData: serviceWindowPayload(230, 2),
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- f.a.sendPayloadData(ctx, []*chunkPayloadData{candidate}) }()
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("native admission returned before cancellation: %v", err)
		default:
		}
		cancel()
		synctest.Wait()
		require.ErrorIs(t, <-done, context.Canceled)
		f.state(50, 1156, 1206)
		require.Zero(t, candidate.tsn)
		require.Zero(t, candidate.nSent)
		require.NoError(t, f.ack(100, 512*1024))
		require.Len(t, f.pop(), 1)
		f.write(serviceWindowPayload(230, 2))
		require.Len(t, f.pop(), 1)
		f.finishAll()
	})
}

func TestAssociationServiceWindowPriorityInterleavingEligibility(t *testing.T) {
	for _, tc := range []struct {
		name         string
		interleaving bool
		unordered    bool
	}{
		{name: "unordered_i_data_can_deliver_urgent_mid_first", interleaving: true, unordered: true},
		{name: "ordered_i_data_retains_receiver_head_of_line", interleaving: true},
		{name: "unordered_data_cannot_interleave_fragment_tsns", unordered: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// This is a protocol-eligibility control, not a scheduler prototype.
			// Deliberately place a later message's TSN between two bulk fragments.
			// Only negotiated unordered I-DATA provides early delivery AND intact
			// bulk reassembly; blindly doing this for legacy DATA is invalid.
			s := newTestPacketizingStream(t, tc.interleaving, maxPayloadSizeForMTU(1191, tc.interleaving))
			s.SetReliabilityParams(tc.unordered, ReliabilityTypeReliable, 0)
			bulkPayload, urgentPayload := serviceWindowPayload(1206, 1), serviceWindowPayload(230, 2)
			bulk, _ := s.packetize(bulkPayload, PayloadTypeWebRTCBinary)
			urgent, _ := s.packetize(urgentPayload, PayloadTypeWebRTCBinary)
			require.Len(t, bulk, 2)
			require.Len(t, urgent, 1)
			bulk[0].tsn, urgent[0].tsn, bulk[1].tsn = 100, 101, 102
			receiver := newReassemblyQueue(1, 0)
			deliver := func(c *chunkPayloadData) {
				t.Helper()
				raw, err := c.marshal()
				require.NoError(t, err)
				received := &chunkPayloadData{}
				require.NoError(t, received.unmarshal(raw))
				_, err = receiver.pushWithError(received)
				require.NoError(t, err)
			}
			read := func(want []byte) {
				t.Helper()
				buffer := make([]byte, 2048)
				n, ppi, err := receiver.read(buffer)
				require.NoError(t, err)
				require.Equal(t, PayloadTypeWebRTCBinary, ppi)
				require.Equal(t, len(want), n)
				require.Equal(t, sha256.Sum256(want), sha256.Sum256(buffer[:n]))
			}
			blockedRead := func() {
				t.Helper()
				_, _, err := receiver.read(make([]byte, 2048))
				require.ErrorIs(t, err, errTryAgain)
			}
			deliver(bulk[0])
			blockedRead()
			deliver(urgent[0])
			if tc.unordered {
				read(urgentPayload)
			} else {
				blockedRead()
			}
			deliver(bulk[1])
			if !tc.interleaving {
				blockedRead()
				require.Equal(t, len(bulkPayload), receiver.getNumBytes(), "noncontiguous legacy DATA remains incomplete")
				return
			}
			read(bulkPayload)
			if !tc.unordered {
				read(urgentPayload)
			}
			require.Zero(t, receiver.getNumBytes())
			blockedRead()
		})
	}
}
