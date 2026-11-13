package connect

// net_tls_chain_cache_test.go — the verified-chain cache (net_tls_chain_cache.go)
// against local Go tls servers with a synthetic pki shaped like the platform's
// Let's Encrypt chain (P-384 root and intermediate, P-256 leaf). Each verdict
// case dials once through a handshaker with the cache and once through one
// without it (uTLS verifies the chain itself), and compares the two: a valid
// chain, an expired or not yet valid certificate, another name, another root,
// a server holding the genuine chain without its key, over tls 1.3 and 1.2,
// and resumption. The clocks are explicit: the dial's clock is the config's
// Time, and the cache's clock is its settings' Now.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
)

// The synthetic names the chain test servers are certified for.
const (
	testTlsChainServerName      = "api.chain-cache.example"
	testTlsChainOtherServerName = "other.chain-cache.example"
)

// A clock a test sets.
type testTlsChainClock struct {
	stateLock sync.Mutex
	time      time.Time
}

func newTestTlsChainClock(now time.Time) *testTlsChainClock {
	return &testTlsChainClock{
		time: now,
	}
}

func (self *testTlsChainClock) now() time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.time
}

func (self *testTlsChainClock) set(now time.Time) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.time = now
}

// A synthetic pki: a P-384 root, a P-384 intermediate it signs, and leaves the
// intermediate signs. With a cross root, the intermediate is signed by a
// P-384 root that the pool root cross-signs, as Let's Encrypt's Root YE is
// cross-signed by ISRG Root X2, and the server presents the cross-signed root
// after the intermediate.
type testTlsChainPki struct {
	rootPool        *x509.CertPool
	root            *x509.Certificate
	intermediate    *x509.Certificate
	intermediateKey *ecdsa.PrivateKey
	// presented after the intermediate; nil without a cross root
	crossRoot *x509.Certificate
}

var testTlsChainSerialNumber = struct {
	stateLock sync.Mutex
	value     int64
}{}

func nextTestTlsChainSerialNumber() *big.Int {
	testTlsChainSerialNumber.stateLock.Lock()
	defer testTlsChainSerialNumber.stateLock.Unlock()
	testTlsChainSerialNumber.value += 1
	return big.NewInt(testTlsChainSerialNumber.value)
}

func newTestTlsChainKey(t testing.TB, curve elliptic.Curve) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(curve, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func newTestTlsChainCertificate(t testing.TB, template *x509.Certificate, parent *x509.Certificate, publicKey any, signer *ecdsa.PrivateKey) *x509.Certificate {
	t.Helper()
	template.SerialNumber = nextTestTlsChainSerialNumber()
	if parent == nil {
		parent = template
	}
	der, err := x509.CreateCertificate(rand.Reader, template, parent, publicKey, signer)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return certificate
}

func newTestTlsChainCaTemplate(name string, notBefore time.Time, notAfter time.Time) *x509.Certificate {
	return &x509.Certificate{
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
}

// A pki valid around now: the root for ten years either side, the
// intermediate for one.
func newTestTlsChainPki(t testing.TB, now time.Time, crossRoot bool) *testTlsChainPki {
	t.Helper()
	return newTestTlsChainPkiWithIntermediateWindow(t, now, now.Add(-365*24*time.Hour), now.Add(365*24*time.Hour), crossRoot)
}

func newTestTlsChainPkiWithIntermediateWindow(t testing.TB, now time.Time, intermediateNotBefore time.Time, intermediateNotAfter time.Time, crossRoot bool) *testTlsChainPki {
	t.Helper()
	rootKey := newTestTlsChainKey(t, elliptic.P384())
	root := newTestTlsChainCertificate(t, newTestTlsChainCaTemplate("chain cache test root", now.Add(-10*365*24*time.Hour), now.Add(10*365*24*time.Hour)), nil, &rootKey.PublicKey, rootKey)
	pki := &testTlsChainPki{
		rootPool: x509.NewCertPool(),
		root:     root,
	}
	pki.rootPool.AddCert(root)

	issuer := root
	issuerKey := rootKey
	if crossRoot {
		crossRootKey := newTestTlsChainKey(t, elliptic.P384())
		pki.crossRoot = newTestTlsChainCertificate(t, newTestTlsChainCaTemplate("chain cache test cross root", now.Add(-5*365*24*time.Hour), now.Add(5*365*24*time.Hour)), root, &crossRootKey.PublicKey, rootKey)
		issuer = pki.crossRoot
		issuerKey = crossRootKey
	}
	pki.intermediateKey = newTestTlsChainKey(t, elliptic.P384())
	pki.intermediate = newTestTlsChainCertificate(t, newTestTlsChainCaTemplate("chain cache test intermediate", intermediateNotBefore, intermediateNotAfter), issuer, &pki.intermediateKey.PublicKey, issuerKey)
	return pki
}

// A leaf for serverName valid from notBefore to notAfter, with the chain the
// server presents and the leaf's key.
func (self *testTlsChainPki) newLeaf(t testing.TB, serverName string, notBefore time.Time, notAfter time.Time) tls.Certificate {
	t.Helper()
	leafKey := newTestTlsChainKey(t, elliptic.P256())
	leaf := newTestTlsChainCertificate(t, &x509.Certificate{
		Subject:     pkix.Name{CommonName: serverName},
		DNSNames:    []string{serverName},
		NotBefore:   notBefore,
		NotAfter:    notAfter,
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, self.intermediate, &leafKey.PublicKey, self.intermediateKey)
	chain := [][]byte{leaf.Raw, self.intermediate.Raw}
	if self.crossRoot != nil {
		chain = append(chain, self.crossRoot.Raw)
	}
	return tls.Certificate{
		Certificate: chain,
		PrivateKey:  leafKey,
		Leaf:        leaf,
	}
}

// The same chain bytes held with another key: an impostor that copied the
// genuine chain but not its private key.
func testTlsChainImpostor(t testing.TB, genuine tls.Certificate) tls.Certificate {
	t.Helper()
	return tls.Certificate{
		Certificate: genuine.Certificate,
		PrivateKey:  newTestTlsChainKey(t, elliptic.P256()),
		Leaf:        genuine.Leaf,
	}
}

// A local tls server presenting one certificate. Each connection completes
// its handshake and writes one byte, which the client reads so that a tls 1.3
// session ticket sent before it is taken in.
type testTlsChainServer struct {
	address string
}

func newTestTlsChainServer(t testing.TB, certificate tls.Certificate, maxVersion uint16) *testTlsChainServer {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	config := &tls.Config{
		Certificates: []tls.Certificate{certificate},
		MaxVersion:   maxVersion,
		NextProtos:   []string{"http/1.1"},
	}
	var connections sync.WaitGroup
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			connections.Add(1)
			go func() {
				defer connections.Done()
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(30 * time.Second))
				tlsConn := tls.Server(conn, config)
				if err := tlsConn.Handshake(); err != nil {
					return
				}
				if _, err := tlsConn.Write([]byte{1}); err != nil {
					return
				}
				io.Copy(io.Discard, tlsConn)
			}()
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		connections.Wait()
	})
	return &testTlsChainServer{
		address: listener.Addr().String(),
	}
}

