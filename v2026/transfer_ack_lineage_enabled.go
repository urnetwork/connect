//go:build acklineagetrace

package connect

// The diagnostic build still needs a non-nil, bounded ProgressObserver.
// Ordinary product builds compile these additional hooks out entirely.
const ackLineageTraceEnabled = true
