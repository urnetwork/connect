package portmap

import (
	"bufio"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/huin/goupnp"
	"github.com/huin/goupnp/dcps/internetgateway2"
	"github.com/huin/goupnp/soap"
)

// upnp.go — UPnP IGD, through goupnp (BSD-2-Clause) for the device description
// and the SOAP actions, with the search and the description fetch here so every
// request goes to the gateway and no further.
//
// The search is SSDP's M-SEARCH (UPnP Device Architecture §1.3), sent to the
// gateway and to the SSDP group, and only an answer from the gateway's address
// is read. Its description url must be on the gateway, and so must the control
// url of the connection service it names, however the description's URLBase
// reads. The connection services are tried in the order WANIPConnection:2,
// WANIPConnection:1, WANPPPConnection:1.
//
// A mapping asks for the internal port as the external one. A router that holds
// that external port for another host answers 718, and an IGD:2 router is then
// asked for any port (AddAnyPortMapping), which is how a upnp router gives a
// different external port. A router that grants only permanent leases (725) is
// not used: a permanent mapping that outlives a process that crashed would later
// forward to whichever host takes this address.

const (
	upnpSearchTarget  = "urn:schemas-upnp-org:device:InternetGatewayDevice:1"
	upnpMulticastHost = "239.255.255.250:1900"

	upnpErrorConflictInMappingEntry       = 718
	upnpErrorOnlyPermanentLeasesSupported = 725
)

// The actions of a WANIPConnection or WANPPPConnection service a mapping needs,
// which every generated client of them has.
type upnpConnection interface {
	AddPortMappingCtx(
		ctx context.Context,
		NewRemoteHost string,
		NewExternalPort uint16,
		NewProtocol string,
		NewInternalPort uint16,
		NewInternalClient string,
		NewEnabled bool,
		NewPortMappingDescription string,
		NewLeaseDuration uint32,
	) error
	DeletePortMappingCtx(ctx context.Context, NewRemoteHost string, NewExternalPort uint16, NewProtocol string) error
	GetExternalIPAddressCtx(ctx context.Context) (NewExternalIPAddress string, err error)
	GetServiceClient() *goupnp.ServiceClient
}

// AddAnyPortMapping of a WANIPConnection:2 service.
type upnpAnyPortConnection interface {
	AddAnyPortMappingCtx(
		ctx context.Context,
		NewRemoteHost string,
		NewExternalPort uint16,
		NewProtocol string,
		NewInternalPort uint16,
		NewInternalClient string,
		NewEnabled bool,
		NewPortMappingDescription string,
		NewLeaseDuration uint32,
	) (NewReservedPort uint16, err error)
}

// The connection service of the gateway a mapping was made through.
type upnpGateway struct {
	connection upnpConnection
}

type upnpMethod struct {
	mapper  *PortMapper
	gateway netip.Addr
	localIp netip.Addr
	// found by the first mapping and kept for the renewals and the removal
	device *upnpGateway
}

func (self *PortMapper) newUpnpMethod(gateway netip.Addr, localIp netip.Addr) *upnpMethod {
	return &upnpMethod{
		mapper:  self,
		gateway: gateway,
		localIp: localIp,
	}
}

func (self *upnpMethod) name() string {
	return MethodUpnp
}

