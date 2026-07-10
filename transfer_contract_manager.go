package connect

import (
	"context"
	"sync"
	"time"

	// "errors"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"fmt"

	// "slices"
	// "runtime/debug"
	mathrand "math/rand"

	"golang.org/x/exp/maps"

	// "google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/protocol"
)

// manage contracts which are embedded into each transfer sequence

type ContractKey struct {
	Destination       TransferPath
	IntermediaryIds   MultiHopId
	CompanionContract bool
	ForceStream       bool
	// EncryptionRole separates the contract queues of the two per-peer
	// encryption send sequences to the same destination: the client-role
	// sequence (normal application data) and the server-role sequence
	// (EncryptedControl carrier + server replies). Without this, both would
	// share one queue, and one sequence's exit-flush (`FlushContractQueue`
	// on idle) would discard the other's pending contracts — starving the
	// handshake carrier. Zero value is client, so non-encrypted traffic and
	// legacy/pushed contracts key the same as before.
	EncryptionRole sequenceTlsRole
	// EncryptionCompanion separates the contract queues of two same-role send
	// sequences differing only by session identity companion — the two
	// server-role reply carriers that echo a companion vs non-companion
	// initiator both ride the same EncryptionControlUseCompanion contract, so
	// `CompanionContract` alone doesn't separate them. Without this they share a
	// queue and starve each other on exit-flush, as `EncryptionRole` guards for
	// the client/server split. Zero value false, so non-encrypted and
	// legacy/pushed contracts key as before.
	EncryptionCompanion bool
}

func (self ContractKey) Legacy() ContractKey {
	return ContractKey{
		Destination: self.Destination,
	}
}

type ContractStatus struct {
	Key     ContractKey
	Error   *protocol.ContractError
	Premium bool
}

type ContractStatusFunction = func(ContractStatus *ContractStatus)

type contractStatusCallbackWorker struct {
	ctx                     context.Context
	cancel                  context.CancelFunc
	callback                ContractStatusFunction
	receiveContractStatuses chan *ContractStatus
}

func newContractStatusCallbackWorker(ctx context.Context, callback ContractStatusFunction, bufferSize int) *contractStatusCallbackWorker {
	callbackCtx, cancel := context.WithCancel(ctx)
	worker := &contractStatusCallbackWorker{
		ctx:                     callbackCtx,
		cancel:                  cancel,
		callback:                callback,
		receiveContractStatuses: make(chan *ContractStatus, bufferSize),
	}
	go HandleError(worker.run, cancel)
	return worker
}

func (self *contractStatusCallbackWorker) run() {
	for {
		select {
		case <-self.ctx.Done():
			return
		case contractStatus := <-self.receiveContractStatuses:
			if self.ctx.Err() != nil {
				return
			}
			HandleError(func() {
				self.callback(contractStatus)
			})
		}
	}
}

func (self *contractStatusCallbackWorker) Dispatch(contractStatus *ContractStatus) {
	select {
	case <-self.ctx.Done():
	case self.receiveContractStatuses <- contractStatus:
	}
}

func (self *contractStatusCallbackWorker) Close() {
	self.cancel()
}

type ContractManagerStats struct {
	ContractOpenCount  int64
	ContractCloseCount int64
	// contract id -> byte count
	ContractOpenByteCounts map[Id]ByteCount
	// contract id -> contract key
	ContractOpenKeys              map[Id]ContractKey
	ContractCloseByteCount        ByteCount
	ReceiveContractCloseByteCount ByteCount
}

func NewContractManagerStats() *ContractManagerStats {
	return &ContractManagerStats{
		ContractOpenCount:             0,
		ContractCloseCount:            0,
		ContractOpenByteCounts:        map[Id]ByteCount{},
		ContractOpenKeys:              map[Id]ContractKey{},
		ContractCloseByteCount:        0,
		ReceiveContractCloseByteCount: 0,
	}
}

func (self *ContractManagerStats) ContractOpenByteCount() ByteCount {
	netContractOpenByteCount := ByteCount(0)
	for _, contractOpenByteCount := range self.ContractOpenByteCounts {
		netContractOpenByteCount += contractOpenByteCount
	}
	return netContractOpenByteCount
}

// SignStoredContract returns the HMAC signature for a stored contract using the
// format appropriate for the current time relative to
// settings.NetworkEventTimeChangeHmac. Before that time, signers emit the
// legacy form (`mac.Sum(storedContractBytes)`, which appends a key-only HMAC
// to the contract bytes). At or after that time, signers emit the standard
// form (`mac.Write(storedContractBytes); mac.Sum(nil)`).
//
// Both connect and server/connect must use this helper so the cutover is
// consistent across client and server.
func SignStoredContract(settings *ContractManagerSettings, provideSecretKey []byte, storedContractBytes []byte) []byte {
	mac := hmac.New(sha256.New, provideSecretKey)
	if time.Now().Before(settings.NetworkEventTimeChangeHmac) {
		// legacy: this leaves HMAC(key, "") in the trailing 32 bytes of the
		// returned slice. preserved for backward compatibility.
		return mac.Sum(storedContractBytes)
	}
	mac.Write(storedContractBytes)
	return mac.Sum(nil)
}

// VerifyStoredContract validates a stored-contract HMAC against the provide
// secret key, accepting both the legacy and standard HMAC formats so that
// signers may cross over at settings.NetworkEventTimeChangeHmac without
// breaking compatibility with peers that have not yet cut over.
func VerifyStoredContract(settings *ContractManagerSettings, provideSecretKey []byte, storedContractBytes []byte, storedContractHmac []byte) bool {
	legacyMac := hmac.New(sha256.New, provideSecretKey)
	if hmac.Equal(storedContractHmac, legacyMac.Sum(storedContractBytes)) {
		return true
	}
	standardMac := hmac.New(sha256.New, provideSecretKey)
	standardMac.Write(storedContractBytes)
	return hmac.Equal(storedContractHmac, standardMac.Sum(nil))
}

func DefaultContractManagerSettings() *ContractManagerSettings {
	return DefaultContractManagerSettingsWithBufferSize(defaultTransferBufferSize)
}

