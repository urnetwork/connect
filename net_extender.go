package connect

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	mathrand "math/rand"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"

	quic "github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/protocol"
)

// The extender client (EXTENDER.md A1, A3, A10, B3).
//
// An extender uses an independent url that forwards to the platform. The
// connection to the platform is end-to-end encrypted with TLS using the
// hostname of the destination, so an extender sees only ciphertext.
//
// Three carriers reach one extender, and all three yield one reliable byte
// stream: tcp 443 terminated TLS, udp 443 QUIC with ALPN h3, and the dns
// carrier, the same QUIC over the dns packet translation on udp 4053 and, where
// the extender binds it, on 53; one dial tries both (net_extender_dns_ports.go).
// On every carrier the client sends one `POST /` with the serialized
// ExtenderHeader as the body and reads an ExtenderResponse; after that the
// stream carries the inner bytes raw.
//
// The outer TLS is always InsecureSkipVerify: the extender presents a
// certificate for a spoof name it does not own. When the caller knows the
// extender's identity key from a signed record, VerifyPeerCertificate requires
// the presented leaf to be signed by that key (B3), which is the only outer
// authentication there is.

type ExtenderConnectMode string

const (
	ExtenderConnectModeTcpTls ExtenderConnectMode = "tcptls"
	ExtenderConnectModeQuic   ExtenderConnectMode = "quic"
	ExtenderConnectModeDns    ExtenderConnectMode = "dns"
	// The peer-to-peer webrtc carrier (EXTENDER.md S, C2a). Not an ip:port
	// dial: the extender is reached through the exchange signaling and
	// ICE/STUN, by the rendezvous id its record carries, so a config in this
	// mode needs ConnectSettings.WebRtcExtenderCarrier and ignores its port,
	// fragment and reorder (net_extender_webrtc.go).
	ExtenderConnectModeWebRtc ExtenderConnectMode = "webrtc"
)

// Carrier names as they appear in an ExtenderResponse and a record (A4, B2).
const (
	ExtenderCarrierTcp  = "tcp"
	ExtenderCarrierQuic = "quic"
	ExtenderCarrierDns  = "dns"
	// The peer-to-peer webrtc carrier (EXTENDER.md C2a, webrtc extender
	// section). It is not an ip:port carrier: it is reached through the
	// exchange signaling and ICE/STUN, so it is never dialed by
	// dialExtenderStream. The dial and serve live in net_extender_webrtc.go
	// (not built for js/wasm). The name appears in records and the acceptance
	// path so clients, the uptime task, and the strategy can name it.
	ExtenderCarrierWebRtc = "webrtc"
)

// Fixed carrier ports (A1, L2). The old multi-port personas are removed. A
// record may name other ports (B2); these are what an address with no record
// is dialed on. The dns carrier is on the unprivileged 4053, which every
// extender binds; only the sn miner also binds 53, and a client tries both
// for every extender, whether or not a record lists 53
// (net_extender_dns_ports.go).
const (
	ExtenderTcpPort  = 443
	ExtenderQuicPort = 443
	ExtenderDnsPort  = DefaultWhodisPort
)

// The connect mode of a carrier name, and whether the name is one this client
// can dial.
func ExtenderConnectModeForCarrier(carrier string) (ExtenderConnectMode, bool) {
	switch carrier {
	case ExtenderCarrierTcp:
		return ExtenderConnectModeTcpTls, true
	case ExtenderCarrierQuic:
		return ExtenderConnectModeQuic, true
	case ExtenderCarrierDns:
		return ExtenderConnectModeDns, true
	case ExtenderCarrierWebRtc:
		return ExtenderConnectModeWebRtc, true
	default:
		return "", false
	}
}

// Reserved services of the extender header (A8). 0 forwards to the
// destination; the others hand the taken-over stream to an in-process server.
const (
	ExtenderServiceForward uint32 = 0
	ExtenderServiceGossip  uint32 = 1
	ExtenderServiceFeed    uint32 = 2
	// A latency probe (DESIGNNOTES4.md, GEOMAP §2). The response is the
	// ordinary one, with a nonce when the client identified itself as an
	// attesting provider or extender; the stream then carries at most one
	// attestation frame, which the extender answers with one verdict frame
	// before it closes. Nothing is forwarded.
	ExtenderServiceProbe uint32 = 3
)

// Content type of both the extender request and its response (A3).
const ExtenderContentType = "application/x-ur-extender"

// Encoding tld of the dns carrier when a record does not carry one (A1).
const DefaultExtenderDnsTld = "ur.xyz."

// The extender protocol's limits (A3): the protocol's values, which the client
// and the extender share. Changing one is a protocol change no deployment makes
// on one side alone, so each side's settings carry this one object, the
// client's ConnectSettings.ExtenderFormatSettings and the extender's
// ExtenderSettings.ExtenderFormatSettings, and its defaults stay here (QD-702).
// The carrier ports and the dns tld above name endpoints rather than bound
// anything, and stay identifiers.
type ExtenderFormatSettings struct {
	// The most bytes a serialized extender header holds, the request's and the
	// response's alike: 1024 (A3). It also bounds every frame the extender
	// streams carry before their raw bytes, the response frame and a probe's
	// attestation and verdict frames, the tcp response's http head, and the v1
	// header length the extender still tells from an http request.
	// Non-positive refuses every header and frame.
	MaxHeaderByteCount int
}

// A3's values, the only source of their defaults. A fresh value each call,
// sharing nothing mutable with any other.
func DefaultExtenderFormatSettings() *ExtenderFormatSettings {
	return &ExtenderFormatSettings{
		MaxHeaderByteCount: 1024,
	}
}

// How a client takes an extender's 429 (A12): how far it believes the
// Retry-After the answer carried, and how a limited backoff is jittered, so the
// clients one limit turned away do not all come back at once. The client's
// ConnectSettings, the directory's settings and an NLayer front's settings each
// carry this one object, so a front jitters a hop's limit as a client jitters
// an extender's, and its defaults stay here (QD-702).
type ExtenderLimitedSettings struct {
	// The most a Retry-After is taken for, in either of RFC 9110's forms: a
	// day, which is past any backoff this client keeps and bounds the
	// arithmetic of a hostile value. Non-positive takes none, so every 429
	// reads as one that carried no Retry-After.
	RetryAfterMax time.Duration
	// How far a limited backoff is jittered either way, as a fraction of it,
	// drawn uniformly: 0.5, A12's +-50 %, so a backoff lands between half and
	// one and a half times itself. 0 or less jitters nothing, and more than 1
	// is taken as 1, so a backoff never falls below zero.
	BackoffJitterFraction float64
}

