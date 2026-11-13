package connect

// net_tls_chain_cache.go — the verified-chain cache of the Chrome hello's
// pinned-root dials.
//
// A platform dial verifies the server's certificate chain against connect's
// pinned roots. For the Let's Encrypt chain that is about four P-384 ecdsa
// verifications and one rsa verification per handshake, and Go's P-384 has no
// assembly path, so chain verification is about a quarter of a probe
// process's CPU. The chain is the same on every dial to the platform's names,
// so one exact chain's verification under one exact root pool is remembered
// and reused.
//
// The cache remembers only a verification that succeeded, keyed by the
// presented certificates byte for byte, the server name, and the identity of
// the root pool it ran under, with the chains it built. Every handshake still
// checks, cached or not:
//   - key possession: the server's signature with the leaf key (the tls 1.3
//     CertificateVerify, the tls 1.2 ServerKeyExchange) or the rsa key
//     transport, and the Finished macs. The tls stack checks these after this
//     verification, on every handshake.
//   - the server name, against the leaf (`x509.Certificate.VerifyHostname`).
//   - the validity time: a remembered chain answers only while the dial's
//     clock is inside every certificate's validity window, as Go compares it.
//
// Anything else is a miss, which verifies in full exactly as the tls stack
// would (uTLS `verifyServerCertificate`) and returns its errors: a different
// byte in any certificate, another server name, another root pool, a window
// that no longer covers the dial's clock, an entry past its maximum age.
// Errors are never remembered, so a refusal always comes from a full
// verification.
//
// A resumed handshake runs the same verification, on the session's
// certificates. The tls stack itself does not verify a resumed session's
// chain again (it checks only the leaf's expiry and the name), so a resumed
// handshake is refused where its chain no longer verifies at the dial's clock,
// as a full handshake would be at that time, where the stack would have
// resumed.
//
// Eligible are the Chrome hello's dials whose configuration verifies against a
// root pool registered here (connect's pinned roots: `sharedDefaultPinnedCertPool`
// and the extra-roots pools of `defaultRootCertPool`), with no verification
// callback of its own, verification not skipped, and a server name. Everything
// else verifies as before: system roots, a caller's pool, the extender and
// vless configurations, custom verifiers, Go's hello and h3. The probe's
// target tls (the exit's) is none of these dials.
//
// A registered pool's identity is its registration, not its contents. A
// CertPool can only gain certificates, and a pool's remembered verifications
// answer only dials that verify against that same pool, so a pool changed after
// registration still answers as the full verification would: a chain that
// verified before the change verifies after it.
//
// The cache is process-wide. It holds facts about public certificate bytes
// under a root pool, the same for every owner of the process, and nothing on
// the wire depends on it. A short-lived owner (a probe tunnel) would start a
// cache of its own cold on every dial path, which is the cost it removes.
//
// Safe for concurrent use.

import (
	"container/list"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"slices"
	"sync"
	"time"

	utls "github.com/refraction-networking/utls"
)

// tlsVerifiedChainCacheSettings tunes a verified-chain cache.
type tlsVerifiedChainCacheSettings struct {
	// MaxEntryCount bounds the remembered verifications; storing past it drops
	// the least recently used. 256: the platform presents a handful of chains
	// (one per certificate and name), and an entry holds its parsed chains, a
	// few KiB. Non-positive remembers nothing, and every dial verifies as the
	// tls stack does.
	MaxEntryCount int
	// MaxEntryAge bounds how long a verification is reused after it was made,
	// on the cache's clock, whatever the certificates' validity. 1h: a renewed
	// certificate or a changed chain is new bytes, a miss at once; the age only
	// bounds how stale the set of chains a verification built can grow (a chain
	// that becomes valid later is not added). Non-positive remembers nothing.
	MaxEntryAge time.Duration
	// MaxRootPoolCount bounds the registered root pools; registering past it
	// drops the oldest registration, whose dials then verify as the tls stack
	// does. 16: a process registers one, the pinned roots, unless it adds roots
	// from a file (`ExtraRootCAFileEnv`), which registers one pool per
	// configuration built. Non-positive registers none.
	MaxRootPoolCount int
	// Now is the cache's clock, for entry age. A chain's validity is judged on
	// the dial's own clock (`tls.Config.Time`), as the tls stack judges it.
	Now func() time.Time
}