func DefaultContractManagerSettingsWithBufferSize(bufferSize int) *ContractManagerSettings {
	// NETWORK EVENT: at the enable contracts date, all clients will require contracts
	// up to that time, contracts are optional for the sender and match for the receiver
	networkEventTimeEnableContracts, err := time.Parse(time.RFC3339, "2024-05-01T00:00:00Z")
	if err != nil {
		panic(err)
	}
	// NETWORK EVENT: at the change-hmac date, signers cut over from the legacy
	// HMAC format to the standard form. verifiers accept both forms at all
	// times so the cutover can be deployed asymmetrically.
	// Pushed out from the original 2026-07-01: clients on connect < v2026.5.14
	// (2026-05-13) have legacy-only verifiers and cannot verify the standard
	// form, so the original date broke them the moment it passed. Hold the
	// cutover until that older fleet has drained.
	networkEventTimeChangeHmac, err := time.Parse(time.RFC3339, "2026-09-01T00:00:00Z")
	if err != nil {
		panic(err)
	}
	return &ContractManagerSettings{
		SequenceBufferSize:                bufferSize,
		InitialContractTransferByteCount:  kib(16),
		StandardContractTransferByteCount: mib(128),
		ContractTransferByteSeqScale:      4,

		NetworkEventTimeEnableContracts: networkEventTimeEnableContracts,
		NetworkEventTimeChangeHmac:      networkEventTimeChangeHmac,

		ProvidePingTimeout: 0,

		OriginContractLinger: 300 * time.Second,

		ContractQueueExpireTimeout: 120 * time.Second,

		ContractStatsEpoch: 1 * time.Second,

		ProtocolVersion: DefaultProtocolVersion,

		// TODO remove
		LegacyCreateContract: false,
		// TODO remove
		TrackUsedContracts: false,
	}
}

func DefaultContractManagerSettingsNoNetworkEvents() *ContractManagerSettings {
	settings := DefaultContractManagerSettings()
	settings.NetworkEventTimeEnableContracts = time.Time{}
	settings.NetworkEventTimeChangeHmac = time.Time{}
	return settings
}

type ContractManagerSettings struct {
	SequenceBufferSize int

	// this should be enough to do a single ping
	InitialContractTransferByteCount  ByteCount
	StandardContractTransferByteCount ByteCount
	// scale up the contract size over this many contracts
	ContractTransferByteSeqScale uint64

	// enable contracts on the network
	// this can be removed after wide adoption
	NetworkEventTimeEnableContracts time.Time

	// cut over the stored-contract HMAC signing format. before this time,
	// SignStoredContract emits the legacy form (mac.Sum(bytes)); at or after,
	// it emits the standard form (mac.Write(bytes); mac.Sum(nil)). verifiers
	// accept both forms at all times.
	NetworkEventTimeChangeHmac time.Time

	// an active ping to the control fast-tracks any timeouts
	ProvidePingTimeout time.Duration

	// server-side companion policy: allow a return (companion) contract to be
	// created for up to this long after the origin contract in the opposite
	// direction was closed, so reply traffic can resume after the request side
	// goes idle.
	OriginContractLinger time.Duration

	// expire queued contracts that no sequence has taken within this window.
	// Bounds `destinationContracts` growth from orphans (e.g. a
	// `CreateContractResult` that lands after the owning sequence exit-flushed
	// its queue, for a destination that is never used again), and prevents
	// handing out a stale contract the platform may have already force-closed
	// server-side — keep this below the platform's unused-contract force-close
	// window (5 minutes). <= 0 disables expiry.
	ContractQueueExpireTimeout time.Duration

	// the epoch for emitting open contract usage events to
	// `AddContractStatsCallback` listeners
	ContractStatsEpoch time.Duration

	ProtocolVersion int

	// TODO remove
	LegacyCreateContract bool
	// TODO remove
	TrackUsedContracts bool
}

func (self *ContractManagerSettings) ContractsEnabled() bool {
	return self.NetworkEventTimeEnableContracts.Before(time.Now())
}

type ContractManager struct {
	ctx    context.Context
	client *Client

	settings *ContractManagerSettings

	mutex sync.Mutex

	// `provideSecretKeys` retains all keys until app restart (typically system restart)
	// this makes it faster for clients to reconnect with existing contracts
	// otherwise the client will have to time out the send sequence and flush its pending contracts
	provideSecretKeys map[protocol.ProvideMode][]byte
	provideModes      map[protocol.ProvideMode]bool
	// provide paused overrides the set provide modes
	providePaused  bool
	provideMonitor *Monitor

	destinationContracts map[ContractKey]*contractQueue

	receiveNoContractClientIds map[Id]bool
	sendNoContractClientIds    map[Id]bool

	contractStatusCallbacks *CallbackList[*contractStatusCallbackWorker]

	localStats *ContractManagerStats

	// open contract usage, updated by the owning sequences and
	// emitted per epoch (see transfer_contract_stats.go)
	contractStatsLock      sync.Mutex
	contractStatsEntries   map[contractStatsKey]*contractStatsEntry
	contractStatsCallbacks *CallbackList[ContractStatsFunction]
	contractStatsStarted   bool

	controlSyncProvide    *ControlSync
	controlSyncProvideOob *ControlSyncOob
}

func NewContractManagerWithDefaults(ctx context.Context, client *Client) *ContractManager {
	return NewContractManager(ctx, client, DefaultContractManagerSettings())
}

func NewContractManager(
	ctx context.Context,
	client *Client,
	settings *ContractManagerSettings,
) *ContractManager {
	// at a minimum
	// - messages to/from the platform (ControlId) do not need a contract
	//   this is because the platform is needed to create contracts
	// - messages to self do not need a contract
	receiveNoContractClientIds := map[Id]bool{
		ControlId:         true,
		client.ClientId(): true,
	}
	sendNoContractClientIds := map[Id]bool{
		ControlId:         true,
		client.ClientId(): true,
	}

	contractManager := &ContractManager{
		ctx:                        ctx,
		client:                     client,
		settings:                   settings,
		provideSecretKeys:          map[protocol.ProvideMode][]byte{},
		provideModes:               map[protocol.ProvideMode]bool{},
		providePaused:              false,
		provideMonitor:             NewMonitor(),
		destinationContracts:       map[ContractKey]*contractQueue{},
		receiveNoContractClientIds: receiveNoContractClientIds,
		sendNoContractClientIds:    sendNoContractClientIds,
		contractStatusCallbacks:    NewCallbackList[*contractStatusCallbackWorker](),
		localStats:                 NewContractManagerStats(),
		contractStatsEntries:       map[contractStatsKey]*contractStatsEntry{},
		contractStatsCallbacks:     NewCallbackList[ContractStatsFunction](),
		controlSyncProvide:         NewControlSync(ctx, client, "provide"),
		controlSyncProvideOob:      NewControlSyncOob(ctx, client, "provide-oob"),
	}

	if client.ClientId() != ControlId {
		go HandleError(contractManager.providePing, client.Cancel)
	}

	go HandleError(contractManager.expireQueuedContracts, client.Cancel)

	return contractManager
}