func (self *upnpMethod) mapPorts(
	ctx context.Context,
	requests []PortMapRequest,
	previous []*methodMapping,
	lifetime time.Duration,
) (externalIp netip.Addr, mappings []*methodMapping, errs []error, err error) {
	mappings = make([]*methodMapping, len(requests))
	errs = make([]error, len(requests))
	if self.device == nil {
		device, err := self.discover(ctx)
		if err != nil {
			return netip.Addr{}, mappings, errs, err
		}
		self.device = device
	}
	settings := self.mapper.settings

	func() {
		callCtx, cancel := context.WithTimeout(ctx, settings.UpnpRequestTimeout)
		defer cancel()
		// only for the status, so a router that does not say is no error
		if externalIpText, err := self.device.connection.GetExternalIPAddressCtx(callCtx); err == nil {
			if ip, err := netip.ParseAddr(strings.TrimSpace(externalIpText)); err == nil {
				externalIp = ip.Unmap()
			}
		}
	}()

	leaseSeconds := self.mapper.lifetimeSeconds(lifetime)
	for i, request := range requests {
		var previousMapping *methodMapping
		if i < len(previous) {
			previousMapping = previous[i]
		}
		suggestedExternalPort := request.InternalPort
		if previousMapping != nil {
			suggestedExternalPort = previousMapping.ExternalPort
		}
		externalPort, err := self.addPortMapping(ctx, request, suggestedExternalPort, leaseSeconds)
		if err != nil {
			if ctx.Err() != nil {
				return netip.Addr{}, mappings, errs, ctx.Err()
			}
			errs[i] = err
			continue
		}
		if previousMapping != nil && previousMapping.ExternalPort != externalPort {
			// a upnp mapping is named by its external port, so the one the
			// router moved from would otherwise stay
			self.removePorts(ctx, []*methodMapping{previousMapping})
		}
		mappings[i] = &methodMapping{
			PortMapping: PortMapping{
				Protocol:     request.Protocol,
				InternalPort: request.InternalPort,
				ExternalPort: externalPort,
				Lifetime:     time.Duration(leaseSeconds) * time.Second,
				ExpireTime:   self.mapper.now().Add(time.Duration(leaseSeconds) * time.Second),
			},
			upnp: self.device,
		}
	}
	return externalIp, mappings, errs, nil
}

// Maps one request on `externalPort`, or on any port the router picks when it
// holds that one for another host and speaks IGD:2.
func (self *upnpMethod) addPortMapping(
	ctx context.Context,
	request PortMapRequest,
	externalPort int,
	leaseSeconds uint32,
) (int, error) {
	settings := self.mapper.settings
	protocol := strings.ToUpper(request.Protocol)
	callCtx, cancel := context.WithTimeout(ctx, settings.UpnpRequestTimeout)
	defer cancel()
	err := self.device.connection.AddPortMappingCtx(
		callCtx,
		"",
		uint16(externalPort),
		protocol,
		uint16(request.InternalPort),
		self.localIp.String(),
		true,
		settings.Description,
		leaseSeconds,
	)
	if err == nil {
		return externalPort, nil
	}
	switch upnpErrorCode(err) {
	case upnpErrorOnlyPermanentLeasesSupported:
		return 0, errors.New("the router grants only permanent mappings, which would outlive this device's extender")
	case upnpErrorConflictInMappingEntry:
		anyPortConnection, ok := self.device.connection.(upnpAnyPortConnection)
		if !ok {
			return 0, fmt.Errorf("the router maps external port %d to another host", externalPort)
		}
		anyCtx, cancel := context.WithTimeout(ctx, settings.UpnpRequestTimeout)
		defer cancel()
		reservedPort, err := anyPortConnection.AddAnyPortMappingCtx(
			anyCtx,
			"",
			uint16(externalPort),
			protocol,
			uint16(request.InternalPort),
			self.localIp.String(),
			true,
			settings.Description,
			leaseSeconds,
		)
		if err != nil {
			return 0, upnpError(err)
		}
		if reservedPort == 0 {
			return 0, errors.New("the router granted no external port")
		}
		return int(reservedPort), nil
	default:
		return 0, upnpError(err)
	}
}

func (self *upnpMethod) removePorts(ctx context.Context, mappings []*methodMapping) error {
	errs := []error{}
	for _, mapping := range mappings {
		device := mapping.upnp
		if device == nil {
			device = self.device
		}
		if device == nil {
			continue
		}
		err := func() error {
			callCtx, cancel := context.WithTimeout(ctx, self.mapper.settings.UpnpRequestTimeout)
			defer cancel()
			return device.connection.DeletePortMappingCtx(
				callCtx,
				"",
				uint16(mapping.ExternalPort),
				strings.ToUpper(mapping.Protocol),
			)
		}()
		if err != nil {
			errs = append(errs, fmt.Errorf("%s %d: %w", mapping.Protocol, mapping.ExternalPort, upnpError(err)))
		}
	}
	return errors.Join(errs...)
}