// A Chrome-hello handshaker of one dial path, verifying through cache; nil
// leaves the verification to uTLS, as before the cache.
func newTestTlsChainHandshaker(cache *tlsVerifiedChainCache) *clientTlsHandshaker {
	return &clientTlsHandshaker{
		chrome:             true,
		chromeSessionCache: newChromeSessionCache(),
		chainCache:         cache,
	}
}

func newTestTlsChainCache(clock *testTlsChainClock, rootPools ...*x509.CertPool) *tlsVerifiedChainCache {
	settings := defaultTlsVerifiedChainCacheSettings()
	settings.Now = clock.now
	cache := newTlsVerifiedChainCache(settings)
	for _, rootPool := range rootPools {
		cache.addRootPool(rootPool)
	}
	return cache
}

// The configuration of a dial path: the roots, the server name, the dial's
// clock and the websocket path's protocol.
func newTestTlsChainConfig(rootPool *x509.CertPool, serverName string, clock *testTlsChainClock) *tls.Config {
	return &tls.Config{
		RootCAs:    rootPool,
		ServerName: serverName,
		Time:       clock.now,
		NextProtos: []string{"http/1.1"},
	}
}

// Dials server and runs the handshake as a dial path does, on its own clone of
// the configuration, then reads the server's byte. The state is the
// connection's as a caller reads it.
func testTlsChainDial(t testing.TB, handshaker *clientTlsHandshaker, server *testTlsChainServer, config *tls.Config) (tls.ConnectionState, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp4", server.address)
	if err != nil {
		t.Fatal(err)
	}
	tlsConn, err := handshaker.handshake(ctx, conn, config.Clone())
	if err != nil {
		return tls.ConnectionState{}, err
	}
	defer tlsConn.Close()
	tlsConn.SetDeadline(time.Now().Add(30 * time.Second))
	var b [1]byte
	if _, err := io.ReadFull(tlsConn, b[:]); err != nil {
		t.Fatalf("read after the handshake: %s", err)
	}
	stater, ok := tlsConn.(interface{ ConnectionState() tls.ConnectionState })
	if !ok {
		t.Fatalf("dialed %T, which has no tls connection state", tlsConn)
	}
	return stater.ConnectionState(), nil
}

// The raw certificates of each chain, for comparing chains across handshakes.
func testTlsChainRaws(chains [][]*x509.Certificate) [][][]byte {
	raws := [][][]byte{}
	for _, chain := range chains {
		chainRaws := [][]byte{}
		for _, certificate := range chain {
			chainRaws = append(chainRaws, certificate.Raw)
		}
		raws = append(raws, chainRaws)
	}
	return raws
}

func testTlsChainRawsEqual(a [][]*x509.Certificate, b [][]*x509.Certificate) bool {
	return slices.EqualFunc(testTlsChainRaws(a), testTlsChainRaws(b), func(x [][]byte, y [][]byte) bool {
		return slices.EqualFunc(x, y, func(p []byte, q []byte) bool {
			return string(p) == string(q)
		})
	})
}

// Fails unless the cached dial's verdict is the uncached dial's: both accept
// with the same verified chains, or both refuse with the same error type and
// text.
func assertTestTlsChainSameVerdict(t *testing.T, description string, cachedState tls.ConnectionState, cachedErr error, uncachedState tls.ConnectionState, uncachedErr error) {
	t.Helper()
	if (cachedErr == nil) != (uncachedErr == nil) {
		t.Fatalf("%s: cached err = %v, uncached err = %v", description, cachedErr, uncachedErr)
	}
	if cachedErr != nil {
		if fmt.Sprintf("%T", cachedErr) != fmt.Sprintf("%T", uncachedErr) || cachedErr.Error() != uncachedErr.Error() {
			t.Fatalf("%s: cached err = %v (%T), uncached err = %v (%T)", description, cachedErr, cachedErr, uncachedErr, uncachedErr)
		}
		var cachedVerificationErr *tls.CertificateVerificationError
		var uncachedVerificationErr *tls.CertificateVerificationError
		if errors.As(cachedErr, &cachedVerificationErr) != errors.As(uncachedErr, &uncachedVerificationErr) {
			t.Fatalf("%s: one refusal is a certificate verification error and the other is not", description)
		}
		if cachedVerificationErr != nil && fmt.Sprintf("%T", cachedVerificationErr.Err) != fmt.Sprintf("%T", uncachedVerificationErr.Err) {
			t.Fatalf("%s: cached cause %T, uncached cause %T", description, cachedVerificationErr.Err, uncachedVerificationErr.Err)
		}
		return
	}
	if len(cachedState.VerifiedChains) == 0 || !testTlsChainRawsEqual(cachedState.VerifiedChains, uncachedState.VerifiedChains) {
		t.Fatalf("%s: cached chains %d, uncached chains %d, or they differ", description, len(cachedState.VerifiedChains), len(uncachedState.VerifiedChains))
	}
	if cachedState.DidResume != uncachedState.DidResume || cachedState.Version != uncachedState.Version {
		t.Fatalf("%s: cached resumed=%t version=%x, uncached resumed=%t version=%x", description, cachedState.DidResume, cachedState.Version, uncachedState.DidResume, uncachedState.Version)
	}
}

func assertTestTlsChainStats(t *testing.T, description string, cache *tlsVerifiedChainCache, want TlsVerifiedChainCacheStats) {
	t.Helper()
	if stats := cache.stats(); stats != want {
		t.Fatalf("%s: stats = %+v, want %+v", description, stats, want)
	}
}

