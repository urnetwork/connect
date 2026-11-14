package connect

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/v2026/protocol"
)

// The accepted seed state of each network space (EXTENDER-UPDATE2.md §2.2).
//
// For each normalized network host the client keeps the greatest sequence it
// has accepted, the digest of that bundle's body, and the bundle itself. The
// three are one value and every host's value is one document, written by one
// `Save`: a store never holds a sequence without the bundle that carried it, so
// a restart can never keep the higher sequence while losing the usable bundle.
// The sequence is the replay floor: a lower sequence is a rollback, a different
// body at the same sequence a conflict, and the identical bundle again changes
// nothing.
//
// A floor binds only while its bundle's envelope signature verifies under the
// root keys in force. The floor outlives the bundle's freshness -- an expired
// bundle still holds it -- and it continues across a key rotation the client
// followed, since the bundle it holds was then signed by a key that is still
// authorized. A floor whose bundle no longer verifies was set by authority the
// space no longer grants: a key a root update removed, or a root a settings
// import installed and a later update replaced. Such a floor is void and is
// dropped with its bundle, so a bundle signed by a removed or hostile root can
// never hold the real space's floor above its genuine sequence. A dropped floor
// leaves the space where a fresh install starts: replay bounded by the validity
// window and the configured minimum sequence.

// The byte backend of the accepted seed state, the same shape as
// ExtenderDirectoryStore. `Load` returns what the last successful `Save` wrote,
// or nothing (nil or empty, and no error) when nothing was saved. `Save` must
// replace the whole value atomically -- after a crash at any point the next
// `Load` returns either the previous value or the new one in full -- which a
// file backend gets from a temporary file, a sync and a rename
// (ExtenderSeedFileStore).
type ExtenderSeedStore interface {
	Load() ([]byte, error)
	Save([]byte) error
}

// The state of one network space as the acceptor holds it. NetworkHost is
// normalized (NormalizeExtenderNetworkHost); BodyDigest is the sha256 of the
// envelope's body bytes; Envelope is the serialized envelope that was accepted.
type ExtenderSeedState struct {
	NetworkHost string
	Sequence    uint64
	BodyDigest  []byte
	Envelope    []byte
	AcceptTime  time.Time
}

// Checks that a state is one value: its envelope parses, its body's sequence,
// digest and network host are the state's own, and the host is normalized.
// The envelope signature is not checked here; that is the void rule above,
// judged under the keys of each use.
func CheckExtenderSeedState(state *ExtenderSeedState) error {
	if state == nil {
		return fmt.Errorf("extender seed state is missing")
	}
	networkHost := NormalizeExtenderNetworkHost(state.NetworkHost)
	if networkHost == "" || networkHost != state.NetworkHost {
		return fmt.Errorf("extender seed state network host is not normalized")
	}
	envelope, err := ParseExtenderSeedEnvelope(state.Envelope)
	if err != nil {
		return err
	}
	body := &protocol.ExtenderSeedBody{}
	if err := proto.Unmarshal(envelope.Body, body); err != nil {
		return fmt.Errorf("extender seed state body does not decode: %w", err)
	}
	if !bytes.Equal(ExtenderSeedBodyDigest(envelope.Body), state.BodyDigest) {
		return fmt.Errorf("extender seed state digest does not match its bundle")
	}
	if body.Sequence != state.Sequence {
		return fmt.Errorf("extender seed state sequence does not match its bundle")
	}
	if NormalizeExtenderNetworkHost(body.NetworkHost) != networkHost {
		return fmt.Errorf("extender seed state network host does not match its bundle")
	}
	return nil
}

func cloneExtenderSeedState(state *ExtenderSeedState) *ExtenderSeedState {
	if state == nil {
		return nil
	}
	return &ExtenderSeedState{
		NetworkHost: state.NetworkHost,
		Sequence:    state.Sequence,
		BodyDigest:  slices.Clone(state.BodyDigest),
		Envelope:    slices.Clone(state.Envelope),
		AcceptTime:  state.AcceptTime,
	}
}

// Version of the stored document. A document of another version, or one that
// does not decode, is no history at all -- what a fresh install has -- and is
// replaced by the next accept. An attacker who could write the store could as
// well delete it, which is the same state.
const extenderSeedStoreVersion = 1

