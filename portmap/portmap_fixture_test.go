package portmap

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The fakes the port mapper is tested against: a pcp and nat-pmp server answered
// in memory through the exchange seam, a upnp internet gateway device served by
// httptest on loopback, and a clock whose waits a test fires.

// Synthetic addresses only (RFC 5737): the lan side of a fake router is the
// mapper's own business, and the external address is a documentation address,
// which reads as public.
var (
	testGatewayIp  = netip.MustParseAddr("192.0.2.1")
	testLocalIp    = netip.MustParseAddr("192.0.2.10")
	testExternalIp = netip.MustParseAddr("203.0.113.7")
)

// The requests of an extender role: tcp and udp 443, udp 4053.
func testExtenderRequests() []PortMapRequest {
	return []PortMapRequest{
		{Protocol: ProtocolTcp, InternalPort: 443},
		{Protocol: ProtocolUdp, InternalPort: 443},
		{Protocol: ProtocolUdp, InternalPort: 4053},
	}
}

// One wait the mapper asked for, which the test fires.
type testWait struct {
	wait time.Duration
	fire chan time.Time
}

// A clock whose waits are handed to the test, so a renewal or a retry runs
// exactly when the test says.
type testClock struct {
	stateLock sync.Mutex
	now       time.Time
	waits     chan testWait
}

func newTestClock() *testClock {
	return &testClock{
		now:   time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC),
		waits: make(chan testWait, 16),
	}
}

func (self *testClock) Now() time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.now
}

func (self *testClock) After(wait time.Duration) <-chan time.Time {
	fire := make(chan time.Time, 1)
	self.waits <- testWait{wait: wait, fire: fire}
	return fire
}

// The next wait the mapper asked for.
func (self *testClock) nextWait(t *testing.T) testWait {
	t.Helper()
	select {
	case wait := <-self.waits:
		return wait
	case <-time.After(30 * time.Second):
		t.Fatal("the mapper asked for no wait")
		return testWait{}
	}
}

// Advances the clock past the wait and fires it.
func (self *testClock) elapse(wait testWait) {
	self.stateLock.Lock()
	self.now = self.now.Add(wait.wait)
	now := self.now
	self.stateLock.Unlock()
	wait.fire <- now
}

// One map request a fake router received, decoded.
type testMapRequest struct {
	version           int
	protocol          string
	internalPort      int
	suggestedExternal int
	lifetimeSeconds   uint32
	clientIp          netip.Addr
	nonce             [12]byte
}

type testPmpMapping struct {
	externalPort int
	nonce        [12]byte
}

// A pcp and nat-pmp server, answered in memory (RFC 6887, RFC 6886).
type testPmpGateway struct {
	stateLock sync.Mutex
	pcp       bool
	natPmp    bool
	// answers nothing at all
	silent bool
	// answers every map with this result code, 0 for success
	resultCode int
	// answers a map of this protocol and internal port with this result code
	refusedResultCodes map[string]int
	externalIp         netip.Addr
	// the external port the router grants, given what was suggested; nil
	// grants the suggestion
	externalPort func(protocol string, internalPort int, suggestedExternal int) int
	// the most lifetime it grants, 0 for no cap
	lifetimeCapSeconds uint32
	// what it holds, by protocol and internal port
	mappings map[string]testPmpMapping
	// every map request, deletions included
	maps []testMapRequest
	// how many requests it received
	requestCount int
}

func newTestPmpGateway() *testPmpGateway {
	return &testPmpGateway{
		pcp:                true,
		natPmp:             true,
		externalIp:         testExternalIp,
		refusedResultCodes: map[string]int{},
		mappings:           map[string]testPmpMapping{},
	}
}

func testMappingKey(protocol string, internalPort int) string {
	return fmt.Sprintf("%s/%d", protocol, internalPort)
}

func (self *testPmpGateway) exchange(
	ctx context.Context,
	server netip.AddrPort,
	localIp netip.Addr,
	request []byte,
	accept func(response []byte) bool,
) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if server != netip.AddrPortFrom(testGatewayIp, 5351) {
		return nil, fmt.Errorf("the exchange went to %s, not the gateway", server)
	}
	response := self.answer(request)
	if response == nil || !accept(response) {
		return nil, errNoAnswer
	}
	return response, nil
}

func (self *testPmpGateway) answer(request []byte) []byte {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.requestCount += 1
	if self.silent || len(request) < 2 {
		return nil
	}
	switch request[0] {
	case pcpVersion:
		if !self.pcp {
			if self.natPmp {
				// a nat-pmp server's answer to a version it does not speak
				response := make([]byte, 8)
				response[0] = natPmpVersion
				response[1] = pcpResponseBit | request[1]
				binary.BigEndian.PutUint16(response[2:4], natPmpResultUnsupportedVersion)
				return response
			}
			return nil
		}
		return self.answerPcp(request)
	case natPmpVersion:
		if !self.natPmp {
			return nil
		}
		return self.answerNatPmp(request)
	default:
		return nil
	}
}

