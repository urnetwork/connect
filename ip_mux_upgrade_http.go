package connect

// HTTP upgrade (TCP/80) for the UpgradeMux.
//
// Claimed TCP/80 is DNAT'd onto the internal stack, terminated there, and each request
// is re-issued to the original destination over HTTPS (modern TLS) — or plaintext on
// fallback — with the response relayed back to the client. The stack's client-bound
// replies (src muxAddr:80) are un-NAT'd in the pump so the OS never sees the mux
// address. Only the Https / HttpsFallback modes use this path; Unencrypted passes
// through and Block drops, both without termination (see handleHttp).
//
// Per-packet gopacket rewrite is acceptable here: :80 plaintext HTTP is not the bulk
// of traffic and the upgrade is opt-in. Bulk-path optimization can come later. The
// connection is handled one request per client connection (Connection: close); HTTP
// keep-alive reuse is a future improvement.

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"

	"github.com/urnetwork/connect/protocol"
)

type httpNatKey struct {
	ip   netip.Addr
	port int
}

type httpNatEntry struct {
	// dst is the original destination IP the client targeted on :80
	dst netip.Addr
	// lastActivityNanos is refreshed on each packet of the flow; the maintenance loop evicts
	// records idle longer than NatIdleTimeout — a backstop for flows that never establish,
	// since the normal cleanup is the terminated connection's close (handleHttpConn).
	lastActivityNanos int64
}

// handleHttpUpgrade DNATs a claimed TCP/80 packet onto the internal stack for local
// termination. The flow's original destination is recorded (keyed by the client
// ip:port), and every client→server packet is rewritten dst→muxAddr and injected into
// the tun. Returns true (claimed); a rewrite failure passes the packet through.
func (self *UpgradeMux) handleHttpUpgrade(source TransferPath, provideMode protocol.ProvideMode, ipPath *IpPath, packet []byte) bool {
	if !self.muxAddr.IsValid() {
		return false
	}
	srcAddr, ok := netIPAddr(ipPath.SourceIp)
	if !ok {
		return false
	}
	dstAddr, ok := netIPAddr(ipPath.DestinationIp)
	if !ok {
		return false
	}

	// bind the terminator on first use, synchronously, so the DNAT'd packet below is
	// not RST'd by the stack before the listener is ready
	self.httpOnce.Do(self.startHttp)

	// record the flow's original destination and refresh its activity time, so the
	// maintenance loop only evicts genuinely idle/abandoned flows
	key := httpNatKey{ip: srcAddr, port: ipPath.SourcePort}
	now := time.Now().UnixNano()
	self.httpNatLock.Lock()
	if e, exists := self.httpNat[key]; exists && e.dst == dstAddr {
		e.lastActivityNanos = now
		self.httpNat[key] = e
	} else {
		self.httpNat[key] = httpNatEntry{dst: dstAddr, lastActivityNanos: now}
	}
	self.httpNatLock.Unlock()

	rewritten := rewriteTcpAddrs(packet, netip.Addr{}, self.muxAddr)
	if rewritten == nil {
		return false
	}
	self.mux.Tun().Write(rewritten)
	return true
}

// onPump intercepts the stack's client-bound HTTP replies (src muxAddr:80), un-NATs
// the source back to the original destination, and delivers them downstream. The
// stack's own upstream connections (HTTPS upgrade, DoH) return false to be forwarded
// upstream.
func (self *UpgradeMux) onPump(packet []byte) bool {
	ipPath, _, err := ParseIpPathWithPayload(packet)
	if err != nil {
		return false
	}
	if IpProtocolTcp != ipPath.Protocol || 80 != ipPath.SourcePort {
		return false
	}
	srcAddr, ok := netIPAddr(ipPath.SourceIp)
	if !ok || srcAddr != self.muxAddr {
		return false
	}
	dstAddr, ok := netIPAddr(ipPath.DestinationIp)
	if !ok {
		return false
	}
	key := httpNatKey{ip: dstAddr, port: ipPath.DestinationPort}
	self.httpNatLock.Lock()
	entry, ok := self.httpNat[key]
	if ok {
		entry.lastActivityNanos = time.Now().UnixNano()
		self.httpNat[key] = entry
	}
	self.httpNatLock.Unlock()
	if !ok {
		return false
	}
	rewritten := rewriteTcpAddrs(packet, entry.dst, netip.Addr{})
	if rewritten == nil {
		return false
	}
	outPath, _, err := ParseIpPathWithPayload(rewritten)
	if err != nil {
		return false
	}
	self.mux.deliverDownstream(self.source, self.provideMode, outPath, rewritten)
	return true
}

