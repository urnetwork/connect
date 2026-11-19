// Receive-ACK health corroboration follows accepted application packet offers
// through their original stats buckets.
package connect

import (
	"sync/atomic"
	"time"
)

// Matches the destination projection of the raw IPv4 and IPv6 path maps.
type receiveAckDestination struct {
	ip       [16]byte
	port     int
	protocol IpProtocol
	version  int
}

// The key is independent of the source address and any borrowed packet bytes.
func receiveAckDestinationOf(path *IpPath) receiveAckDestination {
	destination := receiveAckDestination{port: path.DestinationPort, protocol: path.Protocol, version: path.Version}
	if path.Version == 4 {
		copy(destination.ip[:], path.DestinationIp.To4())
	} else {
		copy(destination.ip[:], path.DestinationIp.To16())
	}
	return destination
}

// One distinct destination in one raw event bucket. An offer may keep this
// scalar entry alive after expiry, but it does not keep the bucket maps alive.
// The first offer time is an immutable origin. The source and admission
// publish qualified offsets atomically. Channel stateLock guards the counts.
type receiveAckDestinationUse struct {
	destination        receiveAckDestination
	firstOfferTime     time.Time
	firstSecuredOffset atomic.Uint64
	pending            int
	retained           bool
	retired            bool
}

// One original offer can complete before its admission call returns. The
// immutable destination entry is set before handoff, and stateLock orders the
// public return and completion. An offset starts at one so zero needs no
// observation. Disposition bits join source events with the public return.
type receiveAckOffer struct {
	use         *receiveAckDestinationUse
	clockOffset uint64
	disposition atomic.Uint32
	admitted    bool
	finished    bool
	unwritten   bool
}

const (
	receiveAckSourceDispatched uint32 = 1 << iota
	receiveAckClockAdmitted
	receiveAckClockSecured
)

// Only an admitted original with irreversible source evidence can start a
// mixed bucket's receive clock. Source callbacks may precede public return.
// The atomic minimum retains one offset per tuple and no packet or bucket.
func (self *receiveAckOffer) observeDisposition(flags uint32) {
	if self.clockOffset == 0 {
		if flags&receiveAckSourceDispatched != 0 {
			self.disposition.Store(receiveAckSourceDispatched)
		}
		return
	}
	if self.disposition.Load()&flags == flags {
		return
	}
	previous := self.disposition.Or(flags)
	const ready = receiveAckClockAdmitted | receiveAckClockSecured
	if previous&ready == ready || (previous|flags)&ready != ready {
		return
	}
	earliest := &self.use.firstSecuredOffset
	for current := earliest.Load(); current == 0 || self.clockOffset < current; current = earliest.Load() {
		if earliest.CompareAndSwap(current, self.clockOffset) {
			return
		}
	}
}

// A selected carrier accepted the original. A later missing peer ACK cannot
// retract that evidence or restart its clock.
func (self *receiveAckOffer) observeCarrierWrite(TransportType) {
	self.observeDisposition(receiveAckSourceDispatched | receiveAckClockSecured)
}

// The provider consumes an empty reset as a terminal action on either a live
// or an absent TCP flow. Other TCP controls and payload keep their eligibility.
func terminalReceiveAckReset(path *IpPath) bool {
	return path.Protocol == IpProtocolTcp && path.Rst && !path.Syn && !path.Fin && path.TcpPayloadByteCount == 0
}

// A real nonempty window owns its receive clock. Bare classifier samples
// have no buckets and retain their historical clock.
func receiveAckOfferSince(stats *clientWindowStats) time.Time {
	if 0 < stats.bucketCount {
		return stats.firstReceiveAckOfferTime
	}
	return stats.firstSendNackTime
}

// Ordinary buckets keep the historical send clock. A bucket containing a
// terminal reset can start receive silence only with another eligible offer.
func (self *multiClientEventBucket) receiveAckOfferSinceWithLock() time.Time {
	if !self.terminalResetOffered {
		return self.sendNackTime
	}
	var first time.Time
	for _, use := range self.receiveAckDestinationUses {
		if offset := use.firstSecuredOffset.Load(); offset != 0 && (use.pending != 0 || use.retained) {
			offered := use.firstOfferTime.Add(time.Duration(offset - 1))
			if first.IsZero() || offered.Before(first) {
				first = offered
			}
		}
	}
	return first
}

