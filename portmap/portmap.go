package portmap

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/connect"
)

// portmap.go — the router port mapping of the provider extender role
// (connect/EXTENDER.md S8, the port mapping).
//
// A host behind a home router has no inbound path on the extender's ports
// unless the router forwards them, and many routers forward a port when asked:
// by PCP (RFC 6887), by its predecessor NAT-PMP (RFC 6886), or by UPnP IGD. A
// PortMapper asks the router on the host's default route, by the first of the
// three that maps the caller's first port, to forward each of the caller's
// ports to this host for a bounded lifetime. It renews the mappings at a
// fraction of the lifetime the router granted, tries again on a backoff while
// nothing maps, and removes every mapping it made when it closes. The router
// may give an external port other than the internal one, and the status says
// which, so the caller can offer the external port to whoever dials it.
//
// What it maps is fixed when it is built: the caller's own ports, forwarded to
// this host's own address and nowhere else. It talks only to the default
// gateway: a pcp or nat-pmp answer from any other source is dropped, and a upnp
// description or control url on any other host is refused, so another device on
// the lan can neither steer a mapping nor make this host send a request
// elsewhere. A upnp router that grants only permanent mappings is not used: a
// mapping that outlives a process that crashed would later forward to whichever
// host takes this address. A router whose own external address is not public
// sits behind another NAT, which no mapping on it crosses, so its mappings are
// removed and the attempt counts as failed.
//
// Only ipv4: the three protocols map an ipv4 NAT. A host with a public address
// has no NAT to map, and the caller does not build a mapper for it.
//
// Every wait is bounded by the settings, and every clock and every exchange with
// the router is a settings seam, so the whole lifecycle is deterministic in
// tests.
//
// Safe for concurrent use.

const (
	ProtocolTcp = "tcp"
	ProtocolUdp = "udp"
)

// The methods a mapping is made with, in the order they are tried.
const (
	MethodPcp    = "pcp"
	MethodNatPmp = "nat-pmp"
	MethodUpnp   = "upnp"
)

// The states of a status.
const (
	// the first attempt has not finished
	StateMapping = "mapping"
	// the first port, and maybe more, is mapped
	StateMapped = "mapped"
	// nothing is mapped; Error says why, and the mapper tries again on its
	// backoff
	StateFailed = "failed"
	// the mapper closed and removed what it mapped
	StateClosed = "closed"
)

// One port the caller holds and asks the router to forward to this host.
type PortMapRequest struct {
	// ProtocolTcp or ProtocolUdp
	Protocol     string
	InternalPort int
}

func (self PortMapRequest) String() string {
	return fmt.Sprintf("%s %d", self.Protocol, self.InternalPort)
}

// One mapping the router granted.
type PortMapping struct {
	Protocol     string
	InternalPort int
	// the port the router forwards to InternalPort, which may differ from it
	ExternalPort int
	// how long the router granted, from the answer that last renewed it
	Lifetime time.Duration
	// when the router drops it unless it is renewed
	ExpireTime time.Time
}

type PortMapperStatus struct {
	// one of the State values above
	State string
	// the method of the mappings, empty while none is mapped
	Method string
	// the router asked, and this host's address it forwards to; invalid until
	// the gateway is known
	Gateway netip.Addr
	LocalIp netip.Addr
	// the router's own external address, invalid when it did not say
	ExternalIp netip.Addr
	// what is mapped, in the order of the requests
	Mappings []PortMapping
	// why nothing is mapped, or why a request beside the mapped ones is not;
	// empty when every request is mapped
	Error string
	// when the last attempt or renewal finished, zero before the first
	UpdateTime time.Time
}

// The mapping of one request, nil when it is not mapped.
func (self *PortMapperStatus) Mapping(protocol string, internalPort int) *PortMapping {
	if self == nil {
		return nil
	}
	for i := range self.Mappings {
		mapping := &self.Mappings[i]
		if mapping.Protocol == protocol && mapping.InternalPort == internalPort {
			return mapping
		}
	}
	return nil
}

// The default gateway of the host's ipv4 default route and this host's address
// on that route, or why there is none to ask.
type GatewayFunction func() (gateway netip.Addr, localIp netip.Addr, err error)

// One request to the gateway's pcp and nat-pmp server and the first answer
// `accept` takes, retransmitted on the settings' schedule (RFC 6886 §3.1, RFC
// 6887 §8.1.1). Only an answer from `server` is offered to `accept`.
type ExchangeFunction func(
	ctx context.Context,
	server netip.AddrPort,
	localIp netip.Addr,
	request []byte,
	accept func(response []byte) bool,
) ([]byte, error)

