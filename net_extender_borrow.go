package connect

import (
	"context"
	"crypto/ecdh"
	"crypto/tls"
	"embed"
	"io/fs"
	"net"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	utls "github.com/refraction-networking/utls"
)

// The bundled borrowed-names list of the camouflage splice (EXTENDER.md P5).
//
// The splice relays an unauthenticated hello to the real site named in it, so a
// borrowed name must be a site that is actually reachable, speaks tls 1.3,
// offers an X25519 or X25519MLKEM768 key share, and is plausibly hosted on an
// arbitrary address rather than pinned to a well-known cdn range. The spoof
// list (A10) is the wrong list for this: those are big-site names whose
// addresses a censor knows, so an extender on a home address claiming one is an
// obvious sni-to-ip mismatch. The borrow list is therefore separate, in the
// same xor-masked gzip form as the spoof resource and with the same per-country
// override, curated for splice-friendliness.
//
// The client draws its front sni from it and the server validates and splices
// to it from one shared source, so there is nothing per-extender to agree on.
// At role start the extender verifies each candidate it would use
// (VerifyExtenderBorrowDomain) and keeps only the ones that pass; an extender
// with none verified publishes no camouflage key and serves the legacy
// terminate path alone, so a bad or empty list degrades to today rather than
// breaking.
//
// No borrowed list is bundled yet: like the country spoof lists, what is
// splice-friendly in a region has to be measured before its list can be
// written. The bundled resource therefore decodes to an empty list, which is
// the deliberate degrade-to-legacy fallback, not a defect.

// The borrow resource shares the spoof resource's mask size and codec
// (EncodeSpoofDomainsResource / DecodeSpoofDomainsResource).
//
//go:embed res/extender_borrow*.bin
var extenderBorrowResources embed.FS

// The bundled global borrow list in its resource form.
var extenderBorrowResource, _ = extenderBorrowResources.ReadFile(BorrowResourcePath(""))

var (
	borrowDomainsOnce      sync.Once
	borrowDomainsStateLock sync.Mutex
	borrowDomainsValues    []string
	// the files the country lists are read from: the embedded resources, unless
	// a test installed others
	borrowCountryResources fs.FS = extenderBorrowResources
	// the decoded country lists by country code, read on first use
	borrowCountryDomainsValues = map[string][]string{}
)

// BorrowDomains is the bundled global borrow list, decoded on first use. The
// result is a copy the caller may keep. An unreadable or empty resource yields
// an empty list, which degrades the splice to the legacy terminate path (P5).
func BorrowDomains() []string {
	borrowDomainsOnce.Do(func() {
		borrowDomains, err := DecodeSpoofDomainsResource(extenderBorrowResource)
		if err != nil {
			borrowDomains = []string{}
		}
		borrowDomainsStateLock.Lock()
		defer borrowDomainsStateLock.Unlock()
		if borrowDomainsValues == nil {
			borrowDomainsValues = borrowDomains
		}
	})
	borrowDomainsStateLock.Lock()
	defer borrowDomainsStateLock.Unlock()
	return slices.Clone(borrowDomainsValues)
}

// Installs a synthetic list in place of the bundled one and returns the
// restore. Tests use it so no real domain appears in the repository.
func setBorrowDomainsForTest(borrowDomains []string) func() {
	// load the bundled list first, so the restore puts the real list back (see
	// setSpoofDomainsForTest for why the once must be taken with it loaded)
	BorrowDomains()
	borrowDomainsStateLock.Lock()
	defer borrowDomainsStateLock.Unlock()
	previousBorrowDomains := borrowDomainsValues
	borrowDomainsValues = slices.Clone(borrowDomains)
	return func() {
		borrowDomainsStateLock.Lock()
		defer borrowDomainsStateLock.Unlock()
		borrowDomainsValues = previousBorrowDomains
	}
}