func defaultTlsVerifiedChainCacheSettings() *tlsVerifiedChainCacheSettings {
	return &tlsVerifiedChainCacheSettings{
		MaxEntryCount:    256,
		MaxEntryAge:      1 * time.Hour,
		MaxRootPoolCount: 16,
		Now:              time.Now,
	}
}

// processTlsVerifiedChainCache verifies the Chrome hello's dials of this
// process (`newClientTlsHandshaker`), and holds the pinned root pools
// connect builds.
var processTlsVerifiedChainCache = newTlsVerifiedChainCache(defaultTlsVerifiedChainCacheSettings())

// TlsVerifiedChainCacheStats counts the process verified-chain cache's work
// since the process started (net_tls_chain_cache.go).
type TlsVerifiedChainCacheStats struct {
	// eligible handshakes that reused a remembered verification
	HitCount uint64
	// eligible handshakes that verified in full
	MissCount uint64
	// verifications remembered
	StoreCount uint64
	// remembered verifications dropped to stay within the entry bound
	EvictCount uint64
	// remembered verifications held now
	EntryCount int
}

// GetTlsVerifiedChainCacheStats returns the process verified-chain cache's
// counts.
func GetTlsVerifiedChainCacheStats() TlsVerifiedChainCacheStats {
	return processTlsVerifiedChainCache.stats()
}

// The key of one verification: a digest of the root pool's identity, the
// server name and every presented certificate, each length prefixed, so two
// different inputs never share the encoding the digest covers.
type tlsVerifiedChainKey [sha256.Size]byte

func newTlsVerifiedChainKey(rootPoolIdentity uint64, serverName string, certificates []*x509.Certificate) tlsVerifiedChainKey {
	hash := sha256.New()
	var numberBytes [8]byte
	writeNumber := func(number uint64) {
		binary.BigEndian.PutUint64(numberBytes[:], number)
		hash.Write(numberBytes[:])
	}
	hash.Write([]byte("urnetwork tls verified chain v1"))
	writeNumber(rootPoolIdentity)
	writeNumber(uint64(len(serverName)))
	hash.Write([]byte(serverName))
	writeNumber(uint64(len(certificates)))
	for _, certificate := range certificates {
		writeNumber(uint64(len(certificate.Raw)))
		hash.Write(certificate.Raw)
	}
	var key tlsVerifiedChainKey
	hash.Sum(key[:0])
	return key
}

// One chain a verification built, leaf first and root last, with the window
// in which every certificate of it is valid: the latest NotBefore and the
// earliest NotAfter.
type tlsVerifiedChain struct {
	certificates []*x509.Certificate
	notBefore    time.Time
	notAfter     time.Time
}

// Valid at now as x509 compares a certificate's validity: not before
// NotBefore and not after NotAfter, for every certificate of the chain.
func (self *tlsVerifiedChain) validAt(now time.Time) bool {
	return !now.Before(self.notBefore) && !now.After(self.notAfter)
}

// One remembered verification.
type tlsVerifiedChainEntry struct {
	key    tlsVerifiedChainKey
	chains []*tlsVerifiedChain
	// on the cache's clock
	storeTime time.Time
}

type tlsVerifiedChainCache struct {
	settings *tlsVerifiedChainCacheSettings

	stateLock sync.Mutex
	// remembered verifications, most recently used first; each value is a
	// *tlsVerifiedChainEntry
	entries    *list.List
	keyEntries map[tlsVerifiedChainKey]*list.Element
	// the registered root pools, with the identity each verifies under
	rootPoolIdentities map[*x509.CertPool]uint64
	// the registered root pools, oldest registration first
	rootPools            []*x509.CertPool
	nextRootPoolIdentity uint64
	hitCount             uint64
	missCount            uint64
	storeCount           uint64
	evictCount           uint64
}

