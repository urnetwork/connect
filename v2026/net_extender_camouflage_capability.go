package connect

// net_extender_camouflage_capability.go — the client capability of the tcp
// camouflage rollout (EXTENDER-UPDATE2 §3.2).
//
// A Phase B extender splices every unauthenticated tcp hello to its borrowed
// site, so a client that dials it with a legacy hello loses the tcp carrier: the
// spliced handshake fails the B3 leaf check, and the camouflaged-then-legacy
// race is no recovery. The release therefore issues a Phase B extender's record
// only to a client that says it sends authenticated hellos, through the request
// capability tcp-camouflage-v1, and the record names the capabilities it needs
// (RequiredCapabilities, signed). Capabilities change eligibility only, never
// the disclosure budget, the partition or the identity.
//
// The client claims the capability only when every camouflaged dial it would
// make to such an extender authenticates and fronts a name the cohort covers:
//
//   - The fingerprint switch selects the Chrome hello ("" or "chrome"). A "go"
//     client sends the Go-TLS hello alone, which a Phase B extender splices
//     away, and an unknown value is not the Chrome hello the camouflage is.
//   - Its borrow list for its country names at least one selectable name. With
//     none the camouflaged attempt has no front name and is dropped.
//   - That list is measured, in a manifest this build understands, and within
//     its maximum measurement age. An unmeasured or stale list may front names
//     the cohort no longer covers.
//
// The record's own camouflage key is not an input. A record that requires the
// capability always carries its RealityPublicKey (the issuer adds the
// requirement only beside a key), and the client carries every verified
// record's key into its tcp dial (extenderConfigsForCandidate).

import (
	"slices"
	"strings"
	"time"
)

// The release request capability of a client that sends authenticated tcp
// camouflage hellos and works with Phase B spliced extenders (§3.2).
const ExtenderCapabilityTcpCamouflageV1 = "tcp-camouflage-v1"

// The capabilities this build knows. A requirement outside it is never
// satisfied, so the release withholds a record gated on a later capability from
// a client that cannot meet it. The check is the release's alone: a client does
// not re-read a record's requirement when it dials. One that has since lost the
// capability, by a fingerprint switch to "go" say, fails that dial against a
// splicing extender, and the gated attempt's bound falls back to open records.
var extenderKnownCapabilities = []string{
	ExtenderCapabilityTcpCamouflageV1,
}

// The most offered capabilities a check reads, and the longest one, so a
// hostile list is bounded work. A longer list keeps its first values.
const (
	extenderMaxOfferedCapabilityCount     = 16
	extenderMaxOfferedCapabilityByteCount = 64
)

// The reasons of ExtenderTcpCamouflageReadinessResult, a fixed set for status
// and telemetry.
const (
	ExtenderTcpCamouflageReadyOk                     = "ok"
	ExtenderTcpCamouflageReadyFingerprintOverride    = "fingerprint_override"
	ExtenderTcpCamouflageReadyNoBorrowList           = "no_borrow_list"
	ExtenderTcpCamouflageReadyBorrowListIncompatible = "borrow_list_incompatible"
	ExtenderTcpCamouflageReadyBorrowListStale        = "borrow_list_stale"
)

// ExtenderTcpCamouflageReadinessResult is whether a client may claim
// tcp-camouflage-v1, and why not when it may not.
type ExtenderTcpCamouflageReadinessResult struct {
	Capable bool
	// One of the ExtenderTcpCamouflageReady* reasons.
	Reason string
	// The version of the borrow list the decision read, zero when there is
	// none or it is unversioned.
	BorrowListVersion int
}

// Whether a fingerprint setting selects the Chrome hello the camouflaged attempt
// presents. Shared by the dial (extenderCamouflageApplies) and the capability,
// so a client never claims a capability its dial would not exercise.
func extenderCamouflageFingerprintAllows(fingerprint string) bool {
	switch fingerprint {
	case "", TlsClientHelloFingerprintChrome:
		return true
	default:
		return false
	}
}

// ExtenderTcpCamouflageReadiness decides whether a client in countryCode may
// claim tcp-camouflage-v1 (see the file comment). countryCode is the country
// the directory places the client in (SpoofCountryCode), "" for the global list.
func ExtenderTcpCamouflageReadiness(connectSettings *ConnectSettings, countryCode string) ExtenderTcpCamouflageReadinessResult {
	return extenderTcpCamouflageReadinessAt(connectSettings, countryCode, time.Now())
}

func extenderTcpCamouflageReadinessAt(connectSettings *ConnectSettings, countryCode string, now time.Time) ExtenderTcpCamouflageReadinessResult {
	fingerprint := ""
	if connectSettings != nil {
		fingerprint = connectSettings.TlsClientHelloFingerprint
	}
	if !extenderCamouflageFingerprintAllows(fingerprint) {
		return ExtenderTcpCamouflageReadinessResult{Reason: ExtenderTcpCamouflageReadyFingerprintOverride}
	}
	borrowList := ExtenderCamouflageBorrowListForCountry(countryCode)
	if borrowList == nil || len(borrowList.SelectableNames(borrowList.Version)) == 0 {
		return ExtenderTcpCamouflageReadinessResult{Reason: ExtenderTcpCamouflageReadyNoBorrowList}
	}
	result := ExtenderTcpCamouflageReadinessResult{BorrowListVersion: borrowList.Version}
	if !borrowList.Measured || borrowList.Version <= 0 {
		result.Reason = ExtenderTcpCamouflageReadyBorrowListIncompatible
		return result
	}
	if borrowList.stale(now) {
		result.Reason = ExtenderTcpCamouflageReadyBorrowListStale
		return result
	}
	result.Capable = true
	result.Reason = ExtenderTcpCamouflageReadyOk
	return result
}

// ExtenderClientCapabilities is the capability list a client sends in its
// release request (§1.1, §3.2): tcp-camouflage-v1 when the client is ready for
// Phase B extenders, else empty. The list is a fresh slice the caller may keep.
func ExtenderClientCapabilities(connectSettings *ConnectSettings, countryCode string) []string {
	if ExtenderTcpCamouflageReadiness(connectSettings, countryCode).Capable {
		return []string{ExtenderCapabilityTcpCamouflageV1}
	}
	return []string{}
}

// ExtenderCapabilitiesSatisfied reports whether the offered capabilities meet
// every required one. A required capability this build does not know is never
// met; an offered value this build does not know is ignored. Values compare
// trimmed and case-insensitively, and only the first
// extenderMaxOfferedCapabilityCount offered values of at most
// extenderMaxOfferedCapabilityByteCount bytes are read. No requirement is
// always met.
func ExtenderCapabilitiesSatisfied(required []string, offered []string) bool {
	offeredCapabilities := map[string]bool{}
	for i, capability := range offered {
		if extenderMaxOfferedCapabilityCount <= i {
			break
		}
		if extenderMaxOfferedCapabilityByteCount < len(capability) {
			continue
		}
		offeredCapabilities[normalizeExtenderCapability(capability)] = true
	}
	for _, capability := range required {
		capability = normalizeExtenderCapability(capability)
		if capability == "" {
			continue
		}
		if !slices.Contains(extenderKnownCapabilities, capability) || !offeredCapabilities[capability] {
			return false
		}
	}
	return true
}

func normalizeExtenderCapability(capability string) string {
	return strings.ToLower(strings.TrimSpace(capability))
}
