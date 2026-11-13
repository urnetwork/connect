package connect

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/v2026/protocol"
)

// The authenticated seed bundle (EXTENDER-UPDATE2.md §2).
//
// A seed is a small signed set of ordinary open-tier extender records for one
// network space. It exists for the cold start the feed and the dns bootstrap
// cannot serve: a client with no usable record and no route to the operator
// fetches a seed from a mirror (net_extender_seed_bootstrap.go), or imports one
// from a file or pasted text, and its records make extenders dialable again.
//
// Two signatures are involved and neither stands in for the other. The
// envelope signature, under the domain `ExtenderSeedSignatureDomain`,
// authenticates the distribution metadata: which space, which sequence, when
// it was issued and until when it may be used. Every record inside keeps its
// own root signature and is judged exactly as a record from the feed is --
// root, network host, expiry, revocation and tier -- so an envelope signature
// alone never makes a bare address dialable. A seed carries records, never
// addresses.
//
// Trust comes only from what the network space already authorizes: the root
// keys of its configuration or its authenticated update path, and its network
// hosts. The key id of an envelope is a lookup hint, never an anchor, and the
// body has no field that could replace a root key, a network host, a gossip url
// or a resolver setting. That is what keeps a seed apart from the unsigned K7
// share (`ur-ext:1:`, net_extender_share.go), whose optional settings block
// does replace the trust anchor and is taken only by an explicit import.
//
// Freshness is judged against the local clock with the bounded allowance
// `ExtenderSeedClockSkew`. A clock that puts a seed outside its validity gets an
// explicit `expired` or `not_yet_valid` refusal; nothing here ever relaxes a
// check because the clock looks wrong.
//
// The functions in this file are pure functions of their arguments and are
// safe for concurrent use.

const (
	// signature domain of a seed body
	ExtenderSeedSignatureDomain = "ur-extender-seed-v1"
	// The text prefix of the pasted and file form. It carries the version, so
	// a reader refuses a payload it does not understand before decoding it,
	// and it is distinct from the unsigned share's `ur-ext:1:`.
	ExtenderSeedTextPrefix = "ur-seed:1:"
	// The only body format this release writes and accepts.
	ExtenderSeedFormatVersion = 1
)

// The parser limits (§2.2). A complete envelope is checked against the byte cap
// before anything inside it is decoded, and the counts before any record or
// revocation is decoded.
const (
	// a complete serialized envelope
	ExtenderSeedMaxEnvelopeByteCount = 64 * 1024
	// records of one bundle
	ExtenderSeedMaxRecordCount = 8
	// addresses over every record of one bundle
	ExtenderSeedMaxAddressCount = 32
	// revocations of one bundle
	ExtenderSeedMaxRevocationCount = 32
	// One serialized record. A record of a single extender is a few hundred
	// bytes; this keeps every record of a full bundle within the envelope cap.
	ExtenderSeedMaxRecordByteCount = ExtenderSeedMaxEnvelopeByteCount / ExtenderSeedMaxRecordCount
	// One serialized revocation, which is a key, a time and a host.
	ExtenderSeedMaxRevocationByteCount = 1024
)

// The longest text form a full envelope has. A longer text is refused before
// it is decoded.
var ExtenderSeedMaxTextByteCount = len(ExtenderSeedTextPrefix) + base64.RawURLEncoding.EncodedLen(ExtenderSeedMaxEnvelopeByteCount)

// The clock skew allowed when judging a seed's freshness: a seed is usable from
// `IssuedAt - skew` to `ExpiresAt + skew` by the local clock. It is the record
// expiry allowance of the directory (ExtenderDirectorySettings.RecordExpireSkew)
// so a bundle and the records in it lapse together. A local clock further off
// than this gets an explicit freshness refusal rather than a relaxed check.
const ExtenderSeedClockSkew = 5 * time.Minute

// The longest validity one bundle may claim, `ExpiresAt - IssuedAt`. On a fresh
// install with no sequence history, replay is bounded only by the validity
// window (and any minimum sequence the application configures), so a bundle
// embedded in a release eventually becomes unusable instead of being a
// permanent offline escape hatch. Every record a bundle carries must also
// outlive the bundle (a bundle never promises records past their expiry).
const ExtenderSeedMaxValidity = 30 * 24 * time.Hour