// A12's values, the only source of their defaults. A fresh value each call,
// sharing nothing mutable with any other.
func DefaultExtenderLimitedSettings() *ExtenderLimitedSettings {
	return &ExtenderLimitedSettings{
		RetryAfterMax:         24 * time.Hour,
		BackoffJitterFraction: 0.5,
	}
}

// The extender protocol's limits this client dials with: its own, else A3's.
func (self *ConnectSettings) extenderFormatSettings() *ExtenderFormatSettings {
	if self.ExtenderFormatSettings != nil {
		return self.ExtenderFormatSettings
	}
	return DefaultExtenderFormatSettings()
}

// How this client takes an extender's 429: its own settings, else A12's.
func (self *ConnectSettings) extenderLimitedSettings() *ExtenderLimitedSettings {
	if self.ExtenderLimitedSettings != nil {
		return self.ExtenderLimitedSettings
	}
	return DefaultExtenderLimitedSettings()
}

// The carrier name of a connect mode, as it appears on the wire.
func ExtenderCarrierForConnectMode(connectMode ExtenderConnectMode) string {
	switch connectMode {
	case ExtenderConnectModeQuic:
		return ExtenderCarrierQuic
	case ExtenderConnectModeDns:
		return ExtenderCarrierDns
	case ExtenderConnectModeWebRtc:
		return ExtenderCarrierWebRtc
	default:
		return ExtenderCarrierTcp
	}
}

// One carrier endpoint guess. Fragment and reorder apply to tcp only, and
// DnsTld only to the dns carrier. Comparable, so the strategy can hold a
// visited set of profiles.
type ExtenderProfile struct {
	ConnectMode ExtenderConnectMode
	ServerName  string
	Port        int
	Fragment    bool
	Reorder     bool
	DnsTld      string
}

type ExtenderConfig struct {
	Profile ExtenderProfile
	Ip      netip.Addr
	Secret  string
	// PublicKey, when set, is the extender identity key from a verified
	// record. The outer leaf certificate must be signed by it (B3). Empty
	// keeps the unauthenticated outer TLS of a manually configured extender.
	PublicKey []byte
	// RealityPublicKey, when set, is the 32 byte X25519 static public key of
	// the extender camouflage from a verified record (EXTENDER.md P1, P6,
	// field 15). The tcp carrier then runs a camo-first-then-legacy staggered
	// race (P8): the camouflaged attempt fronts a borrowed name with an
	// authenticated browser hello sealed under this key, the legacy attempt is
	// today's Go-TLS dial. The camouflaged attempt is tried only when the
	// identity PublicKey is also known (the B3 check needs it) and the kill
	// switch ConnectSettings.TlsClientHelloFingerprint is not "go". Empty, or
	// an identity-only config, dials the legacy attempt alone.
	RealityPublicKey []byte
	// The country the directory places this client in (SpoofCountryCode),
	// whose borrow list the camouflaged attempt selects its front name from
	// (EXTENDER-UPDATE2 §3.1). Empty selects from the global list.
	CamouflageCountryCode string
	// The ports one dns carrier dial races, in launch order (L2), with
	// Profile.Port the first of them. A config drawn from a directory or
	// manual address carries the ports its record lists and then whichever
	// of 4053 and 53 it does not (ExtenderCandidate.dnsCarrierPorts). Empty
	// dials Profile.Port alone, which is what an exact endpoint names:
	// ExtenderConfigs, an NLayer hop, the operator's probe of one port.
	DnsPorts []int
}

func NewExtenderHttpClient(
	connectSettings *ConnectSettings,
	extenderConfig *ExtenderConfig,
) *http.Client {
	transport := &http.Transport{
		DialTLSContext:    newExtenderDialTlsContext(connectSettings, extenderConfig, clientHttpNextProtos),
		ForceAttemptHTTP2: true,
		HTTP2:             nativeHttp2Config(connectSettings),
	}
	return &http.Client{
		Transport: transport,
		Timeout:   connectSettings.RequestTimeout,
	}
}

// One extender request (A3, A4). The zero value forwards to the destination
// with no challenge, which is what an ordinary dial sends.
type ExtenderDial struct {
	DestinationHost string
	DestinationPort int
	// 32 random bytes; the response carries the signature over it. Probes and
	// activation set it, an ordinary dial leaves it empty.
	Challenge []byte
	// 0 forward, 1 gossip, 2 feed, 3 probe. DestinationHost is ignored when
	// set.
	Service uint32
	// Datagram asks the extender to relay udp datagrams to the destination
	// rather than a stream. The carrier is still one reliable byte stream; the
	// datagrams are framed on it (net_extender_datagram.go).
	Datagram bool
	// Probe only: the 16 byte client id of an attesting PROVIDER, which asks
	// the extender for a nonce (DESIGNNOTES4.md). Empty for a ranking probe,
	// which identifies itself to the extender no more than a forward does.
	ProbeClientId []byte
	// Probe only: the 32 byte identity key of an attesting EXTENDER, which
	// asks for a nonce the same way (GEOMAP §2.2). At most one of the two is
	// set; the extender refuses a header that names both.
	ProbeExtenderPublicKey []byte
	// How many extenders the request has already crossed (A11). A client
	// leaves it 0; an NLayer extender sends one more than the header it
	// received.
	HopCount uint32
	// RoundTrip, when set, receives the send time of the request and the
	// receive time of the response. Every dial leaves it nil but a probe.
	RoundTrip *ExtenderRoundTrip
}

// The timing of one extender request over an established carrier: the
// request is written after the outer handshake and the response is the first
// thing read after it, so the two marks bracket exactly one round trip plus
// the extender's handling of the header. A probe reads its rtt from here
// (DESIGNNOTES4.md).
type ExtenderRoundTrip struct {
	SendTime    time.Time
	ReceiveTime time.Time
}

func (self *ExtenderRoundTrip) markSend() {
	if self != nil {
		self.SendTime = time.Now()
	}
}

func (self *ExtenderRoundTrip) markReceive() {
	if self != nil {
		self.ReceiveTime = time.Now()
	}
}

// Rtt is the measured round trip, zero until both marks are set. The marks
// carry the monotonic clock, so a wall clock step cannot make it negative.
func (self *ExtenderRoundTrip) Rtt() time.Duration {
	if self == nil || self.SendTime.IsZero() || self.ReceiveTime.IsZero() {
		return 0
	}
	return self.ReceiveTime.Sub(self.SendTime)
}