func (self *testPmpGateway) grant(protocol string, internalPort int, suggestedExternal int, lifetimeSeconds uint32) (int, uint32, int) {
	if resultCode, ok := self.refusedResultCodes[testMappingKey(protocol, internalPort)]; ok {
		return 0, 0, resultCode
	}
	if self.resultCode != 0 {
		return 0, 0, self.resultCode
	}
	externalPort := suggestedExternal
	if externalPort == 0 {
		externalPort = internalPort
	}
	if self.externalPort != nil {
		externalPort = self.externalPort(protocol, internalPort, suggestedExternal)
	}
	if 0 < self.lifetimeCapSeconds && self.lifetimeCapSeconds < lifetimeSeconds {
		lifetimeSeconds = self.lifetimeCapSeconds
	}
	return externalPort, lifetimeSeconds, 0
}

func (self *testPmpGateway) answerPcp(request []byte) []byte {
	if len(request) < pcpRequestByteCount || request[1] != pcpOpcodeMap {
		return nil
	}
	body := request[pcpHeaderByteCount:]
	mapRequest := testMapRequest{
		version:           pcpVersion,
		internalPort:      int(binary.BigEndian.Uint16(body[16:18])),
		suggestedExternal: int(binary.BigEndian.Uint16(body[18:20])),
		lifetimeSeconds:   binary.BigEndian.Uint32(request[4:8]),
		clientIp:          netip.AddrFrom16([16]byte(request[8:24])).Unmap(),
	}
	copy(mapRequest.nonce[:], body[0:12])
	switch body[12] {
	case pcpProtocolTcp:
		mapRequest.protocol = ProtocolTcp
	case pcpProtocolUdp:
		mapRequest.protocol = ProtocolUdp
	}
	self.maps = append(self.maps, mapRequest)

	response := make([]byte, pcpResponseByteCount)
	response[0] = pcpVersion
	response[1] = pcpResponseBit | pcpOpcodeMap
	copy(response[pcpHeaderByteCount:], body)
	key := testMappingKey(mapRequest.protocol, mapRequest.internalPort)
	if mapRequest.lifetimeSeconds == 0 {
		delete(self.mappings, key)
		return response
	}
	externalPort, lifetimeSeconds, resultCode := self.grant(
		mapRequest.protocol, mapRequest.internalPort, mapRequest.suggestedExternal, mapRequest.lifetimeSeconds)
	response[3] = byte(resultCode)
	if resultCode != 0 {
		return response
	}
	self.mappings[key] = testPmpMapping{externalPort: externalPort, nonce: mapRequest.nonce}
	binary.BigEndian.PutUint32(response[4:8], lifetimeSeconds)
	responseBody := response[pcpHeaderByteCount:]
	binary.BigEndian.PutUint16(responseBody[18:20], uint16(externalPort))
	externalAddress := pcpAddressBytes(self.externalIp)
	copy(responseBody[20:36], externalAddress[:])
	return response
}

func (self *testPmpGateway) answerNatPmp(request []byte) []byte {
	switch request[1] {
	case natPmpOpcodeExternalAddress:
		response := make([]byte, natPmpAddressByteCount)
		response[0] = natPmpVersion
		response[1] = pcpResponseBit | natPmpOpcodeExternalAddress
		copy(response[8:12], self.externalIp.AsSlice())
		return response
	case natPmpOpcodeMapUdp, natPmpOpcodeMapTcp:
		if len(request) < natPmpMapRequestByteCount {
			return nil
		}
		mapRequest := testMapRequest{
			version:           natPmpVersion,
			protocol:          ProtocolUdp,
			internalPort:      int(binary.BigEndian.Uint16(request[4:6])),
			suggestedExternal: int(binary.BigEndian.Uint16(request[6:8])),
			lifetimeSeconds:   binary.BigEndian.Uint32(request[8:12]),
		}
		if request[1] == natPmpOpcodeMapTcp {
			mapRequest.protocol = ProtocolTcp
		}
		self.maps = append(self.maps, mapRequest)
		response := make([]byte, natPmpMapResponseByteCount)
		response[0] = natPmpVersion
		response[1] = pcpResponseBit | request[1]
		binary.BigEndian.PutUint16(response[8:10], uint16(mapRequest.internalPort))
		key := testMappingKey(mapRequest.protocol, mapRequest.internalPort)
		if mapRequest.lifetimeSeconds == 0 {
			delete(self.mappings, key)
			return response
		}
		externalPort, lifetimeSeconds, resultCode := self.grant(
			mapRequest.protocol, mapRequest.internalPort, mapRequest.suggestedExternal, mapRequest.lifetimeSeconds)
		binary.BigEndian.PutUint16(response[2:4], uint16(resultCode))
		if resultCode != 0 {
			return response
		}
		self.mappings[key] = testPmpMapping{externalPort: externalPort}
		binary.BigEndian.PutUint16(response[10:12], uint16(externalPort))
		binary.BigEndian.PutUint32(response[12:16], lifetimeSeconds)
		return response
	default:
		return nil
	}
}

