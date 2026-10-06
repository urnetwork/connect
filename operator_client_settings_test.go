package connect

// Tests of the operator-backed client settings (operator_client_settings.go).
//
// The implementation was the sdk's private newDeviceClientSettings, and its
// tests move with it: the install and preserve tests of the public key fetcher,
// and the partial override half of the completion test. The rest pin what the
// configuration is for: the fetchers it installs read the operator api's two key
// routes, and a session built from it reaches the api for each verification it
// performs.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestOperatorClientSettingsInstallsPeerKeyFetcherWithoutMutatingInput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clientStrategy := NewClientStrategy(ctx, DefaultClientStrategySettings())
	defer clientStrategy.Close()
	settings := DefaultClientSettings()
	originalEncryptionSettings := settings.EncryptionSettings

	clientSettings := NewOperatorClientSettings(settings, "https://api.example", clientStrategy)

	AssertNotEqual(t, clientSettings, settings)
	AssertNotEqual(t, clientSettings.EncryptionSettings, originalEncryptionSettings)
	AssertEqual(t, settings.EncryptionSettings, originalEncryptionSettings)
	AssertEqual(t, settings.EncryptionSettings.NewPeerClientPublicKeyFetcher == nil, true)
	AssertEqual(t, clientSettings.EncryptionSettings.NewPeerClientPublicKeyFetcher != nil, true)
	// the signed history resolver is installed the same way
	AssertEqual(t, settings.EncryptionSettings.NewPeerClientKeyHistoryFetcher == nil, true)
	AssertEqual(t, clientSettings.EncryptionSettings.NewPeerClientKeyHistoryFetcher != nil, true)
	// the asserts above compare values; the result must not alias the caller either
	if clientSettings == settings ||
		clientSettings.EncryptionSettings == originalEncryptionSettings ||
		settings.EncryptionSettings != originalEncryptionSettings {
		t.Fatal("the result aliases the caller's settings")
	}
}

func TestOperatorClientSettingsPreservesConfiguredPeerKeyFetcher(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clientStrategy := NewClientStrategy(ctx, DefaultClientStrategySettings())
	defer clientStrategy.Close()
	settings := DefaultClientSettings()
	settings.EncryptionSettings.NewPeerClientPublicKeyFetcher = func(peerId Id) func(context.Context) ([]byte, error) {
		return func(context.Context) ([]byte, error) {
			return peerId.Bytes(), nil
		}
	}

	clientSettings := NewOperatorClientSettings(settings, "https://api.example", clientStrategy)
	clientId := NewId()
	publicKey, err := clientSettings.EncryptionSettings.NewPeerClientPublicKeyFetcher(clientId)(ctx)

	AssertEqual(t, err, nil)
	AssertEqual(t, publicKey, clientId.Bytes())
}

func TestOperatorClientSettingsPreservesConfiguredPeerKeyHistoryFetcher(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clientStrategy := NewClientStrategy(ctx, DefaultClientStrategySettings())
	defer clientStrategy.Close()
	settings := DefaultClientSettings()
	settings.EncryptionSettings.NewPeerClientKeyHistoryFetcher = func(peerId Id) func(context.Context) ([][]byte, error) {
		return func(context.Context) ([][]byte, error) {
			return [][]byte{peerId.Bytes()}, nil
		}
	}

	clientSettings := NewOperatorClientSettings(settings, "https://api.example", clientStrategy)
	clientId := NewId()
	history, err := clientSettings.EncryptionSettings.NewPeerClientKeyHistoryFetcher(clientId)(ctx)

	AssertEqual(t, err, nil)
	AssertEqual(t, history, [][]byte{clientId.Bytes()})
	// each fetcher is decided on its own: the unset one is still installed
	AssertEqual(t, clientSettings.EncryptionSettings.NewPeerClientPublicKeyFetcher != nil, true)
}