// NewExtenderDialContext returns a PLAIN dial through the extender: the raw
// stream the extender relays to (destinationHost, destinationPort), with no
// inner TLS on top.
//
// The extender passes the inner stream through verbatim, so a caller that does
// not want TLS to the destination -- plain http:// or ws://, which have no tls
// dialer to reach this layer through -- gets an ordinary net.Conn here and
// speaks whatever it likes over it. Without this, those schemes silently
// bypassed every extender and dialed the destination directly.
func NewExtenderDialContext(
	connectSettings *ConnectSettings,
	extenderConfig *ExtenderConfig,
) DialContextFunction {
	return newExtenderDialContext(connectSettings, extenderConfig)
}

func newExtenderDialContext(
	connectSettings *ConnectSettings,
	extenderConfig *ExtenderConfig,
) DialContextFunction {
	// one outer config per dialer, so its session cache is not shared with
	// any other egress path
	extenderTlsConfig := newExtenderTlsConfig(extenderConfig)
	return func(
		ctx context.Context,
		network string,
		address string,
	) (net.Conn, error) {
		if network != "tcp" {
			panic(fmt.Errorf("Extender only support tcp network."))
		}

		host, portStr, err := net.SplitHostPort(address)
		if err != nil {
			panic(err)
		}
		port, err := strconv.Atoi(portStr)
		if err != nil {
			panic(err)
		}

		// the outer name is the dialer's spoof name, and nothing at all with
		// an empty spoof list (A10). The destination is deliberately not
		// substituted: it is the operator name this whole layer exists to keep
		// out of the outer ClientHello
		serverConn, _, err := dialExtenderStream(
			ctx,
			connectSettings,
			extenderConfig,
			&ExtenderDial{
				DestinationHost: host,
				DestinationPort: port,
			},
			extenderTlsConfig,
		)
		if err != nil {
			return nil, err
		}
		return serverConn, nil
	}
}

// create a tls connect to (destinationHost, destinationPort) on the connection
// returned by this
// the returned connection is not a tls connection
func NewExtenderDialTlsContext(
	connectSettings *ConnectSettings,
	extenderConfig *ExtenderConfig,
) DialTlsContextFunction {
	return newExtenderDialTlsContext(connectSettings, extenderConfig, nil)
}

// The tls dial is the plain dial with the inner handshake on top: the extender
// relays the inner stream verbatim, so there is nothing tls-specific about
// reaching the destination, only about what is spoken once it is reached.
func newExtenderDialTlsContext(
	connectSettings *ConnectSettings,
	extenderConfig *ExtenderConfig,
	nextProtos []string,
) DialTlsContextFunction {
	dialContext := newExtenderDialContext(connectSettings, extenderConfig)
	innerBaseTlsConfig := newClientTlsConfig(connectSettings.TlsConfig, nextProtos)
	return func(
		ctx context.Context,
		network string,
		address string,
	) (net.Conn, error) {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			panic(err)
		}

		serverConn, err := dialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}

		innerTlsConfig := innerBaseTlsConfig.Clone()
		if innerTlsConfig.ServerName == "" {
			innerTlsConfig.ServerName = host
		}
		ownedConn, err := ownClientHttpPoolConn(ctx, serverConn)
		if err != nil {
			return nil, err
		}
		tlsServerConn := tls.Client(ownedConn, innerTlsConfig)

		// inner handshake; bound the timeout so a slow/malicious extender cannot
		// hold the dial open indefinitely
		innerSuccess := false
		defer func() {
			if !innerSuccess {
				tlsServerConn.Close()
			}
		}()
		func() {
			innerCtx, innerCancel := context.WithTimeout(ctx, connectSettings.TlsTimeout)
			defer innerCancel()
			err = tlsServerConn.HandshakeContext(innerCtx)
		}()
		if err != nil {
			return nil, err
		}
		innerSuccess = true

		return tlsServerConn, nil
	}
}

// DialExtender opens one carrier, performs the extender request and returns
// the raw stream that follows the response. The caller owns the returned
// connection, which for the udp carriers also owns the QUIC connection and its
// socket. Phase 1b probes and the gossip and feed clients use this directly;
// an ordinary dial goes through NewExtenderDialTlsContext, which runs the
// inner TLS on top.
func DialExtender(
	ctx context.Context,
	connectSettings *ConnectSettings,
	extenderConfig *ExtenderConfig,
	extenderDial *ExtenderDial,
) (net.Conn, *protocol.ExtenderResponse, error) {
	return dialExtenderStream(
		ctx,
		connectSettings,
		extenderConfig,
		extenderDial,
		newExtenderTlsConfig(extenderConfig),
	)
}

// The outer TLS configuration of one extender dialer. The spoof name is
// presented as the sni, an empty name presents no sni at all (A10), the
// self-signed leaf is never checked against a root, and 1.3 is required so the
// certificate is encrypted on the wire.
func newExtenderTlsConfig(extenderConfig *ExtenderConfig) *tls.Config {
	tlsConfig := newClientTlsConfig(&tls.Config{
		ServerName:         extenderConfig.Profile.ServerName,
		InsecureSkipVerify: true,
		// require 1.3 to mask self-signed certs
		MinVersion: tls.VersionTLS13,
	}, nil)
	if 0 < len(extenderConfig.PublicKey) {
		tlsConfig.VerifyPeerCertificate = newExtenderLeafVerifier(extenderConfig.PublicKey)
	}
	return tlsConfig
}

// The B3 outer check: the presented leaf must be signed by the extender
// identity key from the record. No chain is built and no root is consulted,
// because the extender certificate is issued for a name it does not own.
func newExtenderLeafVerifier(extenderPublicKey []byte) func([][]byte, [][]*x509.Certificate) error {
	publicKey := ed25519.PublicKey(append([]byte(nil), extenderPublicKey...))
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(publicKey) != ed25519.PublicKeySize {
			return fmt.Errorf("extender public key is not an ed25519 key")
		}
		if len(rawCerts) == 0 {
			return fmt.Errorf("extender presented no certificate")
		}
		leaf, err := x509.ParseCertificate(rawCerts[0])
		if err != nil {
			return err
		}
		if leaf.SignatureAlgorithm != x509.PureEd25519 {
			return fmt.Errorf("extender leaf is signed with %s, expected ed25519", leaf.SignatureAlgorithm)
		}
		if !ed25519.Verify(publicKey, leaf.RawTBSCertificate, leaf.Signature) {
			return fmt.Errorf("extender leaf is not signed by the record key")
		}
		return nil
	}
}

