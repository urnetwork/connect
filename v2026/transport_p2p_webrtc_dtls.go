//go:build !js

package connect

// DTLS ClientHello shaping for the WebRTC p2p/extender path. pion/dtls' default
// ClientHello carries a single fixed ja3/ja4 fingerprint, the exact shape
// Russia's TSPU block-listed 2026-03-30 to kill Snowflake. We replay a real
// browser (Chrome/Firefox) WebRTC ClientHello from the MIT covert-dtls corpus
// so the handshake does not look like pion's default.
//
// pion invokes the ClientHelloMessageHook only on the DTLS client — the SDP
// answerer, which per RFC 5763 becomes setup:active and sends the ClientHello —
// so installing the hook on both peers shapes exactly the answerer. covert-dtls
// splices the live handshake's random, session id, and cookie into the replayed
// hello, so the transcript stays consistent and the handshake completes. One
// fingerprint is bound per peer connection and reused across that handshake's
// flights; rotation across the corpus happens from one connection to the next.
//
// This file is excluded from js/wasm: browser WebRTC owns DTLS and already
// presents a real browser fingerprint, so there is nothing to shape there.

import (
	mathrand "math/rand"

	"github.com/pion/dtls/v3/pkg/protocol/handshake"
	"github.com/theodorsm/covert-dtls/pkg/fingerprints"
	"github.com/theodorsm/covert-dtls/pkg/mimicry"
)

// dtlsClientHelloMimicryHookForFingerprint builds a pion ClientHelloMessageHook
// that replays the given browser fingerprint. A corrupt corpus entry returns an
// error rather than a hook, so the caller can fall back to pion's default hello
// instead of breaking the handshake.
func dtlsClientHelloMimicryHookForFingerprint(
	fingerprint fingerprints.ClientHelloFingerprint,
) (func(handshake.MessageClientHello) handshake.Message, error) {
	mimicked := &mimicry.MimickedClientHello{}
	if err := mimicked.LoadFingerprint(fingerprint); err != nil {
		return nil, err
	}
	return mimicked.Hook, nil
}

// dtlsClientHelloMimicryHook chooses a random fingerprint from the covert-dtls
// corpus and returns its hook. rng selects the entry; nil uses a crypto-seeded
// source. Returns nil (leave pion's default hello in place) when the corpus is
// empty or the chosen entry fails to load, so shaping can never fail the
// handshake.
func dtlsClientHelloMimicryHook(
	rng *mathrand.Rand,
) func(handshake.MessageClientHello) handshake.Message {
	corpus := fingerprints.GetClientHelloFingerprints()
	if len(corpus) == 0 {
		return nil
	}
	if rng == nil {
		rng = cryptoSeededRand()
	}
	hook, err := dtlsClientHelloMimicryHookForFingerprint(corpus[rng.Intn(len(corpus))])
	if err != nil {
		return nil
	}
	return hook
}
