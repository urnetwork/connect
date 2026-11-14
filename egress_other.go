//go:build !windows && !darwin && (!linux || android)

package connect

// On other platforms self-exclusion is handled at the OS layer. Android uses
// VpnService.protect / addDisallowedApplication. The forced-egress binding and
// the egress mark are no-ops so the egress control is inert in those builds.
// Linux marks its sockets (egress_linux.go).
func applyEgressInterface(_ uintptr, _ uint32, _ uint32, _ uint32) error {
	return nil
}