// Establishes the carrier, sends the A3 request and reads the response. On
// success the returned connection is positioned at the first byte after the
// response.
func dialExtenderStream(
	ctx context.Context,
	connectSettings *ConnectSettings,
	extenderConfig *ExtenderConfig,
	extenderDial *ExtenderDial,
	extenderTlsConfig *tls.Config,
) (net.Conn, *protocol.ExtenderResponse, error) {
	if extenderConfig.Profile.ConnectMode == ExtenderConnectModeWebRtc {
		// the webrtc carrier builds its own header: with no outer leaf to
		// pin, it challenges the extender itself when the config names its
		// key (net_extender_webrtc.go)
		return dialExtenderWebRtc(ctx, connectSettings, extenderConfig, extenderDial)
	}

	headerBytes, err := extenderRequestHeaderBytes(connectSettings.extenderFormatSettings(), extenderConfig, extenderDial)
	if err != nil {
		return nil, nil, err
	}

	switch extenderConfig.Profile.ConnectMode {
	case ExtenderConnectModeTcpTls:
		return dialExtenderTcp(ctx, connectSettings, extenderConfig, extenderTlsConfig, headerBytes, extenderDial.RoundTrip)
	case ExtenderConnectModeQuic:
		return dialExtenderQuic(ctx, connectSettings, extenderConfig, extenderTlsConfig, headerBytes, extenderDial.RoundTrip)
	case ExtenderConnectModeDns:
		return dialExtenderDns(ctx, connectSettings, extenderConfig, extenderTlsConfig, headerBytes, extenderDial.RoundTrip)
	default:
		return nil, nil, fmt.Errorf("bad connect mode %s", extenderConfig.Profile.ConnectMode)
	}
}

// The serialized request header, with the hmac over timestamp and nonce when
// the extender is private, and never longer than the format's header limit.
func extenderRequestHeaderBytes(
	formatSettings *ExtenderFormatSettings,
	extenderConfig *ExtenderConfig,
	extenderDial *ExtenderDial,
) ([]byte, error) {
	header := &protocol.ExtenderHeader{
		DestinationHost:        extenderDial.DestinationHost,
		DestinationPort:        uint32(extenderDial.DestinationPort),
		Timestamp:              uint64(time.Now().UnixMilli()),
		Challenge:              extenderDial.Challenge,
		Service:                extenderDial.Service,
		Datagram:               extenderDial.Datagram,
		ProbeClientId:          extenderDial.ProbeClientId,
		ProbeExtenderPublicKey: extenderDial.ProbeExtenderPublicKey,
		HopCount:               extenderDial.HopCount,
	}
	if extenderConfig.Secret != "" {
		nonce := NewId()
		header.Nonce = nonce.Bytes()

		mac := hmac.New(sha256.New, []byte(extenderConfig.Secret))
		timestampBytes := make([]byte, 8)
		binary.BigEndian.PutUint64(timestampBytes[0:8], header.Timestamp)
		mac.Write(timestampBytes)
		mac.Write(header.Nonce)
		header.Signature = mac.Sum(nil)
	}

	headerMessageBytes, err := ProtoMarshal(header)
	if err != nil {
		return nil, err
	}
	defer MessagePoolReturn(headerMessageBytes)
	if formatSettings.MaxHeaderByteCount < len(headerMessageBytes) {
		return nil, fmt.Errorf("extender header is %d bytes, at most %d", len(headerMessageBytes), formatSettings.MaxHeaderByteCount)
	}
	return append([]byte(nil), headerMessageBytes...), nil
}

// The A3 request. The body is written separately on the udp carriers, where
// the request stream carries it as http3 DATA frames, so the body is optional
// here while the content length is not.
func newExtenderRequest(
	extenderConfig *ExtenderConfig,
	headerBytes []byte,
	body io.ReadCloser,
) *http.Request {
	authorityHost := extenderConfig.Ip.String()
	if !extenderConfig.Ip.IsValid() {
		// the webrtc carrier has no ip: the request rides an already
		// authenticated data channel, and the extender reads only the
		// method, path and content type of it (A3)
		authorityHost = ExtenderCarrierForConnectMode(extenderConfig.Profile.ConnectMode)
	}
	authority := net.JoinHostPort(
		authorityHost,
		strconv.Itoa(extenderConfig.Profile.Port),
	)
	requestHost := extenderConfig.Profile.ServerName
	if requestHost == "" {
		requestHost = authority
	}
	return &http.Request{
		Method: http.MethodPost,
		URL: &url.URL{
			Scheme: "https",
			Host:   authority,
			Path:   "/",
		},
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Host:       requestHost,
		Header: http.Header{
			"Content-Type": []string{ExtenderContentType},
		},
		Body:          body,
		ContentLength: int64(len(headerBytes)),
	}
}

// The tcp carrier: terminated outer TLS, then one HTTP/1.1 request. The
// buffered reader that read the response is kept, because it may already hold
// the first bytes the extender sent after it.
//
// One tcp carrier memory reservation covers the whole dial, held once however
// the dial reaches the extender (P8): when the config carries a camouflage key
// the dial is a camo-first-then-legacy staggered race, and the winning
// attempt's stream is wrapped in that one reservation, so a camouflaged attempt
// that fails neither holds the address nor spends a second carrier slot.
func dialExtenderTcp(
	ctx context.Context,
	connectSettings *ConnectSettings,
	extenderConfig *ExtenderConfig,
	extenderTlsConfig *tls.Config,
	headerBytes []byte,
	roundTrip *ExtenderRoundTrip,
) (net.Conn, *protocol.ExtenderResponse, error) {
	reservation, err := acquireExtenderTcpMemory(ctx)
	if err != nil {
		return nil, nil, err
	}
	owned := false
	defer func() {
		if !owned {
			reservation.Release()
		}
	}()

	legacyDial := func(ctx context.Context, roundTrip *ExtenderRoundTrip) (net.Conn, *protocol.ExtenderResponse, error) {
		return dialExtenderTcpLegacyConn(ctx, connectSettings, extenderConfig, extenderTlsConfig, headerBytes, roundTrip)
	}

	var streamConn net.Conn
	var response *protocol.ExtenderResponse
	if frontName, _ := extenderCamouflageDecision(connectSettings, extenderConfig); frontName != "" {
		camoDial := func(ctx context.Context, roundTrip *ExtenderRoundTrip) (net.Conn, *protocol.ExtenderResponse, error) {
			return dialExtenderTcpCamouflageConn(ctx, connectSettings, extenderConfig, frontName, headerBytes, roundTrip)
		}
		streamConn, response, err = raceExtenderTcpCamouflage(ctx, roundTrip, extenderTcpCamouflageStaggerC, camoDial, legacyDial)
	} else {
		streamConn, response, err = legacyDial(ctx, roundTrip)
	}
	if err != nil {
		return nil, nil, err
	}

	owned = true
	return &extenderBudgetConn{
		Conn:        streamConn,
		reservation: reservation,
	}, response, nil
}

