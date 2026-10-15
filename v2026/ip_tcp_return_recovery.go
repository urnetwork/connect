package connect

import (
	"time"
	"unsafe"
)

// The origin socket and Transfer acknowledge different delivery boundaries.
// Keep bounded origin chunks until the source's inner TCP acknowledges them;
// a successful Transfer callback alone cannot prove that a TUN/kernel kept a
// segment. Chunk storage avoids retaining one pool root per MTU-sized packet.
type tcpReturnChunk struct {
	start   uint32
	end     uint32
	payload []byte
	charge  ByteCount
	fin     bool
}

func (self *TcpSequence) returnMemoryBudget() *TransferMemoryBudget {
	if self.tcpBufferSettings.MemoryBudget != nil {
		return self.tcpBufferSettings.MemoryBudget
	}
	return self.tcpBufferSettings.ReturnQueueBudget
}

// retainReturnChunk borrows payload and keeps an owned copy. Only the socket
// reader calls it; waiting here propagates pressure into this flow's origin.
// ACK callbacks merely release capacity and signal the dedicated replay worker.
func (self *TcpSequence) retainReturnChunk(payload []byte, start uint32, fin bool) bool {
	// Account for the pool root and geometric slice growth of chunk metadata.
	// Reserve before copying: a blocked flow borrows its existing socket read
	// buffer and must not allocate another uncharged replay buffer.
	charge := ByteCount(128)
	if len(payload) != 0 {
		capacity := len(payload)
		for _, pool := range orderedMessagePools() {
			if capacity <= pool.size {
				capacity = pool.size
				break
			}
		}
		charge += ByteCount(capacity + MessagePoolMetaByteCount)
	}
	budget := self.returnMemoryBudget()
	limit := self.tcpBufferSettings.ReturnQueueMaxByteCount
	if limit <= 0 {
		limit = max(ByteCount(self.tcpBufferSettings.MaxWindowSize), mib(1))
		if budget != nil {
			limit = budget.TotalByteCount()
		}
	}
	if charge > limit || (budget != nil && charge > budget.TotalByteCount()) {
		self.cancel()
		return false
	}
	end := start + uint32(len(payload))
	if fin {
		end++
	}
	for {
		if self.ctx.Err() != nil {
			return false
		}
		var budgetChanged <-chan struct{}
		if budget != nil {
			budgetChanged = budget.CapacityNotify()
		}
		self.mutex.Lock()
		if int32(self.receiveSeqAck-end) >= 0 {
			self.mutex.Unlock()
			return true
		}
		metadataCharge := ByteCount(0)
		metadataCapacity := cap(self.returnChunks)
		if self.tcpBufferSettings.MemoryBudget != nil && len(self.returnChunks) == cap(self.returnChunks) && self.returnHead != len(self.returnChunks) {
			metadataCapacity = max(4, 2*cap(self.returnChunks))
			metadataCharge = ByteCount(metadataCapacity) * ByteCount(unsafe.Sizeof(tcpReturnChunk{}))
		} else if self.tcpBufferSettings.MemoryBudget != nil && cap(self.returnChunks) == 0 {
			metadataCapacity = 4
			metadataCharge = ByteCount(metadataCapacity) * ByteCount(unsafe.Sizeof(tcpReturnChunk{}))
		}
		if self.returnByteCount+charge <= limit && (budget == nil || budget.TryReserve(charge+metadataCharge)) {
			var owned []byte
			if len(payload) != 0 {
				owned = MessagePoolCopy(payload)
			}
			if self.returnHead == len(self.returnChunks) {
				self.returnChunks = self.returnChunks[:0]
				self.returnHead = 0
				self.returnProgressTime = time.Now()
			}
			if metadataCharge > 0 {
				chunks := make([]tcpReturnChunk, len(self.returnChunks), metadataCapacity)
				copy(chunks, self.returnChunks)
				self.returnChunks = chunks
				// Growth temporarily retains both arrays. Admission paid for the
				// complete new backing before allocation; only now retire the old.
				budget.Release(self.returnMetadataByteCount)
				self.returnMetadataByteCount = metadataCharge
			}
			self.returnChunks = append(self.returnChunks, tcpReturnChunk{start: start, end: end, payload: owned, charge: charge, fin: fin})
			self.returnByteCount += charge
			self.signalReturnRecoveryWithLock()
			self.mutex.Unlock()
			return true
		}
		self.mutex.Unlock()
		select {
		case <-self.ctx.Done():
			return false
		case <-budgetChanged:
		case <-self.returnCapacity:
		}
	}
}

func (self *TcpSequence) signalReturnRecoveryWithLock() {
	select {
	case self.returnWake <- struct{}{}:
	default:
	}
}