func (self *testPmpGateway) held() map[string]testPmpMapping {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	held := map[string]testPmpMapping{}
	for key, mapping := range self.mappings {
		held[key] = mapping
	}
	return held
}

func (self *testPmpGateway) mapRequests() []testMapRequest {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]testMapRequest{}, self.maps...)
}

func (self *testPmpGateway) set(f func()) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	f()
}

// One port mapping a fake upnp router holds.
type testUpnpMapping struct {
	internalPort   int
	internalClient string
	leaseSeconds   int
	description    string
}

// One SOAP action a fake upnp router received.
type testUpnpAction struct {
	name string
	args map[string]string
}

// A upnp internet gateway device, served by httptest on loopback (UPnP IGD:1
// and IGD:2).
type testUpnpGateway struct {
	server *httptest.Server
	// the version of its WANIPConnection service, 1 or 2
	version    int
	externalIp string
	// a description whose URLBase names this host instead
	urlBaseHost string

	stateLock sync.Mutex
	// external ports it holds for another host, by protocol and port
	takenPorts map[string]bool
	// the external port AddAnyPortMapping grants for a taken one
	anyPort       int
	permanentOnly bool
	// what it holds, by protocol and external port
	mappings map[string]testUpnpMapping
	actions  []testUpnpAction
}

func newTestUpnpGateway(t *testing.T, version int) *testUpnpGateway {
	self := &testUpnpGateway{
		version:    version,
		externalIp: testExternalIp.String(),
		takenPorts: map[string]bool{},
		mappings:   map[string]testUpnpMapping{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/rootDesc.xml", self.serveDescription)
	mux.HandleFunc("/ctl/IPConn", self.serveControl)
	self.server = httptest.NewServer(mux)
	t.Cleanup(self.server.Close)
	return self
}

func (self *testUpnpGateway) location() string {
	return self.server.URL + "/rootDesc.xml"
}

func (self *testUpnpGateway) serviceType() string {
	return fmt.Sprintf("urn:schemas-upnp-org:service:WANIPConnection:%d", self.version)
}

func (self *testUpnpGateway) serveDescription(w http.ResponseWriter, r *http.Request) {
	urlBase := ""
	if self.urlBaseHost != "" {
		urlBase = fmt.Sprintf("<URLBase>http://%s/</URLBase>", self.urlBaseHost)
	}
	w.Header().Set("Content-Type", "text/xml")
	fmt.Fprintf(w, `<?xml version="1.0"?>
<root xmlns="urn:schemas-upnp-org:device-1-0">
<specVersion><major>1</major><minor>0</minor></specVersion>
%s
<device>
<deviceType>urn:schemas-upnp-org:device:InternetGatewayDevice:%d</deviceType>
<friendlyName>test router</friendlyName>
<UDN>uuid:00000000-0000-0000-0000-000000000001</UDN>
<deviceList><device>
<deviceType>urn:schemas-upnp-org:device:WANDevice:%d</deviceType>
<UDN>uuid:00000000-0000-0000-0000-000000000002</UDN>
<deviceList><device>
<deviceType>urn:schemas-upnp-org:device:WANConnectionDevice:%d</deviceType>
<UDN>uuid:00000000-0000-0000-0000-000000000003</UDN>
<serviceList><service>
<serviceType>%s</serviceType>
<serviceId>urn:upnp-org:serviceId:WANIPConn1</serviceId>
<controlURL>/ctl/IPConn</controlURL>
<eventSubURL>/evt/IPConn</eventSubURL>
<SCPDURL>/WANIPCn.xml</SCPDURL>
</service></serviceList>
</device></deviceList>
</device></deviceList>
</device>
</root>`, urlBase, self.version, self.version, self.version, self.serviceType())
}

var testSoapArgPattern = regexp.MustCompile(`<(New[A-Za-z]+)>([^<]*)</New[A-Za-z]+>`)

func (self *testUpnpGateway) serveControl(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	soapAction := strings.Trim(r.Header.Get("SOAPACTION"), `"`)
	_, actionName, _ := strings.Cut(soapAction, "#")
	args := map[string]string{}
	for _, match := range testSoapArgPattern.FindAllStringSubmatch(string(body), -1) {
		args[match[1]] = match[2]
	}

	result, faultCode := func() (string, int) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.actions = append(self.actions, testUpnpAction{name: actionName, args: args})
		key := args["NewProtocol"] + "/" + args["NewExternalPort"]
		switch actionName {
		case "GetExternalIPAddress":
			return "<NewExternalIPAddress>" + self.externalIp + "</NewExternalIPAddress>", 0
		case "AddPortMapping", "AddAnyPortMapping":
			leaseSeconds, _ := strconv.Atoi(args["NewLeaseDuration"])
			if self.permanentOnly && leaseSeconds != 0 {
				return "", upnpErrorOnlyPermanentLeasesSupported
			}
			if self.takenPorts[key] {
				if actionName == "AddPortMapping" {
					return "", upnpErrorConflictInMappingEntry
				}
				key = fmt.Sprintf("%s/%d", args["NewProtocol"], self.anyPort)
				args["NewExternalPort"] = strconv.Itoa(self.anyPort)
			}
			internalPort, _ := strconv.Atoi(args["NewInternalPort"])
			self.mappings[key] = testUpnpMapping{
				internalPort:   internalPort,
				internalClient: args["NewInternalClient"],
				leaseSeconds:   leaseSeconds,
				description:    args["NewPortMappingDescription"],
			}
			if actionName == "AddAnyPortMapping" {
				return "<NewReservedPort>" + args["NewExternalPort"] + "</NewReservedPort>", 0
			}
			return "", 0
		case "DeletePortMapping":
			if _, ok := self.mappings[key]; !ok {
				// NoSuchEntryInArray
				return "", 714
			}
			delete(self.mappings, key)
			return "", 0
		default:
			// Invalid Action
			return "", 401
		}
	}()

	w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
	if faultCode != 0 {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, `<?xml version="1.0"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body><s:Fault><faultcode>s:Client</faultcode><faultstring>UPnPError</faultstring><detail><UPnPError xmlns="urn:schemas-upnp-org:control-1-0"><errorCode>%d</errorCode><errorDescription>test fault</errorDescription></UPnPError></detail></s:Fault></s:Body></s:Envelope>`, faultCode)
		return
	}
	fmt.Fprintf(w, `<?xml version="1.0"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body><u:%sResponse xmlns:u="%s">%s</u:%sResponse></s:Body></s:Envelope>`,
		actionName, self.serviceType(), result, actionName)
}

