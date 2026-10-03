package extender

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/quic-go/quic-go/http3"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// The extender request handler (EXTENDER.md A3, A4, A7, A8, A11).
//
// One handler serves every carrier: the http/1.1 and h2 servers on the tcp
// carrier and the h3 server on the udp carriers. An accepted request is
// answered with 200 and the ExtenderResponse, after which the stream is taken
// over -- hijacked on tcp, taken with HTTPStreamer on h3 -- and carries the
// inner bytes. Every refusal is 403 with no body and closes the connection.
// On an NLayer extender the taken-over stream of a forward goes to another
// extender rather than to the destination (extender_nlayer.go).
//
// An extender request that arrives over h2 is refused, because an h2 stream
// cannot be hijacked. A request that is not an extender request is not refused
// at all: it goes to the reverse proxy, which answers it with the real site
// when its server name is on the whitelist (A5).

type extenderHandler struct {
	server *ExtenderServer
}

func (self *extenderHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	server := self.server

	if !isExtenderRequest(req) {
		// everything that is not the extender protocol looks like an ordinary
		// host to whoever asked (A5)
		server.proxy.serve(w, req)
		return
	}
	if req.ProtoMajor == 2 {
		self.refuse(w, req, "request", fmt.Errorf("an extender request cannot use h2"))
		return
	}

	header, err := readExtenderHeader(req)
	if err != nil {
		self.refuse(w, req, "header decode", err)
		return
	}
	if !server.IsAllowedSecret(header) {
		self.refuse(w, req, "header authorization", fmt.Errorf("secret signature is not allowed"))
		return
	}
	// every action is admitted under the limits of A12, whichever service it
	// asks for; a header signed with one of this extender's secrets is spared
	// the per-subnet one
	if limit, retryAfter := server.admitAction(req.RemoteAddr, 0 < len(server.allowedSecrets)); limit != extenderAdmitted {
		self.limit(w, req, extenderAdmissionStage(limit), fmt.Errorf("the source is over its admission limit"), retryAfter)
		return
	}

	var serviceConnHandler func(conn net.Conn)
	var probe *extenderProbe
	nlayerProbe := false
	switch header.Service {
	case connect.ExtenderServiceForward:
		// the depth bound comes first, on every extender: a chain that loops
		// ends here whatever else the header says (A11)
		if err := server.checkNLayerDepth(header); err != nil {
			self.refuse(w, req, "hop count", err)
			return
		}
		if !server.IsAllowedHost(header.DestinationHost) {
			self.refuse(w, req, "destination authorization", fmt.Errorf("host %q is not allowed", header.DestinationHost))
			return
		}
		// an NLayer extender whose every hop is limited says so now, while it
		// still can: once the response is out, a forward can only close (A12)
		if retryAfter, limited := server.nlayerHopsLimited(); limited {
			self.limit(w, req, "nlayer limited", fmt.Errorf("every NLayer hop is limited"), retryAfter)
			return
		}
	case connect.ExtenderServiceGossip:
		serviceConnHandler = server.settings.GossipConnHandler
	case connect.ExtenderServiceFeed:
		serviceConnHandler = server.settings.FeedConnHandler
	case connect.ExtenderServiceProbe:
		// the depth bound comes first, as for a forward: a probe relayed
		// around a loop of NLayer extenders ends here (A11, GEOMAP §2.9)
		if err := server.checkNLayerDepth(header); err != nil {
			self.refuse(w, req, "hop count", err)
			return
		}
		if 0 < len(server.nlayerHops) {
			// an NLayer extender never answers a probe itself: it relays it
			// to the end of its chain (GEOMAP §2.9)
			nlayerProbe = true
			break
		}
		// always served: a probe is answered by the response itself, and a
		// nonce is added only for an attesting pinger this extender has an
		// identity to judge its claim with (DESIGNNOTES4.md, GEOMAP §2.2)
		if probe, err = server.beginProbe(header, req.RemoteAddr); err != nil {
			self.refuse(w, req, "probe", err)
			return
		}
	default:
		self.refuse(w, req, "service", fmt.Errorf("service %d is not known", header.Service))
		return
	}
	if header.Service != connect.ExtenderServiceForward &&
		header.Service != connect.ExtenderServiceProbe &&
		serviceConnHandler == nil {
		self.refuse(w, req, "service", fmt.Errorf("service %d is not available", header.Service))
		return
	}
	if server.settings.HeaderHandler != nil {
		server.settings.HeaderHandler(header)
	}
	if nlayerProbe {
		self.serveNLayerProbe(w, req, header)
		return
	}

	// the response is this extender's own on an NLayer extender too, so the
	// identity a client sees of a chain is its first layer's key (A11). A
	// probe answered here is the end of whatever chain it crossed, and says
	// how deep that was (GEOMAP §2.9).
	response := &protocol.ExtenderResponse{
		PublicKey:          server.PublicKey(),
		ChallengeSignature: server.SignChallenge(header.Challenge),
		Carriers:           server.Carriers(),
		ProbeNonce:         probe.nonceBytes(),
	}
	if probe != nil {
		response.HopCount = header.HopCount
	}
	if !self.writeResponse(w, req, response) {
		return
	}

	clientConn, err := takeOverConn(w, req)
	if err != nil {
		server.reportError("take over", err)
		return
	}
	defer clientConn.Close()

	handleCtx, handleCancel := context.WithCancel(req.Context())
	defer handleCancel()
	// the request context of the http servers does not follow the extender's,
	// so Close ends what this request dials and relays -- a forward or an
	// NLayer hop dial still in flight included -- as it ends the connection
	defer context.AfterFunc(server.ctx, handleCancel)()

	if probe != nil {
		// the response was flushed by the take over; the interval the gate
		// judges against starts now, and the verdict is written before the
		// stream closes (DESIGNNOTES4.md §3, GEOMAP §2.3)
		server.serveProbe(handleCtx, clientConn, probe)
		return
	}

	if serviceConnHandler != nil {
		// the service owns the stream until it returns (A8)
		serviceConnHandler(clientConn)
		return
	}

	if 0 < len(server.nlayerHops) {
		// an NLayer extender relays the inner stream to another extender,
		// datagram framing and all: the hop is what turns the frames into
		// udp (A11)
		server.serveNLayer(handleCtx, handleCancel, clientConn, req.RemoteAddr, header)
		return
	}

	if header.Datagram {
		// The carrier stays one reliable byte stream; the datagrams framed on
		// it become real udp packets here. This is how quic reaches a
		// destination through an extender: the inner tls is opaque to us, so
		// there is nothing to reframe and the boundaries have to be carried
		// explicitly (extender_datagram.go).
		server.relayDatagram(handleCtx, handleCancel, clientConn, header)
		return
	}

	forwardConn, err := server.dialForward(handleCtx, req.RemoteAddr, header)
	if err != nil {
		return
	}
	defer forwardConn.Close()

	server.relay(handleCtx, handleCancel, clientConn, forwardConn)
}