// The connection service of the first device the gateway answered the search
// for. errMethodNotSupported when nothing answered.
func (self *upnpMethod) discover(ctx context.Context) (*upnpGateway, error) {
	settings := self.mapper.settings
	search := settings.UpnpSearch
	if search == nil {
		search = func(ctx context.Context, gateway netip.Addr, localIp netip.Addr) ([]string, error) {
			return UpnpSearch(
				ctx,
				gateway,
				localIp,
				settings.UpnpSearchTimeout,
				settings.UpnpSsdpPort,
				settings.UpnpMulticastAddress,
			)
		}
	}
	locations, err := search(ctx, self.gateway, self.localIp)
	if err != nil {
		return nil, err
	}
	if len(locations) == 0 {
		return nil, errMethodNotSupported
	}
	errs := []error{}
	for _, location := range locations {
		device, err := self.deviceAt(ctx, location)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		return device, nil
	}
	return nil, errors.Join(errs...)
}

// The connection service the description at `location` names, which must be on
// the gateway along with its control url.
func (self *upnpMethod) deviceAt(ctx context.Context, location string) (*upnpGateway, error) {
	locationUrl, err := url.Parse(location)
	if err != nil {
		return nil, fmt.Errorf("the description url is not readable: %w", err)
	}
	if !self.onGateway(locationUrl) {
		return nil, fmt.Errorf("the description is not on the gateway %s", self.gateway)
	}
	root, err := self.fetchDescription(ctx, locationUrl)
	if err != nil {
		return nil, err
	}
	httpClient := self.httpClient()

	connections := []upnpConnection{}
	if clients, err := internetgateway2.NewWANIPConnection2ClientsFromRootDevice(root, locationUrl); err == nil {
		for _, client := range clients {
			connections = append(connections, client)
		}
	}
	if clients, err := internetgateway2.NewWANIPConnection1ClientsFromRootDevice(root, locationUrl); err == nil {
		for _, client := range clients {
			connections = append(connections, client)
		}
	}
	if clients, err := internetgateway2.NewWANPPPConnection1ClientsFromRootDevice(root, locationUrl); err == nil {
		for _, client := range clients {
			connections = append(connections, client)
		}
	}
	for _, connection := range connections {
		serviceClient := connection.GetServiceClient()
		if serviceClient == nil || serviceClient.SOAPClient == nil {
			continue
		}
		if !self.onGateway(&serviceClient.SOAPClient.EndpointURL) {
			continue
		}
		serviceClient.SOAPClient.HTTPClient = *httpClient
		return &upnpGateway{connection: connection}, nil
	}
	return nil, errors.New("the router's upnp device has no internet gateway connection service on the gateway")
}

// Whether `u` is a plain http url on the gateway's own address.
func (self *upnpMethod) onGateway(u *url.URL) bool {
	if u == nil || u.Scheme != "http" {
		return false
	}
	host, err := netip.ParseAddr(u.Hostname())
	return err == nil && host.Unmap() == self.gateway
}

func (self *upnpMethod) fetchDescription(ctx context.Context, locationUrl *url.URL) (*goupnp.RootDevice, error) {
	settings := self.mapper.settings
	callCtx, cancel := context.WithTimeout(ctx, settings.UpnpRequestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(callCtx, http.MethodGet, locationUrl.String(), nil)
	if err != nil {
		return nil, err
	}
	response, err := self.httpClient().Do(request)
	if err != nil {
		return nil, fmt.Errorf("the router's upnp description: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the router's upnp description answered %s", response.Status)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, int64(settings.UpnpMaxResponseByteCount)+1))
	if err != nil {
		return nil, fmt.Errorf("the router's upnp description: %w", err)
	}
	if settings.UpnpMaxResponseByteCount < len(body) {
		return nil, errors.New("the router's upnp description is too large")
	}
	root := &goupnp.RootDevice{}
	decoder := xml.NewDecoder(bytes.NewReader(body))
	decoder.DefaultSpace = goupnp.DeviceXMLNamespace
	decoder.CharsetReader = func(charset string, input io.Reader) (io.Reader, error) {
		// what goupnp reads by default: utf-8 and its ascii subset
		return input, nil
	}
	if err := decoder.Decode(root); err != nil {
		return nil, fmt.Errorf("the router's upnp description is not readable: %w", err)
	}
	urlBase := locationUrl
	if root.URLBaseStr != "" {
		if parsed, err := url.Parse(root.URLBaseStr); err == nil {
			urlBase = parsed
		}
	}
	root.SetURLBase(urlBase)
	return root, nil
}