func (self *testUpnpGateway) held() map[string]testUpnpMapping {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	held := map[string]testUpnpMapping{}
	for key, mapping := range self.mappings {
		held[key] = mapping
	}
	return held
}

func (self *testUpnpGateway) receivedActions() []testUpnpAction {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]testUpnpAction{}, self.actions...)
}

func (self *testUpnpGateway) set(f func()) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	f()
}

// The settings of a mapper over the fakes: the pcp and nat-pmp gateway in
// memory, and the upnp device on loopback, which is the gateway of these
// settings when it is given.
func testPortMapperSettings(clock *testClock, pmpGateway *testPmpGateway, upnpGateway *testUpnpGateway) *PortMapperSettings {
	settings := DefaultPortMapperSettings()
	settings.Now = clock.Now
	settings.After = clock.After
	gateway := testGatewayIp
	localIp := testLocalIp
	if upnpGateway != nil {
		// the device is on loopback, so the gateway and this host are too
		gateway = netip.MustParseAddr("127.0.0.1")
		localIp = netip.MustParseAddr("127.0.0.1")
	}
	settings.Gateway = func() (netip.Addr, netip.Addr, error) {
		return gateway, localIp, nil
	}
	if pmpGateway != nil {
		settings.Exchange = pmpGateway.exchange
	} else {
		settings.Exchange = func(
			ctx context.Context,
			server netip.AddrPort,
			localIp netip.Addr,
			request []byte,
			accept func(response []byte) bool,
		) ([]byte, error) {
			return nil, errNoAnswer
		}
	}
	settings.UpnpSearch = func(ctx context.Context, searchGateway netip.Addr, searchLocalIp netip.Addr) ([]string, error) {
		if upnpGateway == nil {
			return nil, nil
		}
		return []string{upnpGateway.location()}, nil
	}
	return settings
}

// Waits for the mapper's first attempt.
func testWaitReady(t *testing.T, mapper *PortMapper) *PortMapperStatus {
	t.Helper()
	select {
	case <-mapper.Ready():
	case <-time.After(30 * time.Second):
		t.Fatal("the mapper's first attempt did not finish")
	}
	return mapper.Status()
}

// Waits for a status the predicate accepts.
func testWaitStatus(t *testing.T, mapper *PortMapper, accept func(status *PortMapperStatus) bool) *PortMapperStatus {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		_, update := mapper.ChangeMonitor().Get()
		if status := mapper.Status(); accept(status) {
			return status
		}
		select {
		case <-update:
		case <-deadline:
			t.Fatalf("the status never matched, last = %+v", mapper.Status())
			return nil
		}
	}
}