// A partial override is completed without writing to the caller, the nested
// settings the caller supplied are copied rather than aliased, and only the
// pieces memory sizing reads are completed here.
func TestOperatorClientSettingsCompletesPartialSettingsWithoutMutatingInput(t *testing.T) {
	partial := &ClientSettings{
		ForwardBufferSettings:   DefaultForwardBufferSettings(),
		ContractManagerSettings: DefaultContractManagerSettings(),
	}
	originalForward := partial.ForwardBufferSettings
	originalContract := partial.ContractManagerSettings
	originalForwardSequenceBufferSize := originalForward.SequenceBufferSize
	originalContractSequenceBufferSize := originalContract.SequenceBufferSize
	settings := NewOperatorClientSettings(partial, "", nil)
	if partial.SendBufferSettings != nil ||
		partial.ReceiveBufferSettings != nil ||
		partial.WebRtcSettings != nil {
		t.Fatal("completing partial settings mutated the caller")
	}
	if settings.ForwardBufferSettings == nil ||
		settings.ContractManagerSettings == nil ||
		settings.ForwardBufferSettings == originalForward ||
		settings.ContractManagerSettings == originalContract {
		t.Fatal("nested ownership settings were not copied")
	}
	settings.ForwardBufferSettings.SequenceBufferSize += 1
	settings.ContractManagerSettings.SequenceBufferSize += 1
	if partial.ForwardBufferSettings.SequenceBufferSize != originalForwardSequenceBufferSize ||
		partial.ContractManagerSettings.SequenceBufferSize != originalContractSequenceBufferSize {
		t.Fatal("derived settings mutated caller-owned nested settings")
	}

	defaults := DefaultClientSettings()
	empty := NewOperatorClientSettings(&ClientSettings{}, "", nil)
	AssertEqual(t, empty.SendBufferSettings, defaults.SendBufferSettings)
	AssertEqual(t, empty.ReceiveBufferSettings, defaults.ReceiveBufferSettings)
	AssertEqual(t, empty.WebRtcSettings, defaults.WebRtcSettings)
	// the rest are left as the caller left them; with no encryption settings
	// there is nothing to install a fetcher on
	if empty.ForwardBufferSettings != nil ||
		empty.ContractManagerSettings != nil ||
		empty.StreamManagerSettings != nil ||
		empty.PeerManagerSettings != nil ||
		empty.EncryptionSettings != nil {
		t.Fatal("a nested setting the caller left nil was filled here")
	}
}

