package connect

import (
	"context"
	"sync"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Each read lease supplies its own intrusive node and one datagram's raw
// packetization owner. This sender adds no packet channel or per-flow worker.
// A refused zero-timeout Pack remains on that same bounded owner until retry.
type providerDatagramSender struct {
	provider   *RemoteUserNatProvider
	mutex      sync.Mutex
	head, tail *providerDatagramReturn
	closed     bool
	wake       chan struct{}
	done       chan struct{}
}

type providerDatagramReturn struct {
	sender                *providerDatagramSender
	next                  *providerDatagramReturn
	item                  providerReturnItem
	flowContext           context.Context
	path                  IpPath
	prepared              *preparedSendMemory
	released              func()
	ack                   *providerDatagramAck
	packetIndex           int
	awaiting              bool
	policyChecked         bool
	nextAttempt           time.Time
	publicDispatch        *providerDatagramPublicDispatch
	admissionObservations *sendPackAdmissionObservations
}

func newProviderDatagramSender(provider *RemoteUserNatProvider) *providerDatagramSender {
	sender := &providerDatagramSender{provider: provider, wake: make(chan struct{}, 1), done: make(chan struct{})}
	go func() { defer close(sender.done); sender.run() }()
	return sender
}

func providerDatagramPacketCredit(packetBytes int, legacy bool) ByteCount {
	return providerDatagramAckMemoryByteCount + (&sendItem{}).retainedMemoryByteCount(
		ByteCount(packetBytes)+512+32, 1, legacy)
}

// A flow can receive a path-MTU reduction concurrently. The pre-read claim
// uses the family's minimum fragment payload, not the current larger MTU.
func providerUdpReadCredit(sequence *UdpSequence, readBytes int, legacy bool) ByteCount {
	mtu := min(sequence.udpBufferSettings.Mtu, ipMinimumPathMtu(sequence.ipVersion))
	header := Ipv4HeaderSizeWithoutExtensions
	if sequence.ipVersion == 6 {
		header = Ipv6HeaderSize + ipv6FragmentHeaderSize
	}
	payload := max(8, (mtu-header)&^7)
	fragments := max(1, (readBytes+UdpHeaderSize+payload-1)/payload)
	packetBytes := min(sequence.udpBufferSettings.Mtu, readBytes+Ipv6HeaderSize+UdpHeaderSize)
	return ByteCount(fragments) * providerDatagramPacketCredit(packetBytes, legacy)
}

func (sender *providerDatagramSender) prepareUdp(sequence *UdpSequence, readBytes int, released func()) (udpReturnReadConsumer, bool) {
	provider := sender.provider
	if provider.ctx.Err() != nil || !provider.memoryOperations.start() {
		return nil, false
	}
	source, transferKey := sequence.transferState.get()
	lifecycle := provider.acquireSourceLifecycle(source.SourceId)
	if lifecycle == nil {
		provider.memoryOperations.finish()
		return nil, false
	}
	budget := provider.client.settings.SendBufferSettings.ResendQueueBudget
	if budget == nil {
		budget = provider.memoryBudget()
	}
	credit, admitted := prepareSendMemory(budget, providerUdpReadCredit(sequence, readBytes, provider.settings.ProtocolVersion < 2))
	if !admitted {
		provider.releaseSourceLifecycle(source.SourceId, lifecycle)
		provider.memoryOperations.finish()
		return nil, false
	}
	provideMode := provider.sourceReturnProvideMode(source.SourceId, sequence.provideMode)
	return &providerDatagramReturn{
		sender: sender, flowContext: sequence.ctx, path: *sequence.IpPath(), prepared: credit, released: released,
		item: providerReturnItem{
			source: source.LocalMask(), transferKey: providerReplyTransferKey(transferKey, provideMode),
			provideMode: provideMode, recoveryMode: receiveRecoveryModePreparedDatagram,
			ipProtocol: IpProtocolUdp, sourceLifecycle: lifecycle,
			schedulingKey: ipSendSchedulingKey(sequence.IpPath()), batch: true,
		},
	}, true
}

func (owner *providerDatagramReturn) abort() {
	owner.finish(false, false)
}

func (owner *providerDatagramReturn) commit(packets [][]byte) {
	owner.item.packets = packets
	actualCredit := ByteCount(0)
	for _, packet := range packets {
		owner.item.packetByteCount += ByteCount(len(packet))
		actualCredit += providerDatagramPacketCredit(len(packet), owner.sender.provider.settings.ProtocolVersion < 2)
	}
	if !owner.prepared.trim(actualCredit) {
		// A violated packetization bound must never consume uncharged bytes.
		// This is a real software failure, not an allowed network-loss sample.
		owner.finish(false, true)
		return
	}
	owner.sender.append(owner)
}

func (sender *providerDatagramSender) append(owner *providerDatagramReturn) {
	sender.mutex.Lock()
	if sender.closed {
		sender.mutex.Unlock()
		owner.finish(false, true)
		return
	}
	if sender.tail == nil {
		sender.head = owner
	} else {
		sender.tail.next = owner
	}
	sender.tail = owner
	sender.mutex.Unlock()
	select {
	case sender.wake <- struct{}{}:
	default:
	}
}

func (sender *providerDatagramSender) takeAll() *providerDatagramReturn {
	sender.mutex.Lock()
	defer sender.mutex.Unlock()
	head := sender.head
	sender.head, sender.tail = nil, nil
	return head
}

func (sender *providerDatagramSender) run() {
	defer func() {
		sender.mutex.Lock()
		sender.closed = true
		sender.mutex.Unlock()
		for owner := sender.takeAll(); owner != nil; {
			next := owner.next
			owner.next = nil
			owner.finish(false, true)
			owner = next
		}
	}()
	retry := sender.provider.settings.ReturnSendRetryTimeout
	if retry <= 0 {
		retry = 10 * time.Millisecond
	}
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()
	closing := false
	for {
		var capacity <-chan struct{}
		if budget := sender.provider.client.settings.SendBufferSettings.ResendQueueBudget; budget != nil {
			capacity = budget.CapacityNotify()
		}
		if sender.provider.ctx.Err() != nil {
			closing = true
			sender.mutex.Lock()
			sender.closed = true
			sender.mutex.Unlock()
		}
		pending := false
		for owner := sender.takeAll(); owner != nil; {
			next := owner.next
			owner.next = nil
			if owner.step(retry) {
				// Requeue without publishing another wake: otherwise a refused
				// Pack would drive a self-waking busy loop instead of paced retry.
				sender.mutex.Lock()
				if sender.tail == nil {
					sender.head = owner
				} else {
					sender.tail.next = owner
				}
				sender.tail = owner
				sender.mutex.Unlock()
				pending = true
			}
			owner = next
		}
		if closing && !pending {
			return
		}
		providerDone := sender.provider.ctx.Done()
		if closing {
			providerDone = nil
		}
		var deadline <-chan time.Time
		if pending {
			timer.Reset(retry)
			deadline = timer.C
		}
		select {
		case <-providerDone:
		case <-sender.wake:
			sender.provider.localUserNat.providerReturnCapacity.notify()
		case <-capacity:
			sender.provider.localUserNat.providerReturnCapacity.notify()
		case <-deadline:
		}
		if pending && !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}
}

func (owner *providerDatagramReturn) step(retry time.Duration) bool {
	provider := owner.sender.provider
	if provider.ctx.Err() != nil || owner.flowContext.Err() != nil || owner.item.sourceLifecycle.ctx.Err() != nil {
		if owner.awaiting && !owner.ack.handoff.cancelBeforeHandoff() {
			// Provider teardown joins queued ownership, not a peer ACK. A
			// materialized reliable flight remains detached and charged.
			return true
		}
		owner.finish(false, true)
		return false
	}
	if owner.publicDispatch != nil {
		if !owner.publicDispatch.tryEnqueue() {
			return true
		}
		owner.publicDispatch = nil
	}
	if !owner.policyChecked {
		owner.policyChecked = true
		provider.beginReturnSendObservation(&owner.item)
		// These packets were built from this exact socket's immutable tuple.
		// Inspect the same real server-to-client identity as the legacy path,
		// including fragmented datagrams, without a second reassembly queue.
		path := owner.path.ReverseValue()
		result, err := inspectAndRefreshEgressForSenderBorrowed(provider.securityPolicy,
			owner.item.source.SourceId, owner.item.provideMode, path, nil)
		if err != nil || result != SecurityPolicyResultAllow {
			provider.packetStatsCounters.blockEgressPacketCount.Add(int64(len(owner.item.packets)))
			provider.packetStatsCounters.blockEgressByteCount.Add(owner.item.packetByteCount)
			provider.recordProviderBlock(owner.item.source.SourceId, false, len(owner.item.packets), owner.item.packetByteCount)
			provider.publishProviderDiagnostics(owner.item.source, owner.item.transferKey, owner.item.provideMode)
			owner.finish(false, false)
			return false
		}
	}
	if owner.awaiting {
		written, terminal, err := owner.ack.disposition()
		if written || (terminal && err == nil) {
			MessagePoolReturn(owner.item.packets[owner.packetIndex])
			owner.item.packets[owner.packetIndex] = nil
			owner.ack, owner.awaiting = nil, false
			owner.packetIndex++
			if owner.packetIndex == len(owner.item.packets) {
				owner.finish(true, false)
				return false
			}
		} else if terminal {
			if !providerDatagramUnwrittenRetry(err) {
				owner.finish(false, true)
				return false
			}
			// No wire attempt happened. The previous capsule is terminal;
			// its original credit and attribution move to the retry capsule.
			owner.ack = owner.newAck(owner.ack.credit, owner.ack.attribution)
			owner.awaiting = false
		} else {
			owner.ack.credit.growRequired()
			return true
		}
	}
	if time.Now().Before(owner.nextAttempt) {
		return true
	}
	packet := owner.item.packets[owner.packetIndex]
	if owner.ack == nil {
		credit, admitted := owner.prepared.split(providerDatagramPacketCredit(len(packet), provider.settings.ProtocolVersion < 2), providerDatagramAckMemoryByteCount)
		if !admitted {
			owner.finish(false, true)
			return false
		}
		owner.ack = owner.newAck(credit, newTransportPacketAttribution(provider.packetStatsCounters, 1, ByteCount(len(packet))))
	}
	if !owner.ack.credit.growRequired() {
		return true
	}
	options := providerReturnTransferOptions(provider.client.settings.DefaultTransferOpts, owner.item.provideMode, owner.item.transferKey)
	options.Ack = !provider.settings.UdpTransferNoAck
	opts := []any{options, owner.item.transferKey, Ctx(owner.item.sendContext(provider.ctx)),
		sendSchedulingKeyOption{key: owner.item.schedulingKey}, observeTransportWrite(owner.ack.observeWrite),
		provider.returnSendRecoveryOption(&owner.item)}
	lifecycleObserver := provider.client.settings.SendBufferSettings.SendPackLifecycleObserver
	noAckObserver := provider.client.settings.SendBufferSettings.NoAckSendObserver
	if lifecycleObserver != nil || noAckObserver != nil {
		if owner.admissionObservations == nil {
			owner.admissionObservations = &sendPackAdmissionObservations{}
		}
		if lifecycleObserver != nil {
			opts = append(opts, sendPackLifecycleObserverOption{observer: owner.admissionObservations.wrap(lifecycleObserver)})
		}
		if noAckObserver != nil {
			opts = append(opts, sendNoAckObserverOption{observer: owner.admissionObservations.wrapNoAck(noAckObserver)})
		}
	}
	var sent bool
	if provider.settings.ProtocolVersion >= 2 {
		share := MessagePoolShareReadOnly(packet)
		sent, _ = provider.client.sendRawWithTimeoutDetailed(protocol.MessageType_IpIpPacketFromProvider,
			share, owner.item.source.SourceId, owner.ack, 0, 0, opts...)
		if !sent {
			MessagePoolReturn(share)
		}
	} else {
		frame, err := ipPacketFromProviderFrame(packet, provider.settings.ProtocolVersion)
		if err != nil {
			owner.finish(false, true)
			return false
		}
		opts = append(opts, sendAckTargetOption{target: owner.ack})
		sent, _ = provider.client.SendWithTimeoutDetailed(frame, owner.item.source.SourceId, nil, 0, opts...)
		if !sent {
			MessagePoolReturn(frame.MessageBytes)
		}
	}
	if sent {
		owner.awaiting = true
		owner.ack.admitted()
		owner.ack.attribution.admit()
	} else {
		owner.nextAttempt = time.Now().Add(retry)
	}
	return true
}

func (owner *providerDatagramReturn) newAck(credit *preparedSendMemory, attribution *transportPacketAttribution) *providerDatagramAck {
	provider := owner.sender.provider
	var evidence *sourceAckEvidence
	if !provider.settings.UdpTransferNoAck {
		evidence = owner.item.sourceLifecycle.evidence
	}
	return &providerDatagramAck{credit: credit, wake: owner.sender.wake,
		handoff:  preparedSendHandoff{upstreamWake: owner.sender.wake},
		evidence: evidence, observer: provider.returnAckTargetForTest, attribution: attribution}
}

func (owner *providerDatagramReturn) finish(sent, softwareFailure bool) {
	provider := owner.sender.provider
	if owner.publicDispatch != nil {
		owner.publicDispatch.release()
		owner.publicDispatch = nil
	}
	transferred := owner.awaiting && owner.ack.handoff.transferred()
	if transferred && owner.packetIndex+1 == len(owner.item.packets) {
		sent = true
	}
	if softwareFailure {
		remainingBytes, remainingPackets := ByteCount(0), 0
		for index, packet := range owner.item.packets {
			if packet != nil && !(transferred && index == owner.packetIndex) {
				remainingPackets++
				remainingBytes += ByteCount(len(packet))
			}
		}
		provider.congestionDrops.addReturnSend(remainingPackets, remainingBytes)
	}
	if owner.ack != nil {
		owner.ack.abandon()
		owner.ack = nil
	}
	if owner.admissionObservations != nil {
		owner.admissionObservations.complete(sent)
	}
	if owner.item.observerToken != 0 {
		provider.observeReturnSend(&owner.item, providerReturnSendResult{packetCount: len(owner.item.packets), packetByteCount: owner.item.packetByteCount, sent: sent})
	}
	owner.item.returnPackets()
	owner.prepared.release()
	provider.releaseSourceLifecycle(owner.item.source.SourceId, owner.item.sourceLifecycle)
	provider.memoryOperations.finish()
	released := owner.released
	owner.item.sourceLifecycle = nil
	owner.released = nil
	if released != nil {
		released()
	}
}
