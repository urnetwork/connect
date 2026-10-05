package connect

import "sync/atomic"

// TransferPayloadOwnerLedger optionally accounts for two bounded kinds of
// payload lifetime. Set ClientSettings.PayloadOwnerLedger before constructing
// clients. A ledger can be shared without retaining clients or payloads. The
// zero value is ready for use; nil disables accounting. Do not copy after use.
//
// SendAck starts after an acknowledged sendItem owns its encoded frame and ends
// at its terminal pool return, including retries temporarily outside the resend
// queue. It excludes unencoded SendPacks, no-ACK writes and temporary encrypted
// wire copies. Forward starts before a ForwardSequence attempts publication and
// includes pending offers, accepted queue entries and the active route write.
// A refusal rolls back that offer. It ends at the sequence's final local return;
// ownership already handed to a transport is outside this ledger's scope.
//
// BackingByteCharges sums cap(bytes) for each tracked owner. This includes pool
// metadata for complete pooled buffers, but is only the visible capacity for an
// arbitrary subslice. Shared roots can be charged repeatedly, within or across
// stages/ledgers. They exclude allocator rounding; other pool/resident ledgers
// may also exclude pool metadata. These are not additive physical heap bytes. No pointer
// keys, labels, queue traversal, owner locks or per-item fields are used.
type TransferPayloadOwnerLedger struct {
	shards [transferPayloadOwnerShardCount]transferPayloadOwnerShard
}

const transferPayloadOwnerShardCount = 16

type transferPayloadOwnerShard struct {
	writers  atomic.Int64
	revision atomic.Uint64
	sendAck  transferPayloadOwnerCounters
	forward  transferPayloadOwnerCounters
}

type transferPayloadOwnerCounters struct {
	owners   atomic.Int64
	bytes    atomic.Int64
	admitted atomic.Uint64
	released atomic.Uint64
}

type TransferPayloadOwnerGroup struct {
	Owners             int64 `json:"owners"`
	BackingByteCharges int64 `json:"backing_byte_charges"`
	// AdmittedTotal means admission to tracking, not accepted traffic. Forward
	// includes offers refused after tracking starts; those also release once.
	AdmittedTotal uint64 `json:"admitted_total"`
	ReleasedTotal uint64 `json:"released_total"`
}

type TransferPayloadOwnerSnapshot struct {
	Enabled  bool                      `json:"enabled"`
	Complete bool                      `json:"complete"`
	Revision uint64                    `json:"revision"`
	SendAck  TransferPayloadOwnerGroup `json:"send_ack"`
	Forward  TransferPayloadOwnerGroup `json:"forward"`
}

// Snapshot performs a fixed number of atomic loads, without retries or
// allocation. Complete requires all shards to stay unchanged across the common
// read interval and to satisfy nonnegative ownership and conservation. An
// overlapping update or inconsistent counter marks the aggregate incomplete;
// it never changes admission or transfer disposition. Callers must not use an
// incomplete sample as a coherent census or conservation proof.
func (l *TransferPayloadOwnerLedger) Snapshot() TransferPayloadOwnerSnapshot {
	return l.snapshot(nil)
}

// boundary exists for deterministic interleaving controls only and is never
// retained. Production snapshots pass nil.
func (l *TransferPayloadOwnerLedger) snapshot(boundary func(final bool)) TransferPayloadOwnerSnapshot {
	if l == nil {
		return TransferPayloadOwnerSnapshot{}
	}
	var beforeWriters [transferPayloadOwnerShardCount]int64
	var beforeRevision [transferPayloadOwnerShardCount]uint64
	for i := range l.shards {
		beforeWriters[i] = l.shards[i].writers.Load()
		beforeRevision[i] = l.shards[i].revision.Load()
	}
	if boundary != nil {
		boundary(false)
	}
	out := TransferPayloadOwnerSnapshot{Enabled: true, Complete: true}
	for i := range l.shards {
		sendAck := l.shards[i].sendAck.snapshot()
		forward := l.shards[i].forward.snapshot()
		out.Complete = out.Complete && sendAck.valid() && forward.valid()
		out.SendAck.add(sendAck)
		out.Forward.add(forward)
	}
	for i := range l.shards {
		// Closing revision must follow active writers: a writer finishing
		// between these loads cannot disguise a partially read group.
		afterWriters := l.shards[i].writers.Load()
		if boundary != nil {
			boundary(true)
		}
		afterRevision := l.shards[i].revision.Load()
		out.Revision += afterRevision
		out.Complete = out.Complete && beforeWriters[i] == 0 &&
			afterWriters == 0 && beforeRevision[i] == afterRevision
	}
	out.Complete = out.Complete && out.SendAck.valid() && out.Forward.valid()
	return out
}