// expireQueuedContracts periodically closes queued contracts that no sequence
// took within `ContractQueueExpireTimeout` and removes the emptied queues.
// This bounds `destinationContracts` against orphans — e.g. a
// `CreateContractResult` that lands after the owning sequence exit-flushed its
// queue (`FlushContractQueue` force-remove) re-creates the queue entry, and if
// that destination is never used again (provider rotation) the entry would
// otherwise be retained forever.
func (self *ContractManager) expireQueuedContracts() {
	timeout := self.settings.ContractQueueExpireTimeout

	// the contract manager is closing: close all still-queued (pending)
	// contracts so their escrow is released promptly. `closeContracts`
	// routes shutdown closes over the out-of-band api on a Background
	// context, since the client context (and the in-band transport) is
	// already closed.
	finalFlush := func() {
		pending := []*protocol.Contract{}
		func() {
			self.mutex.Lock()
			defer self.mutex.Unlock()

			for contractKey, contractQueue := range self.destinationContracts {
				pending = append(pending, contractQueue.Flush(false)...)
				if contractQueue.IsDone() {
					delete(self.destinationContracts, contractKey)
				}
			}
		}()
		if 0 < len(pending) {
			if self.client.log.V(1).Enabled() {
				self.client.log.Infof("[contract]closing %d pending contracts on close\n", len(pending))
			}
			self.closeContracts(pending)
		}
	}

	for {
		// when expiry is disabled the nil tick channel blocks forever and the
		// loop only waits for shutdown
		var tick <-chan time.Time
		if 0 < timeout {
			tick = time.After(timeout / 2)
		}

		select {
		case <-self.ctx.Done():
			finalFlush()
			return
		case <-self.client.Done():
			// the manager ctx is the client's parent ctx; the client closing
			// is the shutdown signal
			finalFlush()
			return
		case <-tick:
		}

		minEnqueueTime := time.Now().Add(-timeout)
		expired := []*protocol.Contract{}
		func() {
			self.mutex.Lock()
			defer self.mutex.Unlock()

			for contractKey, contractQueue := range self.destinationContracts {
				expired = append(expired, contractQueue.Expire(minEnqueueTime)...)
				if contractQueue.IsDone() {
					delete(self.destinationContracts, contractKey)
				}
			}
		}()
		if 0 < len(expired) {
			if self.client.log.V(1).Enabled() {
				self.client.log.Infof("[contract]expired %d queued contracts\n", len(expired))
			}
			// close outside the manager mutex: CloseContract re-takes it
			self.closeContracts(expired)
		}
	}
}

func (self *ContractManager) providePing() {
	if self.settings.ProvidePingTimeout == 0 {
		return
	}

	// Wait for the client to finish wiring before our first send. This
	// goroutine is started from `NewContractManager`, which runs inside
	// `NewClientWithTag` before `initBuffers` constructs `sendBuffer`.
	// Without this gate the ping path can race the buffer wiring.
	select {
	case <-self.client.ReadyNotify():
	case <-self.ctx.Done():
		return
	}

	// used for logging states only
	logWait := false

	waitForProvide := func() bool {
		for {
			notify := self.provideMonitor.NotifyChannel()
			var provide bool
			func() {
				self.mutex.Lock()
				defer self.mutex.Unlock()

				if self.providePaused {
					provide = false
				} else {
					provide = self.provideModes[protocol.ProvideMode_Public] || self.provideModes[protocol.ProvideMode_PublicStream]
				}
			}()
			if provide {
				if logWait {
					logWait = false
					self.client.log.Infof("[contract]provide ping continue\n")
				}
				return true
			}
			if !logWait {
				logWait = true
				self.client.log.Infof("[contract]provide ping wait\n")
			}
			select {
			case <-self.ctx.Done():
				return false
			case <-notify:
			}
		}
	}

	lastPingTime := time.Time{}
	for {
		if !waitForProvide() {
			return
		}

		// uniform timeout with mean `ProvidePingTimeout`
		timeout := time.Duration(mathrand.Int63n(int64(2*self.settings.ProvidePingTimeout))) - time.Now().Sub(lastPingTime)
		if 0 < timeout {
			select {
			case <-self.ctx.Done():
				return
			case <-WakeupAfter(timeout, self.settings.ProvidePingTimeout):
			}
		} else {
			select {
			case <-self.ctx.Done():
				return
			default:
			}
		}

		ack := make(chan error)
		providePing := &protocol.ProvidePing{}
		frame, err := ToFrame(providePing, self.settings.ProtocolVersion)
		if err != nil {
			self.client.log.Infof("[contract]could not create provide ping frame = %s", err)
			return
		}
		self.client.SendControl(frame, func(err error) {
			select {
			case ack <- err:
			case <-self.ctx.Done():
			}
		})
		// wait for the ack before sending another ping
		select {
		case err := <-ack:
			if err != nil {
				self.client.log.Infof("[contract]provide ping err = %s\n", err)
			}
		case <-self.ctx.Done():
			return
		}
		lastPingTime = time.Now()
	}
}

func (self *ContractManager) StandardContractTransferByteCount() ByteCount {
	return self.settings.StandardContractTransferByteCount
}

func (self *ContractManager) AddContractStatusCallback(contractStatusCallback ContractStatusFunction) func() {
	worker := newContractStatusCallbackWorker(self.ctx, contractStatusCallback, self.settings.SequenceBufferSize)
	callbackId := self.contractStatusCallbacks.Add(worker)
	return func() {
		self.contractStatusCallbacks.Remove(callbackId)
		worker.Close()
	}
}