// Every reference a ClientSettings carries is either copied, so the result can
// be written without reaching the caller, or carried over as the caller's, and
// every value the caller supplied is kept. A reference field added to
// ClientSettings fails here until it is given a disposition below.
func TestOperatorClientSettingsCopiesExactlyTheNestedSettingsItOwns(t *testing.T) {
	ownedFields := map[string]bool{
		"EncryptionSettings":      true,
		"SendBufferSettings":      true,
		"ReceiveBufferSettings":   true,
		"ForwardBufferSettings":   true,
		"ContractManagerSettings": true,
		"WebRtcSettings":          true,
	}
	sharedFields := map[string]bool{
		"MemoryOwnerLedger":     true,
		"PayloadOwnerLedger":    true,
		"StreamManagerSettings": true,
		"PeerManagerSettings":   true,
		"Log":                   true,
		"ClientKeySeed":         true,
	}
	isReference := func(kind reflect.Kind) bool {
		switch kind {
		case reflect.Pointer, reflect.Slice, reflect.Map, reflect.Interface, reflect.Func, reflect.Chan:
			return true
		default:
			return false
		}
	}

	// every reference is non-nil and every scalar differs from its default, so a
	// field the factory rebuilt instead of carrying over cannot match by chance
	settings := DefaultClientSettings()
	settings.Log = NewNoopLogger()
	settings.ClientKeySeed = bytes.Repeat([]byte{7}, ed25519.SeedSize)
	settings.DefaultTransferOpts = TransferOptions{
		Ack:               false,
		CompanionContract: true,
		ForceStream:       true,
		NetworkPeer:       true,
	}
	settingsValue := reflect.ValueOf(settings).Elem()
	settingsType := settingsValue.Type()
	for i := 0; i < settingsType.NumField(); i += 1 {
		field := settingsType.Field(i)
		if !field.IsExported() {
			continue
		}
		value := settingsValue.Field(i)
		switch kind := field.Type.Kind(); kind {
		case reflect.Bool:
			value.SetBool(!value.Bool())
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			value.SetInt(value.Int() + 7)
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			value.SetUint(value.Uint() + 7)
		case reflect.String:
			value.SetString(value.String() + "-supplied")
		default:
			if isReference(kind) && value.IsNil() {
				if kind != reflect.Pointer {
					t.Fatalf("no non-nil fixture for %s (%s)", field.Name, field.Type)
				}
				value.Set(reflect.New(field.Type.Elem()))
			}
		}
	}

	result := NewOperatorClientSettings(settings, "https://api.example", nil)

	resultValue := reflect.ValueOf(result).Elem()
	for i := 0; i < settingsType.NumField(); i += 1 {
		field := settingsType.Field(i)
		if !field.IsExported() {
			continue
		}
		supplied := settingsValue.Field(i)
		got := resultValue.Field(i)
		if !isReference(field.Type.Kind()) {
			if !reflect.DeepEqual(got.Interface(), supplied.Interface()) {
				t.Errorf("%s = %v, want the supplied %v", field.Name, got.Interface(), supplied.Interface())
			}
			continue
		}
		switch {
		case ownedFields[field.Name] && sharedFields[field.Name]:
			t.Errorf("%s is listed as both copied and shared", field.Name)
		case ownedFields[field.Name]:
			if field.Type.Kind() != reflect.Pointer {
				t.Errorf("%s (%s) is not a struct pointer and cannot be copied", field.Name, field.Type)
				continue
			}
			if got.IsNil() {
				t.Errorf("%s was dropped instead of copied", field.Name)
				continue
			}
			if got.Pointer() == supplied.Pointer() {
				t.Errorf("%s is the caller's: a write to the result reaches the caller", field.Name)
				continue
			}
			copied := got.Elem().Interface()
			if encryptionSettings, ok := copied.(EncryptionSettings); ok {
				// the two fetchers are the only fields set here
				encryptionSettings.NewPeerClientPublicKeyFetcher = nil
				encryptionSettings.NewPeerClientKeyHistoryFetcher = nil
				copied = encryptionSettings
			}
			if !reflect.DeepEqual(copied, supplied.Elem().Interface()) {
				t.Errorf("%s was not copied value for value", field.Name)
			}
		case sharedFields[field.Name]:
			same := false
			switch field.Type.Kind() {
			case reflect.Interface:
				same = got.Equal(supplied)
			case reflect.Slice:
				same = got.Pointer() == supplied.Pointer() && got.Len() == supplied.Len()
			default:
				same = got.Pointer() == supplied.Pointer()
			}
			if !same {
				t.Errorf("%s is not the caller's", field.Name)
			}
		default:
			t.Errorf("%s (%s) has no disposition: decide whether the result copies it or shares the caller's", field.Name, field.Type)
		}
	}
	// and the other way: every disposition names a reference field
	for _, dispositions := range []map[string]bool{ownedFields, sharedFields} {
		for name := range dispositions {
			field, ok := settingsType.FieldByName(name)
			if !ok || !field.IsExported() || !isReference(field.Type.Kind()) {
				t.Errorf("the disposition of %s names no reference field of ClientSettings", name)
			}
		}
	}
}

// The fetchers read the operator api's own routes: one unauthenticated GET
// each, on the api url as given, whatever path it carries.
func TestOperatorClientSettingsFetchersReadTheOperatorKeyRoutes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	api := newOperatorKeyApiForTest(t, "/api")
	peerId := NewId()
	publicKey := bytes.Repeat([]byte{0x5a}, ed25519.PublicKeySize)
	history := [][]byte{[]byte("synthetic generation 1"), []byte("synthetic generation 2")}
	api.publish(peerId, publicKey, history)
	clientStrategy := newOperatorKeyApiTestStrategy(ctx)
	defer clientStrategy.Close()

	newPublicKeyFetcher, newHistoryFetcher := operatorKeyFetchersForTest(t, NewOperatorClientSettings(nil, api.url(), clientStrategy))
	fetchedPublicKey, err := newPublicKeyFetcher(peerId)(ctx)
	AssertEqual(t, err, nil)
	AssertEqual(t, fetchedPublicKey, publicKey)
	fetchedHistory, err := newHistoryFetcher(peerId)(ctx)
	AssertEqual(t, err, nil)
	AssertEqual(t, fetchedHistory, history)

	AssertEqual(t, api.recordedRequests(), []operatorKeyApiRequestForTest{
		operatorKeyApiRequestForTest{method: http.MethodGet, path: "/api/key/" + peerId.String()},
		operatorKeyApiRequestForTest{method: http.MethodGet, path: "/api/key/" + peerId.String() + "/history"},
	})
}