// The description urls of the upnp devices that answered a search from the
// gateway (SSDP, UPnP Device Architecture §1.3).
type UpnpSearchFunction func(ctx context.Context, gateway netip.Addr, localIp netip.Addr) ([]string, error)

type PortMapperSettings struct {
	Log connect.Logger

	// What each mapping asks the router for. A router may grant less, and the
	// mapping is renewed on what it granted. An hour bounds how long a mapping
	// outlives a process that could not remove it.
	MappingLifetime time.Duration
	// The fraction of the granted lifetime after which a mapping is renewed: a
	// half, as RFC 6886 §3.3 recommends, leaves the second half for a renewal
	// that has to be retried.
	RenewFraction float64
	// The least wait before a renewal, which keeps a router that grants a few
	// seconds from turning the renewals into a poll.
	MinRenewTimeout time.Duration
	// The wait before another attempt after one mapped nothing, doubling to the
	// max. Each attempt is a few datagrams and a search, and a router without
	// any of the methods answers none of them.
	MinRetryTimeout time.Duration
	MaxRetryTimeout time.Duration
	// The bound of one whole attempt or renewal, every method included.
	AttemptTimeout time.Duration
	// The first wait for a pcp or nat-pmp answer and the number of sends,
	// each wait twice the last (RFC 6886 §3.1 starts at 250 ms). Three sends
	// wait 1.75 seconds in all.
	RequestTimeout      time.Duration
	RequestAttemptCount int
	// How long a upnp search waits for answers, and the bound of each http
	// request to a upnp device.
	UpnpSearchTimeout  time.Duration
	UpnpRequestTimeout time.Duration
	// The largest upnp description or control answer read.
	UpnpMaxResponseByteCount int
	// The bound of removing every mapping as the mapper closes. A router that
	// does not answer keeps them until their lifetime ends.
	RemoveTimeout time.Duration
	// What a upnp mapping is described as on the router's page.
	Description string
	// The methods tried, in order. Empty tries none.
	Methods []string

	// The pcp and nat-pmp server port (RFC 6887 §19.1, RFC 6886 §3.1).
	ServerPort int
	// The gateway's SSDP port a upnp search is sent to, and the SSDP group it
	// is also sent to; an empty group sends to the gateway alone (UPnP Device
	// Architecture §1.3).
	UpnpSsdpPort         int
	UpnpMulticastAddress string

	// The only clock the mapper reads, and its waits. Tests install fakes.
	Now   func() time.Time
	After func(wait time.Duration) <-chan time.Time
	// The seams of the router. Nil takes the host's default route, a udp
	// socket, and an SSDP search on the lan.
	Gateway    GatewayFunction
	Exchange   ExchangeFunction
	UpnpSearch UpnpSearchFunction
	// The client of the upnp description and control requests. Nil takes one
	// that never uses a proxy, since every request goes to the gateway.
	UpnpHttpClient *http.Client
}

func DefaultPortMapperSettings() *PortMapperSettings {
	return &PortMapperSettings{
		MappingLifetime:          1 * time.Hour,
		RenewFraction:            0.5,
		MinRenewTimeout:          1 * time.Minute,
		MinRetryTimeout:          5 * time.Minute,
		MaxRetryTimeout:          1 * time.Hour,
		AttemptTimeout:           10 * time.Second,
		RequestTimeout:           250 * time.Millisecond,
		RequestAttemptCount:      3,
		UpnpSearchTimeout:        1500 * time.Millisecond,
		UpnpRequestTimeout:       2 * time.Second,
		UpnpMaxResponseByteCount: 64 * 1024,
		RemoveTimeout:            3 * time.Second,
		Description:              "URnetwork extender",
		Methods:                  []string{MethodPcp, MethodUpnp},
		ServerPort:               5351,
		UpnpSsdpPort:             1900,
		UpnpMulticastAddress:     upnpMulticastHost,
		Now:                      time.Now,
		After:                    time.After,
	}
}

// What one method mapped, and the state a renewal reads.
type portMapperMethodResult struct {
	method     string
	externalIp netip.Addr
	// by request index, nil where the request is not mapped
	mappings []*methodMapping
	// by request index, why a request is not mapped
	errs []error
}