// The stable codes of seed refusals and accept outcomes, one per thing a caller
// acts on differently (the sdk maps each to a result id). They are the fixed
// label set of any metric over seed outcomes.
const (
	// not decodable, a foreign version, a missing or inconsistent field
	ExtenderSeedCodeMalformed = "malformed"
	// over a byte cap: the envelope, its text form, one record or revocation
	ExtenderSeedCodeTooLarge = "too_large"
	// over a count limit: records, addresses or revocations
	ExtenderSeedCodeTooMany = "too_many"
	// the network space authorizes no root key, so nothing can verify
	ExtenderSeedCodeNoRootKeys = "no_root_keys"
	// the envelope signature does not verify under any authorized root key:
	// forged, or signed by a key the space does not (or no longer) authorize
	ExtenderSeedCodeSignature = "signature"
	// the bundle names a network host the space does not accept
	ExtenderSeedCodeForeignNetwork = "foreign_network"
	// a carried record fails its own verification or the bundle rules
	ExtenderSeedCodeRecordInvalid = "record_invalid"
	// a carried record is gated or restricted to a canary channel
	ExtenderSeedCodeGatedRecord = "gated_record"
	// a carried revocation fails its own verification or the bundle rules
	ExtenderSeedCodeRevocationInvalid = "revocation_invalid"
	// issued later than the local clock allows: a freshness failure, most
	// likely a local clock that is behind
	ExtenderSeedCodeNotYetValid = "not_yet_valid"
	// past its expiry by the local clock: a freshness failure, an old bundle
	// or a local clock that is ahead
	ExtenderSeedCodeExpired = "expired"

	// accept outcomes (ExtenderSeedAcceptor.Accept): a newer bundle was
	// committed, or the accepted bundle arrived again and nothing changed
	ExtenderSeedOutcomeAccepted  = "accepted"
	ExtenderSeedOutcomeIdentical = "identical"
	// a sequence below the one already accepted for the space, or below the
	// minimum the application configures
	ExtenderSeedCodeRollback = "rollback"
	// a different body at the sequence already accepted
	ExtenderSeedCodeConflict = "conflict"
	// the accepted state could not be read or committed
	ExtenderSeedCodeStore = "store"

	// bootstrap pass (net_extender_seed_bootstrap.go)
	// no usable mirror is configured
	ExtenderSeedCodeNoMirror = "no_mirror"
	// no mirror produced an acceptable bundle before the pass ended
	ExtenderSeedCodeFetch = "fetch"
	// the pass was canceled or its deadline passed
	ExtenderSeedCodeCanceled = "canceled"
	// a bundle was accepted but every record in it is expired or revoked, so
	// there is nothing verified to dial
	ExtenderSeedCodeNoUsableRecord = "no_usable_record"
)

// The typed refusal of a seed. The detail never carries an address, a key or
// anything else from the bundle beyond counts, times and the network host.
type ExtenderSeedError struct {
	Code   string
	Detail string
	Err    error
}

func (self *ExtenderSeedError) Error() string {
	var b strings.Builder
	b.WriteString("extender seed: ")
	b.WriteString(self.Code)
	if self.Detail != "" {
		b.WriteString(": ")
		b.WriteString(self.Detail)
	}
	if self.Err != nil {
		b.WriteString(": ")
		b.WriteString(self.Err.Error())
	}
	return b.String()
}

func (self *ExtenderSeedError) Unwrap() error {
	return self.Err
}

// `errors.Is(err, &ExtenderSeedError{Code: c})` matches any refusal with
// code `c`.
func (self *ExtenderSeedError) Is(target error) bool {
	targetErr, ok := target.(*ExtenderSeedError)
	return ok && targetErr.Code == self.Code && targetErr.Detail == "" && targetErr.Err == nil
}

func newExtenderSeedError(code string, err error, detailFormat string, detailArgs ...any) *ExtenderSeedError {
	detail := detailFormat
	if 0 < len(detailArgs) {
		detail = fmt.Sprintf(detailFormat, detailArgs...)
	}
	return &ExtenderSeedError{
		Code:   code,
		Detail: detail,
		Err:    err,
	}
}

// The code of a seed refusal, or empty for nil and for errors that are not
// seed refusals.
func ExtenderSeedErrorCode(err error) string {
	var seedErr *ExtenderSeedError
	if errors.As(err, &seedErr) {
		return seedErr.Code
	}
	return ""
}

// Whether a refusal is a freshness failure -- expired or not yet valid by the
// local clock -- which a caller reports as a possible clock problem.
func ExtenderSeedFreshnessError(err error) bool {
	switch ExtenderSeedErrorCode(err) {
	case ExtenderSeedCodeExpired, ExtenderSeedCodeNotYetValid:
		return true
	default:
		return false
	}
}

// The network host normalization every network space comparison uses (B2):
// case and a trailing dot are ignored. Seed state is keyed by it.
func NormalizeExtenderNetworkHost(networkHost string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(networkHost), "."))
}

