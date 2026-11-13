//go:build acklineagetrace

package connect

import (
	"sync"
	"sync/atomic"
	"time"
)

// AckLineagePacingIdentity names one dispatch without retaining wire bytes.
// This surface exists only in an explicitly diagnostic build.
type AckLineagePacingIdentity struct {
	Client, Peer, Sequence, Message Id
	Number                          uint64
	Resend, NoAck                   bool
}

// AckLineagePacingSnapshot distinguishes the owner's cached dispatch rates
// from a fresh, non-mutating window estimate. Claim runs before either read.
type AckLineagePacingSnapshot struct {
	AckLineagePacingIdentity
	AtUnixNano                        int64
	CachedRate, CachedServiceRate     ByteCount
	CachedProbeRate, CachedProbeLimit ByteCount
	CachedAge                         time.Duration
	ItemPacingBytes                   ByteCount
	HasService                        bool
	Window                            SendWindowEstimate
}

type ackLineagePacingOwner struct {
	join    sync.RWMutex
	closed  bool
	claim   func(AckLineagePacingIdentity) bool
	observe func(AckLineagePacingSnapshot)
}

var ackLineagePacingOwnerSlot atomic.Pointer[ackLineagePacingOwner]

// InstallAckLineagePacingObserver admits one diagnostic owner. Claim and
// observe must be bounded/nonblocking and must not call cleanup themselves.
// Cleanup refuses new claims and joins any callback that already entered.
func InstallAckLineagePacingObserver(
	claim func(AckLineagePacingIdentity) bool,
	observe func(AckLineagePacingSnapshot),
) (cleanup func(), installed bool) {
	if claim == nil || observe == nil {
		return nil, false
	}
	owner := &ackLineagePacingOwner{claim: claim, observe: observe}
	if !ackLineagePacingOwnerSlot.CompareAndSwap(nil, owner) {
		return nil, false
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			owner.join.Lock()
			owner.closed = true
			ackLineagePacingOwnerSlot.CompareAndSwap(owner, nil)
			owner.join.Unlock()
		})
	}, true
}

func observeAckLineagePacing(sequence *SendSequence, item *sendItem, resend bool) {
	owner := ackLineagePacingOwnerSlot.Load()
	if owner == nil || sequence == nil || item == nil {
		return
	}
	owner.join.RLock()
	defer owner.join.RUnlock()
	// An observer panic must not alter transport or buffer ownership.
	defer func() { _ = recover() }()
	if owner.closed {
		return
	}
	identity := AckLineagePacingIdentity{
		Client: sequence.client.ClientId(), Peer: sequence.destination,
		Sequence: sequence.sequenceId, Message: item.messageId, Number: item.sequenceNumber,
		Resend: resend, NoAck: !item.expectsAck,
	}
	if !owner.claim(identity) {
		return
	}
	now := time.Now()
	pacer := &sequence.windowPacer // This call runs on its actual send owner.
	age := time.Duration(0)
	if !pacer.rateUpdated.IsZero() {
		age = max(0, now.Sub(pacer.rateUpdated))
	}
	owner.observe(AckLineagePacingSnapshot{
		AckLineagePacingIdentity: identity, AtUnixNano: now.UnixNano(),
		CachedRate: pacer.rate, CachedServiceRate: pacer.estimateRate,
		CachedProbeRate: pacer.probeRate, CachedProbeLimit: pacer.probeLimit,
		CachedAge: age, ItemPacingBytes: item.pacingByteCount, HasService: pacer.service != nil,
		Window: sequence.sendWindowSnapshot(now),
	})
}