func (self *UpgradeMux) startHttp() {
	listener, err := self.mux.Tun().ListenTCP(&net.TCPAddr{IP: self.muxAddr.AsSlice(), Port: 80})
	if err != nil {
		self.mux.log.Infof("[mux]http listen error: %s\n", err)
		return
	}
	go func() {
		<-self.ctx.Done()
		listener.Close()
	}()
	go HandleError(func() {
		self.acceptHttpLoop(listener)
	})
}

func (self *UpgradeMux) acceptHttpLoop(listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go HandleError(func() {
			self.handleHttpConn(conn)
		})
	}
}

func (self *UpgradeMux) handleHttpConn(conn net.Conn) {
	defer conn.Close()

	remote, ok := conn.RemoteAddr().(*net.TCPAddr)
	if !ok {
		return
	}
	clientAddr, ok := netIPAddr(remote.IP)
	if !ok {
		return
	}
	key := httpNatKey{ip: clientAddr, port: remote.Port}
	self.httpNatLock.Lock()
	entry, ok := self.httpNat[key]
	self.httpNatLock.Unlock()
	if !ok {
		return
	}
	// the nat entry is intentionally NOT deleted on return: the final response and the
	// connection teardown (FIN, final ACK) are emitted by the stack asynchronously after this
	// returns, and onPump needs the entry to un-nat them on the way to the client. deleting
	// here drops a response-then-close (e.g. a strict-mode 502) most of the time. the entry
	// goes idle once teardown completes and is reclaimed by evictHttpNat (NatIdleTimeout).

	settings := self.settings.Load()
	fallback := settings.Http != nil && HttpUpgradeHttpsFallback == settings.Http.Mode

	// keep the client connection alive across requests; each request gets its own
	// one-shot upstream connection (upstream keep-alive/pooling is a future optimization)
	reader := bufio.NewReader(conn)
	for {
		req, err := http.ReadRequest(reader)
		if err != nil {
			return
		}
		clientKeepAlive := !req.Close

		resp, err := self.roundTrip(req, entry, fallback)
		if err != nil {
			writeHttpError(conn, http.StatusBadGateway)
			return
		}

		// frame the response per the client's keep-alive intent (the upstream connection
		// was one-shot). a body of unknown length is written chunked so the client finds
		// the boundary without a connection close.
		resp.Header.Del("Connection")
		resp.Header.Del("Keep-Alive")
		resp.Close = !clientKeepAlive
		if !clientKeepAlive {
			resp.Header.Set("Connection", "close")
		}

		werr := resp.Write(conn)
		resp.Body.Close()
		if werr != nil || !clientKeepAlive {
			return
		}
	}
}