// One record of a verified bundle, with the body its root signature covers.
type ExtenderSeedRecord struct {
	Record *protocol.ExtenderRecord
	Body   *protocol.ExtenderRecordBody
	// A revocation in the same bundle is at or after the record's issue
	// time. The record is never a candidate; it is kept here so a caller can
	// count what the bundle promised.
	Revoked bool
}

// One revocation of a verified bundle, with the body its root signature
// covers.
type ExtenderSeedRevocation struct {
	Revocation *protocol.ExtenderRevocation
	Body       *protocol.ExtenderRevocationBody
}

// A bundle whose envelope, records and revocations have all verified under
// the space's root keys and whose freshness held at `now`. Read only.
type ExtenderSeedBundle struct {
	Envelope *protocol.ExtenderSeedEnvelope
	Body     *protocol.ExtenderSeedBody
	// normalized (NormalizeExtenderNetworkHost)
	NetworkHost string
	Sequence    uint64
	IssueTime   time.Time
	ExpireTime  time.Time
	// sha256 of the exact body bytes that were signed
	BodyDigest []byte
	// the authorized root key the envelope verified under
	SignerPublicKey ed25519.PublicKey
	Records         []*ExtenderSeedRecord
	Revocations     []*ExtenderSeedRevocation
}

// The records of the bundle that no revocation in it dominates.
func (self *ExtenderSeedBundle) UnrevokedRecords() []*ExtenderSeedRecord {
	records := []*ExtenderSeedRecord{}
	for _, record := range self.Records {
		if !record.Revoked {
			records = append(records, record)
		}
	}
	return records
}

// The digest seed state is compared by: sha256 of the exact signed body.
func ExtenderSeedBodyDigest(bodyBytes []byte) []byte {
	digest := sha256.Sum256(bodyBytes)
	return digest[:]
}

// Builds the body of a bundle from signed records and revocations, the
// publisher's first step. The body is checked as `SignExtenderSeed` checks it.
func NewExtenderSeedBody(
	networkHost string,
	sequence uint64,
	issueTime time.Time,
	expireTime time.Time,
	records []*protocol.ExtenderRecord,
	revocations []*protocol.ExtenderRevocation,
) (*protocol.ExtenderSeedBody, error) {
	body := &protocol.ExtenderSeedBody{
		FormatVersion:      ExtenderSeedFormatVersion,
		NetworkHost:        networkHost,
		Sequence:           sequence,
		IssuedAtUnixMilli:  extenderSeedUnixMilli(issueTime),
		ExpiresAtUnixMilli: extenderSeedUnixMilli(expireTime),
		Records:            [][]byte{},
		Revocations:        [][]byte{},
	}
	for _, record := range records {
		recordBytes, err := proto.Marshal(record)
		if err != nil {
			return nil, newExtenderSeedError(ExtenderSeedCodeRecordInvalid, err, "a record does not serialize")
		}
		body.Records = append(body.Records, recordBytes)
	}
	for _, revocation := range revocations {
		revocationBytes, err := proto.Marshal(revocation)
		if err != nil {
			return nil, newExtenderSeedError(ExtenderSeedCodeRevocationInvalid, err, "a revocation does not serialize")
		}
		body.Revocations = append(body.Revocations, revocationBytes)
	}
	if _, _, err := checkExtenderSeedBody(body, nil); err != nil {
		return nil, err
	}
	return body, nil
}

// Serializes and signs a seed body with a root key: the publisher's half.
// Everything a reader checks without trust is checked first -- the format, the
// limits, the times, and that every record is an open-tier record of the same
// space that outlives the bundle -- so nothing is signed that a reader would
// refuse for its shape. The records' own root signatures are not checked here
// (the publisher may hold them from another signer); a publisher runs
// `VerifyExtenderSeed` over the result under its published key set to check
// those too.
func SignExtenderSeed(
	rootPrivateKey ed25519.PrivateKey,
	body *protocol.ExtenderSeedBody,
) (*protocol.ExtenderSeedEnvelope, error) {
	if len(rootPrivateKey) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("extender seed root key must be %d bytes, got %d", ed25519.PrivateKeySize, len(rootPrivateKey))
	}
	if body == nil {
		return nil, newExtenderSeedError(ExtenderSeedCodeMalformed, nil, "the body is missing")
	}
	if body.FormatVersion == 0 {
		// the version is the format's own, not the caller's to forget
		body = proto.Clone(body).(*protocol.ExtenderSeedBody)
		body.FormatVersion = ExtenderSeedFormatVersion
	}
	if _, _, err := checkExtenderSeedBody(body, nil); err != nil {
		return nil, err
	}
	bodyBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(body)
	if err != nil {
		return nil, err
	}
	rootPublicKey := rootPrivateKey.Public().(ed25519.PublicKey)
	envelope := &protocol.ExtenderSeedEnvelope{
		Body:      bodyBytes,
		Signature: ed25519.Sign(rootPrivateKey, extenderSigningBytes(ExtenderSeedSignatureDomain, bodyBytes)),
		KeyId:     ExtenderKeyId(rootPublicKey),
	}
	if size := proto.Size(envelope); ExtenderSeedMaxEnvelopeByteCount < size {
		return nil, newExtenderSeedError(
			ExtenderSeedCodeTooLarge,
			nil,
			"the envelope is %d bytes, at most %d",
			size,
			ExtenderSeedMaxEnvelopeByteCount,
		)
	}
	return envelope, nil
}

