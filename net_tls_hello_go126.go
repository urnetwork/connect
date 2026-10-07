//go:build !go1.27

package connect

// net_tls_hello_go126.go — net/http before Go 1.27 (verified for 1.25, and
// assumed for 1.26) reads the negotiated protocol of a DialTLSContext
// connection only from a *tls.Conn, and would speak http/1.1 into an h2
// negotiated over uTLS. A path that offers h2 keeps Go's hello
// (net_tls_hello.go); the websocket path, which offers http/1.1 alone, still
// presents the Chrome hello.

const httpTransportReadsTlsConnectionState = false