func (c *transferPayloadOwnerCounters) snapshot() TransferPayloadOwnerGroup {
	return TransferPayloadOwnerGroup{
		Owners: c.owners.Load(), BackingByteCharges: c.bytes.Load(),
		AdmittedTotal: c.admitted.Load(), ReleasedTotal: c.released.Load(),
	}
}

func (g TransferPayloadOwnerGroup) valid() bool {
	return g.Owners >= 0 && g.BackingByteCharges >= 0 &&
		g.ReleasedTotal <= g.AdmittedTotal && uint64(g.Owners) == g.AdmittedTotal-g.ReleasedTotal &&
		(g.Owners != 0 || g.BackingByteCharges == 0)
}

func (g *TransferPayloadOwnerGroup) add(other TransferPayloadOwnerGroup) {
	g.Owners += other.Owners
	g.BackingByteCharges += other.BackingByteCharges
	g.AdmittedTotal += other.AdmittedTotal
	g.ReleasedTotal += other.ReleasedTotal
}

type transferPayloadOwnerKind bool

const (
	transferPayloadOwnerSendAck transferPayloadOwnerKind = false
	transferPayloadOwnerForward transferPayloadOwnerKind = true
)

func (l *TransferPayloadOwnerLedger) update(kind transferPayloadOwnerKind, selector byte, owners, bytes int64) {
	s := &l.shards[int(selector)&(transferPayloadOwnerShardCount-1)]
	s.writers.Add(1)
	c := &s.sendAck
	if kind == transferPayloadOwnerForward {
		c = &s.forward
	}
	c.owners.Add(owners)
	c.bytes.Add(bytes)
	if owners > 0 {
		c.admitted.Add(1)
	} else if owners < 0 {
		c.released.Add(1)
	}
	s.revision.Add(1)
	s.writers.Add(-1)
}

// The sequence already owns the immutable ledger pointer and shard selector.
// Keeping these at the sequence boundary avoids growing every sendItem.
func (s *SendSequence) returnSendItem(item *sendItem) {
	var ledger *TransferPayloadOwnerLedger
	var bytes int64
	if s.client != nil && item.expectsAck {
		ledger = s.client.payloadOwnerLedger
		if ledger != nil {
			bytes = int64(cap(item.transferFrameBytes))
		}
	}
	item.messagePoolReturn()
	if ledger != nil {
		ledger.update(transferPayloadOwnerSendAck, s.sequenceId[15], -1, -bytes)
	}
}

func (s *SendSequence) replaceSendItemFrame(item *sendItem, frame []byte) {
	oldBytes := cap(item.transferFrameBytes)
	MessagePoolReturn(item.transferFrameBytes)
	item.transferFrameBytes = frame
	if s.client != nil && item.expectsAck {
		if ledger := s.client.payloadOwnerLedger; ledger != nil {
			ledger.update(transferPayloadOwnerSendAck, s.sequenceId[15], 0, int64(cap(frame)-oldBytes))
		}
	}
}

func (s *ForwardSequence) returnForwardPayload(frame []byte) {
	bytes := cap(frame)
	MessagePoolReturn(frame)
	if ledger := s.client.payloadOwnerLedger; ledger != nil {
		ledger.update(transferPayloadOwnerForward, s.destination.DestinationId[15], -1, -int64(bytes))
	}
}
