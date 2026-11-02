package connect

import (
	"fmt"
)

// Applying a verified seed bundle to the directory (EXTENDER-UPDATE2.md §2.1,
// §2.3).
//
// A bundle enters the directory only through the signed paths every record
// takes: its revocations through ApplyRevocationSource and then its records
// through ApplyRecord, both with ExtenderSourceSeed, both verified again under
// the directory's own root keys and network hosts. Nothing here adds an
// address: a seed never makes a bare address dialable, and the directory's
// dialability rule (addressDialableWithLock) dials an address only once a
// verified record names it.

// What one apply changed.
type ExtenderSeedApplyResult struct {
	// revocations that changed the directory
	RevocationCount int
	// records that changed the directory
	RecordCount int
	// records of the bundle the directory now holds as usable: verified,
	// not revoked -- by the bundle or by anything the directory already
	// held -- and not expired
	UsableRecordCount int
}

// Applies a bundle VerifyExtenderSeed returned: every revocation first, so a
// revocation dominates any record before a candidate can be drawn from it, then
// every record no revocation in the bundle dominates. A revocation the
// directory refuses stops the apply before any record lands. A record the
// directory refuses is skipped and the first refusal returned once the rest
// are applied; it happens only when the directory's keys or hosts changed
// since the bundle was verified.
func ApplyExtenderSeed(directory *ExtenderDirectory, bundle *ExtenderSeedBundle) (*ExtenderSeedApplyResult, error) {
	if directory == nil {
		return nil, fmt.Errorf("extender seed apply needs a directory")
	}
	if bundle == nil {
		return nil, newExtenderSeedError(ExtenderSeedCodeMalformed, nil, "the bundle is missing")
	}
	// a bundle verified a while ago is judged fresh again by the directory's
	// clock, so holding a bundle never outlasts its validity
	if err := checkExtenderSeedFreshness(bundle.IssueTime, bundle.ExpireTime, directory.settings.Now()); err != nil {
		return nil, err
	}
	result := &ExtenderSeedApplyResult{}
	for _, revocation := range bundle.Revocations {
		changed, err := directory.ApplyRevocationSource(revocation.Revocation, ExtenderSourceSeed)
		if err != nil {
			return result, newExtenderSeedError(ExtenderSeedCodeRevocationInvalid, err, "the directory refused a revocation")
		}
		if changed {
			result.RevocationCount += 1
		}
	}
	var recordErr error
	for _, record := range bundle.Records {
		if record.Revoked {
			continue
		}
		changed, err := directory.ApplyRecord(record.Record, ExtenderSourceSeed)
		if err != nil {
			if recordErr == nil {
				recordErr = newExtenderSeedError(ExtenderSeedCodeRecordInvalid, err, "the directory refused a record")
			}
			continue
		}
		if changed {
			result.RecordCount += 1
		}
	}
	result.UsableRecordCount = ExtenderSeedUsableRecordCount(directory, bundle)
	return result, recordErr
}

// The records of a bundle the directory holds as usable now: an active key
// (IsActiveKey) whose current record is the bundle's or newer.
func ExtenderSeedUsableRecordCount(directory *ExtenderDirectory, bundle *ExtenderSeedBundle) int {
	if directory == nil || bundle == nil {
		return 0
	}
	count := 0
	for _, record := range bundle.Records {
		if record.Revoked {
			continue
		}
		if directory.IsActiveKey(record.Body.PublicKey) {
			count += 1
		}
	}
	return count
}

// What an import or a stored apply did.
type ExtenderSeedImportResult struct {
	Bundle *ExtenderSeedBundle
	// ExtenderSeedOutcomeAccepted or ExtenderSeedOutcomeIdentical; empty for
	// a stored apply, which accepts nothing
	Outcome string
	Apply   *ExtenderSeedApplyResult
}