// One granted mapping with what the method needs to renew and remove it.
type methodMapping struct {
	PortMapping
	// the pcp mapping nonce (RFC 6887 §11.1)
	pcpNonce [12]byte
	// the upnp connection that made it
	upnp *upnpGateway
}

// A method of making mappings with one router.
type portMapMethod interface {
	name() string
	// Maps every request, renewing where `previous` has a mapping for it, and
	// removes a renewed mapping the router replaced with one on another
	// external port when the old one would otherwise remain. An error says the
	// method could not be used at all.
	mapPorts(
		ctx context.Context,
		requests []PortMapRequest,
		previous []*methodMapping,
		lifetime time.Duration,
	) (externalIp netip.Addr, mappings []*methodMapping, errs []error, err error)
	removePorts(ctx context.Context, mappings []*methodMapping) error
}

// errors a method answers that say the router does not speak it, which moves
// the attempt on to the next method without a word in the status
var errMethodNotSupported = errors.New("the router does not answer this method")

type PortMapper struct {
	ctx       context.Context
	cancel    context.CancelFunc
	done      chan struct{}
	closeOnce sync.Once

	log      connect.Logger
	requests []PortMapRequest
	settings *PortMapperSettings

	// closed once the first attempt finished, mapped or not
	ready     chan struct{}
	readyOnce sync.Once

	// increments on every status change
	changeMonitor *connect.MonitorValue[uint64]

	stateLock   sync.Mutex
	status      PortMapperStatus
	changeCount uint64
	// what the method in use mapped, which renewals and the removal read; the
	// loop alone writes it
	current       *portMapperMethodResult
	currentMethod portMapMethod
}

// Starts mapping `requests` at once. The first request is the one the caller
// needs: a method that cannot map it is not used, and the next one is tried.
func NewPortMapper(ctx context.Context, requests []PortMapRequest, settings *PortMapperSettings) *PortMapper {
	if settings == nil {
		settings = DefaultPortMapperSettings()
	}
	log := settings.Log
	if log == nil {
		log = connect.DefaultLogger()
	}
	cancelCtx, cancel := context.WithCancel(ctx)
	self := &PortMapper{
		ctx:           cancelCtx,
		cancel:        cancel,
		done:          make(chan struct{}),
		log:           log,
		requests:      slices.Clone(requests),
		settings:      settings,
		ready:         make(chan struct{}),
		changeMonitor: connect.NewMonitorValue[uint64](0),
		status:        PortMapperStatus{State: StateMapping},
	}
	go connect.HandleError(func() {
		defer close(self.done)
		defer self.setReady()
		self.run()
	}, func(err error) {
		self.log.Errorf("[portmap]the port mapper failed = %s\n", err)
	})
	return self
}

// Closed once the first attempt has finished, mapped or not, and when the
// mapper closes.
func (self *PortMapper) Ready() <-chan struct{} {
	return self.ready
}

func (self *PortMapper) setReady() {
	self.readyOnce.Do(func() {
		close(self.ready)
	})
}

// Increments on every change of the status.
func (self *PortMapper) ChangeMonitor() *connect.MonitorValue[uint64] {
	return self.changeMonitor
}

// A copy of the status.
func (self *PortMapper) Status() *PortMapperStatus {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	status := self.status
	status.Mappings = slices.Clone(self.status.Mappings)
	return &status
}

// Ends the loop, removes every mapping within RemoveTimeout, and joins.
func (self *PortMapper) Close() {
	self.closeOnce.Do(func() {
		self.cancel()
		<-self.done
	})
}

func (self *PortMapper) now() time.Time {
	if self.settings.Now != nil {
		return self.settings.Now()
	}
	return time.Now()
}

func (self *PortMapper) after(wait time.Duration) <-chan time.Time {
	if self.settings.After != nil {
		return self.settings.After(wait)
	}
	return time.After(wait)
}

func (self *PortMapper) setStatus(status PortMapperStatus) {
	changeCount := func() uint64 {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.status = status
		self.changeCount += 1
		return self.changeCount
	}()
	self.changeMonitor.Set(changeCount)
}

func (self *PortMapper) run() {
	defer self.removeAll()

	retryTimeout := self.settings.MinRetryTimeout
	for {
		renewAfter, mapped := self.attempt()
		self.setReady()

		var wait time.Duration
		if mapped {
			wait = renewAfter
			retryTimeout = self.settings.MinRetryTimeout
		} else {
			wait = retryTimeout
			retryTimeout = min(2*retryTimeout, self.settings.MaxRetryTimeout)
		}
		select {
		case <-self.ctx.Done():
			return
		case <-self.after(wait):
		}
	}
}