// roundTrip re-issues the request to the original destination over HTTPS (modern TLS),
// falling back to plaintext HTTP when configured. The dial, TLS handshake, and
// response-header read are each bounded by UpgradeTimeout; a server that fails or stalls
// any of them falls back — dial/handshake before the request is sent, and the response
// stage only for idempotent, bodyless methods that are safe to re-issue. A server name
// whose upgrade recently failed is held off (UpgradeFailureHoldOff) and goes straight to
// plaintext. Each outcome is logged to aid diagnosis of servers that mis-handle the upgrade.
func (self *UpgradeMux) roundTrip(req *http.Request, entry httpNatEntry, fallback bool) (*http.Response, error) {
	hostname := req.Host
	if h, _, err := net.SplitHostPort(hostname); err == nil {
		hostname = h
	}

	settings := self.settings.Load()
	var upgradeTimeout, holdOff time.Duration
	if settings.Http != nil {
		upgradeTimeout = settings.Http.UpgradeTimeout
		holdOff = settings.Http.UpgradeFailureHoldOff
	}

	// connFailed handles a dial/handshake failure — the request has not been sent, so it is
	// always safe to re-issue over plaintext when in fallback mode
	connFailed := func(reason string, cause error) (*http.Response, error) {
		if fallback {
			self.mux.log.Infof("[mux]https upgrade %s: %s (%v); falling back to http\n", hostname, reason, cause)
			self.recordUpgradeFailure(hostname, holdOff)
			return self.roundTripPlain(req, entry)
		}
		self.mux.log.Infof("[mux]https upgrade %s: %s (%v); no fallback\n", hostname, reason, cause)
		return nil, cause
	}

	// in fallback mode, skip the upgrade for a server name that recently failed it —
	// otherwise a server without HTTPS on the original IP re-stalls every request
	if fallback && self.recentlyFailedUpgrade(hostname, holdOff) {
		self.mux.log.V(1).Infof("[mux]https upgrade %s: held off (recent failure); using http\n", hostname)
		return self.roundTripPlain(req, entry)
	}

	// HttpsPort comes from settings (DefaultHttpUpgradeSettings sets 443); 443 here is
	// only the standard-port fallback for a settings value left unset
	port := 443
	if settings.Http != nil && 0 < settings.Http.HttpsPort {
		port = settings.Http.HttpsPort
	}
	target := net.JoinHostPort(entry.dst.String(), strconv.Itoa(port))

	// bound the dial + handshake + response-header read so a closed/filtered/stalled :443
	// (or a server that handshakes but never answers) does not hang the request — the mux
	// ctx itself has no deadline. The streamed response body below is not bounded.
	upgradeCtx := self.ctx
	if 0 < upgradeTimeout {
		var upgradeCancel context.CancelFunc
		upgradeCtx, upgradeCancel = context.WithTimeout(self.ctx, upgradeTimeout)
		defer upgradeCancel()
	}

	rawConn, err := self.dialUpstream(upgradeCtx, "tcp", target)
	if err != nil {
		return connFailed("dial failed", err)
	}

	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if settings.Http != nil && settings.Http.TlsConfig != nil {
		tlsConfig = settings.Http.TlsConfig.Clone()
		if 0 == tlsConfig.MinVersion {
			tlsConfig.MinVersion = tls.VersionTLS12
		}
	}
	tlsConfig.ServerName = hostname

	tlsConn := tls.Client(rawConn, tlsConfig)
	if err := tlsConn.HandshakeContext(upgradeCtx); err != nil {
		rawConn.Close()
		return connFailed("tls handshake failed", err)
	}

	// bound the request write + response-header read with the same deadline, then clear it
	// so the (possibly long-lived) response body is not bounded
	if deadline, ok := upgradeCtx.Deadline(); ok {
		tlsConn.SetDeadline(deadline)
	}
	resp, err := writeReadHttp(tlsConn, req)
	if err != nil {
		tlsConn.Close()
		// the request was already sent; only re-issue idempotent, bodyless methods
		safeReissue := http.MethodGet == req.Method || http.MethodHead == req.Method
		if fallback && safeReissue {
			self.mux.log.Infof("[mux]https upgrade %s: response failed (%v); falling back to http\n", hostname, err)
			self.recordUpgradeFailure(hostname, holdOff)
			return self.roundTripPlain(req, entry)
		}
		self.mux.log.Infof("[mux]https upgrade %s: response failed (%v); no safe fallback (method=%s)\n", hostname, err, req.Method)
		return nil, err
	}
	tlsConn.SetDeadline(time.Time{})
	if 400 <= resp.StatusCode {
		// connected over HTTPS but the server answered with an error. in fallback mode, for an
		// idempotent bodyless method, the content is very likely served only over plaintext —
		// e.g. an HTTP-only CDN whose scheme-scoped signed URL 4xx/redirects the re-issued HTTPS
		// request (this is what broke Pandora audio). re-issue over http and hold off further
		// upgrades for this host so the rest of its requests skip straight to plaintext.
		safeReissue := http.MethodGet == req.Method || http.MethodHead == req.Method
		if fallback && safeReissue {
			resp.Body.Close()
			tlsConn.Close()
			self.mux.log.Infof("[mux]https upgrade %s: connected but status=%d; falling back to http\n", hostname, resp.StatusCode)
			self.recordUpgradeFailure(hostname, holdOff)
			return self.roundTripPlain(req, entry)
		}
		// not safe to re-issue (non-idempotent method, or strict mode) — surface the status so
		// an http-only server can be diagnosed
		self.mux.log.Infof("[mux]https upgrade %s: connected but status=%d\n", hostname, resp.StatusCode)
	} else {
		self.mux.log.V(1).Infof("[mux]https upgrade %s: ok status=%d\n", hostname, resp.StatusCode)
	}
	return resp, nil
}

