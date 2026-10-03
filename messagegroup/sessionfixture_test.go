// The fixture every session, seal and engine case is built on: a REAL connect/mls group behind
// the real adapter, and nothing standing in for a key.
//
// THAT IS THE POINT AND IT IS THE MILESTONE'S. CP3b's bar is "every key real, no test-only key
// source anywhere on the path", and a stub GroupHandle answering a made up exporter output would
// satisfy every case in this package while leaving that bar exactly as far away as it was before
// task 9a. So the fixture builds an mls.CryptoProvider, an mls.StateStore, a signature key pair
// and an X-Wing leaf key, founds a group through NewConnectMlsEngine, and hands the session the
// adapter over it. Every KEY on that path is the real one.
//
// FOUR VALUES HERE ARE THE TEST'S AND NOT THE PRODUCT'S, named rather than left for a reader to
// find: pq_secret, which NewPqSecret draws and which the session takes as a required argument with
// no default -- absent rather than defaulted, which is the discipline this project's own rule
// states; the state store, which is a map and persists nothing; the clock, which is a constant
// because this package has no timing sensitive test and must not gain one; and the server nonce,
// which the submitting connection chooses and there is no connection.
//
// AND ONE OF THE FOUR IS A KEY. This paragraph used to end "none of the four is a KEY", and that
// is wrong about pq_secret and only about pq_secret: StorageRoot takes it as the IKM of every
// storage_root this session extracts, so it is key material on the seal and open path by the only
// definition that matters. Three things are true at once and the sentence has to carry all three.
// It is a KEY VALUE the test supplies; NewPqSecret is a PRODUCTION function and is what draws the
// real one, so it is not a test-only key SOURCE -- which is the distinction CP3b's bar draws and
// the reason the owner's 2026-09-10 ruling reads "key SOURCE" literally; and NOTHING IN PRODUCTION
// CALLS NewPqSecret, anywhere in this package, so this value has no production driver at all. That
// last clause is the one worth carrying forward: its delivery is m1 task 14, gated on ledger item
// 152, which is an owner ruling. Do not build a driver for it here.
//
// The other three are not keys and the reasons differ: a store holds key material and is not any,
// a clock is a number, and the server nonce is a mac input spec A hands to the server in the
// clear.
//
// The state store is in memory and is test-only by construction: it is declared in a _test.go
// file, so no production build of this package can reach it, and imports_test.go's pin over the
// production import set is what keeps a durable one from arriving here instead.
package messagegroup

import (
	"bytes"
	"fmt"
	"maps"
	"slices"
	"sync"
	"testing"

	"github.com/urnetwork/connect/message"
	"github.com/urnetwork/connect/mls"
)

// memoryStateStore is mls.StateStore in a map. It persists nothing and it is not meant to: what
// a case here needs is a store that answers what it was given within one process.
type memoryStateStore struct {
	lock        sync.Mutex
	groupStates map[string][]byte
	privateKeys map[string][]byte
	keyPackages map[string][3][]byte
}

func newMemoryStateStore() *memoryStateStore {
	return &memoryStateStore{
		groupStates: map[string][]byte{},
		privateKeys: map[string][]byte{},
		keyPackages: map[string][3][]byte{},
	}
}

func (self *memoryStateStore) PutGroupState(groupId []byte, epoch uint64, state []byte) error {
	self.lock.Lock()
	defer self.lock.Unlock()
	self.groupStates[fmt.Sprintf("%x/%d", groupId, epoch)] = append([]byte(nil), state...)
	return nil
}

func (self *memoryStateStore) GetGroupState(groupId []byte, epoch uint64) ([]byte, error) {
	self.lock.Lock()
	defer self.lock.Unlock()
	state, isHeld := self.groupStates[fmt.Sprintf("%x/%d", groupId, epoch)]
	if !isHeld {
		return nil, fmt.Errorf("no group state for %x at epoch %d", groupId, epoch)
	}
	return append([]byte(nil), state...), nil
}

func (self *memoryStateStore) DeleteGroupStateBefore(groupId []byte, epoch uint64) error {
	self.lock.Lock()
	defer self.lock.Unlock()
	for at := uint64(0); at < epoch; at += 1 {
		delete(self.groupStates, fmt.Sprintf("%x/%d", groupId, at))
	}
	return nil
}

func (self *memoryStateStore) PutPrivateKey(pub []byte, priv []byte) error {
	self.lock.Lock()
	defer self.lock.Unlock()
	self.privateKeys[fmt.Sprintf("%x", pub)] = append([]byte(nil), priv...)
	return nil
}