// A route that cannot answer is an error from the fetcher, never an empty
// answer: an empty history is how the operator says "no signed evidence", and
// the session decides very differently on the two.
func TestOperatorClientSettingsKeyFetchFailureIsAnErrorNotEvidence(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	api := newOperatorKeyApiForTest(t, "")
	peerId := NewId()
	api.publish(peerId, bytes.Repeat([]byte{0x5a}, ed25519.PublicKeySize), [][]byte{[]byte("synthetic generation 1")})
	api.setFailStatus(http.StatusInternalServerError)
	clientStrategy := newOperatorKeyApiTestStrategy(ctx)
	defer clientStrategy.Close()
	newPublicKeyFetcher, newHistoryFetcher := operatorKeyFetchersForTest(t, NewOperatorClientSettings(nil, api.url(), clientStrategy))

	publicKey, err := newPublicKeyFetcher(peerId)(ctx)
	var statusError *HttpStatusError
	if !errors.As(err, &statusError) || statusError.StatusCode != http.StatusInternalServerError || publicKey != nil {
		t.Fatalf("public key fetch from a failing api = %v, %v; want a 500 status error", publicKey, err)
	}
	history, err := newHistoryFetcher(peerId)(ctx)
	if !errors.As(err, &statusError) || statusError.StatusCode != http.StatusInternalServerError || history != nil {
		t.Fatalf("history fetch from a failing api = %v, %v; want a 500 status error", history, err)
	}

	// control: the same route answering for a peer with no signed registration
	// is a successful, empty history
	api.setFailStatus(0)
	history, err = newHistoryFetcher(NewId())(ctx)
	if err != nil || len(history) != 0 {
		t.Fatalf("history of an unsigned peer = %v, %v; want empty and no error", history, err)
	}
}

// The signed history a Required session resolves is the operator's, read over
// http through the installed fetcher: consistent evidence opens the gate and
// pins the head generation.
func TestOperatorClientSettingsResolvesTheOperatorsSignedHistory(t *testing.T) {
	api := newOperatorKeyApiForTest(t, "")
	api.publish(goldenClientKeyId(), nil, goldenClientKeyHistory())
	store := newTestPeerClientKeyPinStore()
	sess, workerDone := newOperatorClientSessionForTest(t, api, goldenClientKeyId(), operatorKeyHistoryRequiredForTest(t, store))

	armKeyHistoryGate(sess)
	sess.resolvePeerClientKeyHistory(goldenHeadPublicKey(t))
	waitForOperatorClientSessionWorkerForTest(t, workerDone, "signed identity resolve")

	if got := keyHistoryState(sess); got != clientKeyHistoryVerified {
		t.Fatalf("state = %s, want verified", got)
	}
	pin, ok := store.GetPeerClientKeyPin(goldenClientKeyId())
	if !ok || pin.Generation != 2 || store.setPinCalls != 1 || !store.signedSeen {
		t.Fatalf("pinned generation = %d (present %t, writes %d, signed seen %t), want 2 from one write", pin.Generation, ok, store.setPinCalls, store.signedSeen)
	}
	AssertEqual(t, api.recordedPaths(), []string{"/key/" + goldenClientKeyId().String() + "/history"})
}

// A contract key the operator's signed history does not attest is rejected,
// and the cipher stays withheld.
func TestOperatorClientSettingsRejectsAKeyTheOperatorsHistoryDoesNotSign(t *testing.T) {
	api := newOperatorKeyApiForTest(t, "")
	api.publish(goldenClientKeyId(), nil, goldenClientKeyHistory())
	store := newTestPeerClientKeyPinStore()
	sess, workerDone := newOperatorClientSessionForTest(t, api, goldenClientKeyId(), operatorKeyHistoryRequiredForTest(t, store))

	substituted := make(ed25519.PublicKey, ed25519.PublicKeySize)
	copy(substituted, goldenHeadPublicKey(t))
	substituted[0] ^= 0x01

	armKeyHistoryGate(sess)
	sess.resolvePeerClientKeyHistory(substituted)
	waitForOperatorClientSessionWorkerForTest(t, workerDone, "signed identity resolve")

	if got := keyHistoryState(sess); got != clientKeyHistoryRejected {
		t.Fatalf("state = %s, want rejected", got)
	}
	if !sess.KeyIdentityRejected() || sess.Cipher() != nil {
		t.Fatal("a rejected peer must report the rejection and expose no cipher")
	}
	if store.setPinCalls != 0 {
		t.Fatal("a rejected resolution must not write a pin")
	}
}