// Imports one seed artifact -- a file, pasted text, or a bundle an application
// release carries -- in either form (ParseExtenderSeedArtifact). It is judged
// exactly as a mirror's artifact is: verified under the root keys and network
// hosts the directory holds and fresh by the directory's clock, accepted under
// the sequence rules, and only then applied. An import is no exception to any
// of them, so an expired embedded bundle fails its freshness check like any
// other. A bundle that is accepted but whose records are all expired or
// revoked is a `no_usable_record` refusal, with the result.
func ImportExtenderSeed(
	directory *ExtenderDirectory,
	acceptor *ExtenderSeedAcceptor,
	artifact []byte,
) (*ExtenderSeedImportResult, error) {
	if directory == nil || acceptor == nil {
		return nil, fmt.Errorf("extender seed import needs a directory and an acceptor")
	}
	envelope, err := ParseExtenderSeedArtifact(artifact)
	if err != nil {
		return nil, err
	}
	rootKeys := directory.RootKeys()
	bundle, err := VerifyExtenderSeed(envelope, rootKeys, directory.settings.NetworkHosts, directory.settings.Now())
	if err != nil {
		return nil, err
	}
	outcome, err := acceptor.Accept(bundle, rootKeys)
	if err != nil {
		return nil, err
	}
	return applyExtenderSeedImport(directory, bundle, outcome)
}

// Applies the accepted bundles of the directory's space again, which is what a
// start does before any mirror is asked: the accepted bundle of every one of
// the directory's network hosts, each verified under the root keys and
// network hosts in force and fresh by the directory's clock. A host whose
// bundle no longer verifies or is no longer fresh is passed over and keeps its
// floor (ExtenderSeedAcceptor.Stored), so a stale bundle of one host never
// keeps a fresh one of another from applying. The result is the first applied
// bundle that left a usable record, else the first applied one with its
// refusal. Nil and no error when no host has an accepted bundle; the first
// refusal when every host's bundle was refused.
func ApplyStoredExtenderSeed(
	directory *ExtenderDirectory,
	acceptor *ExtenderSeedAcceptor,
) (*ExtenderSeedImportResult, error) {
	if directory == nil || acceptor == nil {
		return nil, fmt.Errorf("extender seed apply needs a directory and an acceptor")
	}
	rootKeys := directory.RootKeys()
	networkHosts := directory.settings.NetworkHosts
	var usableResult *ExtenderSeedImportResult
	var appliedResult *ExtenderSeedImportResult
	var appliedErr error
	var refusal error
	for _, networkHost := range networkHosts {
		bundle, err := acceptor.Stored(networkHost, rootKeys, networkHosts, directory.settings.Now())
		if err != nil {
			if ExtenderSeedErrorCode(err) == ExtenderSeedCodeStore {
				// the document did not load or save, which no other host can
				// change
				return nil, err
			}
			if refusal == nil {
				refusal = err
			}
			continue
		}
		if bundle == nil {
			continue
		}
		result, err := applyExtenderSeedImport(directory, bundle, "")
		if err == nil && usableResult == nil {
			usableResult = result
		}
		if appliedResult == nil {
			appliedResult = result
			appliedErr = err
		}
	}
	switch {
	case usableResult != nil:
		return usableResult, nil
	case appliedResult != nil:
		return appliedResult, appliedErr
	default:
		return nil, refusal
	}
}

func applyExtenderSeedImport(
	directory *ExtenderDirectory,
	bundle *ExtenderSeedBundle,
	outcome string,
) (*ExtenderSeedImportResult, error) {
	apply, err := ApplyExtenderSeed(directory, bundle)
	result := &ExtenderSeedImportResult{
		Bundle:  bundle,
		Outcome: outcome,
		Apply:   apply,
	}
	if err != nil {
		return result, err
	}
	if apply.UsableRecordCount == 0 {
		return result, newExtenderSeedError(
			ExtenderSeedCodeNoUsableRecord,
			nil,
			"every record of the bundle is expired or revoked",
		)
	}
	return result, nil
}
