//go:build darwin || ios || linux || android

package connect

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

type udpReturnReadTestConsumer struct {
	released func()
	onCommit func()
	commits  int
	aborts   int
}

func (consumer *udpReturnReadTestConsumer) commit(packets [][]byte) {
	consumer.commits++
	for _, packet := range packets {
		MessagePoolReturn(packet)
	}
	if consumer.onCommit != nil {
		consumer.onCommit()
	}
}

func (consumer *udpReturnReadTestConsumer) abort() {
	consumer.aborts++
	consumer.released()
}

func newUdpReturnReadTestSequence(t *testing.T) (*UdpSequence, *TransferMemoryBudget) {
	t.Helper()
	settings := DefaultUdpBufferSettingsWithBufferSize(1)
	parent := NewTransferMemoryBudget(256 * 1024)
	settings.MemoryBudget = parent
	settings.WriteBatchSize = 1
	settings.Log = NewNoopLogger()
	sequence := NewUdpSequence(context.Background(), nil,
		SourceId(NewId()), protocol.ProvideMode_Network, 4,
		net.IPv4(192, 0, 2, 1).To4(), 42000,
		net.IPv4(127, 0, 0, 1).To4(), 44000, settings)
	if sequence == nil {
		t.Fatal("test flow did not fit the unchanged parent")
	}
	sequence.sharedSocketLifecycle = true
	t.Cleanup(func() {
		sequence.Close()
		if parent.UsedByteCount() != 0 {
			t.Errorf("flow retained %d bytes after retirement", parent.UsedByteCount())
		}
	})
	return sequence, parent
}

func TestUdpReturnReadLeaseOwnershipAndRetirement(t *testing.T) {
	assertMessagePoolOwnership(t)
	sequence, parent := newUdpReturnReadTestSequence(t)
	consumer := &udpReturnReadTestConsumer{}
	sequence.prepareReturnReadCallback = func(_ *UdpSequence, _ int, released func()) (udpReturnReadConsumer, bool) {
		consumer.released = released
		return consumer, true
	}
	lease, err := sequence.prepareReturnRead(1000)
	if err != nil || lease == nil {
		t.Fatalf("prepare: lease=%v err=%v", lease, err)
	}
	if second, err := sequence.prepareReturnRead(1000); second != nil || !errors.Is(err, errUdpReturnReadPaused) {
		t.Fatalf("second read escaped the one-datagram owner: lease=%v err=%v", second, err)
	}
	packet := MessagePoolGet(32)
	lease.commit([][]byte{packet})
	sequence.Close()
	if parent.UsedByteCount() == 0 {
		t.Fatal("flow scratch released while asynchronous consumer still owned the read")
	}
	consumer.released()
	consumer.released()
	if parent.UsedByteCount() != 0 || consumer.commits != 1 || consumer.aborts != 0 {
		t.Fatalf("terminal ownership: bytes=%d commits=%d aborts=%d", parent.UsedByteCount(), consumer.commits, consumer.aborts)
	}
}

func TestUdpReturnReadLeaseRetirementRejectsWithoutNilError(t *testing.T) {
	sequence, _ := newUdpReturnReadTestSequence(t)
	sequence.prepareReturnReadCallback = func(*UdpSequence, int, func()) (udpReturnReadConsumer, bool) {
		t.Error("consumer preparation ran after retirement admission closed")
		return nil, true
	}
	// Closing admission is ordered before context cancellation in Close.
	sequence.retirementOperations.close()
	if lease, err := sequence.prepareReturnRead(1000); lease != nil || err == nil {
		t.Fatalf("retirement looked like permission to perform a legacy read: lease=%v err=%v", lease, err)
	}
}

type udpReturnReadTestBackend struct {
	ctx     context.Context
	events  chan udpSocketPollEvent
	waits   chan struct{}
	mutex   sync.Mutex
	added   map[int]bool
	removed map[int]int
}

func (backend *udpReturnReadTestBackend) add(fd int) error {
	backend.mutex.Lock()
	defer backend.mutex.Unlock()
	backend.added[fd] = true
	return nil
}
func (backend *udpReturnReadTestBackend) remove(fd int) {
	backend.mutex.Lock()
	defer backend.mutex.Unlock()
	delete(backend.added, fd)
	backend.removed[fd]++
}
func (backend *udpReturnReadTestBackend) wait(events []udpSocketPollEvent) (int, error) {
	select {
	case backend.waits <- struct{}{}:
	case <-backend.ctx.Done():
		return 0, backend.ctx.Err()
	}
	select {
	case event := <-backend.events:
		events[0] = event
		return 1, nil
	case <-backend.ctx.Done():
		return 0, backend.ctx.Err()
	}
}
func (backend *udpReturnReadTestBackend) wake() {
	select {
	case backend.events <- udpSocketPollEvent{fd: -1}:
	default:
	}
}
func (*udpReturnReadTestBackend) close() error { return nil }

