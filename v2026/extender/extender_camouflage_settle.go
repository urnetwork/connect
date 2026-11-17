package extender

// extender_camouflage_settle.go — the safe flip from Phase A to Phase B
// (EXTENDER-UPDATE2 §3.2, EXTENDER-SPLICE-ROLLOUT.md).
//
// A legacy client's untagged hello cannot be told from a prober's, so a splicing
// extender sends it to the borrowed site, where it fails its identity check. The
// release hands a record that requires tcp-camouflage-v1 only to a client that
// declares the capability, so splicing strands no legacy client once every record
// issued without the requirement has expired. That is one record lifetime after
// the operator first signed the requirement.
//
// ExtenderCamouflageSpliceSettle decides that from this extender's own activation
// records, and its Open is the value of ExtenderSettings.CamouflageSpliceGate. A
// record qualifies when it names this identity, carries this extender's own
// camouflage key, and requires tcp-camouflage-v1. The gate opens only after an
// unbroken run of qualifying records has lasted the longest lifetime in the run
// plus a margin, which allows for a client clock behind the operator's and for
// the operator's processes restarting in waves, during which some still sign
// without the requirement.
//
// Anything else closes the gate at once and restarts the wait when the records
// qualify again:
//
//   - a record without the key or the requirement, which is how a rollback
//     reaches the extender;
//   - an activation whose record does not decode or verify, since nothing then
//     says the operator still signs them;
//   - the latest record expiring, since the operator may have signed anything
//     in the gap, and a record that arrives already expired.
//
// Nothing is persisted, so a restart restarts the wait: the cohort host runs as
// Phase A for that long. That errs toward compatibility, since a run seen before
// a restart says nothing about the records issued while it was down.
//
// Safe for concurrent use. Open takes one short lock and never blocks on I/O, as
// the gate runs on the accept path.

import (
	"bytes"
	"crypto/ed25519"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

// The settings of an ExtenderCamouflageSpliceSettle.
type ExtenderCamouflageSpliceSettleSettings struct {
	// How long past one record lifetime the gate waits (see the file comment).
	Margin time.Duration
	// The clock, which tests replace.
	Now func() time.Time
}

func DefaultExtenderCamouflageSpliceSettleSettings() *ExtenderCamouflageSpliceSettleSettings {
	return &ExtenderCamouflageSpliceSettleSettings{
		Margin: time.Hour,
		Now:    time.Now,
	}
}

// ExtenderCamouflageSpliceSettle is the safe-flip gate (see the file comment).
type ExtenderCamouflageSpliceSettle struct {
	identityPublicKey ed25519.PublicKey
	realityPublicKey  []byte
	settings          *ExtenderCamouflageSpliceSettleSettings

	stateLock sync.Mutex
	// when the current run of qualifying records began, zero with no run
	runStartTime time.Time
	// the longest lifetime of a record in the run
	runRecordLifetime time.Duration
	// the latest expiry of a record in the run
	runExpireTime time.Time
}

// NewExtenderCamouflageSpliceSettle is a closed gate for the extender with this
// identity key and camouflage key. A nil settings takes the defaults.
func NewExtenderCamouflageSpliceSettle(
	identityPublicKey ed25519.PublicKey,
	realityPublicKey []byte,
	settings *ExtenderCamouflageSpliceSettleSettings,
) *ExtenderCamouflageSpliceSettle {
	if settings == nil {
		settings = DefaultExtenderCamouflageSpliceSettleSettings()
	}
	return &ExtenderCamouflageSpliceSettle{
		identityPublicKey: identityPublicKey,
		realityPublicKey:  bytes.Clone(realityPublicKey),
		settings:          settings,
	}
}

// ObserveActivation reads the record of one successful activation
// (ExtenderActivatorSettings.OnActivated), verified under rootKeys. An activation
// with no record, or one that does not decode or verify, closes the gate.
func (self *ExtenderCamouflageSpliceSettle) ObserveActivation(
	result *connect.ExtenderActivateResult,
	rootKeys *connect.ExtenderRootKeySet,
) {
	if result == nil || result.Record == "" || rootKeys == nil {
		self.close()
		return
	}
	record, err := connect.DecodeExtenderRecordBase64(result.Record)
	if err != nil {
		self.close()
		return
	}
	body, err := rootKeys.VerifyRecord(record)
	if err != nil {
		self.close()
		return
	}
	self.ObserveRecord(body)
}

// ObserveRecord reads the verified body of one of this extender's records. A
// record of another identity changes nothing. It is the caller's to verify the
// body under the root keys; ObserveActivation does.
func (self *ExtenderCamouflageSpliceSettle) ObserveRecord(body *protocol.ExtenderRecordBody) {
	if body == nil || !bytes.Equal(body.PublicKey, self.identityPublicKey) {
		return
	}
	now := self.settings.Now()
	issueTime := time.UnixMilli(int64(body.IssueTimeMs))
	expireTime := time.UnixMilli(int64(body.ExpireTimeMs))
	recordLifetime := expireTime.Sub(issueTime)

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if !self.recordQualifies(body) || recordLifetime <= 0 || expireTime.Before(now) {
		self.closeWithLock()
		return
	}
	// a run whose latest record expired has a gap the extender saw nothing of
	if !self.runStartTime.IsZero() && self.runExpireTime.Before(now) {
		self.closeWithLock()
	}
	if self.runStartTime.IsZero() {
		self.runStartTime = now
	}
	self.runRecordLifetime = max(self.runRecordLifetime, recordLifetime)
	if self.runExpireTime.Before(expireTime) {
		self.runExpireTime = expireTime
	}
}

// Whether a record carries this extender's own camouflage key and requires
// tcp-camouflage-v1. Capabilities compare as ExtenderCapabilitiesSatisfied does.
func (self *ExtenderCamouflageSpliceSettle) recordQualifies(body *protocol.ExtenderRecordBody) bool {
	if len(self.realityPublicKey) == 0 || !bytes.Equal(body.RealityPublicKey, self.realityPublicKey) {
		return false
	}
	for _, capability := range body.RequiredCapabilities {
		if strings.EqualFold(strings.TrimSpace(capability), connect.ExtenderCapabilityTcpCamouflageV1) {
			return true
		}
	}
	return false
}

// Open reports whether the run has lasted long enough for every record issued
// without the requirement to have expired. It is the value of
// ExtenderSettings.CamouflageSpliceGate.
func (self *ExtenderCamouflageSpliceSettle) Open() bool {
	now := self.settings.Now()

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if self.runStartTime.IsZero() || self.runExpireTime.Before(now) {
		return false
	}
	return !now.Before(self.runStartTime.Add(self.runRecordLifetime + self.settings.Margin))
}

func (self *ExtenderCamouflageSpliceSettle) close() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.closeWithLock()
}

func (self *ExtenderCamouflageSpliceSettle) closeWithLock() {
	self.runStartTime = time.Time{}
	self.runRecordLifetime = 0
	self.runExpireTime = time.Time{}
}