func (self *memoryStateStore) GetPrivateKey(pub []byte) ([]byte, error) {
	self.lock.Lock()
	defer self.lock.Unlock()
	priv, isHeld := self.privateKeys[fmt.Sprintf("%x", pub)]
	if !isHeld {
		return nil, fmt.Errorf("no private key for %x", pub)
	}
	return append([]byte(nil), priv...), nil
}

func (self *memoryStateStore) DeletePrivateKey(pub []byte) error {
	self.lock.Lock()
	defer self.lock.Unlock()
	delete(self.privateKeys, fmt.Sprintf("%x", pub))
	return nil
}

func (self *memoryStateStore) PutKeyPackage(ref []byte, kp []byte, initPriv []byte, encPriv []byte) error {
	self.lock.Lock()
	defer self.lock.Unlock()
	self.keyPackages[fmt.Sprintf("%x", ref)] = [3][]byte{
		append([]byte(nil), kp...), append([]byte(nil), initPriv...), append([]byte(nil), encPriv...),
	}
	return nil
}

func (self *memoryStateStore) TakeKeyPackage(ref []byte) ([]byte, []byte, []byte, error) {
	self.lock.Lock()
	defer self.lock.Unlock()
	held, isHeld := self.keyPackages[fmt.Sprintf("%x", ref)]
	if !isHeld {
		return nil, nil, nil, fmt.Errorf("no key package for %x", ref)
	}
	delete(self.keyPackages, fmt.Sprintf("%x", ref))
	return held[0], held[1], held[2], nil
}

// testEngine is one device: its provider, its store, its identity and the engine over them.
type testEngine struct {
	engine      GroupEngine
	crypto      mls.CryptoProvider
	store       *memoryStateStore
	identityPub []byte
	leafKeys    []byte
	// THE PUBLIC HALF OF THE KEY THIS DEVICE SIGNS WITH, which is a different value from
	// identityPub after this fixture's own repair below. Without it a gate asserting "the leaf
	// names the device signer" has nothing to compare the leaf against: testEngine retained the
	// credential identity and neither the signer nor its public half.
	signerPub []byte
	// THE PRIVATE HALF, COPIED AT CONSTRUCTION AND BEFORE ANY DOOR OF THE ENGINE HAS BEEN DRIVEN.
	// It is the CONTROL for the clone coupling pin in joincoupling_test.go and it is a copy rather
	// than the engine's own array on purpose: NewConnectMlsEngine clones what it is handed, so this
	// field and the engine's field are two arrays, and a pin comparing them is comparing the array
	// under test against a value taken before anything could have erased it. A field that aliased
	// the engine's would read zero exactly when the engine's did and would prove nothing.
	signer []byte
	// the store the engine was actually built over, which is memoryStateStore for every fixture
	// but the ones that hand it an observation instrument.
	outerStore mls.StateStore
}

// newTestEngine builds one device's engine, with a real signature key pair and a real X-Wing
// public half in its leaf keys extension.
func newTestEngine(t *testing.T) *testEngine {
	t.Helper()
	engine, err := buildTestEngine()
	if err != nil {
		t.Fatalf("build the engine: %v", err)
	}
	return engine
}

// buildTestEngine is newTestEngine without a *testing.T, because the one way ladder probes in
// recordkey_test.go are plain functions and still have to run on the real thing.
func buildTestEngine() (*testEngine, error) {
	memory := newMemoryStateStore()
	return buildTestEngineOver(memory, memory)
}

// buildTestEngineOver is buildTestEngine with the store chosen by the caller, so that a gate can
// put an observation instrument where the engine's store goes without replacing the one every
// other case runs on. memory is the same store unless the instrument wraps one.
func buildTestEngineOver(store mls.StateStore, memory *memoryStateStore) (*testEngine, error) {
	return buildTestEngineWrapped(store, memory, nil)
}