// ContractStatusFunction
func (self *ContractManager) contractStatus(contractStatus *ContractStatus) {
	for _, contractStatusCallback := range self.contractStatusCallbacks.Get() {
		contractStatusCallback.Dispatch(contractStatus)
	}
}

/*
// ReceiveFunction
func (self *ContractManager) Receive(source TransferPath, frames []*protocol.Frame, peer Peer) {
	if source.IsControlSource() {
		for _, frame := range frames {
			self.handleControlFrame(nil, frame)
		}
	}
}
*/

func (self *ContractManager) HandleControlFrame(contractKey ContractKey, frame *protocol.Frame) error {
	switch frame.MessageType {
	case protocol.MessageType_TransferCreateContractResult:
		contracts, contractErrors := self.parseControlFrame(frame)
		for _, contract := range contracts {
			c := func() error {
				var contractStatus *ContractStatus
				defer func() {
					if contractStatus != nil {
						self.contractStatus(contractStatus)
					}
				}()
				err := self.addContract(contractKey, contract)
				if err != nil {
					// contract rejected
					contractError := protocol.ContractError_Trust
					contractStatus = &ContractStatus{
						Key:   contractKey,
						Error: &contractError,
					}
					return err
				}
				storedContract := &protocol.StoredContract{}
				err = ProtoUnmarshal(contract.StoredContractBytes, storedContract)
				if err != nil {
					contractError := protocol.ContractError_Invalid
					contractStatus = &ContractStatus{
						Key:   contractKey,
						Error: &contractError,
					}
					return err
				}
				premium := false
				if storedContract.Priority != nil {
					premium = 0 < *storedContract.Priority
				}
				contractStatus = &ContractStatus{
					Key:     contractKey,
					Premium: premium,
				}
				return nil
			}
			if self.client.log.V(2).Enabled() {
				TraceWithReturn(
					"[contract]add",
					c,
				)
			} else {
				c()
			}
		}
		for _, contractError := range contractErrors {
			if self.client.log.V(1).Enabled() {
				self.client.log.Infof("[contract]error = %s\n", contractError)
			}
			c := func() {
				contractStatus := &ContractStatus{
					Key:   contractKey,
					Error: &contractError,
				}

				self.contractStatus(contractStatus)
			}
			if self.client.log.V(2).Enabled() {
				Trace(
					fmt.Sprintf("[contract]error = %s", contractError),
					c,
				)
			} else {
				c()
			}
		}
	}
	return nil
}

// frames are verified before calling to be from source ControlId
func (self *ContractManager) parseControlFrame(frame *protocol.Frame) (
	contracts []*protocol.Contract,
	contractErrors []protocol.ContractError,
) {
	addResult := func(v *protocol.CreateContractResult) {
		if contractError := v.Error; contractError != nil {
			contractErrors = append(contractErrors, *contractError)
		} else if contract := v.Contract; contract != nil {
			storedContract := &protocol.StoredContract{}
			err := ProtoUnmarshal(contract.StoredContractBytes, storedContract)
			if err != nil {
				return
			}

			contracts = append(contracts, contract)
		}
	}

	switch frame.MessageType {
	case protocol.MessageType_TransferCreateContractResult:
		b := make([]byte, len(frame.MessageBytes))
		copy(b, frame.MessageBytes)
		r := &protocol.CreateContractResult{}
		err := ProtoUnmarshal(b, r)
		if err == nil {
			addResult(r)
		}
	}
	return
}

func (self *ContractManager) GetProvideSecretKeys() map[protocol.ProvideMode][]byte {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	return maps.Clone(self.provideSecretKeys)
}

func (self *ContractManager) LoadProvideSecretKeys(provideSecretKeys map[protocol.ProvideMode][]byte) {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	for provideMode, provideSecretKey := range provideSecretKeys {
		self.provideSecretKeys[provideMode] = provideSecretKey
	}
}

func (self *ContractManager) InitProvideSecretKeys() {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	for i, _ := range protocol.ProvideMode_name {
		provideMode := protocol.ProvideMode(i)
		provideSecretKey, ok := self.provideSecretKeys[provideMode]
		if !ok {
			// generate a new key
			provideSecretKey = make([]byte, 32)
			_, err := rand.Read(provideSecretKey)
			if err != nil {
				panic(err)
			}
			self.provideSecretKeys[provideMode] = provideSecretKey
		}
	}
}

func (self *ContractManager) SetProvidePaused(providePaused bool) bool {
	changed := false
	func() {
		self.mutex.Lock()
		defer self.mutex.Unlock()

		if self.providePaused != providePaused {
			self.providePaused = providePaused
			self.provideMonitor.NotifyAll()
			changed = true
		}
	}()
	if changed {
		if provideFrame, err := self.provideFrame(); err == nil && provideFrame != nil {
			self.controlSyncProvide.Send(
				provideFrame,
				nil,
				nil,
			)
		}
		return true
	}
	return false
}

func (self *ContractManager) IsProvidePaused() bool {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	return self.providePaused
}

func (self *ContractManager) provideFrame() (*protocol.Frame, error) {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	var provide *protocol.Provide
	if self.providePaused {
		// pause stops providing to public/ff only.
		// keep ProvideMode_Stream to allow return traffic and
		// ProvideMode_Network so network peers never fall back to stream, if set
		provideKeys := []*protocol.ProvideKey{}
		for provideMode, allow := range self.provideModes {
			if allow && (provideMode == protocol.ProvideMode_Stream || provideMode == protocol.ProvideMode_Network) {
				provideSecretKey, ok := self.provideSecretKeys[provideMode]
				if ok {
					provideKeys = append(provideKeys, &protocol.ProvideKey{
						Mode:             provideMode,
						ProvideSecretKey: provideSecretKey,
					})
				} else {
					self.client.log.Infof("[contract]missing provide key for %d. Will omit.\n", provideMode)
				}
			}
		}

		provide = &protocol.Provide{
			Keys: provideKeys,
		}
	} else {
		provideKeys := []*protocol.ProvideKey{}
		for provideMode, allow := range self.provideModes {
			if allow {
				provideSecretKey, ok := self.provideSecretKeys[provideMode]
				if ok {
					provideKeys = append(provideKeys, &protocol.ProvideKey{
						Mode:             provideMode,
						ProvideSecretKey: provideSecretKey,
					})
				} else {
					self.client.log.Infof("[contract]missing provide key for %d. Will omit.\n", provideMode)
				}
			}
		}

		provide = &protocol.Provide{
			Keys: provideKeys,
		}
	}
	provideFrame, err := ToFrame(provide, self.settings.ProtocolVersion)
	if err != nil {
		self.client.log.Infof("[contract]could not create provide frame = %s", err)
		return nil, err
	}
	return provideFrame, nil
}

