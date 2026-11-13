//go:build !race

package extender

// Whether the race detector is on: it slows the 1 GiB streams of the flat
// memory tests past use, and its allocations are not the API's.
const testLagRaceEnabled = false