// buildTestEngineWrapped is buildTestEngineOver with the PROVIDER chosen by the caller too, so a
// gate can put an observation instrument where the engine's crypto goes. wrap is nil for every
// fixture but the one that observes the join's erase through an alias.
//
// The provider is wrapped AFTER the key pairs and the X-Wing seed are drawn, so the instrument
// observes only what the engine does with it and not what this fixture did.
func buildTestEngineWrapped(store mls.StateStore, memory *memoryStateStore,
	wrap func(mls.CryptoProvider) mls.CryptoProvider) (*testEngine, error) {

	crypto, err := mls.NewCryptoProvider(mls.CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		return nil, err
	}
	signer, signerPub, err := crypto.SignatureKeyPair()
	if err != nil {
		return nil, err
	}
	// A SECOND, INDEPENDENT DRAW FOR THE CREDENTIAL IDENTITY, and it is the smaller half of j1
	// task 4 that matters most. This fixture used to draw signer and identityPub from ONE
	// SignatureKeyPair call and pass mls.BasicCredential(identityPub) -- so the device's credential
	// identity WAS its signer's public half, and under that fixture the assertion "the leaf names
	// the device signer" and the assertion "the leaf names the credential" are the same program. A
	// gate written over it cannot fail for the reason task 4 exists.
	//
	// Blast radius, measured: identityPub appears on 14 lines of this package's tests, and the only
	// one that compares the two is engine_test.go's MemberAt(0) case, which reads
	// Credential.Identity and stays true.
	_, identityPub, err := crypto.SignatureKeyPair()
	if err != nil {
		return nil, err
	}
	xwingPrivate, err := XwingGenerateKey(bytes.NewReader(crypto.Random(XwingSeedSize)))
	if err != nil {
		return nil, err
	}
	leafKeys, err := (&mls.LeafKeysExtension{
		AlgId:          mls.AlgIdXwing,
		DeviceXwingPub: xwingPrivate.Public().Bytes(),
	}).Encode()
	if err != nil {
		return nil, err
	}
	if wrap != nil {
		crypto = wrap(crypto)
	}
	engine, err := NewConnectMlsEngine(crypto, store, signer,
		mls.BasicCredential(identityPub), leafKeys.ExtensionData)
	if err != nil {
		return nil, err
	}
	return &testEngine{
		engine:      engine,
		crypto:      crypto,
		store:       memory,
		identityPub: append([]byte(nil), identityPub...),
		leafKeys:    leafKeys.ExtensionData,
		signerPub:   append([]byte(nil), signerPub...),
		signer:      append([]byte(nil), signer...),
		outerStore:  store,
	}, nil
}

// ---------------------------------------------------------------------------
// the two observation instruments j1 task 4 builds, and neither is a store
// ---------------------------------------------------------------------------

// storeCall is one call this device's engine made into its store: the method, and a COPY of every
// byte argument it was handed, in order.
//
// The arity half of "what reaches the store is unchanged" reads this and not the map.
// memoryStateStore holds two maps and records nothing, so a body that persisted a FIFTH value
// through a second store method leaves a keyPackages entry that still looks exactly right.
type storeCall struct {
	method string
	args   [][]byte
}

// recordingAliasStore is mls.StateStore as an OBSERVATION INSTRUMENT and not as a store.
//
// It does two things memoryStateStore does not, and each answers a half of a property no other
// route reaches:
//
//   - PutKeyPackage RETAINS the caller's slice headers rather than copying them. That is the only
//     route to "the two HPKE private halves are erased before NewKeyPackage returns": measured,
//     memoryStateStore's own entry is byte-identical under a correct body, under a body that
//     erases NEITHER and under one that erases only the init half, because it copies at call time
//     and nothing the engine does afterwards changes one octet of it. An erase is observable only
//     through an ALIAS of the array erased, and wherever the far side copies, the property must
//     build the alias or it is measuring a photograph.
//   - it records every call, so "and NOTHING ELSE" is a question that can be asked at all.
//
// IT IS NOT A STORE AND MUST NOT BECOME ONE. A production store that aliased a caller's array is
// exactly the defect the erase discipline forbids, and memoryStateStore must keep copying: the
// join's put-back is a statement about the store's OWN arrays.
//
// The methods are written out rather than promoted from an embedded mls.StateStore on purpose: a
// method added to that interface would arrive here already implemented, recording nothing, and
// quietly narrowing what every gate reading this can see.
type recordingAliasStore struct {
	inner     *memoryStateStore
	calls     []storeCall
	initAlias []byte
	encAlias  []byte
	// when set, TakeKeyPackage answers it instead of reading. StateStore.TakeKeyPackage returns
	// a BARE error with no declared not-found value, so a broken disk and a ref this store never
	// held are one answer to a caller matching on the type; this is how a gate drives the first
	// of the two.
	failTake error
}

var _ mls.StateStore = (*recordingAliasStore)(nil)

func newRecordingAliasStore() *recordingAliasStore {
	return &recordingAliasStore{inner: newMemoryStateStore()}
}

func (self *recordingAliasStore) recordStoreCall(method string, args ...[]byte) {
	copies := [][]byte{}
	for _, argument := range args {
		copies = append(copies, append([]byte(nil), argument...))
	}
	self.calls = append(self.calls, storeCall{method: method, args: copies})
}

// callsTo answers every call this store took of one method, in order.
func (self *recordingAliasStore) callsTo(method string) []storeCall {
	found := []storeCall{}
	for _, call := range self.calls {
		if call.method == method {
			found = append(found, call)
		}
	}
	return found
}

// methodsCalled answers the distinct method names this store was driven through, sorted.
func (self *recordingAliasStore) methodsCalled() []string {
	seen := map[string]bool{}
	for _, call := range self.calls {
		seen[call.method] = true
	}
	names := slices.Sorted(maps.Keys(seen))
	return names
}

