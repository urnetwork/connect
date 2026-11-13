//go:build go1.27

package connect

// net_tls_hello_go127.go — net/http (Go 1.27) reads the negotiated protocol
// of a DialTLSContext connection from any crypto/tls ConnectionState method,
// so a path that offers h2 can present the Chrome hello (net_tls_hello.go).

const httpTransportReadsTlsConnectionState = true
