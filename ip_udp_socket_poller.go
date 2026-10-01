package connect

import (
	"context"
	"errors"
	"net"
	"sync"
	"syscall"
)

const udpSocketPollEventCount = 64

type udpSocketPollEvent struct {
	fd       int
	terminal bool
}

// udpSocketPollBackend is implemented with epoll on Linux/Android and kqueue
// on Darwin/iOS. The portable fallback returns nil from
// newUdpSocketPollBackend, leaving UdpSequence's established reader intact.
type udpSocketPollBackend interface {
	add(fd int) error
	remove(fd int)
	wake()
	wait(events []udpSocketPollEvent) (int, error)
	close() error
}

type udpSocketReadRegistration struct {
	fd       int
	sequence *UdpSequence
	socket   net.Conn
	rawConn  syscall.RawConn
	// Protected by the shard mutex. The registration itself, rather than
	// its reusable fd number, identifies every pause/rearm operation.
	pause       *udpReturnReadPause
	inPauseList bool
	previous    *udpSocketReadRegistration
	next        *udpSocketReadRegistration
}

type udpSocketReadPollShard struct {
	ctx                context.Context
	backend            udpSocketPollBackend
	readBuffer         []byte
	mutex              sync.RWMutex
	byFd               map[int]*udpSocketReadRegistration
	pauseHead          *udpSocketReadRegistration
	pauseTail          *udpSocketReadRegistration
	pauseCount         int
	pauseChanged       receiveCapacitySignal
	returnReadCapacity receiveCapacitySignal
}

type udpSocketReadPoller struct {
	shards    []udpSocketReadPollShard
	waitGroup sync.WaitGroup
}

func newUdpSocketReadPoller(
	ctx context.Context,
	settings *UdpBufferSettings,
) *udpSocketReadPoller {
	if settings == nil || settings.SocketReadShardCount <= 0 {
		return nil
	}
	shards := make([]udpSocketReadPollShard, 0, settings.SocketReadShardCount)
	for range settings.SocketReadShardCount {
		backend, err := newUdpSocketPollBackend()
		if err != nil {
			for i := range shards {
				_ = shards[i].backend.close()
			}
			return nil
		}
		shards = append(shards, udpSocketReadPollShard{
			ctx:        ctx,
			backend:    backend,
			readBuffer: make([]byte, settings.ReadBufferByteCount),
			byFd:       map[int]*udpSocketReadRegistration{},
		})
	}
	poller := &udpSocketReadPoller{shards: shards}
	for i := range poller.shards {
		shard := &poller.shards[i]
		poller.waitGroup.Add(1)
		go HandleError(func() {
			defer poller.waitGroup.Done()
			shard.run()
		})
	}
	return poller
}

// Completion means every readiness shard has stopped reading and can no
// longer publish a packet into the receive dispatcher.
func (self *udpSocketReadPoller) waitForLifecycle() {
	if self == nil {
		return
	}
	self.waitGroup.Wait()
}

func socketRawConn(socket net.Conn) (syscall.RawConn, bool) {
	syscallConn, ok := socket.(syscall.Conn)
	if !ok {
		return nil, false
	}
	rawConn, err := syscallConn.SyscallConn()
	return rawConn, err == nil
}

func (self *udpSocketReadPoller) register(sequence *UdpSequence, socket net.Conn) bool {
	if self == nil || sequence == nil || socket == nil || len(self.shards) == 0 {
		return false
	}
	rawConn, ok := socketRawConn(socket)
	if !ok {
		return false
	}
	shardIndex := sequence.receiveShard % len(self.shards)
	shard := &self.shards[shardIndex]
	fd := -1
	var registerErr error
	controlErr := rawConn.Control(func(rawFd uintptr) {
		fd = int(rawFd)
		shard.mutex.Lock()
		sequence.socketReadPollShard = shardIndex
		sequence.socketReadPollFd = fd
		if previous := shard.byFd[fd]; previous != nil {
			shard.backend.remove(fd)
			shard.unlinkPauseLocked(previous)
		}
		shard.byFd[fd] = &udpSocketReadRegistration{
			fd:       fd,
			sequence: sequence,
			socket:   socket,
			rawConn:  rawConn,
		}
		registerErr = shard.backend.add(fd)
		if registerErr != nil {
			delete(shard.byFd, fd)
			sequence.socketReadPollFd = -1
		}
		shard.mutex.Unlock()
	})
	shard.pauseChanged.notify()
	if controlErr != nil || registerErr != nil || fd < 0 {
		return false
	}
	return true
}