func (self *recordingAliasStore) PutGroupState(groupId []byte, epoch uint64, state []byte) error {
	self.recordStoreCall("PutGroupState", groupId, state)
	return self.inner.PutGroupState(groupId, epoch, state)
}

func (self *recordingAliasStore) GetGroupState(groupId []byte, epoch uint64) ([]byte, error) {
	self.recordStoreCall("GetGroupState", groupId)
	return self.inner.GetGroupState(groupId, epoch)
}

func (self *recordingAliasStore) DeleteGroupStateBefore(groupId []byte, epoch uint64) error {
	self.recordStoreCall("DeleteGroupStateBefore", groupId)
	return self.inner.DeleteGroupStateBefore(groupId, epoch)
}

func (self *recordingAliasStore) PutPrivateKey(pub []byte, priv []byte) error {
	self.recordStoreCall("PutPrivateKey", pub, priv)
	return self.inner.PutPrivateKey(pub, priv)
}

func (self *recordingAliasStore) GetPrivateKey(pub []byte) ([]byte, error) {
	self.recordStoreCall("GetPrivateKey", pub)
	return self.inner.GetPrivateKey(pub)
}

func (self *recordingAliasStore) DeletePrivateKey(pub []byte) error {
	self.recordStoreCall("DeletePrivateKey", pub)
	return self.inner.DeletePrivateKey(pub)
}

func (self *recordingAliasStore) PutKeyPackage(ref []byte, kp []byte, initPriv []byte, encPriv []byte) error {
	self.recordStoreCall("PutKeyPackage", ref, kp, initPriv, encPriv)
	// THE SLICE HEADERS, not clones. This is the instrument.
	self.initAlias = initPriv
	self.encAlias = encPriv
	return self.inner.PutKeyPackage(ref, kp, initPriv, encPriv)
}

func (self *recordingAliasStore) TakeKeyPackage(ref []byte) ([]byte, []byte, []byte, error) {
	self.recordStoreCall("TakeKeyPackage", ref)
	if self.failTake != nil {
		return nil, nil, nil, self.failTake
	}
	return self.inner.TakeKeyPackage(ref)
}

// newRecordingEngine is one device whose engine writes into the instrument above.
func newRecordingEngine(t *testing.T) (*testEngine, *recordingAliasStore) {
	t.Helper()
	store := newRecordingAliasStore()
	engine, err := buildTestEngineOver(store, store.inner)
	if err != nil {
		t.Fatalf("build the engine this gate observes: %v", err)
	}
	return engine, store
}

// newEraseObservingEngine is one device whose engine writes into BOTH instruments: the store that
// records its calls and the provider that retains the private key array the join hands to HPKE.
//
// The two are needed together and neither is redundant. The store answers where the material came
// from -- the arrays TakeKeyPackage handed back, which the join must NOT erase and must put back
// byte for byte. The provider answers whether the copies the join assembled over them were erased
// at all, which is the half no route through a store can reach.
func newEraseObservingEngine(t *testing.T) (*testEngine, *recordingAliasStore, *aliasingCryptoProvider) {
	t.Helper()
	store := newRecordingAliasStore()
	observed := &aliasingCryptoProvider{}
	engine, err := buildTestEngineWrapped(store, store.inner, func(inner mls.CryptoProvider) mls.CryptoProvider {
		observed.inner = inner
		return observed
	})
	if err != nil {
		t.Fatalf("build the engine this gate observes: %v", err)
	}
	return engine, store, observed
}

// createGroup founds a group whose id is thirty two octets, which is the width a record header
// carries.
func (self *testEngine) createGroup(t *testing.T, name string) GroupHandle {
	t.Helper()
	handle, err := self.buildGroup(name)
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	return handle
}

// buildGroup is createGroup without a *testing.T, for the probes.
func (self *testEngine) buildGroup(name string) (GroupHandle, error) {
	policy := &mls.GroupPolicyExtension{
		Roles: []mls.RoleEntry{{MemberId: self.identityPub, Role: mls.RoleOwner}},
	}
	if err := policy.Canonicalize(); err != nil {
		return nil, err
	}
	encoded, err := policy.Encode()
	if err != nil {
		return nil, err
	}
	return self.engine.CreateGroup(testGroupId(name), encoded.ExtensionData, self.leafKeys)
}