// One attempt: a renewal of what the method in use mapped, or every method in
// turn. It publishes the outcome and answers when to renew, and whether the
// first request is mapped.
func (self *PortMapper) attempt() (renewAfter time.Duration, mapped bool) {
	ctx, cancel := context.WithTimeout(self.ctx, self.settings.AttemptTimeout)
	defer cancel()

	gatewaySource := self.settings.Gateway
	if gatewaySource == nil {
		gatewaySource = DefaultGateway
	}
	gateway, localIp, err := gatewaySource()
	if err != nil {
		self.dropCurrent()
		self.publishFailed(netip.Addr{}, netip.Addr{}, err.Error())
		return 0, false
	}

	methodErrs := []string{}
	tryMethod := func(method portMapMethod, previous []*methodMapping) bool {
		externalIp, mappings, errs, err := method.mapPorts(ctx, self.requests, previous, self.settings.MappingLifetime)
		if err == nil && (len(mappings) == 0 || mappings[0] == nil) {
			// the first request is the one the caller needs
			if 0 < len(errs) && errs[0] != nil {
				err = errs[0]
			} else {
				err = errors.New("the router mapped nothing")
			}
		}
		if err == nil && externalIp.IsValid() && !externalIp.IsUnspecified() && !isPublicIpv4(externalIp) {
			err = fmt.Errorf("the router's own address %s is not public: another NAT is in front of it", externalIp)
		}
		if err != nil {
			// a method that mapped some of the requests and not the first
			// leaves nothing behind
			self.removeMappings(method, mappings)
			if !errors.Is(err, errMethodNotSupported) {
				methodErrs = append(methodErrs, fmt.Sprintf("%s: %s", method.name(), err))
			} else {
				methodErrs = append(methodErrs, fmt.Sprintf("%s: no answer", method.name()))
			}
			return false
		}
		result := &portMapperMethodResult{
			method:     method.name(),
			externalIp: externalIp,
			mappings:   mappings,
			errs:       errs,
		}
		// a mapping the renewal could not renew is removed rather than left to
		// run out. One the router moved is the method's to clean up, since only
		// the method knows whether the old one still exists (mapPorts)
		if previous != nil {
			unrenewed := []*methodMapping{}
			for i, previousMapping := range previous {
				if previousMapping != nil && (len(mappings) <= i || mappings[i] == nil) {
					unrenewed = append(unrenewed, previousMapping)
				}
			}
			self.removeMappings(method, unrenewed)
		}
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.current = result
			self.currentMethod = method
		}()
		self.publishMapped(gateway, localIp, result)
		return true
	}

	// a renewal asks the router that mapped, by the method that mapped
	currentMethod, current := func() (portMapMethod, *portMapperMethodResult) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.currentMethod, self.current
	}()
	if currentMethod != nil && current != nil {
		if tryMethod(currentMethod, current.mappings) {
			return self.renewAfter(current), true
		}
		// the router lost or refused them; a fresh attempt follows at once
		self.dropCurrent()
	}

	for _, methodName := range self.settings.Methods {
		if ctx.Err() != nil {
			break
		}
		var method portMapMethod
		switch methodName {
		case MethodPcp:
			// a pcp server answers a pcp request, and a nat-pmp server answers
			// it with its version, which is how nat-pmp is found (RFC 6887 §9)
			method = self.newPcpMethod(gateway, localIp, false)
		case MethodNatPmp:
			method = self.newPcpMethod(gateway, localIp, true)
		case MethodUpnp:
			method = self.newUpnpMethod(gateway, localIp)
		default:
			continue
		}
		if tryMethod(method, nil) {
			current := func() *portMapperMethodResult {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				return self.current
			}()
			return self.renewAfter(current), true
		}
	}
	reason := strings.Join(methodErrs, "; ")
	if reason == "" {
		reason = "no method to try"
	}
	if ctx.Err() != nil && self.ctx.Err() == nil {
		reason = fmt.Sprintf("the router did not finish within %s: %s", self.settings.AttemptTimeout, reason)
	}
	self.publishFailed(gateway, localIp, reason)
	return 0, false
}