// Called alongside offered-path accounting. A destination with irreversible
// evidence in this bucket needs no offer or additional admission lock.
func (self *multiClientEventBucket) prepareReceiveAckOfferWithLock(path *IpPath, offer *receiveAckOffer, resetOnly bool) {
	if resetOnly {
		self.terminalResetOffered = true
		return
	}
	if offer == nil {
		return
	}
	destination := receiveAckDestinationOf(path)
	use := self.receiveAckDestinationUses[destination]
	if use != nil && use.retained && use.firstSecuredOffset.Load() != 0 {
		return
	}
	if use == nil {
		use = &receiveAckDestinationUse{destination: destination, firstOfferTime: self.eventTime}
		if self.receiveAckDestinationUses == nil {
			self.receiveAckDestinationUses = map[receiveAckDestination]*receiveAckDestinationUse{}
		}
		self.receiveAckDestinationUses[destination] = use
	}
	offer.use = use
	if use.firstSecuredOffset.Load() == 0 {
		offer.clockOffset = uint64(self.eventTime.Sub(use.firstOfferTime)) + 1
	}
}

// Public success publishes the still-live offer. An inline unwritten terminal
// result cannot become evidence later, and this does not create a fresh bucket
// or extend raw event time.
func (self *multiClientChannel) admitReceiveAckOffer(offer *receiveAckOffer) {
	use := offer.use
	if use == nil {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if use.retired || offer.unwritten {
		return
	}
	offer.observeDisposition(receiveAckClockAdmitted)
	if use.retained {
		return
	}
	if use.pending == 0 {
		if self.receiveAckDestinationBucketCounts == nil {
			self.receiveAckDestinationBucketCounts = map[receiveAckDestination]int{}
		}
		self.receiveAckDestinationBucketCounts[use.destination]++
	}
	if offer.finished {
		use.pending = 0
		use.retained = true
	} else {
		offer.admitted = true
		use.pending++
	}
}

// Terminal ambiguity keeps existing corroboration. Exact unwritten results
// retract only this original. A retained reliable prefix or a written NoAck
// prefix survives a joined result that only reports an unwritten suffix.
func (self *multiClientChannel) finishReceiveAckOffer(offer *receiveAckOffer, noAck bool, err error) {
	use := offer.use
	if use == nil {
		return
	}
	if err == nil {
		offer.observeDisposition(receiveAckClockSecured)
	}
	unwritten := offer.disposition.Load()&receiveAckSourceDispatched == 0 && receiveAckTransferUnwritten(err, noAck)
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if use.retired || use.retained {
		return
	}
	if !offer.admitted {
		offer.finished, offer.unwritten = true, unwritten
		return
	}
	use.pending--
	if !unwritten {
		use.pending = 0
		use.retained = true
	} else if use.pending == 0 {
		self.removeReceiveAckDestinationBucketWithLock(use)
	}
}

// Physical carrier acceptance precedes completion, including per-chunk
// writes. Multiplex the same existing callback to preserve attribution.
func (self *parsedPacketGroup) observeReceiveAckNoAckRouteWrite(transportType TransportType) {
	self.receiveAckOffer.observeCarrierWrite(transportType)
	if attribution := self.transportAttribution; attribution != nil {
		attribution.observe(transportType)
	}
}

// A generic admission or structural error may describe retained bytes. Every
// reported failure must carry exact local proof before retraction is allowed.
// A NoAck route timeout describes its terminal write with no accepting carrier.
func receiveAckTransferUnwritten(err error, noAck bool) bool {
	if err == errSendPackExpiredUnwritten || noAck && err == errTransferRouteWriteTimeout {
		return true
	}
	switch err := err.(type) {
	case *sendPackUnwrittenError, *sendGroupCapacityError:
		return true
	case interface{ Unwrap() []error }:
		causes := err.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !receiveAckTransferUnwritten(cause, noAck) {
				return false
			}
		}
		return true
	case interface{ Unwrap() error }:
		return receiveAckTransferUnwritten(err.Unwrap(), noAck)
	default:
		return false
	}
}

// Release one counted bucket reference without affecting another bucket.
func (self *multiClientChannel) removeReceiveAckDestinationBucketWithLock(use *receiveAckDestinationUse) {
	if count := self.receiveAckDestinationBucketCounts[use.destination]; count == 1 {
		delete(self.receiveAckDestinationBucketCounts, use.destination)
	} else {
		self.receiveAckDestinationBucketCounts[use.destination] = count - 1
	}
}
