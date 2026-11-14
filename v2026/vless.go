package connect

// vless.go — VLESS as an additional client strategy.
//
// A VLESS server the user names in the network space carries the strategy's
// connections -- api requests and the platform websocket -- to their real
// destination, for networks where every direct path is blocked. The server
// sees only the destination host and port: the api and platform tls still runs
// end to end inside the stream, so the server can neither read nor alter the
// traffic, and a server that connects somewhere else fails the inner
// certificate check.
//
// One VLESS stream is one tcp connection to the server:
//
//	tcp -> security (none, tls, reality) -> transport (raw tcp, ws, httpupgrade)
//	    -> request header -> optional vision padding -> the inner bytes
//
// The share link (`vless://id@host:port?...#name`) is the interchange format
// every VLESS client reads and writes, and `ParseVlessLink` and `Link`
// round-trip it. Only the parameters this client implements are kept.
// Features it does not implement (grpc, xhttp, kcp, quic, mux, VLESS
// encryption, the http header disguise of raw tcp) are refused when the link
// is read, rather than dialed wrong.
//
// The framing is in vless_stream.go, vision in vless_vision.go, reality in
// vless_reality.go and the dial with its transports in vless_dial.go.

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

const (
	VlessFlowNone   = ""
	VlessFlowVision = "xtls-rprx-vision"

	VlessNetworkTcp         = "tcp"
	VlessNetworkWs          = "ws"
	VlessNetworkHttpUpgrade = "httpupgrade"

	VlessSecurityNone    = "none"
	VlessSecurityTls     = "tls"
	VlessSecurityReality = "reality"
)

// The error codes of `VlessConfigError`, one per thing a user fixes. The sdk
// maps each to a localized message.
const (
	// not a vless:// link at all, or one that does not parse
	VlessErrorLinkInvalid = "link_invalid"
	// a link that asks for a VLESS feature this client does not implement
	VlessErrorLinkUnsupported     = "link_unsupported"
	VlessErrorAddressInvalid      = "address_invalid"
	VlessErrorPortInvalid         = "port_invalid"
	VlessErrorIdInvalid           = "id_invalid"
	VlessErrorNetworkUnsupported  = "network_unsupported"
	VlessErrorSecurityUnsupported = "security_unsupported"
	// an unknown flow, or vision over anything but raw tcp with tls or reality
	VlessErrorFlowInvalid            = "flow_invalid"
	VlessErrorServerNameRequired     = "server_name_required"
	VlessErrorFingerprintUnsupported = "fingerprint_unsupported"
	VlessErrorPublicKeyInvalid       = "public_key_invalid"
	VlessErrorShortIdInvalid         = "short_id_invalid"
)

// What is wrong with a configuration, as a code from the list above.
type VlessConfigError struct {
	Code   string
	Detail string
}

func (self *VlessConfigError) Error() string {
	if self.Detail == "" {
		return fmt.Sprintf("vless: %s", self.Code)
	}
	return fmt.Sprintf("vless: %s: %s", self.Code, self.Detail)
}

// The code of a configuration error, or empty for nil and for errors that are
// not configuration errors.
func VlessConfigErrorCode(err error) string {
	var configErr *VlessConfigError
	if errors.As(err, &configErr) {
		return configErr.Code
	}
	return ""
}

// The tls client hellos a configuration can imitate (`fp=`). "random" picks
// one of the browser hellos per dial; an empty fingerprint keeps the Go tls
// client for tls, while reality always imitates one and takes chrome.
var vlessFingerprints = []string{
	"chrome",
	"firefox",
	"safari",
	"ios",
	"android",
	"edge",
	"360",
	"qq",
	"random",
	"randomized",
}

// One VLESS server. The zero value is not usable; build one with
// `ParseVlessLink` or set the fields and call `Validate`.
type VlessConfig struct {
	// the share link's fragment, a label for people
	Name string

	// host name or ip literal of the server
	Address string
	Port    int
	// the user id: a uuid, or a short text that maps to one the way Xray maps
	// it (`vlessId`)
	Id string
	// `VlessFlowNone` or `VlessFlowVision`
	Flow string

	// one of the VlessNetwork values
	Network string
	// one of the VlessSecurity values
	Security string

	// the tls server name (tls, reality). Empty takes the address for tls when
	// it is a host name, and is refused for reality.
	ServerName string
	// one of `vlessFingerprints`, or empty
	Fingerprint string
	// the outer tls alpn (tls). Empty keeps the default: http/1.1 for the
	// http transports, the imitated hello's own list otherwise.
	Alpns []string
	// skips the outer certificate check (tls). The inner tls to the
	// destination is still verified, so this exposes only the VLESS
	// handshake, never the traffic.
	AllowInsecure bool

	// the server's x25519 public key (reality)
	PublicKey []byte
	// the short id, up to 8 bytes (reality)
	ShortId []byte
	// the crawl path of a failed reality check in other clients. Kept so a
	// link round-trips; this client fails the dial instead of crawling.
	SpiderX string

	// the http request path (ws, httpupgrade), "/" when empty. An `ed` early
	// data query is dropped when dialing: the server takes the stream without
	// early data.
	Path string
	// the http host header (ws, httpupgrade). Empty takes the server name,
	// then the address.
	Host string
}