// The legacy tcp attempt: today's Go-TLS dial to the extender ip, terminated
// outer TLS, then the A3 exchange. It returns the raw stream; the carrier's one
// memory reservation is wrapped around the winning attempt by dialExtenderTcp,
// so this neither acquires nor releases it.
func dialExtenderTcpLegacyConn(
	ctx context.Context,
	connectSettings *ConnectSettings,
	extenderConfig *ExtenderConfig,
	extenderTlsConfig *tls.Config,
	headerBytes []byte,
	roundTrip *ExtenderRoundTrip,
) (net.Conn, *protocol.ExtenderResponse, error) {
	authority := net.JoinHostPort(
		extenderConfig.Ip.String(),
		strconv.Itoa(extenderConfig.Profile.Port),
	)

	// Deliberately NOT routed through dialControlTlsWithFamilyFallback,
	// unlike the normal and resilient dialers. `authority` is built
	// from extenderConfig.Ip, a netip.Addr, so it is always an IP
	// LITERAL: the family is fixed by the address, there is no other
	// family to retry onto -- `dial tcp6 1.1.1.1:443` is "no suitable
	// address found" -- and there is no name resolution whose family
	// choice a strike could inform. controlDialNetwork leaves literal
	// dials unnarrowed for the same reason, which is what keeps this
	// whole fallback layer alive under a demotion.
	conn, err := connectSettings.DialContext(ctx, "tcp", authority)
	if err != nil {
		return nil, nil, err
	}
	// close the underlying conn on any failure path before we return a
	// successful serverConn to the caller
	success := false
	defer func() {
		if !success {
			conn.Close()
		}
	}()

	var serverConn net.Conn
	if extenderConfig.Profile.Fragment || extenderConfig.Profile.Reorder {
		rconn := NewResilientTlsConn(conn, extenderConfig.Profile.Fragment, extenderConfig.Profile.Reorder)
		tlsServerConn := tls.Client(rconn, extenderTlsConfig)

		func() {
			tlsCtx, tlsCancel := context.WithTimeout(ctx, connectSettings.TlsTimeout)
			defer tlsCancel()
			err = tlsServerConn.HandshakeContext(tlsCtx)
		}()
		if err != nil {
			return nil, nil, err
		}
		// once the stream is established, no longer need the resilient features
		if err := offResilientTlsConn(ctx, rconn, connectSettings.ConnectTimeout); err != nil {
			return nil, nil, err
		}

		serverConn = tlsServerConn
	} else {
		tlsServerConn := tls.Client(conn, extenderTlsConfig)

		func() {
			tlsCtx, tlsCancel := context.WithTimeout(ctx, connectSettings.TlsTimeout)
			defer tlsCancel()
			err = tlsServerConn.HandshakeContext(tlsCtx)
		}()
		if err != nil {
			return nil, nil, err
		}

		serverConn = tlsServerConn
	}

	// the extender's Date is an authenticated time source only when the leaf
	// was checked under the record key (B3)
	streamConn, response, err := extenderStreamRequest(
		ctx,
		connectSettings,
		extenderConfig,
		serverConn,
		headerBytes,
		roundTrip,
		newExtenderCamouflageTimeSource(extenderConfig, extenderTlsConfig != nil && extenderTlsConfig.VerifyPeerCertificate != nil),
	)
	if err != nil {
		return nil, nil, err
	}

	success = true
	return streamConn, response, nil
}

// The A3 exchange of the stream carriers on an established, terminated
// stream: one HTTP/1.1 request carrying the header, then the response frame.
// The tcp carrier runs it after its outer tls and the webrtc carrier after its
// data channel opens (net_extender_webrtc.go), so both are refused, limited
// and answered by exactly the same extender handler. On success the returned
// connection is positioned at the first byte after the response; it keeps the
// buffered reader, which may already hold the first bytes the extender sent
// after the response. On an error the caller still owns serverConn.
//
// timeSource, when set, is the record identity the stream's outer leaf already
// verified under (B3): the response's Date then corrects the camouflage clock
// for that extender (net_extender_camouflage_clock.go). A stream not verified
// before the response — no record key, or the webrtc carrier, whose challenge
// is checked after it — passes nil.
func extenderStreamRequest(
	ctx context.Context,
	connectSettings *ConnectSettings,
	extenderConfig *ExtenderConfig,
	serverConn net.Conn,
	headerBytes []byte,
	roundTrip *ExtenderRoundTrip,
	timeSource *extenderCamouflageTimeSource,
) (net.Conn, *protocol.ExtenderResponse, error) {
	request := newExtenderRequest(
		extenderConfig,
		headerBytes,
		io.NopCloser(bytes.NewReader(headerBytes)),
	)
	var sendTime time.Time
	if err := withConnWritePhaseDeadline(ctx, serverConn, connectSettings.ConnectTimeout, func() error {
		roundTrip.markSend()
		sendTime = extenderCamouflageLocalNow()
		return request.Write(serverConn)
	}); err != nil {
		return nil, nil, err
	}

	// the reader may buffer past the response; it becomes the read side of
	// the returned connection
	formatSettings := connectSettings.extenderFormatSettings()
	headerReader := newExtenderResponseHeaderReader(serverConn, formatSettings)
	reader := bufio.NewReader(headerReader)
	var response *protocol.ExtenderResponse
	if err := withConnReadPhaseDeadline(ctx, serverConn, connectSettings.ConnectTimeout, func() error {
		httpResponse, err := http.ReadResponse(reader, request)
		if err != nil {
			return err
		}
		timeSource.observe(httpResponse.Header, sendTime, extenderCamouflageLocalNow())
		headerReader.remaining = -1
		if httpResponse.StatusCode == http.StatusTooManyRequests {
			return &ExtenderLimitedError{RetryAfter: extenderRetryAfter(connectSettings.extenderLimitedSettings(), httpResponse.Header, time.Now())}
		}
		if httpResponse.StatusCode != http.StatusOK {
			return &ExtenderRefusedError{StatusCode: httpResponse.StatusCode}
		}
		// A stream extender sends exactly one bounded, length-delimited frame
		// before handing the connection over. Reject chunked bodies/trailers:
		// Body.Close would otherwise parse unbounded trailer MIME headers.
		if len(httpResponse.TransferEncoding) != 0 || httpResponse.ContentLength < 4 || httpResponse.ContentLength > int64(4+formatSettings.MaxHeaderByteCount) {
			return fmt.Errorf("extender response must have a bounded content length")
		}
		defer httpResponse.Body.Close()
		response, err = ReadExtenderResponseFrame(formatSettings, httpResponse.Body)
		if err == nil {
			roundTrip.markReceive()
		}
		return err
	}); err != nil {
		return nil, nil, err
	}
	return newBufferedConn(serverConn, reader), response, nil
}

