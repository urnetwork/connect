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