// Refuses with 403 and no body, closing the connection (A4).
func (self *extenderHandler) refuse(w http.ResponseWriter, req *http.Request, stage string, err error) {
	refuseRequest(self.server, w, req, http.StatusForbidden, stage, err)
}

// Answers 429 with no body and a Retry-After in whole seconds, closing the
// connection (A12). It is the ordinary answer of any rate-limited site, which
// is why a limit has it and every other refusal is 403.
func (self *extenderHandler) limit(
	w http.ResponseWriter,
	req *http.Request,
	stage string,
	err error,
	retryAfter time.Duration,
) {
	if seconds := int64((retryAfter + time.Second - 1) / time.Second); 0 < seconds {
		w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
	}
	refuseRequest(self.server, w, req, http.StatusTooManyRequests, stage, err)
}

// Answers an accepted request with 200 and its response frame (A3), reporting
// whether the stream can be taken over.
func (self *extenderHandler) writeResponse(
	w http.ResponseWriter,
	req *http.Request,
	response *protocol.ExtenderResponse,
) bool {
	responseFrameBytes, err := connect.ExtenderResponseFrame(response)
	if err != nil {
		self.refuse(w, req, "response", err)
		return false
	}
	w.Header().Set("Content-Type", connect.ExtenderContentType)
	if req.ProtoMajor == 1 {
		// http/1.1 would otherwise chunk a flushed body, and the bytes after
		// the response are raw. h3 must not carry a content length at all:
		// there the response body and the raw bytes that follow are the same
		// DATA stream, and a length would bound the reader the client keeps.
		w.Header().Set("Content-Length", strconv.Itoa(len(responseFrameBytes)))
	}
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(responseFrameBytes); err != nil {
		self.server.reportError("response", err)
		return false
	}
	return true
}

