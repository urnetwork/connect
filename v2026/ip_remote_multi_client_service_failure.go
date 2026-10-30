package connect

import (
	"net"
	"sort"
	"time"
)

// A provider can be healthy in general while one destination rejects its
// egress address (SMTP anti-abuse policy is a common example). Remembering a
// failure by destination AND port keeps that local fact from demoting the
// whole provider, while ensuring the retransmitted handshake actually tries a
// different exit.
const (
	destinationServiceFailureTtl      = 10 * time.Minute
	destinationServiceFailureMaxCount = 4096
)

type destinationServiceFailureKey struct {
	destinationIp   [net.IPv6len]byte
	destinationPort uint16
	protocol        IpProtocol
	ipVersion       uint8
	client          *multiClientChannel
}

type destinationServiceFailure struct {
	time  time.Time
	count uint32
}

// Ordinary fresh TCP affinity cannot bypass a current failure of this exact
// service. The parent stateLock owns this read; pins and established flows keep
// their existing policy, and missing or expired history supplies no veto.
func (self *RemoteUserNatMultiClient) affinityDonorServiceFailedWithLock(update *multiClientChannelUpdate, client *multiClientChannel, now time.Time) bool {
	if update == nil || update.pinned || update.receivedInbound.Load() {
		return false
	}
	path := update.ipPath
	if path == nil || path.Protocol != IpProtocolTcp || !path.Syn || path.Ack {
		return false
	}
	key, ok := destinationServiceFailureKeyFor(path, client)
	if !ok {
		return false
	}
	failure, failed := self.destinationServiceFailures[key]
	return failed && now.Sub(failure.time) < destinationServiceFailureTtl
}

func destinationServiceFailureKeyFor(
	ipPath *IpPath,
	client *multiClientChannel,
) (destinationServiceFailureKey, bool) {
	if ipPath == nil || client == nil ||
		ipPath.DestinationPort < 0 || 65535 < ipPath.DestinationPort {
		return destinationServiceFailureKey{}, false
	}
	var destinationIp net.IP
	switch ipPath.Version {
	case 4:
		destinationIp = ipPath.DestinationIp.To4()
	case 6:
		destinationIp = ipPath.DestinationIp.To16()
	default:
		return destinationServiceFailureKey{}, false
	}
	if destinationIp == nil {
		return destinationServiceFailureKey{}, false
	}
	key := destinationServiceFailureKey{
		destinationPort: uint16(ipPath.DestinationPort),
		protocol:        ipPath.Protocol,
		ipVersion:       uint8(ipPath.Version),
		client:          client,
	}
	copy(key.destinationIp[:], destinationIp)
	return key, true
}

// recordDestinationServiceFailureWithLock records one failed connect. The
// parent stateLock must be held.
func (self *RemoteUserNatMultiClient) recordDestinationServiceFailureWithLock(
	client *multiClientChannel,
	ipPath *IpPath,
) {
	key, ok := destinationServiceFailureKeyFor(ipPath, client)
	if !ok {
		return
	}
	// A receive callback may outlive removal's prune. Both decisions
	// serialize on the parent lock, so a done channel cannot reinsert a key.
	if client.ctx != nil && client.IsDone() {
		return
	}
	if self.destinationServiceFailures == nil {
		self.destinationServiceFailures = map[destinationServiceFailureKey]destinationServiceFailure{}
	}
	now := time.Now()
	if entry, found := self.destinationServiceFailures[key]; found {
		entry.time = now
		entry.count++
		self.destinationServiceFailures[key] = entry
		return
	}
	if destinationServiceFailureMaxCount <= len(self.destinationServiceFailures) {
		var oldestKey destinationServiceFailureKey
		var oldestTime time.Time
		found := false
		for candidateKey, entry := range self.destinationServiceFailures {
			if now.Sub(entry.time) >= destinationServiceFailureTtl {
				delete(self.destinationServiceFailures, candidateKey)
				continue
			}
			if !found || entry.time.Before(oldestTime) {
				oldestKey, oldestTime, found = candidateKey, entry.time, true
			}
		}
		if destinationServiceFailureMaxCount <= len(self.destinationServiceFailures) && found {
			delete(self.destinationServiceFailures, oldestKey)
		}
	}
	self.destinationServiceFailures[key] = destinationServiceFailure{time: now, count: 1}
}

// Releases this retired channel generation's cache roots without provider
// events or flow migration.
func (self *RemoteUserNatMultiClient) clearDestinationServiceFailures(client *multiClientChannel) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.clearDestinationServiceFailuresWithLock(client)
}

// The caller holds the parent stateLock.
func (self *RemoteUserNatMultiClient) clearDestinationServiceFailuresWithLock(client *multiClientChannel) {
	for key := range self.destinationServiceFailures {
		if key.client == client {
			delete(self.destinationServiceFailures, key)
		}
	}
}

func (self *RemoteUserNatMultiClient) clearDestinationServiceFailure(
	client *multiClientChannel,
	ipPath *IpPath,
) {
	key, ok := destinationServiceFailureKeyFor(ipPath, client)
	if !ok {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	delete(self.destinationServiceFailures, key)
}

// filterDestinationServiceFailures removes exits that recently failed this
// exact destination service. If every candidate has failed, the oldest failure
// is tried first so the flow remains routable and recovery is periodically
// re-tested instead of deadlocking behind negative memory.
func (self *RemoteUserNatMultiClient) filterDestinationServiceFailures(
	candidates []*multiClientChannel,
	ipPath *IpPath,
) []*multiClientChannel {
	if len(candidates) <= 1 || ipPath == nil {
		return candidates
	}
	type failedCandidate struct {
		client *multiClientChannel
		time   time.Time
	}
	now := time.Now()
	available := make([]*multiClientChannel, 0, len(candidates))
	failed := make([]failedCandidate, 0, len(candidates))

	self.stateLock.Lock()
	for _, client := range candidates {
		key, ok := destinationServiceFailureKeyFor(ipPath, client)
		if !ok {
			available = append(available, client)
			continue
		}
		entry, found := self.destinationServiceFailures[key]
		if found && destinationServiceFailureTtl <= now.Sub(entry.time) {
			delete(self.destinationServiceFailures, key)
			found = false
		}
		if found {
			failed = append(failed, failedCandidate{client: client, time: entry.time})
		} else {
			available = append(available, client)
		}
	}
	self.stateLock.Unlock()

	if 0 < len(available) {
		return available
	}
	sort.SliceStable(failed, func(i, j int) bool {
		return failed[i].time.Before(failed[j].time)
	})
	result := make([]*multiClientChannel, 0, len(failed))
	for _, candidate := range failed {
		result = append(result, candidate.client)
	}
	return result
}

func (self *RemoteUserNatMultiClient) destinationServiceFailurePresent(
	candidates []*multiClientChannel,
	ipPath *IpPath,
) bool {
	if len(candidates) == 0 || ipPath == nil {
		return false
	}
	now := time.Now()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for _, client := range candidates {
		key, ok := destinationServiceFailureKeyFor(ipPath, client)
		if !ok {
			continue
		}
		if entry, found := self.destinationServiceFailures[key]; found {
			if now.Sub(entry.time) < destinationServiceFailureTtl {
				return true
			}
			delete(self.destinationServiceFailures, key)
		}
	}
	return false
}
