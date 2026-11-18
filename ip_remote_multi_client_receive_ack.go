// Receive-ACK health corroboration follows accepted application packet offers
// through their original stats buckets.
package connect

import "sync/atomic"

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
// Channel stateLock guards its lifetime and counts.
type receiveAckDestinationUse struct {
	destination receiveAckDestination
	pending     int
	retained    bool
	retired     bool
}

// One original offer can complete before its admission call returns. The
// immutable destination entry is set before handoff, and stateLock orders the
// public return and completion. The source sets dispatched.
type receiveAckOffer struct {
	use        *receiveAckDestinationUse
	dispatched atomic.Bool
	admitted   bool
	finished   bool
	unwritten  bool
}

// Called alongside offered-path accounting. A destination with irreversible
// evidence in this bucket needs no offer or additional admission lock.
func (self *multiClientEventBucket) prepareReceiveAckOfferWithLock(path *IpPath, offer *receiveAckOffer) {
	if offer == nil {
		return
	}
	destination := receiveAckDestinationOf(path)
	use := self.receiveAckDestinationUses[destination]
	if use != nil && use.retained {
		return
	}
	if use == nil {
		use = &receiveAckDestinationUse{destination: destination}
		if self.receiveAckDestinationUses == nil {
			self.receiveAckDestinationUses = map[receiveAckDestination]*receiveAckDestinationUse{}
		}
		self.receiveAckDestinationUses[destination] = use
	}
	offer.use = use
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
	if use.retired || use.retained || offer.unwritten {
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
	unwritten := !offer.dispatched.Load() && receiveAckTransferUnwritten(err, noAck)
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
	self.receiveAckOffer.dispatched.Store(true)
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