// The most network spaces the document keeps, the least recently accepted
// evicted first. A client accepts only its space's hosts (the space and its
// migration host); the cap bounds a client that moves between many.
const ExtenderSeedMaxStoredNetworkCount = 8

// The most the document can hold: every space's envelope in base64 and the
// json around it. A larger document is no history, like a corrupt one.
const extenderSeedStoreMaxByteCount = ExtenderSeedMaxStoredNetworkCount * (2*ExtenderSeedMaxEnvelopeByteCount + 1024)

type extenderSeedStoreDocument struct {
	Version  int                         `json:"version"`
	Networks []*extenderSeedStoreNetwork `json:"networks"`
}

type extenderSeedStoreNetwork struct {
	NetworkHost  string `json:"network_host"`
	Sequence     uint64 `json:"sequence"`
	BodyDigest   string `json:"body_digest"`
	Envelope     []byte `json:"envelope"`
	AcceptTimeMs int64  `json:"accept_time_ms,omitempty"`
}

// Decodes the stored document into states by normalized host. A document that
// cannot be read as this version is no history; a single entry that is not one
// consistent value (CheckExtenderSeedState) is dropped alone.
func decodeExtenderSeedStoreDocument(documentBytes []byte) (map[string]*ExtenderSeedState, bool) {
	states := map[string]*ExtenderSeedState{}
	if len(documentBytes) == 0 {
		return states, true
	}
	if extenderSeedStoreMaxByteCount < len(documentBytes) {
		return states, false
	}
	document := &extenderSeedStoreDocument{}
	if err := json.Unmarshal(documentBytes, document); err != nil {
		return states, false
	}
	if document.Version != extenderSeedStoreVersion {
		return states, false
	}
	consistent := true
	for _, network := range document.Networks {
		if network == nil {
			consistent = false
			continue
		}
		bodyDigest, err := hex.DecodeString(network.BodyDigest)
		if err != nil {
			consistent = false
			continue
		}
		state := &ExtenderSeedState{
			NetworkHost: network.NetworkHost,
			Sequence:    network.Sequence,
			BodyDigest:  bodyDigest,
			Envelope:    network.Envelope,
			AcceptTime:  extenderTimeFromMs(network.AcceptTimeMs),
		}
		if err := CheckExtenderSeedState(state); err != nil {
			consistent = false
			continue
		}
		if _, ok := states[state.NetworkHost]; ok {
			// a host twice is not a document this release writes
			consistent = false
			continue
		}
		states[state.NetworkHost] = state
	}
	return states, consistent
}

// Encodes states as the document, ordered by host so equal states are equal
// bytes.
func encodeExtenderSeedStoreDocument(states map[string]*ExtenderSeedState) ([]byte, error) {
	document := &extenderSeedStoreDocument{
		Version:  extenderSeedStoreVersion,
		Networks: []*extenderSeedStoreNetwork{},
	}
	for _, state := range states {
		document.Networks = append(document.Networks, &extenderSeedStoreNetwork{
			NetworkHost:  state.NetworkHost,
			Sequence:     state.Sequence,
			BodyDigest:   hex.EncodeToString(state.BodyDigest),
			Envelope:     state.Envelope,
			AcceptTimeMs: extenderTimeMs(state.AcceptTime),
		})
	}
	slices.SortFunc(document.Networks, func(a *extenderSeedStoreNetwork, b *extenderSeedStoreNetwork) int {
		return strings.Compare(a.NetworkHost, b.NetworkHost)
	})
	return json.Marshal(document)
}

// An in-memory backend, for a host with no storage and for tests. Safe for
// concurrent use.
type ExtenderSeedMemoryStore struct {
	stateLock sync.Mutex
	value     []byte
}

func NewExtenderSeedMemoryStore() *ExtenderSeedMemoryStore {
	return &ExtenderSeedMemoryStore{}
}

func (self *ExtenderSeedMemoryStore) Load() ([]byte, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.value), nil
}

func (self *ExtenderSeedMemoryStore) Save(value []byte) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.value = slices.Clone(value)
	return nil
}

// A file backend. A save writes a temporary file beside the target, syncs it
// and renames it over the target, then syncs the directory, so the file is
// always a whole value, the previous one or the new one. A load of a file that
// does not exist is no value. Safe for concurrent use within one process; two
// processes must not share a path.
type ExtenderSeedFileStore struct {
	path string

	stateLock sync.Mutex
	// test seam only, nil otherwise: runs after the temporary file is synced
	// and before the rename, which is where a crash keeps the previous value
	beforeRenameHook func() error
}

