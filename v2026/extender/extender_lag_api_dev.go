//go:build lagdev

package extender

// A test build of the measurement API (lag.dev/PLAN.md M2): built with
// `-tags lagdev`, it keeps dev origins -- a local site on plain http or a
// loopback host -- in the allowed list, for the local stack the site's
// end-to-end tests run against (T4). A release build never does.

func init() {
	lagApiDevOriginsAllowed = true
}
