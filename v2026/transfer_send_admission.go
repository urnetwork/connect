// Admission diagnostics retain only the exact local refusal gate. They never
// change admission, retries, ownership, or the public false/nil send contract.
package connect

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