// The udp carriers: one QUIC connection with ALPN h3 straight to the extender
// ip, the dns carrier over the packet translation first, then one H3 request
// stream that becomes the byte stream. It dials Profile.Port alone; the dns
// carrier's ports are raced over it by dialExtenderDns.
func dialExtenderQuic(
	ctx context.Context,
	connectSettings *ConnectSettings,
	extenderConfig *ExtenderConfig,
	extenderTlsConfig *tls.Config,
	headerBytes []byte,
	roundTrip *ExtenderRoundTrip,
) (net.Conn, *protocol.ExtenderResponse, error) {
	if !extenderConfig.Ip.IsValid() {
		return nil, nil, fmt.Errorf("extender address is not valid")
	}
	udpAddr := net.UDPAddrFromAddrPort(netip.AddrPortFrom(
		extenderConfig.Ip,
		uint16(extenderConfig.Profile.Port),
	))

	policy := newExtenderQuicMemoryPolicy(ctx, connectSettings)
	var ptSettings *PacketTranslationSettings
	if extenderConfig.Profile.ConnectMode == ExtenderConnectModeDns {
		ptSettings = policy.packetTranslationSettings()
	}
	reservation, err := policy.acquire(ctx)
	if err != nil {
		return nil, nil, err
	}
	// Reverse-order cleanup releases the claim only after the connection,
	// QUIC transport, translation and raw socket have all stopped owning bytes.
	closers := []func(){reservation.Release}
	success := false
	defer func() {
		if !success {
			for i := len(closers) - 1; 0 <= i; i -= 1 {
				closers[i]()
			}
		}
	}()

	packetConn, err := openExtenderPacketConn(ctx, connectSettings, udpAddr)
	if err != nil {
		// ownership transfers for every non-nil result, including a
		// rejected one
		if packetConn != nil {
			packetConn.Close()
		}
		return nil, nil, err
	}
	if packetConn == nil {
		return nil, nil, fmt.Errorf("extender packet connection factory returned nil")
	}
	packetConn = capPlatformPacketConn(packetConn, policy.readBufferByteCount, policy.writeBufferByteCount)
	closers = append(closers, func() { packetConn.Close() })

	if extenderConfig.Profile.ConnectMode == ExtenderConnectModeDns {
		tld := extenderConfig.Profile.DnsTld
		if tld == "" {
			tld = DefaultExtenderDnsTld
		}
		ptSettings.Log = connectSettings.Log
		ptSettings.DnsTlds = [][]byte{[]byte(tld)}
		// The connection cleanup owns the translated PacketConn. Keep its
		// encoder alive while cancellation closes QUIC gracefully, exactly as
		// the platform h3 dns carrier does.
		translation, err := NewPacketTranslation(
			context.WithoutCancel(ctx),
			PacketTranslationModeDns,
			packetConn,
			ptSettings,
		)
		if err != nil {
			return nil, nil, err
		}
		packetConn = translation
		closers = append(closers, func() { translation.Close() })
	}

	quicTransport := &quic.Transport{
		Conn: packetConn,
	}
	closers = append(closers, func() { quicTransport.Close() })

	// with an empty spoof list the outer name is empty (A10). quic-go fills an
	// empty ServerName with the ip literal it is dialing, and crypto/tls omits
	// an ip literal from the sni extension, so the ClientHello still carries no
	// name -- the same shape the tcp carrier sends
	quicTlsConfig := extenderTlsConfig.Clone()
	quicTlsConfig.NextProtos = []string{http3.NextProtoH3}
	installQuicSendFlight(policy.quicConfig)
	quicConn, err := quicTransport.Dial(ctx, udpAddr, quicTlsConfig, policy.quicConfig)
	if err != nil {
		return nil, nil, err
	}
	closers = append(closers, func() { quicConn.CloseWithError(0, "") })
	flight := quicSendFlightForConn(quicConn)
	flight.bind(quicConn)

	formatSettings := connectSettings.extenderFormatSettings()
	// http3 takes a non-positive limit as its own default, so the request
	// header's check above is what refuses one; this stays bounded either way
	h3Transport := &http3.Transport{MaxResponseHeaderBytes: max(1, formatSettings.MaxHeaderByteCount)}
	clientConn := h3Transport.NewClientConn(quicConn)
	stream, err := clientConn.OpenRequestStream(ctx)
	if err != nil {
		return nil, nil, err
	}
	writer := flight.newWriter(stream)

	// http3 rejects a request stream header that carries a body, so the body
	// is written as DATA frames after it; the content length still describes
	// it
	request := newExtenderRequest(extenderConfig, headerBytes, http.NoBody)
	deadline := time.Now().Add(connectSettings.ConnectTimeout)
	if requestDeadline, ok := ctx.Deadline(); ok && requestDeadline.Before(deadline) {
		deadline = requestDeadline
	}
	if err := stream.SetDeadline(deadline); err != nil {
		return nil, nil, err
	}
	if err := writer.SetWriteDeadline(deadline); err != nil {
		return nil, nil, err
	}
	roundTrip.markSend()
	sendTime := extenderCamouflageLocalNow()
	if err := stream.SendRequestHeader(request); err != nil {
		return nil, nil, err
	}
	if _, err := writer.Write(headerBytes); err != nil {
		return nil, nil, err
	}
	httpResponse, err := stream.ReadResponse()
	if err != nil {
		return nil, nil, err
	}
	// the quic handshake checked the leaf under the record key when the config
	// carries one (B3), so the extender's Date corrects the camouflage clock
	// for it; the tcp carrier's camouflaged attempt then seals in its time
	newExtenderCamouflageTimeSource(extenderConfig, quicTlsConfig.VerifyPeerCertificate != nil).observe(
		httpResponse.Header,
		sendTime,
		extenderCamouflageLocalNow(),
	)
	if httpResponse.StatusCode == http.StatusTooManyRequests {
		return nil, nil, &ExtenderLimitedError{RetryAfter: extenderRetryAfter(connectSettings.extenderLimitedSettings(), httpResponse.Header, time.Now())}
	}
	if httpResponse.StatusCode != http.StatusOK {
		return nil, nil, &ExtenderRefusedError{StatusCode: httpResponse.StatusCode}
	}
	response, err := ReadExtenderResponseFrame(formatSettings, stream)
	if err != nil {
		return nil, nil, err
	}
	roundTrip.markReceive()
	if err := stream.SetDeadline(time.Time{}); err != nil {
		return nil, nil, err
	}
	if err := writer.SetWriteDeadline(time.Time{}); err != nil {
		return nil, nil, err
	}

	success = true
	return newStreamConn(
		stream,
		writer,
		packetConn.LocalAddr(),
		udpAddr,
		closers,
	), response, nil
}

