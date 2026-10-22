// Actual Run/window transitions distinguish a new fully-ACKed zero-window
// episode from a probe whose dedicated callback still belongs to the old one.
package connect

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// One real socket reader, one parsed HTTP request and one bounded held probe.
// Every mutable peer observation is copied under its own short lock; no peer
// lock is held while applying an ACK, waiting or advancing the virtual clock.
type persistEpisodeFixture struct {
	t                 *testing.T
	sequence          *TcpSequence
	settings          *TcpBufferSettings
	source            TransferPath
	cancel            context.CancelFunc
	local, origin     net.Conn
	request, response []byte
	waits             *zeroWindowWaitLocker
	handshake         chan struct{}
	firstData         chan persistControlEvent
	probes            chan persistControlEvent
	probeCompleted    chan time.Time
	releaseProbe      chan struct{}
	done, receivedAll chan struct{}
	serverDone        chan error
	handshakeDo       sync.Once
	firstDataDo       sync.Once
	allDo, releaseDo  sync.Once
	closeDo           sync.Once
	probeCount        atomic.Int64
	fullWindow        atomic.Bool
	holdFirstProbe    bool
	peerStateLock     sync.Mutex
	nextAck           uint32
	received          []byte
	serverJoined      bool
}

// Callbacks and the real reader-wait observation are installed before Run.
func newPersistEpisodeFixture(t *testing.T, holdFirstProbe bool) *persistEpisodeFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	local, origin := net.Pipe()
	settings := DefaultTcpBufferSettingsWithBufferSize(4)
	settings.Log = NewNoopLogger()
	settings.Mtu = 44
	settings.ReadBufferByteCount = 128
	settings.ReturnQueueBudget = NewTransferMemoryBudget(2048)
	settings.DialContextSettings = &DialContextSettings{DialContext: func(context.Context, string, string) (net.Conn, error) { return local, nil }}
	f := &persistEpisodeFixture{t: t, settings: settings, source: SourceId(NewId()), cancel: cancel, local: local, origin: origin,
		request:   []byte("GET /episode HTTP/1.1\r\nHost: persist-episode.example\r\n\r\n"),
		response:  []byte("HTTP/1.1 200 OK\r\nContent-Length: 4\r\n\r\ndone"),
		handshake: make(chan struct{}), firstData: make(chan persistControlEvent, 1), probes: make(chan persistControlEvent, 8),
		probeCompleted: make(chan time.Time, 1), releaseProbe: make(chan struct{}), done: make(chan struct{}),
		receivedAll: make(chan struct{}), serverDone: make(chan error, 1), holdFirstProbe: holdFirstProbe, nextAck: 101}
	f.sequence = newTcpSequenceWithTransferKey(ctx, func(_ TransferPath, _ TransferKey, _ protocol.ProvideMode, mode receiveRecoveryMode, _ *IpPath, packet []byte) {
		_, sourceIp, destinationIp, transport, ok := parseIpv4(packet)
		var tcp parsedTcp
		if !ok || !parseTcpPacket(sourceIp, destinationIp, transport, &tcp) {
			t.Error("episode callback received malformed TCP")
			return
		}
		if tcp.syn {
			f.ack(101, 0)
			f.handshakeDo.Do(func() { close(f.handshake) })
			return
		}
		if len(tcp.payload) == 0 {
			return
		}
		event := persistControlEvent{at: time.Now(), mode: mode, seq: tcp.seq, ack: tcp.ackNumber,
			payload: string(tcp.payload), valid: tcp.ack && !tcp.syn && !tcp.fin && !tcp.rst && !tcp.psh}
		if mode == receiveRecoveryModeDedicatedTcpControl {
			select {
			case f.probes <- event:
			default:
				t.Error("episode probe observations exceeded fixed slots")
			}
			if f.probeCount.Add(1) == 1 && f.holdFirstProbe {
				select {
				case <-f.releaseProbe:
				case <-f.sequence.ctx.Done():
				}
				f.probeCompleted <- time.Now()
			}
			return
		}
		f.peerStateLock.Lock()
		validData := mode == receiveRecoveryModeTcpSocket && event.valid && tcp.seq == f.nextAck &&
			len(f.received)+len(tcp.payload) <= len(f.response) && tcp.ackNumber == 101+uint32(len(f.request))
		if validData {
			f.received = append(f.received, tcp.payload...)
			f.nextAck += uint32(len(tcp.payload))
		}
		number, complete := f.nextAck, len(f.received) == len(f.response)
		f.peerStateLock.Unlock()
		if !validData {
			t.Errorf("episode changed HTTP sequence/custody: %+v", event)
			return
		}
		window := uint16(0)
		if f.fullWindow.Load() {
			window = 4096
		}
		f.ack(number, window)
		f.firstDataDo.Do(func() { f.firstData <- event })
		if complete {
			f.allDo.Do(func() { close(f.receivedAll) })
		}
	}, f.source, TransferKey{}, protocol.ProvideMode_Network, 4,
		net.IPv4(192, 0, 2, 17).To4(), 41005, net.IPv4(198, 51, 100, 17).To4(), 80, 100, settings)
	f.waits = &zeroWindowWaitLocker{sequence: f.sequence, waited: make(chan zeroWindowWaitSnapshot, 16)}
	f.sequence.receiveAckCondOnce.Do(func() { f.sequence.receiveAckCond = sync.NewCond(f.waits) })
	go func() {
		parsed, err := http.ReadRequest(bufio.NewReader(origin))
		if err == nil {
			_ = parsed.Body.Close()
			if parsed.Method != http.MethodGet || parsed.URL.Path != "/episode" || parsed.Host != "persist-episode.example" {
				err = fmt.Errorf("unexpected actual HTTP request: %s %s %s", parsed.Method, parsed.URL.Path, parsed.Host)
			}
		}
		if err == nil {
			var n int
			n, err = origin.Write(f.response)
			if err == nil && n != len(f.response) {
				err = io.ErrShortWrite
			}
		}
		f.serverDone <- err
	}()
	go func() { defer close(f.done); f.sequence.Run() }()
	t.Cleanup(f.close)
	f.send(zeroWindowSourcePacket(100, 0, tcpFlagSyn, 0, nil))
	<-f.handshake
	f.send(zeroWindowSourcePacket(101, 101, tcpFlagAck, 0, f.request))
	f.waitAt(101)
	return f
}