// buildProbeSession founds a fresh group and opens a session over it with the caller's secret as
// pq_secret, which is the one secret the constructor takes and does not derive.
//
// It exists for the one way ladder probes, which are handed a rung of the record key ladder and
// have to answer everything the member under test produces from it.
func buildProbeSession(pqSecret []byte) (*testSession, error) {
	engine, err := buildTestEngine()
	if err != nil {
		return nil, err
	}
	handle, err := engine.buildGroup("probe")
	if err != nil {
		return nil, err
	}
	reserver := newStreamIndexMemory()
	session, err := NewGroupSession(handle, pqSecret, nil, reserver, testClock(), testServerNonce())
	if err != nil {
		return nil, err
	}
	return &testSession{session: session, handle: handle, engine: engine, reserver: reserver}, nil
}

// buildProbePair is buildProbeSession for the cases that need TWO members of ONE group and cannot
// call t.Fatalf, which after MASTER section 8.4 is every case that has to OPEN an application
// record: a member has no receiving ratchet for its own leaf, so one session can never open a
// record it sealed.
//
// It is deliberately the minimum: found, add, commit, merge, join, and one session at each end. The
// three controls newTwoEngineChainAtClock carries -- the ones that caught a zeroed group_handle_key
// walking through a green round trip -- are NOT reproduced here, because a control that cannot fail
// a test is decoration. Anything asserting a property of the fixture itself uses that chain; this
// answers the one-way probes, whose subject is the ladder and not the join.
func buildProbePair(pqSecret []byte) (sender *GroupSession, opener *GroupSession,
	senderLeaf uint32, release func(), err error) {

	a, err := buildTestEngine()
	if err != nil {
		return nil, nil, 0, nil, err
	}
	b, err := buildTestEngine()
	if err != nil {
		return nil, nil, 0, nil, err
	}
	keyPackage, err := b.engine.NewKeyPackage()
	if err != nil {
		return nil, nil, 0, nil, err
	}
	founder, err := a.buildGroup("probe-pair")
	if err != nil {
		return nil, nil, 0, nil, err
	}
	// group_handle_key is the EPOCH ZERO storage root's expansion and never moves, so it is taken
	// here -- before the add commits -- and handed to both sessions. A session founded after the
	// commit would expand a later root and compute a sender_handle nobody else reproduces.
	mlsSecret, err := founder.Export(engineJoinExporterLabel, nil, engineJoinExporterLength)
	if err != nil {
		return nil, nil, 0, nil, err
	}
	groupHandleKey := GroupHandleKey(StorageRoot(mlsSecret, pqSecret))
	if _, err := founder.ProposeAdd(keyPackage); err != nil {
		return nil, nil, 0, nil, err
	}
	_, welcome, ratchetTree, err := founder.Commit(nil)
	if err != nil {
		return nil, nil, 0, nil, err
	}
	if err := founder.MergePendingCommit(); err != nil {
		return nil, nil, 0, nil, err
	}
	joined, err := b.engine.JoinFromWelcome(welcome, ratchetTree)
	if err != nil {
		return nil, nil, 0, nil, err
	}
	senderSession, err := NewGroupSession(founder, pqSecret, groupHandleKey, newStreamIndexMemory(),
		testClock(), testServerNonce())
	if err != nil {
		return nil, nil, 0, nil, err
	}
	openerSession, err := NewGroupSession(joined, pqSecret, groupHandleKey, newStreamIndexMemory(),
		testClock(), testServerNonce())
	if err != nil {
		senderSession.Close()
		return nil, nil, 0, nil, err
	}
	release = func() {
		openerSession.Close()
		senderSession.Close()
	}
	return senderSession, openerSession, founder.OwnLeafIndex(), release, nil
}

// testGroupId is one distinct thirty two octet group id per name.
func testGroupId(name string) []byte {
	groupId := make([]byte, 32)
	copy(groupId, name)
	return groupId
}

// The pq_secret every fixture supplies. NewPqSecret draws the real one; this is a constant so that a
// case comparing two sessions is comparing the two sessions rather than two draws.
//
// It is NOT a default and it is not reachable from production: the constructor refuses an empty
// pq_secret and takes no value of its own, so nothing in a shipped build can arrive at this
// value or at any other by accident.
func testPqSecret() []byte {
	secret := make([]byte, 32)
	for i := range secret {
		secret[i] = byte(0xA0 + i)
	}
	return secret
}

// testServerNonce is the submitting connection's nonce a fixture macs write_auth under.
func testServerNonce() []byte {
	return []byte("test-server-nonce")
}

// testClock is the injected clock: a fixed millisecond value, because this package has no
// timing sensitive test and must not gain one.
func testClock() func() int64 {
	return func() int64 { return 1_700_000_000_000 }
}

// testSession is one member's session over a real group, with the reserver it writes into.
type testSession struct {
	session  *GroupSession
	handle   GroupHandle
	engine   *testEngine
	reserver *streamIndexFake
}

// newTestSession founds a group and opens a session over it.
func newTestSession(t *testing.T, name string) *testSession {
	t.Helper()
	return newTestSessionAtClock(t, name, testClock())
}

