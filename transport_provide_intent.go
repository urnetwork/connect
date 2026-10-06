package connect

// transport_provide_intent.go -- the provide intent a platform transport
// declares on its connect handshake.
//
// A client that provides publicly declares it on every platform connection.
// The platform counts a declared connection apart from the network's ordinary
// clients and exempts it from the client limit while it qualifies as a public
// provider; a connection that declares nothing is an ordinary client. The
// declaration rides with the auth generation (ClientAuth.ProvideIntent), so a
// change reaches the platform with the next dial, and an owner that needs it
// at once replaces its transports.

import (
	"net/http"
)

// HeaderProvideIntent is the h1 auth header that declares provide intent.
// Absent when the client does not intend to provide publicly.
const HeaderProvideIntent = "X-UR-Provide-Intent"

// ProvideIntentDeclared is the HeaderProvideIntent value of a declaring client.
const ProvideIntentDeclared = "1"

// applyProvideIntentHeader declares the auth generation's provide intent on
// the h1 v2 auth headers.
func applyProvideIntentHeader(header http.Header, auth ClientAuth) {
	if auth.ProvideIntent {
		header.Set(HeaderProvideIntent, ProvideIntentDeclared)
	}
}
