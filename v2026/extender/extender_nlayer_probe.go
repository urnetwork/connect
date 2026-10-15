package extender

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

// A probe through an NLayer extender (EXTENDER.md A11, GEOMAP §2.9).
//
// An NLayer extender -- a front -- never answers a probe itself. It relays the
// probe to the end of its chain, so the round trip a pinger measures is the
// path a client of the front would use, and the extender that judges and
// co-signs the claim is the one that terminates the chain. The front dials its
// hop with the same pinger identity and HopCount + 1 and waits for the hop's
// response before it answers; the answer carries the front's own identity --
// its key, its challenge signature, its carriers, since the pin a pinger holds
// is the front's -- and the end's nonce, the end's HopCount and the end's key.
// From there the bytes are relayed both ways, the attestation going to the end
// and the verdict coming back, exactly as a forward's are.
//
// There is no inner tls in a probe, so the loop check of a forward cannot see
// one that comes back. A relayed probe is keyed instead by the pinger identity
// in its header: a front holds one relayed probe per source at a time and
// refuses another while it is in flight. The identity is unsigned when the
// front reads it, but it is the identity the attestation must be signed by at
// the end, so a spoofed one buys a refused claim. Anyone can occupy a source's
// one slot through a front for the life of one probe, which the end's
// attestation timeout bounds. A ranking probe names no source and gets no
// entry; the depth bound is its loop guard.

// The identity a relayed probe is keyed by: the kind and the bytes of the one
// well formed pinger its header names.
type nlayerProbeSource struct {
	pingerKind connect.ExtenderPingerKind
	identity   string
}

// The source of a probe header, and whether it names one. A header naming an
// identity of the wrong size names none, exactly as the end judges it: it is
// a ranking probe there, issued no nonce.
func nlayerProbeSourceOf(header *protocol.ExtenderHeader) (nlayerProbeSource, bool) {
	switch {
	case len(header.ProbeClientId) == connect.ExtenderProbeClientIdByteCount:
		return nlayerProbeSource{
			pingerKind: connect.ExtenderPingerKindProvider,
			identity:   string(header.ProbeClientId),
		}, true
	case len(header.ProbeExtenderPublicKey) == ed25519.PublicKeySize:
		return nlayerProbeSource{
			pingerKind: connect.ExtenderPingerKindExtender,
			identity:   string(header.ProbeExtenderPublicKey),
		}, true
	default:
		return nlayerProbeSource{}, false
	}
}

// Admits a source to the in-flight set for the life of its relayed probe,
// refusing one that already has a probe in flight through this extender.
func (self *ExtenderServer) beginNLayerProbeSource(source nlayerProbeSource) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.nlayerProbeSources[source] {
		return false
	}
	self.nlayerProbeSources[source] = true
	return true
}

// Releases a source when its relayed probe ends.
func (self *ExtenderServer) endNLayerProbeSource(source nlayerProbeSource) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	delete(self.nlayerProbeSources, source)
}

// Relays one probe request of an NLayer extender to the end of its chain and
// answers it (GEOMAP §2.9). The depth bound has been checked. A hop dial that
// fails before its response is a 403 here, so the pinger, which has had no
// response, probes again.
func (self *extenderHandler) serveNLayerProbe(
	w http.ResponseWriter,
	req *http.Request,
	header *protocol.ExtenderHeader,
) {
	server := self.server
	// the same admission as a probe answered here: a relay costs a hop dial
	// and more, so a source over its rate is refused before any of it
	if err := server.admitProbe(header, req.RemoteAddr); err != nil {
		self.refuse(w, req, "probe", err)
		return
	}
	if source, ok := nlayerProbeSourceOf(header); ok {
		if !server.beginNLayerProbeSource(source) {
			self.refuse(w, req, "nlayer probe", fmt.Errorf(
				"a relayed probe of this %s pinger is already in flight", source.pingerKind,
			))
			return
		}
		defer server.endNLayerProbeSource(source)
	}

	// the hop dial runs before any response, on the request's context, which
	// Close does not reach on its own
	probeCtx, probeCancel := context.WithCancel(req.Context())
	defer probeCancel()
	defer context.AfterFunc(server.ctx, probeCancel)()
	// the request's read deadline bounds a header that has been read; the
	// chain's answer is bounded by the hop dial instead. Left in place it
	// would end the request's context mid dial on http/1.1, whose connection
	// reader cancels it on the deadline. A carrier that cannot lift it keeps
	// it, which only bounds the chain further.
	http.NewResponseController(w).SetReadDeadline(time.Time{})

	hopConn, hopResponse, err := server.dialNLayerHop(probeCtx, req.RemoteAddr, &connect.ExtenderDial{
		Service:                connect.ExtenderServiceProbe,
		ProbeClientId:          header.ProbeClientId,
		ProbeExtenderPublicKey: header.ProbeExtenderPublicKey,
		HopCount:               header.HopCount + 1,
	})
	if err != nil {
		// a chain whose every hop is limited is limited itself, and says so
		// with the shortest backoff (A12)
		var limitedErr *connect.ExtenderLimitedError
		if errors.As(err, &limitedErr) {
			self.limit(w, req, "nlayer limited", err, limitedErr.RetryAfter)
			return
		}
		self.refuse(w, req, "nlayer probe dial", err)
		return
	}
	defer hopConn.Close()

	// the end is whichever extender answered the probe itself: the hop, or
	// the end the hop relayed to and named
	chainEndPublicKey := hopResponse.ChainEndPublicKey
	if len(chainEndPublicKey) == 0 {
		chainEndPublicKey = hopResponse.PublicKey
	}
	if !self.writeResponse(w, req, &protocol.ExtenderResponse{
		PublicKey:          server.PublicKey(),
		ChallengeSignature: server.SignChallenge(header.Challenge),
		Carriers:           server.Carriers(),
		ProbeNonce:         hopResponse.ProbeNonce,
		HopCount:           hopResponse.HopCount,
		ChainEndPublicKey:  chainEndPublicKey,
	}) {
		return
	}

	clientConn, err := takeOverConn(w, req)
	if err != nil {
		server.reportError("take over", err)
		return
	}
	defer clientConn.Close()
	// the attestation goes to the end and the verdict comes back, until the
	// end closes after its verdict or the pinger goes away
	server.relay(probeCtx, probeCancel, clientConn, hopConn)
}