// Verifies a seed envelope and everything it carries, and judges its freshness
// at `now` (§2.1, §2.2). `rootKeys` must be the keys the network space already
// authorizes -- its configuration or its authenticated update path, never
// anything a mirror supplied -- and `networkHosts` the hosts the space accepts.
// The envelope's key id only orders the keys tried.
//
// The order is: the byte cap before anything is decoded, the envelope
// signature before the body is decoded, the counts before any record is
// decoded, and then every revocation and every record under its own root
// signature. A record must be an open-tier record (`ExtenderRecordOpen`) of the
// bundle's space, list at least one public address, and expire no earlier than
// the bundle. A record a revocation in the same bundle dominates is returned
// marked `Revoked`. Any failure refuses the whole bundle: nothing of it is
// applied.
//
// Sequence and replay are judged against the accepted state, not here
// (ExtenderSeedAcceptor).
func VerifyExtenderSeed(
	envelope *protocol.ExtenderSeedEnvelope,
	rootKeys *ExtenderRootKeySet,
	networkHosts []string,
	now time.Time,
) (*ExtenderSeedBundle, error) {
	if envelope == nil {
		return nil, newExtenderSeedError(ExtenderSeedCodeMalformed, nil, "the envelope is missing")
	}
	if size := proto.Size(envelope); ExtenderSeedMaxEnvelopeByteCount < size {
		return nil, newExtenderSeedError(
			ExtenderSeedCodeTooLarge,
			nil,
			"the envelope is %d bytes, at most %d",
			size,
			ExtenderSeedMaxEnvelopeByteCount,
		)
	}
	if len(envelope.ProtoReflect().GetUnknown()) != 0 {
		return nil, newExtenderSeedError(ExtenderSeedCodeMalformed, nil, "the envelope carries unknown fields")
	}
	if len(envelope.Body) == 0 {
		return nil, newExtenderSeedError(ExtenderSeedCodeMalformed, nil, "the envelope carries no body")
	}
	if len(envelope.KeyId) != 0 && len(envelope.KeyId) != ExtenderKeyIdByteCount {
		return nil, newExtenderSeedError(ExtenderSeedCodeMalformed, nil, "the key id is %d bytes", len(envelope.KeyId))
	}
	if len(envelope.Signature) != ed25519.SignatureSize {
		return nil, newExtenderSeedError(ExtenderSeedCodeSignature, nil, "the signature is %d bytes", len(envelope.Signature))
	}
	if rootKeys == nil || rootKeys.Len() == 0 {
		return nil, newExtenderSeedError(ExtenderSeedCodeNoRootKeys, nil, "")
	}
	signerPublicKey, err := rootKeys.Verify(
		ExtenderSeedSignatureDomain,
		envelope.Body,
		envelope.Signature,
		envelope.KeyId,
	)
	if err != nil {
		return nil, newExtenderSeedError(ExtenderSeedCodeSignature, err, "")
	}

	// decoded only once the signature holds
	body := &protocol.ExtenderSeedBody{}
	if err := proto.Unmarshal(envelope.Body, body); err != nil {
		return nil, newExtenderSeedError(ExtenderSeedCodeMalformed, err, "the body does not decode")
	}
	if err := checkExtenderSeedHeader(body); err != nil {
		return nil, err
	}
	networkHost := NormalizeExtenderNetworkHost(body.NetworkHost)
	if !ExtenderNetworkHostAllowed(networkHost, networkHosts...) {
		return nil, newExtenderSeedError(ExtenderSeedCodeForeignNetwork, nil, "the bundle is for network host %q", networkHost)
	}
	issueTime := time.UnixMilli(int64(body.IssuedAtUnixMilli))
	expireTime := time.UnixMilli(int64(body.ExpiresAtUnixMilli))
	if err := checkExtenderSeedFreshness(issueTime, expireTime, now); err != nil {
		return nil, err
	}
	records, revocations, err := checkExtenderSeedBody(body, rootKeys)
	if err != nil {
		return nil, err
	}
	return &ExtenderSeedBundle{
		Envelope:        envelope,
		Body:            body,
		NetworkHost:     networkHost,
		Sequence:        body.Sequence,
		IssueTime:       issueTime,
		ExpireTime:      expireTime,
		BodyDigest:      ExtenderSeedBodyDigest(envelope.Body),
		SignerPublicKey: signerPublicKey,
		Records:         records,
		Revocations:     revocations,
	}, nil
}