func TestUdpReturnReadPollerPausesInsteadOfClosing(t *testing.T) {
	sequence, _ := newUdpReturnReadTestSequence(t)
	ctx, cancel := context.WithCancel(context.Background())
	backend := &udpReturnReadTestBackend{ctx: ctx,
		events: make(chan udpSocketPollEvent, 8), waits: make(chan struct{}, 8),
		added: map[int]bool{}, removed: map[int]int{}}
	poller := &udpSocketReadPoller{shards: []udpSocketReadPollShard{{
		ctx: ctx, backend: backend, readBuffer: make([]byte, 2048),
		byFd: map[int]*udpSocketReadRegistration{},
	}}}
	sequence.socketReadPoller = poller
	sequence.prepareReturnReadCallback = func(*UdpSequence, int, func()) (udpReturnReadConsumer, bool) {
		return nil, false
	}
	socket := newUdpReturnReadTestSocket(t, []byte("paused-datagram"))
	sequence.sharedSocket = socket
	if !poller.register(sequence, socket) {
		t.Fatal("register")
	}
	done := make(chan struct{})
	go func() { defer close(done); poller.shards[0].run() }()
	defer func() { cancel(); <-done }()
	wait := func() {
		t.Helper()
		select {
		case <-backend.waits:
		case <-time.After(5 * time.Second):
			t.Fatal("poller did not reach its next nonblocking wait")
		}
	}
	wait()
	backend.events <- udpSocketPollEvent{fd: sequence.socketReadPollFd}
	wait() // joins the complete admission-failure transition
	if err := sequence.ctx.Err(); err != nil {
		t.Fatalf("temporary parent-credit pause closed the flow: %v", err)
	}
	backend.mutex.Lock()
	stillArmed := backend.added[sequence.socketReadPollFd]
	backend.mutex.Unlock()
	if stillArmed {
		t.Error("paused socket is still armed and can spin on unread readiness")
	}
}

func TestUdpReturnReadNoConsumptionWithoutPreparedCredit(t *testing.T) {
	assertMessagePoolOwnership(t)
	sequence, parent := newUdpReturnReadTestSequence(t)
	occupied := parent.Available()
	if !parent.TryReserve(occupied) {
		t.Fatal("could not establish exhausted-parent barrier")
	}
	defer parent.Release(occupied)
	sequence.prepareReturnReadCallback = func(_ *UdpSequence, _ int, released func()) (udpReturnReadConsumer, bool) {
		if !parent.TryReserve(4096) {
			return nil, false
		}
		return &udpReturnReadTestConsumer{released: func() { parent.Release(4096); released() }}, true
	}
	socket, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	sender, err := net.DialUDP("udp4", nil, socket.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	payload := []byte("still-kernel-owned")
	if _, err := sender.Write(payload); err != nil {
		t.Fatal(err)
	}
	raw, err := socket.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 2048)
	if err := raw.Read(func(fd uintptr) bool {
		n, _, readErr := syscall.Recvfrom(int(fd), buffer, syscall.MSG_PEEK)
		if errors.Is(readErr, syscall.EAGAIN) || errors.Is(readErr, syscall.EWOULDBLOCK) {
			return false
		}
		if readErr != nil || n != len(payload) {
			t.Errorf("readability barrier: n=%d err=%v", n, readErr)
		}
		if err := drainReadyUdpSocket(int(fd), sequence, buffer); !errors.Is(err, errUdpReturnReadPaused) {
			t.Errorf("unadmitted read returned %v", err)
		}
		n, _, readErr = syscall.Recvfrom(int(fd), buffer, syscall.MSG_PEEK)
		if readErr != nil || n != len(payload) {
			t.Errorf("unadmitted datagram consumed: n=%d err=%v", n, readErr)
		}
		return true
	}); err != nil {
		t.Fatal(err)
	}
}

func newUdpReturnReadTestSocket(t *testing.T, payload []byte) *net.UDPConn {
	t.Helper()
	socket, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = socket.Close() })
	sender, err := net.DialUDP("udp4", nil, socket.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	if _, err := sender.Write(payload); err != nil {
		t.Fatal(err)
	}
	raw, err := socket.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var peek [1]byte
	if err := raw.Read(func(fd uintptr) bool {
		_, _, err := syscall.Recvfrom(int(fd), peek[:], syscall.MSG_PEEK)
		return !errors.Is(err, syscall.EAGAIN) && !errors.Is(err, syscall.EWOULDBLOCK)
	}); err != nil {
		t.Fatal(err)
	}
	return socket
}