func (self *udpSocketReadPoller) unregister(sequence *UdpSequence) {
	if self == nil || sequence == nil || len(self.shards) == 0 {
		return
	}
	shardIndex := sequence.socketReadPollShard
	if shardIndex < 0 || len(self.shards) <= shardIndex {
		return
	}
	fd := sequence.socketReadPollFd
	if fd < 0 {
		return
	}
	shard := &self.shards[shardIndex]
	shard.mutex.Lock()
	if registration, ok := shard.byFd[fd]; ok && registration.sequence == sequence {
		shard.backend.remove(fd)
		shard.unlinkPauseLocked(registration)
		delete(shard.byFd, fd)
	}
	shard.mutex.Unlock()
	shard.pauseChanged.notify()
	sequence.socketReadPollFd = -1
}

func (self *udpSocketReadPollShard) registration(fd int) (*udpSocketReadRegistration, bool) {
	self.mutex.RLock()
	registration, ok := self.byFd[fd]
	active := ok && registration.pause == nil
	self.mutex.RUnlock()
	return registration, active
}

func (self *udpSocketReadPollShard) run() {
	defer self.backend.close()
	watchCtx, cancelWatch := context.WithCancel(self.ctx)
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		self.watchPaused(watchCtx)
	}()
	defer func() { cancelWatch(); <-watchDone }()
	events := make([]udpSocketPollEvent, udpSocketPollEventCount)
	for {
		select {
		case <-self.ctx.Done():
			return
		default:
		}
		eventCount, err := self.backend.wait(events)
		if err != nil {
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			select {
			case <-self.ctx.Done():
				return
			default:
			}
			return
		}
		for _, event := range events[:eventCount] {
			if event.fd < 0 {
				self.resumePaused()
				continue
			}
			registration, ok := self.registration(event.fd)
			if !ok {
				continue
			}
			readErr := drainReadyUdpRegistration(
				event.fd,
				*registration,
				self.readBuffer,
			)
			var pause *udpReturnReadPause
			if !event.terminal && errors.As(readErr, &pause) {
				self.pauseRegistration(registration, pause)
				continue
			}
			if event.terminal || (readErr != nil && !errors.Is(readErr, syscall.EAGAIN) &&
				!errors.Is(readErr, syscall.EWOULDBLOCK)) {
				registration.sequence.Close()
			}
		}
	}
}

// A single fixed-channel coordinator per shard observes the shared provider,
// parent budget and lease capacity signals. No paused flow owns a goroutine.
// A wake is acknowledged by the reader's next pause-list generation, avoiding
// a spin on an already-closed capacity notification while it services fds.
func (self *udpSocketReadPollShard) watchPaused(ctx context.Context) {
	for {
		self.mutex.RLock()
		changed := self.pauseChanged.subscribe()
		var wakes [3]<-chan struct{}
		if self.pauseHead != nil {
			wakes = self.pauseHead.pause.wakes
		}
		self.mutex.RUnlock()
		select {
		case <-ctx.Done():
			return
		case <-changed:
			continue
		case <-wakes[0]:
		case <-wakes[1]:
		case <-wakes[2]:
		}
		self.backend.wake()
		select {
		case <-ctx.Done():
			return
		case <-changed:
		}
	}
}

func (self *udpSocketReadPollShard) unlinkPauseLocked(registration *udpSocketReadRegistration) {
	if !registration.inPauseList {
		return
	}
	if registration.previous != nil {
		registration.previous.next = registration.next
	} else {
		self.pauseHead = registration.next
	}
	if registration.next != nil {
		registration.next.previous = registration.previous
	} else {
		self.pauseTail = registration.previous
	}
	registration.previous, registration.next = nil, nil
	registration.inPauseList = false
	self.pauseCount--
}

func (self *udpSocketReadPollShard) pauseRegistration(registration *udpSocketReadRegistration, pause *udpReturnReadPause) {
	self.mutex.Lock()
	if self.byFd[registration.fd] != registration {
		self.mutex.Unlock()
		return
	}
	if registration.pause == nil {
		self.backend.remove(registration.fd)
	}
	registration.pause = pause
	if !registration.inPauseList {
		registration.inPauseList = true
		registration.previous = self.pauseTail
		if self.pauseTail != nil {
			self.pauseTail.next = registration
		} else {
			self.pauseHead = registration
		}
		self.pauseTail = registration
		self.pauseCount++
	}
	self.mutex.Unlock()
	self.pauseChanged.notify()
}

