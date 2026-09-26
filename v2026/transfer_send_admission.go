// Admission diagnostics retain only the exact local refusal gate. They never
// change admission, retries, ownership, or the public false/nil send contract.
package connect

import (
	"fmt"
	"time"
)

// SendPackAdmissionError is observation-only evidence that enqueue returned
// false before transferring this Pack's ownership. It never replaces the
// public send result or an acknowledgement callback error. Unlike the bare
// ErrSendPackNotAdmitted sentinel, this proves synchronous pre-admission.
type SendPackAdmissionError struct {
	Boundary string
	Timeout  time.Duration
	// RecoveredByOwner is set only after the enclosing native packet send
	// successfully admits the same retained input (possibly to another exit).
	RecoveredByOwner      bool
	OwnerTrackingOverflow bool
	Err                   error
}

func (self *SendPackAdmissionError) Error() string {
	return fmt.Sprintf("stage=enqueue boundary=%s timeout=%s recovered-by-owner=%t owner-tracking-overflow=%t: %v",
		self.Boundary, self.Timeout, self.RecoveredByOwner, self.OwnerTrackingOverflow, self.Err)
}

func (self *SendPackAdmissionError) Unwrap() error { return self.Err }

// A synchronous refusal result, independent of asynchronous route delivery.
type sendAdmissionBoundary uint8

const (
	sendAdmissionUnknown sendAdmissionBoundary = iota
	sendAdmissionLoopback
	sendAdmissionResendCapacity
	sendAdmissionPack
	sendAdmissionHandoff
)

// Limits diagnostics to a fixed vocabulary even for an invalid enum value.
func (self sendAdmissionBoundary) String() string {
	switch self {
	case sendAdmissionLoopback:
		return "loopback"
	case sendAdmissionResendCapacity:
		return "resend-capacity"
	case sendAdmissionPack:
		return "pack-admission"
	case sendAdmissionHandoff:
		return "queue-handoff"
	default:
		return "unknown"
	}
}
