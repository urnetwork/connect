//go:build !windows

package connect

// On non-Windows platforms self-exclusion is handled at the OS layer: macOS
// network extensions bypass their own tunnel automatically, and Android uses
// VpnService.protect / addDisallowedApplication. The forced-egress binding is a
// no-op so the egress control is inert in the mobile and desktop app builds.
func applyEgressInterface(_ uintptr, _ uint32, _ uint32) error {
	return nil
}