func NewExtenderSeedFileStore(path string) *ExtenderSeedFileStore {
	return &ExtenderSeedFileStore{
		path: path,
	}
}

func (self *ExtenderSeedFileStore) Load() ([]byte, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	file, err := os.Open(self.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer file.Close()
	// a file past the document cap is read only far enough to know it is
	value, err := io.ReadAll(io.LimitReader(file, extenderSeedStoreMaxByteCount+1))
	if err != nil {
		return nil, err
	}
	return value, nil
}

func (self *ExtenderSeedFileStore) Save(value []byte) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	dirPath := filepath.Dir(self.path)
	if err := os.MkdirAll(dirPath, 0o700); err != nil {
		return err
	}
	tempPattern := "." + filepath.Base(self.path) + "-*.tmp"
	// a crash between a write and its rename leaves a temporary file behind;
	// it is never read, and the next save removes it
	if leftoverPaths, err := filepath.Glob(filepath.Join(dirPath, tempPattern)); err == nil {
		for _, leftoverPath := range leftoverPaths {
			os.Remove(leftoverPath)
		}
	}
	tempFile, err := os.CreateTemp(dirPath, tempPattern)
	if err != nil {
		return err
	}
	tempPath := tempFile.Name()
	keepTemp := false
	defer func() {
		if !keepTemp {
			os.Remove(tempPath)
		}
	}()
	if _, err := tempFile.Write(value); err != nil {
		tempFile.Close()
		return err
	}
	if err := tempFile.Sync(); err != nil {
		tempFile.Close()
		return err
	}
	if err := tempFile.Close(); err != nil {
		return err
	}
	if self.beforeRenameHook != nil {
		if err := self.beforeRenameHook(); err != nil {
			// a simulated crash keeps its temporary file, as a real one would
			keepTemp = true
			return err
		}
	}
	if err := os.Rename(tempPath, self.path); err != nil {
		return err
	}
	keepTemp = true
	// the rename is durable once the directory is; a platform that cannot
	// sync a directory keeps what the rename gives it
	if dir, err := os.Open(dirPath); err == nil {
		dir.Sync()
		dir.Close()
	}
	return nil
}

type ExtenderSeedAcceptorSettings struct {
	// The lowest sequence accepted for any space, from trusted application
	// configuration (§2.3): with no sequence history -- a fresh install, or a
	// floor dropped as void -- it is the replay floor beside the validity
	// window. Zero accepts any.
	MinSequence uint64
	// The clock of the accept times the eviction orders by. Nil is time.Now.
	Now func() time.Time
}

func DefaultExtenderSeedAcceptorSettings() *ExtenderSeedAcceptorSettings {
	return &ExtenderSeedAcceptorSettings{
		Now: time.Now,
	}
}

// Accepts verified bundles into a store under the sequence rules of §2.2, and
// reads the accepted bundles back. Every operation loads the document, so the
// store's owner may clear it at any time (Reset), and operations are
// serialized, so a load, a compare and a save are one step. One acceptor
// serves one store, and one store serves one network space, whose hosts share
// its root keys: every operation judges every state the document holds under
// the keys it is given (the void rule above), so a store shared with a space
// of other roots would void that space's floors.
type ExtenderSeedAcceptor struct {
	store    ExtenderSeedStore
	settings *ExtenderSeedAcceptorSettings

	stateLock sync.Mutex
}

func NewExtenderSeedAcceptor(store ExtenderSeedStore, settings *ExtenderSeedAcceptorSettings) *ExtenderSeedAcceptor {
	if store == nil {
		store = NewExtenderSeedMemoryStore()
	}
	if settings == nil {
		settings = DefaultExtenderSeedAcceptorSettings()
	}
	if settings.Now == nil {
		copied := *settings
		copied.Now = time.Now
		settings = &copied
	}
	return &ExtenderSeedAcceptor{
		store:    store,
		settings: settings,
	}
}