// The resource path of a country's borrow list, `res/extender_borrow_<cc>.bin`,
// and of the global list, `res/extender_borrow.bin`.
// Testing_SetBorrowDomains installs a synthetic global borrow list in place of
// the bundled one and returns the restore, for tests in other packages (the
// extender fixture) that drive the client's front-name selection. The bundled
// list is empty, so a test must install one to exercise the camouflaged dial.
func Testing_SetBorrowDomains(borrowDomains []string) func() {
	return setBorrowDomainsForTest(borrowDomains)
}

func BorrowResourcePath(countryCode string) string {
	if countryCode = NormalizeSpoofCountryCode(countryCode); countryCode != "" {
		return "res/extender_borrow_" + countryCode + ".bin"
	}
	return "res/extender_borrow.bin"
}

// BorrowDomainsForCountry is the borrow list of one country: its own bundled
// list when it has one, and the global list otherwise, exactly as
// SpoofDomainsForCountry (P5). The result is a copy the caller may keep.
func BorrowDomainsForCountry(countryCode string) []string {
	if countryCode = NormalizeSpoofCountryCode(countryCode); countryCode != "" {
		if borrowDomains := countryBorrowDomains(countryCode); 0 < len(borrowDomains) {
			return borrowDomains
		}
	}
	return BorrowDomains()
}

// A country's bundled borrow list, decoded on first use; empty when there is
// none. The code is already normalized.
func countryBorrowDomains(countryCode string) []string {
	borrowDomainsStateLock.Lock()
	defer borrowDomainsStateLock.Unlock()

	borrowDomains, ok := borrowCountryDomainsValues[countryCode]
	if !ok {
		borrowDomains = []string{}
		if resource, err := fs.ReadFile(borrowCountryResources, BorrowResourcePath(countryCode)); err == nil {
			if decodedBorrowDomains, err := DecodeSpoofDomainsResource(resource); err == nil {
				borrowDomains = decodedBorrowDomains
			}
		}
		borrowCountryDomainsValues[countryCode] = borrowDomains
	}
	return slices.Clone(borrowDomains)
}

// Installs other files in place of the embedded country lists and returns the
// restore, exactly as setSpoofCountryResourcesForTest.
func setBorrowCountryResourcesForTest(resources fs.FS) func() {
	borrowDomainsStateLock.Lock()
	defer borrowDomainsStateLock.Unlock()
	previousResources := borrowCountryResources
	previousBorrowDomainsValues := borrowCountryDomainsValues
	borrowCountryResources = resources
	borrowCountryDomainsValues = map[string][]string{}
	return func() {
		borrowDomainsStateLock.Lock()
		defer borrowDomainsStateLock.Unlock()
		borrowCountryResources = previousResources
		borrowCountryDomainsValues = previousBorrowDomainsValues
	}
}

// VerifyExtenderBorrowDomain reports whether a candidate borrowed name is a
// splice-friendly front (P5): a tls 1.3 dial that reaches an X25519 or
// X25519MLKEM768 key share and whose peer is not a shared cdn. The role runs it
// at start over its own egress (dialContext, family-narrowed) and keeps only
// the names that pass, so the splice only ever relays to a reachable modern-tls
// site. A dial failure, a pre-1.3 server, a non-X25519 group, or a cdn peer
// each return false, which drops the name.
//
// The cdn check is deliberately conservative: it rejects a peer whose verified
// chain names a known shared-cdn organization, since such a name pinned to an
// arbitrary extender address is the sni-to-ip tell the borrow list exists to
// avoid. isCdn, when nil, accepts every reachable modern-tls peer.
func VerifyExtenderBorrowDomain(
	ctx context.Context,
	dialContext DialContextFunction,
	network string,
	borrowDomain string,
	timeout time.Duration,
	isCdn func(leaf *tls.Certificate, serverName string) bool,
) bool {
	borrowDomain = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(borrowDomain), "."))
	if borrowDomain == "" || strings.ContainsAny(borrowDomain, "/ \t") {
		return false
	}
	dialCtx := ctx
	if 0 < timeout {
		var cancel context.CancelFunc
		dialCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	conn, err := dialContext(dialCtx, network, net.JoinHostPort(borrowDomain, "443"))
	if err != nil || conn == nil {
		if conn != nil {
			conn.Close()
		}
		return false
	}
	defer conn.Close()

	// a faithful browser hello, so the candidate answers as it would a real
	// visit. the chain is not verified: the extender only relays the prober's
	// bytes to the site and the prober is the one that validates the site's
	// (ca-valid) certificate; the extender reads the leaf only for the cdn check
	uconn := utls.UClient(conn, &utls.Config{
		ServerName:         borrowDomain,
		InsecureSkipVerify: true,
	}, utls.HelloChrome_Auto)
	defer uconn.Close()
	if err := uconn.HandshakeContext(dialCtx); err != nil {
		return false
	}
	state := uconn.ConnectionState()
	if state.Version != tls.VersionTLS13 {
		return false
	}
	// the negotiated group must be one the browser hello shares an X25519
	// ephemeral for, so the spliced handshake is as modern as the hello that
	// fronted it
	if !extenderBorrowGroupIsX25519(uconn) {
		return false
	}
	if isCdn != nil && len(state.PeerCertificates) != 0 {
		leaf := &tls.Certificate{Leaf: state.PeerCertificates[0]}
		if isCdn(leaf, borrowDomain) {
			return false
		}
	}
	return true
}