// Takes the source item on success; failure returns its original pooled root.
func (self *persistEpisodeFixture) send(item *TcpSendItem) {
	item.source = self.source
	if accepted, err := self.sequence.send(item, -1); !accepted {
		MessagePoolReturn(item.ipPacket)
		self.t.Fatalf("actual source item admission: %v", err)
	}
}

// The ACK is serialized and parsed before the normal established transition.
func (self *persistEpisodeFixture) ack(number uint32, window uint16) {
	item := zeroWindowSourcePacket(101+uint32(len(self.request)), number, tcpFlagAck, window, nil)
	if !self.sequence.applyEstablishedPureAck(self.source, TransferKey{}, &item.tcp, item.ipPacket) {
		MessagePoolReturn(item.ipPacket)
		self.t.Error("actual established ACK/window update was refused")
	}
}

// Cond.Wait has registered and released the real reader's mutex at this edge.
func (self *persistEpisodeFixture) waitAt(ack uint32) zeroWindowWaitSnapshot {
	for {
		snapshot := <-self.waits.waited
		if snapshot.window == 0 && snapshot.ack == ack && snapshot.next == ack && snapshot.retained == 0 && snapshot.chunks == 0 {
			synctest.Wait()
			if self.waits.lost.Load() != 0 {
				self.t.Fatal("episode actual-reader observations were incomplete")
			}
			return snapshot
		}
	}
}

// A positive ACK alone releases this very same reader and exact HTTP bytes.
func (self *persistEpisodeFixture) finishHttp() {
	self.fullWindow.Store(true)
	self.peerStateLock.Lock()
	ack := self.nextAck
	self.peerStateLock.Unlock()
	self.ack(ack, 4096)
	<-self.receivedAll
	err := <-self.serverDone
	self.serverJoined = true
	if err != nil {
		self.t.Fatal("actual HTTP origin", err)
	}
	synctest.Wait()
	self.peerStateLock.Lock()
	received := bytes.Clone(self.received)
	self.peerStateLock.Unlock()
	response, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(received)), nil)
	if err != nil {
		self.t.Fatal("actual HTTP response parse", err)
	}
	body, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	self.sequence.mutex.Lock()
	clear := self.sequence.returnByteCount == 0 && self.sequence.returnHead == len(self.sequence.returnChunks) &&
		self.sequence.receiveSeqAck == self.sequence.receiveSeq && !self.sequence.returnPersistPending && self.sequence.returnAttempts == 0
	self.sequence.mutex.Unlock()
	if !bytes.Equal(received, self.response) || response.StatusCode != http.StatusOK || string(body) != "done" || readErr != nil || closeErr != nil || !clear {
		self.t.Fatal("episode recovery changed exact HTTP bytes or left ordinary recovery debt")
	}
}