// Whether a bundle issued at `issueTime` and expiring at `expireTime` is usable
// at `now`, allowing `ExtenderSeedClockSkew` either way. The refusal says which
// side of the window the local clock is on.
func checkExtenderSeedFreshness(issueTime time.Time, expireTime time.Time, now time.Time) error {
	if now.Add(ExtenderSeedClockSkew).Before(issueTime) {
		return newExtenderSeedError(
			ExtenderSeedCodeNotYetValid,
			nil,
			"issued %s ahead of the local clock, beyond the %s allowance; the local clock may be behind",
			issueTime.Sub(now).Round(time.Second),
			ExtenderSeedClockSkew,
		)
	}
	if expireTime.Add(ExtenderSeedClockSkew).Before(now) {
		return newExtenderSeedError(
			ExtenderSeedCodeExpired,
			nil,
			"expired %s ago by the local clock, beyond the %s allowance; the bundle is stale or the local clock is ahead",
			now.Sub(expireTime).Round(time.Second),
			ExtenderSeedClockSkew,
		)
	}
	return nil
}

// The checks of the body's own fields, which need no trust.
func checkExtenderSeedHeader(body *protocol.ExtenderSeedBody) error {
	if body == nil {
		return newExtenderSeedError(ExtenderSeedCodeMalformed, nil, "the body is missing")
	}
	// a field this reader does not know is signed semantics it would ignore
	if len(body.ProtoReflect().GetUnknown()) != 0 {
		return newExtenderSeedError(ExtenderSeedCodeMalformed, nil, "the body carries unknown fields")
	}
	if body.FormatVersion != ExtenderSeedFormatVersion {
		return newExtenderSeedError(ExtenderSeedCodeMalformed, nil, "the body is format version %d", body.FormatVersion)
	}
	if NormalizeExtenderNetworkHost(body.NetworkHost) == "" {
		return newExtenderSeedError(ExtenderSeedCodeMalformed, nil, "the body names no network host")
	}
	if body.Sequence == 0 {
		return newExtenderSeedError(ExtenderSeedCodeMalformed, nil, "the body has no sequence")
	}
	if ExtenderSeedMaxRecordCount < len(body.Records) {
		return newExtenderSeedError(
			ExtenderSeedCodeTooMany,
			nil,
			"the body carries %d records, at most %d",
			len(body.Records),
			ExtenderSeedMaxRecordCount,
		)
	}
	if ExtenderSeedMaxRevocationCount < len(body.Revocations) {
		return newExtenderSeedError(
			ExtenderSeedCodeTooMany,
			nil,
			"the body carries %d revocations, at most %d",
			len(body.Revocations),
			ExtenderSeedMaxRevocationCount,
		)
	}
	if body.IssuedAtUnixMilli == 0 || body.ExpiresAtUnixMilli == 0 {
		return newExtenderSeedError(ExtenderSeedCodeMalformed, nil, "the body is missing its issue or expiry time")
	}
	if extenderSeedMaxUnixMilli < body.IssuedAtUnixMilli || extenderSeedMaxUnixMilli < body.ExpiresAtUnixMilli {
		return newExtenderSeedError(ExtenderSeedCodeMalformed, nil, "the body has a time out of range")
	}
	if body.ExpiresAtUnixMilli <= body.IssuedAtUnixMilli {
		return newExtenderSeedError(ExtenderSeedCodeMalformed, nil, "the body expires before it is issued")
	}
	if validity := time.Duration(body.ExpiresAtUnixMilli-body.IssuedAtUnixMilli) * time.Millisecond; ExtenderSeedMaxValidity < validity {
		return newExtenderSeedError(
			ExtenderSeedCodeMalformed,
			nil,
			"the body claims a validity of %s, at most %s",
			validity,
			ExtenderSeedMaxValidity,
		)
	}
	return nil
}