// Whether the handshake settled on an X25519 or X25519MLKEM768 group, read from
// the key share the client offered and the server selected.
func extenderBorrowGroupIsX25519(uconn *utls.UConn) bool {
	serverHello := uconn.HandshakeState.ServerHello
	if serverHello != nil && serverHello.ServerShare.Group != 0 {
		switch uint16(serverHello.ServerShare.Group) {
		case ExtenderRealityGroupX25519, ExtenderRealityGroupX25519Mlkem768:
			return true
		default:
			return false
		}
	}
	// no selected group visible (an older handshake record shape); fall back to
	// whether the client even offered an X25519 ephemeral, which a Chrome hello
	// always does
	keyShareKeys := uconn.HandshakeState.State13.KeyShareKeys
	if keyShareKeys == nil {
		return false
	}
	return hasX25519Ecdhe(keyShareKeys)
}

// Whether a key-share set carries an X25519 ephemeral, either plain or inside
// the hybrid.
func hasX25519Ecdhe(keyShareKeys *utls.KeySharePrivateKeys) bool {
	for _, key := range []*ecdh.PrivateKey{keyShareKeys.Ecdhe, keyShareKeys.MlkemEcdhe} {
		if key != nil && key.Curve() == ecdh.X25519() {
			return true
		}
	}
	return false
}

// Every bundled borrow name: the global list, then each country's list in
// country order, deduplicated. The server validates and splices to any of them.
func AllBorrowDomains() []string {
	allBorrowDomains := BorrowDomains()
	visited := map[string]bool{}
	for _, borrowDomain := range allBorrowDomains {
		visited[borrowDomain] = true
	}
	for _, countryCode := range borrowCountryCodes() {
		for _, borrowDomain := range countryBorrowDomains(countryCode) {
			if !visited[borrowDomain] {
				visited[borrowDomain] = true
				allBorrowDomains = append(allBorrowDomains, borrowDomain)
			}
		}
	}
	return allBorrowDomains
}

// The countries with a bundled borrow list, in order.
func borrowCountryCodes() []string {
	resources := func() fs.FS {
		borrowDomainsStateLock.Lock()
		defer borrowDomainsStateLock.Unlock()
		return borrowCountryResources
	}()
	resourceDir := path.Dir(BorrowResourcePath(""))
	entries, err := fs.ReadDir(resources, resourceDir)
	if err != nil {
		return []string{}
	}
	countryCodes := []string{}
	for _, entry := range entries {
		countryCode := strings.TrimSuffix(strings.TrimPrefix(entry.Name(), "extender_borrow_"), ".bin")
		if countryCode == "" || NormalizeSpoofCountryCode(countryCode) != countryCode ||
			path.Join(resourceDir, entry.Name()) != BorrowResourcePath(countryCode) {
			continue
		}
		countryCodes = append(countryCodes, countryCode)
	}
	return countryCodes
}