// newTestSessionAtClock is newTestSession with the injected clock supplied by the caller.
//
// IT IS NOT A TIMING SENSITIVE SEAM AND IT MUST NOT BECOME ONE. doc.go's inventory says this
// package has no timing sensitive test in it and takes the time as an injected nowMs func() int64
// for exactly that reason; a caller that hands in a closure over a variable it sets itself is
// still fully deterministic, which is the difference between "the clock moved" and "the test
// slept". What it buys is the one property a fixed clock cannot observe at all: that an opener
// takes the record's own eph_window off the wire and never recomputes one, which is only visible
// when the two would differ.
func newTestSessionAtClock(t *testing.T, name string, nowMs func() int64) *testSession {
	t.Helper()
	engine := newTestEngine(t)
	handle := engine.createGroup(t, name)
	reserver := newStreamIndexMemory()
	session, err := NewGroupSession(handle, testPqSecret(), nil, reserver, nowMs, testServerNonce())
	if err != nil {
		t.Fatalf("NewGroupSession: %v", err)
	}
	t.Cleanup(func() { session.Close() })
	return &testSession{session: session, handle: handle, engine: engine, reserver: reserver}
}

// trackOwn installs a receiver ratchet for this session's OWN durable ladder, which is what lets
// a case seal and open in one session.
func (self *testSession) trackOwn(t *testing.T) {
	t.Helper()
	self.trackOwnLadder(t, message.RetentionDurable, 0, 0)
}

// trackOwnLadder is trackOwn for any class key: the class, the bucket and the WINDOW, because an
// EPH ladder is rooted at EphKey(eph_root, bucket, window) and two windows of one bucket are two
// ladders.
func (self *testSession) trackOwnLadder(t *testing.T, class message.RetentionClass, bucket uint8,
	window uint64) {

	t.Helper()
	if err := self.session.TrackSender(self.handle.OwnLeafIndex(), class, bucket, window, 0); err != nil {
		t.Fatalf("TrackSender(class %d bucket %d window %d): %v", class, bucket, window, err)
	}
}

// testEphRoot is the eph_root a fixture installs.
//
// IT IS A FIXED VALUE AND NOT A DRAW, deliberately, for the reason testPqSecret is: a fixture
// that drew one would make every case that compares two derivations depend on entropy. It is a
// KEY VALUE the test supplies and NewEphRoot is the production function that draws the real one,
// so this is not a test-only key SOURCE -- the same distinction the pq_secret paragraph in this
// file's header draws, and eph_root has the same standing: no production caller of NewEphRoot
// exists in this package, and its carrier is the eph_root device wrap of m1 task 14.
func testEphRoot() []byte {
	root := make([]byte, EphRootBytes)
	for i := range root {
		root[i] = byte(0xE0 + i)
	}
	return root
}

// installEphRoot hands this session the epoch's eph_root, which is what any EPH record needs
// before it can be sealed or opened at all.
func (self *testSession) installEphRoot(t *testing.T) {
	t.Helper()
	if err := self.session.InstallEphRoot(testEphRoot()); err != nil {
		t.Fatalf("InstallEphRoot: %v", err)
	}
}

// ephWindowNow is the window this fixture's clock falls in for one bucket, computed the way the
// sealer computes it rather than written out, so a case that wants "the current window" and a
// sealer that wrote one cannot disagree.
func ephWindowNow(t *testing.T, bucket uint8) uint64 {
	t.Helper()
	window, err := EphWindowAt(bucket, testClock()())
	if err != nil {
		t.Fatalf("EphWindowAt(bucket %d): %v", bucket, err)
	}
	return window
}

// ---------------------------------------------------------------------------
// the erase observation instrument: a provider, and it is not a provider
// ---------------------------------------------------------------------------