// A deep copy, so a strategy can keep a configuration its caller goes on
// changing.
func (self *VlessConfig) Copy() *VlessConfig {
	if self == nil {
		return nil
	}
	copied := *self
	copied.Alpns = slices.Clone(self.Alpns)
	copied.PublicKey = slices.Clone(self.PublicKey)
	copied.ShortId = slices.Clone(self.ShortId)
	return &copied
}

// The normalized defaults: an empty network is raw tcp and an empty security
// is none, as in a link that leaves them out.
func (self *VlessConfig) network() string {
	if self.Network == "" {
		return VlessNetworkTcp
	}
	return self.Network
}

func (self *VlessConfig) security() string {
	if self.Security == "" {
		return VlessSecurityNone
	}
	return self.Security
}

// The tls server name the outer handshake presents.
func (self *VlessConfig) serverName() string {
	if self.ServerName != "" {
		return self.ServerName
	}
	if _, err := netip.ParseAddr(self.Address); err == nil {
		// an ip literal is not a server name; the handshake presents none
		return ""
	}
	return self.Address
}

// The http host header of the ws and httpupgrade transports. An ipv6 literal
// is bracketed, as a host header and a url carry it.
func (self *VlessConfig) httpHost() string {
	host := self.Host
	if host == "" {
		host = self.serverName()
	}
	if host == "" {
		host = self.Address
	}
	if addr, err := netip.ParseAddr(host); err == nil && addr.Is6() {
		return "[" + host + "]"
	}
	return host
}

// The server's dial address.
func (self *VlessConfig) serverAddress() string {
	return net.JoinHostPort(self.Address, strconv.Itoa(self.Port))
}

// Validate reports the first thing wrong with the configuration, as a
// `VlessConfigError`, or nil when it can be dialed.
func (self *VlessConfig) Validate() error {
	if self == nil {
		return &VlessConfigError{Code: VlessErrorLinkInvalid}
	}
	address := strings.TrimSpace(self.Address)
	if address == "" || address != self.Address || strings.ContainsAny(address, "/?#@ []") {
		return &VlessConfigError{Code: VlessErrorAddressInvalid}
	}
	if self.Port < 1 || 65535 < self.Port {
		return &VlessConfigError{Code: VlessErrorPortInvalid}
	}
	if _, err := vlessId(self.Id); err != nil {
		return &VlessConfigError{Code: VlessErrorIdInvalid}
	}
	network := self.network()
	switch network {
	case VlessNetworkTcp, VlessNetworkWs, VlessNetworkHttpUpgrade:
	default:
		return &VlessConfigError{Code: VlessErrorNetworkUnsupported, Detail: network}
	}
	security := self.security()
	switch security {
	case VlessSecurityNone, VlessSecurityTls, VlessSecurityReality:
	default:
		return &VlessConfigError{Code: VlessErrorSecurityUnsupported, Detail: security}
	}
	switch self.Flow {
	case VlessFlowNone:
	case VlessFlowVision:
		// vision pads the inner tls handshake and lets the server send the
		// rest outside the outer tls, which needs a raw stream under tls
		if network != VlessNetworkTcp || security == VlessSecurityNone {
			return &VlessConfigError{Code: VlessErrorFlowInvalid, Detail: self.Flow}
		}
	default:
		return &VlessConfigError{Code: VlessErrorFlowInvalid, Detail: self.Flow}
	}
	if self.Fingerprint != "" && !slices.Contains(vlessFingerprints, self.Fingerprint) {
		return &VlessConfigError{Code: VlessErrorFingerprintUnsupported, Detail: self.Fingerprint}
	}
	if security == VlessSecurityReality {
		if self.ServerName == "" {
			return &VlessConfigError{Code: VlessErrorServerNameRequired}
		}
		if len(self.PublicKey) != 32 {
			return &VlessConfigError{Code: VlessErrorPublicKeyInvalid}
		}
		if 8 < len(self.ShortId) {
			return &VlessConfigError{Code: VlessErrorShortIdInvalid}
		}
	}
	return nil
}