// An extender that answered the request with a status other than 200 (A4):
// it was reached and would not carry this request, which
// is a different fact from an extender that could not be reached at all. An
// NLayer extender holds a hop only for the second (A11), since a refusal can
// be about the request -- its destination or its depth -- rather than the
// extender.
type ExtenderRefusedError struct {
	StatusCode int
}

// Implements error.
func (self *ExtenderRefusedError) Error() string {
	return fmt.Sprintf("extender refused the request with status %d", self.StatusCode)
}

// An extender that answered 429: it is over one of its admission limits (A12)
// and will take the request later. It is not a failure
// of the extender -- no failure count, no hold -- but a backoff: the caller
// leaves the address alone for about RetryAfter and tries other extenders
// first.
type ExtenderLimitedError struct {
	// The Retry-After the answer carried, zero when it carried none.
	RetryAfter time.Duration
}

// Implements error.
func (self *ExtenderLimitedError) Error() string {
	if self.RetryAfter <= 0 {
		return "extender limited the request"
	}
	return fmt.Sprintf("extender limited the request; retry after %s", self.RetryAfter)
}

// The Retry-After of a 429, in either form RFC 9110 allows -- seconds, or a
// date -- and zero when there is none or it does not parse, or has passed. It
// is never taken for longer than the settings' RetryAfterMax.
func extenderRetryAfter(limitedSettings *ExtenderLimitedSettings, header http.Header, now time.Time) time.Duration {
	value := strings.TrimSpace(header.Get("Retry-After"))
	if value == "" || limitedSettings.RetryAfterMax <= 0 {
		return 0
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds <= 0 {
			return 0
		}
		// whole seconds of the bound, so the product cannot overflow
		maxSeconds := int64(limitedSettings.RetryAfterMax / time.Second)
		return time.Duration(min(seconds, maxSeconds)) * time.Second
	}
	if date, err := http.ParseTime(value); err == nil {
		return max(0, min(date.Sub(now), limitedSettings.RetryAfterMax))
	}
	return 0
}

// How long to leave a limited extender alone (A12): the Retry-After it
// answered with, or defaultBackoff when it gave none, jittered uniformly by
// BackoffJitterFraction either way, so the clients one limit turned away do
// not all come back at once. Zero when there is no backoff at all.
func (self *ExtenderLimitedSettings) JitterBackoff(retryAfter time.Duration, defaultBackoff time.Duration) time.Duration {
	backoff := retryAfter
	if backoff <= 0 {
		backoff = defaultBackoff
	}
	if backoff <= 0 {
		return 0
	}
	fraction := min(max(self.BackoffJitterFraction, 0), 1)
	// within [0, backoff] whatever the float rounding of a huge backoff did
	spread := min(max(time.Duration(float64(backoff)*fraction), 0), backoff)
	if spread == 0 {
		return backoff
	}
	// uniform over [backoff - spread, backoff + spread], the top cut at the
	// largest duration so a huge backoff cannot wrap
	low := backoff - spread
	high := backoff + spread
	if high < backoff {
		high = math.MaxInt64
	}
	window := int64(high - low)
	if window < math.MaxInt64 {
		window += 1
	}
	return low + time.Duration(mathrand.Int63n(window))
}

// Limit bytes supplied to http.ReadResponse, not just its bufio read-ahead:
// net/http's MIME parser can otherwise grow a single line or header map
// without bound. Once headers are parsed, -1 allows the buffered reader to
// continue as the inner tunnel's read side. All parser roots fit the outer
// H1 claim; at most the wire header limit (ExtenderFormatSettings) plus the
// fixed bufio buffer is read.
type extenderResponseHeaderReader struct {
	reader    io.Reader
	remaining int
	// the limit remaining started from, for the refusal
	maxByteCount int
}

// A reader of at most the format's header limit from reader. A non-positive
// limit reads nothing, never the unbounded -1.
func newExtenderResponseHeaderReader(reader io.Reader, formatSettings *ExtenderFormatSettings) *extenderResponseHeaderReader {
	maxByteCount := max(0, formatSettings.MaxHeaderByteCount)
	return &extenderResponseHeaderReader{
		reader:       reader,
		remaining:    maxByteCount,
		maxByteCount: maxByteCount,
	}
}

func (self *extenderResponseHeaderReader) Read(b []byte) (int, error) {
	if self.remaining == 0 {
		return 0, fmt.Errorf("extender HTTP response headers exceed %d bytes", self.maxByteCount)
	}
	if self.remaining > 0 {
		b = b[:min(len(b), self.remaining)]
	}
	n, err := self.reader.Read(b)
	if self.remaining > 0 {
		self.remaining -= n
	}
	return n, err
}

// One unconnected udp endpoint for a carrier dial, narrowed to the family of
// the extender address. A configured packet endpoint factory wins, so a
// headless host keeps one source identity; its endpoint is the embedder's, with
// its own binding, and is left alone.
//
// Otherwise the socket is made through the egress machinery (listenEgressUdp),
// so both callers, an extender carrier's QUIC dial (the dns carrier rides it
// too) and the alt api's, are covered: the forced interface on Windows and
// macOS (R1) and the egress mark on Linux (QD-800) land on it before it sends,
// and an extender transport never follows the default route of a tunnel this
// process provides back into it. A socket that cannot take them is refused, as
// a dial is, and the carrier dial fails rather than loop (QD-810).
func openExtenderPacketConn(
	ctx context.Context,
	connectSettings *ConnectSettings,
	udpAddr *net.UDPAddr,
) (net.PacketConn, error) {
	if connectSettings.DialContextSettings != nil && connectSettings.DialContextSettings.PacketConnFactory != nil {
		return connectSettings.DialContextSettings.PacketConnFactory(ctx)
	}
	udpNetwork, wildcard := udpWildcardForFamily(udpAddrFamily(udpAddr))
	udpConn, err := listenEgressUdp(ctx, udpNetwork, wildcard, udpAddr)
	if err != nil {
		return nil, err
	}
	return udpConn, nil
}