// A second dial of the same chain, name and root pool reuses the first dial's
// verification, and both accept with the chains uTLS builds itself.
func TestTlsChainCacheSecondDialReusesVerification(t *testing.T) {
	now := time.Now()
	clock := newTestTlsChainClock(now)
	pki := newTestTlsChainPki(t, now, true)
	server := newTestTlsChainServer(t, pki.newLeaf(t, testTlsChainServerName, now.Add(-time.Hour), now.Add(time.Hour)), 0)
	cache := newTestTlsChainCache(clock, pki.rootPool)
	config := newTestTlsChainConfig(pki.rootPool, testTlsChainServerName, clock)
	config.SessionTicketsDisabled = true

	uncachedState, uncachedErr := testTlsChainDial(t, newTestTlsChainHandshaker(nil), server, config)
	cachedHandshaker := newTestTlsChainHandshaker(cache)
	firstState, firstErr := testTlsChainDial(t, cachedHandshaker, server, config)
	assertTestTlsChainSameVerdict(t, "first dial", firstState, firstErr, uncachedState, uncachedErr)
	assertTestTlsChainStats(t, "first dial", cache, TlsVerifiedChainCacheStats{MissCount: 1, StoreCount: 1, EntryCount: 1})

	secondState, secondErr := testTlsChainDial(t, cachedHandshaker, server, config)
	assertTestTlsChainSameVerdict(t, "second dial", secondState, secondErr, uncachedState, uncachedErr)
	assertTestTlsChainStats(t, "second dial", cache, TlsVerifiedChainCacheStats{HitCount: 1, MissCount: 1, StoreCount: 1, EntryCount: 1})
	if len(secondState.VerifiedChains[0]) != 4 {
		t.Fatalf("the reused chain has %d certificates, want leaf, intermediate, cross root and root", len(secondState.VerifiedChains[0]))
	}

	// another dial path, with its own handshaker and session cache, reuses it
	thirdState, thirdErr := testTlsChainDial(t, newTestTlsChainHandshaker(cache), server, config)
	assertTestTlsChainSameVerdict(t, "another path", thirdState, thirdErr, uncachedState, uncachedErr)
	assertTestTlsChainStats(t, "another path", cache, TlsVerifiedChainCacheStats{HitCount: 2, MissCount: 1, StoreCount: 1, EntryCount: 1})
}

// A server presenting the genuine chain without its private key is refused on
// a reused verification, over tls 1.3 (CertificateVerify) and tls 1.2
// (ServerKeyExchange), exactly as without the cache.
func TestTlsChainCacheRefusesGenuineChainWithoutItsKey(t *testing.T) {
	for _, maxVersion := range []uint16{tls.VersionTLS13, tls.VersionTLS12} {
		now := time.Now()
		clock := newTestTlsChainClock(now)
		pki := newTestTlsChainPki(t, now, false)
		genuine := pki.newLeaf(t, testTlsChainServerName, now.Add(-time.Hour), now.Add(time.Hour))
		genuineServer := newTestTlsChainServer(t, genuine, maxVersion)
		impostorServer := newTestTlsChainServer(t, testTlsChainImpostor(t, genuine), maxVersion)
		cache := newTestTlsChainCache(clock, pki.rootPool)
		config := newTestTlsChainConfig(pki.rootPool, testTlsChainServerName, clock)
		config.SessionTicketsDisabled = true
		description := fmt.Sprintf("version %x", maxVersion)

		genuineState, err := testTlsChainDial(t, newTestTlsChainHandshaker(cache), genuineServer, config)
		if err != nil {
			t.Fatalf("%s: genuine dial: %s", description, err)
		}
		if genuineState.Version != maxVersion {
			t.Fatalf("%s: negotiated %x", description, genuineState.Version)
		}
		uncachedState, uncachedErr := testTlsChainDial(t, newTestTlsChainHandshaker(nil), impostorServer, config)
		cachedState, cachedErr := testTlsChainDial(t, newTestTlsChainHandshaker(cache), impostorServer, config)
		if cachedErr == nil {
			t.Fatalf("%s: the impostor was accepted on a reused verification", description)
		}
		assertTestTlsChainSameVerdict(t, description, cachedState, cachedErr, uncachedState, uncachedErr)
		// the impostor's dial reused the verification and still failed
		assertTestTlsChainStats(t, description, cache, TlsVerifiedChainCacheStats{HitCount: 1, MissCount: 1, StoreCount: 1, EntryCount: 1})
	}
}