// Every failure unblocks the dedicated callback before canceling and joining.
func (self *persistEpisodeFixture) close() {
	self.closeDo.Do(func() {
		self.releaseDo.Do(func() { close(self.releaseProbe) })
		self.cancel()
		_ = self.origin.Close()
		_ = self.local.Close()
		if !self.serverJoined {
			<-self.serverDone
			self.serverJoined = true
		}
		<-self.done
		if used := self.settings.ReturnQueueBudget.UsedByteCount(); used != 0 {
			self.t.Errorf("joined actual episode retained %d return bytes", used)
		}
	})
}

// An accepted reopen before the first persist deadline must disarm probing;
// the real waiting reader still completes the exact response and retires data.
func TestTcpZeroWindowPersistAcceptedReopenBeforeDeadlineSuppressesProbe(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newPersistEpisodeFixture(t, false)
		defer f.close()
		advancePersistControlClock(f.settings.ReturnPersistTimeout / 2)
		f.finishHttp()
		advancePersistControlClock(35 * time.Second)
		select {
		case probe := <-f.probes:
			t.Fatalf("accepted ACK before deadline still emitted persist: %+v", probe)
		default:
		}
	})
}

// Reopen -> actual four bytes -> full ACK/window0 creates a new reader demand
// while the old callback is held. Its next probe uses the fresh base interval,
// current ACK-1 and no old backoff or ordinary data-retry credit.
func TestTcpZeroWindowPersistRecloseDuringHeldProbeStartsNewBaseInterval(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newPersistEpisodeFixture(t, true)
		defer f.close()
		first := <-f.probes
		requirePersistControlProbe(t, first, 101)
		f.ack(101, 4)
		data := <-f.firstData
		if data.mode != receiveRecoveryModeTcpSocket || data.seq != 101 || data.payload != "HTTP" {
			t.Fatalf("reopen did not issue the actual four origin bytes: %+v", data)
		}
		f.waitAt(105)
		f.sequence.mutex.Lock()
		progress, attempts, duplicates := f.sequence.returnProgressTime, f.sequence.returnAttempts, f.sequence.returnDuplicateAcks
		frontier, active, ready := f.sequence.returnRecoveryEnd, f.sequence.returnRecoveryActive, f.sequence.returnRecoveryReady
		f.sequence.mutex.Unlock()
		if attempts != 0 {
			t.Fatal("newly ACKed origin bytes inherited an ordinary retry")
		}
		advancePersistControlClock(3 * time.Second)
		select {
		case event := <-f.probes:
			t.Fatalf("held old callback spawned a parallel persist: %+v", event)
		default:
		}
		f.releaseDo.Do(func() { close(f.releaseProbe) })
		completedAt := <-f.probeCompleted
		second := <-f.probes
		requirePersistControlProbe(t, second, 105)
		synctest.Wait()
		if !second.at.Equal(completedAt.Add(f.settings.ReturnPersistTimeout)) {
			t.Fatalf("new fully ACKed zero-window episode inherited old backoff: completion=%s next=%s base=%s", completedAt, second.at, f.settings.ReturnPersistTimeout)
		}
		f.sequence.mutex.Lock()
		unchanged := f.sequence.returnProgressTime.Equal(progress) && f.sequence.returnAttempts == attempts && f.sequence.returnDuplicateAcks == duplicates &&
			f.sequence.returnRecoveryEnd == frontier && f.sequence.returnRecoveryActive == active && f.sequence.returnRecoveryReady == ready &&
			f.sequence.receiveSeqAck == 105 && f.sequence.receiveSeq == 105 && f.sequence.returnByteCount == 0 && f.sequence.returnHead == len(f.sequence.returnChunks)
		f.sequence.mutex.Unlock()
		if !unchanged {
			t.Fatal("new persist episode changed ordinary recovery or created inner ownership")
		}
		f.finishHttp()
	})
}