// Every check of one body: its own fields, then each revocation and each
// record. With `rootKeys` nil the records and revocations are decoded without
// verifying their root signatures, which is the publisher's structural check;
// otherwise each must verify under `rootKeys`.
func checkExtenderSeedBody(
	body *protocol.ExtenderSeedBody,
	rootKeys *ExtenderRootKeySet,
) ([]*ExtenderSeedRecord, []*ExtenderSeedRevocation, error) {
	if err := checkExtenderSeedHeader(body); err != nil {
		return nil, nil, err
	}
	networkHost := NormalizeExtenderNetworkHost(body.NetworkHost)

	revocations := make([]*ExtenderSeedRevocation, 0, len(body.Revocations))
	revocationKeyIssueTimeMs := map[string]uint64{}
	for i, revocationBytes := range body.Revocations {
		if ExtenderSeedMaxRevocationByteCount < len(revocationBytes) {
			return nil, nil, newExtenderSeedError(
				ExtenderSeedCodeTooLarge,
				nil,
				"revocation %d is %d bytes, at most %d",
				i,
				len(revocationBytes),
				ExtenderSeedMaxRevocationByteCount,
			)
		}
		revocation := &protocol.ExtenderRevocation{}
		if err := proto.Unmarshal(revocationBytes, revocation); err != nil {
			return nil, nil, newExtenderSeedError(ExtenderSeedCodeRevocationInvalid, err, "revocation %d does not decode", i)
		}
		var revocationBody *protocol.ExtenderRevocationBody
		if rootKeys != nil {
			verifiedBody, err := rootKeys.VerifyRevocation(revocation)
			if err != nil {
				return nil, nil, newExtenderSeedError(ExtenderSeedCodeRevocationInvalid, err, "revocation %d", i)
			}
			revocationBody = verifiedBody
		} else {
			revocationBody = &protocol.ExtenderRevocationBody{}
			if err := proto.Unmarshal(revocation.Body, revocationBody); err != nil {
				return nil, nil, newExtenderSeedError(ExtenderSeedCodeRevocationInvalid, err, "revocation %d body does not decode", i)
			}
		}
		if NormalizeExtenderNetworkHost(revocationBody.NetworkHost) != networkHost {
			return nil, nil, newExtenderSeedError(ExtenderSeedCodeRevocationInvalid, nil, "revocation %d names another network host", i)
		}
		if len(revocationBody.PublicKey) != ed25519.PublicKeySize {
			return nil, nil, newExtenderSeedError(ExtenderSeedCodeRevocationInvalid, nil, "revocation %d carries no valid public key", i)
		}
		keyString := string(revocationBody.PublicKey)
		if _, ok := revocationKeyIssueTimeMs[keyString]; ok {
			return nil, nil, newExtenderSeedError(ExtenderSeedCodeRevocationInvalid, nil, "revocation %d repeats a key", i)
		}
		revocationKeyIssueTimeMs[keyString] = revocationBody.IssueTimeMs
		revocations = append(revocations, &ExtenderSeedRevocation{
			Revocation: revocation,
			Body:       revocationBody,
		})
	}

	records := make([]*ExtenderSeedRecord, 0, len(body.Records))
	recordKeys := map[string]bool{}
	addressIps := map[netip.Addr]bool{}
	for i, recordBytes := range body.Records {
		if ExtenderSeedMaxRecordByteCount < len(recordBytes) {
			return nil, nil, newExtenderSeedError(
				ExtenderSeedCodeTooLarge,
				nil,
				"record %d is %d bytes, at most %d",
				i,
				len(recordBytes),
				ExtenderSeedMaxRecordByteCount,
			)
		}
		record := &protocol.ExtenderRecord{}
		if err := proto.Unmarshal(recordBytes, record); err != nil {
			return nil, nil, newExtenderSeedError(ExtenderSeedCodeRecordInvalid, err, "record %d does not decode", i)
		}
		var recordBody *protocol.ExtenderRecordBody
		if rootKeys != nil {
			verifiedBody, err := rootKeys.VerifyRecord(record)
			if err != nil {
				return nil, nil, newExtenderSeedError(ExtenderSeedCodeRecordInvalid, err, "record %d", i)
			}
			recordBody = verifiedBody
		} else {
			recordBody = &protocol.ExtenderRecordBody{}
			if err := proto.Unmarshal(record.Body, recordBody); err != nil {
				return nil, nil, newExtenderSeedError(ExtenderSeedCodeRecordInvalid, err, "record %d body does not decode", i)
			}
		}
		if NormalizeExtenderNetworkHost(recordBody.NetworkHost) != networkHost {
			return nil, nil, newExtenderSeedError(ExtenderSeedCodeRecordInvalid, nil, "record %d names another network host", i)
		}
		if len(recordBody.PublicKey) != ed25519.PublicKeySize {
			return nil, nil, newExtenderSeedError(ExtenderSeedCodeRecordInvalid, nil, "record %d carries no valid public key", i)
		}
		// the publisher and the importer both require the open tier, including
		// an empty canary channel (§2.2)
		if !ExtenderRecordOpen(recordBody) {
			return nil, nil, newExtenderSeedError(ExtenderSeedCodeGatedRecord, nil, "record %d is not an open-tier record", i)
		}
		keyString := string(recordBody.PublicKey)
		if recordKeys[keyString] {
			return nil, nil, newExtenderSeedError(ExtenderSeedCodeRecordInvalid, nil, "record %d repeats a key", i)
		}
		recordKeys[keyString] = true
		if len(recordBody.Addresses) == 0 {
			return nil, nil, newExtenderSeedError(ExtenderSeedCodeRecordInvalid, nil, "record %d lists no address", i)
		}
		for _, recordAddress := range recordBody.Addresses {
			if ExtenderSeedMaxAddressCount <= len(addressIps) {
				return nil, nil, newExtenderSeedError(
					ExtenderSeedCodeTooMany,
					nil,
					"the records list more than %d addresses",
					ExtenderSeedMaxAddressCount,
				)
			}
			if recordAddress == nil {
				return nil, nil, newExtenderSeedError(ExtenderSeedCodeRecordInvalid, nil, "record %d lists an empty address", i)
			}
			ip, err := netip.ParseAddr(recordAddress.Ip)
			if err != nil || !ip.IsValid() {
				return nil, nil, newExtenderSeedError(ExtenderSeedCodeRecordInvalid, nil, "record %d lists an address that does not parse", i)
			}
			ip = ip.Unmap()
			if !extenderSeedPublicAddress(ip) {
				return nil, nil, newExtenderSeedError(ExtenderSeedCodeRecordInvalid, nil, "record %d lists an address that is not public", i)
			}
			if addressIps[ip] {
				return nil, nil, newExtenderSeedError(ExtenderSeedCodeRecordInvalid, nil, "record %d repeats an address", i)
			}
			addressIps[ip] = true
		}
		if recordBody.IssueTimeMs == 0 || body.IssuedAtUnixMilli < recordBody.IssueTimeMs {
			return nil, nil, newExtenderSeedError(ExtenderSeedCodeRecordInvalid, nil, "record %d is not issued before the bundle", i)
		}
		if recordBody.ExpireTimeMs <= recordBody.IssueTimeMs {
			// zero, which the directory reads as never expiring, included
			return nil, nil, newExtenderSeedError(ExtenderSeedCodeRecordInvalid, nil, "record %d has no valid expiry", i)
		}
		// bundle validity never outlives the records it promises (§2.3), so a
		// fresh bundle implies fresh records
		if recordBody.ExpireTimeMs < body.ExpiresAtUnixMilli {
			return nil, nil, newExtenderSeedError(ExtenderSeedCodeRecordInvalid, nil, "record %d expires before the bundle", i)
		}
		revoked := false
		if revocationIssueTimeMs, ok := revocationKeyIssueTimeMs[keyString]; ok {
			// B5: a revocation at or after the record's issue time
			revoked = recordBody.IssueTimeMs <= revocationIssueTimeMs
		}
		records = append(records, &ExtenderSeedRecord{
			Record:  record,
			Body:    recordBody,
			Revoked: revoked,
		})
	}
	return records, revocations, nil
}