// The A3 response body: a 4-byte big-endian length and the serialized
// ExtenderResponse, which is at most the format's header limit. The length
// prefix makes the body self-delimiting, which the udp carriers need: there
// the response body and the raw bytes that follow it are the same http3 DATA
// stream, so a content length would bound the reader the caller keeps using.
func ExtenderResponseFrame(formatSettings *ExtenderFormatSettings, response *protocol.ExtenderResponse) ([]byte, error) {
	responseBytes, err := proto.Marshal(response)
	if err != nil {
		return nil, err
	}
	if formatSettings.MaxHeaderByteCount < len(responseBytes) {
		return nil, fmt.Errorf("extender response is %d bytes, at most %d", len(responseBytes), formatSettings.MaxHeaderByteCount)
	}
	frameBytes := make([]byte, 4+len(responseBytes))
	binary.BigEndian.PutUint32(frameBytes[0:4], uint32(len(responseBytes)))
	copy(frameBytes[4:], responseBytes)
	return frameBytes, nil
}

// Reads exactly one response frame, leaving the reader on the first byte that
// follows it. A length past the format's header limit is refused before
// anything is allocated for it.
func ReadExtenderResponseFrame(formatSettings *ExtenderFormatSettings, reader io.Reader) (*protocol.ExtenderResponse, error) {
	lengthBytes := make([]byte, 4)
	if _, err := io.ReadFull(reader, lengthBytes); err != nil {
		return nil, err
	}
	responseByteCount := int(binary.BigEndian.Uint32(lengthBytes))
	if formatSettings.MaxHeaderByteCount < responseByteCount {
		return nil, fmt.Errorf("extender response is %d bytes, at most %d", responseByteCount, formatSettings.MaxHeaderByteCount)
	}
	responseBytes := make([]byte, responseByteCount)
	if _, err := io.ReadFull(reader, responseBytes); err != nil {
		return nil, err
	}
	response := &protocol.ExtenderResponse{}
	if err := proto.Unmarshal(responseBytes, response); err != nil {
		return nil, err
	}
	return response, nil
}

// Bounds a synchronous connection read phase by the earlier of the caller
// deadline and its phase budget, mirroring the write phase helper.
func withConnReadPhaseDeadline(
	ctx context.Context,
	conn net.Conn,
	phaseTimeout time.Duration,
	read func() error,
) (resultErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline, hasDeadline := ctx.Deadline()
	if 0 < phaseTimeout {
		phaseDeadline := time.Now().Add(phaseTimeout)
		if !hasDeadline || phaseDeadline.Before(deadline) {
			deadline = phaseDeadline
			hasDeadline = true
		}
	}
	if !hasDeadline {
		return read()
	}
	if err := conn.SetReadDeadline(deadline); err != nil {
		return err
	}
	defer func() {
		clearErr := conn.SetReadDeadline(time.Time{})
		if resultErr == nil {
			resultErr = clearErr
		}
	}()
	return read()
}

// bufferedConn drains what a buffered reader already took from the connection
// before reading the socket again. The http response parser reads ahead, so
// the first inner bytes can already be in that buffer.
type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func newBufferedConn(conn net.Conn, reader *bufio.Reader) *bufferedConn {
	return &bufferedConn{
		Conn:   conn,
		reader: reader,
	}
}

func (self *bufferedConn) Read(b []byte) (int, error) {
	return self.reader.Read(b)
}

// streamConn adapts one http3 request stream to a connection. It owns the
// QUIC connection, transport and socket underneath, which are released in
// reverse order on Close, so a caller that holds only the stream still frees
// the whole carrier.
type streamConn struct {
	stream     *http3.RequestStream
	writer     *quicSendFlightWriter
	localAddr  net.Addr
	remoteAddr net.Addr
	closers    []func()
	closeOnce  sync.Once
}

func newStreamConn(
	stream *http3.RequestStream,
	writer *quicSendFlightWriter,
	localAddr net.Addr,
	remoteAddr net.Addr,
	closers []func(),
) *streamConn {
	return &streamConn{
		stream:     stream,
		writer:     writer,
		localAddr:  localAddr,
		remoteAddr: remoteAddr,
		closers:    closers,
	}
}

func (self *streamConn) Read(b []byte) (int, error) {
	return self.stream.Read(b)
}

func (self *streamConn) Write(b []byte) (int, error) {
	return self.writer.Write(b)
}

func (self *streamConn) Close() error {
	self.closeOnce.Do(func() {
		self.stream.Close()
		self.stream.CancelRead(quic.StreamErrorCode(http3.ErrCodeNoError))
		for i := len(self.closers) - 1; 0 <= i; i -= 1 {
			self.closers[i]()
		}
	})
	return nil
}

func (self *streamConn) LocalAddr() net.Addr {
	return self.localAddr
}

func (self *streamConn) RemoteAddr() net.Addr {
	return self.remoteAddr
}

func (self *streamConn) SetDeadline(t time.Time) error {
	if err := self.writer.SetWriteDeadline(t); err != nil {
		return err
	}
	return self.stream.SetReadDeadline(t)
}

func (self *streamConn) SetReadDeadline(t time.Time) error {
	return self.stream.SetReadDeadline(t)
}

func (self *streamConn) SetWriteDeadline(t time.Time) error {
	return self.writer.SetWriteDeadline(t)
}

// NewExtenderPacketDialContext returns a dial that yields a net.PacketConn
// reaching (destinationHost, destinationPort) through the extender.
//
// This is the h3 path. An extender relays one reliable byte stream and cannot
// see inside the inner tls, so it cannot reframe a quic stream into anything;
// the datagrams are framed explicitly instead and the extender turns them back
// into udp at the far end. See net_extender_datagram.go for what that
// preserves and what it costs.
func NewExtenderPacketDialContext(
	connectSettings *ConnectSettings,
	extenderConfig *ExtenderConfig,
) DialPacketContextFunction {
	extenderTlsConfig := newExtenderTlsConfig(extenderConfig)
	return func(
		ctx context.Context,
		network string,
		address string,
	) (net.PacketConn, error) {
		switch network {
		case "udp", "udp4", "udp6":
		default:
			return nil, fmt.Errorf("extender packet dial supports udp, not %s", network)
		}

		host, portStr, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		port, err := strconv.Atoi(portStr)
		if err != nil {
			return nil, err
		}

		serverConn, _, err := dialExtenderStream(
			ctx,
			connectSettings,
			extenderConfig,
			&ExtenderDial{
				DestinationHost: host,
				DestinationPort: port,
				Datagram:        true,
			},
			extenderTlsConfig,
		)
		if err != nil {
			return nil, err
		}

		// ReadFrom's address is informational: the extender already owns the
		// destination from the accepted header. Construct it without resolving
		// the hostname here; an unbound resolver would bypass the egress path
		// and can recurse into the tunnel on Windows.
		remote := newExtenderDatagramAddr(network, address)
		return newExtenderPacketConn(serverConn, remote), nil
	}
}
