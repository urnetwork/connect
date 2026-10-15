package connect

import "sync/atomic"

// Prepared fanout replaces only the internal socket-return provider callbacks.
// Public borrowed batch callbacks still see the same immutable packet list;
// when no provider is registered the established legacy dispatcher is used.
type providerDatagramFanout struct {
	consumers       []udpReturnReadConsumer
	remaining       atomic.Int32
	released        func()
	sequence        *UdpSequence
	publicCallbacks []ReceivePacketsFunction
}

func (nat *LocalUserNat) addProviderDatagramSender(sender *providerDatagramSender) func() {
	id := nat.providerDatagramSenders.Add(sender)
	nat.providerReturnCapacity.notify()
	return func() {
		nat.providerDatagramSenders.Remove(id)
		nat.providerReturnCapacity.notify()
	}
}

func (nat *LocalUserNat) prepareUdpReturnRead(sequence *UdpSequence, readBytes int, released func()) (udpReturnReadConsumer, bool) {
	if nat.providerDatagramSenders == nil {
		return nil, true
	}
	senders := nat.providerDatagramSenders.Get()
	if len(senders) == 0 {
		return nil, true
	}
	fanout := &providerDatagramFanout{consumers: make([]udpReturnReadConsumer, 0, len(senders)), released: released,
		sequence: sequence, publicCallbacks: nat.receivePacketsCallbacks.Get()}
	// Keep an extra producer reference while preparers may complete inline.
	fanout.remaining.Store(1)
	defer fanout.childReleased()
	for _, sender := range senders {
		fanout.remaining.Add(1)
		consumer, admitted := sender.prepareUdp(sequence, readBytes, fanout.childReleased)
		if !admitted || consumer == nil {
			fanout.childReleased()
			fanout.abort()
			return nil, false
		}
		fanout.consumers = append(fanout.consumers, consumer)
	}
	return fanout, true
}

func (fanout *providerDatagramFanout) childReleased() {
	if fanout.remaining.Add(-1) == 0 {
		fanout.released()
	}
}

func (fanout *providerDatagramFanout) abort() {
	for _, consumer := range fanout.consumers {
		consumer.abort()
	}
}

func (fanout *providerDatagramFanout) commit(packets [][]byte) {
	if len(fanout.publicCallbacks) > 0 {
		fanout.remaining.Add(1)
		public := &providerDatagramPublicDispatch{fanout: fanout, packets: make([][]byte, len(packets))}
		for index, packet := range packets {
			public.packets[index] = MessagePoolShareReadOnly(packet)
		}
		// This is the same bounded read owner, not another offered-packet
		// queue. Its first actor nonblockingly retries the existing public
		// dispatcher while the socket-read lease remains paused.
		fanout.consumers[0].(*providerDatagramReturn).publicDispatch = public
	}
	for index, consumer := range fanout.consumers {
		// A borrowed public callback can outlive the provider's first write.
		// It must not keep a finished provider actor/object graph alive.
		fanout.consumers[index] = nil
		if index == len(fanout.consumers)-1 {
			consumer.commit(packets)
		} else {
			shares := make([][]byte, len(packets))
			for index, packet := range packets {
				shares[index] = MessagePoolShareReadOnly(packet)
			}
			consumer.commit(shares)
		}
	}
}

type providerDatagramPublicDispatch struct {
	fanout  *providerDatagramFanout
	packets [][]byte
}

func (public *providerDatagramPublicDispatch) tryEnqueue() bool {
	sequence := public.fanout.sequence
	dispatcher := sequence.receiveDispatcher
	if dispatcher == nil {
		// Directly constructed portable sequences keep their original
		// synchronous callback contract; real shared socket readers always
		// have the existing receive dispatcher.
		public.deliver()
		return true
	}
	shard := &dispatcher.shards[sequence.receiveShard]
	shard.preparedMutex.Lock()
	defer shard.preparedMutex.Unlock()
	if shard.preparedClosed || dispatcher.ctx.Err() != nil || sequence.ctx.Err() != nil {
		return false
	}
	dispatcher.startShard(shard)
	select {
	case shard.items <- udpReceiveDispatchItem{sequence: sequence, preparedPublic: public}:
		return true
	default:
		return false
	}
}

func (public *providerDatagramPublicDispatch) deliver() {
	defer public.release()
	fanout := public.fanout
	source, _ := fanout.sequence.transferState.get()
	for _, callback := range fanout.publicCallbacks {
		HandleError(func() { callback(source, fanout.sequence.provideMode, fanout.sequence.IpPath(), public.packets) })
	}
}

func (public *providerDatagramPublicDispatch) release() {
	for _, packet := range public.packets {
		MessagePoolReturn(packet)
	}
	fanout := public.fanout
	public.packets, public.fanout = nil, nil
	fanout.childReleased()
}