// One datagram per paused registration, in FIFO order, before any flow may
// consume another quantum. RawConn pins each fd through both read and rearm;
// an obsolete capacity wake cannot arm an fd subsequently reused by a flow.
func (self *udpSocketReadPollShard) resumePaused() {
	self.mutex.RLock()
	count := self.pauseCount
	self.mutex.RUnlock()
	defer self.pauseChanged.notify()
	for range count {
		self.mutex.Lock()
		registration := self.pauseHead
		if registration == nil {
			self.mutex.Unlock()
			return
		}
		self.unlinkPauseLocked(registration)
		current := self.byFd[registration.fd] == registration
		self.mutex.Unlock()
		if !current {
			continue
		}
		var readErr error
		rawErr := registration.rawConn.Read(func(rawFd uintptr) bool {
			if int(rawFd) != registration.fd {
				readErr = syscall.EBADF
				return true
			}
			readErr = drainReadyUdpSocketWithLimit(registration.fd, registration.sequence, self.readBuffer, 1)
			return true
		})
		if rawErr != nil {
			readErr = rawErr
		}
		var pause *udpReturnReadPause
		if errors.As(readErr, &pause) {
			self.pauseRegistration(registration, pause)
			continue
		}
		if readErr != nil && !errors.Is(readErr, syscall.EAGAIN) && !errors.Is(readErr, syscall.EWOULDBLOCK) {
			registration.sequence.Close()
			continue
		}
		var rearmErr error
		controlErr := registration.rawConn.Control(func(rawFd uintptr) {
			self.mutex.Lock()
			defer self.mutex.Unlock()
			if int(rawFd) == registration.fd && self.byFd[registration.fd] == registration && registration.sequence.ctx.Err() == nil {
				rearmErr = self.backend.add(registration.fd)
				if rearmErr == nil {
					registration.pause = nil
				}
			}
		})
		if controlErr != nil || rearmErr != nil {
			registration.sequence.Close()
		}
	}
}

// RawConn.Read keeps the descriptor valid for the complete drain callback.
// A readiness event may already be queued when another goroutine unregisters
// and closes the socket; pinning the descriptor prevents an fd reused by a new
// flow from being read into the old sequence.
func drainReadyUdpRegistration(
	fd int,
	registration udpSocketReadRegistration,
	buffer []byte,
) error {
	if registration.rawConn == nil {
		return syscall.EBADF
	}
	var readErr error
	rawErr := registration.rawConn.Read(func(rawFd uintptr) bool {
		if int(rawFd) != fd {
			readErr = syscall.EBADF
			return true
		}
		readErr = drainReadyUdpSocket(fd, registration.sequence, buffer)
		// The custom readiness backend, not the runtime poller, owns the next
		// wait. Even EAGAIN completes this RawConn operation.
		return true
	})
	if rawErr != nil {
		return rawErr
	}
	return readErr
}

func drainReadyUdpSocket(fd int, sequence *UdpSequence, buffer []byte) error {
	if sequence == nil {
		return syscall.EINVAL
	}
	return drainReadyUdpSocketWithLimit(fd, sequence, buffer, max(1, sequence.udpBufferSettings.WriteBatchSize))
}

func drainReadyUdpSocketWithLimit(fd int, sequence *UdpSequence, buffer []byte, maxReads int) error {
	if sequence == nil || len(buffer) == 0 {
		return syscall.EINVAL
	}
	// A cached readiness registration can race unregister/Close. Keep the
	// flow's receive scratch allowance until packetization and handoff finish.
	if !sequence.startRetirementOperation() {
		return syscall.EBADF
	}
	defer sequence.finishRetirementOperation()
	for range maxReads {
		readBytes := len(buffer)
		if sequence.prepareReturnReadCallback != nil {
			// Peek borrows the shard's prepaid scratch but leaves the entire
			// datagram kernel-owned. Charge its actual bounded read length,
			// not a maximum-sized datagram that can strand small control replies
			// during an otherwise-valid old/new provider memory overlap.
			n, err := peekUdpSocket(SocketHandle(fd), buffer)
			if err != nil {
				if errors.Is(err, syscall.EINTR) {
					continue
				}
				return err
			}
			readBytes = n
		}
		lease, prepareErr := sequence.prepareReturnRead(readBytes)
		if prepareErr != nil {
			return prepareErr
		}
		if sequence.ctx.Err() != nil {
			lease.abort()
			return syscall.EBADF
		}
		n, err := syscall.Read(SocketHandle(fd), buffer)
		if 0 < n || n == 0 && err == nil && lease != nil {
			sequence.UpdateLastActivityTime()
			packets, packetsErr := sequence.DataPackets(buffer, n, sequence.udpBufferSettings.Mtu)
			if packetsErr != nil {
				lease.abort()
				return packetsErr
			}
			if lease != nil {
				lease.commit(packets)
			} else {
				for _, packet := range packets {
					if sequence.receiveDispatcher == nil {
						sequence.singleDataPacket[0] = packet
						sequence.receiveBatch(sequence.singleDataPacket[:])
					} else if !sequence.receiveDispatcher.enqueue(sequence, packet) {
						MessagePoolReturn(packet)
					}
				}
			}
		} else {
			lease.abort()
		}
		if err != nil {
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			return err
		}
		if n == 0 {
			return nil
		}
	}
	return nil
}
