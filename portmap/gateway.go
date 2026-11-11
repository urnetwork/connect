package portmap

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"time"

	"github.com/libp2p/go-netroute"

	"github.com/urnetwork/connect"
)

// gateway.go — the router a mapping is asked of, and the udp exchange of pcp and
// nat-pmp with it.

// No send of an exchange was answered.
var errNoAnswer = errors.New("no answer")

// A public address whose route is the host's default route. The lookup reads
// the routing table and sends nothing to it.
var defaultRouteProbeIp = netip.MustParseAddr("1.1.1.1")

// The gateway of the host's ipv4 default route and this host's address on that
// route, read from the routing table (netlink on linux, the route socket on
// darwin and the bsds, GetBestRoute2 on windows). A default route through a
// tunnel, such as this product's own vpn, or one whose gateway is not on a
// private network, has no router to ask.
func DefaultGateway() (gateway netip.Addr, localIp netip.Addr, err error) {
	router, err := netroute.New()
	if err != nil {
		return netip.Addr{}, netip.Addr{}, fmt.Errorf("the routing table is not readable: %w", err)
	}
	iface, gatewayIp, preferredSrc, err := router.Route(net.IP(defaultRouteProbeIp.AsSlice()))
	if err != nil {
		return netip.Addr{}, netip.Addr{}, fmt.Errorf("no ipv4 default route: %w", err)
	}
	if iface != nil && connect.IsTunnelInterfaceName(iface.Name) {
		return netip.Addr{}, netip.Addr{}, fmt.Errorf("the default route is through %s, a tunnel rather than the router", iface.Name)
	}
	gateway, ok := netip.AddrFromSlice(gatewayIp)
	if !ok || gateway.Unmap().IsUnspecified() {
		return netip.Addr{}, netip.Addr{}, errors.New("the default route has no gateway")
	}
	gateway = gateway.Unmap()
	if !isLanIpv4(gateway) {
		return netip.Addr{}, netip.Addr{}, fmt.Errorf("the default gateway %s is not on a private network", gateway)
	}
	localIp, ok = netip.AddrFromSlice(preferredSrc)
	if !ok || !isLanIpv4(localIp.Unmap()) {
		return netip.Addr{}, netip.Addr{}, errors.New("this host has no private address on the default route")
	}
	return gateway, localIp.Unmap(), nil
}

// Sends `request` to `server` from a udp socket on `localIp` and waits for an
// answer from `server` that `accept` takes, sending again after each wait, which
// starts at `requestTimeout` and doubles, `attemptCount` sends in all (RFC 6886
// §3.1, RFC 6887 §8.1.1). A datagram from any other source is dropped unread by
// `accept`.
func UdpExchange(
	ctx context.Context,
	server netip.AddrPort,
	localIp netip.Addr,
	request []byte,
	accept func(response []byte) bool,
	requestTimeout time.Duration,
	attemptCount int,
) ([]byte, error) {
	localAddr := &net.UDPAddr{}
	if localIp.IsValid() {
		localAddr.IP = localIp.AsSlice()
	}
	conn, err := net.ListenUDP("udp4", localAddr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	// a cancel ends a read in flight at once
	stop := context.AfterFunc(ctx, func() {
		conn.Close()
	})
	defer stop()

	serverAddr := net.UDPAddrFromAddrPort(server)
	// the largest pcp message (RFC 6887 §7)
	buffer := make([]byte, 1100)
	wait := requestTimeout
	for range max(1, attemptCount) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, err := conn.WriteToUDP(request, serverAddr); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			return nil, err
		}
		deadline := time.Now().Add(wait)
		if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
			deadline = ctxDeadline
		}
		if err := conn.SetReadDeadline(deadline); err != nil {
			return nil, err
		}
		for {
			n, from, err := conn.ReadFromUDPAddrPort(buffer)
			if err != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return nil, ctxErr
				}
				var netErr net.Error
				if errors.As(err, &netErr) && netErr.Timeout() {
					break
				}
				return nil, err
			}
			if from.Addr().Unmap() != server.Addr().Unmap() || from.Port() != server.Port() {
				continue
			}
			response := slices.Clone(buffer[:n])
			if accept(response) {
				return response, nil
			}
		}
		wait *= 2
	}
	return nil, errNoAnswer
}