// A chain from another authority, self-signed or under another root, is
// refused cold and after the genuine chain's verification is remembered, with
// the error uTLS returns.
func TestTlsChainCacheRefusesUntrustedChains(t *testing.T) {
	now := time.Now()
	clock := newTestTlsChainClock(now)
	pki := newTestTlsChainPki(t, now, false)
	otherPki := newTestTlsChainPki(t, now, false)
	genuineServer := newTestTlsChainServer(t, pki.newLeaf(t, testTlsChainServerName, now.Add(-time.Hour), now.Add(time.Hour)), 0)
	otherRootServer := newTestTlsChainServer(t, otherPki.newLeaf(t, testTlsChainServerName, now.Add(-time.Hour), now.Add(time.Hour)), 0)
	selfSignedKey := newTestTlsChainKey(t, elliptic.P256())
	selfSigned := newTestTlsChainCertificate(t, &x509.Certificate{
		Subject:     pkix.Name{CommonName: testTlsChainServerName},
		DNSNames:    []string{testTlsChainServerName},
		NotBefore:   now.Add(-time.Hour),
		NotAfter:    now.Add(time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, nil, &selfSignedKey.PublicKey, selfSignedKey)
	selfSignedServer := newTestTlsChainServer(t, tls.Certificate{Certificate: [][]byte{selfSigned.Raw}, PrivateKey: selfSignedKey}, 0)
	cache := newTestTlsChainCache(clock, pki.rootPool)
	config := newTestTlsChainConfig(pki.rootPool, testTlsChainServerName, clock)

	for _, warm := range []bool{false, true} {
		if warm {
			if _, err := testTlsChainDial(t, newTestTlsChainHandshaker(cache), genuineServer, config); err != nil {
				t.Fatalf("genuine dial: %s", err)
			}
		}
		for _, untrusted := range []struct {
			description string
			server      *testTlsChainServer
		}{
			{description: "another root", server: otherRootServer},
			{description: "self-signed", server: selfSignedServer},
		} {
			description := fmt.Sprintf("%s warm=%t", untrusted.description, warm)
			uncachedState, uncachedErr := testTlsChainDial(t, newTestTlsChainHandshaker(nil), untrusted.server, config)
			cachedState, cachedErr := testTlsChainDial(t, newTestTlsChainHandshaker(cache), untrusted.server, config)
			var unknownAuthorityErr x509.UnknownAuthorityError
			if !errors.As(cachedErr, &unknownAuthorityErr) {
				t.Fatalf("%s: err = %v, want an unknown authority", description, cachedErr)
			}
			assertTestTlsChainSameVerdict(t, description, cachedState, cachedErr, uncachedState, uncachedErr)
		}
	}
	// refusals are never remembered
	if stats := cache.stats(); stats.HitCount != 0 || stats.StoreCount != 1 || stats.EntryCount != 1 {
		t.Fatalf("stats = %+v, want only the genuine chain remembered and no reuse", stats)
	}
}

// A dial for a name the leaf does not hold is refused, as without the cache:
// the name is part of the key, so it misses; and an entry that would answer
// for that name anyway still fails the name check of every handshake.
func TestTlsChainCacheChecksTheNameOnEveryHandshake(t *testing.T) {
	now := time.Now()
	clock := newTestTlsChainClock(now)
	pki := newTestTlsChainPki(t, now, false)
	server := newTestTlsChainServer(t, pki.newLeaf(t, testTlsChainServerName, now.Add(-time.Hour), now.Add(time.Hour)), 0)
	cache := newTestTlsChainCache(clock, pki.rootPool)
	config := newTestTlsChainConfig(pki.rootPool, testTlsChainServerName, clock)
	otherConfig := newTestTlsChainConfig(pki.rootPool, testTlsChainOtherServerName, clock)

	genuineState, err := testTlsChainDial(t, newTestTlsChainHandshaker(cache), server, config)
	if err != nil {
		t.Fatal(err)
	}
	uncachedState, uncachedErr := testTlsChainDial(t, newTestTlsChainHandshaker(nil), server, otherConfig)
	cachedState, cachedErr := testTlsChainDial(t, newTestTlsChainHandshaker(cache), server, otherConfig)
	var hostnameErr x509.HostnameError
	if !errors.As(cachedErr, &hostnameErr) {
		t.Fatalf("err = %v, want a hostname error", cachedErr)
	}
	assertTestTlsChainSameVerdict(t, "another name", cachedState, cachedErr, uncachedState, uncachedErr)

	// an unsound entry: the genuine verification remembered under the other
	// name. The handshake reuses it, and the name check refuses.
	identity, ok := cache.rootPoolIdentity(pki.rootPool)
	if !ok {
		t.Fatal("the root pool is not registered")
	}
	cache.store(newTestTlsChainKeyForState(identity, testTlsChainOtherServerName, genuineState), genuineState.VerifiedChains)
	hitCount := cache.stats().HitCount
	forcedState, forcedErr := testTlsChainDial(t, newTestTlsChainHandshaker(cache), server, otherConfig)
	if cache.stats().HitCount != hitCount+1 {
		t.Fatal("the dial did not reuse the planted entry")
	}
	if !errors.As(forcedErr, &hostnameErr) {
		t.Fatalf("err = %v, want a hostname error", forcedErr)
	}
	assertTestTlsChainSameVerdict(t, "reused for another name", forcedState, forcedErr, uncachedState, uncachedErr)
}

// The key of the verification of the chain a handshake was presented.
func newTestTlsChainKeyForState(rootPoolIdentity uint64, serverName string, state tls.ConnectionState) tlsVerifiedChainKey {
	return newTlsVerifiedChainKey(rootPoolIdentity, serverName, state.PeerCertificates)
}

// A remembered verification answers only while the dial's clock is inside the
// validity of every certificate of a chain. Past the leaf's end, before its
// start, or past the intermediate's end, the dial verifies in full and is
// refused as without the cache; back inside, it reuses the verification.
func TestTlsChainCacheChecksValidityOnEveryHandshake(t *testing.T) {
	now := time.Now()
	clock := newTestTlsChainClock(now)
	// the intermediate ends before the leaf, so a time between them is
	// outside the chain and inside the leaf
	pki := newTestTlsChainPkiWithIntermediateWindow(t, now, now.Add(-24*time.Hour), now.Add(2*time.Hour), false)
	leafNotBefore := now.Add(-time.Hour)
	leafNotAfter := now.Add(3 * time.Hour)
	server := newTestTlsChainServer(t, pki.newLeaf(t, testTlsChainServerName, leafNotBefore, leafNotAfter), 0)
	// the cache's own clock stays put, so its entry stays young: only the
	// dial's clock, which judges validity, moves
	cacheClock := newTestTlsChainClock(now)
	cache := newTestTlsChainCache(cacheClock, pki.rootPool)
	config := newTestTlsChainConfig(pki.rootPool, testTlsChainServerName, clock)
	config.SessionTicketsDisabled = true

	if _, err := testTlsChainDial(t, newTestTlsChainHandshaker(cache), server, config); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		description string
		dialTime    time.Time
	}{
		{description: "after the leaf", dialTime: leafNotAfter.Add(time.Second)},
		{description: "before the leaf", dialTime: leafNotBefore.Add(-time.Second)},
		{description: "after the intermediate", dialTime: now.Add(2*time.Hour + time.Second)},
	} {
		clock.set(c.dialTime)
		uncachedState, uncachedErr := testTlsChainDial(t, newTestTlsChainHandshaker(nil), server, config)
		cachedState, cachedErr := testTlsChainDial(t, newTestTlsChainHandshaker(cache), server, config)
		if cachedErr == nil {
			t.Fatalf("%s: accepted", c.description)
		}
		assertTestTlsChainSameVerdict(t, c.description, cachedState, cachedErr, uncachedState, uncachedErr)
	}
	stats := cache.stats()
	if stats.HitCount != 0 || stats.MissCount != 4 || stats.EntryCount != 1 {
		t.Fatalf("stats = %+v, want every dial outside the window to miss", stats)
	}

	// inside the window again the remembered verification answers
	clock.set(now.Add(time.Hour))
	uncachedState, uncachedErr := testTlsChainDial(t, newTestTlsChainHandshaker(nil), server, config)
	cachedState, cachedErr := testTlsChainDial(t, newTestTlsChainHandshaker(cache), server, config)
	assertTestTlsChainSameVerdict(t, "inside again", cachedState, cachedErr, uncachedState, uncachedErr)
	if cache.stats().HitCount != 1 {
		t.Fatal("the dial inside the window did not reuse the verification")
	}
}

// A resumed handshake is verified as a full one, from the session's
// certificates: it reuses the remembered verification over tls 1.3 and tls
// 1.2, and exposes the chains. Past the leaf's end the stack drops the session
// itself, and the full handshake is refused as without the cache.
func TestTlsChainCacheVerifiesResumedHandshakes(t *testing.T) {
	for _, maxVersion := range []uint16{tls.VersionTLS13, tls.VersionTLS12} {
		now := time.Now()
		clock := newTestTlsChainClock(now)
		pki := newTestTlsChainPki(t, now, false)
		leafNotAfter := now.Add(time.Hour)
		server := newTestTlsChainServer(t, pki.newLeaf(t, testTlsChainServerName, now.Add(-time.Hour), leafNotAfter), maxVersion)
		cache := newTestTlsChainCache(clock, pki.rootPool)
		config := newTestTlsChainConfig(pki.rootPool, testTlsChainServerName, clock)
		description := fmt.Sprintf("version %x", maxVersion)

		cachedHandshaker := newTestTlsChainHandshaker(cache)
		uncachedHandshaker := newTestTlsChainHandshaker(nil)
		for _, dial := range []string{"first", "resumed"} {
			uncachedState, uncachedErr := testTlsChainDial(t, uncachedHandshaker, server, config)
			cachedState, cachedErr := testTlsChainDial(t, cachedHandshaker, server, config)
			assertTestTlsChainSameVerdict(t, description+" "+dial, cachedState, cachedErr, uncachedState, uncachedErr)
			if cachedState.DidResume != (dial == "resumed") {
				t.Fatalf("%s %s: resumed = %t", description, dial, cachedState.DidResume)
			}
		}
		assertTestTlsChainStats(t, description, cache, TlsVerifiedChainCacheStats{HitCount: 1, MissCount: 1, StoreCount: 1, EntryCount: 1})

		clock.set(leafNotAfter.Add(time.Second))
		uncachedState, uncachedErr := testTlsChainDial(t, uncachedHandshaker, server, config)
		cachedState, cachedErr := testTlsChainDial(t, cachedHandshaker, server, config)
		var invalidErr x509.CertificateInvalidError
		if !errors.As(cachedErr, &invalidErr) {
			t.Fatalf("%s: err = %v, want an invalid certificate", description, cachedErr)
		}
		assertTestTlsChainSameVerdict(t, description+" expired", cachedState, cachedErr, uncachedState, uncachedErr)
	}
}

// A resumed handshake is checked for validity like a full one. With the
// dial's clock stepped back before the leaf's start, the stack itself would
// resume without verifying the chain again (it checks only the leaf's end);
// with the cache the resumption is refused, as a full handshake is at that
// time.
func TestTlsChainCacheRefusesResumptionOutsideValidity(t *testing.T) {
	now := time.Now()
	clock := newTestTlsChainClock(now)
	pki := newTestTlsChainPki(t, now, false)
	leafNotBefore := now.Add(-time.Hour)
	server := newTestTlsChainServer(t, pki.newLeaf(t, testTlsChainServerName, leafNotBefore, now.Add(time.Hour)), 0)
	cache := newTestTlsChainCache(clock, pki.rootPool)
	config := newTestTlsChainConfig(pki.rootPool, testTlsChainServerName, clock)

	cachedHandshaker := newTestTlsChainHandshaker(cache)
	uncachedHandshaker := newTestTlsChainHandshaker(nil)
	for _, handshaker := range []*clientTlsHandshaker{cachedHandshaker, uncachedHandshaker} {
		if _, err := testTlsChainDial(t, handshaker, server, config); err != nil {
			t.Fatal(err)
		}
	}

	clock.set(leafNotBefore.Add(-time.Second))
	uncachedState, uncachedErr := testTlsChainDial(t, uncachedHandshaker, server, config)
	if uncachedErr != nil || !uncachedState.DidResume {
		t.Fatalf("uncached: err = %v resumed = %t, want the stack to resume unverified", uncachedErr, uncachedState.DidResume)
	}
	_, cachedErr := testTlsChainDial(t, cachedHandshaker, server, config)
	var invalidErr x509.CertificateInvalidError
	if !errors.As(cachedErr, &invalidErr) {
		t.Fatalf("cached: err = %v, want an invalid certificate", cachedErr)
	}
	// a full handshake at that time is refused the same way
	fullConfig := config.Clone()
	fullConfig.SessionTicketsDisabled = true
	_, fullErr := testTlsChainDial(t, newTestTlsChainHandshaker(nil), server, fullConfig)
	if fullErr == nil || fullErr.Error() != cachedErr.Error() {
		t.Fatalf("full handshake err = %v, cached resumption err = %v", fullErr, cachedErr)
	}
}

// A verification answers only dials against the root pool it ran under. A
// dial against another registered pool, even one holding the same root,
// verifies in full; a pool whose registration was dropped verifies as before
// the cache.
func TestTlsChainCacheKeysByRootPool(t *testing.T) {
	now := time.Now()
	clock := newTestTlsChainClock(now)
	pki := newTestTlsChainPki(t, now, false)
	otherPki := newTestTlsChainPki(t, now, false)
	server := newTestTlsChainServer(t, pki.newLeaf(t, testTlsChainServerName, now.Add(-time.Hour), now.Add(time.Hour)), 0)
	sameRootPool := x509.NewCertPool()
	sameRootPool.AddCert(pki.root)
	cache := newTestTlsChainCache(clock, pki.rootPool, otherPki.rootPool, sameRootPool)

	if _, err := testTlsChainDial(t, newTestTlsChainHandshaker(cache), server, newTestTlsChainConfig(pki.rootPool, testTlsChainServerName, clock)); err != nil {
		t.Fatal(err)
	}
	otherConfig := newTestTlsChainConfig(otherPki.rootPool, testTlsChainServerName, clock)
	uncachedState, uncachedErr := testTlsChainDial(t, newTestTlsChainHandshaker(nil), server, otherConfig)
	cachedState, cachedErr := testTlsChainDial(t, newTestTlsChainHandshaker(cache), server, otherConfig)
	var unknownAuthorityErr x509.UnknownAuthorityError
	if !errors.As(cachedErr, &unknownAuthorityErr) {
		t.Fatalf("another root: err = %v, want an unknown authority", cachedErr)
	}
	assertTestTlsChainSameVerdict(t, "another root", cachedState, cachedErr, uncachedState, uncachedErr)

	sameRootConfig := newTestTlsChainConfig(sameRootPool, testTlsChainServerName, clock)
	if _, err := testTlsChainDial(t, newTestTlsChainHandshaker(cache), server, sameRootConfig); err != nil {
		t.Fatal(err)
	}
	assertTestTlsChainStats(t, "pools", cache, TlsVerifiedChainCacheStats{MissCount: 3, StoreCount: 2, EntryCount: 2})

	// one registration only: registering another drops the first, whose dials
	// no longer reach the cache
	settings := defaultTlsVerifiedChainCacheSettings()
	settings.Now = clock.now
	settings.MaxRootPoolCount = 1
	boundedCache := newTlsVerifiedChainCache(settings)
	boundedCache.addRootPool(pki.rootPool)
	firstIdentity, _ := boundedCache.rootPoolIdentity(pki.rootPool)
	boundedCache.addRootPool(otherPki.rootPool)
	if boundedCache.verification(newTestTlsChainConfig(pki.rootPool, testTlsChainServerName, clock)) != nil {
		t.Fatal("a dropped registration still reaches the cache")
	}
	if boundedCache.verification(otherConfig) == nil {
		t.Fatal("the newest registration does not reach the cache")
	}
	if _, err := testTlsChainDial(t, newTestTlsChainHandshaker(boundedCache), server, newTestTlsChainConfig(pki.rootPool, testTlsChainServerName, clock)); err != nil {
		t.Fatal(err)
	}
	assertTestTlsChainStats(t, "dropped registration", boundedCache, TlsVerifiedChainCacheStats{})
	// registered again, the pool gets a new identity, so nothing remembered
	// under its first registration answers it
	boundedCache.addRootPool(pki.rootPool)
	if identity, ok := boundedCache.rootPoolIdentity(pki.rootPool); !ok || identity == firstIdentity {
		t.Fatalf("re-registered identity = %d (registered %t), want one other than %d", identity, ok, firstIdentity)
	}
}

// The cache serves only the Chrome hello's dials that verify against a
// registered pool with no verification of their own, and for a server name.
// Every other dial verifies as before and never reaches the cache.
func TestTlsChainCacheLeavesIneligibleDialsUnchanged(t *testing.T) {
	now := time.Now()
	clock := newTestTlsChainClock(now)
	pki := newTestTlsChainPki(t, now, false)
	server := newTestTlsChainServer(t, pki.newLeaf(t, testTlsChainServerName, now.Add(-time.Hour), now.Add(time.Hour)), 0)
	unregisteredPool := x509.NewCertPool()
	unregisteredPool.AddCert(pki.root)
	cache := newTestTlsChainCache(clock, pki.rootPool)
	refused := errors.New("refused by the verifier")
	withConfig := func(change func(*tls.Config)) *tls.Config {
		config := newTestTlsChainConfig(pki.rootPool, testTlsChainServerName, clock)
		change(config)
		return config
	}
	cases := []struct {
		description string
		config      *tls.Config
		wantErr     error
	}{
		{description: "skip verify", config: withConfig(func(config *tls.Config) {
			config.InsecureSkipVerify = true
		})},
		{description: "peer verifier", config: withConfig(func(config *tls.Config) {
			config.VerifyPeerCertificate = func([][]byte, [][]*x509.Certificate) error { return refused }
		}), wantErr: refused},
		{description: "connection verifier", config: withConfig(func(config *tls.Config) {
			config.VerifyConnection = func(tls.ConnectionState) error { return refused }
		}), wantErr: refused},
		{description: "unregistered pool", config: withConfig(func(config *tls.Config) {
			config.RootCAs = unregisteredPool
		})},
		{description: "system roots", config: withConfig(func(config *tls.Config) {
			config.RootCAs = nil
		})},
	}
	for _, c := range cases {
		if cache.verification(c.config) != nil {
			t.Fatalf("%s: the cache serves the dial", c.description)
		}
		uncachedState, uncachedErr := testTlsChainDial(t, newTestTlsChainHandshaker(nil), server, c.config)
		cachedState, cachedErr := testTlsChainDial(t, newTestTlsChainHandshaker(cache), server, c.config)
		if c.wantErr != nil && !errors.Is(cachedErr, c.wantErr) {
			t.Fatalf("%s: err = %v, want the verifier's refusal", c.description, cachedErr)
		}
		if (cachedErr == nil) != (uncachedErr == nil) || cachedErr != nil && cachedErr.Error() != uncachedErr.Error() {
			t.Fatalf("%s: cached err = %v, uncached err = %v", c.description, cachedErr, uncachedErr)
		}
		if !testTlsChainRawsEqual(cachedState.VerifiedChains, uncachedState.VerifiedChains) {
			t.Fatalf("%s: the verified chains differ", c.description)
		}
	}
	if cache.verification(withConfig(func(config *tls.Config) { config.ServerName = "" })) != nil {
		t.Fatal("the cache serves a dial with no server name")
	}
	// Go's hello verifies with crypto/tls and has no chain cache
	if newClientTlsHandshaker(TlsClientHelloFingerprintGo, newTestTlsChainConfig(pki.rootPool, testTlsChainServerName, clock)).chainCache != nil {
		t.Fatal("Go's hello verifies through the chain cache")
	}
	assertTestTlsChainStats(t, "ineligible dials", cache, TlsVerifiedChainCacheStats{})
}

// The chains a caller reads are its own: changing them changes neither the
// remembered verification nor what the next dial reads.
func TestTlsChainCacheExposesChainsTheCallerOwns(t *testing.T) {
	now := time.Now()
	clock := newTestTlsChainClock(now)
	pki := newTestTlsChainPki(t, now, true)
	server := newTestTlsChainServer(t, pki.newLeaf(t, testTlsChainServerName, now.Add(-time.Hour), now.Add(time.Hour)), 0)
	cache := newTestTlsChainCache(clock, pki.rootPool)
	config := newTestTlsChainConfig(pki.rootPool, testTlsChainServerName, clock)
	config.SessionTicketsDisabled = true

	uncachedState, _ := testTlsChainDial(t, newTestTlsChainHandshaker(nil), server, config)
	for _, dial := range []string{"miss", "hit"} {
		state, err := testTlsChainDial(t, newTestTlsChainHandshaker(cache), server, config)
		if err != nil {
			t.Fatalf("%s: %s", dial, err)
		}
		if !testTlsChainRawsEqual(state.VerifiedChains, uncachedState.VerifiedChains) {
			t.Fatalf("%s: chains differ from the stack's", dial)
		}
		for _, chain := range state.VerifiedChains {
			for i := range chain {
				chain[i] = nil
			}
		}
	}
	state, err := testTlsChainDial(t, newTestTlsChainHandshaker(cache), server, config)
	if err != nil {
		t.Fatal(err)
	}
	if !testTlsChainRawsEqual(state.VerifiedChains, uncachedState.VerifiedChains) {
		t.Fatal("a caller's change reached the remembered chains")
	}
}

// The cache holds at most its entry bound, dropping the least recently used,
// and a verification answers only for its maximum age on the cache's clock.
func TestTlsChainCacheIsBoundedAndAged(t *testing.T) {
	now := time.Now()
	clock := newTestTlsChainClock(now)
	pki := newTestTlsChainPki(t, now, false)
	settings := defaultTlsVerifiedChainCacheSettings()
	settings.Now = clock.now
	settings.MaxEntryCount = 2
	settings.MaxEntryAge = time.Hour
	cache := newTlsVerifiedChainCache(settings)
	cache.addRootPool(pki.rootPool)
	identity, _ := cache.rootPoolIdentity(pki.rootPool)

	certificates := []tls.Certificate{}
	keys := []tlsVerifiedChainKey{}
	for range 3 {
		certificate := pki.newLeaf(t, testTlsChainServerName, now.Add(-time.Hour), now.Add(time.Hour))
		certificates = append(certificates, certificate)
		keys = append(keys, newTlsVerifiedChainKey(identity, testTlsChainServerName, []*x509.Certificate{certificate.Leaf, pki.intermediate}))
	}
	chainsOf := func(certificate tls.Certificate) [][]*x509.Certificate {
		return [][]*x509.Certificate{{certificate.Leaf, pki.intermediate, pki.root}}
	}
	cache.store(keys[0], chainsOf(certificates[0]))
	cache.store(keys[1], chainsOf(certificates[1]))
	// the first is used, so the second is the least recently used
	if _, ok := cache.chains(keys[0], now); !ok {
		t.Fatal("the first entry does not answer")
	}
	cache.store(keys[2], chainsOf(certificates[2]))
	if _, ok := cache.chains(keys[1], now); ok {
		t.Fatal("the least recently used entry was kept past the bound")
	}
	for _, i := range []int{0, 2} {
		if _, ok := cache.chains(keys[i], now); !ok {
			t.Fatalf("entry %d was dropped", i)
		}
	}
	if stats := cache.stats(); stats.EvictCount != 1 || stats.EntryCount != 2 {
		t.Fatalf("stats = %+v, want one eviction and two entries", stats)
	}

	// an hour after it was stored no entry answers, and each is dropped
	clock.set(now.Add(time.Hour - time.Nanosecond))
	if _, ok := cache.chains(keys[0], now); !ok {
		t.Fatal("an entry younger than the maximum age does not answer")
	}
	clock.set(now.Add(time.Hour))
	for _, i := range []int{0, 2} {
		if _, ok := cache.chains(keys[i], now); ok {
			t.Fatalf("entry %d answers at its maximum age", i)
		}
	}
	if stats := cache.stats(); stats.EntryCount != 0 {
		t.Fatalf("stats = %+v, want the aged entries dropped", stats)
	}

	// a cache that remembers nothing serves no dial
	settings = defaultTlsVerifiedChainCacheSettings()
	settings.MaxEntryCount = 0
	emptyCache := newTlsVerifiedChainCache(settings)
	emptyCache.addRootPool(pki.rootPool)
	if emptyCache.verification(newTestTlsChainConfig(pki.rootPool, testTlsChainServerName, clock)) != nil {
		t.Fatal("a cache bounded to no entries serves a dial")
	}
}

// Concurrent dials of the genuine server and of one holding its chain without
// the key share one cache: every genuine dial is accepted and every impostor
// dial refused. Run with -race.
func TestTlsChainCacheConcurrentDials(t *testing.T) {
	now := time.Now()
	clock := newTestTlsChainClock(now)
	pki := newTestTlsChainPki(t, now, true)
	genuine := pki.newLeaf(t, testTlsChainServerName, now.Add(-time.Hour), now.Add(time.Hour))
	genuineServer := newTestTlsChainServer(t, genuine, 0)
	impostorServer := newTestTlsChainServer(t, testTlsChainImpostor(t, genuine), 0)
	cache := newTestTlsChainCache(clock, pki.rootPool)
	config := newTestTlsChainConfig(pki.rootPool, testTlsChainServerName, clock)

	const goroutineCount = 8
	const dialCount = 6
	var wait sync.WaitGroup
	errs := make(chan error, 2*goroutineCount*dialCount)
	for i := range goroutineCount {
		wait.Add(1)
		go func() {
			defer wait.Done()
			// half the goroutines share one path's session cache
			handshaker := newTestTlsChainHandshaker(cache)
			for j := range dialCount {
				server := genuineServer
				impostor := (i+j)%2 == 1
				if impostor {
					server = impostorServer
				}
				state, err := testTlsChainDial(t, handshaker, server, config)
				switch {
				case impostor && err == nil:
					errs <- fmt.Errorf("goroutine %d dial %d: the impostor was accepted", i, j)
				case !impostor && err != nil:
					errs <- fmt.Errorf("goroutine %d dial %d: %w", i, j, err)
				case !impostor && len(state.VerifiedChains) == 0:
					errs <- fmt.Errorf("goroutine %d dial %d: no verified chains", i, j)
				}
			}
		}()
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if stats := cache.stats(); stats.HitCount == 0 || stats.EntryCount != 1 {
		t.Fatalf("stats = %+v, want reuse of one remembered chain", stats)
	}
}

// Production wiring: connect's pinned roots and the extra-roots pools are
// registered with the process cache, the Chrome hello's handshaker verifies
// through it, and a dial through the normal dialer reuses a verification
// while presenting the same Chrome hello.
func TestTlsChainCacheServesTheDialPaths(t *testing.T) {
	t.Setenv(ExtraRootCAFileEnv, "")
	defaultConfig, err := DefaultTlsConfig()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := processTlsVerifiedChainCache.rootPoolIdentity(defaultConfig.RootCAs); !ok {
		t.Fatal("the pinned roots are not registered")
	}
	pathConfig := newClientTlsConfig(defaultConfig, clientWebSocketNextProtos)
	handshaker := newClientTlsHandshaker("", pathConfig)
	if handshaker.chainCache != processTlsVerifiedChainCache {
		t.Fatal("the Chrome hello does not verify through the process cache")
	}
	dialConfig := pathConfig.Clone()
	dialConfig.ServerName = testTlsChainServerName
	if processTlsVerifiedChainCache.verification(dialConfig) == nil {
		t.Fatal("the process cache does not serve a pinned-root dial")
	}

	caPem, _ := testingPrivatePlatformCertificates(t)
	caFile := filepath.Join(t.TempDir(), "platform-ca.pem")
	if err := os.WriteFile(caFile, caPem, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(ExtraRootCAFileEnv, caFile)
	configuredConfig, err := DefaultTlsConfig()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := processTlsVerifiedChainCache.rootPoolIdentity(configuredConfig.RootCAs); !ok {
		t.Fatal("an extra-roots pool is not registered")
	}
	t.Setenv(ExtraRootCAFileEnv, "")

	// the normal dialer, with the test server's root registered as the pinned
	// roots are
	server := newTestTlsHelloServer(t, testTlsHelloHandler(), nil)
	processTlsVerifiedChainCache.addRootPool(server.rootCertPool)
	t.Cleanup(func() {
		processTlsVerifiedChainCache.testingRemoveRootPool(server.rootCertPool)
	})
	settings := server.clientStrategySettings(t)
	settings.TlsConfig.SessionTicketsDisabled = true
	before := GetTlsVerifiedChainCacheStats()
	dialTlsContext := newNormalDialTlsContext(settings, clientWebSocketNextProtos)
	for range 2 {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		conn, err := dialTlsContext(ctx, "tcp", server.authority())
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if state := testTlsConnectionState(t, conn); len(state.VerifiedChains) == 0 {
			t.Fatal("the dial exposes no verified chains")
		}
		conn.Close()
	}
	after := GetTlsVerifiedChainCacheStats()
	if after.MissCount-before.MissCount != 1 || after.HitCount-before.HitCount != 1 {
		t.Fatalf("stats moved from %+v to %+v, want one miss and one hit", before, after)
	}
	hellos := server.capturedHellos()
	if len(hellos) != 2 {
		t.Fatalf("captured %d hellos, want 2", len(hellos))
	}
	for _, hello := range hellos {
		assertTestChromeClientHello(t, parseTestClientHello(t, hello.message), clientWebSocketNextProtos)
	}
}

// Drops a registration, for a test that registered a pool with the process
// cache.
func (self *tlsVerifiedChainCache) testingRemoveRootPool(rootPool *x509.CertPool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	delete(self.rootPoolIdentities, rootPool)
	self.rootPools = slices.DeleteFunc(self.rootPools, func(registered *x509.CertPool) bool {
		return registered == rootPool
	})
}

// The chain the platform presents (a Let's Encrypt chain: P-256 leaf, P-384
// intermediate, P-384 root cross-signed by the pinned P-384 root), verified as
// uTLS verifies it, against the reuse of its remembered verification.
func BenchmarkTlsChainVerificationFull(b *testing.B) {
	benchmarkTlsChainVerification(b, false)
}

func BenchmarkTlsChainVerificationReused(b *testing.B) {
	benchmarkTlsChainVerification(b, true)
}

func benchmarkTlsChainVerification(b *testing.B, reuse bool) {
	now := time.Now()
	clock := newTestTlsChainClock(now)
	pki := newTestTlsChainPki(b, now, true)
	certificate := pki.newLeaf(b, testTlsChainServerName, now.Add(-time.Hour), now.Add(time.Hour))
	state := testTlsChainBenchmarkState(b, certificate)
	var cache *tlsVerifiedChainCache
	if reuse {
		cache = newTlsVerifiedChainCache(defaultTlsVerifiedChainCacheSettings())
	} else {
		// a cache that remembers nothing: every verification is a full one
		settings := defaultTlsVerifiedChainCacheSettings()
		settings.MaxEntryAge = time.Nanosecond
		settings.Now = clock.now
		cache = newTlsVerifiedChainCache(settings)
	}
	cache.addRootPool(pki.rootPool)
	config := newTestTlsChainConfig(pki.rootPool, testTlsChainServerName, clock)
	if err := cache.verification(config).verifyConnection(state); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if !reuse {
			// past the age of anything remembered
			clock.set(clock.now().Add(time.Nanosecond))
		}
		if err := cache.verification(config).verifyConnection(state); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	stats := cache.stats()
	if reuse && stats.MissCount != 1 || !reuse && stats.HitCount != 0 {
		b.Fatalf("stats = %+v", stats)
	}
}

// The state uTLS hands the verifier: the presented certificates, parsed.
func testTlsChainBenchmarkState(b *testing.B, certificate tls.Certificate) utls.ConnectionState {
	b.Helper()
	peerCertificates := []*x509.Certificate{}
	for _, der := range certificate.Certificate {
		parsed, err := x509.ParseCertificate(der)
		if err != nil {
			b.Fatal(err)
		}
		peerCertificates = append(peerCertificates, parsed)
	}
	return utls.ConnectionState{PeerCertificates: peerCertificates}
}

// Full Chrome-hello handshakes over loopback (no resumption), client and
// server in this process, with and without a reused chain verification.
func BenchmarkChromeTlsHandshakeChainVerified(b *testing.B) {
	benchmarkChromeTlsHandshake(b, false)
}

func BenchmarkChromeTlsHandshakeChainReused(b *testing.B) {
	benchmarkChromeTlsHandshake(b, true)
}

func benchmarkChromeTlsHandshake(b *testing.B, reuse bool) {
	now := time.Now()
	clock := newTestTlsChainClock(now)
	pki := newTestTlsChainPki(b, now, true)
	server := newTestTlsChainServer(b, pki.newLeaf(b, testTlsChainServerName, now.Add(-time.Hour), now.Add(time.Hour)), 0)
	var cache *tlsVerifiedChainCache
	if reuse {
		cache = newTlsVerifiedChainCache(defaultTlsVerifiedChainCacheSettings())
		cache.addRootPool(pki.rootPool)
	}
	handshaker := newTestTlsChainHandshaker(cache)
	config := newTestTlsChainConfig(pki.rootPool, testTlsChainServerName, clock)
	config.SessionTicketsDisabled = true
	if _, err := testTlsChainDial(b, handshaker, server, config); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := testTlsChainDial(b, handshaker, server, config); err != nil {
			b.Fatal(err)
		}
	}
}