// recentlyFailedUpgrade reports whether an HTTPS upgrade for hostname failed within the
// hold-off window; an expired entry is dropped. holdOff <= 0 disables the hold-off.
func (self *UpgradeMux) recentlyFailedUpgrade(hostname string, holdOff time.Duration) bool {
	if 0 < holdOff {
		self.httpsUpgradeFailLock.Lock()
		defer self.httpsUpgradeFailLock.Unlock()
		if t, ok := self.httpsUpgradeFail[hostname]; ok {
			if time.Since(t) < holdOff {
				return true
			}
			delete(self.httpsUpgradeFail, hostname)
		}
	}
	return false
}

// recordUpgradeFailure marks an HTTPS upgrade failure for hostname. The entry is cleared
// after the hold-off TTL — by recentlyFailedUpgrade on a later check, or by the background
// maintenance loop for names that are never retried.
func (self *UpgradeMux) recordUpgradeFailure(hostname string, holdOff time.Duration) {
	if 0 < holdOff {
		self.httpsUpgradeFailLock.Lock()
		self.httpsUpgradeFail[hostname] = time.Now()
		self.httpsUpgradeFailLock.Unlock()
	}
}

// upgradeFailureHoldOff is the configured hold-off, which doubles as the failure-record
// TTL (0 if unset/disabled).
func (self *UpgradeMux) upgradeFailureHoldOff() time.Duration {
	if settings := self.settings.Load(); settings.Http != nil {
		return settings.Http.UpgradeFailureHoldOff
	}
	return 0
}

// pruneUpgradeFailures drops every failure record at least holdOff old — all of them when
// holdOff <= 0 (the hold-off is disabled, so nothing should be retained).
func (self *UpgradeMux) pruneUpgradeFailures(holdOff time.Duration) {
	self.httpsUpgradeFailLock.Lock()
	defer self.httpsUpgradeFailLock.Unlock()
	now := time.Now()
	for h, t := range self.httpsUpgradeFail {
		if holdOff <= now.Sub(t) {
			delete(self.httpsUpgradeFail, h)
		}
	}
}

// run is the mux's single lifecycle loop. It evicts the mux's TTL-bounded state —
// HTTPS-upgrade failure records, the HTTP-upgrade NAT table, and the IP→hostname affinity
// map — so none grows unbounded. It ticks on the shortest configured TTL (falling back to a
// default cadence when all are disabled) and runs until the mux ctx is done.
func (self *UpgradeMux) run() {
	defaultInterval := DefaultHttpUpgradeSettings().UpgradeFailureHoldOff
	for {
		interval := defaultInterval
		for _, ttl := range []time.Duration{self.upgradeFailureHoldOff(), self.httpNatIdleTimeout(), self.reverseTtl()} {
			if 0 < ttl && ttl < interval {
				interval = ttl
			}
		}
		select {
		case <-self.ctx.Done():
			return
		case <-time.After(interval):
		}
		self.pruneUpgradeFailures(self.upgradeFailureHoldOff())
		self.evictHttpNat(self.httpNatIdleTimeout())
		self.evictReverse(self.reverseTtl())
	}
}

// httpNatIdleTimeout is the configured HTTP-upgrade NAT idle TTL (0 if unset/disabled).
func (self *UpgradeMux) httpNatIdleTimeout() time.Duration {
	if settings := self.settings.Load(); settings.Http != nil {
		return settings.Http.NatIdleTimeout
	}
	return 0
}