// The A3 shape: POST / with the extender content type.
func isExtenderRequest(req *http.Request) bool {
	if req.Method != http.MethodPost {
		return false
	}
	if req.URL == nil || req.URL.Path != "/" {
		return false
	}
	contentType := req.Header.Get("Content-Type")
	if i := strings.IndexByte(contentType, ';'); 0 <= i {
		contentType = contentType[0:i]
	}
	return strings.EqualFold(strings.TrimSpace(contentType), connect.ExtenderContentType)
}

// Reads the serialized header, which every carrier delimits with the request
// content length.
func readExtenderHeader(req *http.Request) (*protocol.ExtenderHeader, error) {
	if req.ContentLength < 0 {
		return nil, fmt.Errorf("extender header has no content length")
	}
	if connect.ExtenderMaxHeaderByteCount < req.ContentLength {
		return nil, fmt.Errorf(
			"extender header is %d bytes, at most %d",
			req.ContentLength,
			connect.ExtenderMaxHeaderByteCount,
		)
	}
	headerBytes := make([]byte, req.ContentLength)
	if _, err := io.ReadFull(req.Body, headerBytes); err != nil {
		return nil, err
	}
	header := &protocol.ExtenderHeader{}
	if err := proto.Unmarshal(headerBytes, header); err != nil {
		return nil, err
	}
	return header, nil
}

// Takes the stream over after the response: the h3 stream on the udp carriers,
// the hijacked connection on tcp. What the buffered reader of a hijack already
// holds is kept, because it can be the first inner bytes; the rest is read from
// the connection itself. The buffered reader reads through net/http's own
// connection reader, which cancels the request context on any read error, a
// read deadline included, and the loop check of an NLayer extender reads under
// one before it dials on that context (A11).
func takeOverConn(w http.ResponseWriter, req *http.Request) (net.Conn, error) {
	if streamer, ok := w.(http3.HTTPStreamer); ok {
		return newStreamConn(
			streamer.HTTPStream(),
			streamAddr{network: "udp", address: ""},
			streamAddr{network: "udp", address: req.RemoteAddr},
		), nil
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		return nil, fmt.Errorf("the extender response writer cannot be taken over")
	}
	if flusher, ok := w.(http.Flusher); ok {
		// the hijack releases the connection without flushing the buffered
		// response body
		flusher.Flush()
	}
	conn, bufrw, err := hijacker.Hijack()
	if err != nil {
		return nil, err
	}
	// the buffered bytes are copied out without touching the connection
	bufferedBytes := make([]byte, bufrw.Reader.Buffered())
	if _, err := io.ReadFull(bufrw.Reader, bufferedBytes); err != nil {
		conn.Close()
		return nil, err
	}
	return newConnWithInitialBytes(conn, bufferedBytes, ""), nil
}

// streamAddr names the endpoint of a taken-over h3 stream, which has no socket
// address of its own.
type streamAddr struct {
	network string
	address string
}

func (self streamAddr) Network() string {
	return self.network
}

func (self streamAddr) String() string {
	return self.address
}