// aliasingCryptoProvider is mls.CryptoProvider as an OBSERVATION INSTRUMENT and not as a provider,
// and it is recordingAliasStore's rule applied one seam over.
//
// THE RULE, quoted from that type because this one exists for exactly it: "an erase is observable
// only through an ALIAS of the array erased, and wherever the far side copies, the property must
// build the alias or it is measuring a photograph."
//
// THE ARRAY IN QUESTION IS A LOCAL OF A PRODUCTION METHOD AND NO RUNTIME ROUTE FROM THIS PACKAGE
// REACHES IT. joinWithTakenKeyPackage assembles mls.JoinKeyMaterial over four copies it made --
// that is the whole point of the helper -- so the store's arrays are the wrong arrays, the
// engine's own signer is the wrong array, and the joined handle holds clones connect/mls made.
// There is ONE seam through which the material's own array crosses back into a type this package
// controls: mls.JoinFromWelcome opens the welcome secret with OpenWithLabel(crypto,
// keys.InitPrivate, ...), which reaches crypto.HpkeOpen(priv, ...) with the slice passed
// STRAIGHT THROUGH -- no clone at either hop. A provider that RETAINS that slice header holds an
// alias of the exact array (*mls.JoinKeyMaterial).Zeroize is obliged to erase.
//
// WHAT IT BUYS, measured rather than argued: a deferred call named Zeroize that erases NOTHING --
// spelled as a no-op method of a decoy type, so the source read at the bottom of
// TestTheDeviceSurvivesItsOwnJoin sees exactly one deferred erase and no plain one -- left the
// whole of ./mls/... ./message/... ./messagegroup/... green at 7,692 passing, 0 failing. Through
// this instrument that mutant is red.
//
// IT IS NOT A PROVIDER AND MUST NOT BECOME ONE. A production provider that retained a caller's
// private key array is the defect the erase discipline exists to forbid.
//
// The methods are written out rather than promoted from an embedded mls.CryptoProvider for
// recordingAliasStore's reason: a method added to that interface would arrive here already
// implemented, recording nothing, and quietly narrowing what every gate reading this can see.
type aliasingCryptoProvider struct {
	inner mls.CryptoProvider
	// one entry per HpkeOpen, in order: the slice HEADER the caller handed over, and a COPY of
	// what it held at call time. The copy is the control -- "it reads all zero afterwards" is
	// satisfied by an array that was all zero to begin with, and by an empty one.
	hpkeOpenPriv       [][]byte
	hpkeOpenPrivAtCall [][]byte
}

var _ mls.CryptoProvider = (*aliasingCryptoProvider)(nil)

func (self *aliasingCryptoProvider) Suite() mls.CipherSuite { return self.inner.Suite() }
func (self *aliasingCryptoProvider) HashSize() int          { return self.inner.HashSize() }
func (self *aliasingCryptoProvider) KeySize() int           { return self.inner.KeySize() }
func (self *aliasingCryptoProvider) NonceSize() int         { return self.inner.NonceSize() }

func (self *aliasingCryptoProvider) Hash(data []byte) []byte { return self.inner.Hash(data) }

func (self *aliasingCryptoProvider) Mac(key []byte, data []byte) []byte {
	return self.inner.Mac(key, data)
}

func (self *aliasingCryptoProvider) MacVerify(key []byte, data []byte, tag []byte) bool {
	return self.inner.MacVerify(key, data, tag)
}

func (self *aliasingCryptoProvider) Extract(salt []byte, ikm []byte) []byte {
	return self.inner.Extract(salt, ikm)
}

func (self *aliasingCryptoProvider) Expand(prk []byte, info []byte, length int) []byte {
	return self.inner.Expand(prk, info, length)
}

func (self *aliasingCryptoProvider) ExpandWithLabel(secret []byte, label string, context []byte,
	length int) []byte {

	return self.inner.ExpandWithLabel(secret, label, context, length)
}

func (self *aliasingCryptoProvider) DeriveSecret(secret []byte, label string) []byte {
	return self.inner.DeriveSecret(secret, label)
}

func (self *aliasingCryptoProvider) DeriveTreeSecret(secret []byte, label string, generation uint32,
	length int) []byte {

	return self.inner.DeriveTreeSecret(secret, label, generation, length)
}

func (self *aliasingCryptoProvider) AeadSeal(key []byte, nonce []byte, aad []byte,
	plaintext []byte) ([]byte, error) {

	return self.inner.AeadSeal(key, nonce, aad, plaintext)
}

func (self *aliasingCryptoProvider) AeadOpen(key []byte, nonce []byte, aad []byte,
	ciphertext []byte) ([]byte, error) {

	return self.inner.AeadOpen(key, nonce, aad, ciphertext)
}

func (self *aliasingCryptoProvider) SignWithLabel(priv mls.SignaturePrivateKey, label string,
	content []byte) ([]byte, error) {

	return self.inner.SignWithLabel(priv, label, content)
}

func (self *aliasingCryptoProvider) VerifyWithLabel(pub mls.SignaturePublicKey, label string,
	content []byte, sig []byte) error {

	return self.inner.VerifyWithLabel(pub, label, content, sig)
}

func (self *aliasingCryptoProvider) HpkeSeal(pub mls.HpkePublicKey, info []byte, aad []byte,
	plaintext []byte) ([]byte, []byte, error) {

	return self.inner.HpkeSeal(pub, info, aad, plaintext)
}

// HpkeOpen RETAINS the caller's private key slice header. This is the instrument.
func (self *aliasingCryptoProvider) HpkeOpen(priv mls.HpkePrivateKey, kemOutput []byte, info []byte,
	aad []byte, ciphertext []byte) ([]byte, error) {

	self.hpkeOpenPriv = append(self.hpkeOpenPriv, priv)
	self.hpkeOpenPrivAtCall = append(self.hpkeOpenPrivAtCall, append([]byte(nil), priv...))
	return self.inner.HpkeOpen(priv, kemOutput, info, aad, ciphertext)
}

