package connect

// The dial side of the peer-to-peer webrtc extender carrier on a device
// (EXTENDER.md S8). A device reaches an extender's carrier by signaling its
// offer to the rendezvous id the extender's verified record carries
// (ExtenderRecordBody.WebRtcClientId), through the exchange connection of its
// own client (WebRtcManager.ExtenderCarrierExchanger). So the carrier is a
// second path for a device that already has an exchange connection, and never
// the bootstrap: with no client there is nothing to signal through, and the
// socket carriers and the dns carrier remain the paths of first contact.
//
// WebRtcExtenderDialSignaling holds the client a device signals through, which
// comes and goes with the device's provider client, and resolves an extender's
// identity key to its exchanger through the directory. It also opens the
// directory's webrtc gate (ExtenderDirectory.SetWebRtcCarrierEnabled) exactly
// while it holds a client: a webrtc dial with no signaling path can only fail,
// and a failure counts against the address's other carriers (S9).
//
// Safe for concurrent use.

import (
	"sync"
)

// WebRtcExtenderDialSignaling is the signaling of one device's webrtc dials.
type WebRtcExtenderDialSignaling struct {
	directory *ExtenderDirectory

	stateLock sync.Mutex
	manager   *WebRtcManager
	clientId  Id

	// orders the directory gate's updates, so two changes that race apply in
	// the order their state changed. It guards no state of its own, is always
	// taken before stateLock and never while holding it, and is held across
	// the directory's SetWebRtcCarrierEnabled, which takes only the
	// directory's own lock and calls nothing back.
	gateLock sync.Mutex
}

// NewWebRtcExtenderDialSignaling builds the signaling of a device whose
// extender candidates come from directory. It starts with no client, so the
// directory's gate is left as it is until SetClient opens it.
func NewWebRtcExtenderDialSignaling(directory *ExtenderDirectory) *WebRtcExtenderDialSignaling {
	return &WebRtcExtenderDialSignaling{
		directory: directory,
	}
}

// SetClient makes manager, the manager of the client clientId, the one the
// device's offers go out through, and opens the directory's webrtc gate. A nil
// manager is ClearClient of whichever client was set.
func (self *WebRtcExtenderDialSignaling) SetClient(manager *WebRtcManager, clientId Id) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.manager = manager
		if manager == nil {
			self.clientId = Id{}
		} else {
			self.clientId = clientId
		}
	}()
	self.applyGate()
}

// ClearClient forgets manager when it is still the one set, and closes the
// directory's webrtc gate. A manager that was replaced by a later SetClient is
// left alone, so the close of an old client never takes the path of its
// successor away.
func (self *WebRtcExtenderDialSignaling) ClearClient(manager *WebRtcManager) {
	cleared := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if manager == nil || self.manager != manager {
			return false
		}
		self.manager = nil
		self.clientId = Id{}
		return true
	}()
	if cleared {
		self.applyGate()
	}
}

// HasClient reports whether a client is set to signal through.
func (self *WebRtcExtenderDialSignaling) HasClient() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.manager != nil
}

// The directory's gate follows the state as it is when the update runs. An
// update that read an older state cannot land after a newer one: each reads
// and applies under gateLock.
func (self *WebRtcExtenderDialSignaling) applyGate() {
	if self.directory == nil {
		return
	}
	self.gateLock.Lock()
	defer self.gateLock.Unlock()
	self.directory.SetWebRtcCarrierEnabled(self.HasClient())
}

// Resolve is the carrier's resolver (WebRtcExtenderExchangerResolver): the
// exchanger to the rendezvous id the verified record of extenderPublicKey
// carries, through the client set. false with no client set, for a key the
// directory has no record id for, and for a rendezvous id that is the device's
// own client: a device that is itself an extender never dials its own carrier.
func (self *WebRtcExtenderDialSignaling) Resolve(extenderPublicKey []byte) (WebRtcExtenderOfferExchanger, bool) {
	if self.directory == nil || len(extenderPublicKey) == 0 {
		return nil, false
	}
	manager, clientId := func() (*WebRtcManager, Id) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.manager, self.clientId
	}()
	if manager == nil {
		return nil, false
	}
	rendezvousId, ok := self.directory.WebRtcClientId(extenderPublicKey)
	if !ok || rendezvousId == clientId {
		return nil, false
	}
	return manager.ExtenderCarrierExchanger(rendezvousId), true
}