func newTlsVerifiedChainCache(settings *tlsVerifiedChainCacheSettings) *tlsVerifiedChainCache {
	return &tlsVerifiedChainCache{
		settings:           settings,
		entries:            list.New(),
		keyEntries:         map[tlsVerifiedChainKey]*list.Element{},
		rootPoolIdentities: map[*x509.CertPool]uint64{},
	}
}

// Registers a root pool connect built, so the dials that verify against it are
// eligible. A pool registered again keeps its identity.
func (self *tlsVerifiedChainCache) addRootPool(rootPool *x509.CertPool) {
	if rootPool == nil || self.settings.MaxRootPoolCount <= 0 {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if _, ok := self.rootPoolIdentities[rootPool]; ok {
		return
	}
	for self.settings.MaxRootPoolCount <= len(self.rootPools) {
		delete(self.rootPoolIdentities, self.rootPools[0])
		self.rootPools = slices.Delete(self.rootPools, 0, 1)
	}
	// identities are never reused, so a pool registered after its drop gets a
	// new one, and no verification made under another registration answers it
	self.nextRootPoolIdentity += 1
	self.rootPoolIdentities[rootPool] = self.nextRootPoolIdentity
	self.rootPools = append(self.rootPools, rootPool)
}

// The identity of a registered root pool. ok is false for every other pool,
// and for nil (the system roots).
func (self *tlsVerifiedChainCache) rootPoolIdentity(rootPool *x509.CertPool) (identity uint64, ok bool) {
	if rootPool == nil {
		return 0, false
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	identity, ok = self.rootPoolIdentities[rootPool]
	return
}

// The chain verification of one dial with config, or nil when the cache does
// not serve the dial: verification skipped, or made by the caller's own
// callbacks, against a pool that is not registered (the system roots among
// them), or for no server name, which the tls stack refuses unless
// verification is skipped. Nil too when the cache remembers nothing.
func (self *tlsVerifiedChainCache) verification(config *tls.Config) *tlsChainVerification {
	if self == nil || self.settings.MaxEntryCount <= 0 || self.settings.MaxEntryAge <= 0 {
		return nil
	}
	if config.InsecureSkipVerify || config.VerifyPeerCertificate != nil || config.VerifyConnection != nil || config.ServerName == "" {
		return nil
	}
	rootPoolIdentity, ok := self.rootPoolIdentity(config.RootCAs)
	if !ok {
		return nil
	}
	return &tlsChainVerification{
		cache:            self,
		rootPoolIdentity: rootPoolIdentity,
		rootPool:         config.RootCAs,
		serverName:       config.ServerName,
		time:             config.Time,
	}
}

// The chains of the remembered verification of key that are valid at now, the
// dial's clock, in copies the caller owns. ok is false on a miss: no entry, an
// entry past its maximum age, or no chain valid at now.
func (self *tlsVerifiedChainCache) chains(key tlsVerifiedChainKey, now time.Time) (chains [][]*x509.Certificate, ok bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	element := self.keyEntries[key]
	if element == nil {
		self.missCount += 1
		return nil, false
	}
	entry := element.Value.(*tlsVerifiedChainEntry)
	if !self.settings.Now().Before(entry.storeTime.Add(self.settings.MaxEntryAge)) {
		self.entries.Remove(element)
		delete(self.keyEntries, key)
		self.missCount += 1
		return nil, false
	}
	for _, chain := range entry.chains {
		if chain.validAt(now) {
			chains = append(chains, slices.Clone(chain.certificates))
		}
	}
	if len(chains) == 0 {
		// the entry may answer again at another clock reading, so it stays
		self.missCount += 1
		return nil, false
	}
	self.entries.MoveToFront(element)
	self.hitCount += 1
	return chains, true
}

// Remembers a verification that succeeded, with a copy of the chains it built.
func (self *tlsVerifiedChainCache) store(key tlsVerifiedChainKey, chains [][]*x509.Certificate) {
	entry := &tlsVerifiedChainEntry{
		key: key,
	}
	for _, certificates := range chains {
		if len(certificates) == 0 {
			continue
		}
		chain := &tlsVerifiedChain{
			certificates: slices.Clone(certificates),
			notBefore:    certificates[0].NotBefore,
			notAfter:     certificates[0].NotAfter,
		}
		for _, certificate := range certificates[1:] {
			if chain.notBefore.Before(certificate.NotBefore) {
				chain.notBefore = certificate.NotBefore
			}
			if certificate.NotAfter.Before(chain.notAfter) {
				chain.notAfter = certificate.NotAfter
			}
		}
		entry.chains = append(entry.chains, chain)
	}
	if len(entry.chains) == 0 {
		return
	}

	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	entry.storeTime = self.settings.Now()
	if element := self.keyEntries[key]; element != nil {
		element.Value = entry
		self.entries.MoveToFront(element)
	} else {
		self.keyEntries[key] = self.entries.PushFront(entry)
	}
	self.storeCount += 1
	for self.settings.MaxEntryCount < self.entries.Len() {
		oldest := self.entries.Back()
		self.entries.Remove(oldest)
		delete(self.keyEntries, oldest.Value.(*tlsVerifiedChainEntry).key)
		self.evictCount += 1
	}
}

func (self *tlsVerifiedChainCache) stats() TlsVerifiedChainCacheStats {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return TlsVerifiedChainCacheStats{
		HitCount:   self.hitCount,
		MissCount:  self.missCount,
		StoreCount: self.storeCount,
		EvictCount: self.evictCount,
		EntryCount: self.entries.Len(),
	}
}

// One handshake's chain verification on a dial the cache serves
// (`tlsVerifiedChainCache.verification`). The tls stack calls verifyConnection
// in place of its own chain verification, on a full handshake and a resumed
// one alike, from the handshaking goroutine.
type tlsChainVerification struct {
	cache            *tlsVerifiedChainCache
	rootPoolIdentity uint64
	rootPool         *x509.CertPool
	serverName       string
	// the dial's clock (`tls.Config.Time`); nil is the wall clock
	time func() time.Time

	// the chains the handshake verified, set when verifyConnection accepts
	verifiedChains [][]*x509.Certificate
}

// Accepts the presented chain when a remembered verification of exactly these
// certificates, this name and this root pool is valid at the dial's clock and
// the leaf holds the name, and otherwise verifies it as the tls stack would
// (uTLS `verifyServerCertificate`, whose fips chain filter is a no-op in uTLS),
// returning its error type. Key possession is the tls stack's to check next.
func (self *tlsChainVerification) verifyConnection(state utls.ConnectionState) error {
	certificates := state.PeerCertificates
	if len(certificates) == 0 {
		// the tls stack refuses an empty certificate message before this
		return &utls.CertificateVerificationError{Err: errors.New("tls: the server presented no certificate")}
	}
	now := time.Now()
	if self.time != nil {
		now = self.time()
	}
	key := newTlsVerifiedChainKey(self.rootPoolIdentity, self.serverName, certificates)
	if chains, ok := self.cache.chains(key, now); ok {
		// the key binds the name, so a sound entry always holds it; it is
		// checked on every handshake regardless
		if err := certificates[0].VerifyHostname(self.serverName); err != nil {
			return &utls.CertificateVerificationError{UnverifiedCertificates: certificates, Err: err}
		}
		self.verifiedChains = chains
		return nil
	}
	options := x509.VerifyOptions{
		Roots:         self.rootPool,
		CurrentTime:   now,
		DNSName:       self.serverName,
		Intermediates: x509.NewCertPool(),
	}
	for _, certificate := range certificates[1:] {
		options.Intermediates.AddCert(certificate)
	}
	chains, err := certificates[0].Verify(options)
	if err != nil {
		return &utls.CertificateVerificationError{UnverifiedCertificates: certificates, Err: err}
	}
	self.cache.store(key, chains)
	self.verifiedChains = chains
	return nil
}