// The 16-byte user id. A uuid in its 32 hex digits, with or without the four
// dashes, is read as written; any other text of 1 to 30 bytes maps to the
// version 5 uuid of the zero namespace, which is how Xray reads a custom id.
func vlessId(id string) ([16]byte, error) {
	var uuid [16]byte
	text := []byte(id)
	if n := len(text); n < 32 || 36 < n {
		if n == 0 || 30 < n {
			return uuid, errors.New("vless id must be a uuid or 1 to 30 bytes")
		}
		h := sha1.New()
		h.Write(uuid[:])
		h.Write(text)
		sum := h.Sum(nil)
		copy(uuid[:], sum[:16])
		uuid[6] = (uuid[6] & 0x0f) | (5 << 4)
		uuid[8] = (uuid[8] & 0x3f) | 0x80
		return uuid, nil
	}
	b := uuid[:]
	for _, groupLength := range []int{8, 4, 4, 4, 12} {
		if 0 < len(text) && text[0] == '-' {
			text = text[1:]
		}
		if len(text) < groupLength {
			return uuid, errors.New("vless id is not a uuid")
		}
		if _, err := hex.Decode(b[:groupLength/2], text[:groupLength]); err != nil {
			return uuid, errors.New("vless id is not a uuid")
		}
		text = text[groupLength:]
		b = b[groupLength/2:]
	}
	if len(text) != 0 {
		return uuid, errors.New("vless id is not a uuid")
	}
	return uuid, nil
}

// ParseVlessLink reads a `vless://` share link (the Xray share link form,
// XTLS/Xray-core discussion 716) into a validated configuration. The error is
// a `VlessConfigError`.
func ParseVlessLink(link string) (*VlessConfig, error) {
	link = strings.TrimSpace(link)
	linkUrl, err := url.Parse(link)
	if err != nil || !strings.EqualFold(linkUrl.Scheme, "vless") || linkUrl.User == nil || linkUrl.Opaque != "" {
		return nil, &VlessConfigError{Code: VlessErrorLinkInvalid}
	}
	if linkUrl.Path != "" && linkUrl.Path != "/" {
		return nil, &VlessConfigError{Code: VlessErrorLinkInvalid}
	}
	query := linkUrl.Query()
	first := func(keys ...string) string {
		for _, key := range keys {
			if value := strings.TrimSpace(query.Get(key)); value != "" {
				return value
			}
		}
		return ""
	}
	truthy := func(value string) bool {
		switch strings.ToLower(value) {
		case "1", "true", "yes":
			return true
		}
		return false
	}

	config := &VlessConfig{
		Name:    linkUrl.Fragment,
		Address: linkUrl.Hostname(),
		Id:      linkUrl.User.Username(),
	}
	if portText := linkUrl.Port(); portText != "" {
		port, err := strconv.Atoi(portText)
		if err != nil {
			return nil, &VlessConfigError{Code: VlessErrorPortInvalid}
		}
		config.Port = port
	}

	switch encryption := strings.ToLower(first("encryption")); encryption {
	case "", "none":
	default:
		// VLESS encryption (the mlkem768x25519plus schemes) is not implemented
		return nil, &VlessConfigError{Code: VlessErrorLinkUnsupported, Detail: "encryption=" + encryption}
	}

	switch network := strings.ToLower(first("type")); network {
	case "", "tcp", "raw":
		config.Network = VlessNetworkTcp
		switch headerType := strings.ToLower(first("headerType")); headerType {
		case "", "none":
		default:
			return nil, &VlessConfigError{Code: VlessErrorLinkUnsupported, Detail: "headerType=" + headerType}
		}
	case VlessNetworkWs, VlessNetworkHttpUpgrade:
		config.Network = network
		config.Path = query.Get("path")
		config.Host = first("host")
	default:
		return nil, &VlessConfigError{Code: VlessErrorNetworkUnsupported, Detail: network}
	}

	switch security := strings.ToLower(first("security")); security {
	case "", VlessSecurityNone:
		config.Security = VlessSecurityNone
	case VlessSecurityTls, VlessSecurityReality:
		config.Security = security
	default:
		return nil, &VlessConfigError{Code: VlessErrorSecurityUnsupported, Detail: security}
	}

	switch flow := strings.ToLower(first("flow")); flow {
	case "":
		config.Flow = VlessFlowNone
	case VlessFlowVision, VlessFlowVision + "-udp443":
		// the udp443 variant differs only for udp, which this client never carries
		config.Flow = VlessFlowVision
	default:
		return nil, &VlessConfigError{Code: VlessErrorFlowInvalid, Detail: flow}
	}

	if config.Security != VlessSecurityNone {
		config.ServerName = first("sni", "peer")
		config.Fingerprint = strings.ToLower(first("fp"))
	}
	if config.Security == VlessSecurityTls {
		if alpn := first("alpn"); alpn != "" {
			for _, value := range strings.Split(alpn, ",") {
				if value = strings.TrimSpace(value); value != "" {
					config.Alpns = append(config.Alpns, value)
				}
			}
		}
		config.AllowInsecure = truthy(first("allowInsecure", "insecure"))
	}
	if config.Security == VlessSecurityReality {
		publicKey, err := decodeVlessPublicKey(first("pbk"))
		if err != nil {
			return nil, &VlessConfigError{Code: VlessErrorPublicKeyInvalid}
		}
		config.PublicKey = publicKey
		shortId, err := decodeVlessShortId(first("sid"))
		if err != nil {
			return nil, &VlessConfigError{Code: VlessErrorShortIdInvalid}
		}
		config.ShortId = shortId
		config.SpiderX = query.Get("spx")
	}

	if err := config.Validate(); err != nil {
		return nil, err
	}
	return config, nil
}