// A client that goes to the gateway alone: no proxy, no redirect.
func (self *upnpMethod) httpClient() *http.Client {
	if self.mapper.settings.UpnpHttpClient != nil {
		return self.mapper.settings.UpnpHttpClient
	}
	return &http.Client{
		Transport: &http.Transport{
			Proxy:             nil,
			DisableKeepAlives: true,
		},
		Timeout: self.mapper.settings.UpnpRequestTimeout,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// The UPnP error code of a SOAP fault, 0 for any other error.
func upnpErrorCode(err error) int {
	var faultErr *soap.SOAPFaultError
	if errors.As(err, &faultErr) {
		return faultErr.Detail.UPnPError.Errorcode
	}
	return 0
}

// A SOAP fault in the router's words, or the error.
func upnpError(err error) error {
	var faultErr *soap.SOAPFaultError
	if errors.As(err, &faultErr) {
		description := strings.TrimSpace(faultErr.Detail.UPnPError.ErrorDescription)
		if description == "" {
			description = strings.TrimSpace(faultErr.FaultString)
		}
		return fmt.Errorf("upnp error %d %s", faultErr.Detail.UPnPError.Errorcode, description)
	}
	return err
}

// An SSDP search (UPnP Device Architecture §1.3.2) for internet gateways, sent
// from a socket on `localIp` to the gateway's `ssdpPort` and to
// `multicastAddress` (empty sends none there), and the description urls of the
// answers from the gateway's address, in the order they came. It returns once an
// answer came, or `searchTimeout` passed.
func UpnpSearch(
	ctx context.Context,
	gateway netip.Addr,
	localIp netip.Addr,
	searchTimeout time.Duration,
	ssdpPort int,
	multicastAddress string,
) ([]string, error) {
	localAddr := &net.UDPAddr{}
	if localIp.IsValid() {
		localAddr.IP = localIp.AsSlice()
	}
	conn, err := net.ListenUDP("udp4", localAddr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() {
		conn.Close()
	})
	defer stop()

	request := []byte(fmt.Sprintf(
		"M-SEARCH * HTTP/1.1\r\nHOST: %s\r\nST: %s\r\nMAN: \"ssdp:discover\"\r\nMX: 1\r\n\r\n",
		upnpMulticastHost,
		upnpSearchTarget,
	))
	destinations := []*net.UDPAddr{net.UDPAddrFromAddrPort(netip.AddrPortFrom(gateway, uint16(ssdpPort)))}
	if multicastAddress != "" {
		if multicastAddrPort, err := netip.ParseAddrPort(multicastAddress); err == nil {
			destinations = append(destinations, net.UDPAddrFromAddrPort(multicastAddrPort))
		}
	}
	sent := false
	for _, destination := range destinations {
		// the group may be unreachable from this socket, which the unicast
		// search does not need
		if _, err := conn.WriteToUDP(request, destination); err == nil {
			sent = true
		}
	}
	if !sent {
		return nil, errors.New("the upnp search could not be sent")
	}

	deadline := time.Now().Add(searchTimeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	if err := conn.SetReadDeadline(deadline); err != nil {
		return nil, err
	}
	locations := []string{}
	buffer := make([]byte, 2048)
	for {
		n, from, err := conn.ReadFromUDPAddrPort(buffer)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				return locations, nil
			}
			return nil, err
		}
		if from.Addr().Unmap() != gateway {
			continue
		}
		location, ok := parseSsdpLocation(buffer[:n])
		if !ok || slices.Contains(locations, location) {
			continue
		}
		locations = append(locations, location)
		// the gateway answered; a second answer would name the same device
		return locations, nil
	}
}

// The LOCATION of an SSDP search answer, or false when `datagram` is not one.
func parseSsdpLocation(datagram []byte) (string, bool) {
	response, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(datagram)), nil)
	if err != nil {
		return "", false
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", false
	}
	location := strings.TrimSpace(response.Header.Get("Location"))
	if location == "" {
		return "", false
	}
	return location, true
}