func (self *ContractManager) SetProvideModesWithReturnTraffic(provideModes map[protocol.ProvideMode]bool) {
	self.SetProvideModesWithReturnTrafficWithAckCallback(provideModes, func(err error) {})
}

// clients must enable `ProvideMode_Stream` to allow return traffic
func (self *ContractManager) SetProvideModesWithReturnTrafficWithAckCallback(provideModes map[protocol.ProvideMode]bool, ackCallback func(err error)) {
	updatedProvideModes := map[protocol.ProvideMode]bool{}
	maps.Copy(updatedProvideModes, provideModes)
	updatedProvideModes[protocol.ProvideMode_Stream] = true
	self.SetProvideModesWithAckCallback(updatedProvideModes, ackCallback)
}

func (self *ContractManager) SetProvideModes(provideModes map[protocol.ProvideMode]bool) {
	self.SetProvideModesWithAckCallback(provideModes, func(err error) {})
}

// applyProvideModes generates any missing provide secret keys and updates the
// active provide modes. The provide frame must be (re)sent afterward to register
// the change with the platform.
func (self *ContractManager) applyProvideModes(provideModes map[protocol.ProvideMode]bool) {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	// keep all keys (see note on `provideSecretKeys`)
	for provideMode, allow := range provideModes {
		if allow {
			provideSecretKey, ok := self.provideSecretKeys[provideMode]
			if !ok {
				// generate a new key
				provideSecretKey = make([]byte, 32)
				_, err := rand.Read(provideSecretKey)
				if err != nil {
					panic(err)
				}
				self.provideSecretKeys[provideMode] = provideSecretKey
			}
		}
	}

	self.provideModes = maps.Clone(provideModes)
	self.provideMonitor.NotifyAll()
}

func (self *ContractManager) SetProvideModesWithAckCallback(provideModes map[protocol.ProvideMode]bool, ackCallback func(err error)) {
	self.applyProvideModes(provideModes)
	if provideFrame, err := self.provideFrame(); err != nil {
		ackCallback(err)
	} else if provideFrame != nil {
		self.controlSyncProvide.Send(
			provideFrame,
			nil,
			ackCallback,
		)
	} else {
		ackCallback(nil)
	}
}

// SetProvideModesWithReturnTrafficWithOobAckCallback is like
// SetProvideModesWithReturnTrafficWithAckCallback, but registers the provide via
// the out-of-band control, so the ack means the platform has committed the
// provide secret (the in-band control ack only means the message was delivered).
// Use this when a caller must wait for the secret to be registered before using
// the client — e.g. the return path of a multi-client client, whose companion
// (Stream) contracts are verified against this secret.
func (self *ContractManager) SetProvideModesWithReturnTrafficWithOobAckCallback(provideModes map[protocol.ProvideMode]bool, ackCallback func(err error)) {
	updatedProvideModes := map[protocol.ProvideMode]bool{}
	maps.Copy(updatedProvideModes, provideModes)
	updatedProvideModes[protocol.ProvideMode_Stream] = true
	self.SetProvideModesWithOobAckCallback(updatedProvideModes, ackCallback)
}

func (self *ContractManager) SetProvideModesWithOobAckCallback(provideModes map[protocol.ProvideMode]bool, ackCallback func(err error)) {
	self.applyProvideModes(provideModes)
	if provideFrame, err := self.provideFrame(); err != nil {
		ackCallback(err)
	} else if provideFrame != nil {
		self.controlSyncProvideOob.Send(
			provideFrame,
			ackCallback,
		)
	} else {
		ackCallback(nil)
	}
}

func (self *ContractManager) GetProvideModes() map[protocol.ProvideMode]bool {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	return maps.Clone(self.provideModes)
}

func (self *ContractManager) Verify(storedContractHmac []byte, storedContractBytes []byte, provideMode protocol.ProvideMode) bool {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	// when paused, only allow ProvideMode_Stream for return traffic and
	// ProvideMode_Network for network peers (pause stops public/ff only)
	if self.providePaused && provideMode != protocol.ProvideMode_Stream && provideMode != protocol.ProvideMode_Network {
		return false
	}

	if !self.provideModes[provideMode] {
		return false
	}

	provideSecretKey, ok := self.provideSecretKeys[provideMode]
	if !ok {
		// provide mode is not enabled
		return false
	}

	return VerifyStoredContract(self.settings, provideSecretKey, storedContractBytes, storedContractHmac)
}

func (self *ContractManager) GetProvideSecretKey(provideMode protocol.ProvideMode) ([]byte, bool) {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	if !self.provideModes[provideMode] {
		return nil, false
	}

	provideSecretKey, ok := self.provideSecretKeys[provideMode]
	return provideSecretKey, ok
}

func (self *ContractManager) RequireProvideSecretKey(provideMode protocol.ProvideMode) []byte {
	secretKey, ok := self.GetProvideSecretKey(provideMode)
	if !ok {
		panic(fmt.Errorf("Missing provide secret for %s", provideMode))
	}
	return secretKey
}

func (self *ContractManager) AddNoContractPeer(clientId Id) {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	self.sendNoContractClientIds[clientId] = true
	self.receiveNoContractClientIds[clientId] = true
}

func (self *ContractManager) SendNoContract(destinationId Id) bool {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	if allow, ok := self.sendNoContractClientIds[destinationId]; ok {
		return allow
	}

	if !self.settings.ContractsEnabled() {
		return true
	}

	return false
}

func (self *ContractManager) ReceiveNoContract(sourceId Id) bool {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	if allow, ok := self.receiveNoContractClientIds[sourceId]; ok {
		return allow
	}

	if !self.settings.ContractsEnabled() {
		return true
	}

	return false
}

