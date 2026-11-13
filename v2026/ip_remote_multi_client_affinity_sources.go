// An admitted TCP group retains exact source custody through public return and
// terminal completion. Prior participation does not exclude a new race candidate.
package connect

import "time"

// One flow may encounter this many distinct unresolved sources. Normal window
// fanout is much smaller. Exhaustion refuses an offer; it never forgets evidence.
const affinitySourceMaxCount = 64

const (
	affinitySourceAdmitted uint8 = 1 << iota
	affinitySourceReturned
	affinitySourceTerminal
	affinitySourceMaterialized
	affinitySourceFailed
	affinitySourceSettled
)

// The remote NAT key includes the authenticated source, not the channel object
// or logical stream. Destination prevents conflating separate provider NATs.
type multiClientAffinitySourceKey struct {
	source      Id
	destination Id
}

// The flow lock owns these scalar records. No channel, packet, transfer worker,
// prior flow or receipt graph is retained. Slot epochs prevent stale completion
// from changing a later occupant after a successful source fence.
type multiClientAffinitySource struct {
	key       multiClientAffinitySourceKey
	epoch     uint64
	pending   uint32
	ambiguous bool
	seen      bool
}

// Inline storage covers the steady one-provider path. Spill exists only when
// several different sources are unresolved at once, or errors preserve taint.
type multiClientAffinitySources struct {
	first   multiClientAffinitySource
	spill   []multiClientAffinitySource
	closed  bool
	renewal bool
}

// This observes the actual transfer client identity, never generator arguments
// that could describe a different or recycled underlying client instance.
func (self *multiClientChannel) affinitySourceKey() (multiClientAffinitySourceKey, bool) {
	if self == nil || self.client == nil || self.args == nil || self.args.Destination.Len() == 0 {
		return multiClientAffinitySourceKey{}, false
	}
	_, destination := self.args.Destination.SplitTail()
	key := multiClientAffinitySourceKey{source: self.client.ClientId(), destination: destination}
	return key, key.source != (Id{}) && key.destination != (Id{})
}

// Index zero is inline. Every recorded offer stores the epoch as well as this
// bounded index; a cleared/reused slot cannot be mistaken for its predecessor.
func (self *multiClientAffinitySources) entryWithLock(index int) *multiClientAffinitySource {
	if index == 0 {
		return &self.first
	}
	if 0 < index && index <= len(self.spill) {
		return &self.spill[index-1]
	}
	return nil
}

// Enrollment precedes the irreversible source offer and remains live through
// both its public return and its terminal callback. Closing enrollment refuses
// late old-generation control before it can transfer any packet ownership.
func (self *multiClientAffinitySources) enrollWithLock(key multiClientAffinitySourceKey) uint64 {
	if self.closed || key.source == (Id{}) || key.destination == (Id{}) {
		return 0
	}
	free := -1
	for index := 0; index <= len(self.spill); index++ {
		entry := self.entryWithLock(index)
		if entry.key == key {
			if entry.pending == ^uint32(0) {
				return 0
			}
			if entry.pending == 0 {
				if entry.epoch == 1<<56-1 {
					return 0
				}
				entry.epoch++
			}
			entry.pending++
			return entry.epoch<<8 | uint64(index+1)
		}
		if free < 0 && entry.pending == 0 && !entry.ambiguous && !entry.seen && entry.epoch < 1<<56-1 {
			free = index
		}
	}
	if free < 0 {
		if 1+len(self.spill) == affinitySourceMaxCount {
			return 0
		}
		if len(self.spill) == cap(self.spill) {
			capacity := min(affinitySourceMaxCount-1, max(1, 2*cap(self.spill)))
			spill := make([]multiClientAffinitySource, len(self.spill), capacity)
			copy(spill, self.spill)
			self.spill = spill
		}
		self.spill = append(self.spill, multiClientAffinitySource{})
		free = len(self.spill)
	}
	entry := self.entryWithLock(free)
	entry.key, entry.epoch, entry.pending, entry.ambiguous = key, entry.epoch+1, 1, false
	return entry.epoch<<8 | uint64(free+1)
}

// A nil terminal TCP result means every physical chunk crossed the cumulative
// peer-delivery frontier. A materialized error is ambiguous even if one sibling
// chunk was refused; disposal or SACK must not erase that possible old control.
func (self *parsedPacketGroup) settleAffinitySourceWithLock() {
	if self.affinitySourceRegistration == 0 || self.affinitySourceState&affinitySourceSettled != 0 ||
		self.affinitySourceState&affinitySourceReturned == 0 {
		return
	}
	admitted := self.affinitySourceState&affinitySourceAdmitted != 0
	if admitted && self.affinitySourceState&affinitySourceTerminal == 0 {
		return
	}
	self.affinitySourceState |= affinitySourceSettled
	sources := &self.collapseAdmission.update.affinitySources
	entry := sources.entryWithLock(int(self.affinitySourceRegistration&0xff) - 1)
	if entry == nil || entry.epoch != self.affinitySourceRegistration>>8 || entry.pending == 0 {
		panic("affinity source completion lost its exact enrollment")
	}
	entry.pending--
	entry.ambiguous = entry.ambiguous || admitted &&
		self.affinitySourceState&(affinitySourceMaterialized|affinitySourceFailed) ==
			affinitySourceMaterialized|affinitySourceFailed
	if entry.pending == 0 && !entry.ambiguous && !entry.seen {
		entry.key = multiClientAffinitySourceKey{}
	}
}

// Absence means no unresolved group, not an assumed dequeue/delivery fence.
// Only the real source completion path removes an enrolled source record.
func (self *multiClientAffinitySources) fencedWithLock(key multiClientAffinitySourceKey) bool {
	if key.source == (Id{}) || key.destination == (Id{}) {
		return false
	}
	for index := 0; index <= len(self.spill); index++ {
		entry := self.entryWithLock(index)
		if entry.key == key {
			return entry.pending == 0 && !entry.ambiguous
		}
	}
	return true
}

// The returned proof applies only while enrollment remains closed by the
// parent reservation; a later unrelated callback cannot invent that closure.
func (self *multiClientChannelUpdate) affinitySourceFenced(client *multiClientChannel) bool {
	key, valid := client.affinitySourceKey()
	if !valid {
		return false
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.affinitySources.closed && self.affinitySources.fencedWithLock(key)
}

// Borrows one generated control. Its read-only share uses the same tracked
// group contract as native application sends; a stale producer is refused by
// closed enrollment before Transfer custody, without revoking accepted owners.
func (self *multiClientChannelUpdate) sendAffinityControl(client *multiClientChannel, packet []byte, timeout time.Duration) bool {
	if self == nil || client == nil || client.client == nil {
		return false
	}
	path, payload, err := ParseIpPathWithPayload(packet)
	if err != nil || path.Protocol != IpProtocolTcp {
		return false
	}
	self.stateLock.Lock()
	admission := tcpCollapseAdmission{update: self, epoch: self.sequenceAdmissionEpoch}
	self.stateLock.Unlock()
	shared := MessagePoolShareReadOnly(packet)
	group := &parsedPacketGroup{
		ipPath: path, byteCount: ByteCount(len(shared)), collapseAdmission: admission,
		packets: []parsedPacket{{packet: shared, ipPath: path, payload: payload, collapseAdmission: admission}},
	}
	accepted, _ := client.SendGroupDetailedWithAck(group, timeout, true)
	if !accepted {
		MessagePoolReturn(shared)
	}
	return accepted
}