// The states of the store, with every void state (above) removed under
// `rootKeys`. `changed` is whether what was loaded differs from what is
// returned -- a void or inconsistent entry dropped, or a document that was not
// readable -- so a caller that saves anyway writes the cleaned document. An
// empty key set is the unconfigured state of a space (SetRootKeys) and judges
// nothing: no floor is dropped for want of keys.
func (self *ExtenderSeedAcceptor) loadWithLock(rootKeys *ExtenderRootKeySet) (map[string]*ExtenderSeedState, bool, error) {
	documentBytes, err := self.store.Load()
	if err != nil {
		return nil, false, newExtenderSeedError(ExtenderSeedCodeStore, err, "the accepted state does not load")
	}
	states, consistent := decodeExtenderSeedStoreDocument(documentBytes)
	changed := !consistent
	if rootKeys == nil || rootKeys.Len() == 0 {
		return states, changed, nil
	}
	for networkHost, state := range states {
		if !extenderSeedStateBinds(state, rootKeys) {
			delete(states, networkHost)
			changed = true
		}
	}
	return states, changed, nil
}

func (self *ExtenderSeedAcceptor) saveWithLock(states map[string]*ExtenderSeedState) error {
	documentBytes, err := encodeExtenderSeedStoreDocument(states)
	if err != nil {
		return newExtenderSeedError(ExtenderSeedCodeStore, err, "the accepted state does not encode")
	}
	if err := self.store.Save(documentBytes); err != nil {
		return newExtenderSeedError(ExtenderSeedCodeStore, err, "the accepted state was not saved")
	}
	return nil
}

// Whether a state's floor binds under a non-empty `rootKeys`: its envelope
// signature verifies under a key in force. Freshness plays no part.
func extenderSeedStateBinds(state *ExtenderSeedState, rootKeys *ExtenderRootKeySet) bool {
	envelope, err := ParseExtenderSeedEnvelope(state.Envelope)
	if err != nil {
		return false
	}
	_, err = rootKeys.Verify(ExtenderSeedSignatureDomain, envelope.Body, envelope.Signature, envelope.KeyId)
	return err == nil
}

// Accepts one bundle that VerifyExtenderSeed returned under `rootKeys`, which
// must be the keys it was verified under. It returns ExtenderSeedOutcomeAccepted
// when the bundle is newer than the space's binding floor and was saved, and
// ExtenderSeedOutcomeIdentical when it is the accepted bundle again, which
// changes nothing. Otherwise it refuses with a typed error: `rollback` below the
// floor or the configured minimum, `conflict` for a different body at the
// floor, `store` when the document cannot be loaded or saved. Nothing is
// written on a refusal, so the last valid state stays intact; a document the
// backend cannot load is a refusal too, never an empty history, so the floor
// fails closed against a transient read failure.
func (self *ExtenderSeedAcceptor) Accept(bundle *ExtenderSeedBundle, rootKeys *ExtenderRootKeySet) (string, error) {
	if bundle == nil || bundle.Envelope == nil {
		return "", newExtenderSeedError(ExtenderSeedCodeMalformed, nil, "the bundle is missing")
	}
	if rootKeys == nil || rootKeys.Len() == 0 {
		return "", newExtenderSeedError(ExtenderSeedCodeNoRootKeys, nil, "")
	}
	// the bundle is judged as the value it claims to be, so a hand-built
	// bundle cannot commit a floor its envelope does not carry
	if _, err := rootKeys.Verify(
		ExtenderSeedSignatureDomain,
		bundle.Envelope.Body,
		bundle.Envelope.Signature,
		bundle.Envelope.KeyId,
	); err != nil {
		return "", newExtenderSeedError(ExtenderSeedCodeSignature, err, "")
	}
	envelopeBytes, err := MarshalExtenderSeedEnvelope(bundle.Envelope)
	if err != nil {
		return "", err
	}
	state := &ExtenderSeedState{
		NetworkHost: bundle.NetworkHost,
		Sequence:    bundle.Sequence,
		BodyDigest:  slices.Clone(bundle.BodyDigest),
		Envelope:    envelopeBytes,
		AcceptTime:  self.settings.Now(),
	}
	if err := CheckExtenderSeedState(state); err != nil {
		return "", newExtenderSeedError(ExtenderSeedCodeMalformed, err, "the bundle is not one consistent value")
	}
	if bundle.Sequence < self.settings.MinSequence {
		return "", newExtenderSeedError(
			ExtenderSeedCodeRollback,
			nil,
			"sequence %d is below the configured minimum %d",
			bundle.Sequence,
			self.settings.MinSequence,
		)
	}

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	states, _, err := self.loadWithLock(rootKeys)
	if err != nil {
		return "", err
	}
	if acceptedState := states[state.NetworkHost]; acceptedState != nil {
		switch {
		case state.Sequence < acceptedState.Sequence:
			return "", newExtenderSeedError(
				ExtenderSeedCodeRollback,
				nil,
				"sequence %d is below the accepted %d",
				state.Sequence,
				acceptedState.Sequence,
			)
		case state.Sequence == acceptedState.Sequence:
			if bytes.Equal(state.BodyDigest, acceptedState.BodyDigest) {
				return ExtenderSeedOutcomeIdentical, nil
			}
			return "", newExtenderSeedError(
				ExtenderSeedCodeConflict,
				nil,
				"a different body at the accepted sequence %d",
				acceptedState.Sequence,
			)
		}
	}
	states[state.NetworkHost] = state
	for ExtenderSeedMaxStoredNetworkCount < len(states) {
		// the least recently accepted other space goes first
		evictNetworkHost := ""
		var evictState *ExtenderSeedState
		for networkHost, otherState := range states {
			if networkHost == state.NetworkHost {
				continue
			}
			if evictState == nil ||
				otherState.AcceptTime.Before(evictState.AcceptTime) ||
				(otherState.AcceptTime.Equal(evictState.AcceptTime) && networkHost < evictNetworkHost) {
				evictNetworkHost = networkHost
				evictState = otherState
			}
		}
		delete(states, evictNetworkHost)
	}
	if err := self.saveWithLock(states); err != nil {
		return "", err
	}
	return ExtenderSeedOutcomeAccepted, nil
}