func (self *ContractManager) TakeContract(
	ctx context.Context,
	contractKey ContractKey,
	timeout time.Duration,
) *protocol.Contract {
	contractQueue := self.openContractQueue(contractKey)
	defer self.closeContractQueue(contractKey)

	enterTime := time.Now()
	for {
		notify := contractQueue.updateMonitor.NotifyChannel()
		var minEnqueueTime time.Time
		if 0 < self.settings.ContractQueueExpireTimeout {
			minEnqueueTime = time.Now().Add(-self.settings.ContractQueueExpireTimeout)
		}
		contract, expired := contractQueue.Poll(minEnqueueTime)
		if 0 < len(expired) {
			// stale queued contracts may already be force-closed server-side;
			// close them rather than handing them to a sequence
			self.closeContracts(expired)
		}

		if contract == nil && contractQueue.Drained() {
			// the queue was force-removed (e.g., the owning send sequence closed).
			// notifications now go to a fresh queue at this key; bail out instead
			// of waiting forever on this orphan's monitor.
			return nil
		}

		if contract != nil {
			storedContract := &protocol.StoredContract{}
			if err := ProtoUnmarshal(contract.StoredContractBytes, storedContract); err == nil {
				if contractId, err := IdFromBytes(storedContract.ContractId); err == nil {
					func() {
						self.mutex.Lock()
						defer self.mutex.Unlock()

						self.localStats.ContractOpenCount += 1
						self.localStats.ContractOpenByteCounts[contractId] = ByteCount(storedContract.TransferByteCount)
						self.localStats.ContractOpenKeys[contractId] = contractKey
					}()
				}
			}

			return contract
		}

		if timeout < 0 {
			select {
			case <-self.ctx.Done():
				return nil
			case <-ctx.Done():
				return nil
			case <-notify:
			}
		} else if timeout == 0 {
			return nil
		} else {
			remainingTimeout := enterTime.Add(timeout).Sub(time.Now())
			if remainingTimeout <= 0 {
				return nil
			}
			select {
			case <-self.ctx.Done():
				return nil
			case <-ctx.Done():
				return nil
			case <-notify:
			case <-time.After(remainingTimeout):
				return nil
			}
		}
	}
}

func (self *ContractManager) addContract(contractKey ContractKey, contract *protocol.Contract) error {
	storedContract := &protocol.StoredContract{}
	err := ProtoUnmarshal(contract.StoredContractBytes, storedContract)
	if err != nil {
		return err
	}

	sourceId, err := IdFromBytes(storedContract.SourceId)
	if err != nil {
		return fmt.Errorf("Contract source id malformed: %w", err)
	}
	if sourceId != self.client.ClientId() {
		return fmt.Errorf("Contract source must be this client: %s<>%s", sourceId, self.client.ClientId())
	}

	if self.client.log.V(1).Enabled() {
		self.client.log.Infof("[contract]add %s %s\n", self.client.ClientId(), contractKey.Destination)
	}

	func() {
		contractQueue := self.openContractQueue(contractKey)
		defer self.closeContractQueue(contractKey)
		contractQueue.Add(contract, storedContract)
	}()

	return nil
}

func (self *ContractManager) CreateContract(contractKey ContractKey, contractSeqIndex uint64, minByteCount ByteCount) {
	// look at destinationContracts and last contract to get previous contract id
	contractQueue := self.openContractQueue(contractKey)
	defer self.closeContractQueue(contractKey)

	streamVersion := uint32(DefaultStreamVersion)

	createContract := &protocol.CreateContract{
		DestinationId:     contractKey.Destination.DestinationId.Bytes(),
		IntermediaryIds:   contractKey.IntermediaryIds.Bytes(),
		TransferByteCount: uint64(self.contractByteCount(contractSeqIndex, minByteCount)),
		Companion:         contractKey.CompanionContract,
		ForceStream:       &contractKey.ForceStream,
		StreamVersion:     &streamVersion,
	}
	if self.settings.TrackUsedContracts {
		createContract.UsedContractIds = contractQueue.UsedContractIdBytes()
	}
	frame, err := ToFrame(createContract, self.settings.ProtocolVersion)
	if err != nil {
		self.client.log.Infof("[contract]could not create contract frame = %s", err)
		return
	}

	if self.client.log.V(1).Enabled() {
		self.client.log.Infof("[contract]create %s %s\n", self.client.ClientId(), contractKey.Destination)
	}

	self.client.ClientOob().SendControl(
		[]*protocol.Frame{frame},
		func(resultFrames []*protocol.Frame, err error) {
			if err == nil {
				for _, resultFrame := range resultFrames {
					self.HandleControlFrame(contractKey, resultFrame)
				}
			} else {
				select {
				case <-self.client.Done():
					// no need to log warnings when the client closes
				default:
					self.client.log.Infof("[contract]oob err = %s\n", err)
				}
			}
		},
	)
}

func (self *ContractManager) contractByteCount(contractSeqIndex uint64, minByteCount ByteCount) ByteCount {
	targetByteCount := func() ByteCount {
		if self.settings.ContractTransferByteSeqScale <= contractSeqIndex {
			return self.settings.StandardContractTransferByteCount
		} else {
			// lerp between initial and standard
			return self.settings.InitialContractTransferByteCount + ByteCount(
				(contractSeqIndex*uint64(self.settings.StandardContractTransferByteCount-self.settings.InitialContractTransferByteCount))/self.settings.ContractTransferByteSeqScale,
			)
		}
	}()
	return max(targetByteCount, minByteCount)
}

func (self *ContractManager) CheckpointContract(
	contractId Id,
	ackedByteCount ByteCount,
	unackedByteCount ByteCount,
) {
	self.CloseContractWithCheckpoint(contractId, ackedByteCount, unackedByteCount, true)
}

func (self *ContractManager) CloseContract(
	contractId Id,
	ackedByteCount ByteCount,
	unackedByteCount ByteCount,
) {
	self.CloseContractWithCheckpoint(contractId, ackedByteCount, unackedByteCount, false)
}