// An operator api that cannot answer leaves the session on the contract key,
// as an availability failure. The ratchet is set first, so the result tells
// the error apart from an empty answer: from an operator that has served
// signed evidence before, an empty history is a downgrade and is rejected.
func TestOperatorClientSettingsTreatsAnUnreachableHistoryAsUnavailable(t *testing.T) {
	api := newOperatorKeyApiForTest(t, "")
	api.publish(goldenClientKeyId(), nil, goldenClientKeyHistory())
	api.setFailStatus(http.StatusInternalServerError)
	store := newTestPeerClientKeyPinStore()
	store.SetSignedHistorySeen()
	sess, workerDone := newOperatorClientSessionForTest(t, api, goldenClientKeyId(), operatorKeyHistoryRequiredForTest(t, store))

	armKeyHistoryGate(sess)
	sess.resolvePeerClientKeyHistory(goldenHeadPublicKey(t))
	waitForOperatorClientSessionWorkerForTest(t, workerDone, "signed identity resolve")

	if got := keyHistoryState(sess); got != clientKeyHistoryVerified {
		t.Fatalf("state = %s, want verified (fallback)", got)
	}
	if store.setPinCalls != 0 {
		t.Fatal("a fallback must not write a pin")
	}
	AssertEqual(t, api.recordedPaths(), []string{"/key/" + goldenClientKeyId().String() + "/history"})
}

// The operator's /key route is the identity source of a peer whose contracts
// carry no key: the session commits the key the api publishes.
func TestOperatorClientSettingsLearnsAPeerKeyFromTheOperator(t *testing.T) {
	api := newOperatorKeyApiForTest(t, "")
	peerId := NewId()
	publicKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{3}, ed25519.SeedSize)).Public().(ed25519.PublicKey)
	api.publish(peerId, publicKey, nil)
	opportunistic := func(settings *ClientSettings) {
		settings.EncryptionSettings.Mode = EncryptionModeOpportunistic
	}

	sess, workerDone := newOperatorClientSessionForTest(t, api, peerId, opportunistic)
	sess.maybeFetchPeerClientPublicKeyForIdentity()
	waitForOperatorClientSessionWorkerForTest(t, workerDone, "identity key fetch")
	AssertEqual(t, []byte(sess.PeerClientPublicKey()), []byte(publicKey))

	// control: a peer the operator has not published stays unknown
	unpublished, unpublishedWorkerDone := newOperatorClientSessionForTest(t, api, NewId(), opportunistic)
	unpublished.maybeFetchPeerClientPublicKeyForIdentity()
	waitForOperatorClientSessionWorkerForTest(t, unpublishedWorkerDone, "identity key fetch")
	if unpublished.PeerClientPublicKey() != nil {
		t.Fatal("a key the operator never published was committed")
	}
}