// Whether an address may appear in a public seed: a global unicast address
// that is not private. A seed is a public bootstrap; a loopback, link-local,
// private, multicast or unspecified address in one is a publisher fault.
func extenderSeedPublicAddress(ip netip.Addr) bool {
	return ip.IsValid() && ip.IsGlobalUnicast() && !ip.IsPrivate()
}

// The largest unix milli a seed time may be, which keeps every conversion to
// time.Time and back exact.
const extenderSeedMaxUnixMilli = uint64(1) << 52

func extenderSeedUnixMilli(t time.Time) uint64 {
	if t.IsZero() || t.UnixMilli() <= 0 {
		return 0
	}
	return uint64(t.UnixMilli())
}

// Serializes an envelope, refusing one over the byte cap. The serialization is
// deterministic, though nothing depends on it: the signature covers the body
// bytes carried inside, not the envelope.
func MarshalExtenderSeedEnvelope(envelope *protocol.ExtenderSeedEnvelope) ([]byte, error) {
	if envelope == nil {
		return nil, newExtenderSeedError(ExtenderSeedCodeMalformed, nil, "the envelope is missing")
	}
	if size := proto.Size(envelope); ExtenderSeedMaxEnvelopeByteCount < size {
		return nil, newExtenderSeedError(
			ExtenderSeedCodeTooLarge,
			nil,
			"the envelope is %d bytes, at most %d",
			size,
			ExtenderSeedMaxEnvelopeByteCount,
		)
	}
	return proto.MarshalOptions{Deterministic: true}.Marshal(envelope)
}