func (self *ContractManager) CloseContractWithCheckpoint(
	contractId Id,
	ackedByteCount ByteCount,
	unackedByteCount ByteCount,
	checkpoint bool,
) {
	// the sequence stops updating the contract at close/checkpoint,
	// so this is final for the stats entry either way
	self.closeContractStats(contractId)

	opened := false
	var contractKey ContractKey

	func() {
		self.mutex.Lock()
		defer self.mutex.Unlock()

		if _, ok := self.localStats.ContractOpenByteCounts[contractId]; ok {
			// opened via the contract manager
			opened = true
			contractKey = self.localStats.ContractOpenKeys[contractId]
			self.localStats.ContractCloseCount += 1
			delete(self.localStats.ContractOpenByteCounts, contractId)
			delete(self.localStats.ContractOpenKeys, contractId)
			self.localStats.ContractCloseByteCount += ackedByteCount
		} else {
			self.localStats.ReceiveContractCloseByteCount += ackedByteCount
		}
	}()

	// Reliable delivery via a per-contract `ControlSync`. The
	// previous implementation called `ClientOob().SendControl(...)`
	// once and dropped the result on the floor — a single transient
	// transport failure would leave the contract `open=true` on the
	// server, its escrow permanently deducted from the network
	// balance with no way for the client to ever signal completion.
	//
	// `ControlSync` retries the send until the platform acks (or
	// the client's context is canceled). One `ControlSync` per
	// close: each contract's close is independent and must not be
	// superseded by another close's `Send` (which is what would
	// happen on a shared `ControlSync` — its `syncCount` would
	// abandon the older close as "replaced"). The per-call instance
	// holds little state — a mutex, a monitor, and a derived context
	// — and its supervisor goroutine exits on success or when the
	// parent context closes, so there's no long-lived leak.
	frame, err := ToFrame(&protocol.CloseContract{
		ContractId:       contractId.Bytes(),
		AckedByteCount:   uint64(ackedByteCount),
		UnackedByteCount: uint64(unackedByteCount),
		Checkpoint:       checkpoint,
	}, self.settings.ProtocolVersion)
	if err != nil {
		self.client.log.Infof("[contract]could not create close contract frame = %s\n", err)
		return
	}

	if self.ctx.Err() != nil || self.client.IsDone() {
		// the client context is closed (the contract manager is closing).
		// note the manager ctx is the client's parent ctx, so check both.
		// `ControlSync` rides the in-band client transport, which is gone —
		// it would drop the close without a single attempt. Send a one-shot
		// cleanup over the out-of-band api on a Background context instead,
		// since the lifecycle context is closed. One shot, never retried, so
		// cleanup cannot run away; the server's expired-contract force-close
		// remains the backstop if the single attempt fails.
		sendCallback := func(resultFrames []*protocol.Frame, sendErr error) {
			if sendErr == nil {
				if self.client.log.V(1).Enabled() {
					self.client.log.Infof("[contract]closed %s after client close\n", contractId)
				}
			} else {
				self.client.log.Infof("[contract]could not close %s after client close = %s\n", contractId, sendErr)
			}
		}
		frames := []*protocol.Frame{frame}
		if clientOob, ok := self.client.ClientOob().(OutOfBandControlWithCtx); ok {
			clientOob.SendControlWithCtx(context.Background(), frames, sendCallback)
		} else {
			self.client.ClientOob().SendControl(frames, sendCallback)
		}
		return
	}

	closeControlSync := NewControlSync(self.ctx, self.client, fmt.Sprintf("close-contract-%s", contractId))
	closeControlSync.Send(frame, nil, func(sendErr error) {
		defer closeControlSync.Close()
		if sendErr == nil && opened {
			contractQueue := self.openContractQueue(contractKey)
			contractQueue.RemoveUsedContract(contractId)
			self.closeContractQueue(contractKey)
		}
	})
}

func (self *ContractManager) LocalStats() *ContractManagerStats {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	return &ContractManagerStats{
		ContractOpenCount:      self.localStats.ContractOpenCount,
		ContractCloseCount:     self.localStats.ContractCloseCount,
		ContractOpenByteCounts: maps.Clone(self.localStats.ContractOpenByteCounts),
		ContractOpenKeys:       maps.Clone(self.localStats.ContractOpenKeys),
		// ContractOpenDestinationIds: maps.Clone(self.localStats.ContractOpenDestinationIds),
		ContractCloseByteCount:        self.localStats.ContractCloseByteCount,
		ReceiveContractCloseByteCount: self.localStats.ReceiveContractCloseByteCount,
	}
}

func (self *ContractManager) ResetLocalStats() {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	self.localStats = NewContractManagerStats()
}

func (self *ContractManager) Flush(resetUsedContractIds bool) []Id {
	// close queued contracts
	contracts := func() []*protocol.Contract {
		self.mutex.Lock()
		defer self.mutex.Unlock()

		if self.client.log.V(1).Enabled() {
			self.client.log.Infof("[contract]flush %s %s\n", self.client.ClientId(), maps.Keys(self.destinationContracts))
		}

		contracts := []*protocol.Contract{}
		for contractKey, contractQueue := range self.destinationContracts {
			for _, contract := range contractQueue.Flush(resetUsedContractIds) {
				contracts = append(contracts, contract)
			}
			if contractQueue.IsDone() {
				delete(self.destinationContracts, contractKey)
			}
		}
		return contracts
	}()

	return self.closeContracts(contracts)
}

func (self *ContractManager) FlushContractQueue(contractKey ContractKey, resetUsedContractIds bool) []Id {
	contractQueue := self.openContractQueue(contractKey)
	defer self.closeContractQueueWithForceRemove(contractKey, true)

	contracts := contractQueue.Flush(resetUsedContractIds)

	return self.closeContracts(contracts)
}

func (self *ContractManager) closeContracts(contracts []*protocol.Contract) []Id {
	contractIds := []Id{}
	for _, contract := range contracts {
		storedContract := &protocol.StoredContract{}
		if err := ProtoUnmarshal(contract.StoredContractBytes, storedContract); err == nil {
			if contractId, err := IdFromBytes(storedContract.ContractId); err == nil {
				contractIds = append(contractIds, contractId)
				self.CloseContract(contractId, ByteCount(0), ByteCount(0))
			}
		}
	}
	return contractIds
}