// When to renew what `result` mapped: the fraction of the shortest lifetime
// granted, at least the floor.
func (self *PortMapper) renewAfter(result *portMapperMethodResult) time.Duration {
	lifetime := time.Duration(0)
	for _, mapping := range result.mappings {
		if mapping != nil && (lifetime == 0 || mapping.Lifetime < lifetime) {
			lifetime = mapping.Lifetime
		}
	}
	renewAfter := time.Duration(float64(lifetime) * self.settings.RenewFraction)
	return max(renewAfter, self.settings.MinRenewTimeout)
}

func (self *PortMapper) dropCurrent() {
	currentMethod, current := func() (portMapMethod, *portMapperMethodResult) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		currentMethod, current := self.currentMethod, self.current
		self.currentMethod = nil
		self.current = nil
		return currentMethod, current
	}()
	if currentMethod != nil && current != nil {
		self.removeMappings(currentMethod, current.mappings)
	}
}

func (self *PortMapper) publishMapped(gateway netip.Addr, localIp netip.Addr, result *portMapperMethodResult) {
	status := PortMapperStatus{
		State:      StateMapped,
		Method:     result.method,
		Gateway:    gateway,
		LocalIp:    localIp,
		ExternalIp: result.externalIp,
		Mappings:   []PortMapping{},
		UpdateTime: self.now(),
	}
	errTexts := []string{}
	for i, request := range self.requests {
		if i < len(result.mappings) && result.mappings[i] != nil {
			status.Mappings = append(status.Mappings, result.mappings[i].PortMapping)
		} else if i < len(result.errs) && result.errs[i] != nil {
			errTexts = append(errTexts, fmt.Sprintf("%s: %s", request, result.errs[i]))
		}
	}
	status.Error = strings.Join(errTexts, "; ")
	if self.log.V(1).Enabled() {
		self.log.Infof("[portmap]mapped by %s through %s = %v %s\n", status.Method, gateway, status.Mappings, status.Error)
	}
	self.setStatus(status)
}

func (self *PortMapper) publishFailed(gateway netip.Addr, localIp netip.Addr, reason string) {
	previous := self.Status()
	if previous.State != StateFailed || previous.Error != reason {
		self.log.Infof("[portmap]nothing mapped = %s\n", reason)
	}
	self.setStatus(PortMapperStatus{
		State:      StateFailed,
		Gateway:    gateway,
		LocalIp:    localIp,
		Mappings:   []PortMapping{},
		Error:      reason,
		UpdateTime: self.now(),
	})
}

// Removes `mappings` with `method`, within RemoveTimeout and outside the loop's
// own context, which may have ended.
func (self *PortMapper) removeMappings(method portMapMethod, mappings []*methodMapping) {
	removing := []*methodMapping{}
	for _, mapping := range mappings {
		if mapping != nil {
			removing = append(removing, mapping)
		}
	}
	if len(removing) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), self.settings.RemoveTimeout)
	defer cancel()
	if err := method.removePorts(ctx, removing); err != nil {
		self.log.Infof("[portmap]remove by %s err = %s\n", method.name(), err)
	}
}

// Removes everything mapped as the mapper closes.
func (self *PortMapper) removeAll() {
	self.dropCurrent()
	self.setStatus(PortMapperStatus{
		State:      StateClosed,
		Mappings:   []PortMapping{},
		UpdateTime: self.now(),
	})
}

func (self *PortMapper) exchange() ExchangeFunction {
	if self.settings.Exchange != nil {
		return self.settings.Exchange
	}
	return func(
		ctx context.Context,
		server netip.AddrPort,
		localIp netip.Addr,
		request []byte,
		accept func(response []byte) bool,
	) ([]byte, error) {
		return UdpExchange(ctx, server, localIp, request, accept, self.settings.RequestTimeout, self.settings.RequestAttemptCount)
	}
}

func (self *PortMapper) lifetimeSeconds(lifetime time.Duration) uint32 {
	seconds := lifetime / time.Second
	if seconds < 1 {
		return 1
	}
	if 0xffffffff < seconds {
		return 0xffffffff
	}
	return uint32(seconds)
}

// Whether `ip` is an ipv4 address the internet can dial: neither private, CGNAT
// (RFC 6598), loopback, link local nor otherwise reserved for local use.
func isPublicIpv4(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.Is4() || !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return false
	}
	return !cgnatPrefix.Contains(ip)
}

var cgnatPrefix = netip.MustParsePrefix("100.64.0.0/10")

// Whether `ip` can be a router on the host's own lan: a private (RFC 1918) or
// CGNAT ipv4 address.
func isLanIpv4(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.Is4() && (ip.IsPrivate() || cgnatPrefix.Contains(ip))
}