// evictHttpNat drops NAT records idle at least idleTimeout (none when idleTimeout <= 0).
func (self *UpgradeMux) evictHttpNat(idleTimeout time.Duration) {
	if 0 < idleTimeout {
		cutoff := time.Now().UnixNano() - int64(idleTimeout)
		self.httpNatLock.Lock()
		defer self.httpNatLock.Unlock()
		for k, e := range self.httpNat {
			if e.lastActivityNanos <= cutoff {
				delete(self.httpNat, k)
			}
		}
	}
}

func (self *UpgradeMux) roundTripPlain(req *http.Request, entry httpNatEntry) (*http.Response, error) {
	target := net.JoinHostPort(entry.dst.String(), "80")
	conn, err := self.dialUpstream(self.ctx, "tcp", target)
	if err != nil {
		return nil, err
	}
	return writeReadHttp(conn, req)
}

// writeReadHttp writes the request to the upstream connection (forcing Connection:
// close) and reads the response. Closing the response body closes the connection.
func writeReadHttp(conn net.Conn, req *http.Request) (*http.Response, error) {
	outReq := req.Clone(req.Context())
	// the upstream connection is one-shot; strip client hop-by-hop headers and force
	// Connection: close so Write frames a single exchange
	outReq.Header.Del("Connection")
	outReq.Header.Del("Keep-Alive")
	outReq.Header.Del("Proxy-Connection")
	outReq.Close = true
	outReq.RequestURI = ""
	if err := outReq.Write(conn); err != nil {
		conn.Close()
		return nil, err
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), outReq)
	if err != nil {
		conn.Close()
		return nil, err
	}
	resp.Body = &connClosingBody{rc: resp.Body, conn: conn}
	return resp, nil
}

func writeHttpError(conn net.Conn, status int) {
	resp := &http.Response{
		StatusCode: status,
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     make(http.Header),
		Body:       http.NoBody,
		Close:      true,
	}
	resp.Write(conn)
}

// connClosingBody closes the underlying connection when the response body is closed.
type connClosingBody struct {
	rc   io.ReadCloser
	conn net.Conn
}

func (self *connClosingBody) Read(p []byte) (int, error) {
	return self.rc.Read(p)
}

func (self *connClosingBody) Close() error {
	err := self.rc.Close()
	self.conn.Close()
	return err
}

// rewriteTcpAddrs rewrites the IPv4/IPv6 source and/or destination (whichever is valid)
// of a TCP packet and fixes the IP and TCP checksums. The common path mutates the packet
// in place with incremental checksum updates (RFC 1624) — the caller owns the buffer —
// and falls back to a gopacket parse/serialize for anything unusual (e.g. IPv6 extension
// headers). Returns the rewritten packet, or nil if it is not a parseable TCP/IP packet.
func rewriteTcpAddrs(packet []byte, newSrc netip.Addr, newDst netip.Addr) []byte {
	if len(packet) == 0 {
		return nil
	}
	switch packet[0] >> 4 {
	case 4:
		if rewriteTcp4InPlace(packet, newSrc, newDst) {
			return packet
		}
	case 6:
		if rewriteTcp6InPlace(packet, newSrc, newDst) {
			return packet
		}
	}
	return rewriteTcpAddrsGopacket(packet, newSrc, newDst)
}

// rewriteTcp4InPlace rewrites a plain IPv4 TCP packet's addresses in place, returning
// false (no mutation) for anything it does not handle.
func rewriteTcp4InPlace(packet []byte, newSrc netip.Addr, newDst netip.Addr) bool {
	if len(packet) < 20 {
		return false
	}
	ihl := int(packet[0]&0x0f) * 4
	if ihl < 20 || len(packet) < ihl+18 {
		return false
	}
	if packet[9] != 6 { // IPPROTO_TCP
		return false
	}
	if (newSrc.IsValid() && !newSrc.Is4()) || (newDst.IsValid() && !newDst.Is4()) {
		return false
	}
	ipChecksum := packet[10:12]
	tcpChecksum := packet[ihl+16 : ihl+18]
	if newSrc.IsValid() {
		next := newSrc.As4()
		checksumUpdate(ipChecksum, packet[12:16], next[:])
		checksumUpdate(tcpChecksum, packet[12:16], next[:])
		copy(packet[12:16], next[:])
	}
	if newDst.IsValid() {
		next := newDst.As4()
		checksumUpdate(ipChecksum, packet[16:20], next[:])
		checksumUpdate(tcpChecksum, packet[16:20], next[:])
		copy(packet[16:20], next[:])
	}
	return true
}