// The cross-check compares the key a contract carries against the operator's
// and reports a disagreement (today the report is its only action; see
// crossCheckPeerClientPublicKey). A key the operator agrees with is not reported.
func TestOperatorClientSettingsCrossChecksAContractKeyAgainstTheOperator(t *testing.T) {
	api := newOperatorKeyApiForTest(t, "")
	peerId := NewId()
	operatorPublicKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{4}, ed25519.SeedSize)).Public().(ed25519.PublicKey)
	contractPublicKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{5}, ed25519.SeedSize)).Public().(ed25519.PublicKey)
	api.publish(peerId, operatorPublicKey, nil)
	opportunisticWithLog := func(log Logger) func(settings *ClientSettings) {
		return func(settings *ClientSettings) {
			settings.Log = log
			settings.EncryptionSettings.Mode = EncryptionModeOpportunistic
		}
	}

	disagreeingLog := newRecordingLogger()
	disagreeing, disagreeingWorkerDone := newOperatorClientSessionForTest(t, api, peerId, opportunisticWithLog(disagreeingLog))
	disagreeing.SetPeerClientPublicKey(contractPublicKey)
	waitForOperatorClientSessionWorkerForTest(t, disagreeingWorkerDone, "peer key cross-check")
	if lines := disagreeingLog.linesWith("MISMATCH"); len(lines) != 1 {
		t.Fatalf("a contract key the operator disagrees with was reported %d times: %v", len(lines), lines)
	}

	// control: the same check on a key the operator agrees with reports nothing
	agreeingLog := newRecordingLogger()
	agreeing, agreeingWorkerDone := newOperatorClientSessionForTest(t, api, peerId, opportunisticWithLog(agreeingLog))
	agreeing.SetPeerClientPublicKey(operatorPublicKey)
	waitForOperatorClientSessionWorkerForTest(t, agreeingWorkerDone, "peer key cross-check")
	if lines := agreeingLog.linesWith("MISMATCH"); len(lines) != 0 {
		t.Fatalf("a contract key the operator agrees with was reported: %v", lines)
	}

	AssertEqual(t, api.recordedPaths(), []string{"/key/" + peerId.String(), "/key/" + peerId.String()})
}

// one request the operator api received
type operatorKeyApiRequestForTest struct {
	method        string
	path          string
	authorization string
}

// The operator api's two key routes, served from memory. Every request is
// recorded so a test can assert which route a fetcher read, and how.
type operatorKeyApiForTest struct {
	server     *httptest.Server
	pathPrefix string

	stateLock  sync.Mutex
	publicKeys map[Id][]byte
	histories  map[Id][][]byte
	failStatus int
	requests   []operatorKeyApiRequestForTest
}

func newOperatorKeyApiForTest(t *testing.T, pathPrefix string) *operatorKeyApiForTest {
	t.Helper()
	api := &operatorKeyApiForTest{
		pathPrefix: pathPrefix,
		publicKeys: map[Id][]byte{},
		histories:  map[Id][][]byte{},
	}
	api.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		failStatus := func() int {
			api.stateLock.Lock()
			defer api.stateLock.Unlock()
			api.requests = append(api.requests, operatorKeyApiRequestForTest{
				method:        r.Method,
				path:          r.URL.Path,
				authorization: r.Header.Get("Authorization"),
			})
			return api.failStatus
		}()
		rest, ok := strings.CutPrefix(r.URL.Path, api.pathPrefix+"/key/")
		if !ok || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		if failStatus != 0 {
			http.Error(w, "synthetic failure", failStatus)
			return
		}
		if idString, ok := strings.CutSuffix(rest, "/history"); ok {
			peerId, err := ParseId(idString)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			history := func() [][]byte {
				api.stateLock.Lock()
				defer api.stateLock.Unlock()
				return api.histories[peerId]
			}()
			if history == nil {
				// a peer with no signed registration
				history = [][]byte{}
			}
			json.NewEncoder(w).Encode(&GetClientKeyHistoryResult{History: history})
			return
		}
		peerId, err := ParseId(rest)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		publicKey, ok := func() ([]byte, bool) {
			api.stateLock.Lock()
			defer api.stateLock.Unlock()
			publicKey, ok := api.publicKeys[peerId]
			return publicKey, ok
		}()
		if !ok {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(&GetClientKeyResult{PublicKey: publicKey})
	}))
	t.Cleanup(api.server.Close)
	return api
}

func (self *operatorKeyApiForTest) url() string {
	return self.server.URL + self.pathPrefix
}

// a nil key or history leaves that route unpublished for the peer
func (self *operatorKeyApiForTest) publish(peerId Id, publicKey []byte, history [][]byte) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if publicKey != nil {
		self.publicKeys[peerId] = publicKey
	}
	if history != nil {
		self.histories[peerId] = history
	}
}

// a nonzero status answers every key route with that status
func (self *operatorKeyApiForTest) setFailStatus(failStatus int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.failStatus = failStatus
}

func (self *operatorKeyApiForTest) recordedRequests() []operatorKeyApiRequestForTest {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]operatorKeyApiRequestForTest{}, self.requests...)
}