// Called only after the ACK was validated against the emitted sequence range.
func (self *TcpSequence) acknowledgeReturnWithLock(ack uint32) {
	if self.returnHead == len(self.returnChunks) {
		return
	}
	if ack != self.receiveSeqAck {
		self.returnDuplicateAcks = 0
		self.returnAttempts = 0
		self.returnProgressTime = time.Now()
		if self.returnRecoveryActive {
			self.returnRecoveryReady = int32(ack-self.returnRecoveryEnd) < 0
			self.returnRecoveryActive = self.returnRecoveryReady
		}
		for self.returnHead < len(self.returnChunks) {
			chunk := &self.returnChunks[self.returnHead]
			if int32(ack-chunk.end) < 0 {
				break
			}
			MessagePoolReturn(chunk.payload)
			self.returnByteCount -= chunk.charge
			if budget := self.returnMemoryBudget(); budget != nil {
				budget.Release(chunk.charge)
			}
			*chunk = tcpReturnChunk{}
			self.returnHead++
		}
		// Amortized compaction keeps metadata bounded by the active window.
		if self.returnHead > 0 && self.returnHead*2 >= len(self.returnChunks) {
			count := copy(self.returnChunks, self.returnChunks[self.returnHead:])
			clear(self.returnChunks[count:])
			self.returnChunks = self.returnChunks[:count]
			self.returnHead = 0
		}
		select {
		case self.returnCapacity <- struct{}{}:
		default:
		}
		self.signalReturnRecoveryWithLock()
	} else if self.returnDuplicateAcks < 3 {
		self.returnDuplicateAcks++
		if self.returnDuplicateAcks == 3 {
			self.signalReturnRecoveryWithLock()
		}
	}
}

// Three duplicate ACKs or the resend timer start recovery of the current
// flight. Each advancing partial ACK permits the next oldest-segment replay;
// waiting for a fresh timer at each hole makes a lost burst repair one segment
// per second. A fixed frontier excludes newly issued data, and a silent peer
// retains exponential backoff. The return callback runs outside the flow mutex.
func (self *TcpSequence) runReturnRecovery() {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		if self.ctx.Err() != nil {
			return
		}
		var packet []byte
		var timeout <-chan time.Time
		self.mutex.Lock()
		if self.returnHead < len(self.returnChunks) {
			delay := self.tcpBufferSettings.ReturnResendTimeout
			if delay <= 0 {
				delay = time.Second
			}
			delay = min(30*time.Second, min(delay, 30*time.Second)*time.Duration(1<<min(self.returnAttempts, 5)))
			remaining := time.Until(self.returnProgressTime.Add(delay))
			if remaining <= 0 || self.returnDuplicateAcks == 3 || self.returnRecoveryReady {
				if !self.returnRecoveryActive {
					self.returnRecoveryEnd = self.receiveSeq
					self.returnRecoveryActive = true
				}
				self.returnRecoveryReady = false
				chunk := &self.returnChunks[self.returnHead]
				seq := chunk.start
				if int32(self.receiveSeqAck-seq) > 0 {
					seq = self.receiveSeqAck
				}
				if chunk.fin {
					packet = self.tcpPacket(tcpFlagAck|tcpFlagFin, seq, nil)
				} else {
					options := 0
					if self.enableTimestamp {
						options = tcpTimestampOptionByteCount
					}
					ipHeader := Ipv4HeaderSizeWithoutExtensions
					if self.ipVersion == 6 {
						ipHeader = Ipv6HeaderSize
					}
					count := self.clampPathMtu(self.tcpBufferSettings.Mtu) - ipHeader - TcpHeaderSizeWithoutExtensions - options
					if self.peerMss != 0 {
						count = min(count, int(self.peerMss)-options)
					}
					payload := chunk.payload[uint32(seq-chunk.start):]
					payload = payload[:min(len(payload), max(1, count))]
					packet = self.tcpPacket(tcpFlagAck, seq, payload)
				}
				self.returnProgressTime = time.Now()
				self.returnAttempts++
				self.returnDuplicateAcks = 4 // one fast replay per unchanged ACK
			} else {
				timer.Reset(remaining)
				timeout = timer.C
			}
		}
		self.mutex.Unlock()
		if packet != nil {
			self.receivePacket(packet, receiveRecoveryModeTcpSocket)
			continue
		}
		select {
		case <-self.ctx.Done():
			return
		case <-self.returnWake:
		case <-timeout:
		}
	}
}

// Run calls this after all readers, callbacks and the replay worker have joined.
func (self *TcpSequence) releaseReturnChunks() {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	for _, chunk := range self.returnChunks[self.returnHead:] {
		MessagePoolReturn(chunk.payload)
		if budget := self.returnMemoryBudget(); budget != nil {
			budget.Release(chunk.charge)
		}
	}
	self.returnChunks = nil
	if self.returnMetadataByteCount != 0 {
		self.returnMemoryBudget().Release(self.returnMetadataByteCount)
		self.returnMetadataByteCount = 0
	}
	self.returnHead = 0
	self.returnByteCount = 0
}