// The binding floor of one space under `rootKeys`: the accepted sequence, or 0
// when the space has none or its floor is void.
func (self *ExtenderSeedAcceptor) Sequence(networkHost string, rootKeys *ExtenderRootKeySet) (uint64, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	states, _, err := self.loadWithLock(rootKeys)
	if err != nil {
		return 0, err
	}
	if state := states[NormalizeExtenderNetworkHost(networkHost)]; state != nil {
		return state.Sequence, nil
	}
	return 0, nil
}

// The accepted bundle of one space, verified again under the keys and hosts in
// force now and judged fresh at `now`: nil and no error when the space has
// none. A floor that is void under `rootKeys` is dropped and saved, and the
// space has no bundle. A bundle that verifies but is no longer fresh, or no
// longer passes some other check, is refused with the typed error a new bundle
// would get, while its floor stays.
func (self *ExtenderSeedAcceptor) Stored(
	networkHost string,
	rootKeys *ExtenderRootKeySet,
	networkHosts []string,
	now time.Time,
) (*ExtenderSeedBundle, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	states, changed, err := self.loadWithLock(rootKeys)
	if err != nil {
		return nil, err
	}
	if changed {
		if err := self.saveWithLock(states); err != nil {
			return nil, err
		}
	}
	state := states[NormalizeExtenderNetworkHost(networkHost)]
	if state == nil {
		return nil, nil
	}
	envelope, err := ParseExtenderSeedEnvelope(state.Envelope)
	if err != nil {
		return nil, err
	}
	return VerifyExtenderSeed(envelope, rootKeys, networkHosts, now)
}

// Judges every space's floor under `rootKeys` and drops and saves those that
// are void (above), returning how many were dropped. The owner calls it when
// the root keys change; every other operation applies the same rule to what it
// reads, so a missed call never lets a void floor bind. An empty key set judges
// nothing and drops nothing.
func (self *ExtenderSeedAcceptor) Revalidate(rootKeys *ExtenderRootKeySet) (int, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	documentBytes, err := self.store.Load()
	if err != nil {
		return 0, newExtenderSeedError(ExtenderSeedCodeStore, err, "the accepted state does not load")
	}
	states, consistent := decodeExtenderSeedStoreDocument(documentBytes)
	droppedCount := 0
	if rootKeys != nil && 0 < rootKeys.Len() {
		for networkHost, state := range states {
			if !extenderSeedStateBinds(state, rootKeys) {
				delete(states, networkHost)
				droppedCount += 1
			}
		}
	}
	if 0 < droppedCount || !consistent {
		if err := self.saveWithLock(states); err != nil {
			return 0, err
		}
	}
	return droppedCount, nil
}

// Clears every space's state, floor included: the reset of the extender
// section, which returns a client to what a fresh install has.
func (self *ExtenderSeedAcceptor) Reset() error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.saveWithLock(map[string]*ExtenderSeedState{})
}
