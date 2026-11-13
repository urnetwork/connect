package connect

import (
	"bytes"
	"context"
	"math"
	"net"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// The real socket reader and recovery worker retain a 21-segment lost prefix.
// A bounded synthetic inner receiver buffers its three later segments, then
// returns cumulative ACKs through the ordinary established-flow entry after a
// positive RTT. Transfer has already succeeded at this injected loss boundary.
func TestTcpReturnFlightRepairsLongPrefixWithDelayedAcks(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, testCase := range []struct {
		maxPackets int
		wantRounds int
	}{
		{maxPackets: 1, wantRounds: 21},
		{maxPackets: 4, wantRounds: 6},
	} {
		synctest.Test(t, func(t *testing.T) {
			checkTcpReturnFlightRtt(t, testCase.maxPackets, testCase.wantRounds)
		})
	}
}

func checkTcpReturnFlightRtt(t *testing.T, maxPackets int, wantRounds int) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	local, origin := net.Pipe()
	settings := DefaultTcpBufferSettingsWithBufferSize(16)
	settings.Mtu = 44
	settings.ReadBufferByteCount = 128
	settings.ReturnRecoveryMaxPacketCount = maxPackets
	settings.MemoryBudget = NewTransferMemoryBudget(2 * 1024 * 1024)
	settings.Log = NewNoopLogger()
	settings.DialContextSettings = &DialContextSettings{DialContext: func(context.Context, string, string) (net.Conn, error) {
		return local, nil
	}}
	const segmentBytes = 4
	const lostSegments = 21
	const suffixSegments = 3
	const rtt = 150 * time.Millisecond
	want := make([]byte, segmentBytes*(lostSegments+suffixSegments))
	for index := range want {
		want[index] = byte(index + 1)
	}
	type delayedAck struct {
		at  time.Time
		ack uint32
	}
	acks := make(chan delayedAck, lostSegments+suffixSegments)
	var sequence *TcpSequence
	applyAck := func(number uint32) {
		packet := MessagePoolGet(40)
		if !sequence.applyEstablishedPureAck(sequence.source, TransferKey{}, &parsedTcp{
			ack: true, seq: 101, ackNumber: number, windowSize: math.MaxUint16,
		}, packet) {
			MessagePoolReturn(packet)
			if ctx.Err() == nil {
				t.Error("delayed ordinary inner ACK was refused")
			}
		}
	}
	var stateLock sync.Mutex
	var base, next uint32
	var received []byte
	seen := make(map[uint32]bool)
	pending := make(map[uint32][]byte)
	originals, repairs := 0, 0
	sequence = NewTcpSequence(ctx, func(_ TransferPath, _ protocol.ProvideMode, _ *IpPath, packet []byte) {
		path, payload, err := ParseIpPathWithPayload(packet)
		if err != nil || path == nil || path.Protocol != IpProtocolTcp || !path.Ack {
			t.Errorf("invalid real return packet: %v", err)
			return
		}
		stateLock.Lock()
		defer stateLock.Unlock()
		if path.Syn {
			base, next = path.SequenceNumber+1, path.SequenceNumber+1
			applyAck(base)
			return
		}
		if len(payload) == 0 {
			return
		}
		if len(payload) != segmentBytes || path.Fin || path.Rst ||
			int32(path.SequenceNumber-base) < 0 || int32(path.SequenceNumber+uint32(len(payload))-(base+uint32(len(want)))) > 0 {
			t.Error("return replay changed the retained payload or emitted outside its original flight")
			return
		}
		if !seen[path.SequenceNumber] {
			seen[path.SequenceNumber] = true
			originals++
			if int32(path.SequenceNumber-(base+lostSegments*segmentBytes)) < 0 {
				return
			}
		} else {
			repairs++
			if repairs > lostSegments+maxPackets-1 {
				t.Error("bounded recovery repeated more than the lost prefix and one flight's unknown suffix")
				return
			}
		}
		if int32(path.SequenceNumber-next) >= 0 {
			pending[path.SequenceNumber] = bytes.Clone(payload)
		}
		for {
			payload, ok := pending[next]
			if !ok {
				break
			}
			delete(pending, next)
			received = append(received, payload...)
			next += uint32(len(payload))
		}
		select {
		case acks <- delayedAck{at: time.Now().Add(rtt), ack: next}:
		default:
			t.Error("bounded delayed-ACK fixture overflowed")
		}
	}, SourceId(NewId()), protocol.ProvideMode_Network, 4,
		net.IPv4(192, 0, 2, 2).To4(), 41000, net.IPv4(198, 51, 100, 2).To4(), 443, 100, settings)
	if sequence == nil {
		cancel()
		local.Close()
		origin.Close()
		t.Fatal("real TCP flow could not reserve its fixed root claim")
	}
	ackDone, runDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(ackDone)
		for {
			select {
			case <-ctx.Done():
				return
			case event := <-acks:
				timer := time.NewTimer(time.Until(event.at))
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
				applyAck(event.ack)
			}
		}
	}()
	go func() { defer close(runDone); sequence.Run() }()
	defer func() {
		cancel()
		origin.Close()
		<-runDone
		<-ackDone
		if used := settings.MemoryBudget.UsedByteCount(); used != 0 {
			t.Errorf("joined RTT fixture retained %d root-budget bytes", used)
		}
	}()
	packet := MessagePoolGet(40)
	if accepted, err := sequence.send(&TcpSendItem{ipPacket: packet, tcp: parsedTcp{syn: true, seq: 100, windowSize: math.MaxUint16}}, -1); !accepted {
		MessagePoolReturn(packet)
		t.Fatalf("real SYN admission: %v", err)
	}
	synctest.Wait()
	started := time.Now()
	if count, err := origin.Write(want); err != nil || count != len(want) {
		t.Fatalf("real origin write: count=%d err=%v", count, err)
	}
	synctest.Wait()
	stateLock.Lock()
	premise := originals == lostSegments+suffixSegments && repairs == 0 && next == base && len(pending) == suffixSegments
	stateLock.Unlock()
	if !premise {
		t.Fatal("the real socket reader did not establish the exact lost prefix and buffered suffix")
	}
	time.Sleep(6 * rtt)
	synctest.Wait()
	stateLock.Lock()
	complete := bytes.Equal(received, want)
	stateLock.Unlock()
	if complete != (maxPackets == 4) {
		t.Fatalf("six-RTT criterion: packet_limit=%d complete=%t", maxPackets, complete)
	}
	if wantRounds > 6 {
		time.Sleep(time.Duration(wantRounds-6) * rtt)
		synctest.Wait()
	}
	stateLock.Lock()
	complete = bytes.Equal(received, want)
	gotRepairs := repairs
	stateLock.Unlock()
	if !complete || time.Since(started) != time.Duration(wantRounds)*rtt {
		t.Fatalf("long-prefix repair: packet_limit=%d complete=%t elapsed=%s rounds=%d repairs=%d", maxPackets, complete, time.Since(started), wantRounds, gotRepairs)
	}
	// The last cumulative ACK, rather than a successful callback, releases
	// the retained chunks. Unknown buffered suffixes cost at most one flight.
	time.Sleep(rtt)
	synctest.Wait()
	sequence.mutex.Lock()
	retained := sequence.returnByteCount
	sequence.mutex.Unlock()
	if retained != 0 {
		t.Fatalf("final delayed cumulative ACK left %d retained bytes", retained)
	}
}