func (self *operatorKeyApiForTest) recordedPaths() []string {
	paths := []string{}
	for _, request := range self.recordedRequests() {
		paths = append(paths, request.path)
	}
	return paths
}

// the two installed fetchers; a missing one fails the test instead of
// panicking the whole binary
func operatorKeyFetchersForTest(
	t *testing.T,
	settings *ClientSettings,
) (func(Id) func(context.Context) ([]byte, error), func(Id) func(context.Context) ([][]byte, error)) {
	t.Helper()
	if settings.EncryptionSettings == nil ||
		settings.EncryptionSettings.NewPeerClientPublicKeyFetcher == nil ||
		settings.EncryptionSettings.NewPeerClientKeyHistoryFetcher == nil {
		t.Fatal("the operator key fetchers were not installed")
	}
	return settings.EncryptionSettings.NewPeerClientPublicKeyFetcher, settings.EncryptionSettings.NewPeerClientKeyHistoryFetcher
}

// one dialer and no resilient variants, so each fetch is exactly one request
func newOperatorKeyApiTestStrategy(ctx context.Context) *ClientStrategy {
	settings := DefaultClientStrategySettings()
	settings.EnableNormal = true
	settings.EnableResilient = false
	settings.RequestTimeout = 5 * time.Second
	return NewClientStrategy(ctx, settings)
}

// Required mode, the given ratchet store, and the golden chain's signer as the
// trusted signer: the configuration newTestKeyHistorySession gives its sessions.
func operatorKeyHistoryRequiredForTest(t *testing.T, store PeerClientKeyPinStore) func(settings *ClientSettings) {
	t.Helper()
	g1, err := decodeClientKeyRegistration([]byte(goldenClientKeyG1))
	if err != nil {
		t.Fatalf("decode golden: %s", err)
	}
	digest, err := g1.Domain.Digest()
	if err != nil {
		t.Fatalf("golden domain digest: %s", err)
	}
	return func(settings *ClientSettings) {
		settings.EncryptionSettings.Mode = EncryptionModeRequired
		settings.EncryptionSettings.TlsTimeout = 2 * time.Second
		settings.EncryptionSettings.PeerClientKeyPinStore = store
		settings.EncryptionSettings.MaxClientKeyHistoryGenerations = 8
		settings.EncryptionSettings.TrustedClientKeySigners = []ClientKeyTrustedSigner{
			ClientKeyTrustedSigner{DomainDigest: digest, Signer: g1.Signer},
		}
	}
}

// A session with peerId, of a client whose settings come from
// NewOperatorClientSettings against api, built the way newTestKeyHistorySession
// builds one. The channel receives the name of each session worker as it
// finishes.
func newOperatorClientSessionForTest(
	t *testing.T,
	api *operatorKeyApiForTest,
	peerId Id,
	configure func(settings *ClientSettings),
) (*peerEncryptionSession, chan string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	clientStrategy := newOperatorKeyApiTestStrategy(ctx)
	t.Cleanup(clientStrategy.Close)

	settings := DefaultClientSettings()
	configure(settings)
	operatorSettings := NewOperatorClientSettings(settings, api.url(), clientStrategy)

	client := NewClient(ctx, NewId(), NewNoContractClientOob(), operatorSettings)
	t.Cleanup(client.Cancel)
	keyManager, err := NewClientKeyManager(ctx, client)
	if err != nil {
		t.Fatalf("NewClientKeyManager: %s", err)
	}
	manager := NewEncryptionSessionManager(ctx, client, keyManager, operatorSettings.EncryptionSettings)
	sess := newPeerEncryptionSession(
		ctx,
		manager,
		client,
		peerId,
		sequenceTlsRoleClient,
		operatorSettings.EncryptionSettings,
		manager.ClientTlsConfig(),
		false,
	)
	workerDone := make(chan string, 16)
	sess.beforeWorkerDoneForTest = func(name string) {
		select {
		case workerDone <- name:
		default:
		}
	}
	return sess, workerDone
}

func waitForOperatorClientSessionWorkerForTest(t *testing.T, workerDone chan string, name string) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case done := <-workerDone:
			if done == name {
				return
			}
		case <-deadline:
			t.Fatalf("the %q worker never finished: the session did not reach the operator api", name)
		}
	}
}