func TestUdpReturnReadEmptyDatagramCommitsInsteadOfAborting(t *testing.T) {
	assertMessagePoolOwnership(t)
	sequence, _ := newUdpReturnReadTestSequence(t)
	consumer := &udpReturnReadTestConsumer{}
	sequence.prepareReturnReadCallback = func(_ *UdpSequence, readBytes int, released func()) (udpReturnReadConsumer, bool) {
		if readBytes != 0 {
			t.Errorf("empty datagram pre-read size=%d", readBytes)
		}
		consumer.released, consumer.onCommit = released, released
		return consumer, true
	}
	socket := newUdpReturnReadTestSocket(t, nil)
	raw, err := socket.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	if err := raw.Read(func(fd uintptr) bool {
		if err := drainReadyUdpSocket(int(fd), sequence, make([]byte, 64)); err != nil {
			t.Error(err)
		}
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if consumer.commits != 1 || consumer.aborts != 0 || sequence.returnReadPending.Load() {
		t.Fatalf("accepted zero-length datagram disappeared: commits=%d aborts=%d", consumer.commits, consumer.aborts)
	}
}

func TestUdpReturnReadPausedQuantumIsFair(t *testing.T) {
	assertMessagePoolOwnership(t)
	backend := &udpReturnReadTestBackend{added: map[int]bool{}, removed: map[int]int{}}
	poller := &udpSocketReadPoller{shards: []udpSocketReadPollShard{{
		ctx: context.Background(), backend: backend, readBuffer: make([]byte, 2048),
		byFd: map[int]*udpSocketReadRegistration{},
	}}}
	shard := &poller.shards[0]
	available := 0
	var served []int
	var registrations [2]*udpSocketReadRegistration
	for index := range registrations {
		sequence, _ := newUdpReturnReadTestSequence(t)
		sequence.socketReadPoller = poller
		socket := newUdpReturnReadTestSocket(t, []byte{byte(index + 1)})
		sequence.sharedSocket = socket
		sequence.prepareReturnReadCallback = func(_ *UdpSequence, _ int, released func()) (udpReturnReadConsumer, bool) {
			if available == 0 {
				return nil, false
			}
			available--
			return &udpReturnReadTestConsumer{released: released, onCommit: func() {
				served = append(served, index)
				released()
			}}, true
		}
		if !poller.register(sequence, socket) {
			t.Fatal("register")
		}
		registrations[index] = shard.byFd[sequence.socketReadPollFd]
		_, err := sequence.prepareReturnRead(2048)
		var pause *udpReturnReadPause
		if !errors.As(err, &pause) {
			t.Fatal("initial pause")
		}
		shard.pauseRegistration(registrations[index], pause)
	}
	available = 1
	shard.resumePaused()
	if len(served) != 1 || served[0] != 0 || shard.pauseHead != registrations[1] {
		t.Fatalf("first quantum was not FIFO: served=%v remaining=%d", served, shard.pauseCount)
	}
	// The first flow becomes ready again while the second still waits. It
	// must rejoin at the tail rather than monopolize the next credit release.
	_, err := registrations[0].sequence.prepareReturnRead(2048)
	var pause *udpReturnReadPause
	if !errors.As(err, &pause) {
		t.Fatal("repeat pause")
	}
	shard.pauseRegistration(registrations[0], pause)
	available = 1
	shard.resumePaused()
	if len(served) != 2 || served[1] != 1 {
		t.Fatalf("oldest waiting flow did not get the next quantum: %v", served)
	}
}

func TestUdpReturnReadStaleRegistrationCannotRearmReplacement(t *testing.T) {
	backend := &udpReturnReadTestBackend{added: map[int]bool{}, removed: map[int]int{}}
	poller := &udpSocketReadPoller{shards: []udpSocketReadPollShard{{
		ctx: context.Background(), backend: backend, readBuffer: make([]byte, 2048),
		byFd: map[int]*udpSocketReadRegistration{},
	}}}
	shard := &poller.shards[0]
	old, _ := newUdpReturnReadTestSequence(t)
	old.socketReadPoller = poller
	old.prepareReturnReadCallback = func(*UdpSequence, int, func()) (udpReturnReadConsumer, bool) {
		return nil, false
	}
	socket := newUdpReturnReadTestSocket(t, []byte("replacement-must-stay-unread"))
	if !poller.register(old, socket) {
		t.Fatal("old register")
	}
	fd := old.socketReadPollFd
	oldRegistration := shard.byFd[fd]
	_, err := old.prepareReturnRead(2048)
	var pause *udpReturnReadPause
	if !errors.As(err, &pause) {
		t.Fatal("pause")
	}
	shard.pauseRegistration(oldRegistration, pause)
	// Publish a new registration at the exact same descriptor; deterministic
	// identity reuse does not depend on the OS choosing a particular fd.
	replacement, _ := newUdpReturnReadTestSequence(t)
	replacement.socketReadPoller = poller
	if !poller.register(replacement, socket) {
		t.Fatal("replacement register")
	}
	newRegistration := shard.byFd[fd]
	poller.unregister(old)
	shard.pauseRegistration(oldRegistration, pause) // a late failed read
	shard.returnReadCapacity.notify()               // a late old lease release
	shard.resumePaused()
	if shard.byFd[fd] != newRegistration || newRegistration.sequence != replacement || shard.pauseCount != 0 || !backend.added[fd] {
		t.Fatal("old generation changed replacement readiness or ownership")
	}
	raw, _ := socket.SyscallConn()
	if err := raw.Control(func(fd uintptr) {
		var peek [64]byte
		n, _, err := syscall.Recvfrom(int(fd), peek[:], syscall.MSG_PEEK)
		if err != nil || n != len("replacement-must-stay-unread") {
			t.Errorf("old registration consumed replacement datagram: n=%d err=%v", n, err)
		}
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUdpReturnReadCapacityReleaseBeforeDisarmIsNotLost(t *testing.T) {
	assertMessagePoolOwnership(t)
	sequence, _ := newUdpReturnReadTestSequence(t)
	settings := *sequence.udpBufferSettings
	settings.SocketReadShardCount = 1
	ctx, cancel := context.WithCancel(context.Background())
	poller := newUdpSocketReadPoller(ctx, &settings)
	if poller == nil {
		cancel()
		t.Skip("socket readiness backend unavailable")
	}
	defer func() { sequence.Close(); cancel(); poller.waitForLifecycle() }()
	sequence.socketReadPoller = poller
	sequence.providerReturnCapacity = &receiveCapacitySignal{}
	checked := make(chan struct{})
	returnFailedCheck := make(chan struct{})
	committed := make(chan struct{}, 1)
	var attempts atomic.Int32
	sequence.prepareReturnReadCallback = func(_ *UdpSequence, _ int, released func()) (udpReturnReadConsumer, bool) {
		if attempts.Add(1) == 1 {
			close(checked)
			<-returnFailedCheck
			return nil, false
		}
		return &udpReturnReadTestConsumer{released: released, onCommit: func() {
			released()
			committed <- struct{}{}
		}}, true
	}
	socket := newUdpReturnReadTestSocket(t, []byte("release-before-disarm"))
	sequence.sharedSocket = socket
	if !poller.register(sequence, socket) {
		t.Fatal("register")
	}
	select {
	case <-checked:
	case <-time.After(5 * time.Second):
		close(returnFailedCheck)
		t.Fatal("readiness never reached admission")
	}
	// The check observed no capacity, but the corresponding release wins
	// before the poller has removed readiness or linked the paused node.
	sequence.providerReturnCapacity.notify()
	close(returnFailedCheck)
	select {
	case <-committed:
	case <-sequence.ctx.Done():
		t.Fatal("temporary pause closed the flow")
	case <-time.After(5 * time.Second):
		t.Fatal("capacity release before disarm was lost")
	}
}

func TestUdpReturnReadPortablePauseCancelsWithoutReading(t *testing.T) {
	sequence, _ := newUdpReturnReadTestSequence(t)
	checked := make(chan struct{}, 1)
	sequence.prepareReturnReadCallback = func(*UdpSequence, int, func()) (udpReturnReadConsumer, bool) {
		checked <- struct{}{}
		return nil, false
	}
	socket := newUdpReturnReadTestSocket(t, []byte("cancel-retains-kernel-owner"))
	done := make(chan error, 1)
	go func() {
		lease, err := sequence.awaitReturnRead(socket, make([]byte, 2048))
		if lease != nil {
			lease.abort()
		}
		done <- err
	}()
	select {
	case <-checked:
	case <-time.After(5 * time.Second):
		t.Fatal("portable readiness never reached admission")
	}
	sequence.Close()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("portable pause cancellation: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("portable paused reader did not cancel")
	}
	raw, _ := socket.SyscallConn()
	if err := raw.Control(func(fd uintptr) {
		var peek [64]byte
		n, _, err := syscall.Recvfrom(int(fd), peek[:], syscall.MSG_PEEK)
		if err != nil || n != len("cancel-retains-kernel-owner") {
			t.Errorf("portable pause consumed datagram: n=%d err=%v", n, err)
		}
	}); err != nil {
		t.Fatal(err)
	}
}
