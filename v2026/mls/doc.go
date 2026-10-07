// RFC 9420, the messaging layer security protocol, in pure go on standard library
// primitives only.
//
// This package is a self contained crypto library so it can be audited and fuzzed
// without the transport. It imports only the standard library, golang.org/x/crypto,
// and its own child mls/syntax. It must never import connect or connect/message, and
// connect must never import it — a package that imports its own subpackage inverts
// the dependency and freezes the child's API against its parent. See
// connect/layering_test.go for the gate.
//
// The whole cryptographic surface is the CryptoProvider interface in crypto.go.
// Nothing outside crypto.go, crypto_labels.go, crypto_x25519.go and hpke.go performs
// a cryptographic operation directly, so an audit reads those four files and a test
// can substitute a deterministic provider for all of them at once.
package mls