func (self *ContractManager) openContractQueue(contractKey ContractKey) *contractQueue {
	if self.settings.LegacyCreateContract {
		contractKey = contractKey.Legacy()
	}

	self.mutex.Lock()
	defer self.mutex.Unlock()

	contractQueue, ok := self.destinationContracts[contractKey]
	if !ok {
		contractQueue = newContractQueue(self.client.log, self.settings.TrackUsedContracts)
		self.destinationContracts[contractKey] = contractQueue
	}
	contractQueue.Open()

	return contractQueue
}

func (self *ContractManager) closeContractQueue(contractKey ContractKey) {
	self.closeContractQueueWithForceRemove(contractKey, false)
}

func (self *ContractManager) closeContractQueueWithForceRemove(contractKey ContractKey, forceRemove bool) {
	if self.settings.LegacyCreateContract {
		contractKey = contractKey.Legacy()
	}

	var toDrain *contractQueue
	func() {
		self.mutex.Lock()
		defer self.mutex.Unlock()

		contractQueue, ok := self.destinationContracts[contractKey]
		if !ok {
			return
		}
		contractQueue.Close()
		if forceRemove {
			// remove from the map so future addContract creates a fresh queue.
			// existing waiters on the old monitor would never wake up
			// otherwise; drain wakes them so they can bail.
			delete(self.destinationContracts, contractKey)
			toDrain = contractQueue
		} else if contractQueue.IsDone() {
			delete(self.destinationContracts, contractKey)
		}
	}()
	if toDrain != nil {
		toDrain.Drain()
	}
}

// a contract waiting in the queue, stamped so unconsumed contracts can be
// expired (see `ContractQueueExpireTimeout`)
type queuedContract struct {
	contract    *protocol.Contract
	enqueueTime time.Time
}

type contractQueue struct {
	updateMonitor *Monitor
	log           Logger

	mutex     sync.Mutex
	openCount int
	contracts map[Id]*queuedContract
	drained   bool

	// remember all added contract ids
	trackUsedContracts bool
	usedContractIds    map[Id]bool
}

func newContractQueue(log Logger, trackUsedContracts bool) *contractQueue {
	return &contractQueue{
		updateMonitor:      NewMonitor(),
		log:                loggerOrDefault(log),
		openCount:          0,
		contracts:          map[Id]*queuedContract{},
		trackUsedContracts: trackUsedContracts,
		usedContractIds:    map[Id]bool{},
	}
}

func (self *contractQueue) Open() {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	self.openCount += 1
}

func (self *contractQueue) Close() {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	self.openCount -= 1
}

// Poll returns one queued contract, never one enqueued before
// `minEnqueueTime` — stale entries are removed and returned as `expired` for
// the caller to close (the platform force-closes unused contracts, so a stale
// queued contract may already be settled server-side). A zero `minEnqueueTime`
// expires nothing.
func (self *contractQueue) Poll(minEnqueueTime time.Time) (*protocol.Contract, []*protocol.Contract) {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	expired := self.expireWithLock(minEnqueueTime)

	// choose arbitrarily
	for contractId, queuedContract := range self.contracts {
		delete(self.contracts, contractId)
		return queuedContract.contract, expired
	}
	return nil, expired
}

// Expire removes and returns all contracts enqueued before `minEnqueueTime`.
func (self *contractQueue) Expire(minEnqueueTime time.Time) []*protocol.Contract {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	return self.expireWithLock(minEnqueueTime)
}

func (self *contractQueue) expireWithLock(minEnqueueTime time.Time) []*protocol.Contract {
	var expired []*protocol.Contract
	for contractId, queuedContract := range self.contracts {
		if queuedContract.enqueueTime.Before(minEnqueueTime) {
			expired = append(expired, queuedContract.contract)
			delete(self.contracts, contractId)
		}
	}
	return expired
}

func (self *contractQueue) Add(contract *protocol.Contract, storedContract *protocol.StoredContract) error {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	contractId, err := IdFromBytes(storedContract.ContractId)
	if err != nil {
		return err
	}

	// update contract if present
	if _, ok := self.contracts[contractId]; ok {
		if self.log.V(2).Enabled() {
			self.log.Infof("[contract]add update existing %s\n", contractId)
		}
		self.contracts[contractId] = &queuedContract{
			contract:    contract,
			enqueueTime: time.Now(),
		}
		self.updateMonitor.NotifyAll()
	} else if !self.trackUsedContracts || !self.usedContractIds[contractId] {
		if self.log.V(2).Enabled() {
			self.log.Infof("[contract]add %s\n", contractId)
		}
		if self.trackUsedContracts {
			self.usedContractIds[contractId] = true
		}
		self.contracts[contractId] = &queuedContract{
			contract:    contract,
			enqueueTime: time.Now(),
		}
		self.updateMonitor.NotifyAll()
	} else {
		if self.log.V(2).Enabled() {
			self.log.Infof("[contract]add already used %s\n", contractId)
		}
		// drop this contract. it has already been used locally
	}
	return nil
}

func (self *contractQueue) RemoveUsedContract(contractId Id) {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	delete(self.usedContractIds, contractId)
}

func (self *contractQueue) Flush(removeUsedContractIds bool) []*protocol.Contract {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	contracts := []*protocol.Contract{}
	for _, queuedContract := range self.contracts {
		contracts = append(contracts, queuedContract.contract)
	}
	self.contracts = map[Id]*queuedContract{}
	if removeUsedContractIds {
		self.usedContractIds = map[Id]bool{}
	}

	return contracts
}

// Drain marks the queue as no longer accepting new contracts and wakes any
// waiters so they can exit cleanly. Used when the queue is being force-removed
// from the manager while waiters still hold references.
func (self *contractQueue) Drain() {
	func() {
		self.mutex.Lock()
		defer self.mutex.Unlock()
		self.drained = true
	}()
	self.updateMonitor.NotifyAll()
}

func (self *contractQueue) Drained() bool {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return self.drained
}

func (self *contractQueue) IsDone() bool {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	if 0 < self.openCount {
		return false
	}

	return 0 == len(self.contracts) && 0 == len(self.usedContractIds)
}

func (self *contractQueue) UsedContractIdBytes() [][]byte {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	usedContractIdBytes := [][]byte{}
	for contractId, _ := range self.usedContractIds {
		usedContractIdBytes = append(usedContractIdBytes, contractId.Bytes())
	}
	return usedContractIdBytes
}