func (self *aliasingCryptoProvider) DeriveKeyPair(ikm []byte) (mls.HpkePrivateKey, mls.HpkePublicKey, error) {
	return self.inner.DeriveKeyPair(ikm)
}

func (self *aliasingCryptoProvider) SignatureKeyPair() (mls.SignaturePrivateKey, mls.SignaturePublicKey, error) {
	return self.inner.SignatureKeyPair()
}

func (self *aliasingCryptoProvider) Random(n int) []byte { return self.inner.Random(n) }

// ---------------------------------------------------------------------------
// two members of one group, MASTER section 8.4's fixture
// ---------------------------------------------------------------------------

// testPair is TWO MEMBERS OF ONE REAL MLS GROUP, and after 2026-09-15 it is the only shape in
// which a record round trips at all.
//
// WHY IT HAD TO EXIST. MASTER section 8.4 makes an application record's ct_body an MLS
// PrivateMessage, Protect consumes a generation of the sealer's OWN sending ratchet, and MLS
// derives no receiving ratchet for a member's own leaf -- a member never receives its own
// messages. So a session cannot open a record it sealed, and every case in this package that
// sealed and opened through one newTestSession was asserting a round trip that the ruling ends.
// Open item MG-4 carries the unruled half; this type is what the cases move onto.
//
// IT IS newTwoEngineChain AND NOT A SECOND CHAIN. That fixture already founds a group, adds a
// second device from a real key package, commits, joins from the Welcome and -- the part that
// makes it worth reusing rather than re-deriving -- holds three controls over the group handle key
// it hands both sessions, including the one a zeroed key walked through. A second assembly of the
// same shape would be a second place for those controls to be missing.
//
// THE TWO SESSIONS ARE AT EPOCH 1 and not at epoch 0, because an add is a commit. A case that
// needs epoch zero -- keysource_test.go's reproduction is the one -- cannot use this and says so.
type testPair struct {
	chain *twoEngineChain
	// the member that seals. Its leaf is the one an opener tracks.
	sender *GroupSession
	// the member that opens. It is a DIFFERENT device with a DIFFERENT signing key, which is
	// what makes the inner frame's signature a real check rather than a round trip with itself.
	opener     *GroupSession
	senderLeaf uint32
	openerLeaf uint32
}

// newTestPair founds the group, adds the second member and answers both sessions.
func newTestPair(t *testing.T, name string) *testPair {
	t.Helper()
	return newTestPairAtClock(t, name, testClock())
}

// newTestPairAtClock is newTestPair with the injected clock supplied by the caller, and both
// sessions take the same one. See newTwoEngineChainAtClock's header for why that is not a timing
// sensitive seam.
func newTestPairAtClock(t *testing.T, name string, nowMs func() int64) *testPair {
	t.Helper()
	chain := newTwoEngineChainAtClock(t, name, nowMs)
	t.Cleanup(chain.close)
	return &testPair{
		chain:      chain,
		sender:     chain.founderSession,
		opener:     chain.joinerSession,
		senderLeaf: chain.founder.OwnLeafIndex(),
		openerLeaf: chain.joined.OwnLeafIndex(),
	}
}

// track installs the opener's receiver ratchet over the sender's ladder for one class key.
//
// The WINDOW is a parameter for the reason trackOwnLadder's is: an EPH ladder is rooted at
// EphKey(eph_root, bucket, window) and two windows of one bucket are two ladders.
func (self *testPair) track(t *testing.T, class message.RetentionClass, bucket uint8, window uint64) {
	t.Helper()
	if err := self.opener.TrackSender(self.senderLeaf, class, bucket, window, 0); err != nil {
		t.Fatalf("the opener's TrackSender(class %d bucket %d window %d): %v", class, bucket, window, err)
	}
}

// trackDurable is track over the class every round trip in this package uses.
func (self *testPair) trackDurable(t *testing.T) {
	t.Helper()
	self.track(t, message.RetentionDurable, 0, 0)
}

// installEphRoot hands BOTH sessions the epoch's eph_root, which is what any EPH record needs
// before either end can touch it. One side alone is a record the other cannot key.
func (self *testPair) installEphRoot(t *testing.T) {
	t.Helper()
	for who, session := range map[string]*GroupSession{"the sender": self.sender, "the opener": self.opener} {
		if err := session.InstallEphRoot(testEphRoot()); err != nil {
			t.Fatalf("InstallEphRoot at %s: %v", who, err)
		}
	}
}
