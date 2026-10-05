// Package vlessinterop checks connect's VLESS client (vless*.go in the parent
// module) against the reference server, Xray-core, run in process.
//
// It is its own module so that Xray-core and everything it requires stay out
// of connect's own go.mod: nothing here ships in a client. Run it from this
// directory with
//
//	go test -count=1 ./...
//
// The Xray configuration is built from its protobuf types rather than its json
// loader, and only the VLESS inbound, freedom outbound and the tcp, websocket,
// httpupgrade, tls and reality transports are imported: the json loader
// registers every Xray feature, some of which need a gvisor version that
// clashes with the fork connect is built with.
//
// A reality server learns its cover site's handshake records on its first
// connection for a server name and holds that connection for about five
// seconds (xtls/reality). Each reality test pays that once.
package vlessinterop
