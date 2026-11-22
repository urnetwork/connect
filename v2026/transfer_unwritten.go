// Original-Pack provenance is separate from provider-failure classification.
package connect

// The original Pack failed before any of its application frames reached
// serialization. This local proof does not excuse the underlying provider
// error and must never be applied to retained items or a partial group.
type sendPackUnwrittenError struct {
	cause error
}

// Preserve diagnostics and callers which compare the original error text.
func (self *sendPackUnwrittenError) Error() string {
	return self.cause.Error()
}

// Preserve the original cancellation, contract and structural cause.
func (self *sendPackUnwrittenError) Unwrap() error {
	return self.cause
}

// Only an owner still before this original Pack's first serialization may
// attach this proof; the result is local to that Pack's terminal disposition.
func newUnwrittenSendPackError(err error) error {
	if err == nil {
		return nil
	}
	return &sendPackUnwrittenError{cause: err}
}

// A single-cause wrapper preserves whole-Pack provenance. A joined result
// does not: successful written chunks are omitted by errors.Join. Only an
// explicit outer marker, supplied by the original owner, can certify a group.
func sendPackFailedUnwritten(err error) bool {
	for err != nil {
		if _, ok := err.(*sendPackUnwrittenError); ok {
			return true
		}
		wrapped, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = wrapped.Unwrap()
	}
	return false
}