// rewriteTcp6InPlace rewrites a plain IPv6 TCP packet's addresses in place. IPv6 has no
// header checksum; only the TCP pseudo-header checksum changes. Returns false for
// packets with extension headers (the gopacket fallback handles those).
func rewriteTcp6InPlace(packet []byte, newSrc netip.Addr, newDst netip.Addr) bool {
	if len(packet) < 40+18 {
		return false
	}
	if packet[6] != 6 { // NextHeader != TCP
		return false
	}
	is6 := func(a netip.Addr) bool { return a.Is6() && !a.Is4In6() }
	if (newSrc.IsValid() && !is6(newSrc)) || (newDst.IsValid() && !is6(newDst)) {
		return false
	}
	tcpChecksum := packet[56:58] // 40-byte fixed header + 16-byte TCP checksum offset
	if newSrc.IsValid() {
		next := newSrc.As16()
		checksumUpdate(tcpChecksum, packet[8:24], next[:])
		copy(packet[8:24], next[:])
	}
	if newDst.IsValid() {
		next := newDst.As16()
		checksumUpdate(tcpChecksum, packet[24:40], next[:])
		copy(packet[24:40], next[:])
	}
	return true
}

// checksumUpdate adjusts a 16-bit internet checksum in place for a field changing from
// oldField to newField (equal, even length), per RFC 1624: HC' = ~(~HC + Σ~m + Σm').
func checksumUpdate(checksum []byte, oldField []byte, newField []byte) {
	sum := uint32(^binary.BigEndian.Uint16(checksum))
	for i := 0; i+1 < len(oldField); i += 2 {
		sum += uint32(^binary.BigEndian.Uint16(oldField[i : i+2]))
		sum += uint32(binary.BigEndian.Uint16(newField[i : i+2]))
	}
	for 0xffff < sum {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	binary.BigEndian.PutUint16(checksum, ^uint16(sum))
}

// rewriteTcpAddrsGopacket is the parse/serialize fallback for packets the in-place path
// declines. It returns a new buffer independent of the input.
func rewriteTcpAddrsGopacket(packet []byte, newSrc netip.Addr, newDst netip.Addr) []byte {
	options := gopacket.SerializeOptions{
		ComputeChecksums: true,
		FixLengths:       true,
	}
	buffer := gopacket.NewSerializeBuffer()

	switch packet[0] >> 4 {
	case 4:
		parsed := gopacket.NewPacket(packet, layers.LayerTypeIPv4, gopacket.NoCopy)
		ip, _ := parsed.Layer(layers.LayerTypeIPv4).(*layers.IPv4)
		tcp, _ := parsed.Layer(layers.LayerTypeTCP).(*layers.TCP)
		if ip == nil || tcp == nil {
			return nil
		}
		if newSrc.IsValid() {
			ip.SrcIP = newSrc.AsSlice()
		}
		if newDst.IsValid() {
			ip.DstIP = newDst.AsSlice()
		}
		tcp.SetNetworkLayerForChecksum(ip)
		if err := gopacket.SerializeLayers(buffer, options, ip, tcp, gopacket.Payload(tcp.Payload)); err != nil {
			return nil
		}
	case 6:
		parsed := gopacket.NewPacket(packet, layers.LayerTypeIPv6, gopacket.NoCopy)
		ip, _ := parsed.Layer(layers.LayerTypeIPv6).(*layers.IPv6)
		tcp, _ := parsed.Layer(layers.LayerTypeTCP).(*layers.TCP)
		if ip == nil || tcp == nil {
			return nil
		}
		if newSrc.IsValid() {
			ip.SrcIP = newSrc.AsSlice()
		}
		if newDst.IsValid() {
			ip.DstIP = newDst.AsSlice()
		}
		tcp.SetNetworkLayerForChecksum(ip)
		if err := gopacket.SerializeLayers(buffer, options, ip, tcp, gopacket.Payload(tcp.Payload)); err != nil {
			return nil
		}
	default:
		return nil
	}
	return buffer.Bytes()
}