// Parses a serialized envelope, refusing one over the byte cap before decoding
// anything. The envelope is not verified (VerifyExtenderSeed).
func ParseExtenderSeedEnvelope(envelopeBytes []byte) (*protocol.ExtenderSeedEnvelope, error) {
	if ExtenderSeedMaxEnvelopeByteCount < len(envelopeBytes) {
		return nil, newExtenderSeedError(
			ExtenderSeedCodeTooLarge,
			nil,
			"the envelope is %d bytes, at most %d",
			len(envelopeBytes),
			ExtenderSeedMaxEnvelopeByteCount,
		)
	}
	if len(envelopeBytes) == 0 {
		return nil, newExtenderSeedError(ExtenderSeedCodeMalformed, nil, "the envelope is empty")
	}
	envelope := &protocol.ExtenderSeedEnvelope{}
	if err := proto.Unmarshal(envelopeBytes, envelope); err != nil {
		return nil, newExtenderSeedError(ExtenderSeedCodeMalformed, err, "the envelope does not decode")
	}
	return envelope, nil
}

// The text form of an envelope: `ur-seed:1:` and the serialized envelope in
// base64url with no padding, one line that survives a qr code, a chat message
// and a copy/paste.
func EncodeExtenderSeedText(envelope *protocol.ExtenderSeedEnvelope) (string, error) {
	envelopeBytes, err := MarshalExtenderSeedEnvelope(envelope)
	if err != nil {
		return "", err
	}
	return ExtenderSeedTextPrefix + base64.RawURLEncoding.EncodeToString(envelopeBytes), nil
}

// Reads the text form, tolerating surrounding whitespace. The length is checked
// before anything is decoded, so an oversized paste costs no allocation. The
// envelope is not verified (VerifyExtenderSeed).
func DecodeExtenderSeedText(text string) (*protocol.ExtenderSeedEnvelope, error) {
	trimmed := strings.TrimSpace(text)
	if ExtenderSeedMaxTextByteCount < len(trimmed) {
		return nil, newExtenderSeedError(
			ExtenderSeedCodeTooLarge,
			nil,
			"the text is %d bytes, at most %d",
			len(trimmed),
			ExtenderSeedMaxTextByteCount,
		)
	}
	if !strings.HasPrefix(trimmed, ExtenderSeedTextPrefix) {
		if strings.HasPrefix(trimmed, extenderSeedTextPrefixStem) {
			return nil, newExtenderSeedError(ExtenderSeedCodeMalformed, nil, "the text is a seed version this release does not read")
		}
		return nil, newExtenderSeedError(ExtenderSeedCodeMalformed, nil, "the text does not start with %s", ExtenderSeedTextPrefix)
	}
	encoded := strings.TrimSpace(strings.TrimPrefix(trimmed, ExtenderSeedTextPrefix))
	if ExtenderSeedMaxEnvelopeByteCount < base64.RawURLEncoding.DecodedLen(len(encoded)) {
		return nil, newExtenderSeedError(ExtenderSeedCodeTooLarge, nil, "the text decodes past %d bytes", ExtenderSeedMaxEnvelopeByteCount)
	}
	envelopeBytes, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, newExtenderSeedError(ExtenderSeedCodeMalformed, err, "the text is not base64url")
	}
	return ParseExtenderSeedEnvelope(envelopeBytes)
}

// The version-independent start of the text form, which tells a seed of a
// version this release does not read from something else entirely.
const extenderSeedTextPrefixStem = "ur-seed:"

// Reads a seed artifact in either form: the text form, as a file or a paste
// holds it, or the serialized envelope, as a mirror or a release may carry it.
// The envelope is not verified (VerifyExtenderSeed).
func ParseExtenderSeedArtifact(artifact []byte) (*protocol.ExtenderSeedEnvelope, error) {
	trimmed := bytes.TrimSpace(artifact)
	if bytes.HasPrefix(trimmed, []byte(extenderSeedTextPrefixStem)) {
		if ExtenderSeedMaxTextByteCount < len(trimmed) {
			return nil, newExtenderSeedError(
				ExtenderSeedCodeTooLarge,
				nil,
				"the text is %d bytes, at most %d",
				len(trimmed),
				ExtenderSeedMaxTextByteCount,
			)
		}
		return DecodeExtenderSeedText(string(trimmed))
	}
	// binary: no whitespace trimming, since leading or trailing bytes that
	// look like whitespace can be part of the encoding
	return ParseExtenderSeedEnvelope(artifact)
}