// The reality public key of a link or a form: base64url without padding as
// Xray prints it, tolerating the padded and standard alphabets other tools
// use.
func decodeVlessPublicKey(text string) ([]byte, error) {
	text = strings.TrimSpace(text)
	for _, encoding := range []*base64.Encoding{
		base64.RawURLEncoding,
		base64.URLEncoding,
		base64.RawStdEncoding,
		base64.StdEncoding,
	} {
		if publicKey, err := encoding.DecodeString(text); err == nil && len(publicKey) == 32 {
			return publicKey, nil
		}
	}
	return nil, errors.New("reality public key must be 32 bytes of base64")
}

// The reality short id of a link or a form: up to 16 hex digits. An odd count
// is read with a leading zero, as a server reads its own list.
func decodeVlessShortId(text string) ([]byte, error) {
	text = strings.TrimSpace(text)
	if 16 < len(text) {
		return nil, errors.New("reality short id is at most 16 hex digits")
	}
	if len(text)%2 == 1 {
		text = "0" + text
	}
	return hex.DecodeString(text)
}

// The reality public key in the form a link and a form carry it.
func EncodeVlessPublicKey(publicKey []byte) string {
	return base64.RawURLEncoding.EncodeToString(publicKey)
}

// DecodeVlessPublicKey reads a reality public key as a form or link carries
// it. The error means it is not 32 bytes of base64.
func DecodeVlessPublicKey(text string) ([]byte, error) {
	return decodeVlessPublicKey(text)
}

// DecodeVlessShortId reads a reality short id as a form or link carries it.
// The error means it is not up to 16 hex digits.
func DecodeVlessShortId(text string) ([]byte, error) {
	return decodeVlessShortId(text)
}

// Link renders the configuration as a share link that `ParseVlessLink` and
// other VLESS clients read back. Parameters at their defaults are left out.
func (self *VlessConfig) Link() string {
	query := url.Values{}
	query.Set("encryption", "none")
	network := self.network()
	query.Set("type", network)
	security := self.security()
	query.Set("security", security)
	if self.Flow != VlessFlowNone {
		query.Set("flow", self.Flow)
	}
	if network == VlessNetworkWs || network == VlessNetworkHttpUpgrade {
		if self.Path != "" {
			query.Set("path", self.Path)
		}
		if self.Host != "" {
			query.Set("host", self.Host)
		}
	}
	if security != VlessSecurityNone {
		if self.ServerName != "" {
			query.Set("sni", self.ServerName)
		}
		if self.Fingerprint != "" {
			query.Set("fp", self.Fingerprint)
		}
	}
	if security == VlessSecurityTls {
		if 0 < len(self.Alpns) {
			query.Set("alpn", strings.Join(self.Alpns, ","))
		}
		if self.AllowInsecure {
			query.Set("allowInsecure", "1")
		}
	}
	if security == VlessSecurityReality {
		query.Set("pbk", EncodeVlessPublicKey(self.PublicKey))
		if 0 < len(self.ShortId) {
			query.Set("sid", hex.EncodeToString(self.ShortId))
		}
		if self.SpiderX != "" {
			query.Set("spx", self.SpiderX)
		}
	}
	linkUrl := url.URL{
		Scheme:   "vless",
		User:     url.User(self.Id),
		Host:     self.serverAddress(),
		RawQuery: query.Encode(),
		Fragment: self.Name,
	}
	return linkUrl.String()
}
